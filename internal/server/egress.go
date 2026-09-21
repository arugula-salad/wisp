package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jhgaylor/mini-sprites/internal/netd"
	"github.com/jhgaylor/mini-sprites/internal/netpolicy"
	"github.com/jhgaylor/mini-sprites/internal/store"
	"github.com/jhgaylor/mini-sprites/internal/vmm"
)

// Ports the nftables rules from setup-host.sh redirect restricted sprites to.
// Both listeners bind the bridge address only.
const (
	egressDNSPort   = 7853
	egressProxyPort = 7880
)

// errUnenforceable means a restrictive policy could not be backed by the
// kernel. Callers must fail closed: refuse the policy, or withhold the NIC.
var errUnenforceable = errors.New("network policy cannot be enforced")

// egress ties network policy together. A sprite whose policy can refuse
// something is "restricted": its address belongs to the nft set restricted4,
// which makes the kernel hand its DNS and TCP to the listeners here and drop
// the rest. Everyone else stays on the plain NAT path and never touches this.
//
// spritesd is unprivileged, so set membership is changed through the root
// helper mini-sprites-netd. Every push carries the complete membership.
type egress struct {
	log     *slog.Logger
	store   *store.Store
	gateway netip.Addr // invalid when this daemon has no sprite network
	enf     *netpolicy.Enforcer
	push    func(context.Context, []netip.Addr) error
	// down is why restrictive policies are impossible here; empty when they are possible.
	down string

	// mu orders policy writes, boots and pushes, so the record, the enforcer
	// and the kernel set cannot be left reflecting different policies.
	mu       sync.Mutex
	lastPush string // outcome of the latest push, to log changes only
}

func newEgress(opts Options, st *store.Store, log *slog.Logger, gateway net.IP) *egress {
	e := &egress{log: log, store: st, enf: netpolicy.NewEnforcer(log)}
	socket := opts.NetdSocket
	if socket == "" {
		socket = netd.DefaultSocket
	}
	e.push = func(ctx context.Context, addrs []netip.Addr) error { return netd.Push(ctx, socket, addrs) }
	gw, ok := netip.AddrFromSlice(gateway.To4())
	if !ok {
		e.down = "this spritesd has no guest network (see scripts/setup-host.sh, --net)"
		return e
	}
	e.gateway = gw
	if err := e.listen(opts.DNS); err != nil {
		e.down = err.Error()
		log.Error("network policy listeners failed; restrictive policies will be refused", "err", err)
	}
	go e.reconcile()
	return e
}

func (e *egress) listen(resolvers string) error {
	d := &netpolicy.DNS{Enforcer: e.enf, Log: e.log, Blocked: netpolicy.Blocked,
		Upstreams: netpolicy.Upstreams(strings.Split(resolvers, ","))}
	dnsAddr, stop, err := d.Start(netip.AddrPortFrom(e.gateway, egressDNSPort).String())
	if err != nil {
		return fmt.Errorf("policy DNS listener: %w", err)
	}
	ln, err := net.Listen("tcp4", netip.AddrPortFrom(e.gateway, egressProxyPort).String())
	if err != nil {
		stop()
		return fmt.Errorf("policy proxy listener: %w", err)
	}
	go netpolicy.NewProxy(e.enf, e.log).Serve(ln)
	e.log.Info("network policy listeners up", "dns", dnsAddr, "proxy", ln.Addr())
	return nil
}

func (e *egress) addr(sp store.Sprite) netip.Addr {
	if !e.gateway.IsValid() || sp.NetIndex == 0 {
		return netip.Addr{}
	}
	gw := e.gateway.As4()
	return netip.AddrFrom4([4]byte{gw[0], gw[1], byte(sp.NetIndex >> 8), byte(sp.NetIndex)})
}

// compile never fails open: rules that were valid when stored but no longer
// compile (say, an include this build lacks) become "refuse everything".
func (e *egress) compile(sp store.Sprite) *netpolicy.Policy {
	p, err := netpolicy.Compile(sp.NetworkRules)
	if err != nil {
		e.log.Error("stored network policy is invalid; denying all egress", "sprite", sp.Name, "err", err)
		p, _ = netpolicy.Compile([]store.NetworkRule{{Domain: "*", Action: "deny"}})
	}
	return p
}

