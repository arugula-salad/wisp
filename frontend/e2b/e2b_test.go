package e2b

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/arugula-salad/wisp/engine"
	"github.com/arugula-salad/wisp/internal/store"
	"github.com/arugula-salad/wisp/internal/vmm"
)

const (
	adminKey = "admin-key"
	readKey  = "read-key"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

// fixture is a front end on a real engine and store, with no VMs: starting
// one is a counter, and the guest's ports are whatever the test serves.
type fixture struct {
	t     *testing.T
	f     *Frontend
	h     http.Handler
	st    *store.Store
	life  *engine.Engine
	mu    sync.Mutex
	boots int
	// guest serves a guest's ports when set: every dial reaches it.
	guest http.Handler
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	dir := t.TempDir()
	base := filepath.Join(dir, "base.ext4")
	disk := filepath.Join(dir, "e2b.ext4")
	for _, p := range []string{base, disk} {
		if err := os.WriteFile(p, bytes.Repeat([]byte("disk"), 4096), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	life := engine.New(engine.Options{DataDir: dir, BaseImage: base, NoNetwork: true}, st, quiet)
	life.StartReaping()
	t.Cleanup(life.Shutdown)
	fx := &fixture{t: t, st: st, life: life}
	fx.f = New(Options{Disk: disk, Domain: "e2b.test:7823", CPUs: 2, MemMiB: 512, DefaultCPUs: 8, DefaultMemMiB: 2048,
		CheckKey: func(k string) (bool, bool) {
			switch k {
			case adminKey:
				return true, true
			case readKey:
				return false, true
			}
			return false, false
		}}, st, life, quiet)
	fx.f.acquire = func(ctx context.Context, rec store.Record) (*vmm.Machine, func(), error) {
		fx.mu.Lock()
		fx.boots++
		fx.mu.Unlock()
		return nil, func() {}, nil
	}
	fx.f.dialEnvd = func(*vmm.Machine) dialFunc { return fx.dial }
	fx.h = fx.f.Handler()
	return fx
}

// dial reaches the test's guest over an in-memory pipe.
func (fx *fixture) dial(ctx context.Context, port string) (net.Conn, error) {
	if fx.guest == nil {
		return nil, &net.OpError{Op: "dial", Err: io.EOF}
	}
	c, s := net.Pipe()
	srv := &http.Server{Handler: fx.guest}
	go srv.Serve(&oneConn{c: s})
	return c, nil
}

// oneConn is a listener that accepts one connection.
type oneConn struct {
	c    net.Conn
	once sync.Once
	done chan struct{}
}

func (l *oneConn) Accept() (net.Conn, error) {
	var c net.Conn
	l.once.Do(func() { c, l.done = l.c, make(chan struct{}) })
	if c != nil {
		return c, nil
	}
	<-l.done
	return nil, net.ErrClosed
}
func (l *oneConn) Close() error   { return nil }
func (l *oneConn) Addr() net.Addr { return &net.UnixAddr{} }

func (fx *fixture) do(method, path, key string, body any, hdr ...string) *httptest.ResponseRecorder {
	fx.t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	r := httptest.NewRequest(method, path, rd)
	if key != "" {
		r.Header.Set("X-API-Key", key)
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		if strings.EqualFold(hdr[i], "Host") {
			r.Host = hdr[i+1]
		} else {
			r.Header.Set(hdr[i], hdr[i+1])
		}
	}
	w := httptest.NewRecorder()
	fx.h.ServeHTTP(w, r)
	return w
}

func decode[T any](t *testing.T, w *httptest.ResponseRecorder) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
		t.Fatalf("%d %q: %v", w.Code, w.Body.String(), err)
	}
	return v
}

func keys(m map[string]any) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

func (fx *fixture) create(body map[string]any) string {
	fx.t.Helper()
	w := fx.do("POST", "/v2/sandboxes", adminKey, body)
	if w.Code != http.StatusCreated {
		fx.t.Fatalf("create: %d %s", w.Code, w.Body)
	}
	return decode[map[string]any](fx.t, w)["sandboxID"].(string)
}

func wantErr(t *testing.T, w *httptest.ResponseRecorder, status int, msg string) {
	t.Helper()
	if w.Code != status {
		t.Fatalf("status %d, want %d: %s", w.Code, status, w.Body)
	}
	got := decode[map[string]any](t, w)
	if got["code"] != float64(status) || !strings.Contains(got["message"].(string), msg) {
		t.Fatalf("body %v, want code %d and a message with %q", got, status, msg)
	}
}

func TestCreate(t *testing.T) {
	fx := newFixture(t)
	before := time.Now()
	w := fx.do("POST", "/v2/sandboxes", adminKey, map[string]any{"templateID": "base", "timeout": 120,
		"metadata": map[string]string{"k": "v"}, "envVars": map[string]string{"A": "b"}})
	if w.Code != http.StatusCreated {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	got := decode[map[string]any](t, w)
	if k := keys(got); !slices.Equal(k, []string{"alias", "clientID", "domain", "envdAccessToken", "envdVersion", "sandboxID", "templateID"}) {
		t.Fatalf("keys %v", k)
	}
	id := got["sandboxID"].(string)
	if !regexp.MustCompile(`^i[a-z0-9]{20}$`).MatchString(id) {
		t.Fatalf("id %q", id)
	}
	if got["alias"] != "base" || got["clientID"] != clientID || got["envdVersion"] != EnvdVersion || got["domain"] != "e2b.test:7823" {
		t.Fatalf("%v", got)
	}
	if fx.boots != 1 {
		t.Fatalf("create booted %d times, want 1", fx.boots)
	}
	sp, err := fx.st.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	if sp.API != API || sp.Name != "" || sp.Hostname != hostname || sp.Config.CPUs != 2 || sp.Config.RamMB != 512 {
		t.Fatalf("record %+v", sp)
	}
	if idle, action := sp.Lifecycle.Idle(time.Minute); action != store.IdleNone || idle != time.Minute {
		t.Fatalf("idle rule %v %v", idle, action)
	}
	if sp.Lifecycle.OnDeadline() != store.DeadlineDelete || sp.ExpiresAt == nil ||
		sp.ExpiresAt.Sub(before) < 119*time.Second || sp.ExpiresAt.Sub(before) > 121*time.Second {
		t.Fatalf("deadline %v %v", sp.ExpiresAt, sp.Lifecycle.OnDeadline())
	}
	m, _ := metaOf(sp.Record)
	if m.AccessToken != got["envdAccessToken"] || m.EnvVars["A"] != "b" || m.Metadata["k"] != "v" {
		t.Fatalf("meta %+v", m)
	}
	// The Sprites namespace does not see it.
	if l := fx.st.List(store.Sprites, ""); len(l) != 0 {
		t.Fatalf("sprites list has %v", l)
	}

	// Defaults: no body is the base template for 300 s; autoPause deadlines suspend.
	id2 := fx.create(nil)
	sp2, _ := fx.st.Get(id2)
	if d := time.Until(*sp2.ExpiresAt); d < 295*time.Second || d > 301*time.Second {
		t.Fatalf("default timeout: %v", d)
	}
	id3 := fx.create(map[string]any{"autoPause": true})
	sp3, _ := fx.st.Get(id3)
	if sp3.Lifecycle.OnDeadline() != store.DeadlineSuspend {
		t.Fatalf("autoPause deadline action %v", sp3.Lifecycle.OnDeadline())
	}
}

func TestCreateErrors(t *testing.T) {
	fx := newFixture(t)
	wantErr(t, fx.do("POST", "/v2/sandboxes", "", nil), 401, "Invalid API key")
	wantErr(t, fx.do("POST", "/v2/sandboxes", "wrong", nil), 401, "Invalid API key")
	wantErr(t, fx.do("POST", "/v2/sandboxes", readKey, nil), 403, "read-only")
	wantErr(t, fx.do("POST", "/v2/sandboxes", adminKey, map[string]any{"templateID": "python"}), 404, "template 'python' not found")
	wantErr(t, fx.do("POST", "/v2/sandboxes", adminKey, map[string]any{"timeout": 100 * 3600}), 400, "Timeout")
	if fx.st.Count() != 0 {
		t.Fatalf("%d records after refused creates", fx.st.Count())
	}
	// A read key may look.
	if w := fx.do("GET", "/v2/sandboxes", readKey, nil); w.Code != 200 {
		t.Fatalf("list with a read key: %d", w.Code)
	}
}

func TestGetAndKill(t *testing.T) {
	fx := newFixture(t)
	id := fx.create(map[string]any{"metadata": map[string]string{"k": "v"}})
	w := fx.do("GET", "/sandboxes/"+id, readKey, nil)
	if w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	d := decode[map[string]any](t, w)
	want := []string{"alias", "clientID", "cpuCount", "diskSizeMB", "domain", "endAt", "envdAccessToken", "envdVersion",
		"lifecycle", "memoryMB", "metadata", "sandboxID", "startedAt", "state", "templateID", "volumeMounts"}
	if k := keys(d); !slices.Equal(k, want) {
		t.Fatalf("keys %v\nwant %v", k, want)
	}
	if d["state"] != "running" || d["cpuCount"] != 2.0 || d["memoryMB"] != 512.0 || d["diskSizeMB"] != 0.0 {
		t.Fatalf("%v", d)
	}
	if lc := d["lifecycle"].(map[string]any); lc["onTimeout"] != "kill" || lc["autoResume"] != false {
		t.Fatalf("lifecycle %v", lc)
	}
	if vm, ok := d["volumeMounts"].([]any); !ok || len(vm) != 0 {
		t.Fatalf("volumeMounts %v", d["volumeMounts"])
	}
	for _, f := range []string{"startedAt", "endAt"} {
		if _, err := time.Parse(time.RFC3339Nano, d[f].(string)); err != nil {
			t.Fatalf("%s: %v", f, err)
		}
	}

	if w := fx.do("DELETE", "/sandboxes/"+id, adminKey, nil); w.Code != 204 {
		t.Fatalf("kill: %d %s", w.Code, w.Body)
	}
	msg := `Sandbox "` + id + `" doesn't exist or you don't have access to it`
	wantErr(t, fx.do("GET", "/sandboxes/"+id, adminKey, nil), 404, msg)
	wantErr(t, fx.do("DELETE", "/sandboxes/"+id, adminKey, nil), 404, msg)
	wantErr(t, fx.do("POST", "/v2/sandboxes/"+id+"/connect", adminKey, map[string]any{}), 404, msg)

	// A sprite is not an E2B sandbox, even by ID.
	now := time.Now()
	sp, err := fx.life.Create(context.Background(), engine.CreateSpec{Sprite: store.Sprite{
		Record: store.Record{ID: store.NewID(), Hostname: "s", CreatedAt: now}, SpriteMeta: store.SpriteMeta{Name: "s"}}})
	if err != nil {
		t.Fatal(err)
	}
	wantErr(t, fx.do("GET", "/sandboxes/"+sp.ID, adminKey, nil), 404, "doesn't exist")
	wantErr(t, fx.do("DELETE", "/sandboxes/"+sp.ID, adminKey, nil), 404, "doesn't exist")
	if _, err := fx.st.Get(sp.ID); err != nil {
		t.Fatalf("the sprite is gone: %v", err)
	}
}

func TestTimeout(t *testing.T) {
	fx := newFixture(t)
	id := fx.create(nil)
	if w := fx.do("POST", "/sandboxes/"+id+"/timeout", adminKey, map[string]any{"timeout": 30}); w.Code != 204 {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	sp, _ := fx.st.Get(id)
	m, _ := metaOf(sp.Record)
	if d := time.Until(*sp.ExpiresAt); d < 28*time.Second || d > 31*time.Second || !m.EndAt.Equal(*sp.ExpiresAt) {
		t.Fatalf("deadline %v, endAt %v", sp.ExpiresAt, m.EndAt)
	}
	wantErr(t, fx.do("POST", "/sandboxes/"+id+"/timeout", adminKey, map[string]any{}), 400, "timeout")
	wantErr(t, fx.do("POST", "/sandboxes/nope/timeout", adminKey, map[string]any{"timeout": 3}), 404, "doesn't exist")
}

func TestPauseConnect(t *testing.T) {
	fx := newFixture(t)
	id := fx.create(map[string]any{"timeout": 600})
	if w := fx.do("POST", "/sandboxes/"+id+"/pause", adminKey, map[string]any{}); w.Code != 204 {
		t.Fatalf("pause: %d %s", w.Code, w.Body)
	}
	d := decode[map[string]any](t, fx.do("GET", "/sandboxes/"+id, adminKey, nil))
	if d["state"] != "paused" || d["volumeMounts"] != nil {
		t.Fatalf("after pause %v", d)
	}
	if end, _ := time.Parse(time.RFC3339Nano, d["endAt"].(string)); time.Since(end) > 5*time.Second {
		t.Fatalf("endAt %v is not the pause time", end)
	}
	if sp, _ := fx.st.Get(id); sp.ExpiresAt != nil {
		t.Fatalf("a paused sandbox keeps its deadline %v", sp.ExpiresAt)
	}
	wantErr(t, fx.do("POST", "/sandboxes/"+id+"/pause", adminKey, map[string]any{}), 409,
		"Error pausing sandbox - sandbox '"+id+"' is already paused")
	wantErr(t, fx.do("POST", "/sandboxes/"+id+"/timeout", adminKey, map[string]any{"timeout": 30}), 404, "paused")
	// Its traffic does not wake it.
	if w := fx.do("POST", "/process.Process/List", "", map[string]any{}, "E2b-Sandbox-Id", id); w.Code != 502 ||
		decode[map[string]any](t, w)["message"] != "The sandbox was not found" {
		t.Fatalf("envd traffic to a paused sandbox: %d %s", w.Code, w.Body)
	}

	boots := fx.boots
	w := fx.do("POST", "/v2/sandboxes/"+id+"/connect", adminKey, map[string]any{})
	if w.Code != 201 || fx.boots != boots+1 {
		t.Fatalf("resume: %d %s (boots %d -> %d)", w.Code, w.Body, boots, fx.boots)
	}
	if k := keys(decode[map[string]any](t, w)); !slices.Equal(k, []string{"alias", "clientID", "domain", "envdAccessToken", "envdVersion", "sandboxID", "templateID"}) {
		t.Fatalf("keys %v", k)
	}
	sp, _ := fx.st.Get(id)
	m, _ := metaOf(sp.Record)
	// Resumed with the default 300 s, not the 600 it was created with, as hosted does.
	if d := time.Until(m.EndAt); m.Paused || d < 295*time.Second || d > 301*time.Second || sp.ExpiresAt == nil || time.Since(m.StartedAt) > 5*time.Second {
		t.Fatalf("after resume: %+v, deadline %v", m, sp.ExpiresAt)
	}
	// Connecting to a running sandbox extends, never shortens, and boots nothing.
	w = fx.do("POST", "/v2/sandboxes/"+id+"/connect", adminKey, map[string]any{"timeout": 10})
	sp2, _ := fx.st.Get(id)
	if w.Code != 200 || !sp2.ExpiresAt.Equal(*sp.ExpiresAt) || fx.boots != boots+1 {
		t.Fatalf("connect running: %d, deadline %v -> %v", w.Code, sp.ExpiresAt, sp2.ExpiresAt)
	}
	w = fx.do("POST", "/v2/sandboxes/"+id+"/connect", adminKey, map[string]any{"timeout": 900})
	if sp3, _ := fx.st.Get(id); w.Code != 200 || time.Until(*sp3.ExpiresAt) < 890*time.Second {
		t.Fatalf("connect extend: %d, deadline %v", w.Code, sp3.ExpiresAt)
	}
	// The deprecated resume is 409 on a running sandbox.
	wantErr(t, fx.do("POST", "/sandboxes/"+id+"/resume", adminKey, map[string]any{}), 409, "already running")
}

// An autoPause sandbox past its deadline is paused, whether or not the engine
// has got round to suspending it yet.
func TestAutoPauseState(t *testing.T) {
	m := meta{AutoPause: true, EndAt: time.Now().Add(-time.Second)}
	if m.state(time.Now()) != "paused" {
		t.Fatal("not paused past its deadline")
	}
	m.AutoPause = false
	if m.state(time.Now()) != "running" {
		t.Fatal("a kill-on-timeout sandbox is running until it is deleted")
	}
}

func TestList(t *testing.T) {
	fx := newFixture(t)
	var ids []string
	base := time.Now().UTC().Add(-time.Hour)
	for i := range 5 {
		id := fx.create(map[string]any{"metadata": map[string]string{"run": "r", "n": string(rune('a' + i))}})
		// Distinct start times, oldest first.
		fx.f.updateMeta(id, func(m *meta) { m.StartedAt = base.Add(time.Duration(i) * time.Minute) })
		ids = append(ids, id)
	}
	fx.create(map[string]any{"metadata": map[string]string{"run": "other"}})
	fx.do("POST", "/sandboxes/"+ids[1]+"/pause", adminKey, nil)

	var got []string
	tok := ""
	pages := 0
	for {
		path := "/v2/sandboxes?limit=2&metadata=run%3Dr"
		if tok != "" {
			path += "&nextToken=" + tok
		}
		w := fx.do("GET", path, adminKey, nil)
		if w.Code != 200 {
			t.Fatalf("%d %s", w.Code, w.Body)
		}
		if w.Header().Get("X-Total-Running") != "4" {
			t.Fatalf("X-Total-Running %q", w.Header().Get("X-Total-Running"))
		}
		items := decode[[]map[string]any](t, w)
		for _, it := range items {
			if _, ok := it["envdAccessToken"]; ok {
				t.Fatal("a listed sandbox carries its token")
			}
			if _, ok := it["lifecycle"]; ok {
				t.Fatal("a listed sandbox carries its lifecycle")
			}
			got = append(got, it["sandboxID"].(string))
		}
		pages++
		if tok = w.Header().Get("X-Next-Token"); tok == "" {
			break
		}
	}
	want := []string{ids[4], ids[3], ids[2], ids[1], ids[0]} // newest first
	if !slices.Equal(got, want) || pages != 3 {
		t.Fatalf("got %v in %d pages, want %v in 3", got, pages, want)
	}

	items := decode[[]map[string]any](t, fx.do("GET", "/v2/sandboxes?state=paused", adminKey, nil))
	if len(items) != 1 || items[0]["sandboxID"] != ids[1] || items[0]["state"] != "paused" {
		t.Fatalf("paused: %v", items)
	}
	w := fx.do("GET", "/v2/sandboxes?state=paused", adminKey, nil)
	if w.Header().Get("X-Total-Running") != "" {
		t.Fatal("X-Total-Running on a paused-only listing")
	}
	items = decode[[]map[string]any](t, fx.do("GET", "/v2/sandboxes?order=asc&limit=1&metadata=run%3Dr%26n%3Dc", adminKey, nil))
	if len(items) != 1 || items[0]["sandboxID"] != ids[2] {
		t.Fatalf("metadata k=v&k=v: %v", items)
	}
	if items := decode[[]map[string]any](t, fx.do("GET", "/v2/sandboxes", adminKey, nil)); len(items) != 6 {
		t.Fatalf("unfiltered: %d", len(items))
	}
	if items := decode[[]map[string]any](t, fx.do("GET", "/v2/sandboxes?template=python", adminKey, nil)); len(items) != 0 {
		t.Fatalf("unknown template: %v", items)
	}
	wantErr(t, fx.do("GET", "/v2/sandboxes?nextToken=bm90LWEtY3Vyc29y", adminKey, nil), 400, "Invalid next token")
	wantErr(t, fx.do("GET", "/v2/sandboxes?metadata=nokv", adminKey, nil), 400, "metadata")
	wantErr(t, fx.do("GET", "/v2/sandboxes?state=dead", adminKey, nil), 400, "state")
	if items := decode[[]map[string]any](t, fx.do("GET", "/sandboxes", adminKey, nil)); len(items) != 5 {
		t.Fatalf("v1 list (running only): %d", len(items))
	}
}

func TestRoute(t *testing.T) {
	fx := newFixture(t)
	for _, c := range []struct {
		host, id, port string
		hdr            []string
		api            bool
	}{
		{host: "127.0.0.1:7823", api: true},
		{host: "localhost", api: true},
		{host: "api.e2b.test", api: true},
		{host: "8080-iabc.e2b.test:7823", id: "iabc", port: "8080"},
		{host: "49983-iabc.e2b.localhost", id: "iabc", port: "49983"},
		{host: "8080-IABC.e2b.test", id: "iabc", port: "8080"},
		{host: "127.0.0.1:7823", hdr: []string{"E2b-Sandbox-Id", "iabc", "E2b-Sandbox-Port", "49983"}, id: "iabc", port: "49983"},
		{host: "127.0.0.1:7823", hdr: []string{"E2b-Sandbox-Id", "iabc"}, id: "iabc", port: "49983"},
		{host: "x-iabc.e2b.test", api: true},
	} {
		r := httptest.NewRequest("POST", "/process.Process/List", nil)
		r.Host = c.host
		for i := 0; i < len(c.hdr); i += 2 {
			r.Header.Set(c.hdr[i], c.hdr[i+1])
		}
		tg, ok := fx.f.route(r)
		if ok == c.api || (!c.api && (tg.id != c.id || tg.port != c.port)) {
			t.Errorf("%s %v: got %+v %v", c.host, c.hdr, tg, ok)
		}
	}
}

func TestSignatureRouting(t *testing.T) {
	fx := newFixture(t)
	a, b := fx.create(nil), fx.create(nil)
	tokenOf := func(id string) string {
		rec, _ := fx.st.GetRecord(id)
		m, _ := metaOf(rec)
		return m.AccessToken
	}
	// The SDKs' algorithm (sandbox/signature.ts), written out independently.
	sign := func(path, op, user, token, exp string) string {
		return fileSignature(path, op, user, token, exp)
	}
	q := func(path, user, sig, exp string) string {
		v := "/files?path=" + url.QueryEscape(path) + "&signature=" + url.QueryEscape(sig)
		if user != "" {
			v += "&username=" + user
		}
		if exp != "" {
			v += "&signature_expiration=" + exp
		}
		return v
	}
	for _, c := range []struct {
		method, url, want string
	}{
		{"GET", q("/tmp/x", "", sign("/tmp/x", "read", "", tokenOf(b), ""), ""), b},
		{"POST", q("/tmp/x", "user", sign("/tmp/x", "write", "user", tokenOf(a), "1790988051"), "1790988051"), a},
		{"GET", q("/tmp/x", "", sign("/tmp/x", "write", "", tokenOf(a), ""), ""), ""},   // signed for a write
		{"GET", q("/tmp/y", "", sign("/tmp/x", "read", "", tokenOf(a), ""), ""), ""},    // another path
		{"GET", q("/tmp/x", "", sign("/tmp/x", "read", "", "not-a-token", ""), ""), ""}, // nobody's token
	} {
		r := httptest.NewRequest(c.method, c.url, nil)
		tg, ok := fx.f.route(r)
		if !ok || (c.want == "") != tg.unmatched || tg.id != c.want || (c.want != "" && tg.port != EnvdPort) {
			t.Errorf("%s %s: got %+v %v, want %q", c.method, c.url, tg, ok, c.want)
		}
	}
	// A vector from the SDK itself: e2b.sandbox.signature.get_signature("/a", "read", None, "tok").
	if got := fileSignature("/a", "read", "", "tok", ""); got != "v1_2/gNbgFPxFSqkheS/SD0NHlzA121E/nxkOtc/ovxeoI" {
		t.Errorf("signature %s", got)
	}
	// A paused sandbox's URLs do not route.
	fx.do("POST", "/sandboxes/"+a+"/pause", adminKey, nil)
	r := httptest.NewRequest("GET", q("/tmp/x", "", sign("/tmp/x", "read", "", tokenOf(a), ""), ""), nil)
	if tg, _ := fx.f.route(r); !tg.unmatched {
		t.Errorf("paused sandbox matched: %+v", tg)
	}
	wantErr(t, fx.do("GET", q("/tmp/x", "", "v1_nope", ""), "", nil), 401, "signature")
}

// envelope is one Connect envelope.
func envelope(flags byte, msg string) []byte {
	b := make([]byte, 5, 5+len(msg))
	b[0] = flags
	binary.BigEndian.PutUint32(b[1:], uint32(len(msg)))
	return append(b, msg...)
}

func TestProxy(t *testing.T) {
	fx := newFixture(t)
	id := fx.create(nil)
	var seen *http.Request
	fx.guest = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r
		switch r.URL.Path {
		case "/process.Process/Start": // a stream that ends properly
			w.Header().Set("Content-Type", "application/connect+json")
			w.Write(envelope(0, `{"event":{"start":{"pid":7}}}`))
			w.(http.Flusher).Flush()
			w.Write(envelope(2, `{}`))
		case "/process.Process/Connect": // a stream the guest drops mid-way, as a pause does
			w.Header().Set("Content-Type", "application/connect+json")
			w.Write(envelope(0, `{"event":{"start":{"pid":7}}}`))
			w.(http.Flusher).Flush()
			conn, _, _ := http.NewResponseController(w).Hijack()
			conn.Close()
		default:
			w.Header().Set("Content-Type", "text/plain")
			io.WriteString(w, "port "+r.Host)
		}
	})

	w := fx.do("POST", "/process.Process/Start", "", nil, "E2b-Sandbox-Id", id, "E2b-Sandbox-Port", "49983", "X-Access-Token", "tok")
	want := append(envelope(0, `{"event":{"start":{"pid":7}}}`), envelope(2, `{}`)...)
	if w.Code != 200 || !bytes.Equal(w.Body.Bytes(), want) {
		t.Fatalf("stream: %d %q", w.Code, w.Body.Bytes())
	}
	if seen.Header.Get("X-Access-Token") != "tok" {
		t.Fatal("X-Access-Token was not passed through")
	}

	w = fx.do("POST", "/process.Process/Connect", "", nil, "E2b-Sandbox-Id", id)
	body := w.Body.Bytes()
	first := envelope(0, `{"event":{"start":{"pid":7}}}`)
	if !bytes.HasPrefix(body, first) {
		t.Fatalf("cut stream: %q", body)
	}
	end := body[len(first):]
	if len(end) < 5 || end[0] != 2 || !strings.Contains(string(end[5:]), `"code":"unavailable"`) ||
		!strings.Contains(string(end[5:]), "the connection to sandbox "+id+" ended before the stream completed") {
		t.Fatalf("cut stream ends with %q", end)
	}

	// User traffic by Host keeps the Host.
	w = fx.do("GET", "/", "", nil, "Host", "8080-"+id+".e2b.test:7823")
	if w.Code != 200 || w.Body.String() != "port 8080-"+id+".e2b.test:7823" {
		t.Fatalf("port: %d %q", w.Code, w.Body)
	}
	// envd's internal API is not reachable from outside.
	wantErr(t, fx.do("POST", "/init", "", map[string]any{}, "E2b-Sandbox-Id", id), 404, "internal")
	// No such sandbox: the proxy's 502.
	w = fx.do("GET", "/health", "", nil, "E2b-Sandbox-Id", "inope")
	if got := decode[map[string]any](t, w); w.Code != 502 || got["sandboxId"] != "inope" || got["code"] != 502.0 {
		t.Fatalf("unknown sandbox: %d %v", w.Code, got)
	}
	// Nothing listening: 502, port not open.
	fx.guest = nil
	w = fx.do("GET", "/", "", nil, "Host", "3000-"+id+".e2b.test")
	if got := decode[map[string]any](t, w); w.Code != 502 || got["message"] != "The sandbox is running but port is not open" {
		t.Fatalf("closed port: %d %v", w.Code, got)
	}
}

func TestEnvelopes(t *testing.T) {
	stream := append(append(envelope(0, `{"a":1}`), envelope(0, ``)...), envelope(2, `{}`)...)
	for split := range len(stream) {
		var e envelopes
		e.advance(stream[:split])
		if e.ended {
			t.Fatalf("ended after %d bytes", split)
		}
		e.advance(stream[split:])
		if !e.ended || !e.atBoundary() {
			t.Fatalf("split at %d: %+v", split, e)
		}
	}
	var e envelopes
	e.advance(envelope(0, `{"a":1}`)[:7])
	if e.atBoundary() {
		t.Fatal("inside a message is not a boundary")
	}
}

func TestInitEnvd(t *testing.T) {
	fx := newFixture(t)
	id := fx.create(map[string]any{"envVars": map[string]string{"K": "V"}})
	rec, _ := fx.st.GetRecord(id)
	m, _ := metaOf(rec)

	var mu sync.Mutex
	var inits []initRequest
	refuse := 3 // the first dials find nothing listening: envd is still starting
	fx.f.dialEnvd = func(*vmm.Machine) dialFunc {
		return func(ctx context.Context, port string) (net.Conn, error) {
			if port != EnvdPort {
				t.Errorf("dialled port %s", port)
			}
			mu.Lock()
			defer mu.Unlock()
			if refuse > 0 {
				refuse--
				return nil, &net.OpError{Op: "dial", Err: io.EOF}
			}
			return fx.dial(ctx, port)
		}
	}
	status := http.StatusNoContent
	fx.guest = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.URL.Path != "/init" || r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("%s %s %s", r.Method, r.URL.Path, r.Header.Get("Content-Type"))
		}
		var req initRequest
		json.NewDecoder(r.Body).Decode(&req)
		mu.Lock()
		inits = append(inits, req)
		mu.Unlock()
		w.WriteHeader(status)
	})

	if err := fx.f.initEnvd(context.Background(), engine.Boot{Record: rec}); err != nil {
		t.Fatalf("cold boot: %v", err)
	}
	if len(inits) != 1 {
		t.Fatalf("%d inits", len(inits))
	}
	got := inits[0]
	if got.AccessToken != m.AccessToken || got.DefaultUser != "user" || got.DefaultWorkdir != "/home/user" || got.EnvVars["K"] != "V" ||
		got.EnvVars["E2B_SANDBOX"] != "true" || got.EnvVars["E2B_SANDBOX_ID"] != id || got.EnvVars["E2B_TEMPLATE_ID"] != "base" {
		t.Fatalf("init %+v", got)
	}
	if ts, err := time.Parse(time.RFC3339Nano, got.Timestamp); err != nil || time.Since(ts) > time.Minute {
		t.Fatalf("timestamp %q", got.Timestamp)
	}

	// Another API's VM is none of its business.
	if err := fx.f.initEnvd(context.Background(), engine.Boot{Record: store.Record{ID: "x"}}); err != nil || len(inits) != 1 {
		t.Fatalf("a sprite's boot: %v, %d inits", err, len(inits))
	}

	// envd already has another token: a cold boot fails at once, a warm one carries on.
	status = http.StatusUnauthorized
	start := time.Now()
	if err := fx.f.initEnvd(context.Background(), engine.Boot{Record: rec}); err == nil || time.Since(start) > 5*time.Second {
		t.Fatalf("cold boot with envd refusing: %v after %v", err, time.Since(start))
	}
	if err := fx.f.initEnvd(context.Background(), engine.Boot{Record: rec, Warm: true}); err != nil {
		t.Fatalf("warm resume with envd refusing: %v", err)
	}

	// envd never answers: the start fails when its context runs out.
	fx.guest = nil
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	if err := fx.f.initEnvd(ctx, engine.Boot{Record: rec}); err == nil {
		t.Fatal("cold boot with no envd succeeded")
	}
}

func TestNewSandboxID(t *testing.T) {
	seen := map[string]bool{}
	re := regexp.MustCompile(`^i[a-z0-9]{20}$`)
	for range 1000 {
		id := newSandboxID()
		if !re.MatchString(id) || seen[id] {
			t.Fatalf("id %q", id)
		}
		seen[id] = true
	}
}
