//go:build e2e

// Package e2e drives a running wispd with the official Sprites Go SDK, so
// wire compatibility is checked against the real client rather than our own.
//
//	SPRITES_E2E_URL=http://127.0.0.1:7788 SPRITES_E2E_TOKEN=$(cat ~/.local/share/wisp/token) \
//	  go test -tags e2e -count=1 -v ./e2e/
package e2e

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	sprites "github.com/superfly/sprites-go"
)

// The daemon under test, from the environment (see the top of this file).
func e2eURL() string   { return os.Getenv("SPRITES_E2E_URL") }
func e2eToken() string { return os.Getenv("SPRITES_E2E_TOKEN") }

func e2eAuth() http.Header { return http.Header{"Authorization": {"Bearer " + e2eToken()}} }

// e2eWS is the WebSocket URL of an API path.
func e2eWS(path string) string { return "ws" + strings.TrimPrefix(e2eURL(), "http") + path }

func client(t *testing.T) *sprites.Client {
	url, tok := e2eURL(), e2eToken()
	if url == "" || tok == "" {
		t.Skip("SPRITES_E2E_URL / SPRITES_E2E_TOKEN not set")
	}
	return sprites.New(tok, sprites.WithBaseURL(url))
}

// fetchURL GETs the sprite's own URL. *.localhost may not resolve everywhere, so
// dial the API address and send the sprite's hostname in the Host header.
func fetchURL(t *testing.T, sprite string) string {
	t.Helper()
	base, _ := url.Parse(e2eURL())
	req, _ := http.NewRequest(http.MethodGet, base.String()+"/", nil)
	req.Host = sprite + ".sprites.localhost:" + base.Port()
	req.Header = e2eAuth()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("fetch sprite URL: %v", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("sprite URL: %s: %s", resp.Status, b)
	}
	return string(b)
}

