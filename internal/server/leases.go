package server

import (
	"encoding/json"
	"net/http"
	"sync"
	"time"

	"github.com/jhgaylor/wisp/internal/store"
)

// A lease is an expiry on a whole sprite: when it runs out the sprite is
// deleted, disk, checkpoints and address included. Nothing else in wispd
// ever deletes a sprite, which was fine while every sprite was somebody's
// workspace and stopped being fine when a lobby began handing one to every
// visitor (spawn.go): spawn_policy.max_children caps how many exist at once,
// but with nothing reaping them the cap is reached and stays reached.
//
// It is opt-in and off by default, and there is deliberately no operator flag
// for a default lease: a persistent sprite must not acquire an expiry because
// of a daemon's configuration. A lease is something a caller asked for, per
// sprite, and `protected` suspends one without forgetting it.
//
// Expiry means deletion and nothing else. It does not suspend the sprite, it
// does not take a backup first, and where a backup bucket is configured the
// reaper leaves the same tombstone DELETE /v1/sprites/{name} leaves: proof the
// sprite was deleted, never a promise that its last upload was current.
//
// The endpoint lives outside /v1, like the event stream and the webhook status,
// so it cannot collide with anything upstream has or adds; the fields on a
// sprite ride along in upstream's shape, where an SDK that does not know them
// ignores them.

// leasePath is the renewal endpoint, ours, outside /v1.
const leasePath = "/wisp/v1/sprites/{name}/lease"

// defaultLeaseWarning is how long before expiry sprite.expiring goes out when
// the operator has set no Options.LeaseWarning.
const defaultLeaseWarning = 5 * time.Minute

// leases is the reaper and the bookkeeping the reaper needs. It hangs off the
// Server because deleting a sprite is the API's path, not the lifecycle's.
type leases struct {
	s *Server

	mu sync.Mutex
	// warned is the deadline each sprite was already warned about, so a sweep
	// every 30 s does not warn every 30 s while a renewal does earn a new
	// warning. In memory, like the event ring itself: a restart may repeat a
	// warning, which is cheaper than a field on the record that a restored
	// sprite could carry back from another host.
	warned map[string]time.Time
	// reaping names the sprites a reap has committed to. A lease change that
	// finds its sprite here has lost the race and is refused, rather than
	// renewing a lease on a disk that is already going away.
	reaping map[string]bool
}

func newLeases(s *Server) *leases {
	return &leases{s: s, warned: map[string]time.Time{}, reaping: map[string]bool{}}
}

// setLeases hands the reaper to the janitor. The nil check on every method
// below covers the window before this, and a Lifecycle built by hand in tests.
func (l *Lifecycle) setLeases(ls *leases) {
	l.mu.Lock()
	l.leases = ls
	l.mu.Unlock()
}

// reapLeases is what the janitor calls. It reads the reaper under l.mu because
// the Server installs it after the janitor is already running.
func (l *Lifecycle) reapLeases() {
	l.mu.Lock()
	ls := l.leases
	l.mu.Unlock()
	ls.sweep()
}

func (ls *leases) warning() time.Duration {
	if d := ls.s.opts.LeaseWarning; d > 0 {
		return d
	}
	return defaultLeaseWarning
}

// leaseExpired is the whole rule. The reaper applies it twice, once on a stale
// record and once on a fresh one under the sprite's lock, so protection and
// expiry are tested together here rather than by each caller.
func leaseExpired(sp store.Sprite, now time.Time) bool {
	return sp.ExpiresAt != nil && !sp.Protected && !now.Before(*sp.ExpiresAt)
}

// sweep deletes what has expired and warns about what is about to. The janitor
// runs it every 30 s, and the Server once at startup: a lease that ran out
// while the daemon was down is no different from one that ran out while it was
// up, and the sprite should not survive the restart.
func (ls *leases) sweep() {
	if ls == nil {
		return
	}
	now := time.Now()
	for _, sp := range ls.s.store.List("") {
		if leaseExpired(sp, now) {
			ls.reap(sp)
			continue
		}
		ls.warn(sp, now)
	}
}

