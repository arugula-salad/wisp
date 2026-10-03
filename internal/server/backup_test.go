package server

import (
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/arugula-salad/wisp/engine"
	"github.com/arugula-salad/wisp/internal/s3/s3test"
	"github.com/arugula-salad/wisp/internal/store"
)

// Backups through the Sprites API: the nobackup label opts a sprite out, and
// the API's view of a sprite carries its backup state. The periodic loop asks
// for every sprite's first backup, which is what drives it here; the engine's
// tests cover the manager itself (engine/backup_test.go).
func TestBackupsThroughTheAPI(t *testing.T) {
	srv := s3test.New("buck")
	t.Cleanup(srv.Close)
	t.Setenv("AWS_ACCESS_KEY_ID", "GKtest")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "secret")
	s, h := newOperatorServer(t, Options{Options: engine.Options{Backup: engine.BackupOptions{
		Endpoint: srv.URL, Bucket: "buck", Region: "home-cloud", Parallel: 4, Interval: time.Second}}})
	status(t, apiCall(t, h, "POST", "/v1/sprites", `{"name":"dev"}`), 201)
	status(t, apiCall(t, h, "POST", "/v1/sprites", `{"name":"skip","labels":["`+NoBackupLabel+`"]}`), 201)
	dev, _ := s.store.GetByName(store.Sprites, "dev")
	skip, _ := s.store.GetByName(store.Sprites, "skip")

	deadline := time.Now().Add(15 * time.Second)
	for st := s.life.BackupState(dev.ID); st == nil || st.LastAt == nil; st = s.life.BackupState(dev.ID) {
		if time.Now().After(deadline) {
			t.Fatalf("dev was never backed up: %+v", st)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if got := s.render(dev); got.Backup == nil || got.Backup.LastAt == nil {
		t.Fatalf("render did not carry the backup state: %+v", got.Backup)
	}
	if st := s.life.BackupState(skip.ID); st.LastAt != nil {
		t.Fatalf("a nobackup sprite was backed up: %+v", st)
	}
	for _, key := range srv.Keys() {
		if strings.HasPrefix(key, "sprites/"+skip.ID) {
			t.Fatalf("a nobackup sprite wrote %s", key)
		}
	}
}

func TestRenderOmitsBackupWhenNoBucketIsConfigured(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	opts := Options{Options: engine.Options{NoNetwork: true}}
	s := New(testURLs(opts, "org", "0"), st, engine.New(opts.Options, st, log), log, "tok")
	sp := store.Sprite{Record: store.Record{ID: "x"}, SpriteMeta: store.SpriteMeta{Name: "dev"}}
	if got := s.render(sp); got.Backup != nil {
		t.Errorf("backup state reported although backups are off: %+v", got.Backup)
	}
}
