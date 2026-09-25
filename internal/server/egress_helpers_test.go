package server

import (
	"io"
	"net"
	"net/netip"

	"github.com/miekg/dns"

	"github.com/arugula-salad/wisp/internal/netpolicy"
)

type rcodeWriter struct {
	src   netip.Addr
	rcode int
}

func (w *rcodeWriter) RemoteAddr() net.Addr      { return &net.UDPAddr{IP: w.src.AsSlice(), Port: 40000} }
func (w *rcodeWriter) LocalAddr() net.Addr       { return &net.UDPAddr{} }
func (w *rcodeWriter) WriteMsg(m *dns.Msg) error { w.rcode = m.Rcode; return nil }
func (w *rcodeWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }
func (w *rcodeWriter) Close() error              { return nil }
func (w *rcodeWriter) TsigStatus() error         { return nil }
func (w *rcodeWriter) TsigTimersOnly(bool)       {}
func (w *rcodeWriter) Hijack()                   {}

// dnsRcode asks the policy DNS listener, as the sprite at src, for name. With no
// upstream configured an allowed name comes back SERVFAIL; a refused one REFUSED.
func dnsRcode(d *netpolicy.DNS, src, name string) string {
	q := new(dns.Msg)
	q.SetQuestion(dns.Fqdn(name), dns.TypeA)
	w := &rcodeWriter{src: netip.MustParseAddr(src)}
	d.ServeDNS(w, q)
	return dns.RcodeToString[w.rcode]
}
