package server

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/arugula-salad/wisp/internal/store"
	"github.com/arugula-salad/wisp/internal/vmm"
)

// newCheckpointServer has one never-booted sprite whose "disk" is a text file:
// with no VM running the checkpoint core is plain file cloning.
func newCheckpointServer(t *testing.T, keep int) (*Server, *runtime, func() string, func(string)) {
	t.Helper()
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	opts := Options{NoNetwork: true, AutoCheckpointKeep: keep}
	s := New(testURLs(opts, "org", "http://%s.%s:0"), st, NewLifecycle(opts, st, log), log, "tok")
	sp := &store.Sprite{ID: store.NewID(), Name: "cp", CreatedAt: time.Now()}
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
	return s, s.life.rt(sp.ID), read, write
}

func ids(cps []store.Checkpoint) []string {
	out := []string{}
	for _, cp := range cps {
		out = append(out, cp.ID)
	}
	return out
}

func TestAutoCheckpointsAreSeparateHiddenAndPruned(t *testing.T) {
	s, rt, _, write := newCheckpointServer(t, 2)
	quiet := func(string, ...any) {}
	write("one")
	for i := 0; i < 2; i++ {
		if _, err := s.life.createCheckpointLocked(rt, "cp", "", false, quiet); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 4; i++ {
		if err := s.life.autoCheckpointLocked(rt, "cp", "", "", quiet); err != nil {
			t.Fatal(err)
		}
	}
	// An auto never consumes a version number.
	if cp, err := s.life.createCheckpointLocked(rt, "cp", "", false, quiet); err != nil || cp.ID != "v3" {
		t.Fatalf("next manual checkpoint = %q, %v; want v3", cp.ID, err)
	}
	sp, _ := s.store.Get("cp")
	if got, want := ids(filterCheckpoints(sp, "", false)), []string{"v3", "v2", "v1"}; !slices.Equal(got, want) {
		t.Errorf("default listing = %v, want %v", got, want)
	}
	if got, want := ids(filterCheckpoints(sp, "", true)), []string{"v3", "auto-4", "auto-3", "v2", "v1"}; !slices.Equal(got, want) {
		t.Errorf("listing with autos = %v, want %v (oldest autos pruned)", got, want)
	}
	for id, want := range map[string]bool{"auto-1": false, "auto-2": false, "auto-3": true, "v1": true} {
		if _, err := os.Stat(s.life.checkpointPath(sp.ID, id)); (err == nil) != want {
			t.Errorf("clone of %s exists = %v, want %v", id, err == nil, want)
		}
	}
}

func TestRestoreIsUndoableAndTracksHistory(t *testing.T) {
	s, rt, read, write := newCheckpointServer(t, 1)
	quiet := func(string, ...any) {}
	write("good")
	s.life.createCheckpointLocked(rt, "cp", "", false, quiet) // v1
	write("better")
	s.life.createCheckpointLocked(rt, "cp", "", false, quiet) // v2
	write("broken")

	if err := s.life.restoreCheckpointLocked(rt, "cp", "v1", quiet, nil); err != nil {
		t.Fatal(err)
	}
	if read() != "good" {
		t.Fatalf("disk after restore = %q", read())
	}
	// The state the restore replaced was saved first; restoring it undoes the
	// restore, and must survive the prune that its own pre-restore auto triggers (keep is 1).
	if err := s.life.restoreCheckpointLocked(rt, "cp", "auto-1", quiet, nil); err != nil {
		t.Fatal(err)
	}
	if read() != "broken" {
		t.Fatalf("disk after undo = %q", read())
	}
	if err := s.life.restoreCheckpointLocked(rt, "cp", "v9", quiet, nil); err != errNoCheckpoint {
		t.Fatalf("restore of a missing checkpoint: %v", err)
	}

	s.life.restoreCheckpointLocked(rt, "cp", "v1", quiet, nil)
	cp, _ := s.life.createCheckpointLocked(rt, "cp", "", false, quiet) // v3, a child of v1 and not of v2
	if !slices.Equal(cp.History, []string{"v1"}) {
		t.Errorf("v3 history = %v, want [v1]", cp.History)
	}
	sp, _ := s.store.Get("cp")
	if got, want := ids(filterCheckpoints(sp, "v1", false)), []string{"v3", "v2"}; !slices.Equal(got, want) {
		t.Errorf("history=v1 = %v, want %v", got, want)
	}
	if got := ids(filterCheckpoints(sp, "v2", false)); len(got) != 0 {
		t.Errorf("history=v2 = %v, want none", got)
	}
}

// A request that arrived on a guest channel is refused once that channel's VM
// is no longer the running one, and is held to the in-guest ceiling.
func TestGuestRequestsAreScopedToTheirVM(t *testing.T) {
	s, rt, _, write := newCheckpointServer(t, 1)
	s.opts.GuestCheckpointLimit = 1
	write("disk")
	sp, _ := s.store.Get("cp")
	post := func(from *guestChan) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		sp, _ := s.store.Get("cp")
		s.createCheckpoint(w, httptest.NewRequest(http.MethodPost, "/v1/checkpoint", nil), sp, from)
		return w
	}

	live, gone := &guestChan{}, &guestChan{}
	rt.guest = live
	if w := post(gone); !strings.Contains(w.Body.String(), errStaleGuest.Error()) {
		t.Errorf("stale channel: %s", w.Body)
	}
	if sp, _ = s.store.Get("cp"); len(sp.Checkpoints) != 0 {
		t.Fatalf("a stale request created %v", ids(sp.Checkpoints))
	}
	if w := post(live); !strings.Contains(w.Body.String(), "Checkpoint v1 created") {
		t.Errorf("live channel: %s", w.Body)
	}
	if w := post(live); w.Code != http.StatusConflict {
		t.Errorf("over the ceiling: %d %s", w.Code, w.Body)
	}
	if w := post(nil); !strings.Contains(w.Body.String(), "Checkpoint v2 created") {
		t.Errorf("the public API has no ceiling: %s", w.Body)
	}
}

