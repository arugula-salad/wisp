package server

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/json"
	"hash/fnv"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"
)

// HTTP request metrics for the web UI's Traffic and Ops pages. Every request
// the daemon serves (the API, sprite URLs on either listener, the web UI
// itself, and calls from inside sprites) is timed and counted into buckets:
// 10-second ones for the last hour, which the ops view draws live, and
// one-minute ones for the last day, which also carry the free-form dimensions
// (routes, pages, referrers, browsers, visitors). The minute buckets are saved
// to the data directory so a restart does not lose the day.
//
// Latency is time to the response headers, not to the last byte: sprite apps
// stream and hold WebSockets open, so the full duration measures the client.
// Upgraded connections are counted but kept out of the latency figures.

const (
	httpFineStep   = 10 * time.Second
	httpFineKeep   = time.Hour
	httpCoarseStep = time.Minute
	httpCoarseKeep = 24 * time.Hour
	httpRecentKeep = 500
	httpSlowPerMin = 5
	httpSaveEvery  = time.Minute

	// Per-minute caps on the free-form dimensions, so a scanner walking random
	// paths costs bounded memory. Past a cap, new keys count as "(other)".
	capRoutes   = 60
	capPages    = 40
	capRefs     = 20
	capAgents   = 20
	capVisitors = 2000
	maxPathLen  = 96

	// Latency histogram: bin i holds (edge(i-1), edge(i)] with edge(i) =
	// 0.25 ms·√2^i, so each bin is about 41% wide. The last bin is open ended.
	latBins = 42
	latBase = 0.25
)

func latEdge(i int) float64 { return latBase * math.Pow(math.Sqrt2, float64(i)) }

func latBin(ms float64) int {
	if ms <= latBase {
		return 0
	}
	i := int(math.Ceil(math.Log(ms/latBase) / math.Log(math.Sqrt2)))
	return min(max(i, 0), latBins-1)
}

type latHist [latBins]uint32

func (h *latHist) add(o *latHist) {
	for i := range h {
		h[i] += o[i]
	}
}

// quantile interpolates within the bin the q-th request falls in, and never
// reports more than the slowest request actually seen.
func (h *latHist) quantile(q, most float64) float64 {
	var total float64
	for _, c := range h {
		total += float64(c)
	}
	if total == 0 {
		return 0
	}
	target := q * total
	var cum float64
	for i, c := range h {
		if c == 0 || cum+float64(c) < target {
			cum += float64(c)
			continue
		}
		lo := 0.0
		if i > 0 {
			lo = latEdge(i - 1)
		}
		v := lo + (latEdge(i)-lo)*(target-cum)/float64(c)
		if most > 0 && v > most {
			v = most
		}
		return v
	}
	return most
}

// HTTPRecord is one finished request.
type HTTPRecord struct {
	T        time.Time `json:"t"` // when the response finished
	Kind     string    `json:"kind"`
	Public   bool      `json:"public,omitempty"` // arrived on the internet-facing listener
	Sprite   string    `json:"sprite,omitempty"`
	Method   string    `json:"method"`
	Path     string    `json:"path"`            // without the query string
	Route    string    `json:"route,omitempty"` // the pattern it matched, for the API, the UI and guest calls
	Status   int       `json:"status"`
	TTFB     float64   `json:"ttfb_ms"`
	Dur      float64   `json:"dur_ms"`
	Bytes    int64     `json:"bytes"`
	Wake     float64   `json:"wake_ms,omitempty"` // time spent starting the sprite, part of TTFB
	WakeFrom string    `json:"wake_from,omitempty"`
	Upgrade  bool      `json:"upgrade,omitempty"`
	Page     bool      `json:"page,omitempty"` // an HTML page served from a sprite URL
	Client   string    `json:"client,omitempty"`
	Agent    string    `json:"agent,omitempty"`
	Referrer string    `json:"referrer,omitempty"`
	Err      string    `json:"err,omitempty"` // why the daemon, not the app, answered with an error

	visitor  uint64
	internal bool // referred by a page of the same site: navigation, not a referral
}

// Request kinds.
const (
	kindSprite = "sprite" // a sprite's URL, proxied to its app
	kindAPI    = "api"    // the management API
	kindUI     = "ui"     // the web UI and its own endpoints
	kindGuest  = "guest"  // a sprite calling the host over its guest channel
)

// httpAgg is what every bucket counts, per kind and sprite.
type httpAgg struct {
	N         int     `json:"n"`
	Status    [6]int  `json:"status"` // by class: [2] is 2xx, and so on
	Upgrades  int     `json:"upgrades,omitempty"`
	Pages     int     `json:"pages,omitempty"`
	Bytes     int64   `json:"bytes,omitempty"`
	Wakes     int     `json:"wakes,omitempty"`
	ColdWakes int     `json:"cold_wakes,omitempty"`
	WakeMs    float64 `json:"wake_ms,omitempty"` // summed
	WakeMax   float64 `json:"wake_max_ms,omitempty"`
	Lat       latHist `json:"lat"` // TTFB, upgrades excluded
	LatSum    float64 `json:"lat_sum_ms,omitempty"`
	LatMax    float64 `json:"lat_max_ms,omitempty"`
}

