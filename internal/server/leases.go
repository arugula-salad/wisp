package server

import (
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/arugula-salad/wisp/internal/store"
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
// That is the lease, and a sprite's deadline is always one. The deadline is
// the engine's, though, and other front ends want something else to happen at
// it: a sandbox whose lifecycle policy (store.LifecyclePolicy) names another
// DeadlineAction is stopped or suspended instead (passDeadline), which spends
// the deadline rather than the sandbox. Everything delete-specific here, the
// sprite.expiring warning, the reaping claim and sprite.expired, is the lease
// alone.

// defaultLeaseWarning is how long before expiry sprite.expiring goes out when
// the operator has set no Options.LeaseWarning.
const defaultLeaseWarning = 5 * time.Minute

// leases is the reaper and the bookkeeping the reaper needs.
type leases struct {
	store   *store.Store
	log     *slog.Logger
	life    *Lifecycle
	warnFor time.Duration // Options.LeaseWarning; 0 is defaultLeaseWarning

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

func newLeases(st *store.Store, log *slog.Logger, life *Lifecycle, warning time.Duration) *leases {
	return &leases{store: st, log: log, life: life, warnFor: warning,
		warned: map[string]time.Time{}, reaping: map[string]bool{}}
}

// StartReaping sweeps the leases once, now, and lets the janitor sweep them
// from then on. The front end calls it once it is attached (its OnDelete hook,
// the webhooks), so that what the reaper deletes, including what ran out while
// the daemon was down, reaches it like any other deletion.
func (l *Lifecycle) StartReaping() {
	l.mu.Lock()
	l.reapStarted = true
	l.mu.Unlock()
	l.leases.sweep()
}

// reapLeases is what the janitor calls.
func (l *Lifecycle) reapLeases() {
	l.mu.Lock()
	on := l.reapStarted
	l.mu.Unlock()
	if on {
		l.leases.sweep()
	}
}

func (ls *leases) warning() time.Duration {
	if d := ls.warnFor; d > 0 {
		return d
	}
	return defaultLeaseWarning
}

// leaseExpired is the whole rule, whatever the deadline's action. The reaper applies it twice, once on a stale
// record and once on a fresh one under the sprite's lock, so protection and
// expiry are tested together here rather than by each caller.
func leaseExpired(sp store.Record, now time.Time) bool {
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
	for _, sp := range ls.store.Records() {
		if leaseExpired(sp, now) {
			if sp.Lifecycle.OnDeadline() == store.DeadlineDelete {
				ls.reap(sp)
			} else {
				ls.life.passDeadline(sp)
			}
			continue
		}
		ls.warn(sp, now)
	}
}

// warn publishes sprite.expiring once per deadline. A renewal or a protection
// clears the mark, so the next deadline is warned about in its own right. Only
// a lease is warned about: a deadline that stops or suspends loses nothing.
func (ls *leases) warn(sp store.Record, now time.Time) {
	if ls == nil {
		return
	}
	if sp.ExpiresAt == nil || sp.Protected || sp.Lifecycle.OnDeadline() != store.DeadlineDelete ||
		now.Add(ls.warning()).Before(*sp.ExpiresAt) {
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
	ls.log.Info("sprite lease running out", "sprite", ls.life.label(sp), "expires_at", sp.ExpiresAt)
	ls.life.emit(sp, "sprite.expiring", map[string]any{
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
func (ls *leases) reap(sp store.Record) {
	var cur store.Record
	commit := false
	ls.life.WithLocked(sp.ID, func() error {
		var err error
		cur, err = ls.store.GetRecord(sp.ID)
		commit = err == nil && leaseExpired(cur, time.Now()) && cur.Lifecycle.OnDeadline() == store.DeadlineDelete
		if commit {
			ls.claim(cur.ID)
		}
		return nil
	})
	if !commit {
		return
	}
	defer ls.release(cur.ID)
	ls.log.Info("lease expired; deleting sprite", "sprite", ls.life.label(cur), "id", cur.ID, "expires_at", cur.ExpiresAt)
	// Before the delete, so a follower sees why the sprite.deleted that comes
	// next was not somebody's DELETE.
	ls.life.emit(cur, "sprite.expired", map[string]any{"expires_at": cur.ExpiresAt.UTC().Format(time.RFC3339)})
	// Lifecycle.Delete, the same deletion a DELETE is, so expiry frees exactly
	// what a DELETE frees, and forgets the warning sent.
	if err := ls.life.Delete(cur); err != nil {
		ls.log.Error("deleting an expired sprite failed; it will be tried again", "sprite", ls.life.label(cur), "err", err)
	}
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

// set changes a sandbox's deadline from outside (Lifecycle.ChangeDeadline):
// change is applied under the sandbox's lifecycle lock, which is what
// serializes it against a reap in flight; see reap for the two orders and
// their outcomes. A sandbox a reap has committed to is errLeaseReaping, one
// that is gone store.ErrNotFound, and an unknown action errBadPolicy.
func (ls *leases) set(id string, change func(*Deadline)) (store.Record, error) {
	if ls == nil {
		return store.Record{}, errors.New("no lease reaper")
	}
	var cur store.Record
	err := ls.life.WithLocked(id, func() error {
		if ls.claimed(id) {
			return errLeaseReaping
		}
		old, err := ls.store.GetRecord(id)
		if err != nil {
			return err
		}
		d := Deadline{At: old.ExpiresAt, Protected: old.Protected, Action: old.Lifecycle.OnDeadline()}
		change(&d)
		p := LifecyclePolicyOf(old)
		p.DeadlineAction = d.Action
		if !p.Valid() {
			return errBadPolicy
		}
		if cur, err = ls.store.UpdateRecord(id, func(sp *store.Record) {
			sp.ExpiresAt, sp.Protected = d.At, d.Protected
			sp.Lifecycle = normalPolicy(p)
			sp.UpdatedAt = time.Now().UTC()
		}); err != nil {
			return err
		}
		ls.rearm(cur, "deadline set")
		return nil
	})
	return cur, err
}

// rearm is what follows a change to a deadline or its action: the warning
// already sent was about the old one, and the new one may be inside the
// warning window already.
func (ls *leases) rearm(cur store.Record, what string) {
	if ls == nil {
		return
	}
	ls.forget(cur.ID)
	ls.log.Info(what, "sprite", ls.life.label(cur), "expires_at", cur.ExpiresAt, "protected", cur.Protected,
		"action", cur.Lifecycle.OnDeadline())
	// forget cleared the mark for the old deadline; warn re-earns it for the new
	// one straight away, because a lease set to less than --lease-warning (or to
	// less than a janitor tick) would otherwise expire unannounced.
	ls.warn(cur, time.Now())
}

// errLeaseReaping is a lease change that lost the race with a reap.
var errLeaseReaping = errors.New("the sprite's lease ran out and it is being deleted")
