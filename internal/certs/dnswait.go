package certs

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/miekg/dns"
)

// recursor finds the zone's nameservers. A public one, not the system's: a home
// resolver may serve a split-horizon view of the very domain we are validating.
const recursor = "1.1.1.1:53"

// nameservers returns the authoritative servers of the zone that holds fqdn.
func nameservers(ctx context.Context, fqdn string) ([]string, error) {
	c := &dns.Client{Timeout: 5 * time.Second}
	labels := dns.SplitDomainName(fqdn)
	for i := range labels {
		zone := dns.Fqdn(strings.Join(labels[i:], "."))
		q := new(dns.Msg)
		q.SetQuestion(zone, dns.TypeNS)
		in, _, err := c.ExchangeContext(ctx, q, recursor)
		if err != nil {
			return nil, err
		}
		var out []string
		for _, rr := range in.Answer {
			if ns, ok := rr.(*dns.NS); ok {
				out = append(out, ns.Ns)
			}
		}
		if len(out) > 0 {
			return out, nil
		}
	}
	return nil, fmt.Errorf("no nameservers found for %s", fqdn)
}

func hasTXT(ctx context.Context, server, fqdn, value string) bool {
	addrs, err := net.DefaultResolver.LookupHost(ctx, strings.TrimSuffix(server, "."))
	if err != nil || len(addrs) == 0 {
		return false
	}
	q := new(dns.Msg)
	q.SetQuestion(dns.Fqdn(fqdn), dns.TypeTXT)
	q.RecursionDesired = false
	c := &dns.Client{Timeout: 5 * time.Second}
	in, _, err := c.ExchangeContext(ctx, q, net.JoinHostPort(addrs[0], "53"))
	if err != nil {
		return false
	}
	for _, rr := range in.Answer {
		if txt, ok := rr.(*dns.TXT); ok && strings.Join(txt.Txt, "") == value {
			return true
		}
	}
	return false
}

// waitAuthoritative blocks until every authoritative server answers with the
// challenge value. Asking the CA to validate before then burns one of its
// hourly failed-validation allowance for nothing.
func waitAuthoritative(ctx context.Context, fqdn, value string) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	servers, err := nameservers(ctx, fqdn)
	if err != nil {
		return err
	}
	for {
		missing := ""
		for _, ns := range servers {
			if !hasTXT(ctx, ns, fqdn, value) {
				missing = ns
				break
			}
		}
		if missing == "" {
			return nil
		}
		select {
		case <-ctx.Done():
			return errors.New("timed out waiting for " + missing)
		case <-time.After(3 * time.Second):
		}
	}
}
