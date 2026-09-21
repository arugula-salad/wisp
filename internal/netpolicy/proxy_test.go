package netpolicy

import (
	"bytes"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

var loopback = netip.MustParseAddr("127.0.0.1")

// echoServer answers each connection with "echo:" + everything it read, which it
// can only do after seeing EOF: the reply arriving proves half-close works.
func echoServer(t *testing.T) (addr netip.AddrPort, accepted *atomic.Int32) {
	t.Helper()
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	accepted = new(atomic.Int32)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			accepted.Add(1)
			go func() {
				defer c.Close()
				b, _ := io.ReadAll(c)
				c.Write(append([]byte("echo:"), b...))
			}()
		}
	}()
	return ln.Addr().(*net.TCPAddr).AddrPort(), accepted
}

type logBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *logBuf) Write(p []byte) (int, error) { l.mu.Lock(); defer l.mu.Unlock(); return l.b.Write(p) }
func (l *logBuf) String() string              { l.mu.Lock(); defer l.mu.Unlock(); return l.b.String() }

// startProxy runs a proxy whose "original destination" is always dst. Test
// clients connect from 127.0.0.1, so that is the sprite's address here.
func startProxy(t *testing.T, e *Enforcer, dst netip.AddrPort, blocked func(netip.Addr) bool) (addr string, logs *logBuf) {
	t.Helper()
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	logs = &logBuf{}
	p := NewProxy(e, slog.New(slog.NewTextHandler(logs, nil)))
	p.OrigDst = func(*net.TCPConn) (netip.AddrPort, error) { return dst, nil }
	p.Blocked = blocked
	go p.Serve(ln)
	return ln.Addr().String(), logs
}

// roundTrip sends msg, half-closes, and returns whatever comes back.
func roundTrip(t *testing.T, proxy, msg string) (string, error) {
	t.Helper()
	c, err := net.DialTimeout("tcp4", proxy, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := c.Write([]byte(msg)); err != nil {
		return "", err
	}
	c.(*net.TCPConn).CloseWrite()
	b, err := io.ReadAll(c)
	return string(b), err
}

func nothingBlocked(netip.Addr) bool { return false }

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(3 * time.Second); !cond(); time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
	}
}

func TestProxyAllowsResolvedAddress(t *testing.T) {
	dst, accepted := echoServer(t)
	e := NewEnforcer(quiet)
	e.Set(loopback, "a", mustCompile(t, rule("example.com", "allow")))
	e.record(loopback, "example.com", dst.Addr(), time.Minute)
	proxy, _ := startProxy(t, e, dst, nothingBlocked)

	big := strings.Repeat("x", 1<<20) // more than any socket buffer, so both directions really stream
	got, err := roundTrip(t, proxy, big)
	if err != nil || got != "echo:"+big {
		t.Fatalf("got %d bytes, err %v", len(got), err)
	}
	if accepted.Load() != 1 {
		t.Errorf("server saw %d connections", accepted.Load())
	}
}

func TestProxyDeniesUnresolvedAddress(t *testing.T) {
	dst, accepted := echoServer(t)
	e := NewEnforcer(quiet)
	e.Set(loopback, "a", mustCompile(t, rule("example.com", "allow")))
	proxy, logs := startProxy(t, e, dst, nothingBlocked)

	if got, err := roundTrip(t, proxy, "hello"); got != "" || err == nil {
		t.Errorf("got %q, err %v; want a reset", got, err)
	}
	waitFor(t, "denial log", func() bool { return strings.Contains(logs.String(), "egress denied") })
	for _, want := range []string{"sprite=a", "dst=" + dst.String(), "not resolved from an allowed domain"} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("denial log lacks %q: %s", want, logs.String())
		}
	}
	if accepted.Load() != 0 {
		t.Error("a denied connection reached the server")
	}
}