func (a *httpAgg) record(r *HTTPRecord) {
	a.N++
	a.Status[min(max(r.Status/100, 0), 5)]++
	a.Bytes += r.Bytes
	if r.Page {
		a.Pages++
	}
	if r.WakeFrom != "" {
		a.Wakes++
		if r.WakeFrom == "cold" {
			a.ColdWakes++
		}
		a.WakeMs += r.Wake
		a.WakeMax = max(a.WakeMax, r.Wake)
	}
	if r.Upgrade {
		a.Upgrades++
		return
	}
	a.Lat[latBin(r.TTFB)]++
	a.LatSum += r.TTFB
	a.LatMax = max(a.LatMax, r.TTFB)
}

func (a *httpAgg) add(o *httpAgg) {
	a.N += o.N
	for i := range a.Status {
		a.Status[i] += o.Status[i]
	}
	a.Upgrades += o.Upgrades
	a.Pages += o.Pages
	a.Bytes += o.Bytes
	a.Wakes += o.Wakes
	a.ColdWakes += o.ColdWakes
	a.WakeMs += o.WakeMs
	a.WakeMax = max(a.WakeMax, o.WakeMax)
	a.Lat.add(&o.Lat)
	a.LatSum += o.LatSum
	a.LatMax = max(a.LatMax, o.LatMax)
}

type httpKey struct{ Kind, Sprite string }

// httpDims are the minute buckets' free-form dimensions for one key.
type httpDims struct {
	Routes   map[string]*httpAgg `json:"routes,omitempty"`
	Pages    map[string]int      `json:"pages,omitempty"`
	Refs     map[string]int      `json:"refs,omitempty"`
	Agents   map[string]int      `json:"agents,omitempty"`
	Visitors map[uint64]struct{} `json:"-"`
	Slow     []HTTPRecord        `json:"slow,omitempty"`
}

func capKey(m map[string]int, k string, limit int) string {
	if _, ok := m[k]; ok || len(m) < limit {
		return k
	}
	return "(other)"
}

func (d *httpDims) record(r *HTTPRecord) {
	route := r.Route
	if r.Kind == kindSprite {
		route = r.Path
	}
	if route == "" {
		route = "(unmatched)"
	}
	if _, ok := d.Routes[route]; !ok && len(d.Routes) >= capRoutes {
		route = "(other)"
	}
	if d.Routes[route] == nil {
		d.Routes[route] = &httpAgg{}
	}
	d.Routes[route].record(r)
	if !r.Upgrade {
		// Keep the slowest few of the minute, fastest first.
		i := sort.Search(len(d.Slow), func(i int) bool { return d.Slow[i].TTFB > r.TTFB })
		if len(d.Slow) < httpSlowPerMin || i > 0 {
			d.Slow = append(d.Slow[:i], append([]HTTPRecord{*r}, d.Slow[i:]...)...)
			if len(d.Slow) > httpSlowPerMin {
				d.Slow = d.Slow[1:]
			}
		}
	}
	if r.Kind != kindSprite {
		return
	}
	if len(d.Visitors) < capVisitors {
		d.Visitors[r.visitor] = struct{}{}
	}
	if r.Agent != "" {
		d.Agents[capKey(d.Agents, r.Agent, capAgents)]++
	}
	if r.Page {
		d.Pages[capKey(d.Pages, r.Path, capPages)]++
		if r.internal {
			return
		}
		ref := r.Referrer
		if ref == "" {
			ref = "(direct)"
		}
		d.Refs[capKey(d.Refs, ref, capRefs)]++
	}
}

func newDims() *httpDims {
	return &httpDims{Routes: map[string]*httpAgg{}, Pages: map[string]int{}, Refs: map[string]int{},
		Agents: map[string]int{}, Visitors: map[uint64]struct{}{}}
}

type httpBucket struct {
	T    time.Time
	Aggs map[httpKey]*httpAgg
	Dims map[httpKey]*httpDims // minute buckets only
}

type httpStats struct {
	mu     sync.Mutex
	fine   []*httpBucket
	coarse []*httpBucket
	recent []HTTPRecord // ring
	next   int
	since  time.Time // the oldest data we have
	began  time.Time // when this process began counting: the fine buckets go back no further

	// Visitors are counted by a hash of address and user agent under a salt
	// that changes every UTC day, so no stored figure identifies anyone.
	salt    [16]byte
	saltDay string

	inflight sync.Map // kind -> *atomic.Int64
	upgraded sync.Map // kind -> *atomic.Int64, upgraded connections still open
}

func newHTTPStats() *httpStats {
	now := time.Now()
	return &httpStats{since: now, began: now}
}

