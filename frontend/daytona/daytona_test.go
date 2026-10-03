package daytona

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"mime"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/arugula-salad/wisp/engine"
	"github.com/arugula-salad/wisp/internal/agent"
	"github.com/arugula-salad/wisp/internal/store"
	"github.com/arugula-salad/wisp/internal/vmm"
	"github.com/gorilla/websocket"
)

const (
	adminKey = "admin-key"
	readKey  = "read-key"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

// fixture is the front end on a real engine and store with no VMs: a VM is
// a flag, and its guest is this host, where a real wisp-agent serves the
// toolbox's requests (as the current user, in a temporary home).
type fixture struct {
	t    *testing.T
	f    *Frontend
	srv  *httptest.Server
	st   *store.Store
	life *engine.Engine
	home string

	mu    sync.Mutex
	up    map[string]bool
	boots int
	// port serves every preview port.
	port http.Handler
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	dir := t.TempDir()
	base := filepath.Join(dir, "base.ext4")
	disk := filepath.Join(dir, "daytona.ext4")
	for _, p := range []string{base, disk} {
		if err := os.WriteFile(p, bytes.Repeat([]byte("disk"), 4096), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	life := engine.New(engine.Options{DataDir: dir, BaseImage: base, NoNetwork: true}, st, quiet)
	life.StartReaping()
	t.Cleanup(life.Shutdown)

	agentSrv := httptest.NewServer((&agent.Server{Sessions: agent.NewManager()}).Handler())
	t.Cleanup(agentSrv.Close)
	fx := &fixture{t: t, st: st, life: life, home: t.TempDir(), up: map[string]bool{}}
	fx.f = New(Options{Disk: disk, Domain: "daytona.test:7842", CheckKey: func(k string) (bool, bool) {
		switch k {
		case adminKey:
			return true, true
		case readKey:
			return false, true
		}
		return false, false
	}}, st, life, quiet)
	fx.f.homeDir, fx.f.stateDir = fx.home, filepath.Join(t.TempDir(), "sessions")
	fx.f.acquire = func(ctx context.Context, rec store.Record) (*vmm.Machine, func(), error) {
		fx.mu.Lock()
		defer fx.mu.Unlock()
		if !fx.up[rec.ID] {
			fx.boots++
		}
		fx.up[rec.ID] = true
		return nil, func() {}, nil
	}
	fx.f.stop = func(rec store.Record) error {
		fx.mu.Lock()
		defer fx.mu.Unlock()
		fx.up[rec.ID] = false
		return nil
	}
	fx.f.running = func(id string) (bool, bool) {
		fx.mu.Lock()
		defer fx.mu.Unlock()
		return fx.up[id], false
	}
	agentAddr := agentSrv.Listener.Addr().String()
	fx.f.agentDial = func(*vmm.Machine) func(ctx context.Context, network, addr string) (net.Conn, error) {
		return func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "tcp", agentAddr)
		}
	}
	fx.f.portDial = func(ctx context.Context, _ *vmm.Machine, port string) (net.Conn, error) {
		if fx.port == nil {
			return nil, &net.OpError{Op: "dial", Err: io.EOF}
		}
		c, s := net.Pipe()
		go (&http.Server{Handler: fx.port}).Serve(&oneConn{c: s})
		return c, nil
	}
	fx.srv = httptest.NewServer(fx.f.Handler())
	t.Cleanup(fx.srv.Close)
	return fx
}

// oneConn is a listener that accepts one connection.
type oneConn struct {
	c    net.Conn
	once sync.Once
	done chan struct{}
}

func (l *oneConn) Accept() (net.Conn, error) {
	var c net.Conn
	l.once.Do(func() { c, l.done = l.c, make(chan struct{}) })
	if c != nil {
		return c, nil
	}
	<-l.done
	return nil, net.ErrClosed
}
func (l *oneConn) Close() error   { return nil }
func (l *oneConn) Addr() net.Addr { return &net.UnixAddr{} }

type resp struct {
	code int
	hdr  http.Header
	body []byte
}

func (r resp) json(t *testing.T, v any) {
	t.Helper()
	if err := json.Unmarshal(r.body, v); err != nil {
		t.Fatalf("decode %s: %v", r.body, err)
	}
}

// do sends a request; body is JSON unless it is an io.Reader, and hdr are
// header pairs ("Host" sets the Host).
func (fx *fixture) do(method, path, key string, body any, hdr ...string) resp {
	fx.t.Helper()
	var rd io.Reader
	switch b := body.(type) {
	case nil:
	case io.Reader:
		rd = b
	default:
		j, _ := json.Marshal(b)
		rd = bytes.NewReader(j)
	}
	req, _ := http.NewRequest(method, fx.srv.URL+path, rd)
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		if strings.EqualFold(hdr[i], "Host") {
			req.Host = hdr[i+1]
		} else {
			req.Header.Set(hdr[i], hdr[i+1])
		}
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		fx.t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return resp{res.StatusCode, res.Header, b}
}

func (fx *fixture) create(body map[string]any) sandboxJSON {
	fx.t.Helper()
	r := fx.do("POST", "/api/sandbox", adminKey, body)
	if r.code != http.StatusOK {
		fx.t.Fatalf("create: %d %s", r.code, r.body)
	}
	var sb sandboxJSON
	r.json(fx.t, &sb)
	return sb
}

// requiredSandboxFields are the generated client's required Sandbox fields,
// each of which must be present (strict validation fails a missing one).
var requiredSandboxFields = []string{"id", "organizationId", "name", "user", "env", "labels", "public",
	"networkBlockAll", "kvm", "target", "cpu", "gpu", "memory", "disk", "toolboxProxyUrl"}

func TestSandboxLifecycle(t *testing.T) {
	fx := newFixture(t)
	r := fx.do("POST", "/api/sandbox", adminKey, map[string]any{"labels": map[string]string{"code-toolbox-language": "python", "team": "a"},
		"env": map[string]string{"A": "1"}, "autoStopInterval": 45, "cpu": 2, "memory": 2})
	if r.code != http.StatusOK {
		t.Fatalf("create: %d %s", r.code, r.body)
	}
	var raw map[string]any
	r.json(t, &raw)
	for _, k := range requiredSandboxFields {
		if _, ok := raw[k]; !ok {
			t.Errorf("Sandbox lacks required field %q", k)
		}
	}
	for _, k := range []string{"cpu", "gpu", "memory", "disk", "autoStopInterval"} {
		if n, ok := raw[k].(float64); !ok || n != float64(int(n)) {
			t.Errorf("%s = %v, want an integer", k, raw[k])
		}
	}
	var sb sandboxJSON
	r.json(t, &sb)
	if sb.State != "started" || sb.Name != sb.ID || sb.User != "daytona" || sb.CPU != 2 || sb.Memory != 2 || sb.AutoStopInterval != 45 {
		t.Fatalf("created: %+v", sb)
	}
	if want := fx.srv.URL + "/toolbox"; sb.ToolboxProxyURL != want {
		t.Errorf("toolboxProxyUrl %q, want %q (from the request's Host)", sb.ToolboxProxyURL, want)
	}
	rec, _ := fx.st.GetRecord(sb.ID)
	if rec.API != API || rec.Config.CPUs != 2 || rec.Config.RamMB != 2048 || rec.Lifecycle == nil ||
		rec.Lifecycle.IdleAction != store.IdleStop || rec.Lifecycle.IdleTimeout != 45*time.Minute {
		t.Fatalf("engine record: API %q config %+v policy %+v", rec.API, rec.Config, rec.Lifecycle)
	}
	if fx.boots != 1 {
		t.Errorf("create booted %d VMs, want 1 (the SDK waits for started)", fx.boots)
	}

	// Names are unique, and a sandbox is found by either.
	named := fx.create(map[string]any{"name": "web", "labels": map[string]string{"team": "b"}})
	if r := fx.do("POST", "/api/sandbox", adminKey, map[string]any{"name": "web"}); r.code != http.StatusConflict {
		t.Errorf("duplicate name: %d %s", r.code, r.body)
	}
	if r := fx.do("GET", "/api/sandbox/web", readKey, nil); r.code != 200 || !strings.Contains(string(r.body), named.ID) {
		t.Errorf("get by name: %d %s", r.code, r.body)
	}

	// The list: newest first, filtered by labels, nextCursor always present.
	var list struct {
		Items      []sandboxJSON `json:"items"`
		NextCursor *string       `json:"nextCursor"`
	}
	r = fx.do("GET", "/api/sandbox?labels="+url.QueryEscape(`{"team":"a"}`), readKey, nil)
	r.json(t, &list)
	if len(list.Items) != 1 || list.Items[0].ID != sb.ID || !strings.Contains(string(r.body), `"nextCursor":null`) {
		t.Fatalf("list by label: %s", r.body)
	}
	r = fx.do("GET", "/api/sandbox?limit=1", readKey, nil)
	r.json(t, &list)
	if len(list.Items) != 1 || list.Items[0].ID != named.ID || list.NextCursor == nil {
		t.Fatalf("first page: %s", r.body)
	}
	r = fx.do("GET", "/api/sandbox?limit=1&cursor="+*list.NextCursor, readKey, nil)
	r.json(t, &list)
	if len(list.Items) != 1 || list.Items[0].ID != sb.ID || list.NextCursor != nil {
		t.Fatalf("second page: %s", r.body)
	}
	r = fx.do("GET", "/api/sandbox?states=stopped", readKey, nil)
	r.json(t, &list)
	if len(list.Items) != 0 {
		t.Fatalf("states=stopped: %s", r.body)
	}

	// Labels, auto-stop (the engine's idle rule), preview and toolbox URLs.
	if r := fx.do("PUT", "/api/sandbox/"+sb.ID+"/labels", adminKey, map[string]any{"labels": map[string]string{"x": "y"}}); r.code != 200 || string(bytes.TrimSpace(r.body)) != `{"labels":{"x":"y"}}` {
		t.Errorf("labels: %d %s", r.code, r.body)
	}
	if r := fx.do("POST", "/api/sandbox/"+sb.ID+"/autostop/0", adminKey, nil); r.code != 200 || !strings.Contains(string(r.body), `"autoStopInterval":0`) {
		t.Errorf("autostop 0: %d %s", r.code, r.body)
	}
	if rec, _ := fx.st.GetRecord(sb.ID); rec.Lifecycle == nil || rec.Lifecycle.IdleAction != store.IdleNone {
		t.Errorf("autostop 0 left the idle rule %+v, want none", rec.Lifecycle)
	}
	var link map[string]string
	fx.do("GET", "/api/sandbox/"+sb.ID+"/ports/3000/preview-url", readKey, nil).json(t, &link)
	if link["url"] != "http://3000-"+sb.ID+".daytona.test:7842" || link["sandboxId"] != sb.ID || len(link["token"]) < 32 {
		t.Errorf("preview-url: %v", link)
	}
	if r := fx.do("GET", "/api/sandbox/"+sb.ID+"/toolbox-proxy-url", readKey, nil); !strings.Contains(string(r.body), fx.srv.URL+"/toolbox") {
		t.Errorf("toolbox-proxy-url: %s", r.body)
	}

	// Stop: stopped, and the toolbox refuses it rather than waking it; start.
	var st sandboxJSON
	fx.do("POST", "/api/sandbox/"+sb.ID+"/stop", adminKey, nil).json(t, &st)
	if st.State != "stopped" || fx.up[sb.ID] {
		t.Fatalf("stop: %s, VM up %v", st.State, fx.up[sb.ID])
	}
	if r := fx.do("GET", "/toolbox/"+sb.ID+"/work-dir", adminKey, nil); r.code != http.StatusConflict || fx.up[sb.ID] {
		t.Errorf("toolbox on a stopped sandbox: %d, VM up %v", r.code, fx.up[sb.ID])
	}
	fx.do("POST", "/api/sandbox/"+sb.ID+"/start", adminKey, nil).json(t, &st)
	if st.State != "started" || !fx.up[sb.ID] {
		t.Fatalf("start: %s", st.State)
	}
	// A sandbox whose VM went down without a stop (auto-stop) is stopped too.
	fx.f.stop(rec)
	fx.do("GET", "/api/sandbox/"+sb.ID, readKey, nil).json(t, &st)
	if st.State != "stopped" {
		t.Errorf("VM down: %s", st.State)
	}

	if r := fx.do("DELETE", "/api/sandbox/"+sb.ID, adminKey, nil); r.code != 200 {
		t.Fatalf("delete: %d %s", r.code, r.body)
	}
	if r := fx.do("GET", "/api/sandbox/"+sb.ID, readKey, nil); r.code != 404 || !strings.Contains(string(r.body), `"source":"DAYTONA_API"`) {
		t.Errorf("get after delete: %d %s", r.code, r.body)
	}
}

func TestControlPlaneRefusals(t *testing.T) {
	fx := newFixture(t)
	for _, c := range []struct {
		method, path, key string
		body              any
		code              int
	}{
		{"GET", "/api/sandbox", "", nil, 401},
		{"GET", "/api/sandbox", "nope", nil, 401},
		{"POST", "/api/sandbox", readKey, map[string]any{}, 403},
		{"POST", "/api/sandbox", adminKey, map[string]any{"snapshot": "ubuntu:22.04"}, 404},
		{"POST", "/api/sandbox", adminKey, map[string]any{"buildInfo": map[string]any{"dockerfileContent": "FROM debian:12\n"}}, 400},
		{"POST", "/api/sandbox", adminKey, map[string]any{"cpu": 0}, 400},
		{"POST", "/api/sandbox", adminKey, map[string]any{"user": "root"}, 400},
		{"GET", "/api/sandbox/no-such-sandbox", adminKey, nil, 404},
		{"GET", "/api/socket.io/?EIO=4&transport=websocket", adminKey, nil, 404},
		{"GET", "/api/snapshots", adminKey, nil, 404},
	} {
		if r := fx.do(c.method, c.path, c.key, c.body); r.code != c.code {
			t.Errorf("%s %s: %d %s, want %d", c.method, c.path, r.code, r.body, c.code)
		} else if !strings.Contains(string(r.body), `"statusCode":`) || !strings.Contains(string(r.body), `"message":`) {
			t.Errorf("%s %s: not the error envelope: %s", c.method, c.path, r.body)
		}
	}
}

// An ephemeral sandbox (auto-delete 0) is deleted once it stops, however it stops.
func TestEphemeralDeletedOnStop(t *testing.T) {
	fx := newFixture(t)
	sb := fx.create(map[string]any{"autoDeleteInterval": 0})
	keep := fx.create(map[string]any{})
	for _, id := range []string{sb.ID, keep.ID} {
		rec, _ := fx.st.GetRecord(id)
		fx.life.Emit(rec, "sprite.stopped", map[string]any{"reason": "idle"})
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := fx.st.GetRecord(sb.ID); err != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("an ephemeral sandbox outlived its stop")
		}
		time.Sleep(20 * time.Millisecond)
	}
	time.Sleep(100 * time.Millisecond)
	if _, err := fx.st.GetRecord(keep.ID); err != nil {
		t.Fatal("a sandbox with auto-delete off was deleted at a stop")
	}
}

