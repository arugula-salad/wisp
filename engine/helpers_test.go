package engine

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/arugula-salad/wisp/internal/store"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

// spriteName is a Describer like the Sprites front end's: a sprite is known
// by its name, and a spawned one by its parent too.
func spriteName(sp store.Sprite) (name, parentID string) {
	if sp.API != store.Sprites {
		return "", ""
	}
	return sp.Name, sp.ParentID
}

// newTestEngine is an engine with a tiny base image and no VMs: enough for
// everything short of booting one. It names records as the Sprites front end
// does, and reaps leases from the start, as a daemon's does once its front end
// is attached.
func newTestEngine(t *testing.T, opts Options) *Engine {
	t.Helper()
	opts.DataDir, opts.NoNetwork = t.TempDir(), true
	opts.BaseImage = filepath.Join(opts.DataDir, "base.ext4")
	if err := os.WriteFile(opts.BaseImage, bytes.Repeat([]byte("base"), 4096), 0o644); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(opts.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	l := New(opts, st, quiet)
	l.SetDescriber(spriteName)
	l.StartReaping()
	return l
}

// createSprite makes a sprite named name the way the Sprites API does, with
// whatever the modifiers set before it is created.
func createSprite(t *testing.T, l *Engine, name string, mod ...func(*store.Sprite)) store.Sprite {
	t.Helper()
	now := time.Now().UTC()
	sp := store.Sprite{Record: store.Record{ID: store.NewID(), Hostname: name, CreatedAt: now, UpdatedAt: now},
		SpriteMeta: store.SpriteMeta{Name: name, URLSettings: store.URLSettings{Auth: "sprite"}}}
	for _, m := range mod {
		m(&sp)
	}
	created, err := l.Create(context.Background(), CreateSpec{Sprite: sp})
	if err != nil {
		t.Fatalf("create %s: %v", name, err)
	}
	return created
}

// waitFor polls cond for up to five seconds.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// collect drains the events published so far.
func collect(sub *Subscription) []Event {
	var out []Event
	for {
		select {
		case e := <-sub.Events():
			out = append(out, e)
		default:
			return out
		}
	}
}