func (h *httpStats) counter(m *sync.Map, kind string) *atomic.Int64 {
	v, _ := m.LoadOrStore(kind, new(atomic.Int64))
	return v.(*atomic.Int64)
}

func bucketFor(list []*httpBucket, t time.Time, step, keep time.Duration, dims bool) ([]*httpBucket, *httpBucket) {
	start := t.Truncate(step)
	if n := len(list); n > 0 && !list[n-1].T.Before(start) {
		return list, list[n-1] // this step's bucket, or the clock went back
	}
	b := &httpBucket{T: start, Aggs: map[httpKey]*httpAgg{}}
	if dims {
		b.Dims = map[httpKey]*httpDims{}
	}
	list = append(list, b)
	cut := 0
	for cut < len(list) && list[cut].T.Before(start.Add(-keep)) {
		cut++
	}
	if cut > 0 {
		list = append([]*httpBucket(nil), list[cut:]...)
	}
	return list, b
}

func (h *httpStats) visitorHash(t time.Time, client, agent string) uint64 {
	if day := t.UTC().Format(time.DateOnly); day != h.saltDay {
		rand.Read(h.salt[:])
		h.saltDay = day
	}
	f := fnv.New64a()
	f.Write(h.salt[:])
	f.Write([]byte(client))
	f.Write([]byte{0})
	f.Write([]byte(agent))
	return f.Sum64()
}

func (h *httpStats) add(r HTTPRecord, ua string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if r.Kind == kindSprite {
		r.visitor = h.visitorHash(r.T, r.Client, ua)
	}
	key := httpKey{r.Kind, r.Sprite}
	var b *httpBucket
	h.fine, b = bucketFor(h.fine, r.T, httpFineStep, httpFineKeep, false)
	aggOf(b, key).record(&r)
	h.coarse, b = bucketFor(h.coarse, r.T, httpCoarseStep, httpCoarseKeep, true)
	aggOf(b, key).record(&r)
	d := b.Dims[key]
	if d == nil {
		d = newDims()
		b.Dims[key] = d
	}
	d.record(&r)
	if len(h.recent) < httpRecentKeep {
		h.recent = append(h.recent, r)
	} else {
		h.recent[h.next] = r
	}
	h.next = (h.next + 1) % httpRecentKeep
}

func aggOf(b *httpBucket, k httpKey) *httpAgg {
	a := b.Aggs[k]
	if a == nil {
		a = &httpAgg{}
		b.Aggs[k] = a
	}
	return a
}

// ---------- recording ----------

// reqNote lets handlers deeper down add what only they know to the record.
type reqNote struct {
	mu       sync.Mutex
	sprite   string
	wake     time.Duration
	wakeFrom string
	err      string
}

type noteKey struct{}

func noteOf(ctx context.Context) *reqNote {
	n, _ := ctx.Value(noteKey{}).(*reqNote)
	return n
}

func noteSprite(ctx context.Context, name string) {
	if n := noteOf(ctx); n != nil {
		n.mu.Lock()
		n.sprite = name
		n.mu.Unlock()
	}
}

func noteErr(ctx context.Context, why string) {
	if n := noteOf(ctx); n != nil {
		n.mu.Lock()
		n.err = why
		n.mu.Unlock()
	}
}

// noteWake is called by Acquire when it had to start the sprite.
func noteWake(ctx context.Context, d time.Duration, from string) {
	if n := noteOf(ctx); n != nil {
		n.mu.Lock()
		n.wake, n.wakeFrom = n.wake+d, from
		n.mu.Unlock()
	}
}

// recWriter sees the status, the time headers went out, and the body size.
type recWriter struct {
	http.ResponseWriter
	status   int
	at       time.Time
	ctype    string
	bytes    int64
	hijacked bool
	onHijack func()
}

func (w *recWriter) mark(code int) {
	if w.status == 0 {
		w.status, w.at, w.ctype = code, time.Now(), w.Header().Get("Content-Type")
	}
}

func (w *recWriter) WriteHeader(code int) {
	if code >= 200 || code == http.StatusSwitchingProtocols { // not 100 or 103, which come before the real one
		w.mark(code)
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *recWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.mark(http.StatusOK)
		if w.ctype == "" {
			w.ctype = http.DetectContentType(b) // what net/http is about to send
		}
	}
	n, err := w.ResponseWriter.Write(b)
	w.bytes += int64(n)
	return n, err
}

func (w *recWriter) Flush() {
	w.mark(http.StatusOK)
	http.NewResponseController(w.ResponseWriter).Flush()
}

func (w *recWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	c, brw, err := http.NewResponseController(w.ResponseWriter).Hijack()
	if err == nil {
		w.hijacked = true
		w.mark(http.StatusSwitchingProtocols) // a reverse proxy writes the 101 itself, on the raw connection
		if w.onHijack != nil {
			w.onHijack()
		}
	}
	return c, brw, err
}

