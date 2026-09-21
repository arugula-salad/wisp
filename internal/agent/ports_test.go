package agent

import (
	"encoding/json"
	"net"
	"net/http/httptest"
	"net/url"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestParseProcAddr(t *testing.T) {
	for in, want := range map[string]string{
		"0100007F:1F90":                         "127.0.0.1:8080",
		"00000000:0050":                         "0.0.0.0:80",
		"00000000000000000000000000000000:0BB8": "[::]:3000",
		"00000000000000000000000001000000:0277": "[::1]:631",
	} {
		ip, port, ok := parseProcAddr(in)
		if got := net.JoinHostPort(ip.String(), strconv.Itoa(port)); !ok || got != want {
			t.Errorf("parseProcAddr(%q) = %q %v, want %q", in, got, ok, want)
		}
	}
	if _, _, ok := parseProcAddr("nonsense"); ok {
		t.Error("parsed nonsense")
	}
}

// listenScript binds, prints the port, and only later listens, so the test
// knows which port to expect before the notification can arrive.
const listenScript = `import socket, time
s = socket.socket(); s.bind(("127.0.0.1", 0)); print(s.getsockname()[1], flush=True)
time.sleep(0.7); s.listen(); time.sleep(1.2); s.close(); time.sleep(0.8)`

// nextText returns the next JSON text message of one of the wanted types.
func nextText(t *testing.T, conn *websocket.Conn, types ...string) map[string]any {
	t.Helper()
	conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	for {
		typ, data, err := conn.ReadMessage()
		if err != nil {
			t.Fatalf("waiting for %v: %v", types, err)
		}
		var m map[string]any
		if typ != websocket.TextMessage || json.Unmarshal(data, &m) != nil {
			continue
		}
		for _, want := range types {
			if m["type"] == want {
				return m
			}
		}
	}
}

func TestPortNotifications(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not available")
	}
	ts, m := newTestServer(t)

	watch := dialPlain(t, ts, "/ports/watch")
	if list := nextText(t, watch, "port_list"); list["ports"] == nil {
		t.Fatalf("first message should be the snapshot: %v", list)
	}
	bystander := dial(t, ts, "/exec", url.Values{"cmd": {"sleep", "30"}, "stdin": {"false"}})
	overheard := make(chan map[string]any, 4)
	go func() {
		for {
			typ, data, err := bystander.ReadMessage()
			if err != nil {
				return
			}
			var msg map[string]any
			if typ == websocket.TextMessage && json.Unmarshal(data, &msg) == nil && strings.HasPrefix(msg["type"].(string), "port_") {
				overheard <- msg
			}
		}
	}()

	// Through a shell, so the listener is not the session's own command but a member of its process group.
	conn := dial(t, ts, "/exec", url.Values{"cmd": {"sh", "-c", "python3 -c '" + listenScript + "' & wait"}, "stdin": {"false"}})
	var port float64
	for port == 0 {
		typ, data, err := conn.ReadMessage()
		if err != nil {
			t.Fatal(err)
		}
		if typ == websocket.BinaryMessage && data[0] == StreamStdout {
			n, _ := strconv.Atoi(strings.TrimSpace(string(data[1:])))
			port = float64(n)
		}
	}
	quietSince, _ := m.Activity()

	opened := nextText(t, conn, "port_opened")
	if opened["port"] != port || opened["address"] != "127.0.0.1" || opened["pid"].(float64) <= 0 {
		t.Fatalf("port_opened = %v, want port %v on 127.0.0.1", opened, port)
	}
	closed := nextText(t, conn, "port_closed", "exit")
	if closed["type"] != "port_closed" || closed["port"] != port || closed["pid"] != opened["pid"] {
		t.Fatalf("port_closed = %v", closed)
	}
	if last, _ := m.Activity(); !last.Equal(quietSince) {
		t.Error("a port notification counted as sprite activity")
	}

	// The watcher hears about every port; this one among them.
	for {
		if ev := nextText(t, watch, "port_opened"); ev["port"] == port {
			break
		}
	}
	for {
		if ev := nextText(t, watch, "port_closed"); ev["port"] == port {
			break
		}
	}
	select {
	case msg := <-overheard:
		t.Fatalf("another session was told about a port that is not its own: %v", msg)
	default:
	}
}

func TestPortPollingStopsWithoutSubscribers(t *testing.T) {
	ts, m := newTestServer(t)
	polling := func() bool {
		m.ports.mu.Lock()
		defer m.ports.mu.Unlock()
		return len(m.ports.subs) > 0
	}
	r := collect(t, dial(t, ts, "/exec", url.Values{"cmd": {"true"}, "stdin": {"false"}}))
	if r.exit != 0 {
		t.Fatalf("exit=%d", r.exit)
	}
	deadline := time.Now().Add(2 * time.Second)
	for polling() {
		if time.Now().After(deadline) {
			t.Fatal("still polling /proc with no client attached")
		}
		time.Sleep(10 * time.Millisecond)
	}
	select {
	case <-m.ports.stop:
	default:
		t.Fatal("poll loop was not stopped")
	}
}

// dialPlain is dial for endpoints that do not advertise exec capabilities.
func dialPlain(t *testing.T, ts *httptest.Server, path string) *websocket.Conn {
	t.Helper()
	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(ts.URL, "http")+path, nil)
	if err != nil {
		t.Fatalf("dial %s: %v", path, err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn
}
