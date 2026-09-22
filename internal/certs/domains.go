package certs

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/acme"
)

// Custom domains are single names that someone else's DNS points at us, so no
// DNS-01 here: the CA proves the name reaches this listener with TLS-ALPN-01,
// which is answered in the TLS handshake itself and therefore survives an
// SNI-routed passthrough in front of us (HTTP-01 would need port 80).

// Domain states as the API reports them.
const (
	DomainPending = "pending" // not issued yet: waiting on DNS, the order budget, or the CA
	DomainIssued  = "issued"  // a current certificate is being served
	DomainError   = "error"   // the last request to the CA failed; retried with backoff
)

// DomainStatus is one domain's certificate state.
type DomainStatus struct {
	Domain      string     `json:"domain"`
	Status      string     `json:"status"`
	Reason      string     `json:"reason,omitempty"`
	NotAfter    *time.Time `json:"not_after,omitempty"`
	NextAttempt *time.Time `json:"next_attempt,omitempty"`
}

// DomainManager keeps one certificate per custom domain, obtained over
// TLS-ALPN-01 and renewed at two thirds of its life, under
// <Dir>/<ca>/domains/<domain>/. It never asks the CA about a domain whose DNS
// does not point here (Check), and it paces itself: failed validations count
// against the CA's rate limits, which are per account and by the hour.
type DomainManager struct {
	Dir          string // <data>/acme, shared with the wildcard Manager
	DirectoryURL string
	Email        string
	HTTPClient   *http.Client
	Log          *slog.Logger

	// Check returns nil once domain's DNS leads to this host.
	Check func(ctx context.Context, domain string) error
	// OrdersPerHour caps new ACME orders across every domain (0 = 10).
	OrdersPerHour int
	// CheckRetry and OrderRetry are the first backoffs after a failed DNS check
	// (default 1m, doubling to 30m) and a failed order (default 5m, doubling to 6h).
	CheckRetry, OrderRetry time.Duration

	once       sync.Once
	mu         sync.Mutex
	domains    map[string]*domainState
	challenges map[string]*tls.Certificate // by domain, while the CA validates
	orders     []time.Time                 // started within the last hour
	wake       chan struct{}
}

type domainState struct {
	cert         *tls.Certificate
	status       string
	reason       string
	next         time.Time
	checkBackoff time.Duration
	orderBackoff time.Duration
}

func (m *DomainManager) init() {
	m.once.Do(func() {
		m.domains = map[string]*domainState{}
		m.challenges = map[string]*tls.Certificate{}
		m.wake = make(chan struct{}, 1)
		if m.OrdersPerHour == 0 {
			m.OrdersPerHour = 10
		}
		if m.CheckRetry == 0 {
			m.CheckRetry = time.Minute
		}
		if m.OrderRetry == 0 {
			m.OrderRetry = 5 * time.Minute
		}
	})
}

func (m *DomainManager) domainDir(domain string) string {
	return filepath.Join(caDir(m.Dir, m.DirectoryURL), "domains", domain)
}

func (m *DomainManager) files(domain string) (certFile, keyFile string) {
	d := m.domainDir(domain)
	return filepath.Join(d, "cert.pem"), filepath.Join(d, "key.pem")
}

func loadPair(certFile, keyFile string) (*tls.Certificate, error) {
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, err
	}
	if cert.Leaf == nil {
		if cert.Leaf, err = x509.ParseCertificate(cert.Certificate[0]); err != nil {
			return nil, err
		}
	}
	return &cert, nil
}

func (m *DomainManager) poke() {
	select {
	case m.wake <- struct{}{}:
	default:
	}
}

// Sync makes the managed set exactly domains: new ones are picked up (with any
// certificate already on disk), dropped ones stop being served and lose their
// certificate files.
func (m *DomainManager) Sync(domains []string) {
	m.init()
	want := map[string]bool{}
	for _, d := range domains {
		want[d] = true
	}
	m.mu.Lock()
	var gone []string
	for d := range m.domains {
		if !want[d] {
			delete(m.domains, d)
			gone = append(gone, d)
		}
	}
	for d := range want {
		if m.domains[d] != nil {
			continue
		}
		st := &domainState{status: DomainPending, reason: "not checked yet", next: time.Now()}
		if c, err := loadPair(m.files(d)); err == nil && c.Leaf.VerifyHostname(d) == nil {
			st.cert, st.status, st.reason = c, DomainIssued, ""
		}
		m.domains[d] = st
	}
	m.mu.Unlock()
	for _, d := range gone {
		os.RemoveAll(m.domainDir(d))
		m.Log.Info("custom domain dropped", "domain", d)
	}
	m.poke()
}