func (w *recWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// instrument records every request h serves. kind sorts requests; sprite, when
// set, is the sprite every request is from (a guest channel).
func (s *Server) instrument(kind func(*http.Request) string, public bool, sprite string, h http.Handler) http.Handler {
	st := s.httpStats
	if st == nil { // a Server assembled by hand, in tests
		return h
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		k := kind(r)
		note := &reqNote{sprite: sprite}
		r = r.WithContext(context.WithValue(r.Context(), noteKey{}, note))
		rw := &recWriter{ResponseWriter: w}
		flight := st.counter(&st.inflight, k)
		flight.Add(1)
		// An upgraded connection stays open until its handler returns.
		var open *atomic.Int64
		rw.onHijack = func() {
			open = st.counter(&st.upgraded, k)
			open.Add(1)
		}
		defer func() {
			flight.Add(-1)
			if open != nil {
				open.Add(-1)
			}
			p := recover()
			st.add(s.httpRecord(r, rw, note, k, public, start, p != nil), r.UserAgent())
			if p != nil {
				panic(p)
			}
		}()
		h.ServeHTTP(rw, r)
	})
}

func (s *Server) httpRecord(r *http.Request, w *recWriter, n *reqNote, kind string, public bool, start time.Time, panicked bool) HTTPRecord {
	now := time.Now()
	status := w.status
	if status == 0 {
		status = http.StatusOK // the handler wrote nothing: net/http sends an empty 200
		if panicked {
			status = http.StatusInternalServerError
		}
		w.at = now
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	rec := HTTPRecord{T: now.UTC(), Kind: kind, Public: public, Sprite: n.sprite, Method: r.Method,
		Path: clip(r.URL.Path), Status: status, TTFB: ms(w.at.Sub(start)), Dur: ms(now.Sub(start)),
		Bytes: w.bytes, Upgrade: w.hijacked || status == http.StatusSwitchingProtocols, Err: n.err}
	if n.wakeFrom != "" {
		rec.Wake, rec.WakeFrom = ms(n.wake), n.wakeFrom
	}
	if kind != kindSprite {
		rec.Route = r.Pattern
		// An API call about a sprite counts against it, if it is one.
		if name := r.PathValue("name"); rec.Sprite == "" && name != "" && rec.Route != "" {
			if _, err := s.store.Get(name); err == nil {
				rec.Sprite = name
			}
		}
	}
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		rec.Client = host
	} else if kind == kindGuest {
		rec.Client = "guest"
	}
	if kind == kindSprite {
		rec.Agent = uaFamily(r.UserAgent())
		rec.Page = r.Method == http.MethodGet && status/100 == 2 && !rec.Upgrade &&
			strings.HasPrefix(w.ctype, "text/html")
		if u, err := url.Parse(r.Referer()); err == nil && u.Host != "" {
			if strings.EqualFold(u.Host, r.Host) {
				rec.internal = true
			} else {
				rec.Referrer = strings.TrimPrefix(strings.ToLower(u.Hostname()), "www.")
			}
		}
	}
	return rec
}

func ms(d time.Duration) float64 { return math.Round(float64(d.Microseconds())) / 1000 }

func clip(p string) string {
	if len(p) <= maxPathLen {
		return p
	}
	p = p[:maxPathLen]
	for !utf8.ValidString(p) {
		p = p[:len(p)-1]
	}
	return p + "…"
}

// uaFamily sorts a user agent into "browser/Chrome", "bot/Googlebot",
// "tool/curl" and so on, with "·mobile" on phones and tablets.
func uaFamily(ua string) string {
	l := strings.ToLower(ua)
	if l == "" {
		return "tool/(none)"
	}
	for _, b := range []struct{ match, name string }{
		{"googlebot", "Googlebot"}, {"bingbot", "Bingbot"}, {"applebot", "Applebot"}, {"duckduckbot", "DuckDuckBot"},
		{"yandex", "YandexBot"}, {"baiduspider", "Baiduspider"}, {"gptbot", "GPTBot"}, {"claudebot", "ClaudeBot"},
		{"facebookexternalhit", "Facebook"}, {"slackbot", "Slackbot"}, {"discordbot", "Discordbot"}, {"twitterbot", "Twitterbot"},
	} {
		if strings.Contains(l, b.match) {
			return "bot/" + b.name
		}
	}
	for _, w := range []string{"bot", "crawl", "spider", "scan", "monitor", "uptime", "headless", "preview", "fetch"} {
		if strings.Contains(l, w) {
			return "bot/other"
		}
	}
	for _, t := range []struct{ match, name string }{
		{"curl/", "curl"}, {"wget/", "Wget"}, {"go-http-client", "Go"}, {"python", "Python"}, {"node", "Node"},
		{"axios", "Node"}, {"okhttp", "OkHttp"}, {"java/", "Java"}, {"postman", "Postman"}, {"httpie", "HTTPie"}, {"deno", "Deno"},
	} {
		if strings.Contains(l, t.match) {
			return "tool/" + t.name
		}
	}
	if !strings.HasPrefix(l, "mozilla/") && !strings.HasPrefix(l, "opera") {
		return "tool/other"
	}
	name := "Other"
	switch {
	case strings.Contains(l, "edg/") || strings.Contains(l, "edga/") || strings.Contains(l, "edgios/"):
		name = "Edge"
	case strings.Contains(l, "opr/") || strings.Contains(l, "opera"):
		name = "Opera"
	case strings.Contains(l, "firefox/") || strings.Contains(l, "fxios/"):
		name = "Firefox"
	case strings.Contains(l, "chrome/") || strings.Contains(l, "crios/"):
		name = "Chrome"
	case strings.Contains(l, "safari/"):
		name = "Safari"
	}
	if strings.Contains(l, "mobi") || strings.Contains(l, "android") || strings.Contains(l, "iphone") || strings.Contains(l, "ipad") {
		name += "·mobile"
	}
	return "browser/" + name
}