func TestExecuteAndCodeRun(t *testing.T) {
	fx := newFixture(t)
	sb := fx.create(map[string]any{"env": map[string]string{"SANDBOX_VAR": "from-sandbox"}})
	tb := "/toolbox/" + sb.ID
	var out struct {
		ExitCode *int   `json:"exitCode"`
		Result   string `json:"result"`
	}
	r := fx.do("POST", tb+"/process/execute", adminKey, map[string]any{"command": "echo $SANDBOX_VAR $REQ_VAR | tr a-z A-Z; echo err >&2; pwd; exit 3",
		"cwd": "/", "envs": map[string]string{"REQ_VAR": "from-request"}})
	r.json(t, &out)
	// stdout and stderr are two pipes: combined, each keeps its own order.
	if r.code != 200 || out.ExitCode == nil || *out.ExitCode != 3 || !strings.HasPrefix(out.Result, "FROM-SANDBOX FROM-REQUEST\n") ||
		!strings.Contains(out.Result, "err\n") || !strings.Contains(out.Result, "/\n") || len(out.Result) != 32 {
		t.Fatalf("execute: %d %s", r.code, r.body)
	}
	if r := fx.do("POST", tb+"/process/execute", readKey, map[string]any{"command": "true"}); r.code != 403 {
		t.Errorf("execute with a read key: %d", r.code)
	}
	start := time.Now()
	r = fx.do("POST", tb+"/process/execute", adminKey, map[string]any{"command": "sleep 20", "timeout": 1})
	if r.code != http.StatusRequestTimeout || !strings.Contains(string(r.body), `"code":"PROCESS_EXECUTION_TIMEOUT"`) || time.Since(start) > 10*time.Second {
		t.Errorf("timeout: %d %s after %v", r.code, r.body, time.Since(start))
	}

	var cr struct {
		ExitCode  int            `json:"exitCode"`
		Result    string         `json:"result"`
		Artifacts map[string]any `json:"artifacts"`
	}
	r = fx.do("POST", tb+"/process/code-run", adminKey, map[string]any{"code": "import sys\nprint('sum', 10 + 20, sys.argv[1:])\nsys.exit(5)",
		"language": "python", "argv": []string{"a", "b c"}})
	r.json(t, &cr)
	if r.code != 200 || cr.ExitCode != 5 || cr.Result != "sum 30 ['a', 'b c']\n" || cr.Artifacts == nil {
		t.Fatalf("code-run: %d %s", r.code, r.body)
	}
	if r := fx.do("POST", tb+"/process/code-run", adminKey, map[string]any{"code": "x", "language": "cobol"}); r.code != 400 {
		t.Errorf("code-run in an unknown language: %d", r.code)
	}

	var dir map[string]string
	fx.do("GET", tb+"/work-dir", readKey, nil).json(t, &dir)
	if dir["dir"] != fx.home {
		t.Errorf("work-dir: %v", dir)
	}
	if r := fx.do("GET", tb+"/work-dir", "", nil); r.code != 401 || !strings.Contains(string(r.body), `"source":"DAYTONA_DAEMON"`) {
		t.Errorf("toolbox without a key: %d %s", r.code, r.body)
	}
	// Clients that cannot send headers use the preview token.
	var link map[string]string
	fx.do("GET", "/api/sandbox/"+sb.ID+"/ports/1/preview-url", readKey, nil).json(t, &link)
	if r := fx.do("GET", tb+"/work-dir?DAYTONA_SANDBOX_AUTH_KEY="+link["token"], "", nil); r.code != 200 {
		t.Errorf("toolbox with the preview token: %d %s", r.code, r.body)
	}
}

