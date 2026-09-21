package agent

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func newControlServer(t *testing.T) (*httptest.Server, *Server) {
	srv := &Server{Sessions: NewManager()}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts, srv
}

// opResult is everything one op put on the socket, up to its closing envelope.
type opResult struct {
	stdout, stderr bytes.Buffer
	texts          []map[string]any // the op's own JSON messages
	closing        map[string]any   // op.complete / op.error
	prefixed       bool
}

func (r *opResult) text(typ string) map[string]any {
	for _, m := range r.texts {
		if m["type"] == typ {
			return m
		}
	}
	return nil
}

func startOp(t *testing.T, conn *websocket.Conn, op string, args map[string]any) {
	t.Helper()
	b, _ := json.Marshal(map[string]any{"type": "op.start", "op": op, "args": args})
	if err := conn.WriteMessage(websocket.TextMessage, append([]byte(controlPrefix), b...)); err != nil {
		t.Fatal(err)
	}
}

// readOp reads a non-TTY exec op (or any op without binary framing needs) to its closing envelope.
func readOp(t *testing.T, conn *websocket.Conn) *opResult {
	t.Helper()
	r := &opResult{}
	conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	for {
		typ, data, err := conn.ReadMessage()
		if err != nil {
			t.Fatalf("socket ended before the op did: %v (stdout=%q texts=%v)", err, r.stdout.String(), r.texts)
		}
		if typ == websocket.BinaryMessage {
			switch data[0] {
			case StreamStdout:
				r.stdout.Write(data[1:])
			case StreamStderr:
				r.stderr.Write(data[1:])
			}
			continue
		}
		rest, prefixed := bytes.CutPrefix(data, []byte(controlPrefix))
		var m map[string]any
		if err := json.Unmarshal(rest, &m); err != nil {
			t.Fatalf("unparseable text frame %q", data)
		}
		if typ, _ := m["type"].(string); strings.HasPrefix(typ, "op.") {
			r.closing, r.prefixed = m, prefixed
			return r
		}
		r.texts = append(r.texts, m)
	}
}

func exitCodeOf(r *opResult) int {
	args, _ := r.closing["args"].(map[string]any)
	code, ok := args["exitCode"].(float64)
	if r.closing["type"] != "op.complete" || !ok {
		return -1
	}
	return int(code)
}

func TestControlRunsSequentialExecsOnOneSocket(t *testing.T) {
	ts, srv := newControlServer(t)
	conn := dial(t, ts, "/control", nil)

	startOp(t, conn, "exec", map[string]any{"cmd": []string{"sh", "-c", "echo one; echo oops >&2; exit 3"}, "path": "sh", "env": []string{"A=1"}})
	r := readOp(t, conn)
	if r.stdout.String() != "one\n" || r.stderr.String() != "oops\n" || exitCodeOf(r) != 3 || !r.prefixed {
		t.Fatalf("first op: stdout=%q stderr=%q closing=%v", r.stdout.String(), r.stderr.String(), r.closing)
	}
	if info := r.text("session_info"); info == nil || info["is_owner"] != true {
		t.Errorf("session_info = %v", info)
	}
	if exit := r.text("exit"); exit == nil || exit["exit_code"] != float64(3) {
		t.Errorf("exit message = %v", exit)
	}

	// The same socket, now with stdin and a working directory.
	startOp(t, conn, "exec", map[string]any{"cmd": []string{"sh", "-c", "cat; pwd; echo $B"}, "stdin": "true", "dir": "/tmp", "env": []string{"B=two"}})
	conn.WriteMessage(websocket.BinaryMessage, append([]byte{StreamStdin}, "piped "...))
	conn.WriteMessage(websocket.BinaryMessage, []byte{StreamStdinEOF})
	r = readOp(t, conn)
	if r.stdout.String() != "piped /tmp\ntwo\n" || exitCodeOf(r) != 0 {
		t.Fatalf("second op: stdout=%q closing=%v", r.stdout.String(), r.closing)
	}

	// Without stdin=true the command must see EOF rather than wait for a frame that never comes.
	startOp(t, conn, "exec", map[string]any{"cmd": []string{"cat"}})
	if r = readOp(t, conn); exitCodeOf(r) != 0 {
		t.Fatalf("third op: closing=%v", r.closing)
	}

	if opened, ops := srv.control.opened.Load(), srv.control.ops.Load(); opened != 1 || ops != 3 {
		t.Fatalf("server saw %d control sockets and %d ops, want 1 and 3", opened, ops)
	}
}

