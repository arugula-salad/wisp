package store

import (
	"errors"
	"slices"
	"testing"
)

func TestDomainsBelongToOneSprite(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	mustCreate(t, s, "a")
	mustCreate(t, s, "b")
	if _, err := s.AttachDomain("a", "x.example.com", 2, 3); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AttachDomain("a", "x.example.com", 2, 3); err != nil {
		t.Fatalf("re-attaching to the same sprite: %v", err)
	}
	if _, err := s.AttachDomain("b", "x.example.com", 2, 3); !errors.Is(err, ErrDomainTaken) {
		t.Fatalf("attached to a second sprite: %v", err)
	}
	s.AttachDomain("a", "y.example.com", 2, 3)
	if _, err := s.AttachDomain("a", "z.example.com", 2, 3); !errors.Is(err, ErrDomainLimit) {
		t.Fatalf("per-sprite cap: %v", err)
	}
	s.AttachDomain("b", "z.example.com", 2, 3)
	if _, err := s.AttachDomain("b", "w.example.com", 2, 3); !errors.Is(err, ErrDomainLimit) {
		t.Fatalf("total cap: %v", err)
	}
	if owner, ok := s.DomainOwner("y.example.com"); !ok || owner != "a" {
		t.Fatalf("owner %q %v", owner, ok)
	}

	// Persisted, and freed by a detach or a delete.
	s2, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if sp, _ := s2.Get("a"); !slices.Equal(sp.Domains, []string{"x.example.com", "y.example.com"}) {
		t.Fatalf("after reopening: %v", sp.Domains)
	}
	if _, err := s2.DetachDomain("a", "x.example.com"); err != nil {
		t.Fatal(err)
	}
	if _, err := s2.DetachDomain("a", "x.example.com"); !errors.Is(err, ErrDomainMissing) {
		t.Fatalf("detaching twice: %v", err)
	}
	if _, err := s2.AttachDomain("b", "x.example.com", 0, 0); err != nil {
		t.Fatalf("a detached domain: %v", err)
	}
	s2.Delete("a")
	if _, ok := s2.DomainOwner("y.example.com"); ok {
		t.Fatal("a deleted sprite still owns its domain")
	}

	// A record that arrives with a domain someone else holds (a restore) loses it.
	sp := &Sprite{ID: NewID(), Name: "c", Domains: []string{"z.example.com", "fresh.example.com"}}
	if err := s2.Create(sp); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(sp.Domains, []string{"fresh.example.com"}) {
		t.Fatalf("restored record kept %v", sp.Domains)
	}
}
