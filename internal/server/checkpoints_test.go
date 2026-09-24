package server

import (
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

	"github.com/jhgaylor/wisp/internal/store"
	"github.com/jhgaylor/wisp/internal/vmm"
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
	s := New(opts, st, NewLifecycle(opts, st, log), log, "tok", "org", []string{"sprites.localhost"}, "http://%s.%s:0")
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
		if _, err := s.createCheckpointLocked(rt, "cp", "", false, quiet); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 4; i++ {
		if err := s.autoCheckpointLocked(rt, "cp", "", "", quiet); err != nil {
			t.Fatal(err)
		}
	}
	// An auto never consumes a version number.
	if cp, err := s.createCheckpointLocked(rt, "cp", "", false, quiet); err != nil || cp.ID != "v3" {
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
		if _, err := os.Stat(s.checkpointPath(sp.ID, id)); (err == nil) != want {
			t.Errorf("clone of %s exists = %v, want %v", id, err == nil, want)
		}
	}
}

func TestRestoreIsUndoableAndTracksHistory(t *testing.T) {
	s, rt, read, write := newCheckpointServer(t, 1)
	quiet := func(string, ...any) {}
	write("good")
	s.createCheckpointLocked(rt, "cp", "", false, quiet) // v1
	write("better")
	s.createCheckpointLocked(rt, "cp", "", false, quiet) // v2
	write("broken")

	if err := s.restoreCheckpointLocked(rt, "cp", "v1", quiet, nil); err != nil {
		t.Fatal(err)
	}
	if read() != "good" {
		t.Fatalf("disk after restore = %q", read())
	}
	// The state the restore replaced was saved first; restoring it undoes the
	// restore, and must survive the prune that its own pre-restore auto triggers (keep is 1).
	if err := s.restoreCheckpointLocked(rt, "cp", "auto-1", quiet, nil); err != nil {
		t.Fatal(err)
	}
	if read() != "broken" {
		t.Fatalf("disk after undo = %q", read())
	}
	if err := s.restoreCheckpointLocked(rt, "cp", "v9", quiet, nil); err != errNoCheckpoint {
		t.Fatalf("restore of a missing checkpoint: %v", err)
	}

	s.restoreCheckpointLocked(rt, "cp", "v1", quiet, nil)
	cp, _ := s.createCheckpointLocked(rt, "cp", "", false, quiet) // v3, a child of v1 and not of v2
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
