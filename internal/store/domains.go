package store

import (
	"errors"
	"slices"
	"time"
)

var (
	ErrDomainTaken   = errors.New("domain belongs to another sprite")
	ErrDomainLimit   = errors.New("too many domains")
	ErrDomainMissing = errors.New("domain not attached to this sprite")
)

// unclaimedLocked filters out the domains some sprite already holds.
func (s *Store) unclaimedLocked(domains []string) []string {
	var out []string
	for _, d := range domains {
		if _, taken := s.domainOwnerLocked(d); !taken && !slices.Contains(out, d) {
			out = append(out, d)
		}
	}
	return out
}

func (s *Store) domainOwnerLocked(domain string) (*Sprite, bool) {
	for _, sp := range s.byName {
		if slices.Contains(sp.Domains, domain) {
			return sp, true
		}
	}
	return nil, false
}

// DomainOwner returns the name of the sprite that domain is attached to.
func (s *Store) DomainOwner(domain string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sp, ok := s.domainOwnerLocked(domain)
	if !ok {
		return "", false
	}
	return sp.Name, true
}

// AllDomains is every attached domain, mapped to its sprite's name.
func (s *Store) AllDomains() map[string]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string]string{}
	for _, sp := range s.byName {
		for _, d := range sp.Domains {
			out[d] = sp.Name
		}
	}
	return out
}

// AttachDomain adds domain to the sprite, which may then hold at most perSprite
// domains, with at most total across the host (0 = no limit). Attaching a domain
// the sprite already has changes nothing.
func (s *Store) AttachDomain(name, domain string, perSprite, total int) (Sprite, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sp, ok := s.byName[name]
	if !ok {
		return Sprite{}, ErrNotFound
	}
	if owner, taken := s.domainOwnerLocked(domain); taken {
		if owner == sp {
			return *sp, nil
		}
		return Sprite{}, ErrDomainTaken
	}
	if perSprite > 0 && len(sp.Domains) >= perSprite {
		return Sprite{}, ErrDomainLimit
	}
	if total > 0 {
		n := 0
		for _, o := range s.byName {
			n += len(o.Domains)
		}
		if n >= total {
			return Sprite{}, ErrDomainLimit
		}
	}
	sp.Domains = append(slices.Clone(sp.Domains), domain)
	sp.UpdatedAt = time.Now().UTC()
	if err := s.save(sp); err != nil {
		sp.Domains = sp.Domains[:len(sp.Domains)-1]
		return Sprite{}, err
	}
	return *sp, nil
}

// DetachDomain removes domain from the sprite.
func (s *Store) DetachDomain(name, domain string) (Sprite, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sp, ok := s.byName[name]
	if !ok {
		return Sprite{}, ErrNotFound
	}
	i := slices.Index(sp.Domains, domain)
	if i < 0 {
		return Sprite{}, ErrDomainMissing
	}
	old := sp.Domains
	sp.Domains = slices.Delete(slices.Clone(old), i, i+1)
	if len(sp.Domains) == 0 {
		sp.Domains = nil
	}
	sp.UpdatedAt = time.Now().UTC()
	if err := s.save(sp); err != nil {
		sp.Domains = old
		return Sprite{}, err
	}
	return *sp, nil
}