// ---------- queries ----------

type HTTPPoint struct {
	T        time.Time `json:"t"`
	N        int       `json:"n"`
	Status   [6]int    `json:"status"`
	Upgrades int       `json:"upgrades"`
	Pages    int       `json:"pages"`
	Visitors int       `json:"visitors"` // minute buckets only
	Bytes    int64     `json:"bytes"`
	Wakes    int       `json:"wakes"`
	P50      float64   `json:"p50_ms"`
	P95      float64   `json:"p95_ms"`
	P99      float64   `json:"p99_ms"`
	Max      float64   `json:"max_ms"`
	Lat      latHist   `json:"lat"`
}

type HTTPTotals struct {
	N         int     `json:"n"`
	Status    [6]int  `json:"status"`
	Upgrades  int     `json:"upgrades"`
	Pages     int     `json:"pages"`
	Visitors  int     `json:"visitors"`
	Bytes     int64   `json:"bytes"`
	Wakes     int     `json:"wakes"`
	ColdWakes int     `json:"cold_wakes"`
	WakeAvg   float64 `json:"wake_avg_ms"`
	WakeMax   float64 `json:"wake_max_ms"`
	Mean      float64 `json:"mean_ms"`
	P50       float64 `json:"p50_ms"`
	P95       float64 `json:"p95_ms"`
	P99       float64 `json:"p99_ms"`
	Max       float64 `json:"max_ms"`
}

type HTTPRow struct {
	Kind     string  `json:"kind"`
	Sprite   string  `json:"sprite,omitempty"`
	Key      string  `json:"key"`
	N        int     `json:"n"`
	Status   [6]int  `json:"status"`
	Upgrades int     `json:"upgrades"`
	Pages    int     `json:"pages,omitempty"`
	Visitors int     `json:"visitors,omitempty"`
	Bytes    int64   `json:"bytes"`
	Wakes    int     `json:"wakes,omitempty"`
	P50      float64 `json:"p50_ms"`
	P95      float64 `json:"p95_ms"`
	P99      float64 `json:"p99_ms"`
	Max      float64 `json:"max_ms"`
}

type HTTPCount struct {
	Key    string `json:"key"`
	Sprite string `json:"sprite,omitempty"`
	N      int    `json:"n"`
}

type HTTPReport struct {
	Now      time.Time        `json:"now"`
	Range    float64          `json:"range_seconds"`
	Covered  float64          `json:"covered_seconds"` // how much of the range there is data for
	Step     float64          `json:"step_seconds"`
	Since    time.Time        `json:"since"` // the oldest data held
	Edges    []float64        `json:"lat_edges_ms"`
	Series   []HTTPPoint      `json:"series"`
	Totals   HTTPTotals       `json:"totals"`
	Prev     *HTTPTotals      `json:"prev,omitempty"` // the same span just before, when we hold it all
	Routes   []HTTPRow        `json:"routes"`
	Sprites  []HTTPRow        `json:"sprites"`
	Pages    []HTTPCount      `json:"pages"`
	Refs     []HTTPCount      `json:"referrers"`
	Agents   []HTTPCount      `json:"agents"`
	Recent   []HTTPRecord     `json:"recent"`
	Slowest  []HTTPRecord     `json:"slowest"`
	Inflight map[string]int64 `json:"inflight"`
	Upgraded map[string]int64 `json:"upgraded"`
	Kinds    []string         `json:"kinds"` // every kind seen in the range, filtered or not
}

type httpQuery struct {
	Range  time.Duration
	Kinds  map[string]bool // nil: all
	Sprite string
	Public string // "", "public" or "private": which listener, for sprite URLs
	Minute bool   // minute steps even for the last hour, which carry visitors
}

func (q httpQuery) match(k httpKey) bool {
	return (q.Kinds == nil || q.Kinds[k.Kind]) && (q.Sprite == "" || q.Sprite == k.Sprite)
}

