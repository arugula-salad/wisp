package server

import (
	"bufio"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLatencyQuantilesAreCloseToTheTruth(t *testing.T) {
	var a httpAgg
	for i := 1; i <= 1000; i++ {
		a.record(&HTTPRecord{Status: 200, TTFB: float64(i) / 10}) // 0.1 .. 100 ms
	}
	for _, c := range []struct{ q, want float64 }{{0.5, 50}, {0.95, 95}, {0.99, 99}} {
		got := a.Lat.quantile(c.q, a.LatMax)
		if got < c.want*0.8 || got > c.want*1.2 {
			t.Errorf("p%v = %.2f, want about %v", c.q*100, got, c.want)
		}
	}
	if got := a.Lat.quantile(1, a.LatMax); got > 100 {
		t.Errorf("p100 = %v, past the slowest request", got)
	}
	var empty latHist
	if empty.quantile(0.5, 0) != 0 {
		t.Error("an empty histogram has no median")
	}
}

func httpReport(t *testing.T, h http.Handler, c *http.Cookie, query string) HTTPReport {
	t.Helper()
	resp := uiCall(h, "GET", "/ui/api/http?"+query, "", func(r *http.Request) { r.AddCookie(c); r.Header.Set(uiHeader, "1") })
	if resp.StatusCode != 200 {
		t.Fatalf("GET /ui/api/http: %s", resp.Status)
	}
	var rep HTTPReport
	if err := json.NewDecoder(resp.Body).Decode(&rep); err != nil {
		t.Fatal(err)
	}
	return rep
}

func findRoute(rep HTTPReport, kind, sprite, key string) *HTTPRow {
	for i, r := range rep.Routes {
		if r.Kind == kind && r.Sprite == sprite && r.Key == key {
			return &rep.Routes[i]
		}
	}
	return nil
}

func TestHTTPStatsCountAPIRoutesPerSprite(t *testing.T) {
	s, h := newOperatorServer(t, Options{})
	c := uiLogin(t, h)
	apiCall(t, h, "POST", "/v1/sprites", `{"name":"one"}`)
	apiCall(t, h, "GET", "/v1/sprites/one", "")
	apiCall(t, h, "GET", "/v1/sprites/one", "")
	apiCall(t, h, "GET", "/v1/sprites/nope", "")
	uiCall(h, "GET", "/v1/sprites", "", nil) // no token

	rep := httpReport(t, h, c, "range=900&kinds=api")
	if r := findRoute(rep, kindAPI, "one", "GET /v1/sprites/{name}"); r == nil || r.N != 2 || r.Status[2] != 2 {
		t.Errorf("GET one: %+v", r)
	}
	// A name that is not a sprite does not become one in the figures.
	if r := findRoute(rep, kindAPI, "", "GET /v1/sprites/{name}"); r == nil || r.N != 1 || r.Status[4] != 1 {
		t.Errorf("GET nope: %+v", r)
	}
	if r := findRoute(rep, kindAPI, "", "(unmatched)"); r == nil || r.Status[4] != 1 {
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
	if !strings.Contains(strings.Join(rep.Kinds, ","), kindUI) {
		t.Errorf("kinds %v lack ui", rep.Kinds)
	}
	if r := httpReport(t, h, c, "range=900&kinds=api&sprite=one"); r.Totals.N != 2 {
		t.Errorf("filtered to one sprite: %d requests", r.Totals.N)
	}
	// A young daemon's steps follow what it has.
	if r := httpReport(t, h, c, "range=86400"); r.Step != 60 {
		t.Errorf("a day's range on a new daemon: step %v", r.Step)
	}
	s.httpStats.mu.Lock()
	s.httpStats.since = time.Now().Add(-48 * time.Hour)
	s.httpStats.mu.Unlock()
	for _, tc := range []struct {
		rng  string
		step float64
	}{{"3600", 10}, {"21600", 60}, {"86400", 300}} {
		if r := httpReport(t, h, c, "range="+tc.rng); r.Step != tc.step || len(r.Series) > 400 {
			t.Errorf("range %s: step %v, %d points", tc.rng, r.Step, len(r.Series))
		}
	}
}

func TestHTTPStatsSeeSpriteURLVisitorsPagesAndErrors(t *testing.T) {
	s, h := newOperatorServer(t, Options{})
	c := uiLogin(t, h)
	app := s.instrument(func(*http.Request) string { return kindSprite }, true, "", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		noteSprite(r.Context(), "site")
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
	var ghost *HTTPRecord
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
	srv := httptest.NewServer(s.instrument(func(*http.Request) string { return kindAPI }, false, "", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
	rep := s.httpStats.query(httpQuery{Range: time.Hour}, time.Now())
	if rep.Upgraded[kindAPI] != 1 || rep.Inflight[kindAPI] != 1 {
		t.Errorf("open: upgraded %v, in flight %v", rep.Upgraded, rep.Inflight)
	}
	close(release)
	deadline := time.Now().Add(5 * time.Second)
	for s.httpStats.query(httpQuery{Range: time.Hour}, time.Now()).Totals.N == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	rep = s.httpStats.query(httpQuery{Range: time.Hour}, time.Now())
	if rep.Totals.Upgrades != 1 || rep.Totals.P50 != 0 || rep.Upgraded[kindAPI] != 0 || rep.Inflight[kindAPI] != 0 {
		t.Errorf("after: %+v, upgraded %v, in flight %v", rep.Totals, rep.Upgraded, rep.Inflight)
	}
	if r := rep.Recent[0]; !r.Upgrade || r.Status != 101 {
		t.Errorf("record: %+v", r)
	}
}

func TestHTTPStatsSurviveARestart(t *testing.T) {
	a := newHTTPStats()
	now := time.Now()
	a.add(HTTPRecord{T: now, Kind: kindSprite, Sprite: "site", Path: "/", Status: 200, TTFB: 3, Page: true, Client: "203.0.113.5"}, "ua")
	a.add(HTTPRecord{T: now, Kind: kindAPI, Route: "GET /v1/sprites", Path: "/v1/sprites", Status: 500, TTFB: 40}, "")
	path := filepath.Join(t.TempDir(), "http-stats.json")
	if err := a.save(path); err != nil {
		t.Fatal(err)
	}
	b := newHTTPStats()
	if err := b.load(path, now); err != nil {
		t.Fatal(err)
	}
	// The same visitor again, after the restart: still one.
	b.add(HTTPRecord{T: now, Kind: kindSprite, Sprite: "site", Path: "/", Status: 200, TTFB: 3, Page: true, Client: "203.0.113.5"}, "ua")
	rep := b.query(httpQuery{Range: time.Hour}, now.Add(time.Second))
	if rep.Totals.N != 3 || rep.Totals.Status[5] != 1 || rep.Totals.Pages != 2 || rep.Totals.Visitors != 1 {
		t.Errorf("totals after reload: %+v", rep.Totals)
	}
	if r := findRoute(rep, kindAPI, "", "GET /v1/sprites"); r == nil || r.Status[5] != 1 || r.P50 == 0 {
		t.Errorf("route after reload: %+v", r)
	}
	if !rep.Since.Before(now) {
		t.Errorf("since %v should come from the saved run", rep.Since)
	}
}

func TestHTTPStatsCapFreeFormKeys(t *testing.T) {
	st := newHTTPStats()
	now := time.Now()
	for i := range capRoutes * 3 {
		st.add(HTTPRecord{T: now, Kind: kindSprite, Sprite: "s", Path: "/scan/" + strings.Repeat("x", i), Status: 404}, "")
	}
	rep := st.query(httpQuery{Range: time.Hour}, now)
	if len(rep.Routes) > capRoutes+1 {
		t.Errorf("%d routes kept", len(rep.Routes))
	}
	if r := findRoute(rep, kindSprite, "s", "(other)"); r == nil || r.N != capRoutes*2 {
		t.Errorf("(other): %+v", r)
	}
}

func TestUAFamily(t *testing.T) {
	for ua, want := range map[string]string{
		"": "tool/(none)",
		"Mozilla/5.0 (iPhone; CPU iPhone OS 17_0 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.0 Mobile/15E148 Safari/604.1": "browser/Safari·mobile",
		"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/140.0 Safari/537.36 Edg/140.0":                   "browser/Edge",
		"Mozilla/5.0 (X11; Linux x86_64; rv:130.0) Gecko/20100101 Firefox/130.0":                                                                  "browser/Firefox",
		"Mozilla/5.0 (compatible; Googlebot/2.1; +http://www.google.com/bot.html)":                                                                "bot/Googlebot",
		"Mozilla/5.0 zgrab/0.x": "browser/Other",
		"Expanse, a Palo Alto Networks company, searches across the global IPv4 space": "tool/other",
		"python-requests/2.31": "tool/Python",
		"Go-http-client/1.1":   "tool/Go",
		"UptimeRobot/2.0":      "bot/other",
	} {
		if got := uaFamily(ua); got != want {
			t.Errorf("uaFamily(%q) = %q, want %q", ua, got, want)
		}
	}
}
