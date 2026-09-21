package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

type watchClient struct {
	t    *testing.T
	conn *websocket.Conn
	seen []watchMsg
}

func dialWatch(t *testing.T, tsURL string) *watchClient {
	t.Helper()
	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(tsURL, "http")+"/fs/watch", nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	return &watchClient{t: t, conn: conn}
}

// until reads messages until one satisfies ok, keeping everything it saw.
func (c *watchClient) until(what string, ok func(watchMsg) bool) watchMsg {
	c.t.Helper()
	c.conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	for {
		var m watchMsg
		if err := c.conn.ReadJSON(&m); err != nil {
			c.t.Fatalf("waiting for %s: %v (saw %+v)", what, err, c.seen)
		}
		c.seen = append(c.seen, m)
		if ok(m) {
			return m
		}
	}
}

func (c *watchClient) event(op, path string) watchMsg {
	c.t.Helper()
	return c.until(op+" "+path, func(m watchMsg) bool { return m.Type == "event" && m.Event == op && m.Path == path })
}

func TestFsWatch(t *testing.T) {
	ts, _ := newTestServer(t)
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "sub/deep"), 0o755)
	os.WriteFile(filepath.Join(root, "single.txt"), []byte("1"), 0o644)
	os.MkdirAll(filepath.Join(root, "flat/inner"), 0o755)

	c := dialWatch(t, ts.URL)
	c.conn.WriteJSON(watchMsg{Type: "subscribe", Paths: []string{"sub", "missing"}, Recursive: true, WorkingDir: root})
	c.until("error for the missing path", func(m watchMsg) bool { return m.Type == "error" && strings.HasSuffix(m.Path, "/missing") })
	sub := c.until("subscribed", func(m watchMsg) bool { return m.Type == "subscribed" })
	if len(sub.Paths) != 1 || sub.Paths[0] != filepath.Join(root, "sub") {
		t.Fatalf("subscribed: %+v", sub)
	}

	// A pre-existing nested directory is covered.
	f := filepath.Join(root, "sub/deep/a.txt")
	os.WriteFile(f, []byte("hello"), 0o644)
	if m := c.event("create", f); m.IsDir || m.Timestamp == "" {
		t.Fatalf("create event: %+v", m)
	}
	if m := c.event("write", f); m.Size != 5 {
		t.Fatalf("write event size: %+v", m)
	}

	// A tree that appears all at once: everything in it is reported even though
	// the contents beat the watches, and the new directories are watched.
	os.MkdirAll(filepath.Join(root, "sub/new/x/y"), 0o755)
	if m := c.event("create", filepath.Join(root, "sub/new/x/y")); !m.IsDir {
		t.Fatalf("nested create: %+v", m)
	}
	late := filepath.Join(root, "sub/new/x/y/late.txt")
	os.WriteFile(late, nil, 0o644)
	c.event("create", late)
	os.Chmod(late, 0o600)
	c.event("chmod", late)

	// A directory renamed away takes its watches with it; its old paths must go quiet.
	os.Rename(filepath.Join(root, "sub/new"), filepath.Join(root, "moved"))
	c.event("rename", filepath.Join(root, "sub/new"))
	os.WriteFile(filepath.Join(root, "moved/x/y/ghost.txt"), nil, 0o644)
	os.Remove(f)
	c.event("remove", f)
	renames := 0
	for _, m := range c.seen {
		if strings.Contains(m.Path, "ghost") {
			t.Fatalf("event from a directory that left the watched tree: %+v", m)
		}
		if m.Event == "rename" {
			renames++
		}
	}
	if renames != 1 {
		t.Fatalf("the directory's move was reported %d times", renames)
	}
	if n := watchDirsInUse.Load(); n != 2 { // sub, sub/deep
		t.Fatalf("watched directories after the rename: %d, want 2", n)
	}

	// Non-recursive: children of subdirectories are not reported. A second subscribe extends the first.
	c.conn.WriteJSON(watchMsg{Type: "subscribe", Paths: []string{filepath.Join(root, "flat"), filepath.Join(root, "single.txt")}})
	c.until("second subscribed", func(m watchMsg) bool { return m.Type == "subscribed" && len(m.Paths) == 2 })
	before := len(c.seen)
	os.WriteFile(filepath.Join(root, "flat/inner/hidden.txt"), nil, 0o644)
	os.WriteFile(filepath.Join(root, "sibling.txt"), nil, 0o644) // same directory as single.txt, but not subscribed
	// Replaced by rename, the way editors save: the watch must survive it.
	os.WriteFile(filepath.Join(root, "single.tmp"), []byte("22"), 0o644)
	os.Rename(filepath.Join(root, "single.tmp"), filepath.Join(root, "single.txt"))
	c.event("create", filepath.Join(root, "single.txt"))
	os.WriteFile(filepath.Join(root, "single.txt"), []byte("333"), 0o644)
	c.event("write", filepath.Join(root, "single.txt"))
	for _, m := range c.seen[before:] {
		if strings.Contains(m.Path, "hidden") || strings.Contains(m.Path, "sibling") || strings.Contains(m.Path, "single.tmp") {
			t.Fatalf("unsubscribed path reported: %+v", m)
		}
	}

	c.hangUp()
}

