package engine

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/arugula-salad/wisp/internal/netd"
	"github.com/arugula-salad/wisp/internal/netpolicy"
	"github.com/arugula-salad/wisp/internal/store"
	"github.com/arugula-salad/wisp/internal/vmm"
)

// fakeHelper stands in for wisp-netd: it records each pushed membership, or fails.
type fakeHelper struct {
	mu     sync.Mutex
	pushes [][]string
	err    error
}

func (f *fakeHelper) push(_ context.Context, addrs []netip.Addr) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	got := []string{}
	for _, a := range addrs {
		got = append(got, a.String())
	}
	f.pushes = append(f.pushes, got)
	return nil
}

func (f *fakeHelper) last() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.pushes) == 0 {
		return nil
	}
	return f.pushes[len(f.pushes)-1]
}

func (f *fakeHelper) fail(err error) { f.mu.Lock(); f.err = err; f.mu.Unlock() }

// newEgressEngine is an engine with a networked egress (gateway 10.209.0.1) but
// no listeners, reconcile loop or VMs.
func newEgressEngine(t *testing.T, helper *fakeHelper) (*Engine, *store.Store) {
	t.Helper()
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	e := &egress{log: quiet, store: st, gateway: netip.MustParseAddr("10.209.0.1"), enf: netpolicy.NewEnforcer(quiet), push: helper.push}
	return &Engine{store: st, log: quiet, runtimes: map[string]*runtime{}, unstored: map[string]store.Sprite{}, egress: e}, st
}

func addSprite(t *testing.T, st *store.Store, name string) store.Sprite {
	t.Helper()
	sp := &store.Sprite{ID: store.NewID(), Name: name}
	if err := st.Create(sp); err != nil {
		t.Fatal(err)
	}
	return *sp
}

// spriteID is the ID of the sprite called name.
func spriteID(t *testing.T, st *store.Store, name string) string {
	t.Helper()
	sp, err := st.GetByName(store.Sprites, name)
	if err != nil {
		t.Fatal(err)
	}
	return sp.ID
}

// setNetPolicy is what POST /v1/sprites/{name}/policy/network does with body.
func setNetPolicy(l *Engine, name, body string) error {
	var req struct {
		Rules []store.NetworkRule `json:"rules"`
	}
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		return err
	}
	p, err := netpolicy.Compile(req.Rules)
	if err != nil {
		return err
	}
	sp, err := l.store.GetByName(store.Sprites, name)
	if err != nil {
		return err
	}
	return l.SetNetworkPolicy(sp.ID, req.Rules, p)
}

const (
	allowGithub = `{"rules":[{"domain":"github.com","action":"allow"},{"include":"defaults"}]}`
	noRules     = `{"rules":[]}`
)

func TestPolicyRoundTripAndRestrictedSet(t *testing.T) {
	helper := &fakeHelper{}
	l, st := newEgressEngine(t, helper)
	a := addSprite(t, st, "a") // 10.209.0.2
	addSprite(t, st, "b")      // 10.209.0.3

	if err := setNetPolicy(l, "a", allowGithub); err != nil {
		t.Fatalf("set: %v", err)
	}
	if got := helper.last(); !reflect.DeepEqual(got, []string{"10.209.0.2"}) {
		t.Errorf("restricted set = %v, want a's address", got)
	}
	// It is in the record as written, includes unexpanded and order kept, so it
	// survives a restart.
	reopened, _ := store.Open(filepath.Dir(filepath.Dir(st.Dir(a.ID))))
	if sp, _ := reopened.Get(a.ID); len(sp.NetworkRules) != 2 || sp.NetworkRules[0].Domain != "github.com" || sp.NetworkRules[1].Include != "defaults" {
		t.Errorf("persisted rules = %v", sp.NetworkRules)
	}

	setNetPolicy(l, "b", `{"rules":[{"domain":"*","action":"deny"}]}`)
	if got := helper.last(); !reflect.DeepEqual(got, []string{"10.209.0.2", "10.209.0.3"}) {
		t.Errorf("restricted set = %v, want both", got)
	}
	// An allow-everything policy is not a restriction: the sprite leaves the set.
	setNetPolicy(l, "b", `{"rules":[{"domain":"*","action":"allow"}]}`)
	if got := helper.last(); !reflect.DeepEqual(got, []string{"10.209.0.2"}) {
		t.Errorf("restricted set = %v, want only a", got)
	}
	if err := setNetPolicy(l, "a", noRules); err != nil {
		t.Fatalf("clear: %v", err)
	}
	if got := helper.last(); len(got) != 0 {
		t.Errorf("restricted set = %v after clearing, want empty", got)
	}
}

