package engine

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/arugula-salad/wisp/internal/store"
	"github.com/arugula-salad/wisp/internal/vmm"
)

func TestMaxRunningRefusesAWake(t *testing.T) {
	l := newTestEngine(t, Options{MaxRunning: 1, IdleTimeout: 30 * time.Second})
	if err := l.reserveRun(); err != nil {
		t.Fatal(err)
	}
	err := l.reserveRun()
	var lim *LimitError
	if !errors.As(err, &lim) {
		t.Fatalf("second reservation: %v", err)
	}
	if lim.Which != "max_running" || lim.Code != "" || lim.Limit != 1 || lim.Current != 1 || lim.RetryAfter != 30 {
		t.Fatalf("limit error = %+v", lim)
	}
	l.releaseRun()
	if err := l.reserveRun(); err != nil {
		t.Fatalf("a freed slot should be usable: %v", err)
	}
}

// fakeVolume stands in for statfs: free space is whatever the test last stored.
// (The guard also probes from its own goroutines, hence the atomic.)
func fakeVolume(l *Engine) *atomic.Int64 {
	free := new(atomic.Int64)
	l.disk.probe = func() (Headroom, error) {
		return Headroom{VolumeTotal: 40 << 30, VolumeFree: free.Load(), Free: free.Load()}, nil
	}
	return free
}

func TestDiskGuardRefusesCreatesAndCheckpoints(t *testing.T) {
	l := newTestEngine(t, Options{DiskReserve: 2 << 30})
	vol := fakeVolume(l)
	vol.Store(3 << 30)
	createSprite(t, l, "fits")

	vol.Store(1 << 30)
	sub, _, _, _ := l.Events().Subscribe(func(e Event) bool { return e.Type == "disk.refused" }, 0, false)
	defer l.Events().Unsubscribe(sub)
	full := store.Sprite{Record: store.Record{ID: store.NewID(), Hostname: "full"}, SpriteMeta: store.SpriteMeta{Name: "full"}}
	if _, err := l.Create(context.Background(), CreateSpec{Sprite: full}); !errors.Is(err, ErrNoRoom) {
		t.Fatalf("create on a full volume: %v", err)
	}
	if _, err := l.store.GetByName(store.Sprites, "full"); err == nil {
		t.Fatal("a refused create left a sprite behind")
	}
	// The refusal comes before there is a record, and still names the sprite.
	select {
	case e := <-sub.Events():
		if e.Sprite != "full" || e.SpriteID == "" || e.Detail["operation"] != "a new sprite" {
			t.Fatalf("disk.refused = %+v", e)
		}
	case <-time.After(time.Second):
		t.Fatal("no disk.refused event")
	}
	sp, _ := l.store.GetByName(store.Sprites, "fits")
	_, err := l.createCheckpointLocked(l.rt(sp.ID), sp.ID, "", false, func(string, ...any) {})
	if !errors.Is(err, ErrNoRoom) {
		t.Fatalf("checkpoint on a full volume: %v", err)
	}
	if got, _ := l.store.GetByName(store.Sprites, "fits"); len(got.Checkpoints) != 0 {
		t.Fatal("a refused checkpoint was recorded")
	}
}

func TestMakeRoomTurnsTheOldestWarmSpritesCold(t *testing.T) {
	l := newTestEngine(t, Options{})
	vol := fakeVolume(l)
	const snap = 1 << 20
	warmed := time.Now().Add(-time.Hour)
	for i, name := range []string{"oldest", "older", "newest", "suspending"} {
		sp := &store.Sprite{ID: store.NewID(), Name: name, CreatedAt: time.Now()}
		if err := l.store.Create(sp); err != nil {
			t.Fatal(err)
		}
		if name == "suspending" {
			continue
		}
		at := warmed.Add(time.Duration(i) * time.Minute)
		l.store.UpdateByName(store.Sprites, name, func(sp *store.Sprite) { sp.LastWarmingAt = &at })
		for _, f := range []string{"snap.vmstate", "snap.mem"} {
			if err := os.WriteFile(filepath.Join(l.store.Dir(sp.ID), f), bytes.Repeat([]byte{1}, snap/2), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	warm := func(name string) bool {
		sp, _ := l.store.GetByName(store.Sprites, name)
		return vmm.HasSnapshot(l.store.Dir(sp.ID))
	}
	me, _ := l.store.GetByName(store.Sprites, "suspending")

	room := func(need int64) bool {
		release, fits := l.makeRoom(me.Record, need)
		release()
		return fits
	}
	vol.Store(10 * snap)
	if !room(5*snap) || !warm("oldest") {
		t.Fatal("a snapshot that fits should cost nobody anything")
	}
	// Two suspends at once may not both be promised the same free bytes.
	release, fits := l.makeRoom(me.Record, 9*snap)
	if !fits || !warm("oldest") {
		t.Fatal("9 of 10 free should fit")
	}
	vol.Store(10*snap + snap/2) // what a second suspend sees while the first still writes
	other, _ := l.store.GetByName(store.Sprites, "newest")
	if _, fits := l.makeRoom(other.Record, 9*snap); fits {
		t.Fatal("the same space was promised twice")
	}
	release()
	if !warm("oldest") || !warm("older") {
		t.Fatal("an attempt that could not succeed still cost sprites their memory state")
	}

	// Half a snapshot short: one demotion covers it, and it is the oldest that goes.
	vol.Store(snap)
	if !room(snap + snap/2) {
		t.Fatal("no room even after a demotion")
	}
	if warm("oldest") || !warm("older") || !warm("newest") {
		t.Fatalf("warm after one demotion: oldest=%v older=%v newest=%v", warm("oldest"), warm("older"), warm("newest"))
	}

	// Hopeless: the caller is told so, and nobody is turned cold for nothing.
	vol.Store(snap)
	if room(100 * snap) {
		t.Fatal("reported room that is not there")
	}
	if !warm("older") || !warm("newest") {
		t.Fatal("warm sprites were dropped for a snapshot that could never fit")
	}
}

func TestExclusiveCountsOnlyUnsharedBytes(t *testing.T) {
	owners := [][]span{
		merge([]span{{0, 60}, {40, 100}}), // overlaps itself: still one owner
		{{50, 150}},
		{{200, 300}},
		nil,
	}
	got := exclusive(owners)
	for i, want := range []int64{50, 50, 100, 0} {
		if got[i] != want {
			t.Fatalf("owner %d: %d exclusive bytes, want %d (all: %v)", i, got[i], want, got)
		}
	}
	if n := total(owners[0]); n != 100 {
		t.Fatalf("merged total = %d, want 100", n)
	}
}