func TestControlTTY(t *testing.T) {
	ts, _ := newControlServer(t)
	conn := dial(t, ts, "/control", nil)
	startOp(t, conn, "exec", map[string]any{"cmd": []string{"sh"}, "tty": "true", "stdin": "true", "rows": "30", "cols": "100"})
	conn.WriteMessage(websocket.BinaryMessage, []byte("stty size; exit 5\n"))
	var out bytes.Buffer
	conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	for {
		typ, data, err := conn.ReadMessage()
		if err != nil {
			t.Fatal(err)
		}
		if typ == websocket.BinaryMessage {
			out.Write(data)
		} else if bytes.HasPrefix(data, []byte(controlPrefix)) {
			if !bytes.Contains(data, []byte(`"exitCode":5`)) || !strings.Contains(out.String(), "30 100") {
				t.Fatalf("closing=%s out=%q", data, out.String())
			}
			return
		}
	}
}

func TestControlErrorsLeaveTheSocketUsable(t *testing.T) {
	ts, _ := newControlServer(t)
	conn := dial(t, ts, "/control", nil)

	startOp(t, conn, "exec", map[string]any{"cmd": []string{"definitely-not-a-binary"}})
	r := readOp(t, conn)
	if r.closing["type"] != "op.error" || r.text("error") == nil || r.text("exit") == nil {
		t.Fatalf("missing executable: texts=%v closing=%v", r.texts, r.closing)
	}
	startOp(t, conn, "exec", map[string]any{"id": "999"})
	if r = readOp(t, conn); r.closing["type"] != "op.error" {
		t.Fatalf("attach to unknown session: %v", r.closing)
	}
	startOp(t, conn, "teleport", nil)
	if r = readOp(t, conn); r.closing["type"] != "op.error" {
		t.Fatalf("unknown op: %v", r.closing)
	}

	// A second op.start while one runs is refused without disturbing the first.
	startOp(t, conn, "exec", map[string]any{"cmd": []string{"sh", "-c", "read x; echo got $x"}, "stdin": "true"})
	startOp(t, conn, "exec", map[string]any{"cmd": []string{"echo", "intruder"}})
	conn.WriteMessage(websocket.BinaryMessage, append([]byte{StreamStdin}, "it\n"...))
	if r = readOp(t, conn); r.closing["type"] != "op.error" {
		t.Fatalf("overlapping op.start: %v", r.closing)
	}
	if r = readOp(t, conn); r.stdout.String() != "got it\n" || exitCodeOf(r) != 0 {
		t.Fatalf("running op after a refused one: stdout=%q closing=%v", r.stdout.String(), r.closing)
	}
}

func TestControlReleaseDetachesAndAttachReplaysFromOffset(t *testing.T) {
	ts, srv := newControlServer(t)
	conn := dial(t, ts, "/control", nil)
	startOp(t, conn, "exec", map[string]any{"cmd": []string{"sh", "-c", "echo first; sleep 1; echo second; sleep 60"}})
	var id string
	var seen bytes.Buffer
	for id == "" || seen.String() != "first\n" {
		typ, data, err := conn.ReadMessage()
		if err != nil {
			t.Fatal(err)
		}
		if typ == websocket.TextMessage {
			var m map[string]any
			json.Unmarshal(data, &m)
			id, _ = m["session_id"].(string)
		} else if data[0] == StreamStdout {
			seen.Write(data[1:])
		}
	}

	// The Go SDK sends release bare, not as a control: frame.
	conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"release"}`))
	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, attached := srv.Sessions.Activity(); attached == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("release did not detach the session")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if l := srv.Sessions.List(); len(l) != 1 || l[0].ID != id {
		t.Fatalf("released session should keep running: %+v", l)
	}

	// Attach by id on the same socket, skipping what was already delivered, then signal it.
	startOp(t, conn, "exec", map[string]any{"id": id, "output_offset": strconv.Itoa(seen.Len())})
	go func() {
		time.Sleep(1500 * time.Millisecond)
		conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"signal","signal":"KILL"}`))
	}()
	r := readOp(t, conn)
	if r.stdout.String() != "second\n" || exitCodeOf(r) != 137 {
		t.Fatalf("after attach: stdout=%q closing=%v", r.stdout.String(), r.closing)
	}
	if info := r.text("session_info"); info == nil || info["is_owner"] != false || info["session_id"] != id {
		t.Errorf("session_info on attach = %v", info)
	}
}

