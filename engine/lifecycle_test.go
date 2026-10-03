package engine

import (
	"context"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/arugula-salad/wisp/internal/store"
)

// Shutdown stops the background loops before it suspends anything, and waits
// for a pass already in flight rather than suspending underneath it.
func TestShutdownStopsLoops(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	l := &Engine{store: st, log: quiet, runtimes: map[string]*runtime{}, unstored: map[string]store.Sprite{}, quit: make(chan struct{})}

	var passes atomic.Int32
	inPass, release := make(chan struct{}), make(chan struct{})
	l.every(time.Millisecond, func() {
		if passes.Add(1) == 1 {
			close(inPass)
			<-release
		}
	})
	<-inPass

	done := make(chan struct{})
	go func() { l.Shutdown(); close(done) }()
	select {
	case <-done:
		t.Fatal("Shutdown returned while a loop pass was still running")
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Shutdown never returned")
	}

	after := passes.Load()
	time.Sleep(20 * time.Millisecond)
	if passes.Load() != after {
		t.Fatal("a loop ran on after Shutdown")
	}
	// A loop started once Shutdown has begun never runs, and a second Shutdown is harmless.
	l.every(time.Millisecond, func() { t.Error("loop started after Shutdown ran") })
	time.Sleep(20 * time.Millisecond)
	l.Shutdown()
}

// Peek never waits for a transition: a held lock reads as busy, with the
// in-flight API count still reported.
func TestPeekDoesNotWaitForATransition(t *testing.T) {
	l, rt, _, _ := newCheckpointEngine(t, 0)
	sp, _ := l.store.GetByName(store.Sprites, "cp")
	rt.begin()
	defer rt.end()
	if vm := l.Peek(sp.ID); vm.Busy || vm.Running() || vm.Inflight != 1 || vm.Pid != 0 {
		t.Errorf("stopped sprite = %+v; want settled, not running, one request in flight", vm)
	}
	if _, ok := l.Peek(sp.ID).TaskHolds(context.Background()); ok {
		t.Error("a stopped sprite answered for its tasks")
	}
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if vm := l.Peek(sp.ID); !vm.Busy || vm.Inflight != 1 {
		t.Errorf("sprite mid-transition = %+v; want busy, one request in flight", vm)
	}
}

// The guest's hostname is the record's, which the front end chose: a sprite's
// is its name, whether it was just created or read back from disk, and the
// engine never looks at the name itself.
func TestVMHostnameIsTheRecords(t *testing.T) {
	l := newTestEngine(t, Options{})
	st := l.store
	dir := filepath.Dir(filepath.Dir(st.Dir("x")))
	createSprite(t, l, "web")
	other := &store.Sprite{Record: store.Record{ID: store.NewID(), API: "e2b", Hostname: "sbx"}}
	if err := st.Create(other); err != nil {
		t.Fatal(err)
	}
	reopened, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, st := range []*store.Store{st, reopened} {
		sp, _ := st.GetByName(store.Sprites, "web")
		if got := l.vmConfig(sp.Record, "").Hostname; got != "web" {
			t.Errorf("sprite hostname %q, want its name", got)
		}
		e, _ := st.GetRecord(other.ID)
		if got := l.vmConfig(e, "").Hostname; got != "sbx" {
			t.Errorf("another API's hostname %q", got)
		}
	}
}
