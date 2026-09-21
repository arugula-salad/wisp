package netpolicy

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

// Proxy is the transparent TCP proxy every connection from a restricted sprite
// is redirected to. It connects onward only to addresses the sprite's allowed
// DNS lookups produced.
type Proxy struct {
	Enforcer *Enforcer
	Log      *slog.Logger
	// OrigDst recovers where the sprite was actually connecting to before the
	// kernel redirected it here.
	OrigDst func(*net.TCPConn) (netip.AddrPort, error)
	// Blocked is checked for every destination regardless of policy. Our onward
	// connection leaves from the host, so the msbr0 forward-chain rules that keep
	// sprites out of private ranges never see it: this check is all there is.
	Blocked func(netip.Addr) bool
	Dial    func(network, addr string, timeout time.Duration) (net.Conn, error)
}

func NewProxy(e *Enforcer, log *slog.Logger) *Proxy {
	return &Proxy{Enforcer: e, Log: log, OrigDst: OriginalDst, Blocked: Blocked, Dial: net.DialTimeout}
}

func (p *Proxy) Serve(ln net.Listener) error {
	for {
		c, err := ln.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return nil
			}
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				continue
			}
			// Typically EMFILE. Back off instead of spinning; existing flows are unaffected.
			p.Log.Error("egress proxy accept failed", "err", err)
			time.Sleep(100 * time.Millisecond)
			continue
		}
		go p.handle(c.(*net.TCPConn))
	}
}

func (p *Proxy) handle(in *net.TCPConn) {
	defer in.Close()
	src := in.RemoteAddr().(*net.TCPAddr).AddrPort().Addr().Unmap()
	deny := func(sprite string, dst any, reason string) {
		p.Log.Warn("egress denied", "sprite", sprite, "src", src, "dst", dst, "reason", reason)
		in.SetLinger(0) // reset rather than FIN, so the guest sees a refusal, not an empty reply
	}
	dst, err := p.OrigDst(in)
	if err != nil {
		// Not a redirected connection: something dialed the proxy port directly.
		deny("", "unknown", "no original destination: "+err.Error())
		return
	}
	if p.Blocked(dst.Addr()) {
		sprite, _, _ := p.Enforcer.lookup(src)
		deny(sprite, dst, "non-public or host address")
		return
	}
	sprite, via, denied := p.Enforcer.authorize(src, dst.Addr())
	if denied != "" {
		deny(sprite, dst, denied)
		return
	}
	out, err := p.Dial("tcp4", dst.String(), 10*time.Second)
	if err != nil {
		p.Log.Info("egress connect failed", "sprite", sprite, "dst", dst, "err", err)
		in.SetLinger(0)
		return
	}
	defer out.Close()
	untrack, ok := p.Enforcer.track(src, via, closers{in, out})
	if !ok {
		deny(sprite, dst, "policy changed while connecting")
		return
	}
	defer untrack()
	splice(in, out)
}

type closers []io.Closer

func (c closers) Close() error {
	for _, x := range c {
		x.Close()
	}
	return nil
}

// splice copies both ways until both directions have ended. One side finishing
// its writes (FIN) is passed on as a half-close, not a teardown: the other
// direction may still have a response in flight.
func splice(a, b net.Conn) {
	var wg sync.WaitGroup
	pipe := func(dst, src net.Conn) {
		defer wg.Done()
		_, err := io.Copy(dst, src)
		if cw, ok := dst.(interface{ CloseWrite() error }); ok && err == nil {
			cw.CloseWrite()
			return
		}
		// A broken direction cannot be half-closed meaningfully; end the whole flow.
		dst.Close()
		src.Close()
	}
	wg.Add(2)
	go pipe(a, b)
	go pipe(b, a)
	wg.Wait()
}

// OriginalDst reads SO_ORIGINAL_DST, the pre-redirect destination conntrack
// keeps for a NATed connection.
func OriginalDst(c *net.TCPConn) (netip.AddrPort, error) {
	raw, err := c.SyscallConn()
	if err != nil {
		return netip.AddrPort{}, err
	}
	var dst netip.AddrPort
	var serr error
	err = raw.Control(func(fd uintptr) {
		// There is no typed getsockopt for a sockaddr_in. IPv6Mreq is simply a
		// struct of at least that size whose leading bytes are addressable.
		var mreq *unix.IPv6Mreq
		if mreq, serr = unix.GetsockoptIPv6Mreq(int(fd), unix.SOL_IP, unix.SO_ORIGINAL_DST); serr != nil {
			return
		}
		b := mreq.Multiaddr // sockaddr_in: family[0:2] port[2:4] addr[4:8]
		dst = netip.AddrPortFrom(netip.AddrFrom4([4]byte(b[4:8])), uint16(b[2])<<8|uint16(b[3]))
	})
	if err == nil {
		err = serr
	}
	if err == nil && !dst.IsValid() {
		err = fmt.Errorf("invalid original destination")
	}
	return dst, err
}
