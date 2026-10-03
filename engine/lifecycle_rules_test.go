package engine

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/arugula-salad/wisp/internal/store"
	"github.com/arugula-salad/wisp/internal/vmm"
)

// The lifecycle rules on sandboxes that are not running; lifecycle_rules_vm_test.go
// has the running ones, which need a VM.

// ruleEvents follows every event a deadline or idle rule can lead to.
func ruleEvents(l *Engine) *Subscription {
	sub, _, _, _ := l.Events().Subscribe(func(e Event) bool {
		switch e.Type {
		case "sprite.expiring", "sprite.expired", "sprite.deleted", "sprite.suspended", "sprite.stopped", "sprite.cold":
			return true
		}
		return false
	}, 0, false)
	return sub
}

// leaseEvents follows the lease's events.
func leaseEvents(l *Engine) *Subscription {
	sub, _, _, _ := l.Events().Subscribe(func(e Event) bool {
		return e.Type == "sprite.expiring" || e.Type == "sprite.expired" || e.Type == "sprite.deleted"
	}, 0, false)
	return sub
}

// expire backdates a sprite's deadline, which no API call will do (a deadline
// in the past is refused as a typo).
func expire(t *testing.T, l *Engine, name string, at time.Time) store.Sprite {
	t.Helper()
	sp, err := l.store.UpdateByName(store.Sprites, name, func(sp *store.Sprite) { sp.ExpiresAt = &at })
	if err != nil {
		t.Fatal(err)
	}
	return sp
}

