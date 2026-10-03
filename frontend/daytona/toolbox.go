package daytona

import (
	"bytes"
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"net/http"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/arugula-salad/wisp/internal/store"
	"github.com/arugula-salad/wisp/internal/vmm"
)

// The toolbox: the API Daytona's SDKs reach inside a sandbox, at
// {toolboxProxyUrl}/{sandboxId}/... (docs/providers/daytona.md section 5).
// Hosted Daytona serves it from a daemon in the sandbox; here it is served on
// the host, each request translated to wisp-agent's API in the guest.

// box is the sandbox a toolbox request is for, with its VM held up for as
// long as the request lasts.
type box struct {
	rec  store.Record
	m    meta
	mach *vmm.Machine
}

type toolboxHandler func(http.ResponseWriter, *http.Request, *box)

func (f *Frontend) toolbox() http.Handler {
	mux := http.NewServeMux()
	p := "/toolbox/{id}"
	handle := func(pattern string, write bool, h toolboxHandler) {
		method, rest, _ := strings.Cut(pattern, " ")
		mux.HandleFunc(method+" "+p+rest, f.tb(write, h))
	}
	handle("GET /user-home-dir", false, func(w http.ResponseWriter, r *http.Request, b *box) {
		writeJSON(w, http.StatusOK, map[string]string{"dir": f.home()})
	})
	handle("GET /work-dir", false, func(w http.ResponseWriter, r *http.Request, b *box) {
		writeJSON(w, http.StatusOK, map[string]string{"dir": f.home()})
	})
	handle("GET /version", false, func(w http.ResponseWriter, r *http.Request, b *box) {
		writeJSON(w, http.StatusOK, map[string]string{"version": DaemonVersion})
	})
	handle("POST /process/execute", true, f.execute)
	handle("POST /process/code-run", true, f.codeRun)

	handle("POST /process/session", true, f.createSession)
	handle("GET /process/session", false, f.listSessions)
	handle("GET /process/session/{sid}", false, f.getSession)
	handle("DELETE /process/session/{sid}", true, f.deleteSession)
	handle("POST /process/session/{sid}/exec", true, f.sessionExec)
	handle("GET /process/session/{sid}/command/{cid}", false, f.getCommand)
	// A log follow is a WebSocket, which a read key may not open (as on the Sprites API).
	handle("GET /process/session/{sid}/command/{cid}/logs", false, f.commandLogs)
	handle("POST /process/session/{sid}/command/{cid}/input", true, f.commandInput)

	handle("GET /files", false, f.listFiles)
	handle("GET /files/info", false, f.fileInfo)
	handle("POST /files/folder", true, f.createFolder)
	handle("DELETE /files", true, f.deleteFile)
	handle("POST /files/move", true, f.moveFile)
	handle("GET /files/download", false, f.downloadFile)
	handle("POST /files/bulk-upload", true, f.bulkUpload)
	handle("POST /files/bulk-download", false, f.bulkDownload)

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeErr(w, r, http.StatusNotFound, "NOT_FOUND", "Not found: this Daytona-compatible toolbox does not implement "+r.Method+" "+r.URL.Path)
	})
	return mux
}

// home is the sandbox user's home, which is also the working directory.
func (f *Frontend) home() string {
	if f.homeDir != "" {
		return f.homeDir
	}
	return defaultHome
}

// tb authenticates a toolbox request (the bearer API key, or the sandbox's
// preview token as DAYTONA_SANDBOX_AUTH_KEY from clients that cannot send
// headers), finds the sandbox, refuses one that is not started, and holds
// its VM up while h runs.
func (f *Frontend) tb(write bool, h toolboxHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		rec, err := f.store.GetRecord(id)
		m, isOurs := metaOf(rec)
		admin, ok := f.checkKey(r)
		if !ok && isOurs && err == nil {
			if t := r.URL.Query().Get("DAYTONA_SANDBOX_AUTH_KEY"); t != "" && subtle.ConstantTimeCompare([]byte(t), []byte(m.PreviewToken)) == 1 {
				admin, ok = true, true
			}
		}
		if !ok {
			writeErr(w, r, http.StatusUnauthorized, "UNAUTHORIZED", "Invalid API key: use this daemon's root token or one of its API keys (wispd keys)")
			return
		}
		if write && !admin || !admin && isWebSocket(r) {
			writeErr(w, r, http.StatusForbidden, "FORBIDDEN", "This API key is read-only")
			return
		}
		if err != nil || !isOurs {
			notFound(w, r, id)
			return
		}
		b, release, ok := f.enter(w, r, id)
		if !ok {
			return
		}
		defer release()
		h(w, r, b)
	}
}

