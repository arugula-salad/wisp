package store

import (
	"bytes"
	"encoding/json"
	"testing"
	"time"
)

// No policy is the daemon's defaults, and so is a policy of zero fields.
func TestLifecyclePolicyDefaults(t *testing.T) {
	for _, p := range []*LifecyclePolicy{nil, {}} {
		if d, a := p.Idle(30 * time.Second); d != 30*time.Second || a != IdleSuspend {
			t.Errorf("%+v: idle rule %v %q, want the daemon's timeout and suspend", p, d, a)
		}
		if a := p.OnDeadline(); a != DeadlineDelete {
			t.Errorf("%+v: deadline action %q, want delete", p, a)
		}
	}
	p := &LifecyclePolicy{IdleTimeout: time.Minute, IdleAction: IdleStop, DeadlineAction: DeadlineSuspend}
	if d, a := p.Idle(30 * time.Second); d != time.Minute || a != IdleStop {
		t.Errorf("idle rule %v %q", d, a)
	}
	if p.OnDeadline() != DeadlineSuspend {
		t.Errorf("deadline action %q", p.OnDeadline())
	}
	for _, bad := range []LifecyclePolicy{{IdleAction: "sleep"}, {DeadlineAction: "pause"}, {IdleTimeout: -1}} {
		if bad.Valid() {
			t.Errorf("%+v is valid", bad)
		}
	}
	if !p.Valid() || !(LifecyclePolicy{}).Valid() {
		t.Error("a good policy is not valid")
	}
}

// A lease written before there were policies reads back with none, which is
// a lease: the sprite is deleted at the deadline. And a record without a
// policy writes no lifecycle key, so sprite.json is unchanged.
func TestLeaseOnDiskIsADeleteDeadline(t *testing.T) {
	onDisk := []byte(`{"id":"a","name":"n","expires_at":"2026-10-01T09:00:00Z","protected":true}`)
	var sp Sprite
	if err := json.Unmarshal(onDisk, &sp); err != nil {
		t.Fatal(err)
	}
	if sp.Lifecycle != nil || sp.Lifecycle.OnDeadline() != DeadlineDelete || sp.ExpiresAt == nil || !sp.Protected {
		t.Fatalf("read back %+v", sp.Record)
	}
	if b := encode(t, sp); bytes.Contains(b, []byte("lifecycle")) {
		t.Fatalf("a record without a policy wrote one: %s", b)
	}
	sp.Lifecycle = &LifecyclePolicy{DeadlineAction: DeadlineStop}
	b := encode(t, sp)
	if !bytes.Contains(b, []byte(`"deadline_action": "stop"`)) {
		t.Fatalf("policy not written: %s", b)
	}
	var back Sprite
	json.Unmarshal(b, &back)
	if back.Lifecycle == nil || back.Lifecycle.OnDeadline() != DeadlineStop {
		t.Fatalf("policy read back as %+v", back.Lifecycle)
	}
}
