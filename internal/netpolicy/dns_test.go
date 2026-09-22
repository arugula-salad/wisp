package netpolicy

import (
	"io"
	"log/slog"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/miekg/dns"
)

var (
	spriteA = netip.MustParseAddr("10.209.0.2")
	spriteB = netip.MustParseAddr("10.209.0.3")
	quiet   = slog.New(slog.NewTextHandler(io.Discard, nil))
)

// upstream is a fake recursive resolver serving fixed zone text, counting queries.
type upstream struct {
	addr    string
	queries atomic.Int32
}

func startUpstream(t *testing.T, zone map[string][]string) *upstream {
	t.Helper()
	u := &upstream{}
	h := dns.HandlerFunc(func(w dns.ResponseWriter, r *dns.Msg) {
		u.queries.Add(1)
		m := new(dns.Msg)
		m.SetReply(r)
		records, ok := zone[r.Question[0].Name]
		if !ok {
			m.Rcode = dns.RcodeNameError
		}
		for _, s := range records {
			rr, err := dns.NewRR(s)
			if err != nil {
				t.Errorf("bad zone record %q: %v", s, err)
				continue
			}
			if rr.Header().Rrtype == r.Question[0].Qtype || rr.Header().Rrtype == dns.TypeCNAME || rr.Header().Name != r.Question[0].Name {
				m.Answer = append(m.Answer, rr)
			}
		}
		w.WriteMsg(m)
	})
	pc, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &dns.Server{PacketConn: pc, Handler: h}
	go srv.ActivateAndServe()
	t.Cleanup(func() { pc.Close() })
	u.addr = pc.LocalAddr().String()
	return u
}

// fakeWriter lets a test choose the source address, which is how the listener tells sprites apart.
type fakeWriter struct {
	src netip.Addr
	tcp bool
	msg *dns.Msg
}

func (w *fakeWriter) RemoteAddr() net.Addr {
	if w.tcp {
		return &net.TCPAddr{IP: w.src.AsSlice(), Port: 40000}
	}
	return &net.UDPAddr{IP: w.src.AsSlice(), Port: 40000}
}
func (w *fakeWriter) LocalAddr() net.Addr {
	return &net.UDPAddr{IP: net.IPv4(10, 209, 0, 1), Port: 7853}
}
func (w *fakeWriter) WriteMsg(m *dns.Msg) error { w.msg = m; return nil }
func (w *fakeWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }
func (w *fakeWriter) Close() error              { return nil }
func (w *fakeWriter) TsigStatus() error         { return nil }
func (w *fakeWriter) TsigTimersOnly(bool)       {}
func (w *fakeWriter) Hijack()                   {}

func query(d *DNS, src netip.Addr, name string, qtype uint16) *dns.Msg {
	q := new(dns.Msg)
	q.SetQuestion(dns.Fqdn(name), qtype)
	w := &fakeWriter{src: src}
	d.ServeDNS(w, q)
	return w.msg
}

func answers(m *dns.Msg) []string {
	var out []string
	for _, rr := range m.Answer {
		if a, ok := rr.(*dns.A); ok {
			out = append(out, a.A.String())
		}
	}
	return out
}

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time          { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) advance(d time.Duration) { c.mu.Lock(); c.t = c.t.Add(d); c.mu.Unlock() }

func newTestDNS(t *testing.T, zone map[string][]string) (*DNS, *Enforcer, *upstream, *clock) {
	t.Helper()
	up := startUpstream(t, zone)
	e := NewEnforcer(quiet)
	c := &clock{t: time.Unix(1_700_000_000, 0)}
	e.now = c.now
	return &DNS{Enforcer: e, Upstreams: []string{up.addr}, Log: quiet, Blocked: NonPublic, Timeout: time.Second}, e, up, c
}

func TestDNSAllowedNameResolvesAndIsRemembered(t *testing.T) {
	d, e, up, _ := newTestDNS(t, map[string][]string{"github.com.": {"github.com. 300 IN A 140.82.112.3"}})
	e.Set(spriteA, "a", mustCompile(t, rule("github.com", "allow")))

	m := query(d, spriteA, "github.com", dns.TypeA)
	if m.Rcode != dns.RcodeSuccess || len(answers(m)) != 1 || answers(m)[0] != "140.82.112.3" {
		t.Fatalf("got rcode %s answers %v", dns.RcodeToString[m.Rcode], answers(m))
	}
	if _, via, denied := e.authorize(spriteA, netip.MustParseAddr("140.82.112.3")); denied != "" || via != "github.com" {
		t.Errorf("resolved address not authorised: via=%q denied=%q", via, denied)
	}
	if up.queries.Load() != 1 {
		t.Errorf("upstream saw %d queries, want 1", up.queries.Load())
	}
}