func TestControlSocketLossDetaches(t *testing.T) {
	ts, srv := newControlServer(t)
	conn := dial(t, ts, "/control", nil)
	startOp(t, conn, "exec", map[string]any{"cmd": []string{"sleep", "60"}, "max_run_after_disconnect": "200ms"})
	conn.ReadMessage()
	conn.Close()
	deadline := time.Now().Add(3 * time.Second)
	for len(srv.Sessions.List()) > 0 {
		if time.Now().After(deadline) {
			t.Fatal("session outlived its control socket and the grace period")
		}
		time.Sleep(50 * time.Millisecond)
	}
	if _, attached := srv.Sessions.Activity(); attached != 0 {
		t.Fatalf("a dead socket still pins the sprite: attached=%d", attached)
	}
}

func TestControlAutoStartFromQuery(t *testing.T) {
	ts, _ := newControlServer(t)
	conn := dial(t, ts, "/control", url.Values{"op": {"exec"}, "cmd": {"echo", "auto"}, "env": {"FROM_SPRITE=1"}})
	if r := readOp(t, conn); r.stdout.String() != "auto\n" || exitCodeOf(r) != 0 {
		t.Fatalf("stdout=%q closing=%v", r.stdout.String(), r.closing)
	}
	// env on the URL is the sprite-level environment spritesd supplies; an op's own env wins.
	startOp(t, conn, "exec", map[string]any{"cmd": []string{"sh", "-c", "echo $FROM_SPRITE"}, "env": []string{"FROM_SPRITE=2"}})
	if r := readOp(t, conn); r.stdout.String() != "2\n" {
		t.Fatalf("stdout=%q", r.stdout.String())
	}
	startOp(t, conn, "exec", map[string]any{"cmd": []string{"sh", "-c", "echo $FROM_SPRITE"}})
	if r := readOp(t, conn); r.stdout.String() != "1\n" {
		t.Fatalf("stdout=%q", r.stdout.String())
	}
}