func (q httpQuery) matchRecord(r *HTTPRecord) bool {
	if !q.match(httpKey{r.Kind, r.Sprite}) {
		return false
	}
	return q.Public == "" || r.Kind != kindSprite || r.Public == (q.Public == "public")
}

func (h *httpStats) query(q httpQuery, now time.Time) HTTPReport {
	h.mu.Lock()
	defer h.mu.Unlock()
	rep := HTTPReport{Now: now.UTC(), Range: q.Range.Seconds(), Since: h.since.UTC(),
		Series: []HTTPPoint{}, Routes: []HTTPRow{}, Sprites: []HTTPRow{}, Pages: []HTTPCount{},
		Refs: []HTTPCount{}, Agents: []HTTPCount{}, Recent: []HTTPRecord{}, Slowest: []HTTPRecord{},
		Inflight: map[string]int64{}, Upgraded: map[string]int64{}, Kinds: []string{}}
	for i := range latBins {
		rep.Edges = append(rep.Edges, math.Round(latEdge(i)*1000)/1000)
	}

	// The fine buckets cover the last hour; past that, minutes, grouped so a
	// chart never has more than a few hundred columns.
	list, step, start := h.coarse, httpCoarseStep, h.since
	if q.Range <= httpFineKeep && !q.Minute {
		list, step, start = h.fine, httpFineStep, h.began
	}
	// Steps are sized by what there is to show, so a young daemon's day is not
	// one five-minute column.
	for step*400 < min(q.Range, now.Sub(start)) {
		step *= 5
	}
	rep.Step = step.Seconds()
	from := now.Add(-q.Range)
	end := now.Truncate(step)
	first := from.Truncate(step)
	if first.Before(from) {
		first = first.Add(step)
	}
	// Nothing was counted before the daemon started: the chart begins there
	// rather than with a flat line that is not a measurement.
	if s := start.Truncate(step); s.After(first) {
		first = s
	}
	rep.Covered = now.Sub(maxTime(from, h.since)).Seconds()
	cols := int(end.Sub(first)/step) + 1
	pts := make([]httpAgg, cols)
	visitors := make([]map[uint64]struct{}, cols)
	kinds := map[string]bool{}
	for _, b := range list {
		if b.T.Before(first) || b.T.After(now) {
			continue
		}
		i := int(b.T.Sub(first) / step)
		for k, a := range b.Aggs {
			kinds[k.Kind] = true
			if q.match(k) {
				pts[i].add(a)
			}
		}
		for k, d := range b.Dims {
			if q.match(k) && len(d.Visitors) > 0 {
				if visitors[i] == nil {
					visitors[i] = map[uint64]struct{}{}
				}
				for v := range d.Visitors {
					visitors[i][v] = struct{}{}
				}
			}
		}
	}
	for i := range pts {
		a := &pts[i]
		rep.Series = append(rep.Series, HTTPPoint{T: first.Add(time.Duration(i) * step).UTC(), N: a.N, Status: a.Status,
			Upgrades: a.Upgrades, Pages: a.Pages, Visitors: len(visitors[i]), Bytes: a.Bytes, Wakes: a.Wakes,
			P50: a.Lat.quantile(0.5, a.LatMax), P95: a.Lat.quantile(0.95, a.LatMax), P99: a.Lat.quantile(0.99, a.LatMax),
			Max: a.LatMax, Lat: a.Lat})
	}
	for k := range kinds {
		rep.Kinds = append(rep.Kinds, k)
	}
	sort.Strings(rep.Kinds)

	// Totals and tables come from the minute buckets.
	rep.Totals = h.totals(q, from, now)
	if !h.since.After(from.Add(-q.Range)) {
		p := h.totals(q, from.Add(-q.Range), from)
		rep.Prev = &p
	}
	h.tables(q, from, now, &rep)

	for i := range len(h.recent) {
		r := &h.recent[(h.next-1-i+2*httpRecentKeep)%httpRecentKeep]
		if len(rep.Recent) < 100 && q.matchRecord(r) && r.T.After(from) {
			rep.Recent = append(rep.Recent, *r)
		}
	}
	h.inflight.Range(func(k, v any) bool {
		rep.Inflight[k.(string)] = v.(*atomic.Int64).Load()
		return true
	})
	h.upgraded.Range(func(k, v any) bool {
		rep.Upgraded[k.(string)] = v.(*atomic.Int64).Load()
		return true
	})
	return rep
}

// inRange is whether a minute bucket belongs to (from, to]: a bucket counts
// if most of its minute does.
func inRange(b *httpBucket, from, to time.Time) bool {
	mid := b.T.Add(httpCoarseStep / 2)
	return mid.After(from) && !b.T.After(to)
}