func TestProxyDeniesPrivateAddressEvenIfResolved(t *testing.T) {
	dst, accepted := echoServer(t) // on 127.0.0.1: loopback, so always off limits
	e := NewEnforcer(quiet)
	e.Set(loopback, "a", mustCompile(t, rule("example.com", "allow")))
	e.record(loopback, "example.com", dst.Addr(), time.Minute)
	if _, _, denied := e.authorize(loopback, dst.Addr()); denied != "" {
		t.Fatalf("precondition: the policy itself should allow it: %s", denied)
	}
	proxy, logs := startProxy(t, e, dst, Blocked)

	if got, err := roundTrip(t, proxy, "hello"); got != "" || err == nil {
		t.Errorf("got %q, err %v; want a reset", got, err)
	}
	waitFor(t, "denial log", func() bool { return strings.Contains(logs.String(), "non-public or host address") })
	if accepted.Load() != 0 {
		t.Error("the proxy connected to a loopback address")
	}
}

func TestProxyBlocksPrivateForUnrestrictedSpritesToo(t *testing.T) {
	dst, accepted := echoServer(t)
	e := NewEnforcer(quiet)
	e.Set(loopback, "a", mustCompile(t)) // no policy at all
	proxy, _ := startProxy(t, e, dst, Blocked)
	if got, _ := roundTrip(t, proxy, "hello"); got != "" {
		t.Errorf("got %q", got)
	}
	if accepted.Load() != 0 {
		t.Error("the proxy connected to a loopback address for an unrestricted sprite")
	}
}

func TestProxyPassesUnrestrictedSprite(t *testing.T) {
	dst, _ := echoServer(t)
	e := NewEnforcer(quiet)
	e.Set(loopback, "a", mustCompile(t))
	proxy, _ := startProxy(t, e, dst, nothingBlocked)
	if got, err := roundTrip(t, proxy, "hi"); err != nil || got != "echo:hi" {
		t.Errorf("got %q, err %v", got, err)
	}
}

func TestProxyDeniesUnknownSource(t *testing.T) {
	dst, accepted := echoServer(t)
	proxy, logs := startProxy(t, NewEnforcer(quiet), dst, nothingBlocked)
	if got, _ := roundTrip(t, proxy, "hello"); got != "" {
		t.Errorf("got %q", got)
	}
	waitFor(t, "denial log", func() bool { return strings.Contains(logs.String(), "not a known sprite") })
	if accepted.Load() != 0 {
		t.Error("connection from an unknown source was proxied")
	}
}

func TestProxyDeniesExpiredAddress(t *testing.T) {
	dst, _ := echoServer(t)
	e := NewEnforcer(quiet)
	c := &clock{t: time.Unix(1_700_000_000, 0)}
	e.now = c.now
	e.Set(loopback, "a", mustCompile(t, rule("example.com", "allow")))
	e.record(loopback, "example.com", dst.Addr(), 10*time.Second)
	proxy, _ := startProxy(t, e, dst, nothingBlocked)
	if got, err := roundTrip(t, proxy, "hi"); err != nil || got != "echo:hi" {
		t.Fatalf("before expiry: got %q, err %v", got, err)
	}
	c.advance(10*time.Second + ttlGrace + time.Second)
	if got, _ := roundTrip(t, proxy, "hi"); got != "" {
		t.Errorf("after expiry: got %q", got)
	}
}

