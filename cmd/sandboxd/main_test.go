package main

import "testing"

func TestPublicURL(t *testing.T) {
	for _, c := range []struct {
		raw, host string
		bad       bool
	}{
		{raw: ""},
		{raw: "https://e2b.example.com", host: "e2b.example.com"},
		{raw: "https://e2b.example.com/", host: "e2b.example.com"},
		{raw: "http://127.0.0.1:8080", host: "127.0.0.1:8080"},
		{raw: "e2b.example.com", bad: true},
		{raw: "ftp://e2b.example.com", bad: true},
		{raw: "https://e2b.example.com/api", bad: true},
		{raw: "https://e2b.example.com?x=1", bad: true},
		{raw: "https://u:p@e2b.example.com", bad: true},
		{raw: "https://", bad: true},
	} {
		u, err := publicURL(c.raw)
		switch {
		case c.bad && err == nil:
			t.Errorf("%q: accepted", c.raw)
		case !c.bad && err != nil:
			t.Errorf("%q: %v", c.raw, err)
		case !c.bad && c.raw == "" && u != nil:
			t.Errorf("empty: %v, want nil", u)
		case !c.bad && c.raw != "" && (u.Host != c.host || u.Path != ""):
			t.Errorf("%q: host %q path %q", c.raw, u.Host, u.Path)
		}
	}
}

func TestReportedDomain(t *testing.T) {
	pub, _ := publicURL("https://e2b.sandbox.example.com")
	pubPort, _ := publicURL("https://e2b.example.com:8443")
	for _, c := range []struct {
		name, domain, listen, want string
		pub                        bool
		port                       bool
	}{
		{name: "local", domain: "e2b.localhost", listen: "127.0.0.1:7820", want: "e2b.localhost:7820"},
		{name: "domain with a port", domain: "e2b.localhost:9000", listen: "127.0.0.1:7820", want: "e2b.localhost:9000"},
		{name: "behind a proxy on 443", domain: "e2b.localhost", listen: "127.0.0.1:7791", pub: true, want: "e2b.sandbox.example.com"},
		{name: "behind a proxy on another port", domain: "e2b.localhost", listen: "127.0.0.1:7791", port: true, want: "e2b.example.com:8443"},
	} {
		u := pub
		if c.port {
			u = pubPort
		} else if !c.pub {
			u = nil
		}
		if got := reportedDomain(c.domain, c.listen, u); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
}
