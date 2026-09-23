package server

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jhgaylor/wisp/internal/certs"
	"github.com/jhgaylor/wisp/internal/store"
	"github.com/miekg/dns"
)

// testZone is a recursive resolver's view of a few names, CNAMEs followed.
type testZone map[string][]dns.RR

func (z testZone) ServeDNS(w dns.ResponseWriter, q *dns.Msg) {
	m := new(dns.Msg)
	m.SetReply(q)
	rrs, ok := z[q.Question[0].Name]
	if !ok {
		m.Rcode = dns.RcodeNameError
	}
	for _, rr := range rrs {
		if rr.Header().Rrtype == q.Question[0].Qtype || rr.Header().Rrtype == dns.TypeCNAME {
			m.Answer = append(m.Answer, rr)
		}
	}
	w.WriteMsg(m)
}

func rr(t *testing.T, s string) dns.RR {
	r, err := dns.NewRR(s)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func domainTestServer(t *testing.T) (*Server, *store.Store) {
	t.Helper()
	zone := testZone{
		"game.widgets.test.":  {rr(t, "game.widgets.test. 60 IN A 203.0.113.5"), rr(t, "game.widgets.test. 60 IN AAAA 2001:db8::5")},
		"other.widgets.test.": {rr(t, "other.widgets.test. 60 IN A 203.0.113.5")},
		"play.example.test.": {rr(t, "play.example.test. 60 IN CNAME game.widgets.test."),
			rr(t, "game.widgets.test. 60 IN A 203.0.113.5"), rr(t, "game.widgets.test. 60 IN AAAA 2001:db8::5")},
		"apex.example.test.":   {rr(t, "apex.example.test. 60 IN A 203.0.113.5")},
		"split.example.test.":  {rr(t, "split.example.test. 60 IN A 203.0.113.5"), rr(t, "split.example.test. 60 IN A 198.51.100.1")},
		"stolen.example.test.": {rr(t, "stolen.example.test. 60 IN A 198.51.100.1")},
		"bare.example.test.":   {},
	}
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &dns.Server{PacketConn: pc, Handler: zone}
	go srv.ActivateAndServe()
	t.Cleanup(func() { srv.Shutdown() })

	s, st := newTestServer(t, &fakeHelper{})
	s.urlDomain = "widgets.test"
	for _, name := range []string{"game", "other"} {
		if err := st.Create(&store.Sprite{ID: store.NewID(), Name: name, URLSettings: store.URLSettings{Auth: "sprite"}}); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	// A CA that is not there, and backoffs long enough that nothing retries mid-test.
	s.EnableCustomDomains(ctx, DomainConfig{ACMEDir: t.TempDir(), DirectoryURL: "http://127.0.0.1:1/dir",
		Resolver: pc.LocalAddr().String(), PerSprite: 2, Total: 3, CheckRetry: time.Hour, OrderRetry: time.Hour},
		func(*tls.ClientHelloInfo) (*tls.Certificate, error) { return nil, nil })
	return s, st
}

func TestValidDomain(t *testing.T) {
	s := &Server{urlDomain: "widgets.test"}
	for _, d := range []string{"game.example.com", "a.b.example.co.uk", "xn--bcher-kva.example", "9gag.com"} {
		if err := s.validDomain(d); err != nil {
			t.Errorf("%s: %v", d, err)
		}
	}
	for _, d := range []string{"", "com", "game..example.com", "-a.example.com", "a_b.example.com", "1.2.3.4",
		"*.example.com", "x.localhost", "widgets.test", "game.widgets.test", "bücher.example", strings.Repeat("a", 64) + ".com"} {
		if err := s.validDomain(d); err == nil {
			t.Errorf("%q accepted", d)
		}
	}
}

func TestDomainDNSCheck(t *testing.T) {
	s, st := domainTestServer(t)
	for _, d := range []string{"play.example.test", "apex.example.test", "split.example.test", "stolen.example.test", "bare.example.test", "missing.example.test"} {
		st.AttachDomain("game", d, 0, 0)
	}
	ctx := context.Background()
	for d, want := range map[string]string{
		"play.example.test":    "", // CNAME to the sprite's own URL
		"apex.example.test":    "", // an A record naming the same address
		"split.example.test":   "resolves to 198.51.100.1, which is not an address of game.widgets.test",
		"stolen.example.test":  "198.51.100.1",
		"bare.example.test":    "no A or AAAA record",
		"missing.example.test": "no A or AAAA record",
		"loose.example.test":   "not attached",
	} {
		err := s.checkDomainDNS(ctx, d)
		if want == "" && err != nil || want != "" && (err == nil || !strings.Contains(err.Error(), want)) {
			t.Errorf("%s: got %v, want %q", d, err, want)
		}
	}
}

func TestDomainsAPI(t *testing.T) {
	s, st := domainTestServer(t)
	decode := func(w interface{ Result() *http.Response }) certs.DomainStatus {
		var out certs.DomainStatus
		json.NewDecoder(w.Result().Body).Decode(&out)
		return out
	}

	w := call(s, "POST", "/v1/sprites/game/domains", `{"domain":"Play.Example.Test."}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("attach: %d %s", w.Code, w.Body)
	}
	if got := decode(w); got.Domain != "play.example.test" || got.Status != certs.DomainPending {
		t.Fatalf("attach answered %+v", got)
	}
	if w := call(s, "POST", "/v1/sprites/game/domains", `{"domain":"play.example.test"}`); w.Code != http.StatusOK {
		t.Errorf("re-attach: %d %s", w.Code, w.Body)
	}
	if w := call(s, "POST", "/v1/sprites/other/domains", `{"domain":"play.example.test"}`); w.Code != http.StatusConflict {
		t.Errorf("a domain on two sprites: %d %s", w.Code, w.Body)
	}
	if w := call(s, "POST", "/v1/sprites/game/domains", `{"domain":"x.widgets.test"}`); w.Code != http.StatusBadRequest {
		t.Errorf("a name under the url domain: %d %s", w.Code, w.Body)
	}
	if w := call(s, "POST", "/v1/sprites/nosuch/domains", `{"domain":"q.example.test"}`); w.Code != http.StatusNotFound {
		t.Errorf("no such sprite: %d", w.Code)
	}

	// Caps: two per sprite, three in all.
	call(s, "POST", "/v1/sprites/game/domains", `{"domain":"apex.example.test"}`)
	if w := call(s, "POST", "/v1/sprites/game/domains", `{"domain":"third.example.test"}`); w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "this sprite") {
		t.Errorf("per-sprite cap: %d %s", w.Code, w.Body)
	}
	call(s, "POST", "/v1/sprites/other/domains", `{"domain":"o1.example.test"}`)
	if w := call(s, "POST", "/v1/sprites/other/domains", `{"domain":"o2.example.test"}`); w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "this host") {
		t.Errorf("host cap: %d %s", w.Code, w.Body)
	}

	// The manager works each domain in the background: the DNS check decides
	// whether the CA is asked at all.
	for start := time.Now(); ; time.Sleep(20 * time.Millisecond) {
		var list struct{ Domains []certs.DomainStatus }
		json.NewDecoder(call(s, "GET", "/v1/sprites/game/domains", "").Body).Decode(&list)
		byName := map[string]certs.DomainStatus{}
		for _, d := range list.Domains {
			byName[d.Domain] = d
		}
		play, other := byName["play.example.test"], call(s, "GET", "/v1/sprites/other/domains/o1.example.test", "")
		o1 := decode(other)
		if play.Status == certs.DomainError && strings.Contains(o1.Reason, "waiting for DNS") {
			if !strings.Contains(o1.Reason, "no A or AAAA record") || o1.NextAttempt == nil {
				t.Errorf("o1: %+v", o1)
			}
			break // play passed the check and reached the (absent) CA
		}
		if time.Since(start) > 10*time.Second {
			t.Fatalf("play=%+v o1=%+v", play, o1)
		}
	}

	// Requests for the domain go to its sprite, under the sprite's URL auth.
	if w := get(s.PublicHandler(), "play.example.test", "/", ""); w.Code != http.StatusUnauthorized {
		t.Errorf("custom domain without a token: %d, want the sprite's 401", w.Code)
	}
	if w := get(s.PublicHandler(), "PLAY.example.test:443", "/", ""); w.Code != http.StatusUnauthorized {
		t.Errorf("custom domain, other spelling: %d", w.Code)
	}
	if w := get(s.PublicHandler(), "stranger.example.test", "/", ""); w.Code != http.StatusNotFound {
		t.Errorf("an unknown host: %d, want 404", w.Code)
	}

	if w := call(s, "DELETE", "/v1/sprites/game/domains/apex.example.test", ""); w.Code != http.StatusNoContent {
		t.Errorf("detach: %d %s", w.Code, w.Body)
	}
	if w := call(s, "GET", "/v1/sprites/game/domains/apex.example.test", ""); w.Code != http.StatusNotFound {
		t.Errorf("a detached domain: %d", w.Code)
	}
	if _, ok := s.domains.mgr.Status("apex.example.test"); ok {
		t.Error("the manager still holds a detached domain")
	}

	// Deleting the sprite frees its domains.
	if w := call(s, "DELETE", "/v1/sprites/game", ""); w.Code != http.StatusNoContent {
		t.Fatalf("delete sprite: %d %s", w.Code, w.Body)
	}
	if _, ok := st.DomainOwner("play.example.test"); ok {
		t.Error("a deleted sprite still owns its domain")
	}
	if _, ok := s.domains.mgr.Status("play.example.test"); ok {
		t.Error("the manager still holds a deleted sprite's domain")
	}
	if w := call(s, "POST", "/v1/sprites/other/domains", `{"domain":"play.example.test"}`); w.Code != http.StatusCreated {
		t.Errorf("a freed domain: %d %s", w.Code, w.Body)
	}
}

func TestDomainsDisabledWithoutPublicListener(t *testing.T) {
	s, st := newTestServer(t, &fakeHelper{})
	s.urlDomain = "widgets.test"
	addSprite(t, st, "game")
	if w := call(s, "POST", "/v1/sprites/game/domains", `{"domain":"play.example.test"}`); w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "--public-listen") {
		t.Errorf("attach: %d %s", w.Code, w.Body)
	}
	st.AttachDomain("game", "play.example.test", 0, 0) // e.g. a daemon restarted without --public-listen
	if w := call(s, "GET", "/v1/sprites/game/domains", ""); !strings.Contains(w.Body.String(), `"inactive"`) {
		t.Errorf("list: %d %s", w.Code, w.Body)
	}
}
