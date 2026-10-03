package engine

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/arugula-salad/wisp/internal/store"
	"github.com/arugula-salad/wisp/internal/vmm"
)

// cpID is the ID of newCheckpointEngine's sprite, named "cp".
const cpID = "0000000000c9"

// newCheckpointEngine has one never-booted sprite whose "disk" is a text file:
// with no VM running the checkpoint core is plain file cloning.
func newCheckpointEngine(t *testing.T, keep int) (*Engine, *runtime, func() string, func(string)) {
	t.Helper()
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	opts := Options{NoNetwork: true, AutoCheckpointKeep: keep}
	l := New(opts, st, log)
	l.SetDescriber(spriteName)
	sp := &store.Sprite{ID: cpID, Name: "cp", CreatedAt: time.Now()}
	if err := st.Create(sp); err != nil {
		t.Fatal(err)
	}
	disk := filepath.Join(st.Dir(sp.ID), vmm.DiskFile)
	read := func() string { b, _ := os.ReadFile(disk); return string(b) }
	write := func(v string) {
		if err := os.WriteFile(disk, []byte(v), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return l, l.rt(sp.ID), read, write
}

func ids(cps []store.Checkpoint) []string {
	out := []string{}
	for _, cp := range cps {
		out = append(out, cp.ID)
	}
	return out
}

func TestAutoCheckpointsAreSeparateHiddenAndPruned(t *testing.T) {
	l, rt, _, write := newCheckpointEngine(t, 2)
	quiet := func(string, ...any) {}
	write("one")
	for i := 0; i < 2; i++ {
		if _, err := l.createCheckpointLocked(rt, cpID, "", false, quiet); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 4; i++ {
		if err := l.autoCheckpointLocked(rt, cpID, "", "", quiet); err != nil {
			t.Fatal(err)
		}
	}
	// An auto never consumes a version number.
	if cp, err := l.createCheckpointLocked(rt, cpID, "", false, quiet); err != nil || cp.ID != "v3" {
		t.Fatalf("next manual checkpoint = %q, %v; want v3", cp.ID, err)
	}
	sp, _ := l.store.GetByName(store.Sprites, "cp")
	if got, want := ids(ListCheckpoints(sp.Record, "", false)), []string{"v3", "v2", "v1"}; !slices.Equal(got, want) {
		t.Errorf("default listing = %v, want %v", got, want)
	}
	if got, want := ids(ListCheckpoints(sp.Record, "", true)), []string{"v3", "auto-4", "auto-3", "v2", "v1"}; !slices.Equal(got, want) {
		t.Errorf("listing with autos = %v, want %v (oldest autos pruned)", got, want)
	}
	for id, want := range map[string]bool{"auto-1": false, "auto-2": false, "auto-3": true, "v1": true} {
		if _, err := os.Stat(l.checkpointPath(sp.ID, id)); (err == nil) != want {
			t.Errorf("clone of %s exists = %v, want %v", id, err == nil, want)
		}
	}
}

func TestRestoreIsUndoableAndTracksHistory(t *testing.T) {
	l, rt, read, write := newCheckpointEngine(t, 1)
	quiet := func(string, ...any) {}
	write("good")
	l.createCheckpointLocked(rt, cpID, "", false, quiet) // v1
	write("better")
	l.createCheckpointLocked(rt, cpID, "", false, quiet) // v2
	write("broken")

	if err := l.restoreCheckpointLocked(rt, cpID, "v1", quiet, nil); err != nil {
		t.Fatal(err)
	}
	if read() != "good" {
		t.Fatalf("disk after restore = %q", read())
	}
	// The state the restore replaced was saved first; restoring it undoes the
	// restore, and must survive the prune that its own pre-restore auto triggers (keep is 1).
	if err := l.restoreCheckpointLocked(rt, cpID, "auto-1", quiet, nil); err != nil {
		t.Fatal(err)
	}
	if read() != "broken" {
		t.Fatalf("disk after undo = %q", read())
	}
	if err := l.restoreCheckpointLocked(rt, cpID, "v9", quiet, nil); err != ErrNoCheckpoint {
		t.Fatalf("restore of a missing checkpoint: %v", err)
	}

	l.restoreCheckpointLocked(rt, cpID, "v1", quiet, nil)
	cp, _ := l.createCheckpointLocked(rt, cpID, "", false, quiet) // v3, a child of v1 and not of v2
	if !slices.Equal(cp.History, []string{"v1"}) {
		t.Errorf("v3 history = %v, want [v1]", cp.History)
	}
	sp, _ := l.store.GetByName(store.Sprites, "cp")
	if got, want := ids(ListCheckpoints(sp.Record, "v1", false)), []string{"v3", "v2"}; !slices.Equal(got, want) {
		t.Errorf("history=v1 = %v, want %v", got, want)
	}
	if got := ids(ListCheckpoints(sp.Record, "v2", false)); len(got) != 0 {
		t.Errorf("history=v2 = %v, want none", got)
	}
}

// A request that arrived on a guest channel is refused once that channel's VM
// is no longer the running one. (The in-guest ceiling is the front end's:
// internal/server's TestGuestCheckpointRequests.)
func TestGuestRequestsAreScopedToTheirVM(t *testing.T) {
	l, rt, _, write := newCheckpointEngine(t, 1)
	write("disk")
	sp, _ := l.store.GetByName(store.Sprites, "cp")
	quiet := func(string, ...any) {}

	live, gone := &GuestChan{}, &GuestChan{}
	rt.guest = live
	if _, err := l.CreateCheckpoint(sp.Record, gone, "", quiet); err != ErrStaleGuest {
		t.Errorf("stale channel: %v, want ErrStaleGuest", err)
	}
	if sp, _ = l.store.GetByName(store.Sprites, "cp"); len(sp.Checkpoints) != 0 {
		t.Fatalf("a stale request created %v", ids(sp.Checkpoints))
	}
	if cp, err := l.CreateCheckpoint(sp.Record, live, "", quiet); err != nil || cp.ID != "v1" {
		t.Errorf("live channel: %q, %v; want v1", cp.ID, err)
	}
	if cp, err := l.CreateCheckpoint(sp.Record, nil, "", quiet); err != nil || cp.ID != "v2" {
		t.Errorf("the public API: %q, %v; want v2", cp.ID, err)
	}
}

// The Engine's checkpoint methods take the sprite's lock themselves, so a
// transition in flight holds them off.
func TestCheckpointMethodsWaitForTheSpriteLock(t *testing.T) {
	l, rt, _, write := newCheckpointEngine(t, 1)
	write("disk")
	sp, _ := l.store.GetByName(store.Sprites, "cp")
	quiet := func(string, ...any) {}

	rt.mu.Lock() // a suspend, say
	done := make(chan error, 1)
	go func() {
		_, err := l.CreateCheckpoint(sp.Record, nil, "", quiet)
		done <- err
	}()
	select {
	case <-done:
		t.Fatal("CreateCheckpoint did not wait for the sprite's lock")
	case <-time.After(50 * time.Millisecond):
	}
	rt.mu.Unlock()
	if err := <-done; err != nil {
		t.Fatal(err)
	}

	live, gone := &GuestChan{}, &GuestChan{}
	rt.guest = live
	if err := l.RestoreCheckpoint(sp.Record, gone, "v1", quiet, nil); err != ErrStaleGuest {
		t.Errorf("restore from a stale channel: %v, want ErrStaleGuest", err)
	}
	if err := l.RestoreCheckpoint(sp.Record, live, "v1", quiet, nil); err != nil {
		t.Errorf("restore from the live channel: %v", err)
	}
	if err := l.DeleteCheckpoint(sp.Record, "v9"); err != ErrNoCheckpoint {
		t.Errorf("delete of a missing checkpoint: %v, want ErrNoCheckpoint", err)
	}
}

// A checkpoint mounted inside a running sprite cannot be deleted, and the
// mount bookkeeping answers what it can without touching a drive.
func TestCheckpointMountsUnderTheLock(t *testing.T) {
	l, rt, _, write := newCheckpointEngine(t, 1)
	write("disk")
	sp, _ := l.store.GetByName(store.Sprites, "cp")
	quiet := func(string, ...any) {}
	l.CreateCheckpoint(sp.Record, nil, "", quiet) // v1
	l.CreateCheckpoint(sp.Record, nil, "", quiet) // v2

	live := &GuestChan{}
	if _, err := l.MountCheckpoint(context.Background(), sp.Record, live, "v1"); err != ErrStaleGuest {
		t.Errorf("mount on a stopped sprite: %v, want ErrStaleGuest", err)
	}
	if err := l.UnmountCheckpoint(context.Background(), sp.Record, live, "v1"); err != ErrStaleGuest {
		t.Errorf("unmount on a stopped sprite: %v, want ErrStaleGuest", err)
	}

	// Running, as far as the bookkeeping can tell; no drive is swapped below.
	rt.m, rt.guest = &vmm.Machine{}, live
	sp, _ = l.store.UpdateByName(store.Sprites, "cp", func(sp *store.Sprite) { sp.Mounts = map[int]string{1: "v1"} })
	if slot, err := l.MountCheckpoint(context.Background(), sp.Record, live, "v1"); err != nil || slot != 1 {
		t.Errorf("mounting a mounted checkpoint = %d, %v; want its slot, 1", slot, err)
	}
	if _, err := l.MountCheckpoint(context.Background(), sp.Record, live, "v9"); err != ErrNoCheckpoint {
		t.Errorf("mount of a missing checkpoint: %v, want ErrNoCheckpoint", err)
	}
	if err := l.UnmountCheckpoint(context.Background(), sp.Record, live, "v2"); err != nil {
		t.Errorf("unmounting what is not mounted: %v", err)
	}
	if err := l.DeleteCheckpoint(sp.Record, "v1"); err != ErrCheckpointMounted {
		t.Errorf("delete of a mounted checkpoint: %v, want ErrCheckpointMounted", err)
	}
	if err := l.DeleteCheckpoint(sp.Record, "v2"); err != nil {
		t.Errorf("delete of an unmounted checkpoint: %v", err)
	}

	full := map[int]string{}
	for i := range vmm.CheckpointSlots {
		full[i] = fmt.Sprintf("x%d", i)
	}
	l.store.UpdateByName(store.Sprites, "cp", func(sp *store.Sprite) { sp.Mounts = full })
	if _, err := l.MountCheckpoint(context.Background(), sp.Record, live, "v1"); err != ErrMountsFull {
		t.Errorf("mount with every slot taken: %v, want ErrMountsFull", err)
	}
	rt.m, rt.guest = nil, nil
}

// HoldCheckpoint picks the newest manual checkpoint by default and keeps it
// from being deleted until it is released.
func TestHoldCheckpointKeepsTheCheckpoint(t *testing.T) {
	l, rt, _, write := newCheckpointEngine(t, 1)
	write("disk")
	sp, _ := l.store.GetByName(store.Sprites, "cp")
	quiet := func(string, ...any) {}
	if _, _, _, err := l.HoldCheckpoint(sp.Record, ""); err != ErrNoCheckpoint {
		t.Fatalf("hold with no checkpoints: %v, want ErrNoCheckpoint", err)
	}
	l.CreateCheckpoint(sp.Record, nil, "", quiet)   // v1
	l.CreateCheckpoint(sp.Record, nil, "", quiet)   // v2
	l.autoCheckpointLocked(rt, cpID, "", "", quiet) // auto-1, never the default
	cur, id, release, err := l.HoldCheckpoint(sp.Record, "")
	if err != nil || id != "v2" || len(cur.Checkpoints) != 3 {
		t.Fatalf("hold = %q (%d checkpoints), %v; want v2 on a fresh record", id, len(cur.Checkpoints), err)
	}
	done := make(chan error, 1)
	go func() { done <- l.DeleteCheckpoint(cur, "v2") }()
	select {
	case <-done:
		t.Fatal("a held checkpoint was deleted")
	case <-time.After(50 * time.Millisecond):
	}
	release()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := l.HoldCheckpoint(store.Record{ID: "nope"}, ""); err != store.ErrNotFound {
		t.Errorf("hold on a deleted sprite: %v, want store.ErrNotFound", err)
	}
}
