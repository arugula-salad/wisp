package server

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/netip"
	"slices"
	"strings"
	"time"

	"github.com/jhgaylor/wisp/internal/certs"
	"github.com/jhgaylor/wisp/internal/store"
	"github.com/miekg/dns"
)

// Custom domains (ours, not upstream's): a sprite can be reached at names of
// its owner's choosing as well as at <name>.<url-domain>. The public listener
// gets each one a certificate over TLS-ALPN-01, once its DNS leads here.

// DomainConfig switches custom domains on.
type DomainConfig struct {
	ACMEDir      string // <data>/acme
	DirectoryURL string
	Email        string
	// Resolver is the recursive DNS server (host:port) the ownership check asks.
	Resolver string
	// PerSprite and Total cap attached domains (0 = no limit).
	PerSprite, Total int
	// OrdersPerHour caps ACME orders for custom domains (0 = the manager's default).
	OrdersPerHour int
	// CheckRetry, OrderRetry: see certs.DomainManager; zero takes its defaults.
	CheckRetry, OrderRetry time.Duration
	HTTPClient             *http.Client
}

type domains struct {
	cfg DomainConfig
	mgr *certs.DomainManager
}

// EnableCustomDomains starts managing certificates for the domains attached to
// sprites, and returns the public listener's certificate source: next, with
// custom domains and TLS-ALPN-01 challenges in front of it.
func (s *Server) EnableCustomDomains(ctx context.Context, cfg DomainConfig, next func(*tls.ClientHelloInfo) (*tls.Certificate, error)) func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	mgr := &certs.DomainManager{Dir: cfg.ACMEDir, DirectoryURL: cfg.DirectoryURL, Email: cfg.Email,
		HTTPClient: cfg.HTTPClient, Log: s.log, Check: s.checkDomainDNS, OrdersPerHour: cfg.OrdersPerHour,
		CheckRetry: cfg.CheckRetry, OrderRetry: cfg.OrderRetry}
	s.domains = &domains{cfg: cfg, mgr: mgr}
	s.syncDomains()
	go mgr.Run(ctx)
	return mgr.GetCertificate(next)
}

// syncDomains hands the store's domains to the certificate manager.
func (s *Server) syncDomains() {
	if s.domains == nil {
		return
	}
	var all []string
	for d := range s.store.AllDomains() {
		all = append(all, d)
	}
	s.domains.mgr.Sync(all)
}

func (s *Server) registerDomains(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/sprites/{name}/domains", s.listDomains)
	mux.HandleFunc("POST /v1/sprites/{name}/domains", s.attachDomain)
	mux.HandleFunc("GET /v1/sprites/{name}/domains/{domain}", s.getDomain)
	mux.HandleFunc("DELETE /v1/sprites/{name}/domains/{domain}", s.detachDomain)
}

// domainStatus is what the API reports for one attached domain.
func (s *Server) domainStatus(d string) certs.DomainStatus {
	if s.domains != nil {
		if st, ok := s.domains.mgr.Status(d); ok {
			return st
		}
	}
	return certs.DomainStatus{Domain: d, Status: "inactive", Reason: "this daemon runs without --public-listen, so it serves no custom domains"}
}

func (s *Server) listDomains(w http.ResponseWriter, r *http.Request) {
	sp, ok := s.lookup(w, r)
	if !ok {
		return
	}
	out := []certs.DomainStatus{}
	for _, d := range sp.Domains {
		out = append(out, s.domainStatus(d))
	}
	writeJSON(w, http.StatusOK, map[string]any{"domains": out})
}

func (s *Server) getDomain(w http.ResponseWriter, r *http.Request) {
	sp, ok := s.lookup(w, r)
	if !ok {
		return
	}
	d := normalizeDomain(r.PathValue("domain"))
	if !slices.Contains(sp.Domains, d) {
		writeErr(w, http.StatusNotFound, "not_found", "domain not attached to this sprite")
		return
	}
	writeJSON(w, http.StatusOK, s.domainStatus(d))
}

