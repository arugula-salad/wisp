//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	sprites "github.com/superfly/sprites-go"
)

// With SPRITES_SDK_DEBUG=1 the SDK logs which kind of connection every exec in
// this package used ("using control conn for exec"), at a level slog hides by default.
func TestMain(m *testing.M) {
	if os.Getenv("SPRITES_SDK_DEBUG") != "" {
		slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug})))
	}
	os.Exit(m.Run())
}

// needGoSDKControl skips a test that drives the control channel with the official Go
// SDK. By default spritesd answers that SDK's /control probe with 404, because its
// ProxyPorts races on a control socket; these tests need a daemon started with
// --control-for-go-sdk, and SPRITES_E2E_GO_CONTROL=1 to say so. The raw-socket
// tests below cover the channel itself either way.
func needGoSDKControl(t *testing.T) {
	t.Helper()
	if os.Getenv("SPRITES_E2E_GO_CONTROL") == "" {
		t.Skip("daemon not started with --control-for-go-sdk (set SPRITES_E2E_GO_CONTROL=1 when it is)")
	}
}

// dialSprite opens one of a sprite's WebSocket endpoints through spritesd.
func dialSprite(t *testing.T, sprite, endpoint string) *websocket.Conn {
	t.Helper()
	u := "ws" + strings.TrimPrefix(os.Getenv("SPRITES_E2E_URL"), "http") + "/v1/sprites/" + sprite + endpoint
	conn, resp, err := websocket.DefaultDialer.Dial(u, http.Header{"Authorization": {"Bearer " + os.Getenv("SPRITES_E2E_TOKEN")}})
	if err != nil {
		body := ""
		if resp != nil {
			b, _ := io.ReadAll(resp.Body)
			body = string(b)
		}
		t.Fatalf("dial %s: %v %s", endpoint, err, body)
	}
	t.Cleanup(func() { conn.Close() })
	return conn
}

// controlExec runs one non-TTY exec op on an open control socket.
func controlExec(t *testing.T, conn *websocket.Conn, cmd ...string) (stdout string, exitCode int) {
	t.Helper()
	b, _ := json.Marshal(map[string]any{"type": "op.start", "op": "exec", "args": map[string]any{"cmd": cmd}})
	if err := conn.WriteMessage(websocket.TextMessage, append([]byte("control:"), b...)); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	conn.SetReadDeadline(time.Now().Add(30 * time.Second))
	for {
		typ, data, err := conn.ReadMessage()
		if err != nil {
			t.Fatalf("control socket ended mid-op: %v", err)
		}
		if typ == websocket.BinaryMessage {
			if data[0] == 1 { // stdout
				out.Write(data[1:])
			}
			continue
		}
		rest, isEnvelope := bytes.CutPrefix(data, []byte("control:"))
		if !isEnvelope {
			continue
		}
		var e struct {
			Type string
			Args struct {
				ExitCode int
				Error    string
			}
		}
		if json.Unmarshal(rest, &e) != nil || e.Type != "op.complete" {
			t.Fatalf("op ended with %s", data)
		}
		return out.String(), e.Args.ExitCode
	}
}