func TestProxyPolicyChangeClosesLiveConnections(t *testing.T) {
	dst, _ := echoServer(t)
	e := NewEnforcer(quiet)
	e.Set(loopback, "a", mustCompile(t, rule("example.com", "allow"), rule("other.com", "allow")))
	e.record(loopback, "example.com", dst.Addr(), time.Minute)
	proxy, _ := startProxy(t, e, dst, nothingBlocked)

	c, err := net.Dial("tcp4", proxy)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.Write([]byte("partial"))
	flows := func() int {
		e.mu.Lock()
		defer e.mu.Unlock()
		return len(e.sprites[loopback].flows)
	}
	waitFor(t, "flow to be tracked", func() bool { return flows() == 1 })

	// A change that keeps the authorising name leaves the connection alone.
	e.Set(loopback, "a", mustCompile(t, rule("example.com", "allow")))
	if flows() != 1 {
		t.Fatal("connection closed by a policy change that still allows it")
	}
	e.Set(loopback, "a", mustCompile(t, rule("other.com", "allow")))
	c.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := c.Read(make([]byte, 1)); err == nil || strings.Contains(err.Error(), "timeout") {
		t.Errorf("connection to a newly refused domain stayed open (err %v)", err)
	}
	waitFor(t, "flow to be untracked", func() bool { return flows() == 0 })
}

func TestProxyRemoveClosesConnections(t *testing.T) {
	dst, _ := echoServer(t)
	e := NewEnforcer(quiet)
	e.Set(loopback, "a", mustCompile(t))
	proxy, _ := startProxy(t, e, dst, nothingBlocked)
	c, err := net.Dial("tcp4", proxy)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	waitFor(t, "flow to be tracked", func() bool {
		e.mu.Lock()
		defer e.mu.Unlock()
		return len(e.sprites[loopback].flows) == 1
	})
	e.Remove(loopback)
	c.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := c.Read(make([]byte, 1)); err == nil || strings.Contains(err.Error(), "timeout") {
		t.Errorf("connection survived its sprite (err %v)", err)
	}
}

// A connection that was never redirected has no original destination; the real
// lookup must say so rather than invent one.
func TestOriginalDstWithoutRedirect(t *testing.T) {
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go net.Dial("tcp4", ln.Addr().String())
	c, err := ln.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if dst, err := OriginalDst(c.(*net.TCPConn)); err == nil && dst != ln.Addr().(*net.TCPAddr).AddrPort() {
		t.Errorf("unredirected connection reported a foreign original destination %s", dst)
	}
}

func TestProxyDeniesDirectConnections(t *testing.T) {
	e := NewEnforcer(quiet)
	e.Set(loopback, "a", mustCompile(t))
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	logs := &logBuf{}
	p := NewProxy(e, slog.New(slog.NewTextHandler(logs, nil))) // real OrigDst, real Blocked
	go p.Serve(ln)
	if got, _ := roundTrip(t, ln.Addr().String(), "hello"); got != "" {
		t.Errorf("got %q", got)
	}
	waitFor(t, "denial log", func() bool { return strings.Contains(logs.String(), "egress denied") })
}

func TestNonPublic(t *testing.T) {
	for addr, want := range map[string]bool{
		"10.209.0.2": true, "10.88.0.1": true, "172.16.0.1": true, "172.31.255.255": true, "192.168.1.1": true,
		"127.0.0.1": true, "127.8.8.8": true, "169.254.169.254": true, "100.64.0.1": true, "100.127.255.255": true,
		"224.0.0.251": true, "239.255.255.250": true, "255.255.255.255": true, "0.0.0.0": true, "::1": true, "2606:4700::1111": true,
		"::ffff:10.0.0.1": true,
		"1.1.1.1":         false, "8.8.8.8": false, "140.82.112.3": false, "172.32.0.1": false, "100.128.0.1": false, "::ffff:1.1.1.1": false,
	} {
		if got := NonPublic(netip.MustParseAddr(addr)); got != want {
			t.Errorf("NonPublic(%s) = %v, want %v", addr, got, want)
		}
	}
}

func TestBlockedIncludesHostAddresses(t *testing.T) {
	addrs, _ := net.InterfaceAddrs()
	for _, a := range addrs {
		if n, ok := a.(*net.IPNet); ok && n.IP.To4() != nil {
			ip, _ := netip.AddrFromSlice(n.IP.To4())
			if !Blocked(ip) {
				t.Errorf("host address %s is not blocked", ip)
			}
		}
	}
}
