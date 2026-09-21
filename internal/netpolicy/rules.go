// Package netpolicy enforces per-sprite egress policy for sprites the kernel has
// diverted to us: a DNS listener that answers only for allowed names and
// remembers what they resolved to, and a transparent TCP proxy that connects
// only to those remembered addresses.
package netpolicy

import (
	"fmt"
	"strings"

	"github.com/jhgaylor/mini-sprites/internal/store"
)

// MaxRules bounds a policy so a client cannot make every DNS query expensive.
const MaxRules = 1000

// Policy is a compiled rule list. The zero value and nil both mean unrestricted.
type Policy struct {
	exact       map[string]bool // host -> allow
	wild        map[string]bool // "example.com" for a "*.example.com" rule -> allow
	all         *bool           // the "*" rule, if any
	restrictive bool
}

// Compile validates rules and builds the matcher. Rule order carries no
// meaning upstream ("more specific rules win"), so none is kept.
func Compile(rules []store.NetworkRule) (*Policy, error) {
	if len(rules) > MaxRules {
		return nil, fmt.Errorf("too many rules (%d, limit %d)", len(rules), MaxRules)
	}
	p := &Policy{exact: map[string]bool{}, wild: map[string]bool{}}
	denies := false
	for i, r := range rules {
		if r.Include != "" {
			if r.Domain != "" || r.Action != "" {
				return nil, fmt.Errorf("rule %d: include cannot be combined with domain or action", i)
			}
			if r.Include != "defaults" {
				return nil, fmt.Errorf("rule %d: unknown include %q (only \"defaults\" exists)", i, r.Include)
			}
			for _, d := range defaultDomains {
				p.add(d, true)
			}
			continue
		}
		if r.Action != "allow" && r.Action != "deny" {
			return nil, fmt.Errorf("rule %d: action must be \"allow\" or \"deny\"", i)
		}
		d := canonical(r.Domain)
		if err := validDomain(d); err != nil {
			return nil, fmt.Errorf("rule %d: %w", i, err)
		}
		denies = denies || r.Action == "deny"
		p.add(d, r.Action == "allow")
	}
	// A policy is an allowlist: once it has rules, whatever matches none of them is
	// refused. Only "allow everything, deny nothing" is equivalent to no policy.
	p.restrictive = len(rules) > 0 && !(p.all != nil && *p.all && !denies)
	return p, nil
}

// add records one rule. Two rules for the same pattern that disagree resolve to deny.
func (p *Policy) add(domain string, allow bool) {
	merge := func(m map[string]bool, k string) {
		if prev, ok := m[k]; ok {
			allow = allow && prev
		}
		m[k] = allow
	}
	switch {
	case domain == "*":
		if p.all != nil {
			allow = allow && *p.all
		}
		p.all = &allow
	case strings.HasPrefix(domain, "*."):
		merge(p.wild, domain[2:])
	default:
		merge(p.exact, domain)
	}
}

// Restrictive reports whether the policy can refuse anything. Sprites whose
// policy cannot stay on the kernel NAT path and never reach this package.
func (p *Policy) Restrictive() bool { return p != nil && p.restrictive }

// Allows reports whether name may be resolved. An exact rule beats a subdomain
// wildcard, a longer wildcard beats a shorter one, and "*" comes last.
// "*.example.com" covers subdomains only, not example.com itself.
func (p *Policy) Allows(name string) bool {
	if !p.Restrictive() {
		return true
	}
	name = canonical(name)
	if allow, ok := p.exact[name]; ok {
		return allow
	}
	for rest := name; ; {
		i := strings.IndexByte(rest, '.')
		if i < 0 {
			break
		}
		rest = rest[i+1:]
		if allow, ok := p.wild[rest]; ok {
			return allow
		}
	}
	return p.all != nil && *p.all
}

func canonical(name string) string {
	return strings.TrimSuffix(strings.ToLower(strings.TrimSpace(name)), ".")
}

func validDomain(d string) error {
	if d == "*" {
		return nil
	}
	host := strings.TrimPrefix(d, "*.")
	if host == "" || len(host) > 253 {
		return fmt.Errorf("invalid domain %q", d)
	}
	for _, label := range strings.Split(host, ".") {
		if label == "" || len(label) > 63 {
			return fmt.Errorf("invalid domain %q", d)
		}
		for _, c := range label {
			if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
				// Also rejects a "*" anywhere but as the whole leading label.
				return fmt.Errorf("invalid domain %q: use an exact host, \"*.example.com\" or \"*\"", d)
			}
		}
	}
	return nil
}

// defaultDomains is what {"include": "defaults"} expands to. Upstream describes
// its bundle only as "common development domains, GitHub, npm, PyPI, Docker Hub,
// and the major AI APIs among them", so this list is our reading of that.
var defaultDomains = []string{
	"github.com", "*.github.com", "*.githubusercontent.com", "*.githubassets.com", "ghcr.io", "*.ghcr.io",
	"gitlab.com", "*.gitlab.com", "bitbucket.org", "*.bitbucket.org",
	"npmjs.org", "*.npmjs.org", "npmjs.com", "*.npmjs.com", "yarnpkg.com", "*.yarnpkg.com", "nodejs.org", "*.nodejs.org",
	"pypi.org", "*.pypi.org", "pythonhosted.org", "*.pythonhosted.org",
	"docker.io", "*.docker.io", "docker.com", "*.docker.com",
	"crates.io", "*.crates.io", "rust-lang.org", "*.rust-lang.org",
	"golang.org", "*.golang.org", "go.dev", "*.go.dev", "pkg.go.dev",
	"rubygems.org", "*.rubygems.org", "hex.pm", "*.hex.pm", "packagist.org", "*.packagist.org",
	"maven.org", "*.maven.org",
	"ubuntu.com", "*.ubuntu.com", "debian.org", "*.debian.org",
	"api.anthropic.com", "api.openai.com", "generativelanguage.googleapis.com",
	"api.mistral.ai", "api.cohere.com", "api.groq.com", "openrouter.ai",
	"huggingface.co", "*.huggingface.co",
}