// enter decides, on the sandbox's metadata as it is now, that it is
// started, and holds its VM up until release; or answers why not. It holds
// the sandbox's gate for reading meanwhile, so that a stop marking the
// sandbox stopped cannot be overtaken by a request that saw it started.
func (f *Frontend) enter(w http.ResponseWriter, r *http.Request, id string) (*box, func(), bool) {
	g := f.gate(id)
	g.RLock()
	defer g.RUnlock()
	rec, err := f.store.GetRecord(id)
	m, ok := metaOf(rec)
	if err != nil || !ok {
		notFound(w, r, id)
		return nil, nil, false
	}
	if st := f.state(rec, m); st != "started" {
		writeErr(w, r, http.StatusConflict, "CONFLICT", fmt.Sprintf("Sandbox %s is %s: start it first", id, st))
		return nil, nil, false
	}
	mach, release, err := f.acquire(r.Context(), rec)
	if err != nil {
		f.bootFailed(w, r, err)
		return nil, nil, false
	}
	return &box{rec: rec, m: m, mach: mach}, release, true
}

func isWebSocket(r *http.Request) bool {
	return strings.EqualFold(r.Header.Get("Upgrade"), "websocket")
}

// agentFailed answers an error from the agent (or from reaching it) the way
// the toolbox answers it, with the code the SDK maps to a typed error.
func agentFailed(w http.ResponseWriter, r *http.Request, err error) {
	var ae *agentError
	if !errors.As(err, &ae) {
		writeErr(w, r, http.StatusBadGateway, "", "Could not reach the sandbox: "+err.Error())
		return
	}
	code := map[int]string{http.StatusNotFound: "FILE_NOT_FOUND", http.StatusForbidden: "FILE_ACCESS_DENIED",
		http.StatusConflict: "CONFLICT", http.StatusBadRequest: "BAD_REQUEST"}[ae.status]
	writeErr(w, r, ae.status, code, ae.msg)
}

// commandEnv is what every command in a sandbox starts with on top of the
// agent's own environment: the sandbox user's name (the agent's account is
// that user under another name, images/daytona), then the sandbox's env
// vars, then the request's.
func commandEnv(m meta, extra map[string]string) []string {
	env := []string{"USER=" + defaultUser, "LOGNAME=" + defaultUser, "DAYTONA_SANDBOX_USER=" + defaultUser}
	for _, vars := range []map[string]string{m.Env, extra} {
		keys := make([]string, 0, len(vars))
		for k := range vars {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			env = append(env, k+"="+vars[k])
		}
	}
	return env
}

// resolve is a toolbox path in the guest: relative paths are relative to the
// working directory.
func (f *Frontend) resolve(p string) string {
	if p == "" || path.IsAbs(p) {
		return p
	}
	return path.Join(f.home(), p)
}

// oneShot runs a command to completion (or until timeout seconds, 0 for
// none) and returns its combined output and exit code. A command that runs
// past its timeout is killed, and timedOut says so.
func (f *Frontend) oneShot(ctx context.Context, b *box, spec execSpec, input []byte, timeout int) (out []byte, code int, timedOut bool, err error) {
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, time.Duration(timeout)*time.Second)
		defer cancel()
	}
	spec.Stdin = input != nil
	c, err := f.startExec(ctx, b.mach, spec)
	if err != nil {
		return nil, 0, false, err
	}
	defer c.close()
	if input != nil {
		c.stdin(input)
		c.closeStdin()
	}
	stop := context.AfterFunc(ctx, func() { c.kill(); c.close() })
	defer stop()
	var buf bytes.Buffer
	code, err = c.run(func(_ byte, data []byte) { buf.Write(data) })
	if err != nil && ctx.Err() != nil {
		return buf.Bytes(), 0, errors.Is(ctx.Err(), context.DeadlineExceeded), ctx.Err()
	}
	return buf.Bytes(), code, false, err
}

