package server

import (
	"errors"
	"net/http"
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
func ruleEvents(s *Server) *eventSub {
	sub, _, _ := s.life.events.subscribe(func(e Event) bool {
		switch e.Type {
		case "sprite.expiring", "sprite.expired", "sprite.deleted", "sprite.suspended", "sprite.stopped", "sprite.cold":
			return true
		}
		return false
	}, 0, false)
	return sub
}

func eventTypes(es []Event) string {
	var out []string
	for _, e := range es {
		out = append(out, e.Type)
	}
	return strings.Join(out, ",")
}

// withPolicy gives a stored sprite a lifecycle policy through the engine.
func withPolicy(t *testing.T, s *Server, id string, p store.LifecyclePolicy) {
	t.Helper()
	if _, err := s.life.SetPolicy(id, p); err != nil {
		t.Fatal(err)
	}
}

func record(t *testing.T, s *Server, id string) store.Record {
	t.Helper()
	r, err := s.store.GetRecord(id)
	if err != nil {
		t.Fatalf("record %s: %v", id, err)
	}
	return r
}

// No policy is today's rule and nothing else: the idle timeout is the
// daemon's and the action a warm suspend. A policy changes it for that
// sandbox alone, and is read afresh, so a change reaches a running watcher.
func TestIdleRuleIsThePolicysOrTheDaemons(t *testing.T) {
	s, _ := newOperatorServer(t, Options{IdleTimeout: 30 * time.Second})
	plain := warmSprite(t, s, "plain", time.Now())
	ruled := warmSprite(t, s, "ruled", time.Now())
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
			withPolicy(t, s, ruled.ID, *tc.p)
		}
		// The watcher holds the record it booted with; the rule comes from the store.
		if d, a := s.life.idleRule(ruled.Record); d != tc.timeout || a != tc.action {
			t.Errorf("policy %+v: idle rule %v %q, want %v %q", tc.p, d, a, tc.timeout, tc.action)
		}
		if d, a := s.life.idleRule(plain.Record); d != 30*time.Second || a != store.IdleSuspend {
			t.Errorf("a sprite without a policy got %v %q", d, a)
		}
	}
	// A sandbox Create holds outside the store is ruled by the record it has.
	unstored := store.Record{ID: "notstored", Lifecycle: &store.LifecyclePolicy{IdleAction: store.IdleNone}}
	if _, a := s.life.idleRule(unstored); a != store.IdleNone {
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
			s, _ := newOperatorServer(t, Options{})
			sp := warmSprite(t, s, "sbx", time.Now())
			if !tc.warm {
				vmm.DiscardSnapshot(s.store.Dir(sp.ID))
			}
			disk := filepath.Join(s.store.Dir(sp.ID), vmm.DiskFile)
			os.WriteFile(disk, []byte("disk"), 0o644)
			withPolicy(t, s, sp.ID, store.LifecyclePolicy{DeadlineAction: tc.action})
			// Inside the warning window: a lease would be warned about now.
			soon := time.Now().Add(time.Minute)
			if _, err := s.life.SetDeadline(sp.ID, &soon, ""); err != nil {
				t.Fatal(err)
			}
			sub := ruleEvents(s)
			s.leases.sweep()
			if got := collect(sub); len(got) != 0 {
				t.Fatalf("before the deadline: events %s, want none", eventTypes(got))
			}

			expire(t, s, sp.Name, time.Now().Add(-time.Second))
			s.leases.sweep()
			got := collect(sub)
			if eventTypes(got) != tc.events {
				t.Errorf("events %q, want %q", eventTypes(got), tc.events)
			}
			if tc.events != "" && got[0].Detail["reason"] != "deadline" {
				t.Errorf("%s detail %v, want reason deadline", got[0].Type, got[0].Detail)
			}
			r := record(t, s, sp.ID)
			if r.ExpiresAt != nil {
				t.Errorf("the deadline was not spent: %v", r.ExpiresAt)
			}
			if r.Lifecycle.OnDeadline() != tc.action {
				t.Errorf("the deadline action changed to %q", r.Lifecycle.OnDeadline())
			}
			if _, err := os.Stat(disk); err != nil {
				t.Errorf("the disk went with the deadline: %v", err)
			}
			if vmm.HasSnapshot(s.store.Dir(sp.ID)) != tc.wantWarm {
				t.Errorf("warm = %v, want %v", !tc.wantWarm, tc.wantWarm)
			}
			// Spent: another pass does nothing more.
			s.leases.sweep()
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
			s, _ := newOperatorServer(t, Options{})
			sp := warmSprite(t, s, "sbx", time.Now())
			withPolicy(t, s, sp.ID, store.LifecyclePolicy{DeadlineAction: action})
			if _, err := s.life.ChangeDeadline(sp.ID, func(d *Deadline) { d.Protected = true }); err != nil {
				t.Fatal(err)
			}
			past := time.Now().Add(-time.Minute)
			expire(t, s, sp.Name, past)
			sub := ruleEvents(s)
			s.leases.sweep()
			if got := collect(sub); len(got) != 0 {
				t.Errorf("a protected sandbox got %s", eventTypes(got))
			}
			r := record(t, s, sp.ID)
			if r.ExpiresAt == nil || !r.ExpiresAt.Equal(past) || !vmm.HasSnapshot(s.store.Dir(sp.ID)) {
				t.Errorf("protected sandbox changed: %+v", r)
			}
			// Unprotected, the deadline is acted on at once.
			s.life.ChangeDeadline(sp.ID, func(d *Deadline) { d.Protected = false })
			s.leases.sweep()
			_, err := s.store.GetRecord(sp.ID)
			if deleted := errors.Is(err, store.ErrNotFound); deleted != (action == store.DeadlineDelete) {
				t.Errorf("deleted = %v once unprotected", deleted)
			}
		})
	}
}