// followLogs reads a command's log stream to its close, as the SDKs do:
// binary frames, each one stream's chunk behind its 3-byte prefix.
func followLogs(t *testing.T, fx *fixture, path string) (stdout, stderr string, closeCode int) {
	t.Helper()
	u := "ws" + strings.TrimPrefix(fx.srv.URL, "http") + path + "?follow=true"
	ws, _, err := websocket.DefaultDialer.Dial(u, http.Header{"Authorization": {"Bearer " + adminKey}})
	if err != nil {
		t.Fatalf("follow: %v", err)
	}
	defer ws.Close()
	var so, se strings.Builder
	for {
		typ, data, err := ws.ReadMessage()
		if err != nil {
			if ce, ok := err.(*websocket.CloseError); ok {
				closeCode = ce.Code
			}
			return so.String(), se.String(), closeCode
		}
		if typ != websocket.BinaryMessage || len(data) < 3 {
			t.Fatalf("frame %d %q", typ, data)
		}
		switch string(data[:3]) {
		case "\x01\x01\x01":
			so.Write(data[3:])
		case "\x02\x02\x02":
			se.Write(data[3:])
		default:
			t.Fatalf("frame without a stream prefix: %q", data)
		}
	}
}

func TestSessions(t *testing.T) {
	fx := newFixture(t)
	sb := fx.create(map[string]any{})
	tb := "/toolbox/" + sb.ID + "/process/session"
	if r := fx.do("POST", tb, adminKey, map[string]any{"sessionId": "s1"}); r.code != http.StatusCreated {
		t.Fatalf("create session: %d %s", r.code, r.body)
	}
	if r := fx.do("POST", tb, adminKey, map[string]any{"sessionId": "s1"}); r.code != http.StatusConflict {
		t.Errorf("duplicate session: %d", r.code)
	}
	if r := fx.do("POST", tb, adminKey, map[string]any{"sessionId": "../x"}); r.code != http.StatusBadRequest {
		t.Errorf("session ID with a slash: %d", r.code)
	}
	type execResp struct {
		CmdID    string  `json:"cmdId"`
		ExitCode *int    `json:"exitCode"`
		Output   *string `json:"output"`
		Stdout   string  `json:"stdout"`
		Stderr   string  `json:"stderr"`
	}
	exec := func(cmd string, async bool) (int, execResp) {
		t.Helper()
		r := fx.do("POST", tb+"/s1/exec", adminKey, map[string]any{"command": cmd, "runAsync": async})
		var e execResp
		r.json(t, &e)
		return r.code, e
	}
	sub := filepath.Join(fx.home, "sub")
	os.Mkdir(sub, 0o755)
	if code, e := exec("cd "+sub+" && export FOO='a b' && BAR=unexported", false); code != 200 || e.ExitCode == nil || *e.ExitCode != 0 || e.CmdID == "" {
		t.Fatalf("first command: %d %+v", code, e)
	}
	code, e := exec(`pwd; echo "$FOO|$BAR"; echo oops >&2; exit 4`, false)
	if code != 200 || *e.ExitCode != 4 || e.Stdout != sub+"\na b|\n" || e.Stderr != "oops\n" || e.Output == nil || !strings.Contains(*e.Output, "oops") {
		t.Fatalf("state carried over (cwd and exports, not plain variables): %d %+v", code, e)
	}

	// Async: 202 with the ID only, then the log stream to its end.
	code, e = exec("for i in 1 2 3; do echo out$i; echo err$i >&2; sleep 0.1; done; exit 2", true)
	if code != http.StatusAccepted || e.CmdID == "" || e.ExitCode != nil {
		t.Fatalf("async: %d %+v", code, e)
	}
	so, se, cc := followLogs(t, fx, tb+"/s1/command/"+e.CmdID+"/logs")
	if so != "out1\nout2\nout3\n" || se != "err1\nerr2\nerr3\n" || cc != websocket.CloseNormalClosure {
		t.Fatalf("followed: stdout %q stderr %q close %d", so, se, cc)
	}
	var cmd commandJSON
	fx.do("GET", tb+"/s1/command/"+e.CmdID, readKey, nil).json(t, &cmd)
	if cmd.ExitCode == nil || *cmd.ExitCode != 2 || !strings.HasPrefix(cmd.Command, "for i") {
		t.Fatalf("command: %+v", cmd)
	}
	var logs map[string]string
	fx.do("GET", tb+"/s1/command/"+e.CmdID+"/logs", readKey, nil, "Accept", "application/json").json(t, &logs)
	if logs["stdout"] != "out1\nout2\nout3\n" || logs["stderr"] != "err1\nerr2\nerr3\n" || len(logs["output"]) != 30 {
		t.Fatalf("logs: %v", logs)
	}
	// A follow that starts after the command ended gets everything, then the close.
	if so, _, cc := followLogs(t, fx, tb+"/s1/command/"+e.CmdID+"/logs"); so != "out1\nout2\nout3\n" || cc != websocket.CloseNormalClosure {
		t.Fatalf("late follow: %q %d", so, cc)
	}

	// Input to a running command; a finished one is gone.
	_, e = exec("read line; echo got:$line", true)
	var c2 commandJSON
	fx.do("GET", tb+"/s1/command/"+e.CmdID, readKey, nil).json(t, &c2)
	if c2.ExitCode != nil {
		t.Fatalf("a command waiting for input has an exit code: %+v", c2)
	}
	if r := fx.do("POST", tb+"/s1/command/"+e.CmdID+"/input", adminKey, map[string]string{"data": "typed\n"}); r.code != http.StatusNoContent {
		t.Fatalf("input: %d %s", r.code, r.body)
	}
	if so, _, _ := followLogs(t, fx, tb+"/s1/command/"+e.CmdID+"/logs"); so != "got:typed\n" {
		t.Fatalf("after input: %q", so)
	}
	if r := fx.do("POST", tb+"/s1/command/"+e.CmdID+"/input", adminKey, map[string]string{"data": "x"}); r.code != http.StatusGone {
		t.Errorf("input to a finished command: %d", r.code)
	}

	var sess struct {
		SessionID string        `json:"sessionId"`
		Commands  []commandJSON `json:"commands"`
	}
	fx.do("GET", tb+"/s1", readKey, nil).json(t, &sess)
	if sess.SessionID != "s1" || len(sess.Commands) != 4 {
		t.Fatalf("session: %+v", sess)
	}
	var all []map[string]any
	fx.do("GET", tb, readKey, nil).json(t, &all)
	if len(all) != 1 || all[0]["sessionId"] != "s1" {
		t.Fatalf("sessions: %v", all)
	}

	// Deleting a session kills what it is running.
	_, e = exec("sleep 60", true)
	if r := fx.do("DELETE", tb+"/s1", adminKey, nil); r.code != http.StatusNoContent {
		t.Fatalf("delete session: %d", r.code)
	}
	if r := fx.do("GET", tb+"/s1", readKey, nil); r.code != 404 {
		t.Errorf("deleted session: %d", r.code)
	}
	if r := fx.do("POST", tb+"/nope/exec", adminKey, map[string]any{"command": "true"}); r.code != 404 {
		t.Errorf("exec in a missing session: %d", r.code)
	}
	if _, err := os.Stat(filepath.Join(fx.f.stateDir, "s1")); !os.IsNotExist(err) {
		t.Errorf("session state left behind: %v", err)
	}
}

