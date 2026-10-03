package server

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/arugula-salad/wisp/engine"
	"github.com/arugula-salad/wisp/internal/store"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

// newTestServer is a Server over an engine with no guest network, and no
// listeners or VMs.
func newTestServer(t *testing.T) (*Server, *store.Store) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	life := engine.New(engine.Options{DataDir: dir, NoNetwork: true}, st, quiet)
	s := &Server{store: st, life: life, log: quiet, token: "t"}
	life.OnDelete(s.deleted)
	return s, st
}

func addSprite(t *testing.T, st *store.Store, name string) store.Sprite {
	t.Helper()
	sp := &store.Sprite{ID: store.NewID(), Name: name}
	if err := st.Create(sp); err != nil {
		t.Fatal(err)
	}
	return *sp
}

// spriteID is the ID of the sprite called name.
func spriteID(t *testing.T, st *store.Store, name string) string {
	t.Helper()
	sp, err := st.GetByName(store.Sprites, name)
	if err != nil {
		t.Fatal(err)
	}
	return sp.ID
}

func call(s *Server, method, path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer t")
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	return w
}

const (
	allowGithub = `{"rules":[{"domain":"github.com","action":"allow"},{"include":"defaults"}]}`
	noRules     = `{"rules":[]}`
)

// Rules come back as written: includes unexpanded, order kept. (The engine
// tests cover what a policy does: engine/egress_test.go.)
func TestPolicyRoundTrip(t *testing.T) {
	s, st := newTestServer(t)
	addSprite(t, st, "a")
	if w := call(s, "GET", "/v1/sprites/a/policy/network", ""); w.Code != 200 || strings.TrimSpace(w.Body.String()) != noRules {
		t.Fatalf("new sprite: %d %s", w.Code, w.Body)
	}
	// Restricts nothing, so it needs no guest network.
	open := `{"rules":[{"domain":"github.com","action":"allow"},{"include":"defaults"},{"domain":"*","action":"allow"}]}`
	if w := call(s, "POST", "/v1/sprites/a/policy/network", open); w.Code != http.StatusNoContent {
		t.Fatalf("set: %d %s", w.Code, w.Body)
	}
	if w := call(s, "GET", "/v1/sprites/a/policy/network", ""); strings.TrimSpace(w.Body.String()) != open {
		t.Errorf("get: %s", w.Body)
	}
	if w := call(s, "POST", "/v1/sprites/a/policy/network", noRules); w.Code != http.StatusNoContent {
		t.Fatalf("clear: %d", w.Code)
	}
	if w := call(s, "GET", "/v1/sprites/a/policy/network", ""); strings.TrimSpace(w.Body.String()) != noRules {
		t.Errorf("after clearing: %s", w.Body)
	}
}

// A restrictive policy the host cannot enforce is a 503, never "accepted but
// not enforced", and nothing reports it as in force.
func TestRestrictivePolicyRefusedIsUnavailable(t *testing.T) {
	s, st := newTestServer(t)
	addSprite(t, st, "a")
	w := call(s, "POST", "/v1/sprites/a/policy/network", allowGithub)
	if w.Code != http.StatusServiceUnavailable || !strings.Contains(w.Body.String(), "policy_unenforceable") {
		t.Fatalf("got %d %s, want 503 policy_unenforceable", w.Code, w.Body)
	}
	if w := call(s, "GET", "/v1/sprites/a/policy/network", ""); strings.TrimSpace(w.Body.String()) != noRules {
		t.Errorf("a rejected policy is being reported: %s", w.Body)
	}
}

func TestPolicyValidationAndLookup(t *testing.T) {
	s, st := newTestServer(t)
	addSprite(t, st, "a")
	for body, want := range map[string]int{
		`{"rules":[{"domain":"a.com","action":"permit"}]}`:  400,
		`{"rules":[{"include":"everything"}]}`:              400,
		`{"rules":[{"domain":"a.*.com","action":"allow"}]}`: 400,
		`not json`: 400,
	} {
		if w := call(s, "POST", "/v1/sprites/a/policy/network", body); w.Code != want {
			t.Errorf("%s: got %d, want %d", body, w.Code, want)
		}
	}
	if w := call(s, "GET", "/v1/sprites/nope/policy/network", ""); w.Code != 404 {
		t.Errorf("get unknown: %d", w.Code)
	}
	if w := call(s, "POST", "/v1/sprites/nope/policy/network", allowGithub); w.Code != 404 {
		t.Errorf("set unknown: %d", w.Code)
	}
}
