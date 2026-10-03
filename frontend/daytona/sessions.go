package daytona

import (
	"context"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// Sessions: Daytona's persistent shells. A session is named by the client;
// commands run in it get an ID, and see the working directory and exported
// environment the session's earlier commands left behind. Their output is
// kept for GET .../logs and streamed to followers over a WebSocket.
//
// There is no long-lived shell process: each command is its own bash run
// through wisp-agent's exec, which first restores the session's state (the
// cwd, and every exported variable) from a file in the guest and saves it
// back when it exits, however it exits. So `cd` and `export` carry over
// from one command to the next, as in one shell; shell functions, aliases,
// options and unexported variables do not. Commands do not wait for each
// other: an async command still running does not hold up the next one,
// which starts from the state the last command to finish left.
//
// The output is held here, on the host, so sessions and their logs live as
// long as this daemon and the sandbox's VM do: a stop ends them.

// Log follow framing (docs/providers/daytona.md section 5.2): every binary
// frame is one chunk of one stream, prefixed with that stream's marker.
var (
	stdoutPrefix = []byte{1, 1, 1}
	stderrPrefix = []byte{2, 2, 2}
)

// endWait is how long ending a session waits for its commands to die.
var endWait = 3 * time.Second

// maxCommandLog is how much output a command keeps; past it the oldest goes.
const maxCommandLog = 8 << 20

// sessionStateDir is where, in the guest, a session's state files live.
const sessionStateDir = "/tmp/.wisp-daytona/sessions"

// sessionDir is where the guest keeps session state.
func (f *Frontend) sessionDir() string {
	if f.stateDir != "" {
		return f.stateDir
	}
	return sessionStateDir
}

type sessions struct {
	f  *Frontend
	mu sync.Mutex
	by map[string]map[string]*session // sandbox ID -> session ID -> session
}

func newSessions(f *Frontend) *sessions {
	return &sessions{f: f, by: map[string]map[string]*session{}}
}

type session struct {
	id      string
	sandbox string
	mu      sync.Mutex
	seq     int
	cmds    []*command
	gone    bool
}

type logFrame struct {
	stream byte
	data   []byte
}

// command is one command run in a session.
type command struct {
	id, text string

	mu       sync.Mutex
	frames   []logFrame
	base     int // absolute index of frames[0]; earlier ones were dropped
	size     int
	exited   bool
	exitCode int
	changed  chan struct{} // closed, and replaced, on every append and at exit
	conn     *execConn
}

func newCommand(id, text string) *command {
	return &command{id: id, text: text, changed: make(chan struct{})}
}

func (c *command) append(stream byte, data []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.frames = append(c.frames, logFrame{stream, append([]byte(nil), data...)})
	c.size += len(data)
	for c.size > maxCommandLog && len(c.frames) > 1 {
		c.size -= len(c.frames[0].data)
		c.frames = c.frames[1:]
		c.base++
	}
	close(c.changed)
	c.changed = make(chan struct{})
}

func (c *command) finish(code int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.exited {
		return
	}
	c.exited, c.exitCode = true, code
	close(c.changed)
	c.changed = make(chan struct{})
}

// since is the frames from absolute index i on, the index after them, whether
// the command has exited, and a channel that is closed when that changes.
func (c *command) since(i int) ([]logFrame, int, bool, <-chan struct{}) {
	c.mu.Lock()
	defer c.mu.Unlock()
	i = min(max(i, c.base), c.base+len(c.frames))
	out := append([]logFrame(nil), c.frames[i-c.base:]...)
	return out, c.base + len(c.frames), c.exited, c.changed
}

// logs is the output so far: combined, stdout and stderr.
func (c *command) logs() (output, stdout, stderr string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	var o, so, se strings.Builder
	for _, fr := range c.frames {
		o.Write(fr.data)
		if fr.stream == streamStderr {
			se.Write(fr.data)
		} else {
			so.Write(fr.data)
		}
	}
	return o.String(), so.String(), se.String()
}

// commandJSON is Command; exitCode is null while it runs.
type commandJSON struct {
	ID       string `json:"id"`
	Command  string `json:"command"`
	ExitCode *int   `json:"exitCode"`
}

func (c *command) json() commandJSON {
	c.mu.Lock()
	defer c.mu.Unlock()
	j := commandJSON{ID: c.id, Command: c.text}
	if c.exited {
		code := c.exitCode
		j.ExitCode = &code
	}
	return j
}

func (s *session) json() map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	cmds := make([]commandJSON, len(s.cmds))
	for i, c := range s.cmds {
		cmds[i] = c.json()
	}
	return map[string]any{"sessionId": s.id, "commands": cmds}
}

