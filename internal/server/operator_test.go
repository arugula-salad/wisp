package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	sprites "github.com/superfly/sprites-go"

	"github.com/arugula-salad/wisp/internal/store"
	"github.com/arugula-salad/wisp/internal/vmm"
)

// newOperatorServer is a daemon with a tiny base image and no VMs: enough for
// everything the operator features do short of booting one.
func newOperatorServer(t *testing.T, opts Options) (*Server, http.Handler) {
	t.Helper()
	opts.DataDir, opts.NoNetwork = t.TempDir(), true
	opts.BaseImage = filepath.Join(opts.DataDir, "base.ext4")
	if err := os.WriteFile(opts.BaseImage, bytes.Repeat([]byte("base"), 4096), 0o644); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(opts.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	s := New(testURLs(opts, "acme", "0"), st, NewLifecycle(opts, st, log), log, "tok")
	return s, s.Handler()
}

// testURLs gives opts the organization and sprite URLs New reports.
func testURLs(opts Options, org, urlFmt string) Options {
	opts.Org, opts.URLDomains, opts.URLFormat = org, []string{"sprites.localhost"}, urlFmt
	return opts
}

func apiCall(t *testing.T, h http.Handler, method, path, body string) *http.Response {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer tok")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Result()
}

// apiError runs a response through the official SDK's own parser.
func apiError(t *testing.T, resp *http.Response) *sprites.APIError {
	t.Helper()
	body, _ := io.ReadAll(resp.Body)
	e := sprites.ParseAPIError(resp, body)
	if e == nil {
		t.Fatalf("status %d is not an error: %s", resp.StatusCode, body)
	}
	return e
}

func TestListCarriesOrgAndMaxSpritesIsEnforced(t *testing.T) {
	_, h := newOperatorServer(t, Options{MaxSprites: 2, MaxRunning: 5})
	for _, name := range []string{"a", "b"} {
		if resp := apiCall(t, h, "POST", "/v1/sprites", `{"name":"`+name+`"}`); resp.StatusCode != http.StatusCreated {
			t.Fatalf("create %s: %d", name, resp.StatusCode)
		}
	}
	e := apiError(t, apiCall(t, h, "POST", "/v1/sprites", `{"name":"c"}`))
	if e.StatusCode != http.StatusForbidden || e.ErrorCode != codeSpriteLimit || e.Limit != 2 || e.CurrentCount != 2 || e.RetryAfterSeconds != 0 {
		t.Fatalf("limit error = %+v", e)
	}

	var list sprites.SpriteList
	if err := json.NewDecoder(apiCall(t, h, "GET", "/v1/sprites?max_results=1", "").Body).Decode(&list); err != nil {
		t.Fatal(err)
	}
	// The counts cover the org, not the page.
	if want := (sprites.OrgInfo{Name: "acme", Cold: 2, RunningLimit: 5}); list.Org == nil || *list.Org != want {
		t.Fatalf("org = %+v, want %+v", list.Org, want)
	}
}

func TestMaxRunningRefusesAWakeInUpstreamsShape(t *testing.T) {
	s, _ := newOperatorServer(t, Options{MaxRunning: 1, IdleTimeout: 30 * time.Second})
	if err := s.life.reserveRun(); err != nil {
		t.Fatal(err)
	}
	err := s.life.reserveRun()
	var lim *LimitError
	if !errors.As(err, &lim) {
		t.Fatalf("second reservation: %v", err)
	}
	rec := httptest.NewRecorder()
	s.writeWakeErr(rec, "x", err)
	e := apiError(t, rec.Result())
	if !e.IsRateLimitError() || !e.IsConcurrentLimitExceeded() || e.Limit != 1 || e.CurrentCount != 1 || e.GetRetryAfterSeconds() != 30 || e.RetryAfterHeader != 30 {
		t.Fatalf("limit error = %+v", e)
	}
	s.life.releaseRun()
	if err := s.life.reserveRun(); err != nil {
		t.Fatalf("a freed slot should be usable: %v", err)
	}
}

// fakeVolume stands in for statfs: free space is whatever the test last stored.
// (The guard also probes from its own goroutines, hence the atomic.)
func fakeVolume(s *Server) *atomic.Int64 {
	free := new(atomic.Int64)
	s.life.disk.probe = func() (Headroom, error) {
		return Headroom{VolumeTotal: 40 << 30, VolumeFree: free.Load(), Free: free.Load()}, nil
	}
	return free
}

func TestDiskGuardRefusesCreatesAndCheckpoints(t *testing.T) {
	s, h := newOperatorServer(t, Options{DiskReserve: 2 << 30})
	vol := fakeVolume(s)
	vol.Store(3 << 30)
	if resp := apiCall(t, h, "POST", "/v1/sprites", `{"name":"fits"}`); resp.StatusCode != http.StatusCreated {
		t.Fatalf("create with room: %d", resp.StatusCode)
	}

	vol.Store(1 << 30)
	resp := apiCall(t, h, "POST", "/v1/sprites", `{"name":"full"}`)
	if e := apiError(t, resp); e.StatusCode != http.StatusInsufficientStorage || e.ErrorCode != "insufficient_storage" {
		t.Fatalf("create on a full volume = %+v", e)
	}
	if _, err := s.store.Get("full"); err == nil {
		t.Fatal("a refused create left a sprite behind")
	}
	sp, _ := s.store.Get("fits")
	_, err := s.createCheckpointLocked(s.life.rt(sp.ID), "fits", "", false, func(string, ...any) {})
	if !errors.Is(err, errNoRoom) {
		t.Fatalf("checkpoint on a full volume: %v", err)
	}
	if got, _ := s.store.Get("fits"); len(got.Checkpoints) != 0 {
		t.Fatal("a refused checkpoint was recorded")
	}
}

func TestMakeRoomTurnsTheOldestWarmSpritesCold(t *testing.T) {
	s, _ := newOperatorServer(t, Options{})
	vol := fakeVolume(s)
	const snap = 1 << 20
	warmed := time.Now().Add(-time.Hour)
	for i, name := range []string{"oldest", "older", "newest", "suspending"} {
		sp := &store.Sprite{ID: store.NewID(), Name: name, CreatedAt: time.Now()}
		if err := s.store.Create(sp); err != nil {
			t.Fatal(err)
		}
		if name == "suspending" {
			continue
		}
		at := warmed.Add(time.Duration(i) * time.Minute)
		s.store.Update(name, func(sp *store.Sprite) { sp.LastWarmingAt = &at })
		for _, f := range []string{"snap.vmstate", "snap.mem"} {
			if err := os.WriteFile(filepath.Join(s.store.Dir(sp.ID), f), bytes.Repeat([]byte{1}, snap/2), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	warm := func(name string) bool {
		sp, _ := s.store.Get(name)
		return vmm.HasSnapshot(s.store.Dir(sp.ID))
	}
	me, _ := s.store.Get("suspending")

	room := func(need int64) bool {
		release, fits := s.life.makeRoom(me, need)
		release()
		return fits
	}
	vol.Store(10 * snap)
	if !room(5*snap) || !warm("oldest") {
		t.Fatal("a snapshot that fits should cost nobody anything")
	}
	// Two suspends at once may not both be promised the same free bytes.
	release, fits := s.life.makeRoom(me, 9*snap)
	if !fits || !warm("oldest") {
		t.Fatal("9 of 10 free should fit")
	}
	vol.Store(10*snap + snap/2) // what a second suspend sees while the first still writes
	other, _ := s.store.Get("newest")
	if _, fits := s.life.makeRoom(other, 9*snap); fits {
		t.Fatal("the same space was promised twice")
	}
	release()
	if !warm("oldest") || !warm("older") {
		t.Fatal("an attempt that could not succeed still cost sprites their memory state")
	}

	// Half a snapshot short: one demotion covers it, and it is the oldest that goes.
	vol.Store(snap)
	if !room(snap + snap/2) {
		t.Fatal("no room even after a demotion")
	}
	if warm("oldest") || !warm("older") || !warm("newest") {
		t.Fatalf("warm after one demotion: oldest=%v older=%v newest=%v", warm("oldest"), warm("older"), warm("newest"))
	}

	// Hopeless: the caller is told so, and nobody is turned cold for nothing.
	vol.Store(snap)
	if room(100 * snap) {
		t.Fatal("reported room that is not there")
	}
	if !warm("older") || !warm("newest") {
		t.Fatal("warm sprites were dropped for a snapshot that could never fit")
	}
}

func TestExclusiveCountsOnlyUnsharedBytes(t *testing.T) {
	owners := [][]span{
		merge([]span{{0, 60}, {40, 100}}), // overlaps itself: still one owner
		{{50, 150}},
		{{200, 300}},
		nil,
	}
	got := exclusive(owners)
	for i, want := range []int64{50, 50, 100, 0} {
		if got[i] != want {
			t.Fatalf("owner %d: %d exclusive bytes, want %d (all: %v)", i, got[i], want, got)
		}
	}
	if n := total(owners[0]); n != 100 {
		t.Fatalf("merged total = %d, want 100", n)
	}
}

func TestStatusLiveAndOffline(t *testing.T) {
	s, h := newOperatorServer(t, Options{MaxRunning: 3})
	apiCall(t, h, "POST", "/v1/sprites", `{"name":"one"}`)
	sp, _ := s.store.Get("one")
	if _, err := s.createCheckpointLocked(s.life.rt(sp.ID), "one", "", false, func(string, ...any) {}); err != nil {
		t.Fatal(err)
	}

	live := s.status(context.Background(), time.Now(), "127.0.0.1:0")
	off, err := OfflineStatus(s.opts.DataDir, filepath.Join(s.opts.DataDir, "no-netd.sock"))
	if err != nil {
		t.Fatal(err)
	}
	if live.Daemon == nil || live.Daemon.Pid != os.Getpid() || off.Daemon != nil {
		t.Fatalf("daemon: live=%+v offline=%+v", live.Daemon, off.Daemon)
	}
	for _, st := range []Status{live, off} {
		if len(st.Sprites) != 1 {
			t.Fatalf("sprites = %+v", st.Sprites)
		}
		got := st.Sprites[0]
		if got.Name != "one" || got.State != "cold" || got.Checkpoints != 1 || got.DiskUsed <= 0 || got.DiskExclusive > got.DiskUsed {
			t.Fatalf("sprite = %+v", got)
		}
		if st.Host.Cold != 1 || st.Host.Volume.VolumeTotal == 0 || st.Orphans == nil {
			t.Fatalf("host = %+v orphans = %v", st.Host, st.Orphans)
		}
	}
	if live.Host.MaxRunning != 3 || off.Host.PolicyHelper.Reachable {
		t.Fatalf("live host = %+v, offline helper = %+v", live.Host, off.Host.PolicyHelper)
	}
	// Scripts depend on these names.
	b, _ := json.Marshal(live)
	for _, field := range []string{`"daemon"`, `"sprites"`, `"orphans"`, `"other_daemons"`, `"state"`, `"disk_used_bytes"`,
		`"disk_exclusive_bytes"`, `"task_holds"`, `"policy_restricted"`, `"volume_free_bytes"`, `"taps_used"`, `"policy_helper"`} {
		if !bytes.Contains(b, []byte(field)) {
			t.Errorf("status JSON lost %s", field)
		}
	}
}
