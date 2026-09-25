package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func bearerCall(h http.Handler, method, path, key string, set func(*http.Request)) *http.Response {
	return uiCall(h, method, path, "", func(r *http.Request) {
		r.Header.Set("Authorization", "Bearer "+key)
		if set != nil {
			set(r)
		}
	})
}

func flipLast(k string) string {
	if strings.HasSuffix(k, "0") {
		return k[:len(k)-1] + "1"
	}
	return k[:len(k)-1] + "0"
}

func TestAPIKeysAuthenticateByScopeAndRevoke(t *testing.T) {
	s, h := newOperatorServer(t, Options{})
	admin, err := s.keys.create("lobby", ScopeAdmin)
	if err != nil {
		t.Fatal(err)
	}
	read, err := s.keys.create("grafana", ScopeRead)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(admin.Key, "wisp_"+admin.ID+"_") {
		t.Errorf("key %q does not carry its id %s", admin.Key, admin.ID)
	}
	b, _ := os.ReadFile(filepath.Join(s.opts.DataDir, keysFile))
	if strings.Contains(string(b), admin.Key) || strings.Contains(string(b), read.Key) {
		t.Fatal("keys.json holds a key, not just its hash")
	}
	if st, _ := os.Stat(filepath.Join(s.opts.DataDir, keysFile)); st.Mode().Perm() != 0o600 {
		t.Errorf("keys.json mode %v", st.Mode().Perm())
	}
	ws := func(r *http.Request) {
		r.Header.Set("Connection", "Upgrade")
		r.Header.Set("Upgrade", "websocket")
	}
	for _, c := range []struct {
		name, method, path, key string
		set                     func(*http.Request)
		want                    int
	}{
		{"admin lists", "GET", "/v1/sprites", admin.Key, nil, 200},
		{"admin creates", "POST", "/v1/sprites", admin.Key, nil, 400}, // past auth: the empty body is refused
		{"read lists", "GET", "/v1/sprites", read.Key, nil, 200},
		{"read may not create", "POST", "/v1/sprites", read.Key, nil, 403},
		{"read may not delete", "DELETE", "/v1/sprites/x", read.Key, nil, 403},
		{"read may not open a socket", "GET", "/v1/sprites/x/exec", read.Key, ws, 403},
		{"a wrong secret", "GET", "/v1/sprites", flipLast(admin.Key), nil, 401},
		{"another key's id", "GET", "/v1/sprites", "wisp_" + read.ID + admin.Key[len("wisp_")+8:], nil, 401},
		{"nothing", "GET", "/v1/sprites", "", nil, 401},
		{"root token", "GET", "/v1/sprites", "tok", nil, 200},
	} {
		if got := bearerCall(h, c.method, c.path, c.key, c.set).StatusCode; got != c.want {
			t.Errorf("%s: %d, want %d", c.name, got, c.want)
		}
	}
	if got := s.keys.list(); got[0].LastUsedAt == nil || got[0].Hash != "" {
		t.Errorf("list after use: %+v", got[0])
	}

	// Keys survive a restart; revoking one stops it at once and leaves the rest.
	s2, h2 := s, h
	s2.keys = openKeyring(s.opts.DataDir)
	if _, err := s2.keys.revoke("lobby"); err != nil {
		t.Fatal(err)
	}
	if got := bearerCall(h2, "GET", "/v1/sprites", admin.Key, nil).StatusCode; got != 401 {
		t.Errorf("revoked key: %d", got)
	}
	if got := bearerCall(h2, "GET", "/v1/sprites", read.Key, nil).StatusCode; got != 200 {
		t.Errorf("the other key after a revoke: %d", got)
	}
	if _, err := s2.keys.revoke("lobby"); err != errNoSuchKey {
		t.Errorf("second revoke: %v", err)
	}
}

func TestAPIKeyCreateValidates(t *testing.T) {
	s, _ := newOperatorServer(t, Options{})
	for _, c := range [][2]string{{"", ScopeAdmin}, {"x", "root"}, {"bad\nname", ScopeRead}, {strings.Repeat("n", 65), ScopeRead}} {
		if _, err := s.keys.create(c[0], c[1]); err == nil {
			t.Errorf("create(%q, %q) succeeded", c[0], c[1])
		}
	}
	if _, err := s.keys.create("dup", ScopeRead); err != nil {
		t.Fatal(err)
	}
	if _, err := s.keys.create("dup", ScopeAdmin); err == nil {
		t.Error("two keys with one name")
	}
}

// An unreadable keys.json fails closed: no key works, none can be made over it,
// and the root token still does.
func TestBrokenKeyringFailsClosed(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, keysFile), []byte("{not json"), 0o600)
	k := openKeyring(dir)
	if k.broken == nil {
		t.Fatal("garbage keys.json read fine")
	}
	if _, err := k.create("x", ScopeAdmin); err == nil {
		t.Error("created a key over a broken keys.json")
	}
	if b, _ := os.ReadFile(filepath.Join(dir, keysFile)); string(b) != "{not json" {
		t.Error("broken keys.json was overwritten")
	}
	s := &Server{token: "tok", keys: k}
	if _, ok := s.authenticate("tok"); !ok {
		t.Error("root token refused")
	}
}

