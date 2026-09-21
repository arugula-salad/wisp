//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	sprites "github.com/superfly/sprites-go"
)

// The Go SDK has no client for the privileges/resources policies, filesystem
// watch or tasks, so these speak the wire formats directly (taken from the API
// docs and the JS SDK) and use the SDK to look at the effect inside the sprite.

// api makes a raw API call and returns the status and body.
func api(t *testing.T, method, path, body string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(method, os.Getenv("SPRITES_E2E_URL")+path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+os.Getenv("SPRITES_E2E_TOKEN"))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, strings.TrimSpace(string(b))
}

func want(t *testing.T, method, path, body string, status int) string {
	t.Helper()
	code, got := api(t, method, path, body)
	if code != status {
		t.Fatalf("%s %s: %d %s, want %d", method, path, code, got, status)
	}
	return got
}

// idleTimeout is the daemon's --idle-timeout, which the lifecycle tests have to know.
func idleTimeout(t *testing.T) time.Duration {
	d, err := time.ParseDuration(os.Getenv("SPRITES_E2E_IDLE_TIMEOUT"))
	if err != nil {
		t.Skip("SPRITES_E2E_IDLE_TIMEOUT (the daemon's --idle-timeout) not set")
	}
	return d
}

func status(t *testing.T, c *sprites.Client, name string) string {
	t.Helper()
	sp, err := c.GetSprite(context.Background(), name)
	if err != nil {
		t.Fatal(err)
	}
	return sp.Status
}

// staysRunning fails if the sprite leaves "running" within d.
func staysRunning(t *testing.T, c *sprites.Client, name string, d time.Duration) {
	t.Helper()
	for end := time.Now().Add(d); time.Now().Before(end); time.Sleep(time.Second) {
		if s := status(t, c, name); s != "running" {
			t.Fatalf("sprite went %s with %s of the hold still to go", s, time.Until(end).Round(time.Second))
		}
	}
}

func suspendsWithin(t *testing.T, c *sprites.Client, name string, d time.Duration) {
	t.Helper()
	for end := time.Now().Add(d); time.Now().Before(end); time.Sleep(500 * time.Millisecond) {
		if status(t, c, name) != "running" {
			return
		}
	}
	t.Fatalf("sprite still running %s after the hold ended", d)
}

