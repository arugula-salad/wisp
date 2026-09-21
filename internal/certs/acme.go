package certs

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/crypto/acme"
)

const LetsEncrypt = "https://acme-v02.api.letsencrypt.org/directory"

// DNSProvider publishes the TXT record a DNS-01 challenge asks for. cleanup
// removes it again and is called whether or not validation succeeded.
type DNSProvider interface {
	Present(ctx context.Context, fqdn, value string) (cleanup func(context.Context) error, err error)
}

// Manager keeps a wildcard certificate for *.Domain in a PEM pair under Dir.
// DNS-01 is the only challenge a CA accepts for a wildcard, and the only one
// that works without the CA being able to reach this machine at all.
type Manager struct {
	Domain       string
	Email        string
	Dir          string // <data>/acme; state is kept per CA beneath it
	DirectoryURL string
	DNS          DNSProvider
	Log          *slog.Logger

	// HTTPClient and WaitDNS are for tests against a local CA.
	HTTPClient *http.Client
	WaitDNS    func(ctx context.Context, fqdn, value string) error
}

// dir is per CA, so that moving from a staging directory to the real one does
// not mistake the staging certificate for a current one.
func (m *Manager) dir() string {
	host := "ca"
	if u, err := url.Parse(m.DirectoryURL); err == nil && u.Host != "" {
		host = u.Hostname()
	}
	return filepath.Join(m.Dir, host)
}

func (m *Manager) CertFile() string { return filepath.Join(m.dir(), "cert.pem") }
func (m *Manager) KeyFile() string  { return filepath.Join(m.dir(), "key.pem") }

func (m *Manager) wildcard() string { return "*." + m.Domain }

// renewAt is when the certificate is due for replacement: two thirds of the way
// through its life, which tracks the CA's lifetime rather than assuming 90 days.
func renewAt(leaf *x509.Certificate) time.Time {
	life := leaf.NotAfter.Sub(leaf.NotBefore)
	return leaf.NotAfter.Add(-life / 3)
}

func (m *Manager) current(st *Store) bool {
	leaf := st.Leaf()
	if leaf == nil || leaf.VerifyHostname("x."+m.Domain) != nil {
		return false
	}
	return time.Now().Before(renewAt(leaf))
}

// Run obtains the certificate if st has none that is current, then keeps it
// renewed for as long as ctx lives. Whatever is already on disk is served meanwhile.
func (m *Manager) Run(ctx context.Context, st *Store) {
	st.Load()
	backoff := 5 * time.Minute
	for {
		wait := 12 * time.Hour
		if !m.current(st) {
			m.Log.Info("requesting TLS certificate", "name", m.wildcard(), "ca", m.DirectoryURL)
			octx, cancel := context.WithTimeout(ctx, 10*time.Minute)
			err := m.obtain(octx)
			cancel()
			if err == nil {
				err = st.Load()
			}
			if err != nil {
				// CAs rate-limit failed validations by the hour; do not hammer them.
				m.Log.Error("TLS certificate request failed", "name", m.wildcard(), "err", err, "retry_in", backoff)
				wait, backoff = backoff, min(backoff*2, 6*time.Hour)
			} else {
				backoff = 5 * time.Minute
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}

func loadOrCreateKey(path string) (*ecdsa.PrivateKey, error) {
	if b, err := os.ReadFile(path); err == nil {
		blk, _ := pem.Decode(b)
		if blk == nil {
			return nil, fmt.Errorf("%s: not PEM", path)
		}
		return x509.ParseECPrivateKey(blk.Bytes)
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	return key, writeKey(path, key)
}

func writeKey(path string, key *ecdsa.PrivateKey) error {
	der, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return err
	}
	return os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der}), 0o600)
}

