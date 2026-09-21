package certs

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/miekg/dns"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

// selfSigned writes a certificate for names into dir and returns the file paths.
func selfSigned(t *testing.T, dir string, names ...string) (certFile, keyFile string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: names[0]},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, IsCA: true, BasicConstraintsValid: true}
	for _, n := range names {
		if ip := net.ParseIP(n); ip != nil {
			tmpl.IPAddresses = append(tmpl.IPAddresses, ip)
		} else {
			tmpl.DNSNames = append(tmpl.DNSNames, n)
		}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	certFile, keyFile = filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
	if err := os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := writeKey(keyFile, key); err != nil {
		t.Fatal(err)
	}
	return certFile, keyFile
}

func TestStorePicksUpAReplacedPair(t *testing.T) {
	dir := t.TempDir()
	certFile, keyFile := selfSigned(t, dir, "*.one.test")
	st := NewStore(certFile, keyFile, quiet)
	if err := st.Load(); err != nil {
		t.Fatal(err)
	}
	served := func() string {
		st.mu.Lock()
		st.checked = time.Time{} // as if the recheck interval had passed
		st.mu.Unlock()
		c, err := st.GetCertificate(nil)
		if err != nil {
			t.Fatal(err)
		}
		return c.Leaf.DNSNames[0]
	}
	if got := served(); got != "*.one.test" {
		t.Fatalf("served %s", got)
	}

	// Half a renewal: the new key is in place, the old cert still is.
	other := t.TempDir()
	newCert, newKey := selfSigned(t, other, "*.two.test")
	future := time.Now().Add(time.Minute)
	b, _ := os.ReadFile(newKey)
	os.WriteFile(keyFile, b, 0o600)
	os.Chtimes(keyFile, future, future)
	if got := served(); got != "*.one.test" {
		t.Fatalf("a mismatched pair on disk changed what is served to %s", got)
	}
	b, _ = os.ReadFile(newCert)
	os.WriteFile(certFile, b, 0o644)
	os.Chtimes(certFile, future.Add(time.Second), future.Add(time.Second))
	if got := served(); got != "*.two.test" {
		t.Fatalf("after the renewal completed, still serving %s", got)
	}
}

func TestStoreWithoutACertificateFailsTheHandshakeOnly(t *testing.T) {
	st := NewStore(filepath.Join(t.TempDir(), "cert.pem"), filepath.Join(t.TempDir(), "key.pem"), quiet)
	if _, err := st.GetCertificate(nil); err == nil {
		t.Fatal("want an error while there is no certificate")
	}
}

// fakeCloudflare is the three endpoints Present uses.
type fakeCloudflare struct {
	mu      sync.Mutex
	records map[string][2]string // id -> name, content
	next    int
}

func (f *fakeCloudflare) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	reply := func(result any) {
		json.NewEncoder(w).Encode(map[string]any{"success": true, "errors": []any{}, "result": result})
	}
	if r.Header.Get("Authorization") != "Bearer cf-token" {
		w.WriteHeader(http.StatusForbidden)
		json.NewEncoder(w).Encode(map[string]any{"success": false, "errors": []map[string]any{{"code": 9109, "message": "Invalid access token"}}})
		return
	}
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/zones":
		if r.URL.Query().Get("name") == "widgets.test" {
			reply([]map[string]string{{"id": "zone1"}})
		} else {
			reply([]any{})
		}
	case r.Method == http.MethodPost && r.URL.Path == "/zones/zone1/dns_records":
		var in struct{ Type, Name, Content string }
		json.NewDecoder(r.Body).Decode(&in)
		f.next++
		id := fmt.Sprintf("rec%d", f.next)
		f.records[id] = [2]string{in.Name, in.Content}
		reply(map[string]string{"id": id})
	case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/zones/zone1/dns_records/"):
		delete(f.records, strings.TrimPrefix(r.URL.Path, "/zones/zone1/dns_records/"))
		reply(map[string]string{})
	default:
		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode(map[string]any{"success": false, "errors": []map[string]any{{"code": 7003, "message": "no route"}}})
	}
}