func TestDNSDeniedNameIsRefusedWithoutAskingUpstream(t *testing.T) {
	d, e, up, _ := newTestDNS(t, map[string][]string{"evil.com.": {"evil.com. 300 IN A 93.184.216.34"}})
	e.Set(spriteA, "a", mustCompile(t, rule("github.com", "allow")))
	var heard []string
	e.OnDeny = func(sprite, kind, target, _ string) { heard = append(heard, sprite+" "+kind+" "+target) }

	for _, qtype := range []uint16{dns.TypeA, dns.TypeAAAA, dns.TypeTXT} {
		m := query(d, spriteA, "evil.com", qtype)
		if m.Rcode != dns.RcodeRefused || len(m.Answer) != 0 {
			t.Errorf("%s: got rcode %s with %d answers, want REFUSED", dns.TypeToString[qtype], dns.RcodeToString[m.Rcode], len(m.Answer))
		}
	}
	if up.queries.Load() != 0 {
		t.Errorf("a refused name reached upstream (%d queries): that is an exfiltration channel", up.queries.Load())
	}
	if _, _, denied := e.authorize(spriteA, netip.MustParseAddr("93.184.216.34")); denied == "" {
		t.Error("address of a refused name was authorised")
	}
	if len(heard) != 3 || heard[0] != "a dns evil.com" {
		t.Errorf("OnDeny heard %q", heard)
	}
}

func TestDNSUnknownSourceIsRefused(t *testing.T) {
	d, _, up, _ := newTestDNS(t, map[string][]string{"github.com.": {"github.com. 300 IN A 140.82.112.3"}})
	if m := query(d, spriteA, "github.com", dns.TypeA); m.Rcode != dns.RcodeRefused {
		t.Errorf("got %s, want REFUSED", dns.RcodeToString[m.Rcode])
	}
	if up.queries.Load() != 0 {
		t.Error("query from an unknown source was forwarded")
	}
}

func TestDNSAAAAIsEmptyNoError(t *testing.T) {
	d, e, up, _ := newTestDNS(t, map[string][]string{"github.com.": {"github.com. 300 IN AAAA 2606:50c0::1"}})
	e.Set(spriteA, "a", mustCompile(t, rule("github.com", "allow")))
	m := query(d, spriteA, "github.com", dns.TypeAAAA)
	if m.Rcode != dns.RcodeSuccess || len(m.Answer) != 0 {
		t.Errorf("got rcode %s with %d answers, want empty NOERROR", dns.RcodeToString[m.Rcode], len(m.Answer))
	}
	if up.queries.Load() != 0 {
		t.Error("AAAA should be answered locally")
	}
}

func TestDNSRebindingAnswersAreStripped(t *testing.T) {
	d, e, _, _ := newTestDNS(t, map[string][]string{"rebind.example.com.": {
		"rebind.example.com. 60 IN A 10.0.0.5",
		"rebind.example.com. 60 IN A 127.0.0.1",
		"rebind.example.com. 60 IN A 169.254.169.254",
		"rebind.example.com. 60 IN A 100.100.100.100",
		"rebind.example.com. 60 IN A 192.168.1.1",
		"rebind.example.com. 60 IN A 172.20.0.1",
		"rebind.example.com. 60 IN A 93.184.216.34",
	}})
	e.Set(spriteA, "a", mustCompile(t, rule("*.example.com", "allow")))
	m := query(d, spriteA, "rebind.example.com", dns.TypeA)
	if got := answers(m); len(got) != 1 || got[0] != "93.184.216.34" {
		t.Errorf("answers = %v, want only the public address", got)
	}
	for _, private := range []string{"10.0.0.5", "127.0.0.1", "169.254.169.254", "100.100.100.100", "192.168.1.1", "172.20.0.1"} {
		if _, _, denied := e.authorize(spriteA, netip.MustParseAddr(private)); denied == "" {
			t.Errorf("%s became an allowed destination", private)
		}
	}
}