func TestDeletingARestrictedSpriteShrinksTheSet(t *testing.T) {
	helper := &fakeHelper{}
	l, st := newEgressEngine(t, helper)
	a := addSprite(t, st, "a")
	addSprite(t, st, "b")
	setNetPolicy(l, "a", allowGithub)
	setNetPolicy(l, "b", allowGithub)
	if err := l.Delete(a.Record); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if got := helper.last(); !reflect.DeepEqual(got, []string{"10.209.0.3"}) {
		t.Errorf("restricted set = %v, want only b", got)
	}
}

func TestRestrictivePolicyFailsClosedWithoutHelper(t *testing.T) {
	helper := &fakeHelper{err: errors.New("dial unix /run/wisp/netd.sock: connect: no such file or directory")}
	l, st := newEgressEngine(t, helper)
	a := addSprite(t, st, "a")

	if err := setNetPolicy(l, "a", allowGithub); !errors.Is(err, ErrUnenforceable) {
		t.Fatalf("got %v, want ErrUnenforceable", err)
	}
	// Refused means refused: nothing may claim the sprite is confined.
	if sp, _ := st.GetByName(store.Sprites, "a"); len(sp.NetworkRules) != 0 {
		t.Errorf("a rejected policy was stored: %v", sp.NetworkRules)
	}
	if err := l.egress.admit(a.Record); err != nil {
		t.Errorf("sprite with no policy should still get a NIC: %v", err)
	}

	// Policies that restrict nothing need no helper.
	for _, body := range []string{noRules, `{"rules":[{"domain":"*","action":"allow"}]}`} {
		if err := setNetPolicy(l, "a", body); err != nil {
			t.Errorf("%s: %v", body, err)
		}
	}
}

func TestTighteningFailureKeepsThePreviousPolicy(t *testing.T) {
	helper := &fakeHelper{}
	l, st := newEgressEngine(t, helper)
	addSprite(t, st, "a")
	setNetPolicy(l, "a", allowGithub)
	helper.fail(errors.New("helper gone"))
	if err := setNetPolicy(l, "a", `{"rules":[{"domain":"only-this.example","action":"allow"}]}`); !errors.Is(err, ErrUnenforceable) {
		t.Fatalf("got %v", err)
	}
	sp, _ := st.GetByName(store.Sprites, "a")
	if len(sp.NetworkRules) != 2 || sp.NetworkRules[0].Domain != "github.com" {
		t.Errorf("rules = %v, want the previous policy intact", sp.NetworkRules)
	}
	// The enforcer must agree with the record, not with the rejected request.
	d := &netpolicy.DNS{Enforcer: l.egress.enf, Log: quiet, Blocked: netpolicy.NonPublic, Timeout: 50 * time.Millisecond}
	if dnsRcode(d, "10.209.0.2", "github.com") == "REFUSED" || dnsRcode(d, "10.209.0.2", "only-this.example") != "REFUSED" {
		t.Error("enforcer is running the rejected policy")
	}
	if err := setNetPolicy(l, "a", noRules); err != nil {
		t.Fatalf("clearing must work without the helper: %v", err)
	}
}

func TestRestrictivePolicyRefusedWithoutGuestNetwork(t *testing.T) {
	helper := &fakeHelper{}
	l, st := newEgressEngine(t, helper)
	l.egress.gateway = netip.Addr{}
	l.egress.down = "this wispd has no guest network"
	a := addSprite(t, st, "a")
	if err := setNetPolicy(l, "a", allowGithub); !errors.Is(err, ErrUnenforceable) || !strings.Contains(err.Error(), "no guest network") {
		t.Errorf("got %v", err)
	}
	// A daemon that does not own the network must never write the shared set, which
	// belongs to the one that does: not for an unrestricted policy, not on delete.
	if err := setNetPolicy(l, "a", noRules); err != nil {
		t.Errorf("clear: %v", err)
	}
	l.Delete(a.Record)
	if len(helper.pushes) != 0 {
		t.Errorf("pushed %v", helper.pushes)
	}
}

