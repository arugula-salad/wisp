package server

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/arugula-salad/wisp/internal/store"
	"github.com/arugula-salad/wisp/internal/vmm"
)

// A sandbox's lifecycle rules (store.LifecyclePolicy): the idle rule, which
// watch applies to a running sandbox, and the deadline rule, which the lease
// sweep applies (leases.go). With no policy they are what every sprite has
// always had: suspend warm after --idle-timeout, delete at the lease.
//
// A front end sets them here, and these methods take the sandbox's lock
// themselves, so they are called holding none (see the order in lifecycle.go).

// errBadPolicy is a lifecycle policy with an action or timeout the engine does
// not know.
var errBadPolicy = errors.New("unknown lifecycle policy action, or a negative idle timeout")

// Deadline is a sandbox's deadline rule: when At passes, unless Protected,
// Action is taken. At nil is no deadline. It is stored as the Sprites lease
// (Record.ExpiresAt and Protected) and the policy's DeadlineAction.
type Deadline struct {
	At        *time.Time
	Protected bool
	Action    store.DeadlineAction
}

// LifecyclePolicyOf is rec's policy, or the zero policy (the defaults) when
// it has none. It is a copy.
func LifecyclePolicyOf(rec store.Record) store.LifecyclePolicy {
	if rec.Lifecycle == nil {
		return store.LifecyclePolicy{}
	}
	return *rec.Lifecycle
}

// normalPolicy is p as it is stored: defaults spelt as zero values, and no
// policy at all when that leaves nothing, so a sprite.json that never had a
// policy does not grow one from a change that changed nothing.
func normalPolicy(p store.LifecyclePolicy) *store.LifecyclePolicy {
	if p.IdleAction == store.IdleSuspend {
		p.IdleAction = ""
	}
	if p.DeadlineAction == store.DeadlineDelete {
		p.DeadlineAction = ""
	}
	if p == (store.LifecyclePolicy{}) {
		return nil
	}
	return &p
}

// SetPolicy replaces a sandbox's lifecycle policy, the deadline action
// included (the deadline itself is SetDeadline's). It takes effect at once: a
// running sandbox's idle watcher reads it on its next tick, and the deadline
// sweep on its next pass. A sandbox whose lease a reap has committed to is
// errLeaseReaping; one that is gone store.ErrNotFound.
func (l *Lifecycle) SetPolicy(id string, p store.LifecyclePolicy) (store.Record, error) {
	if !p.Valid() {
		return store.Record{}, errBadPolicy
	}
	var cur store.Record
	err := l.WithLocked(id, func() error {
		if l.leases.claimed(id) {
			return errLeaseReaping
		}
		var err error
		if cur, err = l.store.UpdateRecord(id, func(r *store.Record) { r.Lifecycle = normalPolicy(p) }); err != nil {
			return err
		}
		// The deadline's action may have changed, and with it whether it is warned about.
		l.leases.rearm(cur, "lifecycle policy set")
		return nil
	})
	return cur, err
}

// SetDeadline gives a sandbox a deadline, at which action is taken; at nil
// clears it, and an empty action leaves the action as it was. Setting it
// again is the extension: E2B's set-timeout, a lease renewal. Protection is
// left as it is. Errors are SetPolicy's, and errBadPolicy for an unknown action.
func (l *Lifecycle) SetDeadline(id string, at *time.Time, action store.DeadlineAction) (store.Record, error) {
	return l.ChangeDeadline(id, func(d *Deadline) {
		d.At = at
		if action != "" {
			d.Action = action
		}
	})
}

// ChangeDeadline is SetDeadline for a caller that changes part of the
// deadline: change sees the current one, under the sandbox's lock, and edits
// it in place. The Sprites lease endpoints are this.
func (l *Lifecycle) ChangeDeadline(id string, change func(*Deadline)) (store.Record, error) {
	return l.leases.set(id, change)
}

// Suspend suspends a running sandbox warm on demand, as the idle rule would
// but without the guest's veto (E2B's pause). One that is not running is left
// as it is. Acquire resumes it, from the snapshot.
func (l *Lifecycle) Suspend(sp store.Record) error { return l.Stop(sp, true) }