func TestDNSCNAMEChainAuthorisesTheFinalAddress(t *testing.T) {
	d, e, _, _ := newTestDNS(t, map[string][]string{"www.example.com.": {
		"www.example.com. 300 IN CNAME edge.cdn.net.",
		"edge.cdn.net. 30 IN A 151.101.1.1",
	}})
	e.Set(spriteA, "a", mustCompile(t, rule("www.example.com", "allow")))
	if m := query(d, spriteA, "www.example.com", dns.TypeA); len(m.Answer) != 2 {
		t.Fatalf("want CNAME + A, got %v", m.Answer)
	}
	if _, via, denied := e.authorize(spriteA, netip.MustParseAddr("151.101.1.1")); denied != "" || via != "www.example.com" {
		t.Errorf("via=%q denied=%q", via, denied)
	}
	// The CNAME target itself was never allowed.
	if m := query(d, spriteA, "edge.cdn.net", dns.TypeA); m.Rcode != dns.RcodeRefused {
		t.Errorf("CNAME target resolved directly: %s", dns.RcodeToString[m.Rcode])
	}
}

func TestDNSAllowedAddressExpiresWithTTL(t *testing.T) {
	d, e, _, c := newTestDNS(t, map[string][]string{"github.com.": {"github.com. 30 IN A 140.82.112.3"}})
	e.Set(spriteA, "a", mustCompile(t, rule("github.com", "allow")))
	dst := netip.MustParseAddr("140.82.112.3")
	query(d, spriteA, "github.com", dns.TypeA)

	c.advance(30*time.Second + ttlGrace - time.Second)
	if _, _, denied := e.authorize(spriteA, dst); denied != "" {
		t.Errorf("denied inside TTL + grace: %s", denied)
	}
	c.advance(2 * time.Second)
	if _, _, denied := e.authorize(spriteA, dst); denied == "" {
		t.Error("still authorised after TTL + grace")
	}
	query(d, spriteA, "github.com", dns.TypeA) // a fresh lookup renews it
	if _, _, denied := e.authorize(spriteA, dst); denied != "" {
		t.Errorf("not renewed by a new lookup: %s", denied)
	}
}

func TestDNSAllowedSetsArePerSprite(t *testing.T) {
	d, e, _, _ := newTestDNS(t, map[string][]string{
		"github.com.": {"github.com. 300 IN A 140.82.112.3"},
		"pypi.org.":   {"pypi.org. 300 IN A 151.101.0.223"},
	})
	e.Set(spriteA, "a", mustCompile(t, rule("github.com", "allow")))
	e.Set(spriteB, "b", mustCompile(t, rule("pypi.org", "allow")))
	query(d, spriteA, "github.com", dns.TypeA)
	query(d, spriteB, "pypi.org", dns.TypeA)

	gh, py := netip.MustParseAddr("140.82.112.3"), netip.MustParseAddr("151.101.0.223")
	if _, _, denied := e.authorize(spriteA, gh); denied != "" {
		t.Errorf("a -> github: %s", denied)
	}
	if _, _, denied := e.authorize(spriteB, py); denied != "" {
		t.Errorf("b -> pypi: %s", denied)
	}
	if _, _, denied := e.authorize(spriteB, gh); denied == "" {
		t.Error("b may reach an address only a resolved")
	}
	if _, _, denied := e.authorize(spriteA, py); denied == "" {
		t.Error("a may reach an address only b resolved")
	}
	if m := query(d, spriteB, "github.com", dns.TypeA); m.Rcode != dns.RcodeRefused {
		t.Error("b resolved a name only a's policy allows")
	}
}