// restart is a second engine on the same data directory, as a wispd stopped
// and started again.
func restart(t *testing.T, l *Engine) *Engine {
	t.Helper()
	st, err := store.Open(l.opts.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	again := New(l.opts, st, quiet)
	again.SetDescriber(spriteName)
	again.StartReaping()
	return again
}

func eventTypes(es []Event) string {
	var out []string
	for _, e := range es {
		out = append(out, e.Type)
	}
	return strings.Join(out, ",")
}

// withPolicy gives a stored sprite a lifecycle policy through the
func withPolicy(t *testing.T, l *Engine, id string, p store.LifecyclePolicy) {
	t.Helper()
	if _, err := l.SetPolicy(id, p); err != nil {
		t.Fatal(err)
	}
}

func record(t *testing.T, l *Engine, id string) store.Record {
	t.Helper()
	r, err := l.store.GetRecord(id)
	if err != nil {
		t.Fatalf("record %s: %v", id, err)
	}
	return r
}

// No policy is today's rule and nothing else: the idle timeout is the
// daemon's and the action a warm suspend. A policy changes it for that
// sandbox alone, and is read afresh, so a change reaches a running watcher.
func TestIdleRuleIsThePolicysOrTheDaemons(t *testing.T) {
	l := newTestEngine(t, Options{IdleTimeout: 30 * time.Second})
	plain := warmSprite(t, l, "plain", time.Now())
	ruled := warmSprite(t, l, "ruled", time.Now())
	for _, tc := range []struct {
		p       *store.LifecyclePolicy
		timeout time.Duration
		action  store.IdleAction
	}{
		{nil, 30 * time.Second, store.IdleSuspend},
		{&store.LifecyclePolicy{IdleAction: store.IdleStop}, 30 * time.Second, store.IdleStop},
		{&store.LifecyclePolicy{IdleTimeout: 2 * time.Minute, IdleAction: store.IdleNone}, 2 * time.Minute, store.IdleNone},
		{&store.LifecyclePolicy{IdleTimeout: time.Second}, time.Second, store.IdleSuspend},
	} {
		if tc.p != nil {
			withPolicy(t, l, ruled.ID, *tc.p)
		}
		// The watcher holds the record it booted with; the rule comes from the store.
		if d, a := l.idleRule(ruled.Record); d != tc.timeout || a != tc.action {
			t.Errorf("policy %+v: idle rule %v %q, want %v %q", tc.p, d, a, tc.timeout, tc.action)
		}
		if d, a := l.idleRule(plain.Record); d != 30*time.Second || a != store.IdleSuspend {
			t.Errorf("a sprite without a policy got %v %q", d, a)
		}
	}
	// A sandbox Create holds outside the store is ruled by the record it has.
	unstored := store.Record{ID: "notstored", Lifecycle: &store.LifecyclePolicy{IdleAction: store.IdleNone}}
	if _, a := l.idleRule(unstored); a != store.IdleNone {
		t.Errorf("unstored record's idle action %q", a)
	}
}

// A deadline whose action is stop or suspend spends the deadline, not the
// sandbox: the record and its disk stay, the deadline is cleared, and none of
// the lease's delete events goes out, before or at the deadline.
func TestDeadlineActionsOnASandboxThatIsNotRunning(t *testing.T) {
	for _, tc := range []struct {
		action   store.DeadlineAction
		warm     bool
		wantWarm bool
		events   string
	}{
		{store.DeadlineStop, true, false, "sprite.cold"}, // a warm sandbox loses its memory state
		{store.DeadlineStop, false, false, ""},
		{store.DeadlineSuspend, true, true, ""}, // already suspended: nothing to do
		{store.DeadlineSuspend, false, false, ""},
	} {
		t.Run(string(tc.action), func(t *testing.T) {
			l := newTestEngine(t, Options{})
			sp := warmSprite(t, l, "sbx", time.Now())
			if !tc.warm {
				vmm.DiscardSnapshot(l.store.Dir(sp.ID))
			}
			disk := filepath.Join(l.store.Dir(sp.ID), vmm.DiskFile)
			os.WriteFile(disk, []byte("disk"), 0o644)
			withPolicy(t, l, sp.ID, store.LifecyclePolicy{DeadlineAction: tc.action})
			// Inside the warning window: a lease would be warned about now.
			soon := time.Now().Add(time.Minute)
			if _, err := l.SetDeadline(sp.ID, &soon, ""); err != nil {
				t.Fatal(err)
			}
			sub := ruleEvents(l)
			l.leases.sweep()
			if got := collect(sub); len(got) != 0 {
				t.Fatalf("before the deadline: events %s, want none", eventTypes(got))
			}

			expire(t, l, sp.Name, time.Now().Add(-time.Second))
			l.leases.sweep()
			got := collect(sub)
			if eventTypes(got) != tc.events {
				t.Errorf("events %q, want %q", eventTypes(got), tc.events)
			}
			if tc.events != "" && got[0].Detail["reason"] != "deadline" {
				t.Errorf("%s detail %v, want reason deadline", got[0].Type, got[0].Detail)
			}
			r := record(t, l, sp.ID)
			if r.ExpiresAt != nil {
				t.Errorf("the deadline was not spent: %v", r.ExpiresAt)
			}
			if r.Lifecycle.OnDeadline() != tc.action {
				t.Errorf("the deadline action changed to %q", r.Lifecycle.OnDeadline())
			}
			if _, err := os.Stat(disk); err != nil {
				t.Errorf("the disk went with the deadline: %v", err)
			}
			if vmm.HasSnapshot(l.store.Dir(sp.ID)) != tc.wantWarm {
				t.Errorf("warm = %v, want %v", !tc.wantWarm, tc.wantWarm)
			}
			// Spent: another pass does nothing more.
			l.leases.sweep()
			if got := collect(sub); len(got) != 0 {
				t.Errorf("a spent deadline acted again: %s", eventTypes(got))
			}
		})
	}
}

// Protection holds off every deadline action, not just the delete; and the
// deadline stays where it was, as a lease's does.
func TestProtectionHoldsOffEveryDeadlineAction(t *testing.T) {
	for _, action := range []store.DeadlineAction{store.DeadlineDelete, store.DeadlineStop, store.DeadlineSuspend} {
		t.Run(string(action), func(t *testing.T) {
			l := newTestEngine(t, Options{})
			sp := warmSprite(t, l, "sbx", time.Now())
			withPolicy(t, l, sp.ID, store.LifecyclePolicy{DeadlineAction: action})
			if _, err := l.ChangeDeadline(sp.ID, func(d *Deadline) { d.Protected = true }); err != nil {
				t.Fatal(err)
			}
			past := time.Now().Add(-time.Minute)
			expire(t, l, sp.Name, past)
			sub := ruleEvents(l)
			l.leases.sweep()
			if got := collect(sub); len(got) != 0 {
				t.Errorf("a protected sandbox got %s", eventTypes(got))
			}
			r := record(t, l, sp.ID)
			if r.ExpiresAt == nil || !r.ExpiresAt.Equal(past) || !vmm.HasSnapshot(l.store.Dir(sp.ID)) {
				t.Errorf("protected sandbox changed: %+v", r)
			}
			// Unprotected, the deadline is acted on at once.
			l.ChangeDeadline(sp.ID, func(d *Deadline) { d.Protected = false })
			l.leases.sweep()
			_, err := l.store.GetRecord(sp.ID)
			if deleted := errors.Is(err, store.ErrNotFound); deleted != (action == store.DeadlineDelete) {
				t.Errorf("deleted = %v once unprotected", deleted)
			}
		})
	}
}

// Extending a deadline is setting it again: the action and the protection
// stay as they were, and a deadline in the future is not acted on.
func TestExtendingADeadline(t *testing.T) {
	l := newTestEngine(t, Options{})
	sp := warmSprite(t, l, "sbx", time.Now())
	soon := time.Now().Add(time.Second)
	if _, err := l.SetDeadline(sp.ID, &soon, store.DeadlineSuspend); err != nil {
		t.Fatal(err)
	}
	if r := record(t, l, sp.ID); r.Lifecycle.OnDeadline() != store.DeadlineSuspend || r.ExpiresAt == nil {
		t.Fatalf("after SetDeadline: %+v", r)
	}
	later := time.Now().Add(time.Hour)
	if _, err := l.SetDeadline(sp.ID, &later, ""); err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Until(soon) + 10*time.Millisecond)
	l.leases.sweep()
	r := record(t, l, sp.ID)
	if r.ExpiresAt == nil || !r.ExpiresAt.Equal(later) || r.Lifecycle.OnDeadline() != store.DeadlineSuspend {
		t.Fatalf("after an extension and a sweep past the first deadline: %+v", r)
	}
	if _, err := l.SetDeadline(sp.ID, &later, "explode"); !errors.Is(err, errBadPolicy) {
		t.Errorf("unknown action: %v", err)
	}
	// Clearing it leaves the action for the next one.
	if _, err := l.SetDeadline(sp.ID, nil, ""); err != nil {
		t.Fatal(err)
	}
	if r := record(t, l, sp.ID); r.ExpiresAt != nil || r.Lifecycle.OnDeadline() != store.DeadlineSuspend {
		t.Errorf("after clearing: %+v", r)
	}
}

