package vercel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path"
	"sort"
	"sync"
	"time"

	"github.com/arugula-salad/wisp/internal/store"
	"github.com/arugula-salad/wisp/internal/vmm"
)

// Commands: Vercel's command API on wisp-agent's exec sessions. A command is
// started over the agent's exec WebSocket and the connection is held here for
// the command's whole life, with the VM held awake (Acquire): its output is
// kept here from the start (for the logs endpoint, which replays from the
// beginning, and for waited commands), and its exit code when it comes. Its
// agent session ID is what a kill signals. Commands live in memory: they end
// with their session, and a daemon restart forgets them.

// maxOutput is how much of a command's output is kept; past it, output is
// dropped (and the drop logged once). Hosted keeps everything.
const maxOutput = 64 << 20

// command is one command of one session.
type command struct {
	ID        string
	Name      string
	Args      []string
	Cwd       string
	SessionID string
	RecordID  string
	StartedAt int64

	mach    *vmm.Machine
	agentID string
	proc    *guestProc

	mu         sync.Mutex
	logs       []logLine
	outBytes   int
	dropped    bool
	exited     bool
	exitCode   int
	durationMs int64
	changed    chan struct{} // closed, and replaced, whenever logs or exited change
	done       chan struct{}
}

type logLine struct {
	Stream string `json:"stream"`
	Data   string `json:"data"`
}

// commandJSON is a command on the wire.
type commandJSON struct {
	ID         string   `json:"id"`
	Name       string   `json:"name"`
	Args       []string `json:"args"`
	Cwd        string   `json:"cwd"`
	SessionID  string   `json:"sessionId"`
	StartedAt  int64    `json:"startedAt"`
	ExitCode   *int     `json:"exitCode"`
	DurationMs *int64   `json:"durationMs,omitempty"`
}

func (c *command) json() commandJSON {
	c.mu.Lock()
	defer c.mu.Unlock()
	j := commandJSON{ID: c.ID, Name: c.Name, Args: c.Args, Cwd: c.Cwd, SessionID: c.SessionID, StartedAt: c.StartedAt}
	if j.Args == nil {
		j.Args = []string{}
	}
	if c.exited {
		code, d := c.exitCode, c.durationMs
		j.ExitCode, j.DurationMs = &code, &d
	}
	return j
}

// append keeps a line of output; firstDrop is the first one past maxOutput.
func (c *command) append(stream string, data []byte) (firstDrop bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.outBytes+len(data) > maxOutput {
		firstDrop, c.dropped = !c.dropped, true
		return firstDrop
	}
	c.outBytes += len(data)
	c.logs = append(c.logs, logLine{Stream: stream, Data: string(data)})
	close(c.changed)
	c.changed = make(chan struct{})
	return false
}

func (c *command) exit(code int, at time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.exited {
		return
	}
	c.exited, c.exitCode = true, code
	c.durationMs = max(0, at.UnixMilli()-c.StartedAt)
	close(c.changed)
	c.changed = make(chan struct{})
	close(c.done)
}

// follow calls emit with the command's log lines from the start, as they
// come, until it has exited and every line is out (true) or ctx ends (false).
func (c *command) follow(ctx context.Context, emit func(logLine)) bool {
	next := 0
	for {
		c.mu.Lock()
		lines, exited, changed := c.logs[next:], c.exited, c.changed
		next = len(c.logs)
		c.mu.Unlock()
		for _, l := range lines {
			emit(l)
		}
		if exited {
			return true
		}
		select {
		case <-changed:
		case <-ctx.Done():
			return false
		}
	}
}

// commandTable is every live command, by ID.
type commandTable struct {
	mu   sync.Mutex
	byID map[string]*command
}

func (t *commandTable) init() { t.byID = map[string]*command{} }

func (t *commandTable) add(c *command) {
	t.mu.Lock()
	t.byID[c.ID] = c
	t.mu.Unlock()
}

func (t *commandTable) get(id string) *command {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.byID[id]
}

func (t *commandTable) of(sid string) []*command {
	t.mu.Lock()
	defer t.mu.Unlock()
	var out []*command
	for _, c := range t.byID {
		if c.SessionID == sid {
			out = append(out, c)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].StartedAt < out[j].StartedAt })
	return out
}

// endSession forgets a session's commands, once their VM is gone: a command
// still running then ends as killed (137), as everything in a stopped
// session has been.
func (t *commandTable) endSession(sid string) {
	t.mu.Lock()
	var gone []*command
	for id, c := range t.byID {
		if c.SessionID == sid {
			gone = append(gone, c)
			delete(t.byID, id)
		}
	}
	t.mu.Unlock()
	for _, c := range gone {
		c.exit(137, time.Now())
		if c.proc != nil {
			c.proc.close()
		}
	}
}

