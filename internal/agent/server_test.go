package agent

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

type result struct {
	stdout, stderr bytes.Buffer
	exit           int
	sessionID      string
	gotExit        bool
}

func newTestServer(t *testing.T) (*httptest.Server, *Manager) {
	m := NewManager()
	ts := httptest.NewServer((&Server{Sessions: m}).Handler())
	t.Cleanup(ts.Close)
	return ts, m
}

func dial(t *testing.T, ts *httptest.Server, path string, q url.Values) *websocket.Conn {
	t.Helper()
	u := "ws" + strings.TrimPrefix(ts.URL, "http") + path + "?" + q.Encode()
	conn, resp, err := websocket.DefaultDialer.Dial(u, nil)
	if err != nil {
		body := ""
		if resp != nil {
			b, _ := io.ReadAll(resp.Body)
			body = string(b)
		}
		t.Fatalf("dial %s: %v %s", u, err, body)
	}
	if got := resp.Header.Get("X-Sprite-Capabilities"); got != "signal" {
		t.Errorf("capabilities header = %q", got)
	}
	t.Cleanup(func() { conn.Close() })
	return conn
}

// collect reads a non-TTY session to completion.
func collect(t *testing.T, conn *websocket.Conn) *result {
	t.Helper()
	r := &result{exit: -1}
	conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	for {
		typ, data, err := conn.ReadMessage()
		if err != nil {
			if !r.gotExit {
				t.Fatalf("connection ended before exit frame: %v", err)
			}
			return r
		}
		if typ == websocket.TextMessage {
			var m map[string]any
			json.Unmarshal(data, &m)
			if m["type"] == "session_info" {
				r.sessionID, _ = m["session_id"].(string)
			}
			continue
		}
		switch data[0] {
		case StreamStdout:
			r.stdout.Write(data[1:])
		case StreamStderr:
			r.stderr.Write(data[1:])
		case StreamExit:
			r.exit, r.gotExit = int(data[1]), true
		}
	}
}

func TestNonTTYStreamsAndExitCode(t *testing.T) {
	ts, _ := newTestServer(t)
	conn := dial(t, ts, "/exec", url.Values{
		"cmd": {"sh", "-c", "echo out; echo err >&2; echo $FOO; exit 7"}, "env": {"FOO=bar"}, "stdin": {"false"}, "dir": {"/"},
	})
	r := collect(t, conn)
	if r.stdout.String() != "out\nbar\n" || r.stderr.String() != "err\n" || r.exit != 7 {
		t.Fatalf("stdout=%q stderr=%q exit=%d", r.stdout.String(), r.stderr.String(), r.exit)
	}
	if r.sessionID == "" {
		t.Error("no session_info received")
	}
}

func TestStdinAndEOF(t *testing.T) {
	ts, _ := newTestServer(t)
	conn := dial(t, ts, "/exec", url.Values{"cmd": {"cat"}, "stdin": {"true"}})
	conn.WriteMessage(websocket.BinaryMessage, append([]byte{StreamStdin}, "hello"...))
	conn.WriteMessage(websocket.BinaryMessage, []byte{StreamStdinEOF})
	r := collect(t, conn)
	if r.stdout.String() != "hello" || r.exit != 0 {
		t.Fatalf("stdout=%q exit=%d", r.stdout.String(), r.exit)
	}
}

func TestMissingExecutable(t *testing.T) {
	ts, _ := newTestServer(t)
	u := "ws" + strings.TrimPrefix(ts.URL, "http") + "/exec?cmd=definitely-not-a-binary"
	_, resp, err := websocket.DefaultDialer.Dial(u, nil)
	if err == nil || resp == nil || resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("want 400 handshake failure, got err=%v resp=%v", err, resp)
	}
}