func (s *Server) attachDomain(w http.ResponseWriter, r *http.Request) {
	sp, ok := s.lookup(w, r)
	if !ok {
		return
	}
	var req struct {
		Domain string `json:"domain"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request", "invalid JSON body")
		return
	}
	if s.domains == nil {
		writeErr(w, http.StatusBadRequest, "domains_disabled", "custom domains need the daemon to run with --public-listen")
		return
	}
	d := normalizeDomain(req.Domain)
	if err := s.validDomain(d); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_domain", err.Error())
		return
	}
	already := slices.Contains(sp.Domains, d)
	_, err := s.store.AttachDomain(sp.Name, d, s.domains.cfg.PerSprite, s.domains.cfg.Total)
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeErr(w, http.StatusNotFound, "not_found", "sprite not found")
		return
	case errors.Is(err, store.ErrDomainTaken):
		writeErr(w, http.StatusConflict, "domain_taken", "that domain is attached to another sprite")
		return
	case errors.Is(err, store.ErrDomainLimit):
		limit, n, what := s.domains.cfg.PerSprite, len(sp.Domains), "this sprite"
		if limit == 0 || n < limit {
			limit, n, what = s.domains.cfg.Total, len(s.store.AllDomains()), "this host"
		}
		writeLimitErr(w, &LimitError{Code: "domain_limit_exceeded", Limit: limit, Current: n,
			Message: fmt.Sprintf("%s already has %d custom domains, the most it allows; detach one first", what, n)})
		return
	case err != nil:
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	status := http.StatusOK
	if !already {
		status = http.StatusCreated
		s.log.Info("custom domain attached", "sprite", sp.Name, "domain", d)
		s.syncDomains()
	}
	writeJSON(w, status, s.domainStatus(d))
}

func (s *Server) detachDomain(w http.ResponseWriter, r *http.Request) {
	sp, ok := s.lookup(w, r)
	if !ok {
		return
	}
	d := normalizeDomain(r.PathValue("domain"))
	if _, err := s.store.DetachDomain(sp.Name, d); err != nil {
		if errors.Is(err, store.ErrDomainMissing) || errors.Is(err, store.ErrNotFound) {
			writeErr(w, http.StatusNotFound, "not_found", "domain not attached to this sprite")
			return
		}
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	s.log.Info("custom domain detached", "sprite", sp.Name, "domain", d)
	s.syncDomains()
	w.WriteHeader(http.StatusNoContent)
}

func normalizeDomain(d string) string {
	return strings.TrimSuffix(strings.ToLower(strings.TrimSpace(d)), ".")
}

// validDomain accepts an ASCII hostname (IDNs in their xn-- form) of at least
// two labels that is not one of our own sprite URLs.
func (s *Server) validDomain(d string) error {
	if len(d) > 253 {
		return errors.New("domain is longer than 253 characters")
	}
	labels := strings.Split(d, ".")
	if len(labels) < 2 {
		return errors.New("domain must be a fully qualified hostname such as game.example.com")
	}
	for _, l := range labels {
		if l == "" || len(l) > 63 || l[0] == '-' || l[len(l)-1] == '-' {
			return fmt.Errorf("%q is not a valid hostname", d)
		}
		for _, c := range l {
			if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
				return fmt.Errorf("%q is not a valid hostname (letters, digits and hyphens; write an IDN in its xn-- form)", d)
			}
		}
	}
	if tld := labels[len(labels)-1]; strings.Trim(tld, "0123456789") == "" {
		return errors.New("an IP address is not a domain")
	}
	if d == "localhost" || strings.HasSuffix(d, ".localhost") {
		return errors.New("no CA issues certificates for localhost")
	}
	if domain, ok := s.underURLDomain(d); ok {
		return fmt.Errorf("names under %s are already sprite URLs", domain)
	}
	return nil
}

// checkDomainDNS is the guard in front of the CA: the domain must resolve, and
// only to addresses that its sprite's own URL resolves to. A CNAME to
// <name>.<url-domain> (or to anything else under the wildcard) passes, and so do
// A/AAAA records naming the same addresses as the wildcard record. Anything
// else, including a domain that resolves partly elsewhere, is not ours to ask
// a certificate for.
func (s *Server) checkDomainDNS(ctx context.Context, domain string) error {
	name, ok := s.store.DomainOwner(domain)
	if !ok {
		return errors.New("not attached to any sprite")
	}
	sp, err := s.store.Get(name)
	if err != nil {
		return errors.New("not attached to any sprite")
	}
	target := name + "." + s.urlDomainOf(sp)
	got, err := resolveAddrs(ctx, s.domains.cfg.Resolver, domain)
	if err != nil {
		return err
	}
	if len(got) == 0 {
		return fmt.Errorf("%s has no A or AAAA record; add a CNAME to %s", domain, target)
	}
	want, err := resolveAddrs(ctx, s.domains.cfg.Resolver, target)
	if err != nil {
		return err
	}
	if len(want) == 0 {
		return fmt.Errorf("%s itself does not resolve; the url-domain needs its wildcard record", target)
	}
	for _, a := range got {
		if !slices.Contains(want, a) {
			return fmt.Errorf("%s resolves to %s, which is not an address of %s (%s); point a CNAME at %s", domain, a, target, joinAddrs(want), target)
		}
	}
	return nil
}

func joinAddrs(as []netip.Addr) string {
	s := make([]string, len(as))
	for i, a := range as {
		s[i] = a.String()
	}
	return strings.Join(s, ", ")
}

// resolveAddrs asks resolver for name's A and AAAA records, following CNAMEs as
// a recursive resolver does. A name that does not exist has no addresses.
func resolveAddrs(ctx context.Context, resolver, name string) ([]netip.Addr, error) {
	var out []netip.Addr
	for _, qt := range []uint16{dns.TypeA, dns.TypeAAAA} {
		q := new(dns.Msg)
		q.SetQuestion(dns.Fqdn(name), qt)
		c := &dns.Client{Timeout: 5 * time.Second}
		in, _, err := c.ExchangeContext(ctx, q, resolver)
		if err == nil && in.Truncated {
			c.Net = "tcp"
			in, _, err = c.ExchangeContext(ctx, q, resolver)
		}
		if err != nil {
			return nil, fmt.Errorf("look up %s: %w", name, err)
		}
		if in.Rcode != dns.RcodeSuccess && in.Rcode != dns.RcodeNameError {
			return nil, fmt.Errorf("look up %s: %s", name, dns.RcodeToString[in.Rcode])
		}
		for _, rr := range in.Answer {
			switch rr := rr.(type) {
			case *dns.A:
				if a, ok := netip.AddrFromSlice(rr.A.To4()); ok {
					out = append(out, a)
				}
			case *dns.AAAA:
				if a, ok := netip.AddrFromSlice(rr.AAAA); ok {
					out = append(out, a)
				}
			}
		}
	}
	return out, nil
}