// The Lifecycle's checkpoint methods take the sprite's lock themselves, so a
// transition in flight holds them off.
func TestCheckpointMethodsWaitForTheSpriteLock(t *testing.T) {
	s, rt, _, write := newCheckpointServer(t, 1)
	write("disk")
	sp, _ := s.store.Get("cp")
	quiet := func(string, ...any) {}

	rt.mu.Lock() // a suspend, say
	done := make(chan error, 1)
	go func() {
		_, err := s.life.CreateCheckpoint(sp, nil, "", quiet)
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

	live, gone := &guestChan{}, &guestChan{}
	rt.guest = live
	if err := s.life.RestoreCheckpoint(sp, gone, "v1", quiet, nil); err != errStaleGuest {
		t.Errorf("restore from a stale channel: %v, want errStaleGuest", err)
	}
	if err := s.life.RestoreCheckpoint(sp, live, "v1", quiet, nil); err != nil {
		t.Errorf("restore from the live channel: %v", err)
	}
	if err := s.life.DeleteCheckpoint(sp, "v9"); err != errNoCheckpoint {
		t.Errorf("delete of a missing checkpoint: %v, want errNoCheckpoint", err)
	}
}

// A checkpoint mounted inside a running sprite cannot be deleted, and the
// mount bookkeeping answers what it can without touching a drive.
func TestCheckpointMountsUnderTheLock(t *testing.T) {
	s, rt, _, write := newCheckpointServer(t, 1)
	write("disk")
	sp, _ := s.store.Get("cp")
	quiet := func(string, ...any) {}
	s.life.CreateCheckpoint(sp, nil, "", quiet) // v1
	s.life.CreateCheckpoint(sp, nil, "", quiet) // v2

	live := &guestChan{}
	if _, err := s.life.MountCheckpoint(context.Background(), sp, live, "v1"); err != errStaleGuest {
		t.Errorf("mount on a stopped sprite: %v, want errStaleGuest", err)
	}
	if err := s.life.UnmountCheckpoint(context.Background(), sp, live, "v1"); err != errStaleGuest {
		t.Errorf("unmount on a stopped sprite: %v, want errStaleGuest", err)
	}

	// Running, as far as the bookkeeping can tell; no drive is swapped below.
	rt.m, rt.guest = &vmm.Machine{}, live
	sp, _ = s.store.Update("cp", func(sp *store.Sprite) { sp.Mounts = map[int]string{1: "v1"} })
	if slot, err := s.life.MountCheckpoint(context.Background(), sp, live, "v1"); err != nil || slot != 1 {
		t.Errorf("mounting a mounted checkpoint = %d, %v; want its slot, 1", slot, err)
	}
	if _, err := s.life.MountCheckpoint(context.Background(), sp, live, "v9"); err != errNoCheckpoint {
		t.Errorf("mount of a missing checkpoint: %v, want errNoCheckpoint", err)
	}
	if err := s.life.UnmountCheckpoint(context.Background(), sp, live, "v2"); err != nil {
		t.Errorf("unmounting what is not mounted: %v", err)
	}
	if err := s.life.DeleteCheckpoint(sp, "v1"); err != errCheckpointMounted {
		t.Errorf("delete of a mounted checkpoint: %v, want errCheckpointMounted", err)
	}
	if err := s.life.DeleteCheckpoint(sp, "v2"); err != nil {
		t.Errorf("delete of an unmounted checkpoint: %v", err)
	}

	full := map[int]string{}
	for i := range vmm.CheckpointSlots {
		full[i] = fmt.Sprintf("x%d", i)
	}
	s.store.Update("cp", func(sp *store.Sprite) { sp.Mounts = full })
	if _, err := s.life.MountCheckpoint(context.Background(), sp, live, "v1"); err != errMountsFull {
		t.Errorf("mount with every slot taken: %v, want errMountsFull", err)
	}
	rt.m, rt.guest = nil, nil
}

// HoldCheckpoint picks the newest manual checkpoint by default and keeps it
// from being deleted until it is released.
func TestHoldCheckpointKeepsTheCheckpoint(t *testing.T) {
	s, rt, _, write := newCheckpointServer(t, 1)
	write("disk")
	sp, _ := s.store.Get("cp")
	quiet := func(string, ...any) {}
	if _, _, _, err := s.life.HoldCheckpoint(sp, ""); err != errNoCheckpoint {
		t.Fatalf("hold with no checkpoints: %v, want errNoCheckpoint", err)
	}
	s.life.CreateCheckpoint(sp, nil, "", quiet)          // v1
	s.life.CreateCheckpoint(sp, nil, "", quiet)          // v2
	s.life.autoCheckpointLocked(rt, "cp", "", "", quiet) // auto-1, never the default
	cur, id, release, err := s.life.HoldCheckpoint(sp, "")
	if err != nil || id != "v2" || len(cur.Checkpoints) != 3 {
		t.Fatalf("hold = %q (%d checkpoints), %v; want v2 on a fresh record", id, len(cur.Checkpoints), err)
	}
	done := make(chan error, 1)
	go func() { done <- s.life.DeleteCheckpoint(cur, "v2") }()
	select {
	case <-done:
		t.Fatal("a held checkpoint was deleted")
	case <-time.After(50 * time.Millisecond):
	}
	release()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := s.life.HoldCheckpoint(store.Sprite{ID: sp.ID, Name: "nope"}, ""); err != store.ErrNotFound {
		t.Errorf("hold on a deleted sprite: %v, want store.ErrNotFound", err)
	}
}