func (s *session) command(id string) *command {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, c := range s.cmds {
		if c.id == id {
			return c
		}
	}
	return nil
}

// end kills the session's running commands, and waits (a little) for them
// to be gone, so that nothing of theirs runs after the session has.
func (s *session) end() {
	s.mu.Lock()
	s.gone = true
	cmds := append([]*command(nil), s.cmds...)
	s.mu.Unlock()
	for _, c := range cmds {
		c.mu.Lock()
		conn, running := c.conn, !c.exited
		c.mu.Unlock()
		if conn != nil && running {
			conn.kill()
		}
	}
	// One deadline for them all, and a timer of its own for each wait: a
	// command that will not die must not hold up the rest, or the stop or
	// delete waiting here (under the sandbox's lock) for good.
	deadline := time.Now().Add(endWait)
	for _, c := range cmds {
		for {
			_, _, exited, changed := c.since(1 << 62) // no frames, only the state
			if exited {
				break
			}
			timer := time.NewTimer(time.Until(deadline))
			select {
			case <-changed:
				timer.Stop()
				continue
			case <-timer.C:
			}
			break
		}
		c.mu.Lock()
		if c.conn != nil {
			c.conn.close()
		}
		c.mu.Unlock()
	}
}

func (ss *sessions) get(sandbox, id string) *session {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	return ss.by[sandbox][id]
}

// dropSandbox ends every session of a sandbox that is stopping or going.
func (ss *sessions) dropSandbox(sandbox string) {
	ss.mu.Lock()
	m := ss.by[sandbox]
	delete(ss.by, sandbox)
	ss.mu.Unlock()
	for _, s := range m {
		s.end()
	}
}

// sessionIDPattern is what a session ID may be: it names a directory in the guest.
var sessionIDPattern = regexp.MustCompile(`^[A-Za-z0-9._-]{1,128}$`)

func (f *Frontend) createSession(w http.ResponseWriter, r *http.Request, b *box) {
	var req struct {
		SessionID string `json:"sessionId"`
	}
	if !readBody(w, r, &req) {
		return
	}
	if !sessionIDPattern.MatchString(req.SessionID) || req.SessionID == "." || req.SessionID == ".." {
		writeErr(w, r, http.StatusBadRequest, "BAD_REQUEST", "sessionId must be 1-128 of A-Z a-z 0-9 . _ -")
		return
	}
	ss := f.sessions
	ss.mu.Lock()
	if ss.by[b.rec.ID] == nil {
		ss.by[b.rec.ID] = map[string]*session{}
	}
	if ss.by[b.rec.ID][req.SessionID] != nil {
		ss.mu.Unlock()
		writeErr(w, r, http.StatusConflict, "CONFLICT", fmt.Sprintf("session %s already exists", req.SessionID))
		return
	}
	ss.by[b.rec.ID][req.SessionID] = &session{id: req.SessionID, sandbox: b.rec.ID}
	ss.mu.Unlock()
	// A session made under this name before (a daemon ago) leaves no state behind.
	f.oneShot(r.Context(), b, execSpec{Cmd: []string{"rm", "-rf", f.sessionDir() + "/" + req.SessionID}}, nil, 10)
	w.WriteHeader(http.StatusCreated)
}

func (f *Frontend) listSessions(w http.ResponseWriter, r *http.Request, b *box) {
	ss := f.sessions
	ss.mu.Lock()
	var list []*session
	for _, s := range ss.by[b.rec.ID] {
		list = append(list, s)
	}
	ss.mu.Unlock()
	out := make([]map[string]any, 0, len(list))
	for _, s := range list {
		out = append(out, s.json())
	}
	writeJSON(w, http.StatusOK, out)
}

// lookupSession finds {sid}, answering 404 when there is none.
func (f *Frontend) lookupSession(w http.ResponseWriter, r *http.Request, b *box) *session {
	s := f.sessions.get(b.rec.ID, r.PathValue("sid"))
	if s == nil {
		writeErr(w, r, http.StatusNotFound, "NOT_FOUND", fmt.Sprintf("session %s not found", r.PathValue("sid")))
	}
	return s
}