// A policy of defaults is no policy: it is not written, so a sprite.json
// keeps its bytes. One the engine does not know is refused.
func TestSetPolicyStoresOnlyWhatDiffersFromTheDefaults(t *testing.T) {
	l := newTestEngine(t, Options{})
	sp := warmSprite(t, l, "sbx", time.Now())
	file := filepath.Join(l.store.Dir(sp.ID), "sprite.json")
	for _, p := range []store.LifecyclePolicy{{}, {IdleAction: store.IdleSuspend, DeadlineAction: store.DeadlineDelete}} {
		if r, err := l.SetPolicy(sp.ID, p); err != nil || r.Lifecycle != nil {
			t.Fatalf("SetPolicy(%+v) = %+v, %v; want no policy", p, r.Lifecycle, err)
		}
		if b, _ := os.ReadFile(file); strings.Contains(string(b), "lifecycle") {
			t.Fatalf("defaults written to disk: %s", b)
		}
	}
	for _, p := range []store.LifecyclePolicy{{IdleAction: "nap"}, {DeadlineAction: "pause"}, {IdleTimeout: -time.Second}} {
		if _, err := l.SetPolicy(sp.ID, p); !errors.Is(err, errBadPolicy) {
			t.Errorf("SetPolicy(%+v) = %v, want errBadPolicy", p, err)
		}
	}
	if _, err := l.SetPolicy("nosuch", store.LifecyclePolicy{}); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("SetPolicy on a missing sandbox: %v", err)
	}
	// And the policy survives a restart.
	withPolicy(t, l, sp.ID, store.LifecyclePolicy{IdleTimeout: time.Minute, IdleAction: store.IdleStop})
	again := restart(t, l)
	if r := record(t, again, sp.ID); r.Lifecycle == nil || r.Lifecycle.IdleTimeout != time.Minute || r.Lifecycle.IdleAction != store.IdleStop {
		t.Errorf("after a restart: %+v", r.Lifecycle)
	}
}

