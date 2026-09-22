package netpolicy

import (
	"errors"
	"log/slog"
	"net"
	"net/netip"
	"time"

	"github.com/miekg/dns"
)

// denyRcode is what a lookup the policy refuses gets. Upstream documents REFUSED
// ("fails fast rather than hanging"); resolvers treat it as final once every
// configured nameserver has said it, and all of a sprite's nameservers are us.
const denyRcode = dns.RcodeRefused

// DNS is the resolver restricted sprites are redirected to. It identifies the
// sprite by source address, answers only for names its policy allows, and
// records the addresses returned so the proxy can admit connections to them.
type DNS struct {
	Enforcer  *Enforcer
	Upstreams []string // host:port, tried in order
	Log       *slog.Logger
	// Blocked filters answers: an allowed name that resolves to a private
	// address (DNS rebinding) must not become a way into the host's networks.
	Blocked func(netip.Addr) bool
	Timeout time.Duration // per upstream attempt; 0 means 2s
}

// Start serves on addr over both UDP and TCP until stop is called, and reports
// the address it bound.
func (d *DNS) Start(addr string) (bound string, stop func(), err error) {
	pc, err := net.ListenPacket("udp4", addr)
	if err != nil {
		return "", nil, err
	}
	// The TCP listener takes the port UDP got, so addr may use port 0 in tests.
	ln, err := net.Listen("tcp4", pc.LocalAddr().String())
	if err != nil {
		pc.Close()
		return "", nil, err
	}
	udp := &dns.Server{PacketConn: pc, Handler: d}
	tcp := &dns.Server{Listener: ln, Handler: d}
	go udp.ActivateAndServe()
	go tcp.ActivateAndServe()
	return pc.LocalAddr().String(), func() { pc.Close(); ln.Close() }, nil
}

func (d *DNS) ServeDNS(w dns.ResponseWriter, r *dns.Msg) {
	rcode := func(code int) {
		m := new(dns.Msg)
		m.SetRcode(r, code)
		w.WriteMsg(m)
	}
	ap, err := netip.ParseAddrPort(w.RemoteAddr().String())
	if err != nil {
		rcode(dns.RcodeRefused)
		return
	}
	src := ap.Addr().Unmap()
	sprite, policy, ok := d.Enforcer.lookup(src)
	if !ok || r.Opcode != dns.OpcodeQuery || len(r.Question) != 1 || r.Question[0].Qclass != dns.ClassINET {
		rcode(dns.RcodeRefused)
		return
	}
	q := r.Question[0]
	if !policy.Allows(q.Name) {
		d.Log.Info("egress dns refused", "sprite", sprite, "domain", canonical(q.Name), "type", dns.TypeToString[q.Qtype])
		d.Enforcer.denied(sprite, "dns", canonical(q.Name), "not allowed by the network policy")
		rcode(denyRcode)
		return
	}
	switch q.Qtype {
	case dns.TypeAAAA:
		// Guests have no IPv6. An empty answer sends clients straight to A
		// instead of trying addresses that can only time out.
		rcode(dns.RcodeSuccess)
		return
	case dns.TypeANY, dns.TypeAXFR, dns.TypeIXFR:
		rcode(dns.RcodeRefused)
		return
	}

	resp, err := d.forward(r)
	if err != nil {
		d.Log.Warn("egress dns upstream failed", "sprite", sprite, "domain", canonical(q.Name), "err", err)
		rcode(dns.RcodeServerFailure)
		return
	}
	resp.Id = r.Id
	resp.Answer = d.filter(resp.Answer, sprite, q.Name)
	resp.Ns = d.filter(resp.Ns, sprite, q.Name)
	resp.Extra = d.filter(resp.Extra, sprite, q.Name)
	// Every A in the answer counts, whatever its owner name: a CNAME chain ends
	// at a name the policy never mentions, and it is the question that was allowed.
	for _, rr := range resp.Answer {
		if a, ok := rr.(*dns.A); ok {
			if dst, ok := netip.AddrFromSlice(a.A.To4()); ok {
				d.Enforcer.record(src, q.Name, dst, time.Duration(a.Hdr.Ttl)*time.Second)
			}
		}
	}
	if _, isUDP := w.RemoteAddr().(*net.UDPAddr); isUDP {
		size := dns.MinMsgSize
		if opt := r.IsEdns0(); opt != nil {
			size = int(opt.UDPSize())
		}
		resp.Truncate(size)
	}
	w.WriteMsg(resp)
}

// filter drops address records the sprite must not be handed.
func (d *DNS) filter(rrs []dns.RR, sprite, name string) []dns.RR {
	out := rrs[:0]
	for _, rr := range rrs {
		switch v := rr.(type) {
		case *dns.AAAA:
			continue
		case *dns.A:
			if ip, ok := netip.AddrFromSlice(v.A.To4()); !ok || d.Blocked(ip) {
				d.Log.Warn("egress dns answer stripped", "sprite", sprite, "domain", canonical(name), "addr", v.A.String(), "reason", "non-public address")
				continue
			}
		}
		out = append(out, rr)
	}
	return out
}

func (d *DNS) forward(r *dns.Msg) (*dns.Msg, error) {
	timeout := d.Timeout
	if timeout == 0 {
		timeout = 2 * time.Second
	}
	req := r.Copy()
	req.Id = dns.Id() // not the guest's choice of ID: upstream replies are matched on it
	err := errors.New("no upstream resolvers configured")
	for _, up := range d.Upstreams {
		var resp *dns.Msg
		if resp, _, err = (&dns.Client{Net: "udp", Timeout: timeout}).Exchange(req, up); err == nil && resp.Truncated {
			resp, _, err = (&dns.Client{Net: "tcp", Timeout: timeout}).Exchange(req, up)
		}
		if err == nil {
			return resp, nil
		}
	}
	return nil, err
}

// Upstreams turns the --dns flag ("1.1.1.1,8.8.8.8") into dialable addresses.
func Upstreams(list []string) []string {
	var out []string
	for _, s := range list {
		if s == "" {
			continue
		}
		if _, _, err := net.SplitHostPort(s); err != nil {
			s = net.JoinHostPort(s, "53")
		}
		out = append(out, s)
	}
	return out
}
