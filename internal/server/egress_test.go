package server

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
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

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

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

// newTestServer is a Server with a networked egress (gateway 10.209.0.1) but no
// listeners, reconcile loop or VMs.
func newTestServer(t *testing.T, helper *fakeHelper) (*Server, *store.Store) {
	t.Helper()
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	e := &egress{log: quiet, store: st, gateway: netip.MustParseAddr("10.209.0.1"), enf: netpolicy.NewEnforcer(quiet), push: helper.push}
	life := &Lifecycle{store: st, log: quiet, runtimes: map[string]*runtime{}, egress: e}
	return &Server{store: st, life: life, log: quiet, token: "t"}, st
}

func addSprite(t *testing.T, st *store.Store, name string) store.Sprite {
	t.Helper()
	sp := &store.Sprite{ID: store.NewID(), Name: name}
	if err := st.Create(sp); err != nil {
		t.Fatal(err)
	}
	return *sp
}

func call(s *Server, method, path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer t")
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	return w
}

const (
	allowGithub = `{"rules":[{"domain":"github.com","action":"allow"},{"include":"defaults"}]}`
	noRules     = `{"rules":[]}`
)

func TestPolicyRoundTripAndRestrictedSet(t *testing.T) {
	helper := &fakeHelper{}
	s, st := newTestServer(t, helper)
	a := addSprite(t, st, "a") // 10.209.0.2
	addSprite(t, st, "b")      // 10.209.0.3

	if w := call(s, "GET", "/v1/sprites/a/policy/network", ""); w.Code != 200 || strings.TrimSpace(w.Body.String()) != `{"rules":[]}` {
		t.Fatalf("new sprite: %d %s", w.Code, w.Body)
	}
	if w := call(s, "POST", "/v1/sprites/a/policy/network", allowGithub); w.Code != http.StatusNoContent {
		t.Fatalf("set: %d %s", w.Code, w.Body)
	}
	if got := helper.last(); !reflect.DeepEqual(got, []string{"10.209.0.2"}) {
		t.Errorf("restricted set = %v, want a's address", got)
	}
	// Rules come back as written: includes unexpanded, order kept.
	want := `{"rules":[{"domain":"github.com","action":"allow"},{"include":"defaults"}]}`
	if w := call(s, "GET", "/v1/sprites/a/policy/network", ""); strings.TrimSpace(w.Body.String()) != want {
		t.Errorf("get: %s", w.Body)
	}
	// It is in the record, so it survives a restart.
	reopened, _ := store.Open(filepath.Dir(filepath.Dir(st.Dir(a.ID))))
	if sp, _ := reopened.Get("a"); len(sp.NetworkRules) != 2 {
		t.Errorf("persisted rules = %v", sp.NetworkRules)
	}

	call(s, "POST", "/v1/sprites/b/policy/network", `{"rules":[{"domain":"*","action":"deny"}]}`)
	if got := helper.last(); !reflect.DeepEqual(got, []string{"10.209.0.2", "10.209.0.3"}) {
		t.Errorf("restricted set = %v, want both", got)
	}
	// An allow-everything policy is not a restriction: the sprite leaves the set.
	call(s, "POST", "/v1/sprites/b/policy/network", `{"rules":[{"domain":"*","action":"allow"}]}`)
	if got := helper.last(); !reflect.DeepEqual(got, []string{"10.209.0.2"}) {
		t.Errorf("restricted set = %v, want only a", got)
	}
	if w := call(s, "POST", "/v1/sprites/a/policy/network", noRules); w.Code != http.StatusNoContent {
		t.Fatalf("clear: %d", w.Code)
	}
	if got := helper.last(); len(got) != 0 {
		t.Errorf("restricted set = %v after clearing, want empty", got)
	}
}

func TestDeletingARestrictedSpriteShrinksTheSet(t *testing.T) {
	helper := &fakeHelper{}
	s, st := newTestServer(t, helper)
	addSprite(t, st, "a")
	addSprite(t, st, "b")
	call(s, "POST", "/v1/sprites/a/policy/network", allowGithub)
	call(s, "POST", "/v1/sprites/b/policy/network", allowGithub)
	if w := call(s, "DELETE", "/v1/sprites/a", ""); w.Code != http.StatusNoContent {
		t.Fatalf("delete: %d %s", w.Code, w.Body)
	}
	if got := helper.last(); !reflect.DeepEqual(got, []string{"10.209.0.3"}) {
		t.Errorf("restricted set = %v, want only b", got)
	}
}