// Status reports on domain, or false when it is not managed.
func (m *DomainManager) Status(domain string) (DomainStatus, bool) {
	m.init()
	m.mu.Lock()
	defer m.mu.Unlock()
	st := m.domains[domain]
	if st == nil {
		return DomainStatus{}, false
	}
	out := DomainStatus{Domain: domain, Status: st.status, Reason: st.reason}
	if st.cert != nil {
		t := st.cert.Leaf.NotAfter
		out.NotAfter = &t
	}
	if !st.next.IsZero() {
		t := st.next
		out.NextAttempt = &t
	}
	return out, true
}

// GetCertificate wraps the listener's own certificate source: a TLS-ALPN-01
// probe from the CA gets its challenge certificate (or nothing), a custom
// domain gets its certificate, and every other name falls through to next.
func (m *DomainManager) GetCertificate(next func(*tls.ClientHelloInfo) (*tls.Certificate, error)) func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	m.init()
	return func(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
		name := strings.TrimSuffix(strings.ToLower(hello.ServerName), ".")
		m.mu.Lock()
		if len(hello.SupportedProtos) == 1 && hello.SupportedProtos[0] == acme.ALPNProto {
			c := m.challenges[name]
			m.mu.Unlock()
			if c == nil {
				return nil, fmt.Errorf("no ACME challenge pending for %q", name)
			}
			return c, nil
		}
		var c *tls.Certificate
		if st := m.domains[name]; st != nil {
			c = st.cert
		}
		m.mu.Unlock()
		if c != nil {
			return c, nil
		}
		return next(hello)
	}
}

// Run works the managed domains until ctx ends: one at a time, each when it is due.
func (m *DomainManager) Run(ctx context.Context) {
	m.init()
	for {
		wait := time.Hour
		if d, at := m.due(); d != "" {
			m.work(ctx, d)
			wait = 0
		} else if !at.IsZero() {
			wait = time.Until(at)
		}
		if wait > 0 {
			select {
			case <-ctx.Done():
				return
			case <-m.wake:
			case <-time.After(min(wait, time.Hour)):
			}
		} else if ctx.Err() != nil {
			return
		}
	}
}

// due is the domain that should be worked now, or else when the next one is due.
func (m *DomainManager) due() (string, time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	names := make([]string, 0, len(m.domains))
	for d := range m.domains {
		names = append(names, d)
	}
	sort.Strings(names)
	var soonest time.Time
	for _, d := range names {
		st := m.domains[d]
		if !time.Now().Before(st.next) {
			return d, time.Time{}
		}
		if soonest.IsZero() || st.next.Before(soonest) {
			soonest = st.next
		}
	}
	return "", soonest
}

// update applies fn to domain's state if it is still managed.
func (m *DomainManager) update(domain string, fn func(*domainState)) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	st := m.domains[domain]
	if st != nil {
		fn(st)
	}
	return st != nil
}

func backoff(cur, first, max time.Duration) (wait, next time.Duration) {
	if cur == 0 {
		cur = first
	}
	return cur, min(cur*2, max)
}