func TestPlatform(t *testing.T) {
	c := client(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	name := fmt.Sprintf("e2e-plat-%d", time.Now().UnixNano()%1e9)
	sp, err := c.CreateSprite(ctx, name, nil)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Cleanup(func() { c.DeleteSprite(context.Background(), name) })
	base := "/v1/sprites/" + name
	sh := func(script string) (string, error) {
		out, err := sp.CommandContext(ctx, "bash", "-c", script).CombinedOutput()
		return strings.TrimSpace(string(out)), err
	}

	t.Run("privileges policy", func(t *testing.T) {
		if got := want(t, "GET", base+"/policy/privileges", "", 200); got != "{}" {
			t.Fatalf("default policy: %s", got)
		}
		if out, err := sh("sudo -n id -u"); err != nil || out != "0" {
			t.Fatalf("unrestricted sudo: %q %v", out, err)
		}

		want(t, "POST", base+"/policy/privileges", `{"profile":"standard","devices":["null"],"noNewPrivileges":false}`, 204)
		var got struct {
			Profile string
			Devices []string
		}
		json.Unmarshal([]byte(want(t, "GET", base+"/policy/privileges", "", 200)), &got)
		if got.Profile != "standard" || len(got.Devices) != 1 {
			t.Fatalf("stored policy: %+v", got)
		}
		// Root through sudo, but only with the standard capability set: no CAP_SYS_ADMIN, so no mounting.
		out, _ := sh("sudo -n grep CapEff /proc/self/status; sudo -n mount -t tmpfs none /mnt 2>&1; echo mount=$?")
		if !strings.Contains(out, "00000000a80425fb") || strings.Contains(out, "mount=0") {
			t.Fatalf("standard profile: %s", out)
		}

		want(t, "POST", base+"/policy/privileges", `{"noNewPrivileges":true}`, 204)
		if out, err := sh("sudo -n id -u"); err == nil || !strings.Contains(out, "no new privileges") {
			t.Fatalf("sudo under noNewPrivileges: %q %v", out, err)
		}

		want(t, "POST", base+"/policy/privileges", `{"profile":"root"}`, 400)
		want(t, "DELETE", base+"/policy/privileges", "", 204)
		if out, err := sh("sudo -n id -u"); err != nil || out != "0" {
			t.Fatalf("sudo after deleting the policy: %q %v", out, err)
		}
		want(t, "GET", "/v1/sprites/no-such-sprite/policy/privileges", "", 404)
	})

	t.Run("resources policy", func(t *testing.T) {
		if got := want(t, "GET", base+"/policy/resources", "", 200); got != "{}" {
			t.Fatalf("default policy: %s", got)
		}
		hog := `python3 -c 'x = bytearray(256 << 20); print("fits")'`
		if out, err := sh(hog); err != nil || out != "fits" {
			t.Fatalf("without a limit: %q %v", out, err)
		}
		want(t, "POST", base+"/policy/resources", `{"memory":{"limit_mb":96,"autoscale":false}}`, 204)
		if got := want(t, "GET", base+"/policy/resources", "", 200); got != `{"memory":{"limit_mb":96}}` {
			t.Fatalf("stored policy: %s", got)
		}
		var ee *sprites.ExitError
		if out, err := sh(hog); !errors.As(err, &ee) || strings.Contains(out, "fits") {
			t.Fatalf("256 MB under a 96 MB limit: %q %v", out, err)
		}
		want(t, "POST", base+"/policy/resources", `{"memory":{"limit_mb":0}}`, 400)
		want(t, "DELETE", base+"/policy/resources", "", 204)
		if out, err := sh(hog); err != nil || out != "fits" {
			t.Fatalf("after deleting the policy: %q %v", out, err)
		}
	})

	t.Run("filesystem watch", func(t *testing.T) {
		if out, err := sh("mkdir -p ~/w/old"); err != nil {
			t.Fatalf("mkdir: %s %v", out, err)
		}
		conn := dialWatch(t, name)
		conn.WriteJSON(map[string]any{"type": "subscribe", "paths": []string{"w"}, "recursive": true, "workingDir": "/home/sprite"})
		expect := func(what string, ok func(m watchMessage) bool) watchMessage {
			t.Helper()
			conn.SetReadDeadline(time.Now().Add(10 * time.Second))
			for {
				var m watchMessage
				if err := conn.ReadJSON(&m); err != nil {
					t.Fatalf("waiting for %s: %v", what, err)
				}
				if m.Type == "error" {
					t.Fatalf("waiting for %s: error message %+v", what, m)
				}
				if ok(m) {
					return m
				}
			}
		}
		event := func(op, path string) watchMessage {
			t.Helper()
			return expect(op+" "+path, func(m watchMessage) bool { return m.Type == "event" && m.Event == op && m.Path == path })
		}
		if m := expect("subscribed", func(m watchMessage) bool { return m.Type == "subscribed" }); len(m.Paths) != 1 || m.Paths[0] != "/home/sprite/w" {
			t.Fatalf("subscribed: %+v", m)
		}

		// A write through the filesystem API, into a directory that predates the watch.
		if err := sp.Filesystem().WriteFile("/home/sprite/w/old/api.txt", []byte("12345"), 0o644); err != nil {
			t.Fatal(err)
		}
		if m := event("create", "/home/sprite/w/old/api.txt"); m.Size != 5 || m.IsDir || m.Timestamp == "" {
			t.Fatalf("create event: %+v", m)
		}
		// A tree made by a process in the sprite after the watch started, then a file deep inside it.
		if out, err := sh("mkdir -p ~/w/a/b/c && sleep 0.3 && echo hi > ~/w/a/b/c/f && chmod 600 ~/w/a/b/c/f && mv ~/w/a/b/c/f ~/w/a/b/c/g && rm ~/w/a/b/c/g"); err != nil {
			t.Fatalf("%s %v", out, err)
		}
		if m := event("create", "/home/sprite/w/a/b/c"); !m.IsDir {
			t.Fatalf("directory create: %+v", m)
		}
		event("create", "/home/sprite/w/a/b/c/f")
		event("write", "/home/sprite/w/a/b/c/f")
		event("chmod", "/home/sprite/w/a/b/c/f")
		event("rename", "/home/sprite/w/a/b/c/f")
		event("create", "/home/sprite/w/a/b/c/g")
		event("remove", "/home/sprite/w/a/b/c/g")
	})

	t.Run("filesystem watch is bounded", func(t *testing.T) {
		// The watch budget is the agent's, not the connection's. Everything (some 7000
		// directories, /proc and /sys included) fits in it once, and is refused the second time.
		for i, wantType := range []string{"subscribed", "error"} {
			greedy := dialWatch(t, name)
			greedy.WriteJSON(map[string]any{"type": "subscribe", "paths": []string{"/"}, "recursive": true})
			greedy.SetReadDeadline(time.Now().Add(30 * time.Second))
			var m watchMessage
			if err := greedy.ReadJSON(&m); err != nil || m.Type != wantType || (i == 1 && !strings.Contains(m.Message, "watch limit")) {
				t.Fatalf("recursive watch of / number %d: %+v %v", i+1, m, err)
			}
			defer greedy.Close()
		}

		// A watcher that never reads, under an event storm, must not slow the sprite down.
		stalled := dialWatch(t, name)
		stalled.WriteJSON(map[string]any{"type": "subscribe", "paths": []string{"/home/sprite/w"}, "recursive": true})
		start := time.Now()
		if out, err := sh("mkdir -p ~/w/storm && cd ~/w/storm && for i in $(seq 1 20000); do echo $i > f$i; done; ls | wc -l"); err != nil || out != "20000" {
			t.Fatalf("storm: %q %v", out, err)
		}
		t.Logf("20000 files (40000+ events) written in %s with a stalled watcher attached", time.Since(start).Round(time.Millisecond))
		// Once it does read, it gets what was queued and is told how much it missed: nothing vanishes silently.
		events, dropped := 0, 0
		stalled.SetReadDeadline(time.Now().Add(5 * time.Second))
		for events+dropped < 40001 { // 1 mkdir, then a create and a write per file
			var m watchMessage
			if err := stalled.ReadJSON(&m); err != nil {
				break
			}
			var n int
			if m.Type == "event" {
				events++
			} else if _, err := fmt.Sscanf(m.Message, "client too slow: %d events dropped", &n); err == nil {
				dropped += n
			}
		}
		t.Logf("the stalled watcher then read %d events and was told of %d dropped", events, dropped)
		if events == 0 || events+dropped != 40001 {
			t.Fatalf("events unaccounted for: %d delivered + %d dropped, want 40001", events, dropped)
		}
		if out, err := sh("rm -rf ~/w/storm && echo ok"); err != nil || out != "ok" {
			t.Fatalf("after the storm: %q %v", out, err)
		}
	})

	t.Run("watch connection keeps the sprite awake", func(t *testing.T) {
		idle := idleTimeout(t)
		conn := dialWatch(t, name)
		conn.WriteJSON(map[string]any{"type": "subscribe", "paths": []string{"/home/sprite"}})
		go func() { // answer pings, as any watcher does by reading
			for {
				if _, _, err := conn.ReadMessage(); err != nil {
					return
				}
			}
		}()
		staysRunning(t, c, name, idle*5/2)
		conn.Close()
		suspendsWithin(t, c, name, idle+15*time.Second)
	})

	// Upstream's own interface to tasks: the management socket inside the sprite.
	guest := func(t *testing.T, args string) string {
		t.Helper()
		out, err := sh(`curl -sS --unix-socket /.sprite/api.sock -H "Content-Type: application/json" -w ' %{http_code}' ` + args)
		if err != nil {
			t.Fatalf("curl %s: %s %v", args, out, err)
		}
		return out
	}

	t.Run("tasks api", func(t *testing.T) {
		if out := guest(t, `-X POST http://sprite/v1/tasks -d '{"name":"agent","expire":"1h"}'`); !strings.HasSuffix(out, "201") {
			t.Fatalf("create: %s", out)
		}
		if out := guest(t, `-X POST http://sprite/v1/tasks -d '{"name":"agent","expire":"1h"}'`); !strings.HasSuffix(out, "409") {
			t.Fatalf("duplicate create: %s", out)
		}
		if out := guest(t, `-X POST http://sprite/v1/tasks -d '{"name":"greedy","expire":"2h"}'`); !strings.HasSuffix(out, "400") {
			t.Fatalf("expire beyond the maximum: %s", out)
		}
		if out := guest(t, `-X PUT http://sprite/v1/tasks/agent -d '{"expire":1800}'`); !strings.HasSuffix(out, "200") {
			t.Fatalf("refresh: %s", out)
		}
		if out := guest(t, `http://sprite/v1/tasks`); !strings.Contains(out, `"name":"agent"`) || !strings.Contains(out, `"expires_at"`) {
			t.Fatalf("list: %s", out)
		}
		if out := guest(t, `-X DELETE http://sprite/v1/tasks/agent`); !strings.HasSuffix(out, "204") {
			t.Fatalf("delete: %s", out)
		}
		if out := guest(t, `-X DELETE http://sprite/v1/tasks/agent`); !strings.HasSuffix(out, "404") {
			t.Fatalf("delete again: %s", out)
		}
	})

	t.Run("tasks hold the sprite awake", func(t *testing.T) {
		idle := idleTimeout(t)
		if out := guest(t, `-X PUT http://sprite/v1/tasks/agent -d '{"expire":"1h"}'`); !strings.HasSuffix(out, "200") {
			t.Fatalf("upsert: %s", out)
		}
		// Held: nothing else is going on, yet the sprite outlives the idle timeout.
		staysRunning(t, c, name, idle*5/2)
		var list struct {
			Tasks []struct{ Name string }
		}
		json.Unmarshal([]byte(want(t, "GET", base+"/tasks", "", 200)), &list)
		if len(list.Tasks) != 1 || list.Tasks[0].Name != "agent" {
			t.Fatalf("the hold is not visible from outside: %+v", list)
		}
		// Released: the sprite is free to suspend again.
		want(t, "DELETE", base+"/tasks/agent", "", 204)
		suspendsWithin(t, c, name, idle+15*time.Second)

		// A suspended sprite holds nothing, and asking must not wake it.
		if got := want(t, "GET", base+"/tasks", "", 200); got != `{"tasks":[]}` {
			t.Fatalf("tasks of a suspended sprite: %s", got)
		}
		want(t, "GET", base+"/tasks/agent", "", 404)
		if s := status(t, c, name); s == "running" {
			t.Fatal("listing tasks woke the sprite")
		}

		// Expiry: a hold nobody releases (a crashed client) ends on its own.
		// Declared from outside, which also wakes the sprite.
		secs := int((2 * idle).Seconds())
		want(t, "PUT", base+"/tasks/crashed", fmt.Sprintf(`{"expire":%d}`, secs), 200)
		staysRunning(t, c, name, 2*idle-time.Second)
		suspendsWithin(t, c, name, idle+15*time.Second)
	})
}

type watchMessage struct {
	Type      string   `json:"type"`
	Paths     []string `json:"paths"`
	Path      string   `json:"path"`
	Event     string   `json:"event"`
	Timestamp string   `json:"timestamp"`
	Size      int64    `json:"size"`
	IsDir     bool     `json:"isDir"`
	Message   string   `json:"message"`
}

func dialWatch(t *testing.T, sprite string) *websocket.Conn {
	t.Helper()
	u := "ws" + strings.TrimPrefix(os.Getenv("SPRITES_E2E_URL"), "http") + "/v1/sprites/" + sprite + "/fs/watch"
	conn, resp, err := websocket.DefaultDialer.Dial(u, http.Header{"Authorization": {"Bearer " + os.Getenv("SPRITES_E2E_TOKEN")}})
	if err != nil {
		var body bytes.Buffer
		if resp != nil {
			io.Copy(&body, resp.Body)
		}
		t.Fatalf("dial watch: %v %s", err, body.String())
	}
	t.Cleanup(func() { conn.Close() })
	return conn
}
