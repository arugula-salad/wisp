package vercel

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
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
)

const (
	adminKey = "admin-key"
	readKey  = "read-key"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

// fixture is a front end on a real engine and store with no VMs: starting
// one is a counter, the guest agent is a real wisp-agent handler running
// commands on this machine (as this user, in a temporary home), and the
// guest's ports are whatever the test serves.
type fixture struct {
	t     *testing.T
	f     *Frontend
	h     http.Handler
	st    *store.Store
	life  *engine.Engine
	home  string
	agent *httptest.Server

	mu    sync.Mutex
	boots int
	now   time.Time
	// port serves the guest's ports when set.
	port http.Handler
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	dir := t.TempDir()
	base := filepath.Join(dir, "base.ext4")
	disk := filepath.Join(dir, "vercel.ext4")
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
	home := filepath.Join(dir, "home")
	os.MkdirAll(home, 0o755)
	ag := httptest.NewServer((&agent.Server{Sessions: agent.NewManager()}).Handler())
	t.Cleanup(ag.Close)
	fx := &fixture{t: t, st: st, life: life, home: home, agent: ag}
	fx.f = New(Options{Disk: disk, Home: home, Sudo: "-", Region: "test1",
		RouteURL: func(sub string) string { return "http://" + sub + ".vercel.test:7824" },
		CheckKey: func(k string) (bool, bool) {
			switch k {
			case adminKey:
				return true, true
			case readKey:
				return false, true
			}
			return false, false
		}}, st, life, quiet)
	fx.f.acquire = func(ctx context.Context, rec store.Record) (*vmm.Machine, func(), error) {
		fx.mu.Lock()
		fx.boots++
		fx.mu.Unlock()
		return nil, func() {}, nil
	}
	fx.f.dialAgent = func(*vmm.Machine) func(context.Context, string, string) (net.Conn, error) {
		return func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "tcp", ag.Listener.Addr().String())
		}
	}
	fx.f.dialPort = func(ctx context.Context, _ *vmm.Machine, port string) (net.Conn, error) {
		if fx.port == nil {
			return nil, &net.OpError{Op: "dial", Err: io.EOF}
		}
		c, s := net.Pipe()
		srv := &http.Server{Handler: fx.port}
		go srv.Serve(&oneConn{c: s})
		return c, nil
	}
	fx.f.now = func() time.Time {
		fx.mu.Lock()
		defer fx.mu.Unlock()
		if fx.now.IsZero() {
			return time.Now()
		}
		return fx.now
	}
	fx.h = fx.f.Handler()
	return fx
}

// later moves the front end's clock d ahead.
func (fx *fixture) later(d time.Duration) {
	fx.mu.Lock()
	fx.now = time.Now().Add(d)
	fx.mu.Unlock()
}

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

// do makes a request as the SDKs do: bearer token, teamId on every request,
// under the /api prefix.
func (fx *fixture) do(method, path, key string, body any) *httptest.ResponseRecorder {
	fx.t.Helper()
	var rd io.Reader
	switch b := body.(type) {
	case nil:
	case []byte:
		rd = bytes.NewReader(b)
	default:
		j, _ := json.Marshal(b)
		rd = bytes.NewReader(j)
	}
	sep := "?"
	if strings.Contains(path, "?") {
		sep = "&"
	}
	req := httptest.NewRequest(method, "http://127.0.0.1:7824/api"+path+sep+"teamId=team_x", rd)
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	fx.h.ServeHTTP(w, req)
	return w
}

func decode(t *testing.T, w *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("not JSON (%d): %q", w.Code, w.Body.String())
	}
	return out
}

// wantErr checks the error envelope.
func wantErr(t *testing.T, w *httptest.ResponseRecorder, status int, code, msg string) {
	t.Helper()
	if w.Code != status {
		t.Fatalf("status %d, want %d: %s", w.Code, status, w.Body.String())
	}
	e, _ := decode(t, w)["error"].(map[string]any)
	if e == nil || e["code"] != code || (msg != "" && e["message"] != msg) {
		t.Fatalf("error body %s, want code %q message %q", w.Body.String(), code, msg)
	}
}

// create makes a sandbox and returns its name and session ID.
func (fx *fixture) create(body map[string]any) (map[string]any, string) {
	fx.t.Helper()
	body["projectId"] = "prj_x"
	w := fx.do("POST", "/v3/sandboxes", adminKey, body)
	if w.Code != 200 {
		fx.t.Fatalf("create: %d %s", w.Code, w.Body.String())
	}
	out := decode(fx.t, w)
	return out, out["session"].(map[string]any)["id"].(string)
}

func TestAuthAndEnvelope(t *testing.T) {
	fx := newFixture(t)
	wantErr(t, fx.do("GET", "/v2/sandboxes", "", nil), 403, "forbidden", "")
	wantErr(t, fx.do("GET", "/v2/sandboxes", "nope", nil), 403, "forbidden", "")
	if w := fx.do("GET", "/v2/sandboxes?project=prj_x", readKey, nil); w.Code != 200 {
		t.Fatalf("read key list: %d", w.Code)
	}
	wantErr(t, fx.do("POST", "/v3/sandboxes", readKey, map[string]any{}), 403, "forbidden", "This API key is read-only.")
	wantErr(t, fx.do("GET", "/v2/sandboxes/missing?projectId=prj_x", adminKey, nil), 404, "not_found",
		"Named sandbox 'missing' not found for this project.")
	wantErr(t, fx.do("GET", "/v9/nothing", adminKey, nil), 404, "not_found", "")
	// No /api prefix works too.
	req := httptest.NewRequest("GET", "/v2/sandboxes", nil)
	req.Header.Set("Authorization", "Bearer "+adminKey)
	w := httptest.NewRecorder()
	fx.h.ServeHTTP(w, req)
	if w.Code != 200 || !strings.HasPrefix(w.Header().Get("Content-Type"), "application/json") {
		t.Fatalf("unprefixed: %d %v", w.Code, w.Header())
	}
}

