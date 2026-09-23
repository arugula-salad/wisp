//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	sprites "github.com/superfly/sprites-go"
)

// TestNetworkPolicy drives the policy API with the official SDK. What a
// restrictive policy should do depends on the daemon under test:
//
//	SPRITES_E2E_NETD=absent   no wisp-netd (or --net=false): it must be refused
//	SPRITES_E2E_NETD=present  helper installed: it must be accepted and round-trip
//
// Unset, either is accepted and the one observed is logged. Enforcement itself is
// not checked here (see scripts/verify-network-policy.sh and test-netpolicy-netns.sh).
func TestNetworkPolicy(t *testing.T) {
	c := client(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	name := fmt.Sprintf("e2e-pol-%d", time.Now().UnixNano()%1e9)
	if _, err := c.CreateSprite(ctx, name, nil); err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Cleanup(func() { c.DeleteSprite(context.Background(), name) })
	sp := c.Sprite(name)

	get := func() []sprites.NetworkPolicyRule {
		t.Helper()
		p, err := sp.GetNetworkPolicy(ctx)
		if err != nil {
			t.Fatalf("get policy: %v", err)
		}
		return p.Rules
	}
	if rules := get(); len(rules) != 0 {
		t.Fatalf("a new sprite should have no policy (unrestricted), got %v", rules)
	}

	// Policies that restrict nothing are accepted everywhere.
	allowAll := []sprites.NetworkPolicyRule{{Domain: "*", Action: "allow"}}
	if err := sp.UpdateNetworkPolicy(ctx, &sprites.NetworkPolicy{Rules: allowAll}); err != nil {
		t.Fatalf("set allow-all: %v", err)
	}
	if rules := get(); !reflect.DeepEqual(rules, allowAll) {
		t.Errorf("allow-all round trip: %v", rules)
	}

	for what, bad := range map[string]sprites.NetworkPolicyRule{
		"unknown action":   {Domain: "example.com", Action: "permit"},
		"unknown include":  {Include: "everything"},
		"inner wildcard":   {Domain: "a.*.com", Action: "allow"},
		"rule with no aim": {},
	} {
		err := sp.UpdateNetworkPolicy(ctx, &sprites.NetworkPolicy{Rules: []sprites.NetworkPolicyRule{bad}})
		if err == nil || !strings.Contains(err.Error(), "invalid policy") { // the SDK's wording for a 400
			t.Errorf("%s: err = %v, want a 400", what, err)
		}
	}
	if rules := get(); !reflect.DeepEqual(rules, allowAll) {
		t.Errorf("a rejected policy changed the stored one: %v", rules)
	}

	restrictive := []sprites.NetworkPolicyRule{
		{Include: "defaults"},
		{Domain: "example.com", Action: "allow"},
		{Domain: "*.example.com", Action: "allow"},
		{Domain: "blocked.example.com", Action: "deny"},
	}
	err := sp.UpdateNetworkPolicy(ctx, &sprites.NetworkPolicy{Rules: restrictive})
	mode := os.Getenv("SPRITES_E2E_NETD")
	switch {
	case err != nil && mode != "present":
		// Fail closed: never "accepted but not enforced".
		if !strings.Contains(err.Error(), "status 503") || !strings.Contains(err.Error(), "policy_unenforceable") {
			t.Fatalf("restrictive policy without the helper: err = %v, want 503 policy_unenforceable", err)
		}
		t.Logf("helper absent: restrictive policy refused: %v", err)
		if rules := get(); !reflect.DeepEqual(rules, allowAll) {
			t.Errorf("the refused policy is being reported as in force: %v", rules)
		}
	case err == nil && mode != "absent":
		t.Log("helper present: restrictive policy accepted")
		if rules := get(); !reflect.DeepEqual(rules, restrictive) {
			t.Errorf("round trip:\n got %v\nwant %v", rules, restrictive)
		}
	default:
		t.Fatalf("SPRITES_E2E_NETD=%s but setting a restrictive policy returned: %v", mode, err)
	}

	// The sprite is still usable either way, and an empty rule list clears the policy.
	if out, err := sp.CommandContext(ctx, "echo", "alive").Output(); err != nil || string(out) != "alive\n" {
		t.Errorf("exec after policy changes: %q, %v", out, err)
	}
	if err := sp.UpdateNetworkPolicy(ctx, &sprites.NetworkPolicy{Rules: []sprites.NetworkPolicyRule{}}); err != nil {
		t.Fatalf("clear: %v", err)
	}
	if rules := get(); len(rules) != 0 {
		t.Errorf("after clearing: %v", rules)
	}

	if _, err := c.Sprite("e2e-pol-no-such-sprite").GetNetworkPolicy(ctx); err == nil || !strings.Contains(err.Error(), "404") {
		t.Errorf("get policy of a missing sprite: %v", err)
	}
}
