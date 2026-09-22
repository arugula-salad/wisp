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
	"sync"
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
func (m *Manager) dir() string { return caDir(m.Dir, m.DirectoryURL) }

func caDir(dir, directoryURL string) string {
	host := "ca"
	if u, err := url.Parse(directoryURL); err == nil && u.Host != "" {
		host = u.Hostname()
	}
	return filepath.Join(dir, host)
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

// keyMu serializes creating the account key, which the wildcard and the custom
// domain managers share.
var keyMu sync.Mutex

func loadOrCreateKey(path string) (*ecdsa.PrivateKey, error) {
	keyMu.Lock()
	defer keyMu.Unlock()
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

// account returns a client for the CA's account kept in dir, registering it on first use.
func account(ctx context.Context, dir, directoryURL, email string, hc *http.Client) (*acme.Client, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	accountKey, err := loadOrCreateKey(filepath.Join(dir, "account.key"))
	if err != nil {
		return nil, fmt.Errorf("account key: %w", err)
	}
	cl := &acme.Client{Key: accountKey, DirectoryURL: directoryURL, HTTPClient: hc, UserAgent: "mini-sprites"}
	acct := &acme.Account{}
	if email != "" {
		acct.Contact = []string{"mailto:" + email}
	}
	if _, err := cl.Register(ctx, acct, acme.AcceptTOS); err != nil && !errors.Is(err, acme.ErrAccountAlreadyExists) {
		return nil, fmt.Errorf("register account: %w", err)
	}
	return cl, nil
}

func (m *Manager) obtain(ctx context.Context) error {
	cl, err := account(ctx, m.dir(), m.DirectoryURL, m.Email, m.HTTPClient)
	if err != nil {
		return err
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
	return finish(ctx, cl, order.URI, m.wildcard(), m.CertFile(), m.KeyFile())
}

// finish waits for an authorized order, has it issued for name, and writes the
// pair. orderURL is taken from the response that created the order: only that
// one is sure to carry it.
func finish(ctx context.Context, cl *acme.Client, orderURL, name, certFile, keyFile string) error {
	order, err := cl.WaitOrder(ctx, orderURL)
	if err != nil {
		return fmt.Errorf("order: %w", err)
	}

	certKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	csr, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{
		Subject: pkix.Name{CommonName: name}, DNSNames: []string{name},
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
	if err := writeKey(keyFile+".tmp", certKey); err != nil {
		return err
	}
	if err := os.WriteFile(certFile+".tmp", certPEM, 0o644); err != nil {
		return err
	}
	if err := os.Rename(keyFile+".tmp", keyFile); err != nil {
		return err
	}
	return os.Rename(certFile+".tmp", certFile)
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
