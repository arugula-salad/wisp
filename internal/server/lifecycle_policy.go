package server

import (
	"context"
	"net/http"
	"strconv"
	"time"

	"github.com/arugula-salad/wisp/internal/store"
	"github.com/arugula-salad/wisp/internal/vmm"
)

// The engine half of the sprite policies: getting them to the guest, on boot,
// on wake and when they change (policy.go and policy_limits.go have the routes).

// ApplyPolicy hands sp's stored privileges and resources policy to the guest
// if the sprite is running. A sleeping sprite is not woken: it gets the policy
// on wake. (applyPolicy, below, is the cold-boot half.)
func (l *Lifecycle) ApplyPolicy(ctx context.Context, sp store.Sprite) error {
	rt := l.rt(sp.ID)
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if rt.m == nil {
		return nil
	}
	return l.pushPolicy(ctx, rt.m, sp.Name)
}

// guestPolicy is agent.Policy: the part of the policies the guest enforces.
type guestPolicy struct {
	Profile       string `json:"profile,omitempty"`
	NoNewPrivs    bool   `json:"no_new_privs,omitempty"`
	MemoryLimitMB int    `json:"memory_limit_mb,omitempty"`
}

func guestPolicyFor(sp store.Sprite) guestPolicy {
	var p guestPolicy
	if sp.Privileges != nil {
		p.Profile, p.NoNewPrivs = sp.Privileges.Profile, sp.Privileges.NoNewPrivileges
	}
	if sp.Resources != nil && sp.Resources.Memory != nil {
		p.MemoryLimitMB = sp.Resources.Memory.LimitMB
	}
	return p
}

// pushPolicy sends the stored policy to a running guest. It reads the store
// rather than take a Sprite, because callers' copies can predate a policy change.
func (l *Lifecycle) pushPolicy(ctx context.Context, m *vmm.Machine, name string) error {
	sp, err := l.store.Get(name)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return agentCall(ctx, m, http.MethodPost, "/internal/policy", guestPolicyFor(sp), nil)
}

// policyResumed brings a sprite that just woke from a snapshot up to date: the
// policy may have changed while it slept. (A cold boot has it on the kernel
// command line.) False means the guest cannot be trusted to enforce the policy
// and must be cold booted instead, at the cost of its memory state. A guest
// that fails to take an empty policy is let through: that is a snapshot from
// before agents knew about policies, and it has nothing to enforce.
func (l *Lifecycle) policyResumed(ctx context.Context, m *vmm.Machine, sp store.Sprite) bool {
	err := l.pushPolicy(ctx, m, sp.Name)
	if err == nil {
		return true
	}
	if cur, gerr := l.store.Get(sp.Name); gerr == nil && guestPolicyFor(cur) == (guestPolicy{}) {
		l.log.Warn("guest did not accept the (empty) policy", "sprite", sp.Name, "err", err)
		return true
	}
	l.log.Warn("guest cannot enforce the policy; discarding warm state to boot a current agent", "sprite", sp.Name, "err", err)
	return false
}

// applyPolicy shapes a cold boot: the policy rides the kernel command line so
// it is in force before the guest starts its services, and a memory limit
// sizes the VM, which is the one bound that root inside the guest cannot lift.
func applyPolicy(cfg *vmm.Config, sp store.Sprite) {
	p := guestPolicyFor(sp)
	if p.Profile != "" {
		cfg.BootArgs = append(cfg.BootArgs, "sprite.profile="+p.Profile)
	}
	if p.NoNewPrivs {
		cfg.BootArgs = append(cfg.BootArgs, "sprite.nnp=1")
	}
	if p.MemoryLimitMB > 0 {
		cfg.BootArgs = append(cfg.BootArgs, "sprite.memlimit="+strconv.Itoa(p.MemoryLimitMB))
		cfg.MemMiB = p.MemoryLimitMB + vmHeadroomMiB
	}
}

// publishNetworkPolicy writes the policy to /.sprite/policy/network.json inside
// a running sprite, where upstream puts it. Best effort: the file is information
// for tools in the guest, enforcement is entirely on the host, and the next wake
// publishes again.
func (l *Lifecycle) publishNetworkPolicy(ctx context.Context, m *vmm.Machine, sp store.Sprite) {
	rules := sp.NetworkRules
	if rules == nil {
		rules = []store.NetworkRule{}
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if err := agentCall(ctx, m, http.MethodPost, "/internal/netpolicy", networkPolicyJSON{Rules: rules}, nil); err != nil {
		l.log.Debug("could not publish the network policy file in the guest", "sprite", sp.Name, "err", err)
	}
}

// RepublishNetworkPolicy is for a policy change on a sprite that may be running.
func (l *Lifecycle) RepublishNetworkPolicy(name string) {
	sp, err := l.store.Get(name)
	if err != nil {
		return
	}
	rt := l.rt(sp.ID)
	rt.mu.Lock()
	m := rt.m
	rt.mu.Unlock()
	if m != nil {
		l.publishNetworkPolicy(context.Background(), m, sp)
	}
}
