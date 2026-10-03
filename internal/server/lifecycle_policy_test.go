package server

import (
	"context"
	"testing"
	"time"

	"github.com/arugula-salad/wisp/internal/store"
)

// A policy change on a sprite that is not running is stored and nothing more:
// ApplyPolicy does not wake it, and waits out a transition in flight first.
func TestApplyPolicyLeavesAStoppedSpriteAlone(t *testing.T) {
	s, rt, _, _ := newCheckpointServer(t, 0)
	sp, _ := s.store.GetByName(store.Sprites, "cp")
	rt.mu.Lock()
	done := make(chan error, 1)
	go func() { done <- s.life.ApplyPolicy(context.Background(), sp.Record) }()
	select {
	case <-done:
		t.Fatal("ApplyPolicy did not wait for the sprite's lock")
	case <-time.After(50 * time.Millisecond):
	}
	rt.mu.Unlock()
	if err := <-done; err != nil {
		t.Fatalf("ApplyPolicy on a stopped sprite: %v", err)
	}
	if s.life.Status(sp.Record) != "cold" {
		t.Errorf("ApplyPolicy changed the sprite's state to %s", s.life.Status(sp.Record))
	}
}