// idleRule is sp's idle timeout and action now. sp is the record a watcher
// started with: a sandbox Create holds outside the store is ruled by that.
func (l *Lifecycle) idleRule(sp store.Record) (time.Duration, store.IdleAction) {
	if cur, err := l.store.GetRecord(sp.ID); err == nil {
		sp = cur
	}
	return sp.Lifecycle.Idle(l.opts.IdleTimeout)
}

// stopLocked stops a running sandbox cold, keeping its disk: the guest syncs
// its filesystems first, which an idle stop lets it veto (errGuestBusy) the
// way an idle suspend does. A stop for any other reason goes ahead even if
// the guest does not answer, as Stop(keepWarm=false) does.
func (l *Lifecycle) stopLocked(sp store.Record, rt *runtime, idle bool, reason string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	path := "/internal/presuspend"
	if idle {
		path += "?idle=1"
	}
	synced := true
	if err := agentCall(ctx, rt.m, http.MethodPost, path, nil, nil); err != nil {
		if idle {
			return err
		}
		synced = false
		l.log.Warn("guest did not sync before a stop; stopping it anyway", "sprite", l.label(sp), "err", err)
	}
	rt.m.Kill()
	l.cleanupLocked(rt)
	vmm.DiscardSnapshot(l.store.Dir(sp.ID))
	l.log.Info("sprite stopped", "sprite", l.label(sp), "reason", reason)
	l.emit(sp, "sprite.stopped", map[string]any{"reason": reason})
	// Synced and stopped: the disk is as quiescent as after a suspend. One the
	// guest did not sync is left to the periodic backup, which does not take it
	// for a clean one.
	if synced {
		l.backups.Enqueue(sp.ID, "suspend")
	}
	return nil
}

// passDeadline takes a deadline action other than delete (which is the
// lease's reap) on a sandbox whose deadline has passed: stop it cold, or
// suspend it warm, whatever it is doing. The deadline is then spent and
// cleared, so the sandbox can be woken again without being stopped again 30 s
// later; a front end that wants another sets one (E2B resets the timeout on
// resume). sp is a candidate from a list read before any lock, so the
// decision is taken again under the lock on a fresh record, as reap does.
// A suspend that fails becomes a cold stop, so the deadline is always spent.
func (l *Lifecycle) passDeadline(sp store.Record) {
	rt := l.rt(sp.ID)
	rt.mu.Lock()
	defer rt.mu.Unlock()
	cur, err := l.store.GetRecord(sp.ID)
	if err != nil || !leaseExpired(cur, time.Now()) {
		return
	}
	dir := l.store.Dir(cur.ID)
	switch action := cur.Lifecycle.OnDeadline(); action {
	case store.DeadlineSuspend:
		if rt.m != nil {
			if err := l.suspendLocked(cur, rt, suspendDeadline); err != nil {
				// A deadline acts whatever the guest is doing: a sandbox that cannot
				// be suspended (its agent wedged, say) is stopped cold instead, as
				// Shutdown does, rather than running past its deadline forever.
				l.log.Warn("suspend at the deadline failed; stopping it cold instead", "sprite", l.label(cur), "err", err)
				if rt.m != nil {
					l.stopLocked(cur, rt, false, "deadline")
				}
			}
		}
	case store.DeadlineStop:
		if rt.m != nil {
			l.stopLocked(cur, rt, false, "deadline")
		} else if vmm.HasSnapshot(dir) {
			vmm.DiscardSnapshot(dir)
			l.emit(cur, "sprite.cold", map[string]any{"reason": "deadline"})
		}
	default:
		return // delete, set while this waited: the reaper's, on its next pass
	}
	l.log.Info("deadline passed", "sprite", l.label(cur), "expires_at", cur.ExpiresAt, "action", cur.Lifecycle.OnDeadline())
	l.store.UpdateRecord(cur.ID, func(r *store.Record) { r.ExpiresAt = nil })
	l.leases.forget(cur.ID)
}