func (m *Manager) obtain(ctx context.Context) error {
	if err := os.MkdirAll(m.dir(), 0o700); err != nil {
		return err
	}
	accountKey, err := loadOrCreateKey(filepath.Join(m.dir(), "account.key"))
	if err != nil {
		return fmt.Errorf("account key: %w", err)
	}
	cl := &acme.Client{Key: accountKey, DirectoryURL: m.DirectoryURL, HTTPClient: m.HTTPClient, UserAgent: "mini-sprites"}
	acct := &acme.Account{}
	if m.Email != "" {
		acct.Contact = []string{"mailto:" + m.Email}
	}
	if _, err := cl.Register(ctx, acct, acme.AcceptTOS); err != nil && !errors.Is(err, acme.ErrAccountAlreadyExists) {
		return fmt.Errorf("register account: %w", err)
	}

	order, err := cl.AuthorizeOrder(ctx, acme.DomainIDs(m.wildcard()))
	if err != nil {
		return fmt.Errorf("new order: %w", err)
	}
	for _, authzURL := range order.AuthzURLs {
		if err := m.authorize(ctx, cl, authzURL); err != nil {
			return err
		}
	}
	orderURL := order.URI // only the response that created the order is sure to carry it
	if order, err = cl.WaitOrder(ctx, orderURL); err != nil {
		return fmt.Errorf("order: %w", err)
	}

	certKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	csr, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{
		Subject: pkix.Name{CommonName: m.wildcard()}, DNSNames: []string{m.wildcard()},
	}, crypto.Signer(certKey))
	if err != nil {
		return err
	}
	chain, _, err := cl.CreateOrderCert(ctx, order.FinalizeURL, csr, true)
	if err != nil {
		// A CA that issues asynchronously answers "processing", and x/crypto then
		// polls the Location header of that answer, which RFC 8555 does not require
		// and Pebble does not send. We know where the order is; ask there.
		if o, werr := cl.WaitOrder(ctx, orderURL); werr == nil && o.Status == acme.StatusValid && o.CertURL != "" {
			chain, err = cl.FetchCert(ctx, o.CertURL, true)
		}
	}
	if err != nil {
		return fmt.Errorf("finalize: %w", err)
	}
	var certPEM []byte
	for _, der := range chain {
		certPEM = append(certPEM, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})...)
	}

	// Key first: a reader that catches the pair between the two renames sees a
	// mismatch and keeps its old certificate, rather than a cert with no key.
	if err := writeKey(m.KeyFile()+".tmp", certKey); err != nil {
		return err
	}
	if err := os.WriteFile(m.CertFile()+".tmp", certPEM, 0o644); err != nil {
		return err
	}
	if err := os.Rename(m.KeyFile()+".tmp", m.KeyFile()); err != nil {
		return err
	}
	return os.Rename(m.CertFile()+".tmp", m.CertFile())
}

func (m *Manager) authorize(ctx context.Context, cl *acme.Client, authzURL string) error {
	authz, err := cl.GetAuthorization(ctx, authzURL)
	if err != nil {
		return fmt.Errorf("authorization: %w", err)
	}
	if authz.Status == acme.StatusValid {
		return nil // the CA remembers a recent validation
	}
	var chal *acme.Challenge
	for _, c := range authz.Challenges {
		if c.Type == "dns-01" {
			chal = c
		}
	}
	if chal == nil {
		return fmt.Errorf("the CA offers no dns-01 challenge for %s", authz.Identifier.Value)
	}
	value, err := cl.DNS01ChallengeRecord(chal.Token)
	if err != nil {
		return err
	}
	fqdn := "_acme-challenge." + authz.Identifier.Value
	cleanup, err := m.DNS.Present(ctx, fqdn, value)
	if err != nil {
		return fmt.Errorf("publish %s: %w", fqdn, err)
	}
	defer func() {
		cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		if err := cleanup(cctx); err != nil {
			m.Log.Warn("could not remove the challenge record; delete it by hand", "record", fqdn, "err", err)
		}
	}()

	wait := m.WaitDNS
	if wait == nil {
		wait = waitAuthoritative
	}
	if err := wait(ctx, fqdn, value); err != nil {
		// The CA's own lookup is what counts, so let it try.
		m.Log.Warn("challenge record not confirmed on the authoritative servers; asking the CA to validate anyway", "record", fqdn, "err", err)
	}
	if _, err := cl.Accept(ctx, chal); err != nil {
		return fmt.Errorf("accept challenge: %w", err)
	}
	if _, err := cl.WaitAuthorization(ctx, authzURL); err != nil {
		return fmt.Errorf("validation of %s: %w", fqdn, err)
	}
	return nil
}