func TestRestrictivePolicyFailsClosedWithoutHelper(t *testing.T) {
	helper := &fakeHelper{err: errors.New("dial unix /run/wisp/netd.sock: connect: no such file or directory")}
	s, st := newTestServer(t, helper)
	a := addSprite(t, st, "a")

	w := call(s, "POST", "/v1/sprites/a/policy/network", allowGithub)
	if w.Code != http.StatusServiceUnavailable || !strings.Contains(w.Body.String(), "policy_unenforceable") {
		t.Fatalf("got %d %s, want 503 policy_unenforceable", w.Code, w.Body)
	}
	// Refused means refused: nothing may claim the sprite is confined.
	if w := call(s, "GET", "/v1/sprites/a/policy/network", ""); strings.TrimSpace(w.Body.String()) != noRules {
		t.Errorf("a rejected policy is being reported: %s", w.Body)
	}
	if sp, _ := st.Get("a"); len(sp.NetworkRules) != 0 {
		t.Errorf("a rejected policy was stored: %v", sp.NetworkRules)
	}
	if err := s.life.egress.admit(a); err != nil {
		t.Errorf("sprite with no policy should still get a NIC: %v", err)
	}

	// Policies that restrict nothing need no helper.
	for _, body := range []string{noRules, `{"rules":[{"domain":"*","action":"allow"}]}`} {
		if w := call(s, "POST", "/v1/sprites/a/policy/network", body); w.Code != http.StatusNoContent {
			t.Errorf("%s: %d %s", body, w.Code, w.Body)
		}
	}
}

func TestTighteningFailureKeepsThePreviousPolicy(t *testing.T) {
	helper := &fakeHelper{}
	s, st := newTestServer(t, helper)
	addSprite(t, st, "a")
	call(s, "POST", "/v1/sprites/a/policy/network", allowGithub)
	helper.fail(errors.New("helper gone"))
	if w := call(s, "POST", "/v1/sprites/a/policy/network", `{"rules":[{"domain":"only-this.example","action":"allow"}]}`); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("got %d", w.Code)
	}
	sp, _ := st.Get("a")
	if len(sp.NetworkRules) != 2 || sp.NetworkRules[0].Domain != "github.com" {
		t.Errorf("rules = %v, want the previous policy intact", sp.NetworkRules)
	}
	// The enforcer must agree with the record, not with the rejected request.
	d := &netpolicy.DNS{Enforcer: s.life.egress.enf, Log: quiet, Blocked: netpolicy.NonPublic, Timeout: 50 * time.Millisecond}
	if dnsRcode(d, "10.209.0.2", "github.com") == "REFUSED" || dnsRcode(d, "10.209.0.2", "only-this.example") != "REFUSED" {
		t.Error("enforcer is running the rejected policy")
	}
	if w := call(s, "POST", "/v1/sprites/a/policy/network", noRules); w.Code != http.StatusNoContent {
		t.Fatalf("clearing must work without the helper: %d", w.Code)
	}
}

func TestRestrictivePolicyRefusedWithoutGuestNetwork(t *testing.T) {
	helper := &fakeHelper{}
	s, st := newTestServer(t, helper)
	s.life.egress.gateway = netip.Addr{}
	s.life.egress.down = "this wispd has no guest network"
	addSprite(t, st, "a")
	w := call(s, "POST", "/v1/sprites/a/policy/network", allowGithub)
	if w.Code != http.StatusServiceUnavailable || !strings.Contains(w.Body.String(), "no guest network") {
		t.Errorf("got %d %s", w.Code, w.Body)
	}
	// A daemon that does not own the network must never write the shared set, which
	// belongs to the one that does: not for an unrestricted policy, not on delete.
	if w := call(s, "POST", "/v1/sprites/a/policy/network", noRules); w.Code != http.StatusNoContent {
		t.Errorf("clear: %d", w.Code)
	}
	call(s, "DELETE", "/v1/sprites/a", "")
	if len(helper.pushes) != 0 {
		t.Errorf("pushed %v", helper.pushes)
	}
}