func TestControlChannel(t *testing.T) {
	c := client(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	name := fmt.Sprintf("e2e-ctl-%d", time.Now().UnixNano()%1e9)

	// The SDK cannot set a sprite-level environment, and the control route has to carry it like /exec does.
	body := fmt.Sprintf(`{"name":%q,"environment":{"FROM_SPRITE":"yes"}}`, name)
	req, _ := http.NewRequest(http.MethodPost, os.Getenv("SPRITES_E2E_URL")+"/v1/sprites", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+os.Getenv("SPRITES_E2E_TOKEN"))
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != http.StatusCreated {
		t.Fatalf("create: %v %v", resp, err)
	}
	resp.Body.Close()
	t.Cleanup(func() { c.DeleteSprite(context.Background(), name) })
	sp := c.Sprite(name)

	t.Run("the SDK picks the control channel on its own", func(t *testing.T) {
		needGoSDKControl(t)
		for i := 0; i < 5; i++ {
			cmd := sp.CommandContext(ctx, "sh", "-c", "echo $FROM_SPRITE "+fmt.Sprint(i))
			out, err := cmd.Output()
			if err != nil || string(out) != fmt.Sprintf("yes %d\n", i) {
				t.Fatalf("exec %d: out=%q err=%v", i, out, err)
			}
			if mode := cmd.ConnectionMode(); mode != "control" {
				t.Fatalf("exec %d went over a %q connection; /control is not being used", i, mode)
			}
		}
	})

	t.Run("many concurrent SDK execs", func(t *testing.T) {
		needGoSDKControl(t)
		const n = 40
		var wg sync.WaitGroup
		errs := make(chan error, n)
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				cmd := sp.CommandContext(ctx, "sh", "-c", fmt.Sprintf("cat; echo ' %d'; exit %d", i, i%7))
				cmd.Stdin = strings.NewReader(strings.Repeat("x", 1000+i))
				out, err := cmd.Output()
				want := strings.Repeat("x", 1000+i) + fmt.Sprintf(" %d\n", i)
				code := 0
				if ee, ok := err.(*sprites.ExitError); ok {
					code = ee.ExitCode()
				} else if err != nil {
					errs <- fmt.Errorf("exec %d: %v", i, err)
					return
				}
				if string(out) != want || code != i%7 || cmd.ConnectionMode() != "control" {
					errs <- fmt.Errorf("exec %d: %d bytes, exit %d, mode %s", i, len(out), code, cmd.ConnectionMode())
				}
			}()
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			t.Error(err)
		}
	})

	t.Run("attach by id over control replays output", func(t *testing.T) {
		needGoSDKControl(t)
		owner := sp.CommandContext(ctx, "sh", "-c", "echo first; sleep 2; echo second")
		var ownerOut bytes.Buffer
		owner.Stdout = &ownerOut
		if err := owner.Start(); err != nil {
			t.Fatal(err)
		}
		var id string
		for i := 0; i < 50 && id == ""; i++ {
			ss, err := sp.ListSessions(ctx)
			if err != nil {
				t.Fatal(err)
			}
			for _, s := range ss {
				if strings.Contains(s.Command, "echo first") {
					id = s.ID
				}
			}
			time.Sleep(50 * time.Millisecond)
		}
		time.Sleep(300 * time.Millisecond) // let "first" be history by the time we attach
		late := c.AttachSession(name, id)
		out, err := late.Output()
		if err != nil || string(out) != "first\nsecond\n" || late.ConnectionMode() != "control" {
			t.Fatalf("attached client: out=%q err=%v mode=%s", out, err, late.ConnectionMode())
		}
		if err := owner.Wait(); err != nil || ownerOut.String() != "first\nsecond\n" {
			t.Fatalf("owner: out=%q err=%v", ownerOut.String(), err)
		}
	})

	// The Go SDK hangs up its control socket after every exec, so reuse is shown
	// with the protocol itself: several ops, one after another, on one socket.
	t.Run("one socket serves sequential ops", func(t *testing.T) {
		conn := dialSprite(t, name, "/control")
		for i := 0; i < 5; i++ {
			out, code := controlExec(t, conn, "sh", "-c", fmt.Sprintf("echo op %d $FROM_SPRITE; exit %d", i, i))
			if out != fmt.Sprintf("op %d yes\n", i) || code != i {
				t.Fatalf("op %d: out=%q exit=%d", i, out, code)
			}
		}
		// The ops ran in one guest: a file written by one is there for the next.
		controlExec(t, conn, "sh", "-c", "echo kept > /tmp/ctl-marker")
		if out, _ := controlExec(t, conn, "cat", "/tmp/ctl-marker"); out != "kept\n" {
			t.Fatalf("marker = %q", out)
		}
	})

	t.Run("many concurrent sockets, several ops each", func(t *testing.T) {
		const n = 25
		var wg sync.WaitGroup
		for i := 0; i < n; i++ {
			conn := dialSprite(t, name, "/control")
			wg.Add(1)
			go func() {
				defer wg.Done()
				for round := 0; round < 4; round++ {
					want := fmt.Sprintf("%d.%d\n", i, round)
					if out, code := controlExec(t, conn, "echo", strings.TrimSpace(want)); out != want || code != 0 {
						t.Errorf("socket %d round %d: out=%q exit=%d", i, round, out, code)
					}
				}
			}()
		}
		wg.Wait()
	})

	t.Run("port notifications and proxy", func(t *testing.T) {
		if _, err := sp.CommandContext(ctx, "sh", "-c", "mkdir -p ~/www && echo proxied > ~/www/index.html").Output(); err != nil {
			t.Fatal(err)
		}
		watch := dialSprite(t, name, "/ports/watch")
		var list struct {
			Type  string
			Ports []sprites.PortNotificationMessage
		}
		if err := watch.ReadJSON(&list); err != nil || list.Type != "port_list" {
			t.Fatalf("ports/watch snapshot: %+v %v", list, err)
		}

		events := make(chan sprites.PortNotificationMessage, 8)
		cmd := sp.CommandContext(ctx, "python3", "-m", "http.server", "8765", "--bind", "127.0.0.1", "--directory", "/home/sprite/www")
		cmd.TextMessageHandler = func(data []byte) {
			var ev sprites.PortNotificationMessage
			if json.Unmarshal(data, &ev) == nil && strings.HasPrefix(ev.Type, "port_") {
				events <- ev
			}
		}
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		var opened sprites.PortNotificationMessage
		select {
		case opened = <-events:
		case <-time.After(15 * time.Second):
			t.Fatal("no port_opened for a session that started listening")
		}
		if opened.Type != "port_opened" || opened.Port != 8765 || opened.Address != "127.0.0.1" || opened.PID <= 0 {
			t.Fatalf("port_opened = %+v", opened)
		}
		var seen sprites.PortNotificationMessage
		watch.SetReadDeadline(time.Now().Add(15 * time.Second))
		for seen.Port != 8765 {
			if err := watch.ReadJSON(&seen); err != nil {
				t.Fatalf("ports/watch: %v", err)
			}
		}
		if seen != opened {
			t.Fatalf("ports/watch reported %+v, the session %+v", seen, opened)
		}

		// Use the notification the way the CLI does: proxy a local port to the address it
		// names. Like the CLI, with the control channel off: over it the SDK's proxy
		// (v0.2.1) reads the socket from two goroutines at once, and the pool's read
		// loop swallows the handshake reply, so it hangs against any server.
		direct := sprites.New(os.Getenv("SPRITES_E2E_TOKEN"), sprites.WithBaseURL(os.Getenv("SPRITES_E2E_URL")), sprites.WithDisableControl())
		ln, _ := net.Listen("tcp", "127.0.0.1:0")
		local := ln.Addr().(*net.TCPAddr).Port
		ln.Close()
		sessions, err := direct.ProxyPorts(ctx, name, []sprites.PortMapping{{LocalPort: local, RemotePort: opened.Port, RemoteHost: opened.Address}})
		if err != nil {
			t.Fatal(err)
		}
		defer sessions[0].Close()
		hc := &http.Client{Timeout: 10 * time.Second}
		for i := 0; i < 3; i++ {
			resp, err := hc.Get(fmt.Sprintf("http://127.0.0.1:%d/", local))
			if err != nil {
				t.Fatalf("GET through the proxy (%d): %v", i, err)
			}
			b, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if string(b) != "proxied\n" {
				t.Fatalf("proxied body = %q", b)
			}
		}

		// The proxy op itself, spoken the way the SDK spells it.
		conn := dialSprite(t, name, "/control")
		conn.WriteJSON(map[string]any{"type": "start", "operation": "proxy",
			"params": map[string]any{"host": opened.Address, "port": opened.Port, "keep_alive": true}})
		var hello struct{ Status, Target string }
		if err := conn.ReadJSON(&hello); err != nil || hello.Status != "connected" || hello.Target != "127.0.0.1:8765" {
			t.Fatalf("proxy op handshake: %+v %v", hello, err)
		}
		conn.WriteMessage(websocket.BinaryMessage, []byte("GET / HTTP/1.1\r\nHost: sprite\r\n\r\n"))
		var page bytes.Buffer
		conn.SetReadDeadline(time.Now().Add(10 * time.Second))
		for !strings.HasSuffix(page.String(), "proxied\n") {
			typ, data, err := conn.ReadMessage()
			if err != nil {
				t.Fatalf("reading the proxied response: %v (so far %q)", err, page.String())
			}
			if typ == websocket.BinaryMessage {
				page.Write(data)
			}
		}
		if !strings.HasPrefix(page.String(), "HTTP/1.0 200") {
			t.Fatalf("proxied response = %q", page.String())
		}
		// http.server speaks HTTP/1.0 and hangs up after the response. The SDK's proxy
		// connection learns of EOF only from a close, so that is how the op ends.
		var closing struct{ Type string }
		if err := conn.ReadJSON(&closing); err != nil || closing.Type != "op.complete" {
			t.Fatalf("proxy op closing envelope: %+v %v", closing, err)
		}
		if _, _, err := conn.ReadMessage(); !websocket.IsCloseError(err, websocket.CloseNormalClosure) {
			t.Fatalf("after the target hung up: %v, want a normal close", err)
		}

		// Other ops in the SDK's spelling, sharing a socket with an exec.
		conn = dialSprite(t, name, "/control")
		conn.WriteJSON(map[string]any{"type": "op.start", "op": "fs.write", "args": "path=www%2Fvia-control.txt"})
		conn.WriteMessage(websocket.BinaryMessage, []byte("written over control"))
		var wrote struct {
			Path string
			Size int
		}
		if err := conn.ReadJSON(&wrote); err != nil || wrote.Path != "/home/sprite/www/via-control.txt" || wrote.Size != 20 {
			t.Fatalf("fs.write: %+v %v", wrote, err)
		}
		if err := conn.ReadJSON(&closing); err != nil || closing.Type != "op.complete" {
			t.Fatalf("fs.write closing envelope: %+v %v", closing, err)
		}
		if out, _ := controlExec(t, conn, "sh", "-c", "cat ~/www/via-control.txt; stat -c ' %U' ~/www/via-control.txt"); out != "written over control sprite\n" {
			t.Fatalf("file written over control reads back as %q", out)
		}

		if err := cmd.Signal("TERM"); err != nil {
			t.Fatal(err)
		}
		cmd.Wait()
		watch.SetReadDeadline(time.Now().Add(15 * time.Second))
		for seen.Type != "port_closed" || seen.Port != 8765 {
			if err := watch.ReadJSON(&seen); err != nil {
				t.Fatalf("ports/watch waiting for port_closed: %v", err)
			}
		}
	})
}
