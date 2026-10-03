package store

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func mustCreate(t *testing.T, s *Store, name string) *Sprite {
	t.Helper()
	sp := &Sprite{ID: NewID(), Name: name, CreatedAt: time.Now().UTC()}
	if err := s.Create(sp); err != nil {
		t.Fatalf("create %s: %v", name, err)
	}
	return sp
}

func TestCreateAllocatesDistinctAddressesAndReusesFreedOnes(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	a, b := mustCreate(t, s, "a"), mustCreate(t, s, "b")
	// Index 1 is the bridge itself, so allocation starts at 2.
	if a.NetIndex != 2 || b.NetIndex != 3 {
		t.Fatalf("indexes = %d, %d; want 2, 3", a.NetIndex, b.NetIndex)
	}
	if err := s.Create(&Sprite{ID: NewID(), Name: "a"}); !errors.Is(err, ErrExists) {
		t.Fatalf("duplicate name: %v", err)
	}
	if err := s.Delete(a.ID); err != nil {
		t.Fatal(err)
	}
	if c := mustCreate(t, s, "c"); c.NetIndex != 2 {
		t.Fatalf("freed index not reused: got %d", c.NetIndex)
	}
}

func TestAllocationSkipsNetworkAndBroadcastLookingOctets(t *testing.T) {
	s, _ := Open(t.TempDir())
	seen := map[int]bool{}
	for i := 0; i < 600; i++ {
		sp := mustCreate(t, s, "s"+NewID())
		if low := sp.NetIndex & 0xff; low == 0 || low == 255 {
			t.Fatalf("allocated index %d (x.x.%d.%d)", sp.NetIndex, sp.NetIndex>>8, low)
		}
		if seen[sp.NetIndex] {
			t.Fatalf("index %d allocated twice", sp.NetIndex)
		}
		seen[sp.NetIndex] = true
	}
}

func TestRecordsSurviveReopenAndDeleteRemovesTheMachineDir(t *testing.T) {
	dir := t.TempDir()
	s, _ := Open(dir)
	sp := mustCreate(t, s, "keep")
	os.WriteFile(filepath.Join(s.Dir(sp.ID), "disk.ext4"), []byte("x"), 0o644)
	if _, err := s.Update(sp.ID, func(sp *Sprite) { sp.Labels = []string{"prod"}; sp.BootIP = "10.209.0.2/16" }); err != nil {
		t.Fatal(err)
	}
	// A directory without a record (a create that died half way) must not break startup.
	os.MkdirAll(filepath.Join(dir, "vm", "deadbeef0000"), 0o755)

	s2, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	got, err := s2.Get(sp.ID)
	if err != nil || got.ID != sp.ID || got.NetIndex != sp.NetIndex || len(got.Labels) != 1 || got.BootIP != "10.209.0.2/16" {
		t.Fatalf("after reopen: %+v %v", got, err)
	}
	if n := len(s2.List(Sprites, "")); n != 1 {
		t.Fatalf("list after reopen has %d sprites", n)
	}
	if err := s2.Delete(sp.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(s2.Dir(sp.ID)); !os.IsNotExist(err) {
		t.Fatalf("machine dir still present after delete: %v", err)
	}
	if _, err := s2.Get(sp.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("get after delete: %v", err)
	}
}

func TestGetReturnsACopy(t *testing.T) {
	s, _ := Open(t.TempDir())
	x := mustCreate(t, s, "x")
	got, _ := s.Get(x.ID)
	got.Name = "mutated"
	if again, _ := s.Get(x.ID); again.Name != "x" {
		t.Fatal("Get exposed the stored record for mutation")
	}
}

func TestListFiltersByPrefixAndSorts(t *testing.T) {
	s, _ := Open(t.TempDir())
	for _, n := range []string{"web-2", "api-1", "web-1"} {
		mustCreate(t, s, n)
	}
	got := s.List(Sprites, "web-")
	if len(got) != 2 || got[0].Name != "web-1" || got[1].Name != "web-2" {
		t.Fatalf("list = %+v", got)
	}
}

// Records are keyed by ID; a name is unique within its API's namespace only,
// and a record without one is reachable by ID alone.
func TestNamesAreUniquePerAPI(t *testing.T) {
	dir := t.TempDir()
	s, _ := Open(dir)
	sp := mustCreate(t, s, "web")
	if err := s.Create(&Sprite{Record: Record{ID: sp.ID, API: "e2b"}}); !errors.Is(err, ErrExists) {
		t.Fatalf("duplicate ID: %v", err)
	}
	other := &Sprite{Record: Record{ID: NewID(), API: "other"}, SpriteMeta: SpriteMeta{Name: "web"}}
	if err := s.Create(other); err != nil {
		t.Fatalf("same name in another namespace: %v", err)
	}
	e2b := &Sprite{Record: Record{ID: NewID(), API: "e2b"}}
	if err := s.Create(e2b); err != nil {
		t.Fatal(err)
	}
	e2b2 := &Sprite{Record: Record{ID: NewID(), API: "e2b"}}
	if err := s.Create(e2b2); err != nil {
		t.Fatalf("a second nameless record: %v", err)
	}
	if e2b.NetIndex == e2b2.NetIndex || e2b.NetIndex == sp.NetIndex || other.NetIndex == sp.NetIndex {
		t.Fatalf("addresses shared across APIs: %d %d %d %d", sp.NetIndex, other.NetIndex, e2b.NetIndex, e2b2.NetIndex)
	}
	for _, s := range []*Store{s, reopen(t, dir)} {
		if got, err := s.GetByName(Sprites, "web"); err != nil || got.ID != sp.ID {
			t.Fatalf("sprite by name: %+v %v", got, err)
		}
		if got, err := s.GetByName("other", "web"); err != nil || got.ID != other.ID {
			t.Fatalf("other API by name: %+v %v", got, err)
		}
		if _, err := s.GetByName("e2b", ""); !errors.Is(err, ErrNotFound) {
			t.Fatalf("nameless record found by its empty name: %v", err)
		}
		if got, err := s.GetRecord(e2b.ID); err != nil || got.API != "e2b" {
			t.Fatalf("by ID: %+v %v", got, err)
		}
		if l := s.List(Sprites, ""); len(l) != 1 || l[0].ID != sp.ID {
			t.Fatalf("sprites list = %+v", l)
		}
		if l := s.List("e2b", ""); len(l) != 0 {
			t.Fatalf("nameless records listed by name: %+v", l)
		}
		if n, all := s.Count(), s.Records(); n != 4 || len(all) != 4 || all[0].ID != sp.ID {
			t.Fatalf("count %d, records %+v", n, all)
		}
	}
	// Deleting one record leaves the same name in another namespace alone.
	if err := s.Delete(other.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetByName(Sprites, "web"); err != nil {
		t.Fatalf("delete in one namespace took the other's name: %v", err)
	}
	if _, err := s.GetByName("other", "web"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted name still found: %v", err)
	}
	if _, err := s.UpdateRecord(e2b.ID, func(r *Record) { r.BootIP = "x" }); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Get(e2b.ID); got.BootIP != "x" {
		t.Fatalf("UpdateRecord not applied: %+v", got.Record)
	}
	if got, err := s.UpdateByName(Sprites, "web", func(sp *Sprite) { sp.Labels = []string{"l"} }); err != nil || got.ID != sp.ID {
		t.Fatalf("UpdateByName: %+v %v", got, err)
	}
	if _, err := s.UpdateByName("other", "web", func(*Sprite) {}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("UpdateByName on a deleted name: %v", err)
	}
}

func reopen(t *testing.T, dir string) *Store {
	t.Helper()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	return s
}