func TestDNSPolicyChangeRevokesLearnedAddresses(t *testing.T) {
	d, e, _, _ := newTestDNS(t, map[string][]string{
		"github.com.": {"github.com. 300 IN A 140.82.112.3"},
		"pypi.org.":   {"pypi.org. 300 IN A 151.101.0.223"},
	})
	e.Set(spriteA, "a", mustCompile(t, rule("github.com", "allow"), rule("pypi.org", "allow")))
	query(d, spriteA, "github.com", dns.TypeA)
	query(d, spriteA, "pypi.org", dns.TypeA)

	e.Set(spriteA, "a", mustCompile(t, rule("pypi.org", "allow")))
	if _, _, denied := e.authorize(spriteA, netip.MustParseAddr("140.82.112.3")); denied == "" {
		t.Error("address of a newly refused name is still authorised")
	}
	if _, _, denied := e.authorize(spriteA, netip.MustParseAddr("151.101.0.223")); denied != "" {
		t.Errorf("address of a still-allowed name was revoked: %s", denied)
	}

	// The address passing to a different sprite carries nothing over.
	e.Set(spriteA, "someone-else", mustCompile(t, rule("pypi.org", "allow")))
	if _, _, denied := e.authorize(spriteA, netip.MustParseAddr("151.101.0.223")); denied == "" {
		t.Error("a reused address inherited the previous sprite's allowed set")
	}
}

func TestDNSUnrestrictedSpriteResolvesAnything(t *testing.T) {
	// Reached only while the kernel set lags a cleared policy; it must not break the sprite.
	d, e, _, _ := newTestDNS(t, map[string][]string{"anything.org.": {"anything.org. 300 IN A 93.184.216.34"}})
	e.Set(spriteA, "a", mustCompile(t))
	if m := query(d, spriteA, "anything.org", dns.TypeA); len(answers(m)) != 1 {
		t.Errorf("got %v", m)
	}
}

func TestDNSUpstreamFailureIsServfail(t *testing.T) {
	e := NewEnforcer(quiet)
	e.Set(spriteA, "a", mustCompile(t, rule("github.com", "allow")))
	pc, _ := net.ListenPacket("udp4", "127.0.0.1:0") // bound, never answers
	defer pc.Close()
	d := &DNS{Enforcer: e, Upstreams: []string{pc.LocalAddr().String()}, Log: quiet, Blocked: NonPublic, Timeout: 50 * time.Millisecond}
	if m := query(d, spriteA, "github.com", dns.TypeA); m.Rcode != dns.RcodeServerFailure {
		t.Errorf("got %s, want SERVFAIL", dns.RcodeToString[m.Rcode])
	}
}

func TestDNSFallsBackToSecondUpstream(t *testing.T) {
	d, e, up, _ := newTestDNS(t, map[string][]string{"github.com.": {"github.com. 300 IN A 140.82.112.3"}})
	dead, _ := net.ListenPacket("udp4", "127.0.0.1:0")
	defer dead.Close()
	d.Upstreams = []string{dead.LocalAddr().String(), up.addr}
	d.Timeout = 50 * time.Millisecond
	e.Set(spriteA, "a", mustCompile(t, rule("github.com", "allow")))
	if m := query(d, spriteA, "github.com", dns.TypeA); len(answers(m)) != 1 {
		t.Errorf("got %v", m)
	}
}

// Over real sockets, UDP and TCP, with the sprite identified by the packet's source address.
func TestDNSOverTheWire(t *testing.T) {
	d, e, _, _ := newTestDNS(t, map[string][]string{"github.com.": {"github.com. 300 IN A 140.82.112.3"}})
	addr, stop, err := d.Start("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	e.Set(netip.MustParseAddr("127.0.0.1"), "local", mustCompile(t, rule("github.com", "allow")))
	for _, network := range []string{"udp", "tcp"} {
		for name, want := range map[string]int{"github.com.": dns.RcodeSuccess, "evil.com.": dns.RcodeRefused} {
			q := new(dns.Msg)
			q.SetQuestion(name, dns.TypeA)
			m, _, err := (&dns.Client{Net: network, Timeout: 2 * time.Second}).Exchange(q, addr)
			if err != nil {
				t.Fatalf("%s %s: %v", network, name, err)
			}
			if m.Rcode != want {
				t.Errorf("%s %s: rcode %s, want %s", network, name, dns.RcodeToString[m.Rcode], dns.RcodeToString[want])
			}
		}
	}
}

func TestUpstreams(t *testing.T) {
	got := Upstreams([]string{"1.1.1.1", "", "8.8.8.8:5353"})
	if len(got) != 2 || got[0] != "1.1.1.1:53" || got[1] != "8.8.8.8:5353" {
		t.Errorf("got %v", got)
	}
}