func TestCreateGetListDelete(t *testing.T) {
	fx := newFixture(t)
	out, sid := fx.create(map[string]any{"name": "box-a", "ports": []int{3000}, "timeout": 240000,
		"resources": map[string]any{"vcpus": 1}, "persistent": false, "tags": map[string]string{"k": "v"}})
	sb := out["sandbox"].(map[string]any)
	if sb["name"] != "box-a" || sb["status"] != "running" || sb["persistent"] != false || sb["vcpus"] != 1.0 ||
		sb["memory"] != 2048.0 || sb["timeout"] != 240000.0 || sb["cwd"] != fx.home || sb["currentSessionId"] != sid || sb["expiresAt"] != nil {
		t.Fatalf("sandbox %v", sb)
	}
	if _, ok := sb["createdAt"].(float64); !ok {
		t.Fatalf("createdAt is not a number: %v", sb["createdAt"])
	}
	if !strings.HasPrefix(sid, "sbx_") || len(sid) != 32 {
		t.Fatalf("session ID %q", sid)
	}
	routes := out["routes"].([]any)
	r0 := routes[0].(map[string]any)
	if len(routes) != 1 || r0["port"] != 3000.0 || !subdomainRE.MatchString(r0["subdomain"].(string)) ||
		r0["url"] != "http://"+r0["subdomain"].(string)+".vercel.test:7824" {
		t.Fatalf("routes %v", routes)
	}
	if fx.boots != 1 {
		t.Fatalf("create booted %d times", fx.boots)
	}
	// The engine record: Vercel's namespace, the session timeout as a deadline whose action is stop.
	sp, err := fx.st.GetByName(API, "box-a")
	if err != nil || sp.Lifecycle == nil || sp.Lifecycle.DeadlineAction != store.DeadlineStop ||
		sp.Lifecycle.IdleAction != store.IdleNone || sp.ExpiresAt == nil || sp.Config.CPUs != 1 {
		t.Fatalf("record %+v %v", sp.Record, err)
	}
	if _, err := fx.st.GetByName(store.Sprites, "box-a"); err == nil {
		t.Fatal("a Vercel sandbox is in the Sprites namespace")
	}

	wantErr(t, fx.do("POST", "/v3/sandboxes", adminKey, map[string]any{"name": "box-a"}), 409, "sandbox_exists", "")
	wantErr(t, fx.do("POST", "/v3/sandboxes", adminKey, map[string]any{"resources": map[string]any{"vcpus": 3}}), 400, "bad_request", "")

	got := decode(t, fx.do("GET", "/v2/sandboxes/box-a?projectId=prj_x", adminKey, nil))
	if got["resumed"] != false || got["session"].(map[string]any)["id"] != sid || got["sandbox"].(map[string]any)["expiresAt"] == nil {
		t.Fatalf("get %v", got)
	}
	wantErr(t, fx.do("GET", "/v2/sandboxes/box-a?projectId=other", adminKey, nil), 404, "not_found", "")

	fx.create(map[string]any{"name": "box-b"})
	fx.create(map[string]any{"name": "other"})
	wantErr(t, fx.do("GET", "/v2/sandboxes?project=prj_x&namePrefix=box", adminKey, nil), 400, "bad_request",
		"Invalid request: `namePrefix` is only valid when `sortBy` is `name`")
	page := decode(t, fx.do("GET", "/v2/sandboxes?project=prj_x&namePrefix=box&sortBy=name&sortOrder=asc&limit=1", adminKey, nil))
	names := page["sandboxes"].([]any)
	pg := page["pagination"].(map[string]any)
	if len(names) != 1 || names[0].(map[string]any)["name"] != "box-a" || pg["count"] != 1.0 || pg["next"] == nil {
		t.Fatalf("page 1 %v", page)
	}
	page = decode(t, fx.do("GET", "/v2/sandboxes?project=prj_x&namePrefix=box&sortBy=name&sortOrder=asc&limit=1&cursor="+pg["next"].(string), adminKey, nil))
	if names = page["sandboxes"].([]any); len(names) != 1 || names[0].(map[string]any)["name"] != "box-b" ||
		page["pagination"].(map[string]any)["next"] != nil {
		t.Fatalf("page 2 %v", page)
	}

	del := decode(t, fx.do("DELETE", "/v2/sandboxes/box-a?projectId=prj_x&deleteOrphanSnapshots=true", adminKey, nil))
	if del["sandbox"].(map[string]any)["status"] != "stopped" {
		t.Fatalf("delete %v", del)
	}
	wantErr(t, fx.do("GET", "/v2/sandboxes/box-a", adminKey, nil), 404, "not_found", "")
	wantErr(t, fx.do("POST", "/v2/sandboxes/sessions/"+sid+"/cmd", adminKey, map[string]any{"command": "true"}), 404, "not_found", "")
}

// ndjsonLines reads a streamed NDJSON body.
func ndjsonLines(t *testing.T, w *httptest.ResponseRecorder) []map[string]any {
	t.Helper()
	if ct := w.Header().Get("Content-Type"); ct != "application/x-ndjson" {
		t.Fatalf("content-type %q, want exactly application/x-ndjson", ct)
	}
	var out []map[string]any
	sc := bufio.NewScanner(w.Body)
	for sc.Scan() {
		var m map[string]any
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			t.Fatalf("not a JSON line: %q", sc.Text())
		}
		out = append(out, m)
	}
	return out
}

