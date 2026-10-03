package engine

import (
	"errors"
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

// An expired lease and a DELETE are one deletion (Engine.Delete): the same
// events after the reaper's sprite.expired, the same hook calls, and the same
// state left behind, on disk, in memory and in the bucket.
func TestLeaseExpiryAndAPIDeleteAreTheSameDeletion(t *testing.T) {
	l, byAPI, srv := newBackupEngine(t)
	byLease := store.Sprite{ID: store.NewID(), Name: "leased", CreatedAt: time.Now().UTC()}
	if err := l.store.Create(&byLease); err != nil {
		t.Fatal(err)
	}
	for _, sp := range []store.Sprite{byAPI, byLease} {
		dir := l.store.Dir(sp.ID)
		if err := os.WriteFile(filepath.Join(dir, vmm.DiskFile), []byte("disk"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(dir, "checkpoints"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(l.checkpointPath(sp.ID, "v1"), []byte("disk"), 0o644); err != nil {
			t.Fatal(err)
		}
		// Backed up, so the deletion has a tombstone to write.
		l.backups.Enqueue(sp.ID, "suspend")
		waitBackup(t, l, sp.ID)
		// Leased and inside the warning window, so the reaper has a warning to forget.
		soon := time.Now().Add(time.Minute).UTC()
		if _, err := l.SetDeadline(sp.ID, &soon, ""); err != nil {
			t.Fatal(err)
		}
		l.rt(sp.ID) // a runtime to forget
	}

	var mu sync.Mutex
	hooked := map[string]int{}
	l.OnDelete(func(sp store.Sprite) { mu.Lock(); hooked[sp.Name]++; mu.Unlock() })
	sub, _, _, _ := l.Events().Subscribe(func(Event) bool { return true }, 0, false)

	if err := l.Delete(byAPI.Record); err != nil { // what DELETE /v1/sprites/{name} does
		t.Fatal(err)
	}
	past := time.Now().Add(-time.Second)
	l.store.UpdateRecord(byLease.ID, func(r *store.Record) { r.ExpiresAt = &past })
	l.leases.sweep()

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
		if _, err := l.store.GetByName(store.Sprites, sp.Name); err == nil {
			t.Errorf("%s: record survived", sp.Name)
		}
		if _, err := os.Stat(l.store.Dir(sp.ID)); !os.IsNotExist(err) {
			t.Errorf("%s: machine directory survived: %v", sp.Name, err)
		}
		if n := hooked[sp.Name]; n != 1 {
			t.Errorf("%s: OnDelete ran %d times", sp.Name, n)
		}
		l.mu.Lock()
		_, kept := l.runtimes[sp.ID]
		l.mu.Unlock()
		if kept {
			t.Errorf("%s: runtime survived", sp.Name)
		}
		l.leases.mu.Lock()
		_, warned := l.leases.warned[sp.ID]
		reaping := l.leases.reaping[sp.ID]
		l.leases.mu.Unlock()
		if warned || reaping {
			t.Errorf("%s: lease bookkeeping survived (warned %v, reaping %v)", sp.Name, warned, reaping)
		}
		if st := l.backups.State(sp.ID); st.LastAt != nil || st.Phase != "idle" {
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
	l := newTestEngine(t, Options{})
	sub, _, _, _ := l.Events().Subscribe(func(Event) bool { return true }, 0, false)
	sp := store.Sprite{ID: store.NewID(), Name: "broken", CreatedAt: time.Now().UTC()}
	_, err := l.Create(t.Context(), CreateSpec{Sprite: sp, ImageDisk: filepath.Join(t.TempDir(), "missing.ext4")})
	if !errors.Is(err, errProvision) || !strings.HasPrefix(err.Error(), "provision disk: ") {
		t.Fatalf("err = %v, want errProvision", err)
	}
	if _, err := l.store.GetByName(store.Sprites, "broken"); err == nil {
		t.Error("the record of a sprite without a disk survived")
	}
	if _, err := os.Stat(l.store.Dir(sp.ID)); !os.IsNotExist(err) {
		t.Errorf("the machine directory survived: %v", err)
	}
	if got := collect(sub); len(got) != 0 {
		t.Errorf("a failed create published %v", got)
	}

	// From the base image it works, and gets a network index of its own.
	made, err := l.Create(t.Context(), CreateSpec{Sprite: sp})
	if err != nil || made.NetIndex == 0 {
		t.Fatalf("create = %+v, %v", made, err)
	}
	if _, err := os.Stat(filepath.Join(l.store.Dir(sp.ID), vmm.DiskFile)); err != nil {
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
	if _, err := l.Create(t.Context(), CreateSpec{Sprite: again}); !errors.Is(err, store.ErrExists) {
		t.Errorf("a second create under the name = %v, want store.ErrExists", err)
	}
}