// warn publishes sprite.expiring once per deadline. A renewal or a protection
// clears the mark, so the next deadline is warned about in its own right.
func (ls *leases) warn(sp store.Sprite, now time.Time) {
	if sp.ExpiresAt == nil || sp.Protected || now.Add(ls.warning()).Before(*sp.ExpiresAt) {
		ls.forget(sp.ID)
		return
	}
	ls.mu.Lock()
	first := !ls.warned[sp.ID].Equal(*sp.ExpiresAt)
	ls.warned[sp.ID] = *sp.ExpiresAt
	ls.mu.Unlock()
	if !first {
		return
	}
	ls.s.log.Info("sprite lease running out", "sprite", sp.Name, "expires_at", sp.ExpiresAt)
	ls.s.life.emit(sp, "sprite.expiring", map[string]any{
		"expires_at": sp.ExpiresAt.UTC().Format(time.RFC3339), "in_ms": sp.ExpiresAt.Sub(now).Milliseconds()})
}

// reap deletes one expired sprite. sp is a candidate from a list read before
// any lock, so the decision is taken again on a fresh record under the sprite's
// lifecycle lock, the one every transition holds: a renewal that got there
// first is seen, and its sprite is left alone. Committing marks the sprite, and
// a lease change arriving after that is refused instead of writing to a record
// on its way out. Either way the sprite is leased for longer or deleted whole,
// never half of each.
//
// The lock is dropped before the delete, because the delete path stops the VM
// and takes that same lock. What makes the gap safe is the mark, not the lock.
func (ls *leases) reap(sp store.Sprite) {
	rt := ls.s.life.rt(sp.ID)
	rt.mu.Lock()
	cur, err := ls.s.store.Get(sp.Name)
	commit := err == nil && cur.ID == sp.ID && leaseExpired(cur, time.Now())
	if commit {
		ls.claim(cur.ID)
	}
	rt.mu.Unlock()
	if !commit {
		return
	}
	defer ls.release(cur.ID)
	ls.s.log.Info("lease expired; deleting sprite", "sprite", cur.Name, "id", cur.ID, "expires_at", cur.ExpiresAt)
	// Before the delete, so a follower sees why the sprite.deleted that comes
	// next was not somebody's DELETE.
	ls.s.life.emit(cur, "sprite.expired", map[string]any{"expires_at": cur.ExpiresAt.UTC().Format(time.RFC3339)})
	if err := ls.s.destroy(cur); err != nil {
		ls.s.log.Error("deleting an expired sprite failed; it will be tried again", "sprite", cur.Name, "err", err)
		return
	}
	ls.forget(cur.ID)
}

func (ls *leases) claim(id string) {
	ls.mu.Lock()
	ls.reaping[id] = true
	ls.mu.Unlock()
}

func (ls *leases) release(id string) {
	ls.mu.Lock()
	delete(ls.reaping, id)
	ls.mu.Unlock()
}

// claimed reports whether a reap has committed to deleting this sprite.
func (ls *leases) claimed(id string) bool {
	if ls == nil {
		return false
	}
	ls.mu.Lock()
	defer ls.mu.Unlock()
	return ls.reaping[id]
}

// forget drops the expiring warning already sent, so a new deadline earns one.
func (ls *leases) forget(id string) {
	if ls == nil {
		return
	}
	ls.mu.Lock()
	delete(ls.warned, id)
	ls.mu.Unlock()
}

// leaseRequest is the lease in a create, an update or a renewal. There are two
// ways to name the moment because callers differ: an operator has a date in
// mind, a lobby handing out a sprite knows only how long it should live. A
// field left out changes nothing, so a caller that knows nothing of leases
// cannot clear one by accident; expires_at "" or ttl_seconds 0 clears it.
type leaseRequest struct {
	ExpiresAt  *string `json:"expires_at"`
	TTLSeconds *int64  `json:"ttl_seconds"`
	Protected  *bool   `json:"protected"`
}

func (q leaseRequest) touchesExpiry() bool { return q.ExpiresAt != nil || q.TTLSeconds != nil }

// expiry resolves the deadline asked for: nil with an empty message means no
// lease, and a non-empty message is what the caller returns as a 400.
func (q leaseRequest) expiry(now time.Time) (*time.Time, string) {
	switch {
	case q.ExpiresAt != nil && q.TTLSeconds != nil:
		return nil, "give either expires_at or ttl_seconds, not both"
	case q.TTLSeconds != nil:
		if *q.TTLSeconds < 0 {
			return nil, "ttl_seconds must not be negative"
		}
		if *q.TTLSeconds == 0 {
			return nil, ""
		}
		t := now.Add(time.Duration(*q.TTLSeconds) * time.Second).UTC()
		return &t, ""
	case q.ExpiresAt != nil:
		if *q.ExpiresAt == "" {
			return nil, ""
		}
		t, err := time.Parse(time.RFC3339, *q.ExpiresAt)
		if err != nil {
			return nil, "expires_at must be an RFC3339 time, e.g. 2026-09-23T10:00:00Z"
		}
		if t.Before(now) {
			// A deadline already past would be reaped within the half minute,
			// which is a lot of deletion for a mistyped year.
			return nil, "expires_at is in the past"
		}
		t = t.UTC()
		return &t, ""
	}
	return nil, ""
}