func TestWaitedCommandNDJSON(t *testing.T) {
	fx := newFixture(t)
	_, sid := fx.create(map[string]any{"name": "cmds"})
	w := fx.do("POST", "/v2/sandboxes/sessions/"+sid+"/cmd?wait=true&logs=true", adminKey, map[string]any{
		"command": "sh", "args": []string{"-c", "echo out1; echo err1 >&2; echo \"$FOO\"; pwd; exit 3"},
		"env": map[string]string{"FOO": "bar"}, "sudo": false, "wait": true, "logs": true})
	if w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	lines := ndjsonLines(t, w)
	first, last := lines[0]["command"].(map[string]any), lines[len(lines)-1]["command"].(map[string]any)
	if first == nil || first["exitCode"] != nil || first["name"] != "sh" || first["cwd"] != fx.home || first["sessionId"] != sid ||
		!strings.HasPrefix(first["id"].(string), "cmd_") {
		t.Fatalf("first line %v", lines[0])
	}
	if last == nil || last["exitCode"] != 3.0 || last["id"] != first["id"] || last["durationMs"] == nil {
		t.Fatalf("last line %v", lines[len(lines)-1])
	}
	var stdout, stderr string
	for _, l := range lines[1 : len(lines)-1] {
		if l["command"] != nil {
			t.Fatalf("a command object mid-stream: %v", l)
		}
		switch l["stream"] {
		case "stdout":
			stdout += l["data"].(string)
		case "stderr":
			stderr += l["data"].(string)
		default:
			t.Fatalf("log line %v", l)
		}
	}
	if stdout != "out1\nbar\n"+fx.home+"\n" || stderr != "err1\n" {
		t.Fatalf("stdout %q stderr %q", stdout, stderr)
	}

	// cwd and a missing executable: 400, not a process with exit code 127.
	wantErr(t, fx.do("POST", "/v2/sandboxes/sessions/"+sid+"/cmd", adminKey, map[string]any{"command": "definitely-not-a-binary", "wait": true, "logs": true}),
		400, "executable_not_found", "[invalid_argument] executable file not found in $PATH: definitely-not-a-binary")
	lines = ndjsonLines(t, fx.do("POST", "/v2/sandboxes/sessions/"+sid+"/cmd", adminKey,
		map[string]any{"command": "pwd", "cwd": "/", "wait": true, "logs": true}))
	if lines[1]["data"] != "/\n" || lines[0]["command"].(map[string]any)["cwd"] != "/" {
		t.Fatalf("cwd: %v", lines)
	}
}

func TestDetachedCommandLogsKillWait(t *testing.T) {
	fx := newFixture(t)
	_, sid := fx.create(map[string]any{"name": "detached"})
	base := "/v2/sandboxes/sessions/" + sid + "/cmd"
	out := decode(t, fx.do("POST", base, adminKey, map[string]any{"command": "sh",
		"args": []string{"-c", `trap "echo got-term; exit 42" TERM; echo tick; while true; do sleep 0.05; done`}}))
	cmd := out["command"].(map[string]any)
	if cmd["exitCode"] != nil {
		t.Fatalf("detached command %v", cmd)
	}
	cid := cmd["id"].(string)
	if g := decode(t, fx.do("GET", base+"/"+cid, adminKey, nil))["command"].(map[string]any); g["exitCode"] != nil {
		t.Fatalf("get %v", g)
	}
	// Wait for the first line, so the trap is set before the signal.
	deadline := time.Now().Add(5 * time.Second)
	for c := fx.f.cmds.get(cid); ; time.Sleep(10 * time.Millisecond) {
		c.mu.Lock()
		n := len(c.logs)
		c.mu.Unlock()
		if n > 0 || time.Now().After(deadline) {
			break
		}
	}
	k := decode(t, fx.do("POST", base+"/"+cid+"/kill", adminKey, map[string]any{"signal": 15}))
	if k["command"].(map[string]any)["id"] != cid {
		t.Fatalf("kill %v", k)
	}
	done := decode(t, fx.do("GET", base+"/"+cid+"?wait=true", adminKey, nil))["command"].(map[string]any)
	if done["exitCode"] != 42.0 {
		t.Fatalf("after SIGTERM %v", done)
	}
	// Logs replay from the start, and end with the command.
	var all string
	for _, l := range ndjsonLines(t, fx.do("GET", base+"/"+cid+"/logs", adminKey, nil)) {
		all += l["data"].(string)
	}
	// (The signal goes to the process group, so sh may also report its sleep "Terminated".)
	if all = strings.Replace(all, "Terminated\n", "", 1); all != "tick\ngot-term\n" {
		t.Fatalf("logs %q", all)
	}
	// SIGKILL is 128+9.
	cid2 := decode(t, fx.do("POST", base, adminKey, map[string]any{"command": "sleep", "args": []string{"30"}}))["command"].(map[string]any)["id"].(string)
	fx.do("POST", base+"/"+cid2+"/kill", adminKey, map[string]any{"signal": 9})
	if c := decode(t, fx.do("GET", base+"/"+cid2+"?wait=true", adminKey, nil))["command"].(map[string]any); c["exitCode"] != 137.0 {
		t.Fatalf("after SIGKILL %v", c)
	}
	if l := decode(t, fx.do("GET", base, adminKey, nil))["commands"].([]any); len(l) != 2 {
		t.Fatalf("list %v", l)
	}
	wantErr(t, fx.do("GET", base+"/cmd_nope", adminKey, nil), 404, "not_found", "")
}

// tgz builds a gzip-compressed tar.
func tgz(t *testing.T, entries ...tar.Header) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	for _, h := range entries {
		body := h.Uname // the content rides in Uname, to keep the call sites short
		h.Uname = ""
		if h.Typeflag == tar.TypeReg {
			h.Size = int64(len(body))
		}
		if err := tw.WriteHeader(&h); err != nil {
			t.Fatal(err)
		}
		tw.Write([]byte(body))
	}
	tw.Close()
	zw.Close()
	return buf.Bytes()
}