func TestControlManyConcurrentExecs(t *testing.T) {
	ts, srv := newControlServer(t)
	const n = 40
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			u := "ws" + strings.TrimPrefix(ts.URL, "http") + "/control"
			conn, _, err := websocket.DefaultDialer.Dial(u, nil)
			if err != nil {
				errs <- err
				return
			}
			defer conn.Close()
			for round := 0; round < 3; round++ {
				want := fmt.Sprintf("%d-%d", i, round)
				b, _ := json.Marshal(map[string]any{"type": "op.start", "op": "exec", "args": map[string]any{"cmd": []string{"echo", want}}})
				conn.WriteMessage(websocket.TextMessage, append([]byte(controlPrefix), b...))
				var out bytes.Buffer
				for {
					typ, data, err := conn.ReadMessage()
					if err != nil {
						errs <- err
						return
					}
					if typ == websocket.BinaryMessage && data[0] == StreamStdout {
						out.Write(data[1:])
					} else if bytes.HasPrefix(data, []byte(controlPrefix)) {
						break
					}
				}
				if out.String() != want+"\n" {
					errs <- fmt.Errorf("socket %d round %d got %q", i, round, out.String())
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	if opened, ops := srv.control.opened.Load(), srv.control.ops.Load(); opened != n || ops != 3*n {
		t.Fatalf("server saw %d sockets and %d ops, want %d and %d", opened, ops, n, 3*n)
	}
}

// The Go SDK's proxy speaks its own envelope and expects the plain /proxy handshake back.
func TestControlProxy(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				buf := make([]byte, 64)
				n, _ := c.Read(buf)
				if string(buf[:n]) == "bye" {
					return // hang up without answering
				}
				c.Write(bytes.ToUpper(buf[:n]))
				io.Copy(io.Discard, c)
			}()
		}
	}()
	port := ln.Addr().(*net.TCPAddr).Port

	ts, srv := newControlServer(t)
	conn := dial(t, ts, "/control", nil)
	start := func() {
		conn.WriteJSON(map[string]any{"type": "start", "operation": "proxy",
			"params": map[string]any{"host": "127.0.0.1", "port": port, "keep_alive": true}})
		var resp map[string]string
		if err := conn.ReadJSON(&resp); err != nil || resp["status"] != "connected" {
			t.Fatalf("handshake: %v %v", resp, err)
		}
	}

	start()
	conn.WriteMessage(websocket.BinaryMessage, []byte("hello"))
	if _, data, err := conn.ReadMessage(); err != nil || string(data) != "HELLO" {
		t.Fatalf("relayed %q %v", data, err)
	}
	if _, attached := srv.Sessions.Activity(); attached != 1 {
		t.Fatalf("an open proxy op should hold the sprite awake: attached=%d", attached)
	}
	conn.WriteJSON(map[string]string{"type": "release"})

	// Released, the socket takes another op.
	startOp(t, conn, "exec", map[string]any{"cmd": []string{"echo", "reused"}})
	if r := readOp(t, conn); r.stdout.String() != "reused\n" {
		t.Fatalf("exec after proxy: %q", r.stdout.String())
	}

	// A refused connection is an error, not the end of the socket.
	conn.WriteJSON(map[string]any{"type": "start", "operation": "proxy", "params": map[string]any{"host": "127.0.0.1", "port": 1}})
	var resp map[string]string
	if err := conn.ReadJSON(&resp); err != nil || resp["status"] != "error" {
		t.Fatalf("refused dial: %v %v", resp, err)
	}
	if r := readOp(t, conn); r.closing["type"] != "op.error" || r.prefixed {
		t.Fatalf("refused dial closing = %v prefixed=%v", r.closing, r.prefixed)
	}

	// When the target hangs up the client must see EOF, which for the SDK means a close.
	start()
	conn.WriteMessage(websocket.BinaryMessage, []byte("bye"))
	if r := readOp(t, conn); r.closing["type"] != "op.complete" {
		t.Fatalf("closing = %v", r.closing)
	}
	if _, _, err := conn.ReadMessage(); !websocket.IsCloseError(err, websocket.CloseNormalClosure) {
		t.Fatalf("want a normal close after the target hung up, got %v", err)
	}
}

