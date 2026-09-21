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
	if err := s.Delete("a"); err != nil {
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
	if _, err := s.Update("keep", func(sp *Sprite) { sp.Labels = []string{"prod"}; sp.BootIP = "10.209.0.2/16" }); err != nil {
		t.Fatal(err)
	}
	// A directory without a record (a create that died half way) must not break startup.
	os.MkdirAll(filepath.Join(dir, "vm", "deadbeef0000"), 0o755)

	s2, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	got, err := s2.Get("keep")
	if err != nil || got.ID != sp.ID || got.NetIndex != sp.NetIndex || len(got.Labels) != 1 || got.BootIP != "10.209.0.2/16" {
		t.Fatalf("after reopen: %+v %v", got, err)
	}
	if n := len(s2.List("")); n != 1 {
		t.Fatalf("list after reopen has %d sprites", n)
	}
	if err := s2.Delete("keep"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(s2.Dir(sp.ID)); !os.IsNotExist(err) {
		t.Fatalf("machine dir still present after delete: %v", err)
	}
	if _, err := s2.Get("keep"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("get after delete: %v", err)
	}
}

func TestGetReturnsACopy(t *testing.T) {
	s, _ := Open(t.TempDir())
	mustCreate(t, s, "x")
	got, _ := s.Get("x")
	got.Name = "mutated"
	if again, _ := s.Get("x"); again.Name != "x" {
		t.Fatal("Get exposed the stored record for mutation")
	}
}

func TestListFiltersByPrefixAndSorts(t *testing.T) {
	s, _ := Open(t.TempDir())
	for _, n := range []string{"web-2", "api-1", "web-1"} {
		mustCreate(t, s, n)
	}
	got := s.List("web-")
	if len(got) != 2 || got[0].Name != "web-1" || got[1].Name != "web-2" {
		t.Fatalf("list = %+v", got)
	}
}