// The session script restores the newest state, so an older command that
// finishes late does not undo a newer one's cd.
func TestSessionStateNewestWins(t *testing.T) {
	fx := newFixture(t)
	sb := fx.create(map[string]any{})
	tb := "/toolbox/" + sb.ID + "/process/session"
	fx.do("POST", tb, adminKey, map[string]any{"sessionId": "s"})
	a, b := filepath.Join(fx.home, "a"), filepath.Join(fx.home, "b")
	os.Mkdir(a, 0o755)
	os.Mkdir(b, 0o755)
	fx.do("POST", tb+"/s/exec", adminKey, map[string]any{"command": "cd " + a + "; sleep 1", "runAsync": true})
	fx.do("POST", tb+"/s/exec", adminKey, map[string]any{"command": "cd " + b})
	time.Sleep(1500 * time.Millisecond) // the first one finishes, and saves a
	var e struct {
		Stdout string `json:"stdout"`
	}
	fx.do("POST", tb+"/s/exec", adminKey, map[string]any{"command": "pwd"}).json(t, &e)
	if e.Stdout != b+"\n" {
		t.Fatalf("pwd %q, want %s", e.Stdout, b)
	}
}

func TestFiles(t *testing.T) {
	fx := newFixture(t)
	sb := fx.create(map[string]any{})
	tb := "/toolbox/" + sb.ID + "/files"
	if r := fx.do("POST", tb+"/folder?path=d/sub&mode=750", adminKey, nil); r.code != http.StatusCreated {
		t.Fatalf("folder: %d %s", r.code, r.body)
	}
	if st, err := os.Stat(filepath.Join(fx.home, "d/sub")); err != nil || st.Mode().Perm() != 0o750 {
		t.Fatalf("folder on disk: %v %v", st, err)
	}

	// Bulk upload: a path before its file, and a file before its path.
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	mw.WriteField("files[0].path", "d/one.txt")
	fw, _ := mw.CreateFormFile("files[0].file", "one.txt")
	fw.Write([]byte("first"))
	fw, _ = mw.CreateFormFile("files[1].file", "two.bin")
	fw.Write([]byte{0, 1, 2, 255})
	mw.WriteField("files[1].path", filepath.Join(fx.home, "d/sub/two.bin"))
	mw.Close()
	if r := fx.do("POST", tb+"/bulk-upload", adminKey, &body, "Content-Type", mw.FormDataContentType()); r.code != 200 {
		t.Fatalf("bulk-upload: %d %s", r.code, r.body)
	}
	if b, _ := os.ReadFile(filepath.Join(fx.home, "d/one.txt")); string(b) != "first" {
		t.Fatalf("uploaded: %q", b)
	}
	if b, _ := os.ReadFile(filepath.Join(fx.home, "d/sub/two.bin")); !bytes.Equal(b, []byte{0, 1, 2, 255}) {
		t.Fatalf("uploaded out of order: %q", b)
	}

	var infos []map[string]any
	r := fx.do("GET", tb+"?path=d", readKey, nil)
	r.json(t, &infos)
	if len(infos) != 2 || infos[0]["name"] != "one.txt" || infos[1]["name"] != "sub" || infos[1]["isDir"] != true {
		t.Fatalf("list: %s", r.body)
	}
	for _, k := range []string{"name", "isDir", "size", "mode", "permissions", "owner", "group", "modTime"} {
		if _, ok := infos[0][k]; !ok {
			t.Errorf("FileInfo lacks required field %q", k)
		}
	}
	if infos[0]["mode"] != "-rw-r--r--" || infos[0]["permissions"] != "0644" || infos[1]["mode"] != "drwxr-x---" {
		t.Errorf("modes: %v %v %v", infos[0]["mode"], infos[0]["permissions"], infos[1]["mode"])
	}
	var info fileInfoJSON
	fx.do("GET", tb+"/info?path=d/sub", readKey, nil).json(t, &info)
	if !info.IsDir || info.Name != "sub" {
		t.Errorf("info: %+v", info)
	}
	if r := fx.do("GET", tb+"/info?path=d/none", readKey, nil); r.code != 404 || !strings.Contains(string(r.body), `"code":"FILE_NOT_FOUND"`) {
		t.Errorf("info of a missing file: %d %s", r.code, r.body)
	}

	// Bulk download: one part per path, in order, an error part for a missing one.
	r = fx.do("POST", tb+"/bulk-download", readKey, map[string]any{"paths": []string{"d/one.txt", "d/missing", filepath.Join(fx.home, "d/sub/two.bin")}})
	mt, params, err := mime.ParseMediaType(r.hdr.Get("Content-Type"))
	if r.code != 200 || err != nil || mt != "multipart/form-data" || params["boundary"] == "" {
		t.Fatalf("bulk-download: %d %q %s", r.code, r.hdr.Get("Content-Type"), r.body)
	}
	mr := multipart.NewReader(bytes.NewReader(r.body), params["boundary"])
	type part struct{ name, filename, ctype, body string }
	var parts []part
	for {
		p, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(p)
		// Not p.FileName(), which keeps only the base name: the SDKs take the
		// filename parameter as it is, the whole path.
		_, cd, _ := mime.ParseMediaType(p.Header.Get("Content-Disposition"))
		parts = append(parts, part{p.FormName(), cd["filename"], p.Header.Get("Content-Type"), string(b)})
	}
	if len(parts) != 3 || parts[0] != (part{"file", "d/one.txt", "application/octet-stream", "first"}) ||
		parts[2].name != "file" || parts[2].body != "\x00\x01\x02\xff" {
		t.Fatalf("parts: %+v", parts)
	}
	var perr map[string]any
	json.Unmarshal([]byte(parts[1].body), &perr)
	if parts[1].name != "error" || parts[1].filename != "d/missing" || parts[1].ctype != "application/json" ||
		perr["code"] != "FILE_NOT_FOUND" || perr["statusCode"] != float64(404) {
		t.Fatalf("error part: %+v", parts[1])
	}

	if r := fx.do("GET", tb+"/download?path=d/one.txt", readKey, nil); r.code != 200 || string(r.body) != "first" {
		t.Errorf("download: %d %q", r.code, r.body)
	}
	if r := fx.do("POST", tb+"/move?source=d/one.txt&destination=d/moved.txt", adminKey, nil); r.code != 200 {
		t.Fatalf("move: %d %s", r.code, r.body)
	}
	if _, err := os.Stat(filepath.Join(fx.home, "d/moved.txt")); err != nil {
		t.Fatal(err)
	}
	if r := fx.do("DELETE", tb+"?path=d", adminKey, nil); r.code != http.StatusConflict {
		t.Errorf("non-recursive delete of a full directory: %d", r.code)
	}
	if r := fx.do("DELETE", tb+"?path=d&recursive=true", adminKey, nil); r.code != http.StatusNoContent {
		t.Fatalf("delete: %d %s", r.code, r.body)
	}
	if _, err := os.Stat(filepath.Join(fx.home, "d")); !os.IsNotExist(err) {
		t.Fatalf("deleted: %v", err)
	}
}