func (f *Frontend) lookupCommand(w http.ResponseWriter, r *http.Request, b *box) (*session, *command) {
	s := f.lookupSession(w, r, b)
	if s == nil {
		return nil, nil
	}
	c := s.command(r.PathValue("cid"))
	if c == nil {
		writeErr(w, r, http.StatusNotFound, "NOT_FOUND", fmt.Sprintf("command %s not found in session %s", r.PathValue("cid"), s.id))
	}
	return s, c
}

func (f *Frontend) getSession(w http.ResponseWriter, r *http.Request, b *box) {
	if s := f.lookupSession(w, r, b); s != nil {
		writeJSON(w, http.StatusOK, s.json())
	}
}

func (f *Frontend) deleteSession(w http.ResponseWriter, r *http.Request, b *box) {
	ss := f.sessions
	ss.mu.Lock()
	s := ss.by[b.rec.ID][r.PathValue("sid")]
	if s != nil {
		delete(ss.by[b.rec.ID], s.id)
	}
	ss.mu.Unlock()
	if s == nil {
		writeErr(w, r, http.StatusNotFound, "NOT_FOUND", fmt.Sprintf("session %s not found", r.PathValue("sid")))
		return
	}
	s.end()
	f.oneShot(r.Context(), b, execSpec{Cmd: []string{"rm", "-rf", f.sessionDir() + "/" + s.id}}, nil, 10)
	w.WriteHeader(http.StatusNoContent)
}

// sessionScript is the bash program a session command runs as: restore the
// state the session's last finished command saved (its working directory and
// exported variables), run the command as it was written, and save the state
// it leaves when bash exits, under this command's sequence number (the
// newest number wins at the next restore, so an old async command finishing
// late does not undo a newer one's cd).
func sessionScript(stateDir, sessionID string, seq int, text string) string {
	dir := stateDir + "/" + sessionID
	n := strconv.Itoa(seq)
	return `__wisp_d='` + dir + `'
mkdir -p "$__wisp_d" 2>/dev/null
__wisp_s=$(ls "$__wisp_d" 2>/dev/null | sort -n | tail -n 1)
[ -n "$__wisp_s" ] && . "$__wisp_d/$__wisp_s" 2>/dev/null
unset __wisp_s
__wisp_save() {
  __wisp_rc=$?
  { printf 'cd %q 2>/dev/null\n' "$PWD"; export -p | grep -v -E '^declare -x (PWD|OLDPWD|SHLVL|_)(=|$)'; } > "$__wisp_d/.` + n + `" 2>/dev/null \
    && mv -f "$__wisp_d/.` + n + `" "$__wisp_d/` + n + `" 2>/dev/null
  exit $__wisp_rc
}
trap __wisp_save EXIT
` + text + "\n"
}

// sessionExec is POST /process/session/{sid}/exec: start the command, and
// answer once it has finished (200, with its output and exit code) or at
// once with runAsync (202, with its ID only).
func (f *Frontend) sessionExec(w http.ResponseWriter, r *http.Request, b *box) {
	s := f.lookupSession(w, r, b)
	if s == nil {
		return
	}
	var req struct {
		Command  string `json:"command"`
		RunAsync bool   `json:"runAsync"`
		Async    bool   `json:"async"`
	}
	if !readBody(w, r, &req) {
		return
	}
	if strings.TrimSpace(req.Command) == "" {
		writeErr(w, r, http.StatusBadRequest, "BAD_REQUEST", "command is required")
		return
	}
	async := req.RunAsync || req.Async
	s.mu.Lock()
	if s.gone {
		s.mu.Unlock()
		writeErr(w, r, http.StatusNotFound, "NOT_FOUND", fmt.Sprintf("session %s not found", s.id))
		return
	}
	s.seq++
	seq := s.seq
	c := newCommand(newSandboxID(), req.Command)
	s.cmds = append(s.cmds, c)
	s.mu.Unlock()

	spec := execSpec{Cmd: []string{"/bin/bash", "-c", sessionScript(f.sessionDir(), s.id, seq, req.Command)},
		Env: commandEnv(b.m, nil), Stdin: true, Detached: true}
	// The command outlives this request: it is started on a context of its own.
	conn, err := f.startExec(context.WithoutCancel(r.Context()), b.mach, spec)
	if err != nil {
		c.append(streamStderr, []byte(err.Error()+"\n"))
		c.finish(127)
		writeErr(w, r, http.StatusBadGateway, "", "Could not start the command: "+err.Error())
		return
	}
	c.mu.Lock()
	c.conn = conn
	c.mu.Unlock()
	go func() {
		defer conn.close()
		code, err := conn.run(c.append)
		if err != nil {
			// Its VM went away under it (a stop), or the session was deleted.
			code = 137
		}
		c.finish(code)
	}()
	if s.isGone() {
		s.end()
	}

	if async {
		writeJSON(w, http.StatusAccepted, map[string]any{"cmdId": c.id})
		return
	}
	for {
		_, _, exited, changed := c.since(0)
		if exited {
			break
		}
		select {
		case <-changed:
		case <-r.Context().Done():
			return
		}
	}
	out, stdout, stderr := c.logs()
	j := c.json()
	writeJSON(w, http.StatusOK, map[string]any{"cmdId": c.id, "exitCode": j.ExitCode, "output": out, "stdout": stdout, "stderr": stderr})
}

