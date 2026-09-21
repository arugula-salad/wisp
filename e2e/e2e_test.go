//go:build e2e

// Package e2e drives a running spritesd with the official Sprites Go SDK, so
// wire compatibility is checked against the real client rather than our own.
//
//	SPRITES_E2E_URL=http://127.0.0.1:7788 SPRITES_E2E_TOKEN=$(cat ~/.local/share/mini-sprites/token) \
//	  go test -tags e2e -count=1 -v ./e2e/
package e2e

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	sprites "github.com/superfly/sprites-go"
)

func client(t *testing.T) *sprites.Client {
	url, tok := os.Getenv("SPRITES_E2E_URL"), os.Getenv("SPRITES_E2E_TOKEN")
	if url == "" || tok == "" {
		t.Skip("SPRITES_E2E_URL / SPRITES_E2E_TOKEN not set")
	}
	return sprites.New(tok, sprites.WithBaseURL(url))
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
		cmd := sp.CommandContext(ctx, "sh", "-c", "test -t 1 && stty size")
		cmd.SetTTY(true)
		cmd.SetTTYSize(40, 120)
		out, err := cmd.CombinedOutput()
		if err != nil || !strings.Contains(string(out), "40 120") {
			t.Fatalf("out=%q err=%v", out, err)
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

	t.Run("delete", func(t *testing.T) {
		if err := c.DeleteSprite(ctx, name); err != nil {
			t.Fatal(err)
		}
		if _, err := c.GetSprite(ctx, name); err == nil {
			t.Fatal("sprite still exists after delete")
		}
	})
}
