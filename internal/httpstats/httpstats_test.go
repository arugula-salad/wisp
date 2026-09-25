package httpstats

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLatencyQuantilesAreCloseToTheTruth(t *testing.T) {
	var a agg
	for i := 1; i <= 1000; i++ {
		a.record(&Record{Status: 200, TTFB: float64(i) / 10}) // 0.1 .. 100 ms
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

func findRoute(rep Report, kind, sprite, key string) *Row {
	for i, r := range rep.Routes {
		if r.Kind == kind && r.Sprite == sprite && r.Key == key {
			return &rep.Routes[i]
		}
	}
	return nil
}

// A daemon that has held figures for days sizes a day's steps by the day, and
// the last hour's by the fine buckets it has had since it began.
func TestStepsFollowWhatIsHeld(t *testing.T) {
	st := New(nil)
	now := time.Now()
	st.since = now.Add(-48 * time.Hour)
	for _, tc := range []struct {
		rng  time.Duration
		step float64
	}{{time.Hour, 10}, {6 * time.Hour, 60}, {24 * time.Hour, 300}} {
		if r := st.Query(Query{Range: tc.rng}, now); r.Step != tc.step || len(r.Series) > 400 {
			t.Errorf("range %v: step %v, %d points", tc.rng, r.Step, len(r.Series))
		}
	}
}

func TestSurviveARestart(t *testing.T) {
	a := New(nil)
	now := time.Now()
	a.add(Record{T: now, Kind: KindSprite, Sprite: "site", Path: "/", Status: 200, TTFB: 3, Page: true, Client: "203.0.113.5"}, "ua")
	a.add(Record{T: now, Kind: KindAPI, Route: "GET /v1/sprites", Path: "/v1/sprites", Status: 500, TTFB: 40}, "")
	path := filepath.Join(t.TempDir(), "http-stats.json")
	if err := a.Save(path); err != nil {
		t.Fatal(err)
	}
	b := New(nil)
	if err := b.Load(path, now); err != nil {
		t.Fatal(err)
	}
	// The same visitor again, after the restart: still one.
	b.add(Record{T: now, Kind: KindSprite, Sprite: "site", Path: "/", Status: 200, TTFB: 3, Page: true, Client: "203.0.113.5"}, "ua")
	rep := b.Query(Query{Range: time.Hour}, now.Add(time.Second))
	if rep.Totals.N != 3 || rep.Totals.Status[5] != 1 || rep.Totals.Pages != 2 || rep.Totals.Visitors != 1 {
		t.Errorf("totals after reload: %+v", rep.Totals)
	}
	if r := findRoute(rep, KindAPI, "", "GET /v1/sprites"); r == nil || r.Status[5] != 1 || r.P50 == 0 {
		t.Errorf("route after reload: %+v", r)
	}
	if !rep.Since.Before(now) {
		t.Errorf("since %v should come from the saved run", rep.Since)
	}
}

func TestCapFreeFormKeys(t *testing.T) {
	st := New(nil)
	now := time.Now()
	for i := range capRoutes * 3 {
		st.add(Record{T: now, Kind: KindSprite, Sprite: "s", Path: "/scan/" + strings.Repeat("x", i), Status: 404}, "")
	}
	rep := st.Query(Query{Range: time.Hour}, now)
	if len(rep.Routes) > capRoutes+1 {
		t.Errorf("%d routes kept", len(rep.Routes))
	}
	if r := findRoute(rep, KindSprite, "s", "(other)"); r == nil || r.N != capRoutes*2 {
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