// Filesystem ops in the Go SDK's dialect: a bare envelope with url-encoded args.
func TestControlFilesystemOps(t *testing.T) {
	ts, _ := newControlServer(t)
	conn := dial(t, ts, "/control", nil)
	dir := t.TempDir()
	op := func(name string, args url.Values, content []byte) (map[string]any, []byte, map[string]any) {
		t.Helper()
		args.Set("workingDir", dir)
		conn.WriteJSON(map[string]any{"type": "op.start", "op": name, "args": args.Encode()})
		if content != nil {
			conn.WriteMessage(websocket.BinaryMessage, content)
		}
		conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		var answer map[string]any
		if err := conn.ReadJSON(&answer); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if answer["error"] != nil {
			return answer, nil, nil
		}
		var data []byte
		if name == "fs.read" {
			typ, d, err := conn.ReadMessage()
			if err != nil || typ != websocket.BinaryMessage {
				t.Fatalf("fs.read content: type=%d err=%v", typ, err)
			}
			data = d
		}
		var closing map[string]any
		if err := conn.ReadJSON(&closing); err != nil { // bare, so it parses without stripping a prefix
			t.Fatalf("%s closing envelope: %v", name, err)
		}
		return answer, data, closing
	}

	answer, _, closing := op("fs.write", url.Values{"path": {"sub/a.txt"}, "mode": {"0600"}}, []byte("content"))
	if answer["size"] != float64(7) || answer["mode"] != "0600" || closing["type"] != "op.complete" {
		t.Fatalf("write: %v %v", answer, closing)
	}
	if info, err := os.Stat(filepath.Join(dir, "sub/a.txt")); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("written file: %v %v", info, err)
	}
	answer, data, _ := op("fs.read", url.Values{"path": {"sub/a.txt"}}, nil)
	if string(data) != "content" || answer["size"] != float64(7) {
		t.Fatalf("read: %v %q", answer, data)
	}
	if answer, _, _ = op("fs.rename", url.Values{"source": {"sub/a.txt"}, "dest": {"sub/b.txt"}}, nil); answer["dest"] != filepath.Join(dir, "sub/b.txt") {
		t.Fatalf("rename: %v", answer)
	}
	if answer, _, _ = op("fs.copy", url.Values{"source": {"sub"}, "dest": {"dup"}, "recursive": {"true"}}, nil); answer["count"] != float64(1) {
		t.Fatalf("copy: %v", answer)
	}
	if answer, _, _ = op("fs.chmod", url.Values{"path": {"dup/b.txt"}, "mode": {"0644"}}, nil); answer["count"] != float64(1) {
		t.Fatalf("chmod: %v", answer)
	}
	if answer, _, _ = op("fs.chown", url.Values{"path": {"dup/b.txt"}, "uid": {strconv.Itoa(os.Getuid())}}, nil); answer["count"] != float64(1) {
		t.Fatalf("chown: %v", answer)
	}
	answer, _, _ = op("fs.list", url.Values{"path": {"dup"}}, nil)
	if entries, _ := answer["entries"].([]any); len(entries) != 1 || entries[0].(map[string]any)["mode"] != "0644" {
		t.Fatalf("list: %v", answer)
	}
	answer, _, _ = op("fs.delete", url.Values{"path": {"dup"}, "recursive": {"true"}}, nil)
	if deleted, _ := answer["deleted"].([]any); len(deleted) != 1 || answer["count"] != float64(1) {
		t.Fatalf("delete: %v", answer)
	}
	// A failure is the error body and nothing after it, so the next op's answer is not mistaken for it.
	if answer, _, _ = op("fs.read", url.Values{"path": {"dup/b.txt"}}, nil); answer["code"] != "not_found" {
		t.Fatalf("read of a deleted file: %v", answer)
	}
	if answer, _, _ = op("fs.list", url.Values{"path": {"sub"}}, nil); answer["count"] != float64(1) {
		t.Fatalf("list after an error: %v", answer)
	}
}

func TestIdlePresuspendClosesIdleControlSocketsAndYieldsToBusyOnes(t *testing.T) {
	ts, srv := newControlServer(t)
	presuspend := func(query string) int {
		resp, err := http.Post(ts.URL+"/internal/presuspend"+query, "", nil)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}

	busy := dial(t, ts, "/control", nil)
	startOp(t, busy, "exec", map[string]any{"cmd": []string{"sh", "-c", "read x; echo $x"}, "stdin": "true"})
	busy.ReadMessage() // session_info: the op is running
	if got := presuspend("?idle=1"); got != http.StatusConflict {
		t.Fatalf("idle presuspend with an op running = %d, want 409", got)
	}
	// A checkpoint or an operator's suspend is not the guest's to refuse.
	if got := presuspend(""); got != http.StatusNoContent {
		t.Fatalf("plain presuspend = %d", got)
	}
	busy.WriteMessage(websocket.BinaryMessage, append([]byte{StreamStdin}, "still here\n"...))
	if r := readOp(t, busy); r.stdout.String() != "still here\n" {
		t.Fatalf("op disturbed by a refused suspend: %q", r.stdout.String())
	}

	if got := presuspend("?idle=1"); got != http.StatusNoContent {
		t.Fatalf("idle presuspend with every socket idle = %d", got)
	}
	busy.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, _, err := busy.ReadMessage(); err == nil {
		t.Fatal("idle control socket survived the suspend")
	}
	if _, attached := srv.Sessions.Activity(); attached != 0 {
		t.Fatalf("attached=%d after everything finished", attached)
	}
}