func (s *session) isGone() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.gone
}

func (f *Frontend) getCommand(w http.ResponseWriter, r *http.Request, b *box) {
	if _, c := f.lookupCommand(w, r, b); c != nil {
		writeJSON(w, http.StatusOK, c.json())
	}
}

var logsUpgrader = websocket.Upgrader{ReadBufferSize: 4 << 10, WriteBufferSize: 64 << 10,
	CheckOrigin: func(*http.Request) bool { return true }}

// commandLogs is GET .../command/{cid}/logs: the output so far as JSON, or,
// with follow=true over a WebSocket, all of it and then the rest as it
// comes, one binary frame per chunk with its stream's 3-byte prefix, closed
// (1000) when the command has exited and everything has been sent.
func (f *Frontend) commandLogs(w http.ResponseWriter, r *http.Request, b *box) {
	_, c := f.lookupCommand(w, r, b)
	if c == nil {
		return
	}
	if r.URL.Query().Get("follow") != "true" || !isWebSocket(r) {
		out, stdout, stderr := c.logs()
		if !strings.Contains(r.Header.Get("Accept"), "application/json") && strings.Contains(r.Header.Get("Accept"), "text/plain") {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.Write([]byte(out))
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"output": out, "stdout": stdout, "stderr": stderr})
		return
	}
	ws, err := logsUpgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer ws.Close()
	// Notice the client going away: its reads are all a WebSocket client does.
	gone := make(chan struct{})
	go func() {
		defer close(gone)
		for {
			if _, _, err := ws.ReadMessage(); err != nil {
				return
			}
		}
	}()
	next := 0
	for {
		frames, n, exited, changed := c.since(next)
		next = n
		for _, fr := range frames {
			prefix := stdoutPrefix
			if fr.stream == streamStderr {
				prefix = stderrPrefix
			}
			ws.SetWriteDeadline(time.Now().Add(30 * time.Second))
			if err := ws.WriteMessage(websocket.BinaryMessage, append(append([]byte(nil), prefix...), fr.data...)); err != nil {
				return
			}
		}
		if exited {
			ws.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""), time.Now().Add(time.Second))
			// Give the client a moment to read the close before the teardown.
			select {
			case <-gone:
			case <-time.After(time.Second):
			}
			return
		}
		select {
		case <-changed:
		case <-gone:
			return
		}
	}
}

// commandInput is POST .../command/{cid}/input: data to the command's stdin.
func (f *Frontend) commandInput(w http.ResponseWriter, r *http.Request, b *box) {
	_, c := f.lookupCommand(w, r, b)
	if c == nil {
		return
	}
	var req struct {
		Data string `json:"data"`
	}
	if !readBody(w, r, &req) {
		return
	}
	c.mu.Lock()
	conn, exited := c.conn, c.exited
	c.mu.Unlock()
	if exited || conn == nil {
		writeErr(w, r, http.StatusGone, "COMMAND_ALREADY_COMPLETED", fmt.Sprintf("command %s has already completed", c.id))
		return
	}
	if err := conn.stdin([]byte(req.Data)); err != nil {
		writeErr(w, r, http.StatusGone, "COMMAND_ALREADY_COMPLETED", "command input failed: "+err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