func TestFilesWriteReadMkdir(t *testing.T) {
	fx := newFixture(t)
	_, sid := fx.create(map[string]any{"name": "files"})
	base := "/v2/sandboxes/sessions/" + sid + "/fs/"
	root := strings.TrimPrefix(fx.home, "/")
	body := tgz(t,
		tar.Header{Name: root + "/probe/hello.txt", Typeflag: tar.TypeReg, Mode: 0o644, Uname: "hello from wisp\n"},
		tar.Header{Name: root + "/probe/run.sh", Typeflag: tar.TypeReg, Mode: 0o755, Uname: "#!/bin/sh\necho ran\n"},
		tar.Header{Name: root + "/empty/dir/", Typeflag: tar.TypeDir, Mode: 0o755},
		tar.Header{Name: root + "/probe/link", Typeflag: tar.TypeSymlink, Linkname: "hello.txt"},
		tar.Header{Name: "../../" + root + "/escape.txt", Typeflag: tar.TypeReg, Mode: 0o600, Uname: "x"},
	)
	req := httptest.NewRequest("POST", "/api"+base+"write?teamId=t", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+adminKey)
	req.Header.Set("Content-Type", "application/gzip")
	req.Header.Set("x-cwd", "/")
	w := httptest.NewRecorder()
	fx.h.ServeHTTP(w, req)
	if w.Code != 200 || strings.TrimSpace(w.Body.String()) != "{}" {
		t.Fatalf("write: %d %s", w.Code, w.Body.String())
	}
	if fi, err := os.Stat(filepath.Join(fx.home, "probe/run.sh")); err != nil || fi.Mode().Perm() != 0o755 {
		t.Fatalf("run.sh: %v %v", fi, err)
	}
	if fi, err := os.Stat(filepath.Join(fx.home, "empty/dir")); err != nil || !fi.IsDir() {
		t.Fatalf("dir entry: %v", err)
	}
	if l, err := os.Readlink(filepath.Join(fx.home, "probe/link")); err != nil || l != "hello.txt" {
		t.Fatalf("symlink: %q %v", l, err)
	}
	if _, err := os.Stat(filepath.Join(fx.home, "escape.txt")); err != nil {
		t.Fatalf("a name with .. stays under x-cwd: %v", err)
	}

	// Relative paths resolve against the home server-side.
	w = fx.do("POST", base+"read", adminKey, map[string]any{"path": "probe/hello.txt"})
	if w.Code != 200 || w.Header().Get("Content-Type") != "application/octet-stream" || w.Body.String() != "hello from wisp\n" {
		t.Fatalf("read: %d %v %q", w.Code, w.Header(), w.Body.String())
	}
	w = fx.do("POST", base+"read", adminKey, map[string]any{"path": "hello.txt", "cwd": filepath.Join(fx.home, "probe")})
	if w.Body.String() != "hello from wisp\n" {
		t.Fatalf("read with cwd: %q", w.Body.String())
	}
	wantErr(t, fx.do("POST", base+"read", adminKey, map[string]any{"path": "probe/nope.txt"}), 404, "not_found", "File not found.")

	if w := fx.do("POST", base+"mkdir", adminKey, map[string]any{"path": "probe-dir"}); w.Code != 200 {
		t.Fatalf("mkdir: %d %s", w.Code, w.Body.String())
	}
	wantErr(t, fx.do("POST", base+"mkdir", adminKey, map[string]any{"path": "probe-dir"}), 400, "file_error",
		"error creating directory: "+filepath.Join(fx.home, "probe-dir")+": File exists")
	wantErr(t, fx.do("POST", base+"mkdir", adminKey, map[string]any{"path": "no/such/parent"}), 400, "file_error",
		"error creating directory: No such file or directory")
	if w := fx.do("POST", base+"mkdir", adminKey, map[string]any{"path": "no/such/parent", "recursive": true}); w.Code != 200 {
		t.Fatalf("recursive mkdir: %d %s", w.Code, w.Body.String())
	}

	// Not a gzip tar.
	req = httptest.NewRequest("POST", "/api"+base+"write", strings.NewReader("plain"))
	req.Header.Set("Authorization", "Bearer "+adminKey)
	w = httptest.NewRecorder()
	fx.h.ServeHTTP(w, req)
	wantErr(t, w, 400, "bad_request", "")
}

func TestStopResumeNonPersistent(t *testing.T) {
	fx := newFixture(t)
	_, sid := fx.create(map[string]any{"name": "np", "persistent": false})
	st := decode(t, fx.do("POST", "/v2/sandboxes/sessions/"+sid+"/stop", adminKey, map[string]any{}))
	if st["session"].(map[string]any)["status"] != "stopped" || st["session"].(map[string]any)["id"] != sid ||
		st["sandbox"].(map[string]any)["status"] != "stopped" || st["snapshot"] != nil {
		t.Fatalf("stop %v", st)
	}
	// Idempotent.
	again := decode(t, fx.do("POST", "/v2/sandboxes/sessions/"+sid+"/stop", adminKey, nil))
	if again["session"].(map[string]any)["stoppedAt"] != st["session"].(map[string]any)["stoppedAt"] {
		t.Fatalf("second stop %v", again)
	}
	// A stopped session is 410 (what makes the SDKs resume) ...
	wantErr(t, fx.do("POST", "/v2/sandboxes/sessions/"+sid+"/cmd", adminKey, map[string]any{"command": "true"}),
		410, "sandbox_stopped", "Sandbox has stopped execution and is no longer available")
	wantErr(t, fx.do("POST", "/v2/sandboxes/sessions/"+sid+"/fs/read", adminKey, map[string]any{"path": "x"}), 410, "sandbox_stopped", "")
	// ... and a non-persistent sandbox without a snapshot cannot resume.
	wantErr(t, fx.do("GET", "/v2/sandboxes/np?projectId=prj_x&resume=true", adminKey, nil), 400, "snapshot_not_found",
		"Cannot resume sandbox: no snapshot available.")
	got := decode(t, fx.do("GET", "/v2/sandboxes/np?resume=false", adminKey, nil))
	if got["sandbox"].(map[string]any)["status"] != "stopped" || got["resumed"] != false {
		t.Fatalf("get %v", got)
	}
}

