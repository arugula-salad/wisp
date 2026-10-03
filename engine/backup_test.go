package engine

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/arugula-salad/wisp/internal/backup"
	"github.com/arugula-salad/wisp/internal/s3/s3test"
	"github.com/arugula-salad/wisp/internal/store"
	"github.com/arugula-salad/wisp/internal/vmm"
)

// noBackup is the label testFilter opts a sprite out with, as the Sprites
// front end's nobackup label does.
const noBackup = "nobackup"

// testFilter is a backup filter like the Sprites front end's.
func testFilter(sp store.Sprite) bool { return !slices.Contains(sp.Labels, noBackup) }

// newBackupEngine is an engine with one never-booted sprite and a stub bucket, so
// the manager can be driven without a VM. Interval is 0: the periodic loop has its
// own coverage and would otherwise race these assertions.
func newBackupEngine(t *testing.T, labels ...string) (*Engine, store.Sprite, *s3test.Server) {
	t.Helper()
	srv := s3test.New("buck")
	t.Cleanup(srv.Close)
	t.Setenv("AWS_ACCESS_KEY_ID", "GKtest")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "secret")

	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	opts := Options{NoNetwork: true, Backup: BackupOptions{
		Endpoint: srv.URL, Bucket: "buck", Region: "home-cloud", Parallel: 4}}
	l := New(opts, st, log)
	l.SetDescriber(spriteName)
	l.SetBackupFilter(testFilter)
	if l.backups == nil {
		t.Fatal("backups should be enabled")
	}

	sp := &store.Sprite{ID: store.NewID(), Name: "dev", Labels: labels, CreatedAt: time.Now().UTC()}
	if err := st.Create(sp); err != nil {
		t.Fatal(err)
	}
	// A 6 MiB sparse "disk": two chunks, one of them a hole.
	disk := filepath.Join(st.Dir(sp.ID), vmm.DiskFile)
	f, err := os.Create(disk)
	if err != nil {
		t.Fatal(err)
	}
	f.Truncate(6 << 20)
	f.WriteAt([]byte("superblock"), 0)
	f.Close()
	return l, *sp, srv
}

