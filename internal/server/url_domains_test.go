package server

import (
	"encoding/json"
	"net/http"
	"testing"
)

// A host serving two URL domains: widgets.test (the default) and arugula.test.
func twoDomainServer(t *testing.T) (*Server, http.Handler) {
	t.Helper()
	s, h := newOperatorServer(t, Options{})
	s.urlDomains = []string{"widgets.test", "arugula.test"}
	s.urlFmt = "https://%s.%s"
	return s, h
}

func rendered(t *testing.T, body []byte) spriteJSON {
	t.Helper()
	var sp spriteJSON
	if err := json.Unmarshal(body, &sp); err != nil {
		t.Fatal(err)
	}
	return sp
}

func TestASpriteIsCreatedUnderTheURLDomainItAsksFor(t *testing.T) {
	_, h := twoDomainServer(t)
	plain := rendered(t, status(t, apiCall(t, h, "POST", "/v1/sprites", `{"name":"plain"}`), http.StatusCreated))
	if plain.URL != "https://plain.widgets.test" || plain.URLDomain != "widgets.test" {
		t.Errorf("default: url %q, url_domain %q", plain.URL, plain.URLDomain)
	}
	other := rendered(t, status(t, apiCall(t, h, "POST", "/v1/sprites", `{"name":"studio","url_domain":"Arugula.Test."}`), http.StatusCreated))
	if other.URL != "https://studio.arugula.test" || other.URLDomain != "arugula.test" {
		t.Errorf("chosen: url %q, url_domain %q", other.URL, other.URLDomain)
	}
	status(t, apiCall(t, h, "POST", "/v1/sprites", `{"name":"lost","url_domain":"evil.example"}`), http.StatusBadRequest)
}

// A sprite answers only under its own domain, so the same name under the
// other domain is not its URL.
func TestASpriteAnswersOnlyUnderItsOwnURLDomain(t *testing.T) {
	s, h := twoDomainServer(t)
	status(t, apiCall(t, h, "POST", "/v1/sprites", `{"name":"old"}`), http.StatusCreated)
	status(t, apiCall(t, h, "POST", "/v1/sprites", `{"name":"new","url_domain":"arugula.test"}`), http.StatusCreated)
	for host, want := range map[string]bool{
		"old.widgets.test": true, "old.arugula.test": false,
		"new.arugula.test": true, "new.widgets.test": false,
		"new.arugula.test:443": true, "a.new.arugula.test": false, "arugula.test": false,
	} {
		if _, ok := s.spriteForHost(host); ok != want {
			t.Errorf("%s: served = %v, want %v", host, ok, want)
		}
	}
}

func TestASpriteMadeFromInsideIsUnderItsParentsURLDomain(t *testing.T) {
	s, h := twoDomainServer(t)
	status(t, apiCall(t, h, "POST", "/v1/sprites", `{"name":"studio","url_domain":"arugula.test"}`), http.StatusCreated)
	status(t, apiCall(t, h, "POST", "/v1/sprites/studio/policy/spawn", `{"enabled":true,"max_children":2}`), http.StatusNoContent)
	// It cannot choose another: its apps are on its domain.
	app := rendered(t, status(t, fromInside(t, s, "studio", "POST", "/v1/sprites", `{"name":"app-1","url_domain":"widgets.test"}`), http.StatusCreated))
	if app.URL != "https://app-1.arugula.test" {
		t.Errorf("child url %q", app.URL)
	}
}

func TestCustomDomainsCannotBeUnderAnyURLDomain(t *testing.T) {
	s := &Server{urlDomains: []string{"widgets.test", "arugula.test"}}
	for _, d := range []string{"x.widgets.test", "arugula.test", "deep.x.arugula.test"} {
		if err := s.validDomain(d); err == nil {
			t.Errorf("%s was accepted", d)
		}
	}
	if err := s.validDomain("game.example.com"); err != nil {
		t.Errorf("game.example.com: %v", err)
	}
}