func TestStopResumePersistent(t *testing.T) {
	fx := newFixture(t)
	_, sid := fx.create(map[string]any{"name": "pers", "timeout": 60000})
	st := decode(t, fx.do("POST", "/v2/sandboxes/sessions/"+sid+"/stop", adminKey, nil))
	snap, _ := st["snapshot"].(map[string]any)
	if snap == nil || snap["status"] != "created" || snap["sourceSessionId"] != sid ||
		st["sandbox"].(map[string]any)["currentSnapshotId"] != snap["id"] {
		t.Fatalf("persistent stop %v", st)
	}
	rec, _ := fx.st.GetByName(API, "pers")
	if len(rec.Checkpoints) != 1 || rec.ExpiresAt != nil {
		t.Fatalf("the stop's snapshot is a checkpoint, and the deadline is gone: %+v", rec.Record)
	}
	wantErr(t, fx.do("POST", "/v2/sandboxes/sessions/"+sid+"/cmd", adminKey, map[string]any{"command": "true"}), 410, "sandbox_stopped", "")
	boots := fx.boots
	res := decode(t, fx.do("GET", "/v2/sandboxes/pers?projectId=prj_x&resume=true", adminKey, nil))
	sess := res["session"].(map[string]any)
	if res["resumed"] != true || sess["id"] == sid || sess["status"] != "running" || sess["sourceSnapshotId"] != snap["id"] ||
		sess["timeout"] != 60000.0 || fx.boots != boots+1 {
		t.Fatalf("resume %v", res)
	}
	rec, _ = fx.st.GetByName(API, "pers")
	if rec.ExpiresAt == nil {
		t.Fatal("the new session has no deadline")
	}
	// The old session is still 410; the new one runs commands.
	wantErr(t, fx.do("POST", "/v2/sandboxes/sessions/"+sid+"/cmd", adminKey, map[string]any{"command": "true"}), 410, "sandbox_stopped", "")
	newSID := sess["id"].(string)
	if w := fx.do("POST", "/v2/sandboxes/sessions/"+newSID+"/cmd", adminKey, map[string]any{"command": "true", "wait": true, "logs": true}); w.Code != 200 {
		t.Fatalf("command in the new session: %d", w.Code)
	}
	// A second stop replaces the stop snapshot rather than piling them up.
	st2 := decode(t, fx.do("POST", "/v2/sandboxes/sessions/"+newSID+"/stop", adminKey, nil))
	rec, _ = fx.st.GetByName(API, "pers")
	m, _ := metaOf(rec.Record)
	if len(rec.Checkpoints) != 1 || len(m.Snapshots) != 1 || m.Snapshots[0].ID != st2["snapshot"].(map[string]any)["id"] {
		t.Fatalf("snapshots after two stops: %+v %+v", rec.Checkpoints, m.Snapshots)
	}
}

func TestExtendAndTimeout(t *testing.T) {
	fx := newFixture(t)
	_, sid := fx.create(map[string]any{"name": "t", "timeout": 60000, "persistent": true})
	ext := decode(t, fx.do("POST", "/v2/sandboxes/sessions/"+sid+"/extend-timeout", adminKey, map[string]any{"duration": 30000}))
	s := ext["session"].(map[string]any)
	if s["timeout"] != 90000.0 {
		t.Fatalf("extend %v", ext)
	}
	rec, _ := fx.st.GetByName(API, "t")
	if want := int64(s["startedAt"].(float64)) + 90000; rec.ExpiresAt == nil || rec.ExpiresAt.UnixMilli() != want {
		t.Fatalf("engine deadline %v, want %d", rec.ExpiresAt, want)
	}
	if sb := decode(t, fx.do("GET", "/v2/sandboxes/t", adminKey, nil))["sandbox"].(map[string]any); sb["timeout"] != 60000.0 {
		t.Fatalf("the sandbox's own timeout changed: %v", sb)
	}
	wantErr(t, fx.do("POST", "/v2/sandboxes/sessions/"+sid+"/extend-timeout", adminKey, map[string]any{"duration": 0}), 400, "bad_request", "")
	// Past the deadline the session is over (the engine stops the VM; the front
	// end notices the next time anything looks) and a persistent one is snapshotted.
	fx.later(2 * time.Minute)
	got := decode(t, fx.do("GET", "/v2/sandboxes/t", adminKey, nil))
	gs := got["session"].(map[string]any)
	if gs["status"] != "stopped" || gs["stoppedAt"] != gs["startedAt"].(float64)+90000 || got["sandbox"].(map[string]any)["currentSnapshotId"] == nil {
		t.Fatalf("after the timeout %v", got)
	}
	wantErr(t, fx.do("POST", "/v2/sandboxes/sessions/"+sid+"/cmd", adminKey, map[string]any{"command": "true"}), 410, "sandbox_stopped", "")
}

func TestSnapshots(t *testing.T) {
	fx := newFixture(t)
	_, sid := fx.create(map[string]any{"name": "src", "persistent": false})
	wantErr(t, fx.do("POST", "/v2/sandboxes/sessions/"+sid+"/snapshot", adminKey, map[string]any{"expiration": 1000}), 400, "bad_request",
		"`expiration` must be 0 (no expiration) or >= 86400000 ms (1 day).")
	w := fx.do("POST", "/v2/sandboxes/sessions/"+sid+"/snapshot", adminKey, map[string]any{"expiration": 86400000})
	if w.Code != 201 {
		t.Fatalf("snapshot: %d %s", w.Code, w.Body.String())
	}
	out := decode(t, w)
	snap := out["snapshot"].(map[string]any)
	id := snap["id"].(string)
	if !strings.HasPrefix(id, "snap_") || snap["status"] != "created" || snap["creationMethod"] != "manual" ||
		snap["expiresAt"] == nil || out["session"].(map[string]any)["status"] != "snapshotting" {
		t.Fatalf("snapshot %v", out)
	}
	// The session is over; the sandbox resumes from the snapshot, persistent or not.
	wantErr(t, fx.do("POST", "/v2/sandboxes/sessions/"+sid+"/cmd", adminKey, map[string]any{"command": "true"}), 410, "sandbox_stopped", "")
	if g := decode(t, fx.do("GET", "/v2/sandboxes/snapshots/"+id, adminKey, nil)); g["snapshot"].(map[string]any)["id"] != id {
		t.Fatalf("get snapshot %v", g)
	}
	if l := decode(t, fx.do("GET", "/v2/sandboxes/snapshots?project=prj_x", adminKey, nil))["snapshots"].([]any); len(l) != 1 {
		t.Fatalf("list snapshots %v", l)
	}
	// A new sandbox from it.
	b, _ := fx.create(map[string]any{"name": "copy", "source": map[string]any{"type": "snapshot", "snapshotId": id}})
	if b["session"].(map[string]any)["sourceSnapshotId"] != id || b["sandbox"].(map[string]any)["currentSnapshotId"] != id {
		t.Fatalf("create from snapshot %v", b)
	}
	wantErr(t, fx.do("POST", "/v3/sandboxes", adminKey, map[string]any{"name": "c2", "source": map[string]any{"type": "snapshot", "snapshotId": "snap_nope"}}),
		404, "snapshot_not_found", "")
	res := decode(t, fx.do("GET", "/v2/sandboxes/src?resume=true", adminKey, nil))
	if res["resumed"] != true || res["session"].(map[string]any)["sourceSnapshotId"] != id {
		t.Fatalf("resume from the snapshot %v", res)
	}
	del := decode(t, fx.do("DELETE", "/v2/sandboxes/snapshots/"+id, adminKey, nil))
	if del["snapshot"].(map[string]any)["status"] != "deleted" {
		t.Fatalf("delete snapshot %v", del)
	}
	wantErr(t, fx.do("GET", "/v2/sandboxes/snapshots/"+id, adminKey, nil), 404, "not_found", "")
}