// The NIC gate: what startLocked consults before attaching a tap.
func TestTapForFailsClosed(t *testing.T) {
	helper := &fakeHelper{}
	l, st := newEgressEngine(t, helper)
	l.freeTaps = []string{"mstap0"}
	open, shut := addSprite(t, st, "open"), addSprite(t, st, "shut")
	setNetPolicy(l, "shut", allowGithub)

	helper.fail(errors.New("helper gone"))
	if tap, err := l.tapFor(open.Record); err != nil || tap != "mstap0" {
		t.Fatalf("unrestricted sprite: tap %q err %v; the helper is none of its business", tap, err)
	}
	l.returnTap("mstap0")

	// Deliberately the stale pre-policy copy: tapFor must go by the record.
	tap, err := l.tapFor(shut.Record)
	if err != nil || tap != "" {
		t.Fatalf("restricted sprite, helper down, cold: tap %q err %v; want no NIC and no error", tap, err)
	}
	if len(l.freeTaps) != 1 {
		t.Error("the withheld tap was not returned to the pool")
	}

	// With a warm snapshot that has a NIC, refuse the wake instead of discarding memory state.
	shut.BootIP = "10.209.0.3/16"
	for _, f := range []string{"snap.vmstate", "snap.mem"} {
		os.WriteFile(filepath.Join(st.Dir(shut.ID), f), nil, 0o644)
	}
	if !vmm.HasSnapshot(st.Dir(shut.ID)) {
		t.Fatal("snapshot file names changed; update this test")
	}
	if tap, err := l.tapFor(shut.Record); err == nil || !errors.Is(err, ErrUnenforceable) || tap != "" {
		t.Errorf("warm restricted sprite, helper down: tap %q err %v; want a refusal", tap, err)
	}

	helper.fail(nil)
	if tap, err := l.tapFor(shut.Record); err != nil || tap != "mstap0" {
		t.Errorf("helper back: tap %q err %v", tap, err)
	}
	if got := helper.last(); !reflect.DeepEqual(got, []string{"10.209.0.3"}) {
		t.Errorf("boot pushed %v, want the restricted sprite's address", got)
	}
}

// End to end through the real helper protocol: SetNetworkPolicy -> egress -> unix socket ->
// netd validation -> the script nft would be given.
func TestPolicyReachesNftThroughTheRealHelper(t *testing.T) {
	socket := filepath.Join(t.TempDir(), "netd.sock")
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: socket, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	var mu sync.Mutex
	var scripts []string
	srv := &netd.Server{Net: netip.MustParsePrefix("10.209.0.0/16"), OwnerUID: os.Getuid(), Log: quiet,
		Apply: func(s string) error { mu.Lock(); scripts = append(scripts, s); mu.Unlock(); return nil }}
	go srv.Serve(ln)

	l, st := newEgressEngine(t, &fakeHelper{})
	l.egress.push = func(ctx context.Context, addrs []netip.Addr) error { return netd.Push(ctx, socket, addrs) }
	addSprite(t, st, "a")
	if err := setNetPolicy(l, "a", allowGithub); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	want := "flush set inet wisp restricted4\nadd element inet wisp restricted4 { 10.209.0.2 }\n"
	if len(scripts) != 1 || scripts[0] != want {
		t.Errorf("nft was given %q", scripts)
	}
}

// A sprite whose policy is tightened while it runs is judged by the new policy at once.
func TestSetPolicyUpdatesTheEnforcerLive(t *testing.T) {
	l, st := newEgressEngine(t, &fakeHelper{})
	addSprite(t, st, "a")
	d := &netpolicy.DNS{Enforcer: l.egress.enf, Log: quiet, Blocked: netpolicy.NonPublic, Timeout: 50 * time.Millisecond}
	refused := func(name string) bool { return dnsRcode(d, "10.209.0.2", name) == "REFUSED" }

	setNetPolicy(l, "a", allowGithub)
	if refused("github.com") || !refused("evil.com") {
		t.Error("policy not in force after POST")
	}
	setNetPolicy(l, "a", `{"rules":[{"domain":"evil.com","action":"allow"}]}`)
	if !refused("github.com") || refused("evil.com") {
		t.Error("replacement policy not in force after POST")
	}
	setNetPolicy(l, "a", noRules)
	if refused("github.com") || refused("evil.com") {
		t.Error("cleared policy still refusing")
	}
}
