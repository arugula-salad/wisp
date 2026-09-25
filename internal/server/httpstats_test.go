package server

import (
	"bufio"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/arugula-salad/wisp/internal/httpstats"
)

func httpReport(t *testing.T, h http.Handler, c *http.Cookie, query string) httpstats.Report {
	t.Helper()
	resp := uiCall(h, "GET", "/ui/api/http?"+query, "", func(r *http.Request) { r.AddCookie(c); r.Header.Set(uiHeader, "1") })
	if resp.StatusCode != 200 {
		t.Fatalf("GET /ui/api/http: %s", resp.Status)
	}
	var rep httpstats.Report
	if err := json.NewDecoder(resp.Body).Decode(&rep); err != nil {
		t.Fatal(err)
	}
	return rep
}

func findRoute(rep httpstats.Report, kind, sprite, key string) *httpstats.Row {
	for i, r := range rep.Routes {
		if r.Kind == kind && r.Sprite == sprite && r.Key == key {
			return &rep.Routes[i]
		}
	}
	return nil
}

func TestHTTPStatsCountAPIRoutesPerSprite(t *testing.T) {
	_, h := newOperatorServer(t, Options{})
	c := uiLogin(t, h)
	apiCall(t, h, "POST", "/v1/sprites", `{"name":"one"}`)
	apiCall(t, h, "GET", "/v1/sprites/one", "")
	apiCall(t, h, "GET", "/v1/sprites/one", "")
	apiCall(t, h, "GET", "/v1/sprites/nope", "")
	uiCall(h, "GET", "/v1/sprites", "", nil) // no token

	rep := httpReport(t, h, c, "range=900&kinds=api")
	if r := findRoute(rep, httpstats.KindAPI, "one", "GET /v1/sprites/{name}"); r == nil || r.N != 2 || r.Status[2] != 2 {
		t.Errorf("GET one: %+v", r)
	}
	// A name that is not a sprite does not become one in the figures.
	if r := findRoute(rep, httpstats.KindAPI, "", "GET /v1/sprites/{name}"); r == nil || r.N != 1 || r.Status[4] != 1 {
		t.Errorf("GET nope: %+v", r)
	}
	if r := findRoute(rep, httpstats.KindAPI, "", "(unmatched)"); r == nil || r.Status[4] != 1 {
		t.Errorf("the refused request: %+v in %+v", r, rep.Routes)
	}
	if rep.Totals.N != 5 || rep.Totals.Status[2] != 3 || rep.Totals.P50 <= 0 {
		t.Errorf("totals: %+v", rep.Totals)
	}
	// 10 s steps, starting when the daemon did rather than 15 minutes back.
	if rep.Step != 10 || len(rep.Series) < 1 || len(rep.Series) > 2 || rep.Covered > 60 {
		t.Errorf("step %v, %d points, %v s covered", rep.Step, len(rep.Series), rep.Covered)
	}
	var sum int
	for _, p := range rep.Series {
		sum += p.N
	}
	if sum != 5 {
		t.Errorf("series adds up to %d, want 5", sum)
	}
	if len(rep.Recent) != 5 || rep.Recent[0].Path != "/v1/sprites" || rep.Recent[0].Status != 401 {
		t.Errorf("recent, newest first: %+v", rep.Recent)
	}
	// The UI's own requests are there too, under their own kind.
	if !strings.Contains(strings.Join(rep.Kinds, ","), httpstats.KindUI) {
		t.Errorf("kinds %v lack ui", rep.Kinds)
	}
	if r := httpReport(t, h, c, "range=900&kinds=api&sprite=one"); r.Totals.N != 2 {
		t.Errorf("filtered to one sprite: %d requests", r.Totals.N)
	}
	// A young daemon's steps follow what it has.
	if r := httpReport(t, h, c, "range=86400"); r.Step != 60 {
		t.Errorf("a day's range on a new daemon: step %v", r.Step)
	}
}