func TestRoutes(t *testing.T) {
	fx := newFixture(t)
	out, sid := fx.create(map[string]any{"name": "web", "ports": []int{3000}})
	sub := out["routes"].([]any)[0].(map[string]any)["subdomain"].(string)
	fx.port = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "served "+r.URL.Path) })
	get := func(u, host string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("GET", u, nil)
		if host != "" {
			req.Host = host
		}
		w := httptest.NewRecorder()
		fx.h.ServeHTTP(w, req)
		return w
	}
	if w := get("http://x/index.html", sub+".vercel.test:7824"); w.Code != 200 || w.Body.String() != "served /index.html" {
		t.Fatalf("host route: %d %q", w.Code, w.Body.String())
	}
	if w := get("http://127.0.0.1:7824/"+sub+"/a/b?c=d", ""); w.Code != 200 || w.Body.String() != "served /a/b" {
		t.Fatalf("path route: %d %q", w.Code, w.Body.String())
	}
	if w := get("http://127.0.0.1:7824/"+sub, ""); w.Body.String() != "served /" {
		t.Fatalf("path route root: %q", w.Body.String())
	}
	fx.port = nil
	if w := get("http://x/", sub+".vercel.test"); w.Code != 502 {
		t.Fatalf("closed port: %d", w.Code)
	}
	fx.do("POST", "/v2/sandboxes/sessions/"+sid+"/stop", adminKey, nil)
	boots := fx.boots
	if w := get("http://x/", sub+".vercel.test"); w.Code != 410 || fx.boots != boots {
		t.Fatalf("a stopped sandbox's route: %d (boots %d -> %d)", w.Code, boots, fx.boots)
	}
	// Unknown subdomains are not routes.
	if w := get("http://127.0.0.1:7824/sb-000000000000", ""); w.Code != 404 {
		t.Fatalf("unknown subdomain: %d", w.Code)
	}
}

func TestMkdirMessage(t *testing.T) {
	for _, c := range []struct{ dir, stderr, want string }{
		{"/vercel/x", "mkdir: cannot create directory '/vercel/x': File exists\n", "error creating directory: /vercel/x: File exists"},
		{"/vercel/a/b", "mkdir: cannot create directory '/vercel/a/b': No such file or directory", "error creating directory: No such file or directory"},
		{"/etc/x", "mkdir: cannot create directory '/etc/x': Permission denied", "error creating directory: /etc/x: Permission denied"},
	} {
		if got := mkdirMessage(c.dir, c.stderr); got != c.want {
			t.Errorf("%q: %q, want %q", c.stderr, got, c.want)
		}
	}
}

// The daemon's sandbox limit counts every API's records, and a Vercel create
// past it is a 429 with a Retry-After the JS SDK will not wait out.
func TestSandboxLimitCountsEveryAPI(t *testing.T) {
	fx := newFixture(t)
	fx.f.opts.MaxSandboxes = 2
	if err := fx.st.Create(&store.Sprite{Record: store.Record{ID: store.NewID()}, SpriteMeta: store.SpriteMeta{Name: "a-sprite"}}); err != nil {
		t.Fatal(err)
	}
	fx.create(map[string]any{"name": "one"})
	w := fx.do("POST", "/v3/sandboxes", adminKey, map[string]any{"name": "two"})
	wantErr(t, w, 429, "too_many_sandboxes", "The concurrency limit has been exceeded: this server holds at most 2 sandboxes.")
	if w.Header().Get("Retry-After") != "60" {
		t.Fatalf("Retry-After %q", w.Header().Get("Retry-After"))
	}
	if _, err := fx.st.GetByName(API, "two"); err == nil {
		t.Fatal("the refused sandbox exists")
	}
	fx.do("DELETE", "/v2/sandboxes/one", adminKey, nil)
	fx.create(map[string]any{"name": "two"})
}