// Extending a deadline is setting it again: the action and the protection
// stay as they were, and a deadline in the future is not acted on.
func TestExtendingADeadline(t *testing.T) {
	s, _ := newOperatorServer(t, Options{})
	sp := warmSprite(t, s, "sbx", time.Now())
	soon := time.Now().Add(time.Second)
	if _, err := s.life.SetDeadline(sp.ID, &soon, store.DeadlineSuspend); err != nil {
		t.Fatal(err)
	}
	if r := record(t, s, sp.ID); r.Lifecycle.OnDeadline() != store.DeadlineSuspend || r.ExpiresAt == nil {
		t.Fatalf("after SetDeadline: %+v", r)
	}
	later := time.Now().Add(time.Hour)
	if _, err := s.life.SetDeadline(sp.ID, &later, ""); err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Until(soon) + 10*time.Millisecond)
	s.leases.sweep()
	r := record(t, s, sp.ID)
	if r.ExpiresAt == nil || !r.ExpiresAt.Equal(later) || r.Lifecycle.OnDeadline() != store.DeadlineSuspend {
		t.Fatalf("after an extension and a sweep past the first deadline: %+v", r)
	}
	if _, err := s.life.SetDeadline(sp.ID, &later, "explode"); !errors.Is(err, errBadPolicy) {
		t.Errorf("unknown action: %v", err)
	}
	// Clearing it leaves the action for the next one.
	if _, err := s.life.SetDeadline(sp.ID, nil, ""); err != nil {
		t.Fatal(err)
	}
	if r := record(t, s, sp.ID); r.ExpiresAt != nil || r.Lifecycle.OnDeadline() != store.DeadlineSuspend {
		t.Errorf("after clearing: %+v", r)
	}
}

// A policy of defaults is no policy: it is not written, so a sprite.json
// keeps its bytes. One the engine does not know is refused.
func TestSetPolicyStoresOnlyWhatDiffersFromTheDefaults(t *testing.T) {
	s, _ := newOperatorServer(t, Options{})
	sp := warmSprite(t, s, "sbx", time.Now())
	file := filepath.Join(s.store.Dir(sp.ID), "sprite.json")
	for _, p := range []store.LifecyclePolicy{{}, {IdleAction: store.IdleSuspend, DeadlineAction: store.DeadlineDelete}} {
		if r, err := s.life.SetPolicy(sp.ID, p); err != nil || r.Lifecycle != nil {
			t.Fatalf("SetPolicy(%+v) = %+v, %v; want no policy", p, r.Lifecycle, err)
		}
		if b, _ := os.ReadFile(file); strings.Contains(string(b), "lifecycle") {
			t.Fatalf("defaults written to disk: %s", b)
		}
	}
	for _, p := range []store.LifecyclePolicy{{IdleAction: "nap"}, {DeadlineAction: "pause"}, {IdleTimeout: -time.Second}} {
		if _, err := s.life.SetPolicy(sp.ID, p); !errors.Is(err, errBadPolicy) {
			t.Errorf("SetPolicy(%+v) = %v, want errBadPolicy", p, err)
		}
	}
	if _, err := s.life.SetPolicy("nosuch", store.LifecyclePolicy{}); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("SetPolicy on a missing sandbox: %v", err)
	}
	// And the policy survives a restart.
	withPolicy(t, s, sp.ID, store.LifecyclePolicy{IdleTimeout: time.Minute, IdleAction: store.IdleStop})
	again, _ := restart(t, s)
	if r := record(t, again, sp.ID); r.Lifecycle == nil || r.Lifecycle.IdleTimeout != time.Minute || r.Lifecycle.IdleAction != store.IdleStop {
		t.Errorf("after a restart: %+v", r.Lifecycle)
	}
}

// The sprite.expiring warning is the lease's: it goes out for a delete
// deadline and for no other. Changing the action re-arms it either way.
func TestOnlyADeleteDeadlineIsWarnedAbout(t *testing.T) {
	s, _ := newOperatorServer(t, Options{})
	sp := warmSprite(t, s, "sbx", time.Now())
	withPolicy(t, s, sp.ID, store.LifecyclePolicy{DeadlineAction: store.DeadlineStop})
	sub := leaseEvents(s)
	soon := time.Now().Add(time.Minute)
	s.life.SetDeadline(sp.ID, &soon, "")
	s.leases.sweep()
	if got := collect(sub); len(got) != 0 {
		t.Fatalf("a stop deadline was warned about: %s", eventTypes(got))
	}
	withPolicy(t, s, sp.ID, store.LifecyclePolicy{}) // back to a lease
	if got := collect(sub); eventTypes(got) != "sprite.expiring" {
		t.Fatalf("a delete deadline inside the window: %q, want one sprite.expiring", eventTypes(got))
	}
}