// executeReq is ExecuteRequest.
type executeReq struct {
	Command string            `json:"command"`
	Cwd     string            `json:"cwd"`
	Envs    map[string]string `json:"envs"`
	Timeout int               `json:"timeout"`
}

// execute is POST /process/execute: one shell command, run with sh -c as the
// SDK's documentation describes it ("a shell command"), with its stdout and
// stderr combined in result.
func (f *Frontend) execute(w http.ResponseWriter, r *http.Request, b *box) {
	var req executeReq
	if !readBody(w, r, &req) {
		return
	}
	if strings.TrimSpace(req.Command) == "" {
		writeErr(w, r, http.StatusBadRequest, "BAD_REQUEST", "command is required")
		return
	}
	spec := execSpec{Cmd: []string{"/bin/sh", "-c", req.Command}, Env: commandEnv(b.m, req.Envs), Dir: f.resolve(req.Cwd)}
	f.runOneShot(w, r, b, spec, nil, req.Timeout, false)
}

// runOneShot runs spec and answers ExecuteResponse (or CodeRunResponse).
func (f *Frontend) runOneShot(w http.ResponseWriter, r *http.Request, b *box, spec execSpec, input []byte, timeout int, codeRun bool) {
	out, code, timedOut, err := f.oneShot(r.Context(), b, spec, input, timeout)
	if timedOut {
		writeErr(w, r, http.StatusRequestTimeout, "PROCESS_EXECUTION_TIMEOUT", fmt.Sprintf("command execution timeout after %d seconds", timeout))
		return
	}
	if err != nil {
		if r.Context().Err() != nil {
			return // the client went away
		}
		var ae *agentError
		if errors.As(err, &ae) {
			writeErr(w, r, http.StatusBadRequest, "BAD_REQUEST", ae.msg)
			return
		}
		writeErr(w, r, http.StatusBadGateway, "", "Command failed: "+err.Error())
		return
	}
	resp := map[string]any{"exitCode": code, "result": string(out)}
	if codeRun {
		resp["artifacts"] = map[string]any{"charts": []any{}}
	}
	writeJSON(w, http.StatusOK, resp)
}

// interpreters run code_run's languages (the code-toolbox-language label),
// from a file with that extension.
var interpreters = map[string]struct{ cmd, ext string }{
	"python":     {"python3", ".py"},
	"javascript": {"node", ".js"},
	"typescript": {"tsx", ".ts"},
}

// codeRun is POST /process/code-run: the code is written to a temporary
// file in the guest and run with the language's interpreter, with argv after
// it. Charts (hosted's matplotlib capture) are not produced.
func (f *Frontend) codeRun(w http.ResponseWriter, r *http.Request, b *box) {
	var req struct {
		Code     string            `json:"code"`
		Language string            `json:"language"`
		Argv     []string          `json:"argv"`
		Envs     map[string]string `json:"envs"`
		Timeout  int               `json:"timeout"`
	}
	if !readBody(w, r, &req) {
		return
	}
	lang := req.Language
	if lang == "" {
		lang = "python"
	}
	in, ok := interpreters[lang]
	if !ok {
		writeErr(w, r, http.StatusBadRequest, "BAD_REQUEST", fmt.Sprintf("Unsupported language %q: want python, javascript or typescript", lang))
		return
	}
	// The code arrives on stdin, so no quoting is involved; the program's own
	// stdin is then empty.
	script := `f=$(mktemp --suffix=` + in.ext + `) || exit 1; cat > "$f"; ` + in.cmd + ` "$f" "$@"; rc=$?; rm -f "$f"; exit $rc`
	spec := execSpec{Cmd: append([]string{"/bin/sh", "-c", script, "code-run"}, req.Argv...), Env: commandEnv(b.m, req.Envs)}
	f.runOneShot(w, r, b, spec, []byte(req.Code), req.Timeout, true)
}