func TestHTTPStatsSeeSpriteURLVisitorsPagesAndErrors(t *testing.T) {
	s, h := newOperatorServer(t, Options{})
	c := uiLogin(t, h)
	app := s.httpStats.Instrument(func(*http.Request) string { return httpstats.KindSprite }, true, "", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		httpstats.NoteSprite(r.Context(), "site")
		if strings.HasSuffix(r.URL.Path, ".css") {
			w.Header().Set("Content-Type", "text/css")
		}
		io.WriteString(w, "<!doctype html><title>hi</title>")
	}))
	get := func(path, ua, ref, addr string) {
		req := httptest.NewRequest("GET", "https://site.widgets.test"+path+"?utm=x", nil)
		req.Header.Set("User-Agent", ua)
		req.Header.Set("Referer", ref)
		req.RemoteAddr = addr
		app.ServeHTTP(httptest.NewRecorder(), req)
	}
	chrome := "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/140.0 Safari/537.36"
	get("/", chrome, "https://news.ycombinator.com/item?id=1", "203.0.113.5:1000")
	get("/app.css", chrome, "https://site.widgets.test/", "203.0.113.5:1000")
	get("/about", chrome, "https://site.widgets.test/", "203.0.113.5:1001")
	get("/", "curl/8.5.0", "", "198.51.100.7:2000")
	// And the daemon's own refusal on the public listener.
	s.PublicHandler().ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "https://ghost.sprites.localhost/", nil))

	rep := httpReport(t, h, c, "range=3600&kinds=sprite")
	tot := rep.Totals
	if tot.N != 5 || tot.Pages != 3 || tot.Visitors != 3 || tot.Status[4] != 1 {
		// Visitors: Chrome from .5, curl from .7, and whoever asked for ghost.
		t.Errorf("totals: %+v", tot)
	}
	want := map[string]int{"/": 2, "/about": 1}
	for _, p := range rep.Pages {
		if p.Sprite != "site" || want[p.Key] != p.N {
			t.Errorf("page %+v", p)
		}
		delete(want, p.Key)
	}
	if len(want) != 0 {
		t.Errorf("pages missing: %v", want)
	}
	refs := map[string]int{}
	for _, r := range rep.Refs {
		refs[r.Key] = r.N
	}
	if refs["news.ycombinator.com"] != 1 || refs["(direct)"] != 1 || len(refs) != 2 {
		t.Errorf("referrers (a page's own site is not one): %v", refs)
	}
	agents := map[string]int{}
	for _, a := range rep.Agents {
		agents[a.Key] = a.N
	}
	if agents["browser/Chrome"] != 3 || agents["tool/curl"] != 1 {
		t.Errorf("agents: %v", agents)
	}
	if len(rep.Sprites) != 1 || rep.Sprites[0].Key != "site" || rep.Sprites[0].N != 4 || rep.Sprites[0].Visitors != 2 {
		t.Errorf("sprites: %+v", rep.Sprites)
	}
	var ghost *httpstats.Record
	for i, r := range rep.Recent {
		if r.Status == 404 {
			ghost = &rep.Recent[i]
		}
		if strings.Contains(r.Path, "utm") {
			t.Errorf("query string kept: %q", r.Path)
		}
	}
	if ghost == nil || !ghost.Public || ghost.Sprite != "" || ghost.Err != "no such sprite" {
		t.Errorf("the refusal: %+v", ghost)
	}
	if r := httpReport(t, h, c, "range=3600&kinds=sprite&listener=private"); len(r.Recent) != 0 {
		t.Errorf("nothing came in privately, got %d", len(r.Recent))
	}
}

func TestHTTPStatsCountUpgradesApartFromLatency(t *testing.T) {
	s, _ := newOperatorServer(t, Options{})
	release := make(chan struct{})
	upgraded := make(chan struct{})
	srv := httptest.NewServer(s.httpStats.Instrument(func(*http.Request) string { return httpstats.KindAPI }, false, "", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, brw, err := http.NewResponseController(w).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.Close()
		brw.WriteString("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: test\r\n\r\n")
		brw.Flush()
		close(upgraded)
		<-release
	})))
	defer srv.Close()
	conn, err := net.Dial("tcp", srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	io.WriteString(conn, "GET /x HTTP/1.1\r\nHost: x\r\nConnection: Upgrade\r\nUpgrade: test\r\n\r\n")
	if resp, err := http.ReadResponse(bufio.NewReader(conn), nil); err != nil || resp.StatusCode != 101 {
		t.Fatalf("upgrade: %v %v", resp, err)
	}
	<-upgraded
	rep := s.httpStats.Query(httpstats.Query{Range: time.Hour}, time.Now())
	if rep.Upgraded[httpstats.KindAPI] != 1 || rep.Inflight[httpstats.KindAPI] != 1 {
		t.Errorf("open: upgraded %v, in flight %v", rep.Upgraded, rep.Inflight)
	}
	close(release)
	deadline := time.Now().Add(5 * time.Second)
	for s.httpStats.Query(httpstats.Query{Range: time.Hour}, time.Now()).Totals.N == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	rep = s.httpStats.Query(httpstats.Query{Range: time.Hour}, time.Now())
	if rep.Totals.Upgrades != 1 || rep.Totals.P50 != 0 || rep.Upgraded[httpstats.KindAPI] != 0 || rep.Inflight[httpstats.KindAPI] != 0 {
		t.Errorf("after: %+v, upgraded %v, in flight %v", rep.Totals, rep.Upgraded, rep.Inflight)
	}
	if r := rep.Recent[0]; !r.Upgrade || r.Status != 101 {
		t.Errorf("record: %+v", r)
	}
}
