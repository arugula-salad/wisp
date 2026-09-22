package certs

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/miekg/dns"
	"golang.org/x/crypto/acme"
)

// loopbackZone answers every A question with 127.0.0.1 and everything else
// with an empty answer (so Pebble's CAA lookups find no restriction).
type loopbackZone struct{}

func (loopbackZone) ServeDNS(w dns.ResponseWriter, q *dns.Msg) {
	m := new(dns.Msg)
	m.SetReply(q)
	m.Authoritative = true
	if q.Question[0].Qtype == dns.TypeA {
		m.Answer = append(m.Answer, &dns.A{Hdr: dns.RR_Header{Name: q.Question[0].Name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 1}, A: net.IPv4(127, 0, 0, 1)})
	}
	w.WriteMsg(m)
}

// fallbackCert is what the wildcard source serves in these tests.
func fallbackCert(t *testing.T) *tls.Certificate {
	certFile, keyFile := selfSigned(t, t.TempDir(), "*.widgets.test")
	c, err := loadPair(certFile, keyFile)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func waitStatus(t *testing.T, m *DomainManager, domain string, done func(DomainStatus) bool) DomainStatus {
	t.Helper()
	for start := time.Now(); ; time.Sleep(50 * time.Millisecond) {
		st, _ := m.Status(domain)
		if done(st) {
			return st
		}
		if time.Since(start) > time.Minute {
			t.Fatalf("%s: stuck at %+v", domain, st)
		}
	}
}

// TestDomainCertificateOverTLSALPN issues a certificate for a custom domain
// against Pebble, which validates it by connecting to our TLS listener, the
// way a public listener behind an SNI passthrough is validated.
func TestDomainCertificateOverTLSALPN(t *testing.T) {
	dnsAddr := serveDNS(t, loopbackZone{})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	directory, mgmt, hc := startPebbleMgmt(t, dnsAddr, ln.Addr().(*net.TCPAddr).Port)

	dir := t.TempDir()
	m := &DomainManager{Dir: filepath.Join(dir, "acme"), DirectoryURL: directory, HTTPClient: hc, Log: quiet,
		Check: func(context.Context, string) error { return nil }}
	fb := fallbackCert(t)
	getCert := m.GetCertificate(func(*tls.ClientHelloInfo) (*tls.Certificate, error) { return fb, nil })
	srv := &http.Server{
		Handler:   http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "hello from "+r.Host) }),
		TLSConfig: &tls.Config{GetCertificate: getCert, NextProtos: []string{"h2", "http/1.1", acme.ALPNProto}},
	}
	go srv.ServeTLS(ln, "", "")
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go m.Run(ctx)
	const domain = "game.example.test"
	m.Sync([]string{domain})
	st := waitStatus(t, m, domain, func(s DomainStatus) bool { return s.Status != DomainPending })
	if st.Status != DomainIssued || st.NotAfter == nil || st.NextAttempt == nil || !st.NextAttempt.Before(*st.NotAfter) {
		t.Fatalf("status %+v", st)
	}

	// The served chain verifies against the CA's root for the domain.
	resp, err := hc.Get(mgmt + "/roots/0")
	if err != nil {
		t.Fatal(err)
	}
	rootPEM, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(rootPEM) {
		t.Fatalf("pebble root: %s", rootPEM)
	}
	client := &http.Client{Transport: &http.Transport{
		TLSClientConfig: &tls.Config{RootCAs: roots},
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "tcp", ln.Addr().String())
		}}}
	resp, err = client.Get("https://" + domain + "/")
	if err != nil {
		t.Fatalf("fetch over the issued certificate: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(body) != "hello from "+domain {
		t.Fatalf("body %q", body)
	}

	// Other names still get the wildcard source, and a TLS-ALPN-01 probe for a
	// name with no challenge pending gets nothing.
	if c, err := getCert(&tls.ClientHelloInfo{ServerName: "x.widgets.test"}); err != nil || c != fb {
		t.Errorf("an unknown name was not passed through: %v", err)
	}
	if _, err := getCert(&tls.ClientHelloInfo{ServerName: domain, SupportedProtos: []string{acme.ALPNProto}}); err == nil {
		t.Error("a challenge probe with nothing pending got a certificate")
	}

	// A restart serves the certificate on disk without asking the CA again.
	m2 := &DomainManager{Dir: m.Dir, DirectoryURL: directory, Log: quiet,
		Check: func(context.Context, string) error { t.Error("a current certificate was re-checked"); return nil }}
	m2.Sync([]string{domain})
	if st, _ := m2.Status(domain); st.Status != DomainIssued {
		t.Errorf("after a restart: %+v", st)
	}

	// Detaching stops serving it and removes the pair.
	m.Sync(nil)
	if c, _ := getCert(&tls.ClientHelloInfo{ServerName: domain}); c != fb {
		t.Error("a detached domain is still served its certificate")
	}
	if _, err := os.Stat(m.domainDir(domain)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("certificate files left behind: %v", err)
	}
}

// A domain whose DNS does not point here never reaches the CA, and failures
// that do reach it are paced by the hourly order budget.
func TestDomainGuardsAndBudget(t *testing.T) {
	// A CA that is not there: every order fails fast.
	dead := "http://127.0.0.1:1/dir"
	var checked []string
	m := &DomainManager{Dir: t.TempDir(), DirectoryURL: dead, Log: quiet, OrdersPerHour: 1,
		CheckRetry: time.Hour, OrderRetry: time.Hour,
		Check: func(_ context.Context, d string) error {
			checked = append(checked, d)
			if strings.HasPrefix(d, "elsewhere.") {
				return errors.New("elsewhere.example.test resolves to 192.0.2.1")
			}
			return nil
		}}
	m.Sync([]string{"a.example.test", "b.example.test", "elsewhere.example.test"})
	for range 3 {
		d, _ := m.due()
		m.work(context.Background(), d)
	}
	if d, at := m.due(); d != "" || time.Until(at) < 50*time.Minute {
		t.Fatalf("something is due again at once: %q %v", d, at)
	}
	a, _ := m.Status("a.example.test")
	b, _ := m.Status("b.example.test")
	e, _ := m.Status("elsewhere.example.test")
	if a.Status != DomainError || a.Reason == "" {
		t.Errorf("the first order failing: %+v", a)
	}
	if b.Status != DomainPending || !strings.Contains(b.Reason, "budget") {
		t.Errorf("the second order should wait for the budget: %+v", b)
	}
	if e.Status != DomainPending || !strings.Contains(e.Reason, "waiting for DNS") || !strings.Contains(e.Reason, "192.0.2.1") {
		t.Errorf("a domain pointed elsewhere: %+v", e)
	}
	if len(m.orders) != 1 {
		t.Errorf("orders counted: %d, want 1 (a failed DNS check must not spend one)", len(m.orders))
	}
	if len(checked) != 3 {
		t.Errorf("checked %v", checked)
	}
}