// Ports are numbers in 1-65535, once each; a route dials its port written
// plainly, and a route's path is cleaned before it is matched or forwarded.
func TestPortsAndRoutePaths(t *testing.T) {
	fx := newFixture(t)
	for _, ports := range []any{[]int{0}, []int{70000}, []int{-1}, []int{3000, 3000}, []string{"3000"}, []any{3000.5}} {
		wantErr(t, fx.do("POST", "/v3/sandboxes", adminKey, map[string]any{"name": "bad", "ports": ports}), 400, "bad_request", "")
	}
	out, _ := fx.create(map[string]any{"name": "web", "ports": []int{3000}})
	sub := out["routes"].([]any)[0].(map[string]any)["subdomain"].(string)
	var dialed []string
	inner := fx.f.dialPort
	fx.f.dialPort = func(ctx context.Context, m *vmm.Machine, port string) (net.Conn, error) {
		dialed = append(dialed, port)
		return inner(ctx, m, port)
	}
	fx.port = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, r.URL.Path) })
	get := func(p string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("GET", "http://127.0.0.1:7824/", nil)
		req.URL.Path = p
		w := httptest.NewRecorder()
		fx.h.ServeHTTP(w, req)
		return w
	}
	for p, want := range map[string]string{
		"/" + sub + "/a/../b//c/": "/b/c/",
		"/./" + sub + "/x":        "/x",
		"/" + sub + "/../" + sub:  "/",
		"/" + sub + "/../../etc":  "", // climbs out of the route: not a route (the API redirects it)
	} {
		w := get(p)
		if want == "" {
			if w.Code == 200 {
				t.Errorf("%s reached the port: %q", p, w.Body.String())
			}
			continue
		}
		if w.Code != 200 || w.Body.String() != want {
			t.Errorf("%s: %d %q, want %q", p, w.Code, w.Body.String(), want)
		}
	}
	for _, d := range dialed {
		if d != "3000" {
			t.Errorf("dialed port %q", d)
		}
	}
	// A route whose stored port is not a port is no route.
	sp, _ := fx.st.GetByName(API, "web")
	fx.f.updateMeta(sp.ID, func(m *meta) { m.Routes[0].Port = 0 })
	if w := get("/" + sub + "/"); w.Code != 404 {
		t.Errorf("a route to port 0: %d", w.Code)
	}
}

// A session marked stopping (as a stop marks it before it stops the VM) is
// not woken by traffic: commands, files and routes are refused with
// sandbox_stopping, which the SDKs resume on, and nothing boots.
func TestStoppingSessionIsNotWoken(t *testing.T) {
	fx := newFixture(t)
	out, sid := fx.create(map[string]any{"name": "s", "ports": []int{3000}})
	sub := out["routes"].([]any)[0].(map[string]any)["subdomain"].(string)
	sp, _ := fx.st.GetByName(API, "s")
	fx.f.updateMeta(sp.ID, func(m *meta) { m.current().Status = "stopping" })
	boots := fx.boots
	wantErr(t, fx.do("POST", "/v2/sandboxes/sessions/"+sid+"/cmd", adminKey, map[string]any{"command": "true"}), 422, "sandbox_stopping", "")
	wantErr(t, fx.do("POST", "/v2/sandboxes/sessions/"+sid+"/fs/read", adminKey, map[string]any{"path": "x"}), 422, "sandbox_stopping", "")
	req := httptest.NewRequest("GET", "http://127.0.0.1:7824/"+sub+"/", nil)
	w := httptest.NewRecorder()
	fx.h.ServeHTTP(w, req)
	wantErr(t, w, 422, "sandbox_stopping", "")
	if fx.boots != boots {
		t.Fatalf("traffic to a stopping sandbox booted it (%d -> %d)", boots, fx.boots)
	}
	// The metadata is re-read under the lock: a handler holding a stale
	// "running" record is still refused.
	rec, m, s, _ := fx.f.findSession(sid)
	s.Status, m.current().Status = "running", "running"
	w = httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/", strings.NewReader(`{"command":"true"}`))
	fx.f.runCommand(w, r, rec, m, s)
	wantErr(t, w, 422, "sandbox_stopping", "")
	// A stop decides and marks under the sandbox's lock, the one traffic takes.
	fx.f.updateMeta(sp.ID, func(m *meta) { m.current().Status = "running" })
	unlock := fx.f.lock(sp.ID)
	done := make(chan struct{})
	go func() {
		defer close(done)
		// Blocks on the lock the test holds; the stop then marks first.
		fx.do("POST", "/v2/sandboxes/sessions/"+sid+"/stop", adminKey, nil)
	}()
	time.Sleep(50 * time.Millisecond)
	if m, _ := metaOf(mustRecord(t, fx, sp.ID)); m.current().Status != "running" {
		t.Fatalf("the stop ran without the lock: %s", m.current().Status)
	}
	unlock()
	<-done
	m2, _ := metaOf(mustRecord(t, fx, sp.ID))
	if m2.current().Status != "stopped" || m2.current().RequestedStopAt == 0 {
		t.Fatalf("after the stop: %+v", m2.current())
	}
}

func mustRecord(t *testing.T, fx *fixture, id string) store.Record {
	t.Helper()
	rec, err := fx.st.GetRecord(id)
	if err != nil {
		t.Fatal(err)
	}
	return rec
}

// Review 1: a snapshot whose checkpoint fails still ends its session stopped
// (its VM is), with no snapshot and the failure answered; a later stop answers
// at once rather than finding a session stuck stopping.
func TestFailedSnapshotEndsStopped(t *testing.T) {
	fx := newFixture(t)
	_, sid := fx.create(map[string]any{"name": "snapfail", "persistent": false})
	fx.f.checkpoint = func(store.Record, string) (store.Checkpoint, error) {
		return store.Checkpoint{}, errors.New("disk full")
	}
	w := fx.do("POST", "/v2/sandboxes/sessions/"+sid+"/snapshot", adminKey, nil)
	wantErr(t, w, 500, "internal_server_error", "")
	if !strings.Contains(w.Body.String(), "disk full") {
		t.Fatalf("the failure is not surfaced: %s", w.Body.String())
	}
	sp, _ := fx.st.GetByName(API, "snapfail")
	m, _ := metaOf(sp.Record)
	if c := m.current(); c.Status != "stopped" || c.StoppedAt == 0 || len(m.Snapshots) != 0 || m.CurrentSnapshotID != "" {
		t.Fatalf("after a failed snapshot: %+v snapshots %v", *c, m.Snapshots)
	}
	st := decode(t, fx.do("POST", "/v2/sandboxes/sessions/"+sid+"/stop", adminKey, nil))
	if st["session"].(map[string]any)["status"] != "stopped" {
		t.Fatalf("stop after a failed snapshot: %v", st)
	}
	// The same for a persistent sandbox's stop: stopped, without a snapshot.
	_, sid2 := fx.create(map[string]any{"name": "stopfail"})
	st = decode(t, fx.do("POST", "/v2/sandboxes/sessions/"+sid2+"/stop", adminKey, nil))
	if st["session"].(map[string]any)["status"] != "stopped" || st["snapshot"] != nil {
		t.Fatalf("persistent stop with a failing checkpoint: %v", st)
	}
}