// hangUp disconnects and waits for the server to give back every watch.
func (c *watchClient) hangUp() {
	c.t.Helper()
	c.conn.Close()
	deadline := time.Now().Add(3 * time.Second)
	for watchDirsInUse.Load() != 0 || watchConnsInUse.Load() != 0 {
		if time.Now().After(deadline) {
			c.t.Fatalf("watches leaked after disconnect: dirs=%d conns=%d", watchDirsInUse.Load(), watchConnsInUse.Load())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// A client that stops reading must not stall the agent or grow its memory:
// events past the queue are dropped and counted.
func TestFsWatchSlowReader(t *testing.T) {
	fw := &fsWatcher{out: make(chan watchMsg, watchQueue)}
	done := make(chan struct{})
	go func() {
		for i := 0; i < watchQueue+500; i++ {
			fw.event("/nonexistent", "write")
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("send blocked on a full queue")
	}
	if _, ok := fw.overflow(); ok {
		t.Fatal("overflow notice offered while the queue is still full")
	}
	for len(fw.out) > 0 {
		<-fw.out
	}
	if m, ok := fw.overflow(); !ok || !strings.Contains(m.Message, "500 events dropped") {
		t.Fatalf("overflow notice: %+v %v", m, ok)
	}
}

// A tree beyond the watch budget is refused whole, and gives the budget back.
func TestFsWatchLimit(t *testing.T) {
	defer func(n int64) { maxWatchDirs = n }(maxWatchDirs)
	maxWatchDirs = 5
	ts, _ := newTestServer(t)
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "big/1/2/3/4/5/6"), 0o755)
	os.MkdirAll(filepath.Join(root, "small/1"), 0o755)

	c := dialWatch(t, ts.URL)
	c.conn.WriteJSON(watchMsg{Type: "subscribe", Paths: []string{filepath.Join(root, "big")}, Recursive: true})
	c.until("limit error", func(m watchMsg) bool { return m.Type == "error" && strings.Contains(m.Message, "watch limit") })
	c.until("subscribed to nothing", func(m watchMsg) bool { return m.Type == "subscribed" && len(m.Paths) == 0 })
	if n := watchDirsInUse.Load(); n != 0 {
		t.Fatalf("a refused subscription kept %d watches", n)
	}
	c.conn.WriteJSON(watchMsg{Type: "subscribe", Paths: []string{filepath.Join(root, "small")}, Recursive: true})
	c.until("subscribed", func(m watchMsg) bool { return m.Type == "subscribed" && len(m.Paths) == 1 })

	// The tree then outgrows the budget: the client is told, once, and what is already watched keeps working.
	os.MkdirAll(filepath.Join(root, "small/a/b/c/d/e"), 0o755)
	c.until("limit error", func(m watchMsg) bool { return m.Type == "error" && strings.Contains(m.Message, "watch limit") })
	f := filepath.Join(root, "small/1/f")
	os.WriteFile(f, nil, 0o644)
	c.event("create", f)
	c.hangUp()
}
