package server

import (
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jhgaylor/wisp/internal/store"
)

func publicTestServer(t *testing.T) *Server {
	t.Helper()
	s, st := newTestServer(t, &fakeHelper{})
	s.urlDomain = "widgets.test"
	for _, sp := range []*store.Sprite{
		{Name: "locked", URLSettings: store.URLSettings{Auth: "sprite"}},
		{Name: "v1", URLSettings: store.URLSettings{Auth: "sprite"}},
	} {
		sp.ID = store.NewID()
		if err := st.Create(sp); err != nil {
			t.Fatal(err)
		}
	}
	return s
}

func get(h http.Handler, host, path, token string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodGet, path, nil)
	r.Host = host
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

// The token that opens the management API must open nothing on the public
// listener except a sprite's own URL.
func TestPublicListenerNeverServesTheAPI(t *testing.T) {
	s := publicTestServer(t)
	if w := get(s.Handler(), "127.0.0.1:7788", "/v1/sprites", "t"); w.Code != http.StatusOK {
		t.Fatalf("sanity: the API handler answered %d to a valid request", w.Code)
	}
	for _, host := range []string{"127.0.0.1:7788", "widgets.test", "localhost", "", "a.b.widgets.test", "widgets.test.evil.example"} {
		for _, path := range []string{"/v1/sprites", "/v1/sprites/locked", "/v1/sprites/locked/exec"} {
			w := get(s.PublicHandler(), host, path, "t")
			if w.Code != http.StatusNotFound {
				t.Errorf("Host %q %s: got %d, want 404", host, path, w.Code)
			}
			if w.Header().Get("Sprite-Version") != "" || strings.Contains(w.Body.String(), "sprite") {
				t.Errorf("Host %q %s: the response gives the server away: %q", host, path, w.Body)
			}
		}
	}
}

func TestPublicSpriteURLStillChecksAuth(t *testing.T) {
	s := publicTestServer(t)
	if w := get(s.PublicHandler(), "locked.widgets.test", "/", ""); w.Code != http.StatusUnauthorized {
		t.Errorf("no token: got %d, want 401", w.Code)
	}
	if w := get(s.PublicHandler(), "locked.widgets.test:8443", "/", "wrong"); w.Code != http.StatusUnauthorized {
		t.Errorf("wrong token: got %d, want 401", w.Code)
	}
	if w := get(s.PublicHandler(), "nosuch.widgets.test", "/", "t"); w.Code != http.StatusNotFound {
		t.Errorf("unknown sprite: got %d, want 404", w.Code)
	}
	// A sprite may be named like an API path segment; that is still only its URL.
	if w := get(s.PublicHandler(), "v1.widgets.test", "/v1/sprites", ""); w.Code != http.StatusUnauthorized {
		t.Errorf("sprite named v1: got %d, want 401", w.Code)
	}
}

func TestLimitListener(t *testing.T) {
	inner, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ln := LimitListener(inner, 0, 2)
	defer ln.Close()
	accepted := make(chan net.Conn, 8)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			accepted <- c
		}
	}()
	dial := func() net.Conn {
		c, err := net.Dial("tcp", inner.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { c.Close() })
		return c
	}
	// refused reports whether the server hung up on c without serving it.
	refused := func(c net.Conn) bool {
		c.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
		_, err := c.Read(make([]byte, 1))
		ne, timeout := err.(net.Error)
		return err != nil && !(timeout && ne.Timeout())
	}

	dial()
	dial()
	first := <-accepted
	<-accepted
	if third := dial(); !refused(third) {
		t.Fatal("a third connection from one client was served with a cap of 2")
	}
	first.Close()
	if fourth := dial(); refused(fourth) {
		t.Fatal("closing a connection did not free its slot")
	}
}

func TestClientKeyGroupsAnIPv6Subscriber(t *testing.T) {
	a := clientKey(&net.TCPAddr{IP: net.ParseIP("2001:db8:1:2:aaaa::1")})
	b := clientKey(&net.TCPAddr{IP: net.ParseIP("2001:db8:1:2:bbbb::2")})
	c := clientKey(&net.TCPAddr{IP: net.ParseIP("2001:db8:1:3::1")})
	if a != b || a == c {
		t.Errorf("keys %q %q %q: want the first two equal and the third different", a, b, c)
	}
	if k := clientKey(&net.TCPAddr{IP: net.ParseIP("203.0.113.9")}); k != "203.0.113.9" {
		t.Errorf("IPv4 key %q", k)
	}
}