func (h *httpStats) totals(q httpQuery, from, to time.Time) HTTPTotals {
	var a httpAgg
	vis := map[uint64]struct{}{}
	for _, b := range h.coarse {
		if !inRange(b, from, to) {
			continue
		}
		for k, x := range b.Aggs {
			if q.match(k) {
				a.add(x)
			}
		}
		for k, d := range b.Dims {
			if q.match(k) {
				for v := range d.Visitors {
					vis[v] = struct{}{}
				}
			}
		}
	}
	t := HTTPTotals{N: a.N, Status: a.Status, Upgrades: a.Upgrades, Pages: a.Pages, Visitors: len(vis), Bytes: a.Bytes,
		Wakes: a.Wakes, ColdWakes: a.ColdWakes, WakeMax: a.WakeMax, Max: a.LatMax,
		P50: a.Lat.quantile(0.5, a.LatMax), P95: a.Lat.quantile(0.95, a.LatMax), P99: a.Lat.quantile(0.99, a.LatMax)}
	if a.Wakes > 0 {
		t.WakeAvg = a.WakeMs / float64(a.Wakes)
	}
	if n := a.N - a.Upgrades; n > 0 {
		t.Mean = a.LatSum / float64(n)
	}
	return t
}

func (h *httpStats) tables(q httpQuery, from, to time.Time, rep *HTTPReport) {
	type rk struct{ Kind, Sprite, Key string }
	routes := map[rk]*httpAgg{}
	sprites := map[string]*httpAgg{}
	spriteVis := map[string]map[uint64]struct{}{}
	pages := map[rk]int{}
	refs := map[string]int{}
	agents := map[string]int{}
	var slow []HTTPRecord
	for _, b := range h.coarse {
		if !inRange(b, from, to) {
			continue
		}
		for k, d := range b.Dims {
			if !q.match(k) {
				continue
			}
			for key, a := range d.Routes {
				r := rk{k.Kind, k.Sprite, key}
				if routes[r] == nil {
					routes[r] = &httpAgg{}
				}
				routes[r].add(a)
			}
			for p, n := range d.Pages {
				pages[rk{k.Kind, k.Sprite, p}] += n
			}
			for r, n := range d.Refs {
				refs[r] += n
			}
			for a, n := range d.Agents {
				agents[a] += n
			}
			for _, r := range d.Slow {
				if q.matchRecord(&r) {
					slow = append(slow, r)
				}
			}
			if k.Sprite != "" {
				if spriteVis[k.Sprite] == nil {
					spriteVis[k.Sprite] = map[uint64]struct{}{}
				}
				for v := range d.Visitors {
					spriteVis[k.Sprite][v] = struct{}{}
				}
			}
		}
		for k, a := range b.Aggs {
			if q.match(k) && k.Sprite != "" {
				if sprites[k.Sprite] == nil {
					sprites[k.Sprite] = &httpAgg{}
				}
				sprites[k.Sprite].add(a)
			}
		}
	}
	row := func(kind, sprite, key string, a *httpAgg) HTTPRow {
		return HTTPRow{Kind: kind, Sprite: sprite, Key: key, N: a.N, Status: a.Status, Upgrades: a.Upgrades,
			Pages: a.Pages, Bytes: a.Bytes, Wakes: a.Wakes, Max: a.LatMax,
			P50: a.Lat.quantile(0.5, a.LatMax), P95: a.Lat.quantile(0.95, a.LatMax), P99: a.Lat.quantile(0.99, a.LatMax)}
	}
	for k, a := range routes {
		rep.Routes = append(rep.Routes, row(k.Kind, k.Sprite, k.Key, a))
	}
	sort.Slice(rep.Routes, func(i, j int) bool {
		a, b := rep.Routes[i], rep.Routes[j]
		if a.N != b.N {
			return a.N > b.N
		}
		return a.Kind+a.Sprite+a.Key < b.Kind+b.Sprite+b.Key
	})
	rep.Routes = rep.Routes[:min(len(rep.Routes), 100)]
	for name, a := range sprites {
		r := row("", name, name, a)
		r.Visitors = len(spriteVis[name])
		rep.Sprites = append(rep.Sprites, r)
	}
	sort.Slice(rep.Sprites, func(i, j int) bool {
		if rep.Sprites[i].N != rep.Sprites[j].N {
			return rep.Sprites[i].N > rep.Sprites[j].N
		}
		return rep.Sprites[i].Key < rep.Sprites[j].Key
	})
	for k, n := range pages {
		rep.Pages = append(rep.Pages, HTTPCount{Key: k.Key, Sprite: k.Sprite, N: n})
	}
	rep.Pages = topCounts(rep.Pages, 25)
	for k, n := range refs {
		rep.Refs = append(rep.Refs, HTTPCount{Key: k, N: n})
	}
	rep.Refs = topCounts(rep.Refs, 15)
	for k, n := range agents {
		rep.Agents = append(rep.Agents, HTTPCount{Key: k, N: n})
	}
	rep.Agents = topCounts(rep.Agents, 20)
	sort.Slice(slow, func(i, j int) bool { return slow[i].TTFB > slow[j].TTFB })
	rep.Slowest = append(rep.Slowest, slow[:min(len(slow), 20)]...)
}