// work does whatever domain needs next: nothing, a DNS check, or an order.
func (m *DomainManager) work(ctx context.Context, domain string) {
	now := time.Now()
	m.mu.Lock()
	st := m.domains[domain]
	if st == nil {
		m.mu.Unlock()
		return
	}
	cert := st.cert
	if cert != nil && now.Before(renewAt(cert.Leaf)) {
		st.status, st.reason, st.next = DomainIssued, "", renewAt(cert.Leaf)
		m.mu.Unlock()
		return
	}
	m.mu.Unlock()

	// While a still-valid certificate is being renewed, the domain stays issued
	// and the reason says what the renewal is waiting on.
	set := func(st *domainState, status, reason string) {
		if st.cert != nil && time.Now().Before(st.cert.Leaf.NotAfter) {
			status, reason = DomainIssued, "renewal: "+reason
		}
		st.status, st.reason = status, reason
	}

	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	err := m.Check(cctx, domain)
	cancel()
	if err != nil {
		m.update(domain, func(st *domainState) {
			var wait time.Duration
			wait, st.checkBackoff = backoff(st.checkBackoff, m.CheckRetry, 30*time.Minute)
			set(st, DomainPending, "waiting for DNS: "+err.Error())
			st.next = time.Now().Add(wait)
		})
		return
	}

	m.mu.Lock()
	if m.domains[domain] == nil {
		m.mu.Unlock()
		return // detached while its DNS was checked
	}
	hour := time.Now().Add(-time.Hour)
	for len(m.orders) > 0 && m.orders[0].Before(hour) {
		m.orders = m.orders[1:]
	}
	if len(m.orders) >= m.OrdersPerHour {
		st := m.domains[domain]
		set(st, DomainPending, fmt.Sprintf("waiting: this host's ACME budget of %d orders an hour is spent", m.OrdersPerHour))
		st.next = m.orders[0].Add(time.Hour)
		m.mu.Unlock()
		return
	}
	m.orders = append(m.orders, time.Now())
	st = m.domains[domain]
	st.checkBackoff = 0
	set(st, DomainPending, "requesting a certificate")
	m.mu.Unlock()

	m.Log.Info("requesting TLS certificate", "name", domain, "ca", m.DirectoryURL, "challenge", "tls-alpn-01")
	octx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	err = m.obtain(octx, domain)
	cancel()
	var c *tls.Certificate
	if err == nil {
		c, err = loadPair(m.files(domain))
	}
	if err != nil {
		if ctx.Err() != nil {
			return
		}
		m.update(domain, func(st *domainState) {
			var wait time.Duration
			wait, st.orderBackoff = backoff(st.orderBackoff, m.OrderRetry, 6*time.Hour)
			set(st, DomainError, err.Error())
			st.next = time.Now().Add(wait)
			m.Log.Error("TLS certificate request failed", "name", domain, "err", err, "retry_in", wait)
		})
		return
	}
	if !m.update(domain, func(st *domainState) {
		st.cert, st.status, st.reason = c, DomainIssued, ""
		st.orderBackoff, st.next = 0, renewAt(c.Leaf)
	}) {
		os.RemoveAll(m.domainDir(domain)) // detached while the CA was busy
		return
	}
	m.Log.Info("TLS certificate issued", "name", domain, "expires", c.Leaf.NotAfter.Format(time.RFC3339))
}

func (m *DomainManager) obtain(ctx context.Context, domain string) error {
	dir := caDir(m.Dir, m.DirectoryURL)
	cl, err := account(ctx, dir, m.DirectoryURL, m.Email, m.HTTPClient)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(m.domainDir(domain), 0o700); err != nil {
		return err
	}
	order, err := cl.AuthorizeOrder(ctx, acme.DomainIDs(domain))
	if err != nil {
		return fmt.Errorf("new order: %w", err)
	}
	for _, authzURL := range order.AuthzURLs {
		if err := m.authorize(ctx, cl, authzURL); err != nil {
			return err
		}
	}
	certFile, keyFile := m.files(domain)
	return finish(ctx, cl, order.URI, domain, certFile, keyFile)
}

func (m *DomainManager) authorize(ctx context.Context, cl *acme.Client, authzURL string) error {
	authz, err := cl.GetAuthorization(ctx, authzURL)
	if err != nil {
		return fmt.Errorf("authorization: %w", err)
	}
	if authz.Status == acme.StatusValid {
		return nil
	}
	name := authz.Identifier.Value
	var chal *acme.Challenge
	for _, c := range authz.Challenges {
		if c.Type == "tls-alpn-01" {
			chal = c
		}
	}
	if chal == nil {
		return fmt.Errorf("the CA offers no tls-alpn-01 challenge for %s", name)
	}
	cert, err := cl.TLSALPN01ChallengeCert(chal.Token, name)
	if err != nil {
		return err
	}
	m.mu.Lock()
	m.challenges[name] = &cert
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		delete(m.challenges, name)
		m.mu.Unlock()
	}()
	if _, err := cl.Accept(ctx, chal); err != nil {
		return fmt.Errorf("accept challenge: %w", err)
	}
	if _, err := cl.WaitAuthorization(ctx, authzURL); err != nil {
		var ae *acme.AuthorizationError
		if errors.As(err, &ae) && len(ae.Errors) > 0 {
			return fmt.Errorf("validation of %s: %v", name, ae.Errors[0])
		}
		return fmt.Errorf("validation of %s: %w", name, err)
	}
	return nil
}
