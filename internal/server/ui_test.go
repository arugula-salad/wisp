package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func uiCall(h http.Handler, method, path, body string, set func(*http.Request)) *http.Response {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if set != nil {
		set(req)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Result()
}

func uiLogin(t *testing.T, h http.Handler) *http.Cookie {
	t.Helper()
	resp := uiCall(h, "POST", "/ui/login", `{"token":"tok"}`, nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("login: %s", resp.Status)
	}
	for _, c := range resp.Cookies() {
		if c.Name == uiCookie {
			if !c.HttpOnly || c.SameSite != http.SameSiteStrictMode {
				t.Errorf("cookie must be HttpOnly and SameSite=Strict: %+v", c)
			}
			return c
		}
	}
	t.Fatal("login set no cookie")
	return nil
}

func TestUIServesThePageWithoutAToken(t *testing.T) {
	_, h := newOperatorServer(t, Options{})
	for path, want := range map[string]int{"/": http.StatusFound, "/ui": http.StatusFound, "/ui/": http.StatusOK,
		"/ui/app.js": http.StatusOK, "/ui/charts.js": http.StatusOK, "/ui/vendor/xterm.js": http.StatusOK} {
		if got := uiCall(h, "GET", path, "", nil).StatusCode; got != want {
			t.Errorf("GET %s = %d, want %d", path, got, want)
		}
	}
	if resp := uiCall(h, "GET", "/ui/", "", nil); !strings.Contains(resp.Header.Get("Content-Security-Policy"), "frame-ancestors 'none'") {
		t.Errorf("page served without a CSP: %v", resp.Header)
	}
}

func TestUILoginRefusesAWrongToken(t *testing.T) {
	_, h := newOperatorServer(t, Options{})
	resp := uiCall(h, "POST", "/ui/login", `{"token":"nope"}`, nil)
	if resp.StatusCode != http.StatusUnauthorized || len(resp.Cookies()) != 0 {
		t.Fatalf("wrong token: %s, cookies %v", resp.Status, resp.Cookies())
	}
}

// The cookie authorizes only alongside the UI header, or on a same-origin
// WebSocket: another origin can send the cookie but can add neither.
func TestUICookieNeedsTheHeaderOrASameOriginSocket(t *testing.T) {
	_, h := newOperatorServer(t, Options{})
	c := uiLogin(t, h)
	withCookie := func(r *http.Request) { r.AddCookie(c) }
	header := func(r *http.Request) { r.AddCookie(c); r.Header.Set(uiHeader, "1") }
	socket := func(origin string) func(*http.Request) {
		return func(r *http.Request) {
			r.AddCookie(c)
			r.Host = "127.0.0.1:7788"
			r.Header.Set("Connection", "Upgrade")
			r.Header.Set("Upgrade", "websocket")
			r.Header.Set("Origin", origin)
		}
	}
	cases := []struct {
		name string
		path string
		set  func(*http.Request)
		ok   bool
	}{
		{"nothing", "/v1/sprites", nil, false},
		{"cookie alone", "/v1/sprites", withCookie, false},
		{"cookie alone on ui api", "/ui/api/status", withCookie, false},
		{"cookie and header", "/v1/sprites", header, true},
		{"cookie and header on ui api", "/ui/api/status", header, true},
		{"header without cookie", "/v1/sprites", func(r *http.Request) { r.Header.Set(uiHeader, "1") }, false},
		{"same-origin socket", "/v1/sprites", socket("http://127.0.0.1:7788"), true},
		{"cross-origin socket", "/v1/sprites", socket("http://evil.sprites.localhost:7788"), false},
		{"socket without origin", "/v1/sprites", socket(""), false},
		{"forged cookie", "/v1/sprites", func(r *http.Request) {
			r.AddCookie(&http.Cookie{Name: uiCookie, Value: strings.Repeat("0", 64)})
			r.Header.Set(uiHeader, "1")
		}, false},
	}
	for _, tc := range cases {
		got := uiCall(h, "GET", tc.path, "", tc.set).StatusCode
		if (got != http.StatusUnauthorized) != tc.ok {
			t.Errorf("%s: GET %s = %d, want authorized=%v", tc.name, tc.path, got, tc.ok)
		}
	}
}

func TestUIMetricsRecordSpritesAndTheirTransitions(t *testing.T) {
	s, h := newOperatorServer(t, Options{})
	c := uiLogin(t, h)
	now := time.Now()
	s.metrics.sample(now) // the baseline: events are changes against a previous sample
	if resp := apiCall(t, h, "POST", "/v1/sprites", `{"name":"one"}`); resp.StatusCode != http.StatusCreated {
		t.Fatalf("create: %s", resp.Status)
	}
	s.metrics.sample(now.Add(metricsEvery))

	resp := uiCall(h, "GET", "/ui/api/metrics", "", func(r *http.Request) { r.AddCookie(c); r.Header.Set(uiHeader, "1") })
	var m Metrics
	if err := json.NewDecoder(resp.Body).Decode(&m); err != nil {
		t.Fatal(err)
	}
	if len(m.Points) < 2 {
		t.Fatalf("want at least 2 points, got %d", len(m.Points))
	}
	last := m.Points[len(m.Points)-1]
	if last.Cold != 1 || last.Sprites["one"].State != "cold" || last.HostMemTotal == 0 {
		t.Errorf("last point: %+v", last)
	}
	var created bool
	for _, e := range m.Events {
		created = created || (e.Name == "one" && e.From == "" && e.To == "cold")
	}
	if !created {
		t.Errorf("no creation event in %+v", m.Events)
	}
	if _, ok := m.Disk["one"]; !ok {
		t.Errorf("no disk figures for the sprite: %+v", m.Disk)
	}

	// since= returns only what is newer.
	resp = uiCall(h, "GET", "/ui/api/metrics?since="+last.T.Format(time.RFC3339), "", func(r *http.Request) { r.AddCookie(c); r.Header.Set(uiHeader, "1") })
	var later Metrics
	json.NewDecoder(resp.Body).Decode(&later)
	if len(later.Points) != 0 || len(later.Events) != 0 {
		t.Errorf("since the last point: %d points, %d events", len(later.Points), len(later.Events))
	}
}

func TestUICoolRefusesASpriteThatIsNotWarm(t *testing.T) {
	_, h := newOperatorServer(t, Options{})
	c := uiLogin(t, h)
	apiCall(t, h, "POST", "/v1/sprites", `{"name":"one"}`)
	resp := uiCall(h, "POST", "/ui/api/sprites/one/cool", "", func(r *http.Request) { r.AddCookie(c); r.Header.Set(uiHeader, "1") })
	if resp.StatusCode != http.StatusConflict {
		t.Errorf("cooling a cold sprite: %s", resp.Status)
	}
}
