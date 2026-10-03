package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	sprites "github.com/superfly/sprites-go"

	"github.com/arugula-salad/wisp/engine"
	"github.com/arugula-salad/wisp/internal/store"
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
	s := New(testURLs(opts, "acme", "0"), st, engine.New(opts.Options, st, log), log, "tok")
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
	_, h := newOperatorServer(t, Options{MaxSprites: 2, Options: engine.Options{MaxRunning: 5}})
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

// A create the disk guard refuses is 507 insufficient_storage, and leaves no
// sprite behind.
func TestDiskGuardRefusalInUpstreamsShape(t *testing.T) {
	_, h := newOperatorServer(t, Options{Options: engine.Options{DiskReserve: 1 << 62}})
	resp := apiCall(t, h, "POST", "/v1/sprites", `{"name":"full"}`)
	if e := apiError(t, resp); e.StatusCode != http.StatusInsufficientStorage || e.ErrorCode != "insufficient_storage" {
		t.Fatalf("create on a full volume = %+v", e)
	}
	if resp := apiCall(t, h, "GET", "/v1/sprites/full", ""); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("a refused create left a sprite behind: %d", resp.StatusCode)
	}
}

func TestStatusLiveAndOffline(t *testing.T) {
	s, h := newOperatorServer(t, Options{Options: engine.Options{MaxRunning: 3}})
	apiCall(t, h, "POST", "/v1/sprites", `{"name":"one"}`)
	sp, _ := s.store.GetByName(store.Sprites, "one")
	if _, err := s.life.CreateCheckpoint(sp.Record, nil, "", func(string, ...any) {}); err != nil {
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