func TestPreview(t *testing.T) {
	fx := newFixture(t)
	fx.port = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "app at "+r.URL.Path+" token:"+r.Header.Get("X-Daytona-Preview-Token"))
	})
	sb := fx.create(map[string]any{})
	pub := fx.create(map[string]any{"public": true})
	var link map[string]string
	fx.do("GET", "/api/sandbox/"+sb.ID+"/ports/8080/preview-url", readKey, nil).json(t, &link)
	host := "8080-" + sb.ID + ".daytona.test:7842"
	if r := fx.do("GET", "/x", "", nil, "Host", host); r.code != http.StatusUnauthorized {
		t.Errorf("private preview without a token: %d %s", r.code, r.body)
	}
	if r := fx.do("GET", "/x", "", nil, "Host", host, "X-Daytona-Preview-Token", "wrong"); r.code != http.StatusUnauthorized {
		t.Errorf("wrong token: %d", r.code)
	}
	if r := fx.do("GET", "/x", "", nil, "Host", host, "X-Daytona-Preview-Token", link["token"]); r.code != 200 || string(r.body) != "app at /x token:" {
		t.Errorf("with the token: %d %q (the token must not reach the app)", r.code, r.body)
	}
	if r := fx.do("GET", "/y", adminKey, nil, "Host", host); r.code != 200 {
		t.Errorf("with an API key: %d", r.code)
	}
	if r := fx.do("GET", "/", "", nil, "Host", "3000-"+pub.ID+".daytona.test:7842"); r.code != 200 {
		t.Errorf("public preview: %d %s", r.code, r.body)
	}
	fx.do("POST", "/api/sandbox/"+sb.ID+"/stop", adminKey, nil)
	if r := fx.do("GET", "/x", adminKey, nil, "Host", host); r.code != http.StatusConflict || fx.up[sb.ID] {
		t.Errorf("preview of a stopped sandbox: %d (VM woken: %v)", r.code, fx.up[sb.ID])
	}
	fx.port = nil
	if r := fx.do("GET", "/", "", nil, "Host", "3000-"+pub.ID+".daytona.test"); r.code != http.StatusBadGateway {
		t.Errorf("nothing listening: %d", r.code)
	}
}
