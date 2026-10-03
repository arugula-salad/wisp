package server

import (
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/arugula-salad/wisp/internal/store"
	"github.com/arugula-salad/wisp/internal/vmm"
)

// An expired lease and a DELETE are one deletion (Lifecycle.Delete): the same
// events after the reaper's sprite.expired, the same hook calls, and the same
// state left behind, on disk, in memory and in the bucket.
func TestLeaseExpiryAndAPIDeleteAreTheSameDeletion(t *testing.T) {
	s, byAPI, srv := newBackupServer(t)
	h := s.Handler()
	byLease := store.Sprite{ID: store.NewID(), Name: "leased", CreatedAt: time.Now().UTC()}
	if err := s.store.Create(&byLease); err != nil {
		t.Fatal(err)
	}
	for _, sp := range []store.Sprite{byAPI, byLease} {
		dir := s.store.Dir(sp.ID)
		if err := os.WriteFile(filepath.Join(dir, vmm.DiskFile), []byte("disk"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(dir, "checkpoints"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(s.life.checkpointPath(sp.ID, "v1"), []byte("disk"), 0o644); err != nil {
			t.Fatal(err)
		}
		// Backed up, so the deletion has a tombstone to write.
		s.backups.Enqueue(sp.ID, "suspend")
		waitBackup(t, s, sp.ID)
		// Leased and inside the warning window, so the reaper has a warning to forget.
		soon := time.Now().Add(time.Minute).UTC()
		if _, err := s.life.SetDeadline(sp.ID, &soon, ""); err != nil {
			t.Fatal(err)
		}
		s.life.rt(sp.ID) // a runtime to forget
	}

	var mu sync.Mutex
	hooked := map[string]int{}
	s.life.OnDelete(func(sp store.Sprite) { mu.Lock(); hooked[sp.Name]++; mu.Unlock() })
	sub, _, _ := s.life.events.subscribe(func(Event) bool { return true }, 0, false)

	status(t, apiCall(t, h, "DELETE", "/v1/sprites/"+byAPI.Name, ""), http.StatusNoContent)
	expire(t, s, byLease.Name, time.Now().Add(-time.Second))
	s.leases.sweep()

	events := map[string][]string{}
	for _, e := range collect(sub) {
		events[e.Sprite] = append(events[e.Sprite], e.Type)
	}
	if got := strings.Join(events[byLease.Name], " "); got != "sprite.expired sprite.deleted" {
		t.Errorf("reaped sprite's events = %q", got)
	}
	if got := strings.Join(events[byAPI.Name], " "); got != "sprite.deleted" {
		t.Errorf("deleted sprite's events = %q", got)
	}

	deadline := time.Now().Add(5 * time.Second)
	for _, sp := range []store.Sprite{byAPI, byLease} {
		if _, err := s.store.GetByName(store.Sprites, sp.Name); err == nil {
			t.Errorf("%s: record survived", sp.Name)
		}
		if _, err := os.Stat(s.store.Dir(sp.ID)); !os.IsNotExist(err) {
			t.Errorf("%s: machine directory survived: %v", sp.Name, err)
		}
		if n := hooked[sp.Name]; n != 1 {
			t.Errorf("%s: OnDelete ran %d times", sp.Name, n)
		}
		s.life.mu.Lock()
		_, kept := s.life.runtimes[sp.ID]
		s.life.mu.Unlock()
		if kept {
			t.Errorf("%s: runtime survived", sp.Name)
		}
		s.leases.mu.Lock()
		_, warned := s.leases.warned[sp.ID]
		reaping := s.leases.reaping[sp.ID]
		s.leases.mu.Unlock()
		if warned || reaping {
			t.Errorf("%s: lease bookkeeping survived (warned %v, reaping %v)", sp.Name, warned, reaping)
		}
		if st := s.backups.State(sp.ID); st.LastAt != nil || st.Phase != "idle" {
			t.Errorf("%s: backup state survived: %+v", sp.Name, st)
		}
		for {
			if _, ok := srv.Get("sprites/" + sp.ID + "/deleted.json"); ok {
				break
			}
			if time.Now().After(deadline) {
				t.Errorf("%s: no tombstone in the bucket", sp.Name)
				break
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
}

func TestCreateRollsBackADiskThatCouldNotBeMade(t *testing.T) {
	s, _ := newOperatorServer(t, Options{})
	sub, _, _ := s.life.events.subscribe(func(Event) bool { return true }, 0, false)
	sp := store.Sprite{ID: store.NewID(), Name: "broken", CreatedAt: time.Now().UTC()}
	_, err := s.life.Create(t.Context(), CreateSpec{Sprite: sp, ImageDisk: filepath.Join(t.TempDir(), "missing.ext4")})
	if !errors.Is(err, errProvision) || !strings.HasPrefix(err.Error(), "provision disk: ") {
		t.Fatalf("err = %v, want errProvision", err)
	}
	if _, err := s.store.GetByName(store.Sprites, "broken"); err == nil {
		t.Error("the record of a sprite without a disk survived")
	}
	if _, err := os.Stat(s.store.Dir(sp.ID)); !os.IsNotExist(err) {
		t.Errorf("the machine directory survived: %v", err)
	}
	if got := collect(sub); len(got) != 0 {
		t.Errorf("a failed create published %v", got)
	}

	// From the base image it works, and gets a network index of its own.
	made, err := s.life.Create(t.Context(), CreateSpec{Sprite: sp})
	if err != nil || made.NetIndex == 0 {
		t.Fatalf("create = %+v, %v", made, err)
	}
	if _, err := os.Stat(filepath.Join(s.store.Dir(sp.ID), vmm.DiskFile)); err != nil {
		t.Errorf("no disk: %v", err)
	}
	var types []string
	for _, e := range collect(sub) {
		types = append(types, e.Type)
	}
	if !slices.Equal(types, []string{"sprite.created"}) {
		t.Errorf("events = %v", types)
	}
	// And the name is taken now.
	again := store.Sprite{ID: store.NewID(), Name: "broken"}
	if _, err := s.life.Create(t.Context(), CreateSpec{Sprite: again}); !errors.Is(err, store.ErrExists) {
		t.Errorf("a second create under the name = %v, want store.ErrExists", err)
	}
}