func TestPolicyValidationAndLookup(t *testing.T) {
	s, st := newTestServer(t, &fakeHelper{})
	addSprite(t, st, "a")
	for body, want := range map[string]int{
		`{"rules":[{"domain":"a.com","action":"permit"}]}`:  400,
		`{"rules":[{"include":"everything"}]}`:              400,
		`{"rules":[{"domain":"a.*.com","action":"allow"}]}`: 400,
		`not json`: 400,
	} {
		if w := call(s, "POST", "/v1/sprites/a/policy/network", body); w.Code != want {
			t.Errorf("%s: got %d, want %d", body, w.Code, want)
		}
	}
	if w := call(s, "GET", "/v1/sprites/nope/policy/network", ""); w.Code != 404 {
		t.Errorf("get unknown: %d", w.Code)
	}
	if w := call(s, "POST", "/v1/sprites/nope/policy/network", allowGithub); w.Code != 404 {
		t.Errorf("set unknown: %d", w.Code)
	}
}

// The NIC gate: what startLocked consults before attaching a tap.
func TestTapForFailsClosed(t *testing.T) {
	helper := &fakeHelper{}
	s, st := newTestServer(t, helper)
	l := s.life
	l.freeTaps = []string{"mstap0"}
	open, shut := addSprite(t, st, "open"), addSprite(t, st, "shut")
	call(s, "POST", "/v1/sprites/shut/policy/network", allowGithub)

	helper.fail(errors.New("helper gone"))
	if tap, err := l.tapFor(open); err != nil || tap != "mstap0" {
		t.Fatalf("unrestricted sprite: tap %q err %v; the helper is none of its business", tap, err)
	}
	l.returnTap("mstap0")

	// Deliberately the stale pre-policy copy: tapFor must go by the record.
	tap, err := l.tapFor(shut)
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
	if tap, err := l.tapFor(shut); err == nil || !errors.Is(err, errUnenforceable) || tap != "" {
		t.Errorf("warm restricted sprite, helper down: tap %q err %v; want a refusal", tap, err)
	}

	helper.fail(nil)
	if tap, err := l.tapFor(shut); err != nil || tap != "mstap0" {
		t.Errorf("helper back: tap %q err %v", tap, err)
	}
	if got := helper.last(); !reflect.DeepEqual(got, []string{"10.209.0.3"}) {
		t.Errorf("boot pushed %v, want the restricted sprite's address", got)
	}
}

// End to end through the real helper protocol: API -> egress -> unix socket ->
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

	s, st := newTestServer(t, nil)
	s.life.egress.push = func(ctx context.Context, addrs []netip.Addr) error { return netd.Push(ctx, socket, addrs) }
	addSprite(t, st, "a")
	if w := call(s, "POST", "/v1/sprites/a/policy/network", allowGithub); w.Code != http.StatusNoContent {
		t.Fatalf("%d %s", w.Code, w.Body)
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
	s, st := newTestServer(t, &fakeHelper{})
	addSprite(t, st, "a")
	d := &netpolicy.DNS{Enforcer: s.life.egress.enf, Log: quiet, Blocked: netpolicy.NonPublic, Timeout: 50 * time.Millisecond}
	refused := func(name string) bool { return dnsRcode(d, "10.209.0.2", name) == "REFUSED" }

	call(s, "POST", "/v1/sprites/a/policy/network", allowGithub)
	if refused("github.com") || !refused("evil.com") {
		t.Error("policy not in force after POST")
	}
	call(s, "POST", "/v1/sprites/a/policy/network", `{"rules":[{"domain":"evil.com","action":"allow"}]}`)
	if !refused("github.com") || refused("evil.com") {
		t.Error("replacement policy not in force after POST")
	}
	call(s, "POST", "/v1/sprites/a/policy/network", noRules)
	if refused("github.com") || refused("evil.com") {
		t.Error("cleared policy still refusing")
	}
}