func TestSDKConformance(t *testing.T) {
	c := client(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	name := fmt.Sprintf("e2e-%d", time.Now().UnixNano()%1e9)

	sp, err := c.CreateSprite(ctx, name, nil)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Cleanup(func() { c.DeleteSprite(context.Background(), name) })

	t.Run("get and list", func(t *testing.T) {
		got, err := c.GetSprite(ctx, name)
		if err != nil || got.Name() != name {
			t.Fatalf("get: %v %v", got, err)
		}
		list, err := c.ListSprites(ctx, &sprites.ListOptions{Prefix: name})
		if err != nil || len(list.Sprites) != 1 {
			t.Fatalf("list: %+v %v", list, err)
		}
	})

	t.Run("output", func(t *testing.T) {
		out, err := sp.CommandContext(ctx, "echo", "hello", "world").Output()
		if err != nil || string(out) != "hello world\n" {
			t.Fatalf("out=%q err=%v", out, err)
		}
	})

	t.Run("exit code and stderr", func(t *testing.T) {
		cmd := sp.CommandContext(ctx, "sh", "-c", "echo oops >&2; exit 42")
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		err := cmd.Run()
		var ee *sprites.ExitError
		if !errors.As(err, &ee) || ee.ExitCode() != 42 || stderr.String() != "oops\n" {
			t.Fatalf("err=%v stderr=%q", err, stderr.String())
		}
	})

	t.Run("stdin env dir", func(t *testing.T) {
		cmd := sp.CommandContext(ctx, "sh", "-c", `cat; echo " $FOO $(pwd)"`)
		cmd.Stdin = strings.NewReader("piped")
		cmd.Env = []string{"FOO=bar"}
		cmd.Dir = "/tmp"
		out, err := cmd.Output()
		if err != nil || string(out) != "piped bar /tmp\n" {
			t.Fatalf("out=%q err=%v", out, err)
		}
	})

	t.Run("large output is complete and ordered", func(t *testing.T) {
		out, err := sp.CommandContext(ctx, "sh", "-c", "seq 1 200000").Output()
		if err != nil {
			t.Fatal(err)
		}
		lines := strings.Split(strings.TrimSpace(string(out)), "\n")
		if len(lines) != 200000 || lines[199999] != "200000" {
			t.Fatalf("got %d lines, last=%q", len(lines), lines[len(lines)-1])
		}
	})

	t.Run("tty", func(t *testing.T) {
		cmd := sp.CommandContext(ctx, "sh", "-c", "test -t 1 && sleep 0.5 && stty size")
		cmd.SetTTY(true)
		var out bytes.Buffer
		cmd.Stdout = &out
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		// Resize once running: over a control channel the SDK has no way to send an initial size.
		if err := cmd.SetTTYSize(40, 120); err != nil {
			t.Fatal(err)
		}
		if err := cmd.Wait(); err != nil || !strings.Contains(out.String(), "40 120") {
			t.Fatalf("out=%q err=%v", out.String(), err)
		}
	})

	t.Run("sessions list, signal", func(t *testing.T) {
		cmd := sp.CommandContext(ctx, "sleep", "300")
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		var id string
		for i := 0; i < 50 && id == ""; i++ {
			ss, err := sp.ListSessions(ctx)
			if err != nil {
				t.Fatal(err)
			}
			for _, s := range ss {
				if strings.HasPrefix(s.Command, "sleep") {
					id = s.ID
				}
			}
			time.Sleep(50 * time.Millisecond)
		}
		if id == "" {
			t.Fatal("running session never appeared in ListSessions")
		}
		if err := cmd.Signal("TERM"); err != nil {
			t.Fatalf("signal: %v", err)
		}
		err := cmd.Wait()
		var ee *sprites.ExitError
		if !errors.As(err, &ee) || ee.ExitCode() != 143 {
			t.Fatalf("after SIGTERM: err=%v", err)
		}
	})

	t.Run("checkpoint and restore", func(t *testing.T) {
		if _, err := sp.CommandContext(ctx, "sh", "-c", "echo before > ~/state").Output(); err != nil {
			t.Fatal(err)
		}
		cs, err := sp.CreateCheckpointWithComment(ctx, "known good")
		if err != nil {
			t.Fatal(err)
		}
		var last *sprites.StreamMessage
		if err := cs.ProcessAll(func(m *sprites.StreamMessage) error { last = m; return nil }); err != nil {
			t.Fatal(err)
		}
		if last == nil || last.Type != "complete" {
			t.Fatalf("checkpoint stream ended with %+v", last)
		}
		cps, err := sp.ListCheckpoints(ctx, "")
		if err != nil || len(cps) != 1 || cps[0].Comment != "known good" {
			t.Fatalf("list checkpoints: %+v %v", cps, err)
		}

		if _, err := sp.CommandContext(ctx, "sh", "-c", "echo after > ~/state; sudo rm -rf /usr/bin/git").Output(); err != nil {
			t.Fatal(err)
		}
		rs, err := sp.RestoreCheckpoint(ctx, cps[0].ID)
		if err != nil {
			t.Fatal(err)
		}
		if err := rs.ProcessAll(func(m *sprites.StreamMessage) error { last = m; return nil }); err != nil || last.Type != "complete" {
			t.Fatalf("restore: %v %+v", err, last)
		}
		out, err := sp.CommandContext(ctx, "sh", "-c", "cat ~/state; test -x /usr/bin/git && echo git-is-back").Output()
		if err != nil || string(out) != "before\ngit-is-back\n" {
			t.Fatalf("after restore: out=%q err=%v", out, err)
		}
	})

	t.Run("filesystem", func(t *testing.T) {
		fsys := sp.FilesystemAt("/home/sprite")
		payload := bytes.Repeat([]byte("0123456789abcdef"), 1<<16) // 1 MiB
		if err := fsys.WriteFile("proj/data/blob.bin", payload, 0o640); err != nil {
			t.Fatalf("write: %v", err)
		}
		got, err := fsys.ReadFile("proj/data/blob.bin")
		if err != nil || !bytes.Equal(got, payload) {
			t.Fatalf("read back: %d bytes, err=%v", len(got), err)
		}
		info, err := fsys.Stat("proj/data/blob.bin")
		if err != nil || info.Size() != int64(len(payload)) || info.Mode().Perm() != 0o640 {
			t.Fatalf("stat: %+v %v", info, err)
		}
		// What the API wrote must look like the user's own files from inside the sprite.
		out, err := sp.CommandContext(ctx, "sh", "-c", "stat -c '%U %a' proj/data/blob.bin proj/data; sha256sum < proj/data/blob.bin | cut -c1-12").Output()
		if err != nil || !strings.HasPrefix(string(out), "sprite 640\nsprite 755\n") {
			t.Fatalf("ownership/mode seen in guest: %q err=%v", out, err)
		}
		if err := fsys.Rename("proj/data/blob.bin", "proj/data/moved.bin"); err != nil {
			t.Fatal(err)
		}
		if err := fsys.MkdirAll("proj/empty", 0o755); err != nil {
			t.Fatal(err)
		}
		entries, err := fsys.ReadDir("proj/data")
		if err != nil || len(entries) != 1 || entries[0].Name() != "moved.bin" {
			t.Fatalf("readdir: %v %v", entries, err)
		}
		if _, err := fsys.ReadFile("proj/nope"); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("missing file: want ErrNotExist, got %v", err)
		}
		if err := fsys.Remove("proj"); err == nil {
			t.Fatal("non-recursive remove of a non-empty directory should fail")
		}
		if err := fsys.RemoveAll("proj"); err != nil {
			t.Fatal(err)
		}
		if _, err := fsys.Stat("proj"); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("after RemoveAll: %v", err)
		}
	})

	t.Run("services", func(t *testing.T) {
		if _, err := sp.CommandContext(ctx, "sh", "-c", "mkdir -p ~/site && echo served-by-a-service > ~/site/index.html").Output(); err != nil {
			t.Fatal(err)
		}
		port := 3000
		stream, err := sp.CreateServiceWithDuration(ctx, "web", &sprites.ServiceRequest{
			Cmd: "python3", Args: []string{"-m", "http.server", "3000", "--directory", "/home/sprite/site"}, HTTPPort: &port,
		}, time.Second)
		if err != nil {
			t.Fatal(err)
		}
		started := false
		if err := stream.ProcessAll(func(ev *sprites.ServiceLogEvent) error {
			started = started || ev.Type == "started"
			return nil
		}); err != nil || !started {
			t.Fatalf("create stream: started=%v err=%v", started, err)
		}
		svc, err := sp.GetService(ctx, "web")
		if err != nil || svc.State == nil || svc.State.Status != "running" {
			t.Fatalf("get service: %+v %v", svc, err)
		}
		if _, err := sp.CreateService(ctx, "web2", &sprites.ServiceRequest{Cmd: "sleep", Args: []string{"1"}, HTTPPort: &port}); err == nil {
			t.Fatal("second http_port service should be a conflict")
		}

		// The sprite URL now routes to the service's port instead of 8080.
		if got := fetchURL(t, name); got != "served-by-a-service\n" {
			t.Fatalf("sprite URL served %q", got)
		}

		// A crash is restarted and counted.
		if err := sp.SignalService(ctx, "web", "KILL"); err != nil {
			t.Fatal(err)
		}
		deadline := time.Now().Add(10 * time.Second)
		for {
			svc, _ = sp.GetService(ctx, "web")
			if svc != nil && svc.State != nil && svc.State.Status == "running" && svc.State.RestartCount == 1 {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("service did not restart after crash: %+v", svc.State)
			}
			time.Sleep(100 * time.Millisecond)
		}

		// Cold boot: a checkpoint restore throws away all process state. The
		// service must come back from its on-disk definition, and the URL with it.
		cs, err := sp.CreateCheckpoint(ctx)
		if err != nil {
			t.Fatal(err)
		}
		cs.ProcessAll(func(*sprites.StreamMessage) error { return nil })
		cps, _ := sp.ListCheckpoints(ctx, "")
		rs, err := sp.RestoreCheckpoint(ctx, cps[0].ID)
		if err != nil {
			t.Fatal(err)
		}
		rs.ProcessAll(func(*sprites.StreamMessage) error { return nil })
		if got := fetchURL(t, name); got != "served-by-a-service\n" {
			t.Fatalf("after cold boot the sprite URL served %q", got)
		}
		svc, _ = sp.GetService(ctx, "web")
		if svc.State.RestartCount != 0 {
			t.Fatalf("restart_count should reset on a fresh boot, got %d", svc.State.RestartCount)
		}

		// Stop is sticky, and the URL proxy starts a stopped HTTP service on demand.
		stop, err := sp.StopService(ctx, "web")
		if err != nil {
			t.Fatal(err)
		}
		stop.ProcessAll(func(*sprites.ServiceLogEvent) error { return nil })
		if svc, _ = sp.GetService(ctx, "web"); svc.State.Status != "stopped" {
			t.Fatalf("after stop: %+v", svc.State)
		}
		if got := fetchURL(t, name); got != "served-by-a-service\n" {
			t.Fatalf("URL should start the stopped service on demand, served %q", got)
		}
		if err := sp.DeleteService(ctx, "web"); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("delete", func(t *testing.T) {
		if err := c.DeleteSprite(ctx, name); err != nil {
			t.Fatal(err)
		}
		if _, err := c.GetSprite(ctx, name); err == nil {
			t.Fatal("sprite still exists after delete")
		}
	})
}
