package server

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/arugula-salad/wisp/engine"
	"github.com/arugula-salad/wisp/internal/store"
	"github.com/arugula-salad/wisp/internal/vmm"
)

// newCheckpointServer has one never-booted sprite, "cp", whose "disk" is a
// text file: with no VM running a checkpoint is plain file cloning.
func newCheckpointServer(t *testing.T, keep int) *Server {
	t.Helper()
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	opts := Options{Options: engine.Options{NoNetwork: true, AutoCheckpointKeep: keep}}
	s := New(testURLs(opts, "org", "http://%s.%s:0"), st, engine.New(opts.Options, st, log), log, "tok")
	sp := &store.Sprite{ID: store.NewID(), Name: "cp", CreatedAt: time.Now()}
	if err := st.Create(sp); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(st.Dir(sp.ID), vmm.DiskFile), []byte("disk"), 0o644); err != nil {
		t.Fatal(err)
	}
	return s
}

// A request from inside is held to the in-guest ceiling, which the public API
// is not; one from a VM that is no longer running is refused as stale.
func TestGuestCheckpointRequests(t *testing.T) {
	s := newCheckpointServer(t, 1)
	s.opts.GuestCheckpointLimit = 1
	post := func(from *engine.GuestChan) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		sp, _ := s.store.GetByName(store.Sprites, "cp")
		s.createCheckpoint(w, httptest.NewRequest(http.MethodPost, "/v1/checkpoint", nil), sp, from)
		return w
	}

	// The sprite is not running, so no channel is its VM's.
	gone := &engine.GuestChan{}
	if w := post(gone); !strings.Contains(w.Body.String(), engine.ErrStaleGuest.Error()) {
		t.Errorf("stale channel: %s", w.Body)
	}
	if sp, _ := s.store.GetByName(store.Sprites, "cp"); len(sp.Checkpoints) != 0 {
		t.Fatalf("a stale request created %d checkpoints", len(sp.Checkpoints))
	}
	if w := post(nil); !strings.Contains(w.Body.String(), "Checkpoint v1 created") {
		t.Errorf("the public API: %s", w.Body)
	}
	if w := post(gone); w.Code != http.StatusConflict {
		t.Errorf("over the ceiling: %d %s", w.Code, w.Body)
	}
	if w := post(nil); !strings.Contains(w.Body.String(), "Checkpoint v2 created") {
		t.Errorf("the public API has no ceiling: %s", w.Body)
	}
}