// Review 2: a stop or snapshot that waited for the sandbox's lock while a stop
// and a resume replaced its session acts on nothing: the new session runs on.
func TestQueuedStopDoesNotStopTheNextSession(t *testing.T) {
	fx := newFixture(t)
	_, sid1 := fx.create(map[string]any{"name": "q", "persistent": true})
	sp, _ := fx.st.GetByName(API, "q")
	for _, op := range []string{"stop", "snapshot"} {
		cur, _ := metaOf(mustRecord(t, fx, sp.ID))
		old := cur.current().ID
		unlock := fx.f.lock(sp.ID)
		done := make(chan *httptest.ResponseRecorder)
		go func() { done <- fx.do("POST", "/v2/sandboxes/sessions/"+old+"/"+op, adminKey, nil) }()
		time.Sleep(50 * time.Millisecond)
		// Meanwhile, under the lock, the session was stopped and a resume started another.
		next := session{ID: newSessionID(), RequestedAt: fx.f.ms(), StartedAt: fx.f.ms(), Timeout: 60000, Status: "running"}
		fx.f.updateMeta(sp.ID, func(m *meta) {
			c := m.current()
			c.Status, c.StoppedAt = "stopped", fx.f.ms()
			m.addSession(next)
		})
		fx.f.sessions.Store(next.ID, sp.ID)
		unlock()
		w := <-done
		m, _ := metaOf(mustRecord(t, fx, sp.ID))
		if m.current().ID != next.ID || m.current().Status != "running" {
			t.Fatalf("a queued %s of %s acted on %s: %+v", op, old, next.ID, *m.current())
		}
		switch op {
		case "stop":
			if w.Code != 200 || decode(t, w)["session"].(map[string]any)["id"] != old {
				t.Fatalf("queued stop: %d %s", w.Code, w.Body.String())
			}
		case "snapshot":
			wantErr(t, w, 410, "sandbox_stopped", "")
		}
	}
	_ = sid1
}

// Review 3: timeouts are compared in ms, so a huge one cannot overflow past
// the limit.
func TestHugeTimeoutsAreRefused(t *testing.T) {
	fx := newFixture(t)
	wantErr(t, fx.do("POST", "/v3/sandboxes", adminKey, map[string]any{"name": "h", "timeout": int64(9300000000000)}), 400, "bad_request", "")
	wantErr(t, fx.do("POST", "/v3/sandboxes", adminKey, map[string]any{"name": "h", "timeout": int64(math.MaxInt64)}), 400, "bad_request", "")
	_, sid := fx.create(map[string]any{"name": "h", "timeout": 60000})
	for _, d := range []int64{9300000000000, math.MaxInt64, int64(24*time.Hour/time.Millisecond) - 59999} {
		wantErr(t, fx.do("POST", "/v2/sandboxes/sessions/"+sid+"/extend-timeout", adminKey, map[string]any{"duration": d}), 400, "bad_request", "")
	}
	ext := decode(t, fx.do("POST", "/v2/sandboxes/sessions/"+sid+"/extend-timeout", adminKey,
		map[string]any{"duration": int64(24*time.Hour/time.Millisecond) - 60000}))
	if ext["session"].(map[string]any)["timeout"] != float64(24*time.Hour/time.Millisecond) {
		t.Fatalf("extend to exactly the maximum: %v", ext)
	}
}

// Review 4: listings with equal sort keys page through every item once.
func TestPagingWithTies(t *testing.T) {
	fx := newFixture(t)
	_, sid := fx.create(map[string]any{"name": "ties"})
	sp, _ := fx.st.GetByName(API, "ties")
	fx.f.updateMeta(sp.ID, func(m *meta) {
		at := m.current().RequestedAt
		for i := 0; i < 5; i++ {
			m.addSession(session{ID: newSessionID(), RequestedAt: at, StartedAt: at, Status: "stopped"})
			m.Snapshots = append(m.Snapshots, snapshot{ID: newSnapshotID(), CreatedAt: at, Method: "manual"})
		}
	})
	_ = sid
	for _, c := range []struct{ path, field string }{
		{"/v2/sandboxes/sessions?name=ties", "sessions"},
		{"/v2/sandboxes/snapshots?name=ties", "snapshots"},
	} {
		for _, order := range []string{"asc", "desc"} {
			seen := map[string]bool{}
			cursor := ""
			for pages := 0; ; pages++ {
				out := decode(t, fx.do("GET", c.path+"&limit=2&sortOrder="+order+cursor, adminKey, nil))
				for _, it := range out[c.field].([]any) {
					id := it.(map[string]any)["id"].(string)
					if seen[id] {
						t.Fatalf("%s %s: %s twice", c.field, order, id)
					}
					seen[id] = true
				}
				next := out["pagination"].(map[string]any)["next"]
				if next == nil || pages > 10 {
					break
				}
				cursor = "&cursor=" + next.(string)
			}
			want := 6
			if c.field == "snapshots" {
				want = 5
			}
			if len(seen) != want {
				t.Fatalf("%s %s: %d items, want %d", c.field, order, len(seen), want)
			}
		}
	}
}

// ordered is a strict weak order in both directions: irreflexive, and never
// true both ways, ties included (the old (a < b) == asc was true both ways for
// equal keys when descending).
func TestOrderedIsStrict(t *testing.T) {
	for _, asc := range []bool{true, false} {
		if ordered(5, 5, "a", "a", asc) {
			t.Errorf("asc=%v: an item comes before itself", asc)
		}
		if ordered(5, 5, "a", "b", asc) == ordered(5, 5, "b", "a", asc) {
			t.Errorf("asc=%v: a tie is not broken by ID one way", asc)
		}
		if ordered(4, 5, "z", "a", asc) != asc || ordered(5, 4, "a", "z", asc) == asc {
			t.Errorf("asc=%v: keys do not decide first", asc)
		}
	}
}
