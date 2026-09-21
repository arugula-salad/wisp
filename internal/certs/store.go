// Package certs supplies the TLS certificate for the public sprite-URL listener:
// a pair of PEM files that is re-read when it changes, and an ACME client that
// keeps such a pair current with a wildcard certificate obtained over DNS-01.
package certs

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"log/slog"
	"os"
	"sync"
	"time"
)

// recheck bounds how stale a served certificate can be after the files change.
const recheck = 30 * time.Second

// Store serves the certificate in a PEM file pair, picking up a replacement
// (a renewal, by us or by an external ACME client) without a restart.
type Store struct {
	certFile, keyFile string
	log               *slog.Logger

	mu      sync.Mutex
	cert    *tls.Certificate
	stamp   time.Time // newest mtime of the pair when cert was loaded
	checked time.Time
}

func NewStore(certFile, keyFile string, log *slog.Logger) *Store {
	return &Store{certFile: certFile, keyFile: keyFile, log: log}
}

func (s *Store) mtime() (time.Time, error) {
	c, err := os.Stat(s.certFile)
	if err != nil {
		return time.Time{}, err
	}
	k, err := os.Stat(s.keyFile)
	if err != nil {
		return time.Time{}, err
	}
	if k.ModTime().After(c.ModTime()) {
		return k.ModTime(), nil
	}
	return c.ModTime(), nil
}

// Load reads the pair now. A failure leaves the previous certificate in place.
func (s *Store) Load() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.loadLocked()
}

func (s *Store) loadLocked() error {
	s.checked = time.Now()
	stamp, err := s.mtime()
	if err != nil {
		return err
	}
	cert, err := tls.LoadX509KeyPair(s.certFile, s.keyFile)
	if err != nil {
		return err
	}
	if cert.Leaf == nil {
		if cert.Leaf, err = x509.ParseCertificate(cert.Certificate[0]); err != nil {
			return err
		}
	}
	s.cert, s.stamp = &cert, stamp
	s.log.Info("TLS certificate loaded", "names", cert.Leaf.DNSNames, "expires", cert.Leaf.NotAfter.Format(time.RFC3339))
	return nil
}

// Leaf is the certificate currently served, or nil.
func (s *Store) Leaf() *x509.Certificate {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cert == nil {
		return nil
	}
	return s.cert.Leaf
}

// GetCertificate is a tls.Config callback.
func (s *Store) GetCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if time.Since(s.checked) > recheck {
		s.checked = time.Now()
		if stamp, err := s.mtime(); err == nil && !stamp.Equal(s.stamp) {
			if err := s.loadLocked(); err != nil {
				// Most likely a renewal caught between its two writes; the next check retries.
				s.log.Warn("TLS certificate changed on disk but does not load; keeping the old one", "err", err)
			}
		}
	}
	if s.cert == nil {
		return nil, errors.New("no TLS certificate yet")
	}
	return s.cert, nil
}