func TestSpriteURLAuthWantsAnAdminKey(t *testing.T) {
	s, h := newOperatorServer(t, Options{})
	read, _ := s.keys.create("viewer", ScopeRead)
	resp := apiCall(t, h, "POST", "/v1/sprites", `{"name":"app"}`)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create: %s", resp.Status)
	}
	host := func(r *http.Request) { r.Host = "app.sprites.localhost" }
	if got := bearerCall(h, "GET", "/", read.Key, host).StatusCode; got != http.StatusUnauthorized {
		t.Errorf("a read key opened a private sprite URL: %d", got)
	}
}

func TestDashboardSessionsFollowTheirKey(t *testing.T) {
	s, h := newOperatorServer(t, Options{})
	admin, _ := s.keys.create("me", ScopeAdmin)
	read, _ := s.keys.create("viewer", ScopeRead)
	login := func(key string) *http.Cookie {
		resp := uiCall(h, "POST", "/ui/login", `{"token":"`+key+`"}`, nil)
		if resp.StatusCode != http.StatusNoContent {
			t.Fatalf("login with %s: %s", key[:13], resp.Status)
		}
		return resp.Cookies()[0]
	}
	as := func(c *http.Cookie) func(*http.Request) {
		return func(r *http.Request) { r.AddCookie(c); r.Header.Set(uiHeader, "1") }
	}
	ca, cr := login(admin.Key), login(read.Key)

	resp := uiCall(h, "GET", "/ui/api/whoami", "", as(cr))
	var who map[string]string
	json.NewDecoder(resp.Body).Decode(&who)
	if who["name"] != "viewer" || who["scope"] != ScopeRead {
		t.Errorf("whoami: %v", who)
	}
	for _, c := range []struct {
		name, method, path, body string
		cookie                   *http.Cookie
		want                     int
	}{
		{"read session sees status", "GET", "/ui/api/status", "", cr, 200},
		{"read session may not see keys", "GET", "/ui/api/keys", "", cr, 403},
		{"read session may not wake", "POST", "/ui/api/sprites/x/wake", "", cr, 403},
		{"read session may not create", "POST", "/v1/sprites", `{"name":"y"}`, cr, 403},
		{"admin session lists keys", "GET", "/ui/api/keys", "", ca, 200},
		{"admin session makes a key", "POST", "/ui/api/keys", `{"name":"ci","scope":"read"}`, ca, 201},
		{"admin session revokes a key", "DELETE", "/ui/api/keys/ci", "", ca, 200},
	} {
		if got := uiCall(h, c.method, c.path, c.body, as(c.cookie)).StatusCode; got != c.want {
			t.Errorf("%s: %d, want %d", c.name, got, c.want)
		}
	}

	// Revoking the key ends the sessions signed in with it, and only those.
	s.keys.revoke(read.ID)
	if got := uiCall(h, "GET", "/ui/api/status", "", as(cr)).StatusCode; got != 401 {
		t.Errorf("session of a revoked key: %d", got)
	}
	if got := uiCall(h, "GET", "/ui/api/status", "", as(ca)).StatusCode; got != 200 {
		t.Errorf("the other session: %d", got)
	}
	// A cookie cannot be moved to another key id.
	forged := &http.Cookie{Name: uiCookie, Value: "root" + ca.Value[len(admin.ID):]}
	if got := uiCall(h, "GET", "/ui/api/status", "", as(forged)).StatusCode; got != 401 {
		t.Errorf("forged root session: %d", got)
	}
}

func TestOperatorSocketManagesKeys(t *testing.T) {
	s, h := newOperatorServer(t, Options{})
	op := s.StatusHandler("127.0.0.1:0")
	rec := httptest.NewRecorder()
	op.ServeHTTP(rec, httptest.NewRequest("POST", "/keys", strings.NewReader(`{"name":"lobby"}`)))
	var k NewAPIKey
	if rec.Code != http.StatusCreated || json.Unmarshal(rec.Body.Bytes(), &k) != nil || k.Scope != ScopeAdmin {
		t.Fatalf("create over the socket: %d %s", rec.Code, rec.Body)
	}
	if got := bearerCall(h, "GET", "/v1/sprites", k.Key, nil).StatusCode; got != 200 {
		t.Errorf("new key: %d", got)
	}
	rec = httptest.NewRecorder()
	op.ServeHTTP(rec, httptest.NewRequest("DELETE", "/keys/"+k.ID, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("revoke over the socket: %d %s", rec.Code, rec.Body)
	}
	if got := bearerCall(h, "GET", "/v1/sprites", k.Key, nil).StatusCode; got != 401 {
		t.Errorf("revoked key: %d", got)
	}
}