// The sprite.expiring warning is the lease's: it goes out for a delete
// deadline and for no other. Changing the action re-arms it either way.
func TestOnlyADeleteDeadlineIsWarnedAbout(t *testing.T) {
	l := newTestEngine(t, Options{})
	sp := warmSprite(t, l, "sbx", time.Now())
	withPolicy(t, l, sp.ID, store.LifecyclePolicy{DeadlineAction: store.DeadlineStop})
	sub := leaseEvents(l)
	soon := time.Now().Add(time.Minute)
	l.SetDeadline(sp.ID, &soon, "")
	l.leases.sweep()
	if got := collect(sub); len(got) != 0 {
		t.Fatalf("a stop deadline was warned about: %s", eventTypes(got))
	}
	withPolicy(t, l, sp.ID, store.LifecyclePolicy{}) // back to a lease
	if got := collect(sub); eventTypes(got) != "sprite.expiring" {
		t.Fatalf("a delete deadline inside the window: %q, want one sprite.expiring", eventTypes(got))
	}
}

// A reap decides on a stale record and commits under the lock on a fresh
// one: a policy that changed the deadline's action in between wins, and the
// sandbox is stopped instead of deleted on the next pass. And a policy change
// that arrives once a reap has committed is refused, as a renewal is.
func TestAPolicyChangeRacesTheReaperLikeARenewal(t *testing.T) {
	l := newTestEngine(t, Options{})
	sp := warmSprite(t, l, "sbx", time.Now())
	stale := expire(t, l, sp.Name, time.Now().Add(-time.Second))
	withPolicy(t, l, sp.ID, store.LifecyclePolicy{DeadlineAction: store.DeadlineStop})
	l.leases.reap(stale.Record)
	if _, err := l.store.GetRecord(sp.ID); err != nil {
		t.Fatal("the reaper deleted a sandbox whose deadline now stops it")
	}
	l.leases.sweep()
	if r := record(t, l, sp.ID); r.ExpiresAt != nil || vmm.HasSnapshot(l.store.Dir(sp.ID)) {
		t.Errorf("the stop deadline was not taken: %+v", r)
	}

	l.leases.claim(sp.ID)
	defer l.leases.release(sp.ID)
	if _, err := l.SetPolicy(sp.ID, store.LifecyclePolicy{}); !errors.Is(err, ErrLeaseReaping) {
		t.Errorf("SetPolicy during a committed reap: %v", err)
	}
	if _, err := l.SetDeadline(sp.ID, nil, ""); !errors.Is(err, ErrLeaseReaping) {
		t.Errorf("SetDeadline during a committed reap: %v", err)
	}
}

// A deadline changed from outside (a Sprites lease renewal or release, which
// is ChangeDeadline) moves the deadline alone: an action set by the policy
// survives it.
func TestAChangedDeadlineKeepsItsAction(t *testing.T) {
	l := newTestEngine(t, Options{})
	sp := createSprite(t, l, "web")
	withPolicy(t, l, sp.ID, store.LifecyclePolicy{DeadlineAction: store.DeadlineSuspend})
	later := time.Now().Add(time.Minute)
	if _, err := l.ChangeDeadline(sp.ID, func(d *Deadline) { d.At, d.Protected = &later, true }); err != nil {
		t.Fatal(err)
	}
	if _, err := l.ChangeDeadline(sp.ID, func(d *Deadline) { d.At, d.Protected = nil, false }); err != nil {
		t.Fatal(err)
	}
	if r := record(t, l, sp.ID); r.Lifecycle.OnDeadline() != store.DeadlineSuspend || r.ExpiresAt != nil || r.Protected {
		t.Errorf("after a renewal and a release: %+v", r)
	}
}

// Suspend is stop(keepWarm): a sandbox that is not running is left as it is,
// warm or cold, and nothing is reported.
func TestSuspendLeavesASandboxThatIsNotRunningAlone(t *testing.T) {
	l := newTestEngine(t, Options{})
	warm := warmSprite(t, l, "warm", time.Now())
	cold := warmSprite(t, l, "cold", time.Now())
	vmm.DiscardSnapshot(l.store.Dir(cold.ID))
	sub := ruleEvents(l)
	for _, sp := range []store.Sprite{warm, cold} {
		if err := l.Suspend(sp.Record); err != nil {
			t.Fatal(err)
		}
	}
	if got := collect(sub); len(got) != 0 {
		t.Errorf("events %s", eventTypes(got))
	}
	if l.Status(warm.Record) != "warm" || l.Status(cold.Record) != "cold" {
		t.Errorf("states %s, %s", l.Status(warm.Record), l.Status(cold.Record))
	}
}