func (t *commandTable) dropSandbox(recordID string) {
	t.mu.Lock()
	var sids []string
	for _, c := range t.byID {
		if c.RecordID == recordID {
			sids = append(sids, c.SessionID)
		}
	}
	t.mu.Unlock()
	for _, s := range sids {
		t.endSession(s)
	}
}

// cmdReq is POST …/cmd's body.
type cmdReq struct {
	Command string            `json:"command"`
	Args    []string          `json:"args"`
	Cwd     string            `json:"cwd"`
	Env     map[string]string `json:"env"`
	Sudo    bool              `json:"sudo"`
	Wait    bool              `json:"wait"`
	Logs    bool              `json:"logs"`
	Timeout *int64            `json:"timeout"`
}

// spec is how a command runs in the guest: as the default user (wisp-agent's
// user, uid 1000, `ubuntu`) with HOME=/vercel, or as root under sudo; in cwd
// (default /vercel); with the sandbox's env, then the command's, over the
// agent's base environment.
func (f *Frontend) spec(m meta, req cmdReq) execSpec {
	dir := req.Cwd
	if dir == "" {
		dir = f.opts.Home
	} else if !path.IsAbs(dir) {
		dir = path.Join(f.opts.Home, dir)
	}
	var env []string
	keys := func(e map[string]string) {
		ks := make([]string, 0, len(e))
		for k := range e {
			ks = append(ks, k)
		}
		sort.Strings(ks)
		for _, k := range ks {
			env = append(env, k+"="+e[k])
		}
	}
	argv := append([]string{req.Command}, req.Args...)
	if req.Sudo && f.opts.Sudo != "-" {
		// -E keeps the environment given here; -n never prompts.
		argv = append([]string{f.opts.Sudo, "-n", "-E", "--"}, argv...)
	} else if f.opts.Sudo != "-" {
		// The agent's user is named `sprite` in the image; Vercel's is `ubuntu`, the same uid.
		env = append(env, "USER="+defaultUser, "LOGNAME="+defaultUser)
	}
	keys(m.Env)
	keys(req.Env)
	return execSpec{argv: argv, dir: dir, env: env}
}

// runCommand is POST …/cmd: start a command, and with wait stream it as
// NDJSON (its command object, its log lines, its command object with the exit
// code) or else answer with the command at once.
func (f *Frontend) runCommand(w http.ResponseWriter, r *http.Request, rec store.Record, m meta, s session) {
	var req cmdReq
	if !readBody(w, r, &req) {
		return
	}
	q := r.URL.Query()
	if q.Get("wait") == "true" {
		req.Wait = true
	}
	if q.Get("logs") == "true" {
		req.Logs = true
	}
	if req.Command == "" {
		writeErr(w, http.StatusBadRequest, "bad_request", "Invalid request: `command` is required.")
		return
	}
	c, status, code, msg := f.startCommand(r.Context(), rec, m, s, req)
	if c == nil {
		writeErr(w, status, code, msg)
		return
	}
	if !req.Wait {
		writeJSON(w, http.StatusOK, map[string]any{"command": c.json()})
		return
	}
	nd := newNDJSON(w)
	nd.line(map[string]any{"command": c.json()})
	ok := true
	if req.Logs {
		ok = c.follow(r.Context(), func(l logLine) { nd.line(l) })
	} else {
		select {
		case <-c.done:
		case <-r.Context().Done():
			ok = false
		}
	}
	if ok {
		nd.line(map[string]any{"command": c.json()})
	}
}

// startCommand starts req in the session's VM. On failure c is nil and the
// rest is the error to answer.
func (f *Frontend) startCommand(ctx context.Context, rec store.Record, m meta, s session, req cmdReq) (c *command, status int, code, msg string) {
	mach, release, err := f.acquire(ctx, rec)
	if err != nil {
		f.log.Warn("could not reach a sandbox's VM for a command", "name", m.Name, "err", err)
		return nil, http.StatusGone, "sandbox_stopped", "Sandbox has stopped execution and is no longer available"
	}
	spec := f.spec(m, req)
	started := f.now()
	p, err := f.startExec(ctx, mach, spec)
	if err != nil {
		release()
		var nf errNotFound
		var se errStart
		switch {
		case errors.As(err, &nf):
			return nil, http.StatusBadRequest, "executable_not_found", nf.Error()
		case errors.As(err, &se):
			return nil, http.StatusBadRequest, "bad_request", "[invalid_argument] " + se.msg
		}
		f.log.Warn("command failed to start", "name", m.Name, "err", err)
		return nil, http.StatusInternalServerError, "internal_server_error", "Failed to start command: " + err.Error()
	}
	c = &command{ID: newCommandID(), Name: req.Command, Args: req.Args, Cwd: spec.dir, SessionID: s.ID, RecordID: rec.ID,
		StartedAt: started.UnixMilli(), mach: mach, agentID: p.agentID, proc: p,
		changed: make(chan struct{}), done: make(chan struct{})}
	f.cmds.add(c)
	var timer *time.Timer
	if req.Timeout != nil && *req.Timeout > 0 {
		timer = time.AfterFunc(time.Duration(*req.Timeout)*time.Millisecond, func() {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			f.signal(ctx, mach, c.agentID, 9)
		})
	}
	go func() {
		defer release()
		defer p.close()
		if timer != nil {
			defer timer.Stop()
		}
		for {
			stream, data, exited, ec, err := p.next()
			if err != nil {
				// The connection ended without an exit: the VM went away under it.
				c.exit(137, f.now())
				return
			}
			if exited {
				c.exit(ec, f.now())
				return
			}
			name := "stdout"
			if stream == streamStderr {
				name = "stderr"
			}
			if c.append(name, data) {
				f.log.Warn("command output past the limit is dropped", "command", c.ID, "limit", maxOutput)
			}
		}
	}()
	return c, 0, "", ""
}