func TestTTYExitAndResize(t *testing.T) {
	ts, _ := newTestServer(t)
	conn := dial(t, ts, "/exec", url.Values{"cmd": {"sh"}, "tty": {"true"}, "cols": {"100"}, "rows": {"30"}})
	conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"resize","cols":132,"rows":43}`))
	time.Sleep(100 * time.Millisecond)
	conn.WriteMessage(websocket.BinaryMessage, []byte("stty size; exit 3\n"))
	var out bytes.Buffer
	exit := -1
	conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	for {
		typ, data, err := conn.ReadMessage()
		if err != nil {
			break
		}
		if typ == websocket.BinaryMessage {
			out.Write(data) // TTY output is unframed
			continue
		}
		var m struct {
			Type     string `json:"type"`
			ExitCode int    `json:"exit_code"`
		}
		if json.Unmarshal(data, &m) == nil && m.Type == "exit" {
			exit = m.ExitCode
		}
	}
	if exit != 3 || !strings.Contains(out.String(), "43 132") {
		t.Fatalf("exit=%d out=%q", exit, out.String())
	}
}

func TestDetachReattachReplaysOutput(t *testing.T) {
	ts, m := newTestServer(t)
	conn := dial(t, ts, "/exec", url.Values{
		"cmd": {"sh", "-c", "echo first; sleep 1; echo second"}, "stdin": {"false"}, "max_run_after_disconnect": {"30s"},
	})
	var id string
	for id == "" {
		typ, data, err := conn.ReadMessage()
		if err != nil {
			t.Fatal(err)
		}
		if typ == websocket.TextMessage {
			var mm map[string]any
			json.Unmarshal(data, &mm)
			id, _ = mm["session_id"].(string)
		}
	}
	conn.Close()
	time.Sleep(200 * time.Millisecond)
	if l := m.List(); len(l) != 1 || l[0].ID != id {
		t.Fatalf("session list after detach = %+v", l)
	}

	r := collect(t, dial(t, ts, "/exec/"+id, nil))
	if r.stdout.String() != "first\nsecond\n" || r.exit != 0 {
		t.Fatalf("replayed stdout=%q exit=%d", r.stdout.String(), r.exit)
	}
}

func TestDisconnectKillsNonTTYAfterGrace(t *testing.T) {
	ts, m := newTestServer(t)
	conn := dial(t, ts, "/exec", url.Values{"cmd": {"sleep", "60"}, "stdin": {"false"}, "max_run_after_disconnect": {"200ms"}})
	conn.ReadMessage()
	conn.Close()
	deadline := time.Now().Add(3 * time.Second)
	for len(m.List()) > 0 {
		if time.Now().After(deadline) {
			t.Fatal("session still running after disconnect grace period")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestKillEndpoint(t *testing.T) {
	ts, _ := newTestServer(t)
	conn := dial(t, ts, "/exec", url.Values{"cmd": {"sleep", "60"}, "stdin": {"false"}})
	_, data, _ := conn.ReadMessage()
	var info map[string]any
	json.Unmarshal(data, &info)
	id := info["session_id"].(string)

	resp, err := http.Post(ts.URL+"/exec/"+id+"/kill?signal=TERM&timeout=5s", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), `"type":"exited"`) || !strings.Contains(string(body), `"type":"complete"`) {
		t.Fatalf("kill events: %s", body)
	}
	if r := collect(t, conn); r.exit != 128+15 {
		t.Fatalf("exit=%d, want 143", r.exit)
	}
	resp, _ = http.Post(ts.URL+"/exec/"+id+"/kill", "", nil)
	if resp.StatusCode != http.StatusGone {
		t.Fatalf("second kill status=%d, want 410", resp.StatusCode)
	}
}

func TestExecPost(t *testing.T) {
	ts, _ := newTestServer(t)
	u := ts.URL + "/exec?cmd=sh&cmd=-c&cmd=" + url.QueryEscape("tr a-z A-Z; echo warn >&2; exit 4") + "&stdin=true"

	// Length framing: what wispd consumes. Unambiguous however the bytes are chunked.
	resp, err := http.Post(u+"&framing=length", "", strings.NewReader("shout"))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	var stdout, stderr []byte
	exit := -1
	for len(body) > 0 {
		n := int(binary.BigEndian.Uint32(body[1:5]))
		payload := body[5 : 5+n]
		switch body[0] {
		case StreamStdout:
			stdout = append(stdout, payload...)
		case StreamStderr:
			stderr = append(stderr, payload...)
		case StreamExit:
			exit = int(payload[0])
		default:
			t.Fatalf("unexpected frame type %d", body[0])
		}
		body = body[5+n:]
	}
	if string(stdout) != "SHOUT" || string(stderr) != "warn\n" || exit != 4 {
		t.Fatalf("stdout=%q stderr=%q exit=%d", stdout, stderr, exit)
	}

	// Upstream framing: the last frame of the body is the two-byte exit frame.
	resp, err = http.Post(u, "", strings.NewReader("shout"))
	if err != nil {
		t.Fatal(err)
	}
	body, _ = io.ReadAll(resp.Body)
	if len(body) < 2 || body[len(body)-2] != StreamExit || body[len(body)-1] != 4 || body[0] != StreamStdout {
		t.Fatalf("upstream-framed body = %q", body)
	}
}
