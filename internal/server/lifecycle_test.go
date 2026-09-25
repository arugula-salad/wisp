package server

import (
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
	l := &Lifecycle{store: st, log: quiet, runtimes: map[string]*runtime{}, quit: make(chan struct{})}

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