// A reap decides on a stale record and commits under the lock on a fresh
// one: a policy that changed the deadline's action in between wins, and the
// sandbox is stopped instead of deleted on the next pass. And a policy change
// that arrives once a reap has committed is refused, as a renewal is.
func TestAPolicyChangeRacesTheReaperLikeARenewal(t *testing.T) {
	s, _ := newOperatorServer(t, Options{})
	sp := warmSprite(t, s, "sbx", time.Now())
	stale := expire(t, s, sp.Name, time.Now().Add(-time.Second))
	withPolicy(t, s, sp.ID, store.LifecyclePolicy{DeadlineAction: store.DeadlineStop})
	s.leases.reap(stale.Record)
	if _, err := s.store.GetRecord(sp.ID); err != nil {
		t.Fatal("the reaper deleted a sandbox whose deadline now stops it")
	}
	s.leases.sweep()
	if r := record(t, s, sp.ID); r.ExpiresAt != nil || vmm.HasSnapshot(s.store.Dir(sp.ID)) {
		t.Errorf("the stop deadline was not taken: %+v", r)
	}

	s.leases.claim(sp.ID)
	defer s.leases.release(sp.ID)
	if _, err := s.life.SetPolicy(sp.ID, store.LifecyclePolicy{}); !errors.Is(err, errLeaseReaping) {
		t.Errorf("SetPolicy during a committed reap: %v", err)
	}
	if _, err := s.life.SetDeadline(sp.ID, nil, ""); !errors.Is(err, errLeaseReaping) {
		t.Errorf("SetDeadline during a committed reap: %v", err)
	}
}

// The Sprites lease is the deadline with its default action: renewing it
// through the API changes the deadline alone, never the action, and a sprite
// with no policy does not get one.
func TestTheLeaseAPIIsTheDeadline(t *testing.T) {
	s, h := newOperatorServer(t, Options{})
	status(t, apiCall(t, h, "POST", "/v1/sprites", `{"name":"web","ttl_seconds":600}`), http.StatusCreated)
	sp, _ := s.store.GetByName(store.Sprites, "web")
	status(t, apiCall(t, h, "POST", leaseURL("web"), `{"ttl_seconds":3600,"protected":true}`), http.StatusOK)
	r := record(t, s, sp.ID)
	if r.Lifecycle != nil || r.ExpiresAt == nil || time.Until(*r.ExpiresAt) < 59*time.Minute || !r.Protected {
		t.Fatalf("after a renewal: %+v", r)
	}
	if b, _ := os.ReadFile(filepath.Join(s.store.Dir(sp.ID), "sprite.json")); strings.Contains(string(b), "lifecycle") {
		t.Fatalf("a lease wrote a policy: %s", b)
	}
	if body := status(t, apiCall(t, h, "GET", "/v1/sprites/web", ""), http.StatusOK); strings.Contains(string(body), "lifecycle") ||
		strings.Contains(string(body), "deadline") || strings.Contains(string(body), "idle_") {
		t.Errorf("the sprite's JSON grew a policy field: %s", body)
	}
	// An action the engine set (no Sprites call can) survives a renewal.
	withPolicy(t, s, sp.ID, store.LifecyclePolicy{DeadlineAction: store.DeadlineSuspend})
	status(t, apiCall(t, h, "POST", leaseURL("web"), `{"ttl_seconds":60}`), http.StatusOK)
	status(t, apiCall(t, h, "DELETE", leaseURL("web"), ""), http.StatusNoContent)
	if r := record(t, s, sp.ID); r.Lifecycle.OnDeadline() != store.DeadlineSuspend || r.ExpiresAt != nil || r.Protected {
		t.Errorf("after a renewal and a release: %+v", r)
	}
}

// Suspend is Stop(keepWarm): a sandbox that is not running is left as it is,
// warm or cold, and nothing is reported.
func TestSuspendLeavesASandboxThatIsNotRunningAlone(t *testing.T) {
	s, _ := newOperatorServer(t, Options{})
	warm := warmSprite(t, s, "warm", time.Now())
	cold := warmSprite(t, s, "cold", time.Now())
	vmm.DiscardSnapshot(s.store.Dir(cold.ID))
	sub := ruleEvents(s)
	for _, sp := range []store.Sprite{warm, cold} {
		if err := s.life.Suspend(sp.Record); err != nil {
			t.Fatal(err)
		}
	}
	if got := collect(sub); len(got) != 0 {
		t.Errorf("events %s", eventTypes(got))
	}
	if s.life.Status(warm.Record) != "warm" || s.life.Status(cold.Record) != "cold" {
		t.Errorf("states %s, %s", s.life.Status(warm.Record), s.life.Status(cold.Record))
	}
}