// waitBackup blocks until the manager has finished whatever it is doing.
func waitBackup(t *testing.T, l *Engine, id string) *BackupState {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		st := l.backups.State(id)
		if st.Phase != "queued" && st.Phase != "running" {
			// One more look: the phase is set before the queue is drained.
			l.backups.mu.Lock()
			idle := len(l.backups.order) == 0 && l.backups.running == ""
			l.backups.mu.Unlock()
			if idle {
				return st
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("backup of %s did not finish", id)
	return nil
}

func TestBackupUploadsAndReportsItsState(t *testing.T) {
	l, sp, srv := newBackupEngine(t)

	l.backups.Enqueue(sp.ID, "suspend")
	st := waitBackup(t, l, sp.ID)
	if st.Phase != "idle" || st.Error != "" || st.LastAt == nil {
		t.Fatalf("state after a successful backup: %+v", st)
	}
	if st.LastBytes == 0 {
		t.Error("a first backup should report the bytes it uploaded")
	}

	// A manifest, a latest pointer and exactly one chunk (the other is a hole).
	var manifests, chunks int
	for _, key := range srv.Keys() {
		switch {
		case strings.HasPrefix(key, "sprites/"+sp.ID+"/manifests/"):
			manifests++
		case strings.HasPrefix(key, "chunks/"):
			chunks++
		}
	}
	if manifests != 1 || chunks != 1 {
		t.Errorf("bucket holds %d manifests and %d chunks, want 1 and 1: %v", manifests, chunks, srv.Keys())
	}
	body, ok := srv.Get("sprites/" + sp.ID + "/latest.json")
	if !ok {
		t.Fatal("no latest.json")
	}
	var m backup.Manifest
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatal(err)
	}
	if m.Reason != "suspend" || m.Sprite.Name != "dev" {
		t.Errorf("manifest = %+v", m)
	}
	if got := l.BackupState(sp.ID); got == nil || got.LastAt == nil {
		t.Fatalf("BackupState = %+v", got)
	}
}

func TestTheBackupFilterOptsOut(t *testing.T) {
	l, sp, srv := newBackupEngine(t, noBackup)

	l.backups.Enqueue(sp.ID, "suspend")
	// Nothing to wait for: the enqueue is refused outright.
	time.Sleep(50 * time.Millisecond)
	if st := l.backups.State(sp.ID); st.Phase != "idle" || st.LastAt != nil {
		t.Fatalf("a nobackup sprite was backed up: %+v", st)
	}
	for _, key := range srv.Keys() {
		if strings.HasPrefix(key, "sprites/") || strings.HasPrefix(key, "chunks/") {
			t.Fatalf("a nobackup sprite wrote %s", key)
		}
	}
}

func TestBackupFailureIsVisibleAndNotFatal(t *testing.T) {
	l, sp, srv := newBackupEngine(t)
	srv.SetFail(func(method, key string) int {
		if method == http.MethodPut && strings.HasPrefix(key, "chunks/") {
			return http.StatusForbidden
		}
		return 0
	})

	l.backups.Enqueue(sp.ID, "suspend")
	st := waitBackup(t, l, sp.ID)
	if st.Phase != "error" || st.Error == "" || st.Failures != 1 {
		t.Fatalf("a failed backup should be visible: %+v", st)
	}
	if st.LastAt != nil {
		t.Error("a failed backup must not claim a recovery point")
	}
	// The sprite is otherwise untouched.
	if got := l.Status(sp.Record); got != "cold" {
		t.Errorf("status = %s", got)
	}

	// It recovers: the bucket comes back and the next attempt clears the error.
	srv.SetFail(nil)
	l.backups.Enqueue(sp.ID, "periodic")
	if st := waitBackup(t, l, sp.ID); st.Phase != "idle" || st.Error != "" || st.Failures != 0 || st.LastAt == nil {
		t.Fatalf("state after recovery: %+v", st)
	}
}

// Without reflink support the disk is read in place, so a running sprite is
// deferred rather than paused for the length of an upload or read torn.
func TestBackupDefersWhenTheSpriteIsRunning(t *testing.T) {
	l, sp, srv := newBackupEngine(t)
	l.storage.reflink = false

	rt := l.rt(sp.ID)
	rt.mu.Lock()
	rt.m = &vmm.Machine{}
	rt.mu.Unlock()
	l.backups.Enqueue(sp.ID, "periodic")
	st := waitBackup(t, l, sp.ID)

	if st.Phase != "idle" || st.LastAt != nil {
		t.Fatalf("a deferred backup should leave the sprite idle with no recovery point: %+v", st)
	}
	if st.Error != "" {
		t.Errorf("deferring is not a failure, but the state carries %q", st.Error)
	}
	for _, key := range srv.Keys() {
		if strings.HasPrefix(key, "sprites/") {
			t.Errorf("a deferred backup wrote %s", key)
		}
	}

	// Once it has stopped the same request succeeds.
	rt.mu.Lock()
	rt.m = nil
	rt.mu.Unlock()
	l.backups.Enqueue(sp.ID, "suspend")
	if st := waitBackup(t, l, sp.ID); st.LastAt == nil {
		t.Fatalf("backup after the sprite stopped: %+v", st)
	}
}

// The other half of reading in place: a sprite that wakes while its disk is being
// uploaded. The wake is not held up, so the disk moves under the reader, and what
// must not happen is a manifest of that torn read reaching the bucket.
func TestAWakeDuringAnInPlaceUploadLeavesNoManifest(t *testing.T) {
	l, sp, srv := newBackupEngine(t)
	l.storage.reflink = false

	// The wake arrives while the first chunk is on the wire.
	rt := l.rt(sp.ID)
	srv.SetFail(func(method, key string) int {
		if method == http.MethodPut && strings.HasPrefix(key, "chunks/") {
			rt.gen.Add(1) // what startLocked does
		}
		return 0
	})
	l.backups.Enqueue(sp.ID, "suspend")
	st := waitBackup(t, l, sp.ID)
	if st.Phase != "idle" || st.Error != "" || st.LastAt != nil {
		t.Fatalf("a backup overtaken by a wake is deferred, not failed and not claimed: %+v", st)
	}
	for _, key := range srv.Keys() {
		if strings.HasPrefix(key, "sprites/") {
			t.Errorf("a torn read was committed: %s", key)
		}
	}

	srv.SetFail(nil)
	l.backups.Enqueue(sp.ID, "suspend")
	if st := waitBackup(t, l, sp.ID); st.LastAt == nil {
		t.Fatalf("backup after the next suspend: %+v", st)
	}
}

// A bucket that is down when wispd starts is visible on every sprite, costs
// nothing else, and is picked up again without a restart.
func TestABucketThatIsDownAtStartupIsReportedAndRetried(t *testing.T) {
	srv := s3test.New("buck")
	t.Cleanup(srv.Close)
	srv.SetFail(func(string, string) int { return http.StatusServiceUnavailable })
	t.Setenv("AWS_ACCESS_KEY_ID", "GKtest")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "secret")

	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	sp := &store.Sprite{ID: store.NewID(), Name: "dev", CreatedAt: time.Now().UTC()}
	if err := st.Create(sp); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(st.Dir(sp.ID), vmm.DiskFile), []byte("superblock"), 0o644); err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	opts := Options{NoNetwork: true, Backup: BackupOptions{Endpoint: srv.URL, Bucket: "buck", Region: "home-cloud"}}
	l := New(opts, st, log)

	l.backups.Enqueue(sp.ID, "suspend")
	if got := waitBackup(t, l, sp.ID); got.Phase != "error" || !strings.Contains(got.Error, "Service Unavailable") || got.LastAt != nil {
		t.Fatalf("state with the bucket down: %+v", got)
	}
	// A sprite that has not tried yet says so too.
	if got := l.backups.State("some-other-sprite"); got.Phase != "error" || got.Error == "" {
		t.Errorf("an unreachable bucket should show on every sprite: %+v", got)
	}

	srv.SetFail(nil)
	l.backups.Enqueue(sp.ID, "catch-up")
	if got := waitBackup(t, l, sp.ID); got.Phase != "idle" || got.Error != "" || got.LastAt == nil {
		t.Fatalf("state once the bucket is back: %+v", got)
	}
	if got := l.backups.State("some-other-sprite"); got.Error != "" {
		t.Errorf("the bucket is back but sprites still report %q", got.Error)
	}
}