// apply writes the lease onto a record that is not in the store yet (a create,
// where there is nothing to race with).
func (q leaseRequest) apply(sp *store.Sprite, now time.Time) string {
	exp, msg := q.expiry(now)
	if msg != "" {
		return msg
	}
	if q.touchesExpiry() {
		sp.ExpiresAt = exp
	}
	if q.Protected != nil {
		sp.Protected = *q.Protected
	}
	return ""
}

// leaseJSON is the lease on its own. ExpiresAt is explicitly null rather than
// absent when there is none, so a client can tell "no lease" from an old daemon.
type leaseJSON struct {
	ExpiresAt *time.Time `json:"expires_at"`
	Protected bool       `json:"protected"`
	// ExpiresIn saves every caller the clock comparison, and is negative for a
	// deadline that has passed on a protected sprite.
	ExpiresIn *int64 `json:"expires_in_seconds,omitempty"`
}

func leaseOf(sp store.Sprite) leaseJSON {
	out := leaseJSON{ExpiresAt: sp.ExpiresAt, Protected: sp.Protected}
	if sp.ExpiresAt != nil {
		in := int64(time.Until(*sp.ExpiresAt).Round(time.Second) / time.Second)
		out.ExpiresIn = &in
	}
	return out
}

func (s *Server) registerLeases(mux *http.ServeMux) {
	mux.HandleFunc("GET "+leasePath, func(w http.ResponseWriter, r *http.Request) {
		if sp, ok := s.lookup(w, r); ok {
			writeJSON(w, http.StatusOK, leaseOf(sp))
		}
	})
	mux.HandleFunc("POST "+leasePath, func(w http.ResponseWriter, r *http.Request) {
		var req leaseRequest
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil {
			writeErr(w, http.StatusBadRequest, "bad_request", "invalid JSON body")
			return
		}
		if sp, ok := s.applyLease(w, r, req); ok {
			writeJSON(w, http.StatusOK, leaseOf(sp))
		}
	})
	// Dropping the lease drops the protection with it: protection only ever
	// meant "not this deadline", and leaving it set on a sprite with no deadline
	// would read as a promise nothing enforces.
	mux.HandleFunc("DELETE "+leasePath, func(w http.ResponseWriter, r *http.Request) {
		none, off := "", false
		if _, ok := s.applyLease(w, r, leaseRequest{ExpiresAt: &none, Protected: &off}); ok {
			w.WriteHeader(http.StatusNoContent)
		}
	})
}

// applyLease is every lease change from outside: the renewal endpoint and the
// lease fields of PUT /v1/sprites/{name}. It writes under the sprite's
// lifecycle lock, which is what serializes it against a reap in flight; see
// reap for the two orders and their outcomes.
func (s *Server) applyLease(w http.ResponseWriter, r *http.Request, req leaseRequest) (store.Sprite, bool) {
	sp, ok := s.lookup(w, r)
	if !ok {
		return sp, false
	}
	exp, msg := req.expiry(time.Now())
	if msg != "" {
		writeErr(w, http.StatusBadRequest, "bad_request", msg)
		return sp, false
	}
	rt := s.life.rt(sp.ID)
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if s.leases.claimed(sp.ID) {
		writeErr(w, http.StatusConflict, "expired",
			"this sprite's lease ran out and it is being deleted; create a new sprite")
		return sp, false
	}
	cur, err := s.store.Update(sp.Name, func(sp *store.Sprite) {
		if req.touchesExpiry() {
			sp.ExpiresAt = exp
		}
		if req.Protected != nil {
			sp.Protected = *req.Protected
		}
		sp.UpdatedAt = time.Now().UTC()
	})
	if err != nil {
		writeErr(w, http.StatusNotFound, "not_found", "sprite not found")
		return sp, false
	}
	s.leases.forget(cur.ID)
	s.log.Info("lease set", "sprite", cur.Name, "expires_at", cur.ExpiresAt, "protected", cur.Protected)
	// forget cleared the mark for the old deadline; warn re-earns it for the new
	// one straight away, because a lease set to less than --lease-warning (or to
	// less than a janitor tick) would otherwise expire unannounced.
	s.leases.warn(cur, time.Now())
	return cur, true
}