// syncLocked pushes the address of every restricted sprite. Addresses are per
// sprite, not per boot, so suspended and cold sprites are members too and a
// wake needs no set change of its own.
func (e *egress) syncLocked() error {
	if !e.gateway.IsValid() {
		// Not the daemon that owns the sprite network (--net=false, or no bridge). The
		// set is shared host state; a push from here would erase the owner's members.
		return nil
	}
	var want []netip.Addr
	for _, sp := range e.store.List("") {
		if a := e.addr(sp); a.IsValid() && e.compile(sp).Restrictive() {
			want = append(want, a)
		}
	}
	sort.Slice(want, func(i, j int) bool { return want[i].Less(want[j]) })
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	err := e.push(ctx, want)
	outcome := "ok"
	if err != nil {
		outcome = err.Error()
	}
	if outcome != e.lastPush {
		switch {
		case err == nil:
			e.log.Info("network policy helper reachable; restricted set pushed", "members", len(want))
		case len(want) == 0:
			e.log.Info("network policy helper unreachable; restrictive policies will be refused", "err", err)
		default:
			e.log.Error("cannot push restricted set; affected sprites boot without a NIC", "members", len(want), "err", err)
		}
		e.lastPush = outcome
	}
	return err
}

// reconcile re-pushes periodically. The set lives in the kernel and can be
// emptied behind our back (setup-host.sh re-run, helper restart, nft flush);
// an empty set means restricted sprites are not restricted.
func (e *egress) reconcile() {
	for {
		e.mu.Lock()
		e.syncLocked()
		e.mu.Unlock()
		time.Sleep(15 * time.Second)
	}
}

// setPolicy stores rules for the sprite and makes them effective immediately.
// A restrictive policy that the kernel cannot back is rolled back and rejected.
func (e *egress) setPolicy(name string, rules []store.NetworkRule, p *netpolicy.Policy) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	sp, err := e.store.Get(name)
	if err != nil {
		return err
	}
	if p.Restrictive() && e.down != "" {
		return fmt.Errorf("%w: %s", errUnenforceable, e.down)
	}
	prev := sp.NetworkRules
	if sp, err = e.store.Update(name, func(s *store.Sprite) {
		s.NetworkRules = rules
		s.UpdatedAt = time.Now().UTC()
	}); err != nil {
		return err
	}
	// The enforcer learns the policy before the kernel starts diverting the
	// sprite to it, so no connection is judged by the policy it replaced.
	if a := e.addr(sp); a.IsValid() {
		e.enf.Set(a, sp.Name, p)
	}
	if err := e.syncLocked(); err != nil {
		if !p.Restrictive() {
			// Loosening cannot leak. Until a push lands the sprite just stays
			// diverted to listeners that now allow it everything.
			return nil
		}
		e.store.Update(name, func(s *store.Sprite) { s.NetworkRules = prev })
		if a := e.addr(sp); a.IsValid() {
			sp.NetworkRules = prev
			e.enf.Set(a, sp.Name, e.compile(sp))
		}
		return fmt.Errorf("%w: mini-sprites-netd: %v (is the helper installed? sudo scripts/setup-host.sh)", errUnenforceable, err)
	}
	return nil
}

// admit is asked before a sprite is given a NIC. It refreshes the enforcer from
// the record and, for a restricted sprite, insists on a push that succeeds now:
// a set that was right a minute ago proves nothing about this boot.
func (e *egress) admit(sp store.Sprite) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if cur, err := e.store.Get(sp.Name); err == nil {
		sp = cur // the caller's copy may predate a policy change
	}
	p := e.compile(sp)
	if a := e.addr(sp); a.IsValid() {
		e.enf.Set(a, sp.Name, p)
	}
	if !p.Restrictive() {
		return nil
	}
	if e.down != "" {
		return fmt.Errorf("%w: %s", errUnenforceable, e.down)
	}
	if err := e.syncLocked(); err != nil {
		return fmt.Errorf("%w: mini-sprites-netd: %v", errUnenforceable, err)
	}
	return nil
}

// forget is called once a sprite is deleted. Its address will be reused.
func (e *egress) forget(sp store.Sprite) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if a := e.addr(sp); a.IsValid() {
		e.enf.Remove(a)
		if e.compile(sp).Restrictive() {
			e.syncLocked()
		}
	}
}

// tapFor takes a tap for the sprite unless its network policy cannot be
// enforced right now, in which case it fails closed: no NIC.
func (l *Lifecycle) tapFor(sp store.Sprite) (string, error) {
	tap := l.takeTap()
	if tap == "" {
		return "", nil
	}
	err := l.egress.admit(sp)
	if err == nil {
		return tap, nil
	}
	l.returnTap(tap)
	if vmm.HasSnapshot(l.store.Dir(sp.ID)) && sp.BootIP != "" {
		// Its snapshot has a NIC, so resuming without one means discarding memory
		// state. An unreachable helper is fixable; lost processes are not.
		return "", fmt.Errorf("refusing to wake %s: %w", sp.Name, err)
	}
	l.log.Warn("booting without a NIC", "sprite", sp.Name, "reason", err)
	return "", nil
}