func maxTime(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

func topCounts(c []HTTPCount, n int) []HTTPCount {
	sort.Slice(c, func(i, j int) bool {
		if c[i].N != c[j].N {
			return c[i].N > c[j].N
		}
		return c[i].Sprite+c[i].Key < c[j].Sprite+c[j].Key
	})
	return c[:min(len(c), n)]
}

// ---------- saving across restarts ----------

type savedEntry struct {
	Kind     string    `json:"kind"`
	Sprite   string    `json:"sprite,omitempty"`
	Agg      *httpAgg  `json:"agg"`
	Dims     *httpDims `json:"dims,omitempty"`
	Visitors []uint64  `json:"visitors,omitempty"`
}

type savedBucket struct {
	T       time.Time    `json:"t"`
	Entries []savedEntry `json:"entries"`
}

type savedHTTP struct {
	Version int           `json:"version"`
	Since   time.Time     `json:"since"`
	Salt    []byte        `json:"salt"`
	SaltDay string        `json:"salt_day"`
	Minutes []savedBucket `json:"minutes"`
}

const httpSaveVersion = 1

func (h *httpStats) save(path string) error {
	h.mu.Lock()
	out := savedHTTP{Version: httpSaveVersion, Since: h.since, Salt: h.salt[:], SaltDay: h.saltDay}
	for _, b := range h.coarse {
		sb := savedBucket{T: b.T}
		for k, a := range b.Aggs {
			e := savedEntry{Kind: k.Kind, Sprite: k.Sprite, Agg: a, Dims: b.Dims[k]}
			if e.Dims != nil {
				for v := range e.Dims.Visitors {
					e.Visitors = append(e.Visitors, v)
				}
			}
			sb.Entries = append(sb.Entries, e)
		}
		out.Minutes = append(out.Minutes, sb)
	}
	buf, err := json.Marshal(out)
	h.mu.Unlock()
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, buf, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// load restores the minute buckets from an earlier run. The fine buckets and
// the recent requests start over.
func (h *httpStats) load(path string, now time.Time) error {
	buf, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var in savedHTTP
	if err := json.Unmarshal(buf, &in); err != nil {
		return err
	}
	if in.Version != httpSaveVersion {
		return nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.coarse = nil
	for _, sb := range in.Minutes {
		if sb.T.Before(now.Add(-httpCoarseKeep)) || sb.T.After(now) {
			continue
		}
		b := &httpBucket{T: sb.T, Aggs: map[httpKey]*httpAgg{}, Dims: map[httpKey]*httpDims{}}
		for _, e := range sb.Entries {
			k := httpKey{e.Kind, e.Sprite}
			if e.Agg != nil {
				b.Aggs[k] = e.Agg
			}
			if e.Dims != nil {
				d := newDims()
				for name, a := range e.Dims.Routes {
					d.Routes[name] = a
				}
				for m, src := range map[*map[string]int]map[string]int{&d.Pages: e.Dims.Pages, &d.Refs: e.Dims.Refs, &d.Agents: e.Dims.Agents} {
					for name, n := range src {
						(*m)[name] = n
					}
				}
				for _, v := range e.Visitors {
					d.Visitors[v] = struct{}{}
				}
				d.Slow = e.Dims.Slow
				b.Dims[k] = d
			}
		}
		h.coarse = append(h.coarse, b)
	}
	sort.Slice(h.coarse, func(i, j int) bool { return h.coarse[i].T.Before(h.coarse[j].T) })
	if len(h.coarse) > 0 && in.Since.Before(h.since) {
		h.since = in.Since
		if oldest := now.Add(-httpCoarseKeep); h.since.Before(oldest) {
			h.since = oldest
		}
	}
	if len(in.Salt) == len(h.salt) {
		copy(h.salt[:], in.Salt)
		h.saltDay = in.SaltDay
	}
	return nil
}

func (s *Server) httpStatsFile() string { return filepath.Join(s.opts.DataDir, "http-stats.json") }

// SaveHTTPStats writes the day's request figures out; the daemon calls it on
// the way down, and the sampler every minute.
func (s *Server) SaveHTTPStats() {
	if err := s.httpStats.save(s.httpStatsFile()); err != nil {
		s.log.Warn("could not save request metrics", "err", err)
	}
}

func (s *Server) runHTTPStats() {
	if err := s.httpStats.load(s.httpStatsFile(), time.Now()); err != nil && !os.IsNotExist(err) {
		s.log.Warn("could not load saved request metrics; starting over", "err", err)
	}
	t := time.NewTicker(httpSaveEvery)
	defer t.Stop()
	for range t.C {
		s.SaveHTTPStats()
	}
}