// command looks up {cid} in the session.
func (f *Frontend) command(w http.ResponseWriter, r *http.Request, s session) *command {
	cid := r.PathValue("cid")
	c := f.cmds.get(cid)
	if c == nil || c.SessionID != s.ID {
		writeErr(w, http.StatusNotFound, "not_found", fmt.Sprintf("Command '%s' not found.", cid))
		return nil
	}
	return c
}

// getCommand is GET …/cmd/{cid}: the command, after it has exited with wait=true.
func (f *Frontend) getCommand(w http.ResponseWriter, r *http.Request, rec store.Record, m meta, s session) {
	c := f.command(w, r, s)
	if c == nil {
		return
	}
	if r.URL.Query().Get("wait") == "true" {
		select {
		case <-c.done:
		case <-r.Context().Done():
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"command": c.json()})
}

// commandLogs is GET …/cmd/{cid}/logs: the command's output from its start,
// as NDJSON log lines, ending when it exits.
func (f *Frontend) commandLogs(w http.ResponseWriter, r *http.Request, rec store.Record, m meta, s session) {
	c := f.command(w, r, s)
	if c == nil {
		return
	}
	nd := newNDJSON(w)
	nd.start()
	c.follow(r.Context(), func(l logLine) { nd.line(l) })
}

// killCommand is POST …/cmd/{cid}/kill: the signal goes to the command's
// process group; the answer does not wait for it to exit.
func (f *Frontend) killCommand(w http.ResponseWriter, r *http.Request, rec store.Record, m meta, s session) {
	c := f.command(w, r, s)
	if c == nil {
		return
	}
	var req struct {
		Signal *int `json:"signal"`
	}
	if !readBody(w, r, &req) {
		return
	}
	sig := 15
	if req.Signal != nil {
		sig = *req.Signal
	}
	if sig < 1 || sig > 64 {
		writeErr(w, http.StatusBadRequest, "bad_request", "Invalid request: `signal` must be a signal number.")
		return
	}
	select {
	case <-c.done:
	default:
		if err := f.signal(r.Context(), c.mach, c.agentID, sig); err != nil {
			writeErr(w, http.StatusInternalServerError, "internal_server_error", "Failed to signal the command: "+err.Error())
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"command": c.json()})
}

// listCommands is GET …/cmd: the session's commands.
func (f *Frontend) listCommands(w http.ResponseWriter, r *http.Request, rec store.Record, m meta, s session) {
	out := []commandJSON{}
	for _, c := range f.cmds.of(s.ID) {
		out = append(out, c.json())
	}
	writeJSON(w, http.StatusOK, map[string]any{"commands": out})
}

// ndjson writes one JSON object per line, flushing each, with exactly the
// content type the JS SDK compares against.
type ndjson struct {
	w       http.ResponseWriter
	fl      http.Flusher
	started bool
}

func newNDJSON(w http.ResponseWriter) *ndjson {
	fl, _ := w.(http.Flusher)
	return &ndjson{w: w, fl: fl}
}

func (n *ndjson) start() {
	if n.started {
		return
	}
	n.started = true
	n.w.Header().Set("Content-Type", "application/x-ndjson")
	n.w.Header().Set("Cache-Control", "no-cache")
	n.w.WriteHeader(http.StatusOK)
	if n.fl != nil {
		n.fl.Flush()
	}
}

func (n *ndjson) line(v any) {
	n.start()
	b, _ := json.Marshal(v)
	n.w.Write(append(b, '\n'))
	if n.fl != nil {
		n.fl.Flush()
	}
}
