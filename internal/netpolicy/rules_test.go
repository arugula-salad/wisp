package netpolicy

import (
	"strings"
	"testing"

	"github.com/jhgaylor/wisp/internal/store"
)

func rule(domain, action string) store.NetworkRule {
	return store.NetworkRule{Domain: domain, Action: action}
}

func mustCompile(t *testing.T, rules ...store.NetworkRule) *Policy {
	t.Helper()
	p, err := Compile(rules)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	return p
}

func TestRestrictive(t *testing.T) {
	for _, tc := range []struct {
		name  string
		rules []store.NetworkRule
		want  bool
	}{
		{"no rules", nil, false},
		{"allow everything", []store.NetworkRule{rule("*", "allow")}, false},
		{"allow everything plus a redundant allow", []store.NetworkRule{rule("*", "allow"), rule("github.com", "allow")}, false},
		{"allow everything except one", []store.NetworkRule{rule("*", "allow"), rule("blocked.com", "deny")}, true},
		{"allow and deny everything", []store.NetworkRule{rule("*", "allow"), rule("*", "deny")}, true},
		{"allowlist without a catch-all", []store.NetworkRule{rule("github.com", "allow")}, true},
		{"deny everything", []store.NetworkRule{rule("*", "deny")}, true},
		{"only a deny", []store.NetworkRule{rule("blocked.com", "deny")}, true},
		{"defaults", []store.NetworkRule{{Include: "defaults"}}, true},
	} {
		if got := mustCompile(t, tc.rules...).Restrictive(); got != tc.want {
			t.Errorf("%s: Restrictive() = %v, want %v", tc.name, got, tc.want)
		}
	}
	var nilPolicy *Policy
	if nilPolicy.Restrictive() || !nilPolicy.Allows("anything.example") {
		t.Error("a nil policy must be unrestricted")
	}
}

func TestAllowsPrecedence(t *testing.T) {
	p := mustCompile(t,
		rule("*", "deny"),
		rule("*.example.com", "allow"),
		rule("secret.example.com", "deny"),
		rule("*.internal.example.com", "deny"),
		rule("ok.internal.example.com", "allow"),
		rule("GitHub.com.", "allow"),
	)
	for name, want := range map[string]bool{
		"github.com":                true,
		"GITHUB.COM.":               true,  // case and the root dot are not significant
		"api.github.com":            false, // an exact rule says nothing about subdomains
		"example.com":               false, // *.example.com is subdomains only
		"www.example.com":           true,
		"a.b.example.com":           true,  // wildcards cover every depth
		"secret.example.com":        false, // exact beats wildcard
		"x.secret.example.com":      true,  // ...but only for that exact host
		"db.internal.example.com":   false, // the longer wildcard beats the shorter
		"ok.internal.example.com":   true,  // exact beats the longer wildcard too
		"a.ok.internal.example.com": false,
		"notexample.com":            false, // suffix match is on label boundaries
		"evil.com":                  false,
		"example.com.evil.com":      false,
		"www.example.com.evil.com":  false,
		"":                          false,
	} {
		if got := p.Allows(name); got != want {
			t.Errorf("Allows(%q) = %v, want %v", name, got, want)
		}
	}
}

func TestAllowlistDeniesTheUnmatched(t *testing.T) {
	// Upstream: once a policy is in force, a name that isn't allowed is refused,
	// whether or not a {"domain":"*","action":"deny"} rule spells it out.
	p := mustCompile(t, rule("github.com", "allow"), rule("*.npmjs.org", "allow"))
	if !p.Allows("github.com") || !p.Allows("registry.npmjs.org") {
		t.Error("allowed names refused")
	}
	if p.Allows("example.com") {
		t.Error("unmatched name allowed")
	}
}

func TestAllowAllWithExceptions(t *testing.T) {
	p := mustCompile(t, rule("*", "allow"), rule("blocked.com", "deny"), rule("*.ads.net", "deny"))
	for name, want := range map[string]bool{"anything.org": true, "blocked.com": false, "sub.blocked.com": true, "x.ads.net": false} {
		if got := p.Allows(name); got != want {
			t.Errorf("Allows(%q) = %v, want %v", name, got, want)
		}
	}
}

func TestConflictingRulesDeny(t *testing.T) {
	for _, rules := range [][]store.NetworkRule{
		{rule("a.com", "allow"), rule("a.com", "deny")},
		{rule("a.com", "deny"), rule("a.com", "allow")},
	} {
		if mustCompile(t, rules...).Allows("a.com") {
			t.Errorf("%v: conflicting rules for one pattern must resolve to deny", rules)
		}
	}
	// An explicit deny also beats the same pattern arriving through an include.
	p := mustCompile(t, store.NetworkRule{Include: "defaults"}, rule("github.com", "deny"))
	if p.Allows("github.com") || !p.Allows("api.github.com") {
		t.Error("deny of a name the defaults allow did not stick, or spread too far")
	}
}

func TestDefaultsInclude(t *testing.T) {
	p := mustCompile(t, store.NetworkRule{Include: "defaults"})
	for _, name := range []string{"github.com", "raw.githubusercontent.com", "registry.npmjs.org", "pypi.org", "files.pythonhosted.org", "registry-1.docker.io", "api.anthropic.com", "api.openai.com"} {
		if !p.Allows(name) {
			t.Errorf("defaults should allow %s", name)
		}
	}
	if p.Allows("example.com") {
		t.Error("defaults must not allow arbitrary names")
	}
	for _, d := range defaultDomains {
		if err := validDomain(d); err != nil {
			t.Errorf("default %q: %v", d, err)
		}
	}
}

func TestCompileRejects(t *testing.T) {
	for name, rules := range map[string][]store.NetworkRule{
		"missing action":     {rule("a.com", "")},
		"unknown action":     {rule("a.com", "permit")},
		"missing domain":     {rule("", "allow")},
		"empty rule":         {{}},
		"unknown include":    {{Include: "everything"}},
		"include and domain": {{Include: "defaults", Domain: "a.com", Action: "allow"}},
		"inner wildcard":     {rule("a.*.com", "allow")},
		"partial wildcard":   {rule("*foo.com", "allow")},
		"bare wildcard dot":  {rule("*..", "allow")},
		"double dot":         {rule("a..com", "allow")},
		"url":                {rule("https://a.com/", "allow")},
		"nft injection":      {rule("a.com } ; flush ruleset", "allow")},
		"long label":         {rule(strings.Repeat("a", 64)+".com", "allow")},
		"too many":           make([]store.NetworkRule, MaxRules+1),
	} {
		if _, err := Compile(rules); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}
