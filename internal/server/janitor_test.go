package server

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/arugula-salad/wisp/internal/store"
	"github.com/arugula-salad/wisp/internal/vmm"
)

// warmSprite makes a suspended sprite whose snapshot was taken at warmedAt.
func warmSprite(t *testing.T, s *Server, name string, warmedAt time.Time) store.Sprite {
	t.Helper()
	sp := &store.Sprite{ID: store.NewID(), Name: name, CreatedAt: time.Now(), LastWarmingAt: &warmedAt}
	if err := s.store.Create(sp); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"snap.vmstate", "snap.mem"} {
		if err := os.WriteFile(filepath.Join(s.store.Dir(sp.ID), f), bytes.Repeat([]byte{1}, 4096), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return *sp
}

func TestJanitorCoolsSpritesWarmPastTheTTL(t *testing.T) {
	s, _ := newOperatorServer(t, Options{WarmTTL: time.Hour})
	old := warmSprite(t, s, "old", time.Now().Add(-2*time.Hour))
	fresh := warmSprite(t, s, "fresh", time.Now().Add(-time.Minute))
	for _, sp := range []store.Sprite{old, fresh} {
		if s.life.warmExpired(sp) {
			s.life.coolIfExpired(sp)
		}
	}
	if vmm.HasSnapshot(s.store.Dir(old.ID)) {
		t.Error("a sprite warm for two hours kept its snapshot past a one-hour TTL")
	}
	if !vmm.HasSnapshot(s.store.Dir(fresh.ID)) {
		t.Error("a sprite warm for a minute lost its snapshot")
	}
}

// The janitor picks candidates from a list read before it waits on each
// sprite's lock. A suspend in flight holds that lock and renews LastWarmingAt,
// so by the time the janitor gets in, its copy is stale: deciding on it threw
// away a snapshot seconds old. That is what turned sprites cold across restarts.
func TestJanitorDoesNotCoolASpriteThatSuspendedWhileItWaited(t *testing.T) {
	s, _ := newOperatorServer(t, Options{WarmTTL: time.Hour})
	stale := warmSprite(t, s, "game", time.Now().Add(-2*time.Hour)) // warm long ago, then woke
	if !s.life.warmExpired(stale) {
		t.Fatal("setup: the stale record should look expired")
	}

	rt := s.life.rt(stale.ID)
	rt.mu.Lock() // a suspend in flight
	done := make(chan struct{})
	go func() {
		s.life.coolIfExpired(stale) // the janitor, holding the stale copy
		close(done)
	}()
	select {
	case <-done:
		t.Fatal("the janitor did not wait for the sprite's lock")
	case <-time.After(50 * time.Millisecond):
	}
	// What suspendLocked does before letting go: a fresh snapshot, a new LastWarmingAt.
	now := time.Now()
	s.store.Update(stale.Name, func(sp *store.Sprite) { sp.LastWarmingAt = &now })
	rt.mu.Unlock()
	<-done

	if !vmm.HasSnapshot(s.store.Dir(stale.ID)) {
		t.Fatal("the janitor dropped a snapshot taken moments ago, deciding on a stale LastWarmingAt")
	}
}

func TestJanitorLeavesADeletedSpriteAlone(t *testing.T) {
	s, _ := newOperatorServer(t, Options{WarmTTL: time.Hour})
	sp := warmSprite(t, s, "gone", time.Now().Add(-2*time.Hour))
	// Deleted and recreated under the same name while the janitor held the old record.
	if err := s.store.Delete(sp.Name); err != nil {
		t.Fatal(err)
	}
	again := warmSprite(t, s, "gone", time.Now().Add(-2*time.Hour))
	s.life.coolIfExpired(sp)
	if !vmm.HasSnapshot(s.store.Dir(again.ID)) {
		t.Error("the janitor cooled a different sprite that took the old one's name")
	}
}