func TestCloudflarePresentAndCleanup(t *testing.T) {
	fake := &fakeCloudflare{records: map[string][2]string{}}
	srv := httptest.NewServer(fake)
	defer srv.Close()
	cf := &Cloudflare{Token: "cf-token", API: srv.URL}

	// The zone is two labels above the record, as it is for sprites.widgets.test.
	cleanup, err := cf.Present(context.Background(), "_acme-challenge.sprites.widgets.test", "abc123")
	if err != nil {
		t.Fatal(err)
	}
	if got := fake.records["rec1"]; got != [2]string{"_acme-challenge.sprites.widgets.test", `"abc123"`} {
		t.Fatalf("record created as %q", got)
	}
	if err := cleanup(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(fake.records) != 0 {
		t.Fatalf("cleanup left %v", fake.records)
	}

	if _, err := cf.Present(context.Background(), "_acme-challenge.elsewhere.test", "x"); err == nil || !strings.Contains(err.Error(), "no zone") {
		t.Fatalf("a domain outside the token's zones: %v", err)
	}
	bad := &Cloudflare{Token: "nope", API: srv.URL}
	if _, err := bad.Present(context.Background(), "_acme-challenge.widgets.test", "x"); err == nil || !strings.Contains(err.Error(), "Invalid access token") {
		t.Fatalf("a rejected token should surface Cloudflare's message: %v", err)
	}
}

// txtServer is both the DNSProvider and the DNS server the CA validates against.
type txtServer struct {
	mu        sync.Mutex
	txt       map[string]string
	presented []string
	cleaned   int
}

func (s *txtServer) Present(_ context.Context, fqdn, value string) (func(context.Context) error, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.txt[dns.Fqdn(fqdn)] = value
	s.presented = append(s.presented, fqdn)
	return func(context.Context) error {
		s.mu.Lock()
		defer s.mu.Unlock()
		delete(s.txt, dns.Fqdn(fqdn))
		s.cleaned++
		return nil
	}, nil
}

func (s *txtServer) ServeDNS(w dns.ResponseWriter, q *dns.Msg) {
	m := new(dns.Msg)
	m.SetReply(q)
	m.Authoritative = true
	s.mu.Lock()
	if v, ok := s.txt[q.Question[0].Name]; ok && q.Question[0].Qtype == dns.TypeTXT {
		m.Answer = append(m.Answer, &dns.TXT{Hdr: dns.RR_Header{Name: q.Question[0].Name, Rrtype: dns.TypeTXT, Class: dns.ClassINET, Ttl: 1}, Txt: []string{v}})
	}
	s.mu.Unlock()
	w.WriteMsg(m)
}

func freePort(t *testing.T) int {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// TestObtainAgainstPebble runs the whole DNS-01 flow against Let's Encrypt's
// test CA. Install it with: go install github.com/letsencrypt/pebble/v2/cmd/pebble@latest
func TestObtainAgainstPebble(t *testing.T) {
	pebble := os.Getenv("PEBBLE_BIN")
	if pebble == "" {
		pebble, _ = exec.LookPath("pebble")
	}
	if pebble == "" {
		t.Skip("pebble not found (set PEBBLE_BIN or put it on PATH)")
	}

	zone := &txtServer{txt: map[string]string{}}
	// Pebble asks over TCP, and falls back between the two; serve one port on both.
	var pc net.PacketConn
	var tl net.Listener
	for try := 0; ; try++ {
		var err error
		if tl, err = net.Listen("tcp", "127.0.0.1:0"); err != nil {
			t.Fatal(err)
		}
		if pc, err = net.ListenPacket("udp", tl.Addr().String()); err == nil {
			break
		}
		tl.Close() // someone else holds that UDP port
		if try == 10 {
			t.Fatal(err)
		}
	}
	dnsSrv := &dns.Server{PacketConn: pc, Handler: zone}
	go dnsSrv.ActivateAndServe()
	defer dnsSrv.Shutdown()
	tcpSrv := &dns.Server{Listener: tl, Handler: zone}
	go tcpSrv.ActivateAndServe()
	defer tcpSrv.Shutdown()

	dir := t.TempDir()
	caCert, caKey := selfSigned(t, dir, "127.0.0.1", "localhost")
	port := freePort(t)
	cfg, _ := json.Marshal(map[string]any{"pebble": map[string]any{
		"listenAddress": fmt.Sprintf("127.0.0.1:%d", port), "managementListenAddress": fmt.Sprintf("127.0.0.1:%d", freePort(t)),
		"certificate": caCert, "privateKey": caKey, "httpPort": 5002, "tlsPort": 5001,
		"ocspResponderURL": "", "externalAccountBindingRequired": false,
	}})
	cfgFile := filepath.Join(dir, "pebble.json")
	os.WriteFile(cfgFile, cfg, 0o644)
	cmd := exec.Command(pebble, "-config", cfgFile, "-dnsserver", pc.LocalAddr().String())
	cmd.Env = append(os.Environ(), "PEBBLE_VA_NOSLEEP=1", "PEBBLE_WFE_NONCEREJECT=0")
	if testing.Verbose() {
		cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { cmd.Process.Kill(); cmd.Wait() }()

	pool := x509.NewCertPool()
	b, _ := os.ReadFile(caCert)
	pool.AppendCertsFromPEM(b)
	hc := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool}}}
	directory := fmt.Sprintf("https://127.0.0.1:%d/dir", port)
	for start := time.Now(); ; time.Sleep(50 * time.Millisecond) {
		if resp, err := hc.Get(directory); err == nil {
			resp.Body.Close()
			break
		} else if time.Since(start) > 10*time.Second {
			t.Fatalf("pebble did not come up: %v", err)
		}
	}

	mgr := &Manager{Domain: "widgets.test", Email: "ops@widgets.test", Dir: filepath.Join(dir, "acme"), DirectoryURL: directory,
		DNS: zone, Log: quiet, HTTPClient: hc,
		WaitDNS: func(context.Context, string, string) error { return nil }}
	st := NewStore(mgr.CertFile(), mgr.KeyFile(), quiet)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if err := mgr.obtain(ctx); err != nil {
		t.Fatal(err)
	}
	if err := st.Load(); err != nil {
		t.Fatal(err)
	}
	leaf := st.Leaf()
	if err := leaf.VerifyHostname("dev.widgets.test"); err != nil {
		t.Errorf("the certificate does not cover a sprite name: %v (names %v)", err, leaf.DNSNames)
	}
	if !mgr.current(st) {
		t.Error("a certificate issued a moment ago is not considered current")
	}
	if len(zone.presented) != 1 || zone.presented[0] != "_acme-challenge.widgets.test" {
		t.Errorf("challenge records presented: %v", zone.presented)
	}
	if zone.cleaned != 1 || len(zone.txt) != 0 {
		t.Errorf("challenge record not cleaned up: cleaned=%d left=%v", zone.cleaned, zone.txt)
	}
	if fi, _ := os.Stat(mgr.KeyFile()); fi.Mode().Perm() != 0o600 {
		t.Errorf("private key mode %v", fi.Mode().Perm())
	}
	c, _ := st.GetCertificate(nil)
	if len(c.Certificate) < 2 {
		t.Error("the served chain has no intermediate; browsers would reject it")
	}

	// A second run reuses the account rather than failing on "already exists".
	if err := mgr.obtain(ctx); err != nil {
		t.Fatalf("renewal with an existing account: %v", err)
	}

	// A certificate for another domain must not pass for ours.
	other := &Manager{Domain: "elsewhere.test", DirectoryURL: directory, Dir: mgr.Dir}
	if other.current(st) {
		t.Error("a *.widgets.test certificate was accepted as current for elsewhere.test")
	}
}