// A restart must not forget recovery points: they are in the bucket.
func TestRecoveryPointsSurviveARestart(t *testing.T) {
	l, sp, srv := newBackupEngine(t)
	l.backups.Enqueue(sp.ID, "suspend")
	first := waitBackup(t, l, sp.ID)

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	opts := Options{NoNetwork: true, Backup: BackupOptions{Endpoint: srv.URL, Bucket: "buck", Region: "home-cloud"}}
	again := New(opts, l.store, log)
	if _, err := again.backups.repository(context.Background()); err != nil {
		t.Fatal(err)
	}
	got := again.backups.State(sp.ID)
	if got.LastAt == nil || !got.LastAt.Equal(*first.LastAt) {
		t.Fatalf("recovery point after a restart = %v, want %v", got.LastAt, first.LastAt)
	}
}

func TestDeleteTombstonesInTheBucket(t *testing.T) {
	l, sp, srv := newBackupEngine(t)
	l.backups.Enqueue(sp.ID, "suspend")
	waitBackup(t, l, sp.ID)

	l.backups.MarkDeleted(sp)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, ok := srv.Get("sprites/" + sp.ID + "/deleted.json"); ok {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	body, ok := srv.Get("sprites/" + sp.ID + "/deleted.json")
	if !ok {
		t.Fatal("deleting a sprite left no tombstone")
	}
	var tomb backup.Tombstone
	if err := json.Unmarshal(body, &tomb); err != nil || tomb.Name != "dev" {
		t.Fatalf("tombstone = %s (%v)", body, err)
	}
	// The backup itself survives: that is the whole point of a tombstone.
	if _, ok := srv.Get("sprites/" + sp.ID + "/latest.json"); !ok {
		t.Error("deleting a sprite removed its backup")
	}
}

// The restore path, end to end through the store, without a VM: back a sprite up,
// lose the entire data directory, and rebuild it somewhere else.
func TestRestoreRebuildsTheMachineDirectory(t *testing.T) {
	l, sp, srv := newBackupEngine(t)
	l.backups.Enqueue(sp.ID, "suspend")
	waitBackup(t, l, sp.ID)

	original, err := os.ReadFile(filepath.Join(l.store.Dir(sp.ID), vmm.DiskFile))
	if err != nil {
		t.Fatal(err)
	}

	// A fresh host: new data directory, same bucket.
	fresh := t.TempDir()
	st2, err := store.Open(fresh)
	if err != nil {
		t.Fatal(err)
	}
	repo, err := backup.Open(context.Background(), backup.Config{Endpoint: srv.URL, Bucket: "buck",
		Region: "home-cloud", Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	info, _, err := repo.Find(context.Background(), "dev")
	if err != nil {
		t.Fatal(err)
	}
	rec := backup.RestoreRecord(info.Latest)
	if err := st2.Create(&rec); err != nil {
		t.Fatal(err)
	}
	if rec.NetIndex == 0 {
		t.Error("the restored sprite should get an address on this host")
	}
	if _, err := repo.Restore(context.Background(), info.Latest, st2.Dir(rec.ID), nil); err != nil {
		t.Fatalf("restore: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(st2.Dir(rec.ID), vmm.DiskFile))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(original) || string(got) != string(original) {
		t.Fatalf("restored disk differs: %d bytes vs %d", len(got), len(original))
	}
}

func TestBackupsAreOffWithoutABucket(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	l := New(Options{NoNetwork: true}, st, log)
	if l.backups != nil || l.BackupState("x") != nil {
		t.Fatal("backups should be off without a bucket")
	}
	// And the lifecycle's hook is a no-op rather than a panic.
	l.backups.Enqueue("x", "suspend")
}

func TestCloneReflinkRefusesToFallBack(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "disk.ext4")
	if err := os.WriteFile(src, []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(dir, ".backup.ext4")
	err := cloneReflink(src, dst)
	if !ProbeReflink(dir) {
		// The point of this function: it errors rather than copying 20 GB while the
		// caller holds a lifecycle lock.
		if err == nil {
			t.Fatal("cloneReflink should fail where the filesystem has no reflinks")
		}
		if _, statErr := os.Stat(dst); statErr == nil {
			t.Error("a failed clone left a file behind")
		}
		return
	}
	if err != nil {
		t.Fatalf("cloneReflink on a reflink-capable filesystem: %v", err)
	}
	if b, _ := os.ReadFile(dst); string(b) != "data" {
		t.Errorf("clone content %q", b)
	}
}

// A sprite that never reached the bucket has nothing there to mark as deleted, and
// a tombstone for it would sit in `wispd backups list` for the whole retention.
func TestDeletingASpriteThatWasNeverBackedUpLeavesNothing(t *testing.T) {
	l, sp, srv := newBackupEngine(t)
	l.backups.tombstone(sp)
	for _, key := range srv.Keys() {
		if strings.HasPrefix(key, "sprites/") {
			t.Errorf("the bucket holds %s for a sprite it never backed up", key)
		}
	}
}
