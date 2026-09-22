package netpolicy

import (
	"io"
	"log/slog"
	"net/netip"
	"sync"
	"time"
)

// ttlGrace is how long past a record's TTL the address stays usable: clients
// cache answers a little longer than they should, and a connection attempt can
// trail the lookup that authorised it.
const ttlGrace = 60 * time.Second

// maxAllowed bounds the remembered addresses per sprite. At the bound expired
// ones are swept; if none have expired, new addresses are not learned (and so
// not reachable) rather than letting a guest grow the table without limit.
const maxAllowed = 4096

// Enforcer holds what the DNS listener and the proxy share: each sprite's
// policy, keyed by the source address the sprite is known by, and the
// destination addresses its allowed lookups have produced.
type Enforcer struct {
	log *slog.Logger
	now func() time.Time
	// OnDeny, when set before the listeners start, hears of every refusal that
	// can be put down to a sprite: kind is "dns" or "connect", target the name
	// or address. It is called on the listener's goroutine and must not block.
	OnDeny func(sprite, kind, target, reason string)

	mu      sync.Mutex
	sprites map[netip.Addr]*sprite
}

type sprite struct {
	name   string
	policy *Policy
	// allowed maps a destination to the names that resolved to it and when each
	// stops counting. Names are kept so a policy change can revoke precisely.
	allowed map[netip.Addr]map[string]time.Time
	flows   map[*flow]struct{}
}

// flow is one live proxied connection and the name that authorised it.
type flow struct {
	via  string
	conn io.Closer
}

// maxFlows bounds one sprite's concurrent proxied connections: each costs the
// daemon two file descriptors, and they are shared with every other sprite.
const maxFlows = 2048

func (e *Enforcer) denied(sprite, kind, target, reason string) {
	if e.OnDeny != nil && sprite != "" {
		e.OnDeny(sprite, kind, target, reason)
	}
}

func NewEnforcer(log *slog.Logger) *Enforcer {
	return &Enforcer{log: log, now: time.Now, sprites: map[netip.Addr]*sprite{}}
}

// Set installs the policy for the sprite at src. Addresses learned through
// names the new policy refuses are forgotten, and connections that relied on
// them are closed, so tightening a policy takes effect on live traffic.
func (e *Enforcer) Set(src netip.Addr, name string, p *Policy) {
	e.mu.Lock()
	sp := e.sprites[src]
	var doomed []*flow
	if sp != nil && sp.name != name {
		// A different sprite now owns this address; nothing of the old one carries over.
		for f := range sp.flows {
			doomed = append(doomed, f)
		}
		sp = nil
	}
	if sp == nil {
		sp = &sprite{name: name, allowed: map[netip.Addr]map[string]time.Time{}, flows: map[*flow]struct{}{}}
		e.sprites[src] = sp
	}
	sp.policy = p
	for dst, names := range sp.allowed {
		for n := range names {
			if !p.Allows(n) {
				delete(names, n)
			}
		}
		if len(names) == 0 {
			delete(sp.allowed, dst)
		}
	}
	for f := range sp.flows {
		if !p.permits(f.via) {
			doomed = append(doomed, f)
			delete(sp.flows, f)
		}
	}
	e.mu.Unlock()
	for _, f := range doomed {
		e.log.Info("egress connection closed by policy change", "sprite", name, "domain", f.via)
		f.conn.Close()
	}
}

// permits reports whether a connection authorised by via is still legitimate
// under p. An empty via marks one made while the sprite had no restrictive policy.
func (p *Policy) permits(via string) bool {
	if via == "" {
		return !p.Restrictive()
	}
	return p.Allows(via)
}

// Remove forgets the sprite at src and closes its connections.
func (e *Enforcer) Remove(src netip.Addr) {
	e.mu.Lock()
	var doomed []*flow
	if sp := e.sprites[src]; sp != nil {
		for f := range sp.flows {
			doomed = append(doomed, f)
		}
	}
	delete(e.sprites, src)
	e.mu.Unlock()
	// Closed outside the lock: each close makes its handler come back to untrack.
	for _, f := range doomed {
		f.conn.Close()
	}
}

// lookup returns the sprite's name and policy; ok is false for an address no sprite owns.
func (e *Enforcer) lookup(src netip.Addr) (name string, p *Policy, ok bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	sp := e.sprites[src]
	if sp == nil {
		return "", nil, false
	}
	return sp.name, sp.policy, true
}

// record remembers that an allowed lookup of name by the sprite at src returned dst.
func (e *Enforcer) record(src netip.Addr, name string, dst netip.Addr, ttl time.Duration) {
	e.mu.Lock()
	defer e.mu.Unlock()
	sp := e.sprites[src]
	if sp == nil || !sp.policy.Allows(name) { // the policy may have changed while upstream answered
		return
	}
	now := e.now()
	if sp.allowed[dst] == nil {
		if len(sp.allowed) >= maxAllowed {
			for d, names := range sp.allowed {
				for n, exp := range names {
					if now.After(exp) {
						delete(names, n)
					}
				}
				if len(names) == 0 {
					delete(sp.allowed, d)
				}
			}
		}
		if len(sp.allowed) >= maxAllowed {
			e.log.Warn("egress address table full; not learning more", "sprite", sp.name, "domain", name)
			return
		}
		sp.allowed[dst] = map[string]time.Time{}
	}
	sp.allowed[dst][canonical(name)] = now.Add(ttl + ttlGrace)
}

// authorize decides whether the sprite at src may connect to dst. via is the
// allowed name that resolved to dst, empty for a sprite with no restrictive
// policy (which may be diverted here briefly while the kernel set catches up).
func (e *Enforcer) authorize(src, dst netip.Addr) (who, via, denied string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	sp := e.sprites[src]
	if sp == nil {
		return "", "", "source is not a known sprite"
	}
	if len(sp.flows) >= maxFlows {
		return sp.name, "", "too many concurrent connections"
	}
	if !sp.policy.Restrictive() {
		return sp.name, "", ""
	}
	now := e.now()
	for n, exp := range sp.allowed[dst] {
		if now.Before(exp) {
			return sp.name, n, ""
		}
	}
	return sp.name, "", "address was not resolved from an allowed domain"
}

// track registers a live connection so a later policy change can close it. It
// re-checks the authorisation, which may have been revoked while we were dialing.
func (e *Enforcer) track(src netip.Addr, via string, conn io.Closer) (untrack func(), ok bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	sp := e.sprites[src]
	if sp == nil || !sp.policy.permits(via) {
		return nil, false
	}
	f := &flow{via: via, conn: conn}
	sp.flows[f] = struct{}{}
	return func() {
		e.mu.Lock()
		delete(sp.flows, f)
		e.mu.Unlock()
	}, true
}
