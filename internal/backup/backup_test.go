package backup

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/jhgaylor/mini-sprites/internal/s3/s3test"
	"github.com/jhgaylor/mini-sprites/internal/store"
)

// stubServer is the in-process stand-in for Garage.
func stubServer(t *testing.T) *s3test.Server {
	t.Helper()
	srv := s3test.New("buck")
	t.Cleanup(srv.Close)
	return srv
}

func testRepo(t *testing.T, srv *s3test.Server, keyFile string) *Repo {
	t.Helper()
	t.Setenv("AWS_ACCESS_KEY_ID", "GKtest")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "secret")
	r, err := Open(context.Background(), Config{Endpoint: srv.URL, Bucket: "buck", Region: "home-cloud",
		KeyFile: keyFile, Parallel: 4,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatalf("open repo: %v", err)
	}
	return r
}

func writeKeyFile(t *testing.T, hexKey string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "backup.key")
	if err := os.WriteFile(path, []byte(hexKey+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// sparseDisk stands in for disk.ext4: a large mostly-empty file with data at a
// few offsets, which is what a real sprite disk looks like.
func sparseDisk(t *testing.T, path string, size int64, at map[int64]string) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := f.Truncate(size); err != nil {
		t.Fatal(err)
	}
	for off, data := range at {
		if _, err := f.WriteAt([]byte(data), off); err != nil {
			t.Fatal(err)
		}
	}
}

func sameFile(t *testing.T, a, b string) {
	t.Helper()
	ab, err := os.ReadFile(a)
	if err != nil {
		t.Fatal(err)
	}
	bb, err := os.ReadFile(b)
	if err != nil {
		t.Fatal(err)
	}
	if len(ab) != len(bb) {
		t.Fatalf("%s is %d bytes, %s is %d", a, len(ab), b, len(bb))
	}
	if !bytes.Equal(ab, bb) {
		sa, sb := sha256.Sum256(ab), sha256.Sum256(bb)
		t.Fatalf("%s and %s differ (%s vs %s)", a, b, hex.EncodeToString(sa[:8]), hex.EncodeToString(sb[:8]))
	}
}

// allocatedBytes is what the file really occupies, so a test can tell a restored
// sparse file from a restored file full of zeroes.
func allocatedBytes(t *testing.T, path string) int64 {
	t.Helper()
	var st syscall.Stat_t
	if err := syscall.Stat(path, &st); err != nil {
		t.Fatal(err)
	}
	return st.Blocks * 512
}

func sprite(name string) store.Sprite {
	return store.Sprite{ID: "abc123", Name: name, NetIndex: 7, BootIP: "10.209.0.7/16",
		CreatedAt: time.Now().UTC().Truncate(time.Second)}
}

func TestSaveRestoreRoundTrip(t *testing.T) {
	srv := stubServer(t)
	r := testRepo(t, srv, "")
	ctx := context.Background()

	dir := t.TempDir()
	disk := filepath.Join(dir, "disk.ext4")
	// 12 MiB: three chunks, only the first and third holding anything.
	sparseDisk(t, disk, 12<<20, map[int64]string{0: "superblock", 9 << 20: "late write"})
	os.MkdirAll(filepath.Join(dir, "checkpoints"), 0o755)
	cp := filepath.Join(dir, "checkpoints", "v1.ext4")
	sparseDisk(t, cp, 12<<20, map[int64]string{0: "superblock", 9 << 20: "late write"})

	refs, err := SpriteFiles(dir, disk, "disk.ext4")
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 2 || refs[1].Name != "checkpoints/v1.ext4" {
		t.Fatalf("SpriteFiles = %+v", refs)
	}

	sp := sprite("dev")
	m, stats, err := r.Save(ctx, sp, refs, "suspend")
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	// The hole in the middle is never read, and the checkpoint is byte-identical to
	// the disk, so a 24 MiB pair of files costs four chunks and uploads two.
	if stats.Chunks != 4 {
		t.Errorf("manifest references %d chunks, want 4", stats.Chunks)
	}
	if stats.NewChunks != 2 {
		t.Errorf("uploaded %d new chunks, want 2 (the checkpoint dedups against the disk)", stats.NewChunks)
	}
	// Two chunks per file are read, not three: the 4-8 MiB hole is never touched.
	if stats.Read != 16<<20 {
		t.Errorf("read %s of a mostly-sparse 24 MiB pair, want 16 MiB", human(stats.Read))
	}

	// Restore into a fresh directory, as a new host would.
	into := t.TempDir()
	if _, err := r.Restore(ctx, m, into, nil); err != nil {
		t.Fatalf("restore: %v", err)
	}
	sameFile(t, disk, filepath.Join(into, "disk.ext4"))
	sameFile(t, cp, filepath.Join(into, "checkpoints", "v1.ext4"))

	// And it is still sparse: a 12 MiB file holding two chunks of data.
	if got := allocatedBytes(t, filepath.Join(into, "disk.ext4")); got > 9<<20 {
		t.Errorf("restored disk occupies %d bytes; the holes were filled in", got)
	}

	// The restored record drops what belonged to the old host.
	rec := RestoreRecord(m)
	if rec.NetIndex != 0 || rec.BootIP != "" || rec.Name != "dev" || rec.ID != "abc123" {
		t.Errorf("RestoreRecord = %+v", rec)
	}
}

func TestSecondBackupTransfersOnlyTheChange(t *testing.T) {
	srv := stubServer(t)
	r := testRepo(t, srv, "")
	ctx := context.Background()

	dir := t.TempDir()
	disk := filepath.Join(dir, "disk.ext4")
	sparseDisk(t, disk, 12<<20, map[int64]string{0: "a", 4 << 20: "b", 8 << 20: "c"})
	refs := []FileRef{{Name: "disk.ext4", Path: disk}}

	if _, stats, err := r.Save(ctx, sprite("dev"), refs, "first"); err != nil || stats.NewChunks != 3 {
		t.Fatalf("first save: %+v %v", stats, err)
	}

	// Change one chunk in place, as a guest writing a file does.
	f, _ := os.OpenFile(disk, os.O_WRONLY, 0)
	f.WriteAt([]byte("changed"), 4<<20)
	f.Close()

	_, stats, err := r.Save(ctx, sprite("dev"), refs, "second")
	if err != nil {
		t.Fatalf("second save: %v", err)
	}
	if stats.Chunks != 3 {
		t.Errorf("manifest references %d chunks, want 3", stats.Chunks)
	}
	if stats.NewChunks != 1 {
		t.Errorf("second backup uploaded %d chunks, want 1: it is not incremental", stats.NewChunks)
	}
	if stats.Uploaded > ChunkSize {
		t.Errorf("second backup sent %s, want at most one chunk", human(stats.Uploaded))
	}
}

func TestEncryptedRoundTripAndKeyMismatch(t *testing.T) {
	srv := stubServer(t)
	keyA := writeKeyFile(t, strings.Repeat("a1", 32))
	keyB := writeKeyFile(t, strings.Repeat("b2", 32))
	ctx := context.Background()

	r := testRepo(t, srv, keyA)
	if !r.Encrypted() {
		t.Fatal("repo should be encrypted")
	}
	dir := t.TempDir()
	disk := filepath.Join(dir, "disk.ext4")
	sparseDisk(t, disk, 8<<20, map[int64]string{0: "secret payload"})
	refs := []FileRef{{Name: "disk.ext4", Path: disk}}

	m, _, err := r.Save(ctx, sprite("dev"), refs, "suspend")
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	if !m.Encrypted {
		t.Error("manifest should record that it is encrypted")
	}
	// The plaintext is not in the bucket.
	for _, key := range srv.Keys() {
		if body, _ := srv.Get(key); bytes.Contains(body, []byte("secret payload")) {
			t.Fatalf("plaintext found in %s", key)
		}
	}
	into := t.TempDir()
	if _, err := r.Restore(ctx, m, into, nil); err != nil {
		t.Fatalf("restore: %v", err)
	}
	sameFile(t, disk, filepath.Join(into, "disk.ext4"))

	// The same plaintext chunked twice keeps the same ID, or dedup would be dead.
	before, _ := srv.Counts()
	if _, stats, err := r.Save(ctx, sprite("dev"), refs, "again"); err != nil || stats.NewChunks != 0 {
		t.Fatalf("re-save of identical data uploaded %d chunks (%v)", stats.NewChunks, err)
	}
	if after, _ := srv.Counts(); after-before > 2 { // the manifest and latest.json
		t.Errorf("re-save issued %d PUTs, want only the manifest pair", after-before)
	}

	// Another key on the same bucket is refused, rather than writing chunks nobody
	// can read.
	t.Setenv("AWS_ACCESS_KEY_ID", "GKtest")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "secret")
	if _, err := Open(ctx, Config{Endpoint: srv.URL, Bucket: "buck", Region: "home-cloud",
		KeyFile: keyB, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}); err == nil ||
		!strings.Contains(err.Error(), "different key") {
		t.Fatalf("want a key-mismatch error, got %v", err)
	}
	// So is no key at all.
	if _, err := Open(ctx, Config{Endpoint: srv.URL, Bucket: "buck", Region: "home-cloud",
		Log: slog.New(slog.NewTextHandler(io.Discard, nil))}); err == nil ||
		!strings.Contains(err.Error(), "--backup-key-file is required") {
		t.Fatalf("want an encryption-mismatch error, got %v", err)
	}
}

func TestPlaintextBucketRefusesAKey(t *testing.T) {
	srv := stubServer(t)
	ctx := context.Background()
	testRepo(t, srv, "") // initialises the repository unencrypted

	_, err := Open(ctx, Config{Endpoint: srv.URL, Bucket: "buck", Region: "home-cloud",
		KeyFile: writeKeyFile(t, strings.Repeat("c3", 32)),
		Log:     slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err == nil || !strings.Contains(err.Error(), "unencrypted") {
		t.Fatalf("want an encryption-mismatch error, got %v", err)
	}
}

func TestRestoreRejectsACorruptChunk(t *testing.T) {
	srv := stubServer(t)
	r := testRepo(t, srv, "")
	ctx := context.Background()

	dir := t.TempDir()
	disk := filepath.Join(dir, "disk.ext4")
	sparseDisk(t, disk, 4<<20, map[int64]string{0: "data"})
	m, _, err := r.Save(ctx, sprite("dev"), []FileRef{{Name: "disk.ext4", Path: disk}}, "suspend")
	if err != nil {
		t.Fatal(err)
	}

	id := m.Files["disk.ext4"].Chunks[0].ID
	body, ok := srv.Get(chunkKey(id))
	if !ok {
		t.Fatalf("chunk %s was never uploaded", id)
	}
	corrupt := append([]byte(nil), body...)
	corrupt[0] ^= 0xff
	srv.Put(chunkKey(id), corrupt)

	if _, err := r.Restore(ctx, m, t.TempDir(), nil); err == nil || !strings.Contains(err.Error(), "corrupt") {
		t.Fatalf("want a corruption error, got %v", err)
	}
}

func TestSaveWritesTheManifestLast(t *testing.T) {
	srv := stubServer(t)
	r := testRepo(t, srv, "")
	ctx := context.Background()

	dir := t.TempDir()
	disk := filepath.Join(dir, "disk.ext4")
	sparseDisk(t, disk, 8<<20, map[int64]string{0: "a", 4 << 20: "b"})

	// Refuse every chunk but the first, for good (a 5xx would be retried). An
	// interrupted backup must leave no manifest at all: chunks with nothing pointing
	// at them are collectable, a manifest pointing at chunks that are missing is not
	// restorable.
	var first string
	var mu sync.Mutex
	srv.SetFail(func(method, key string) int {
		if method != http.MethodPut || !strings.HasPrefix(key, "chunks/") {
			return 0
		}
		mu.Lock()
		defer mu.Unlock()
		if first == "" {
			first = key
		}
		if key != first {
			return http.StatusForbidden
		}
		return 0
	})
	if _, _, err := r.Save(ctx, sprite("dev"), []FileRef{{Name: "disk.ext4", Path: disk}}, "suspend"); err == nil {
		t.Fatal("save should have failed")
	}
	for _, key := range srv.Keys() {
		if strings.HasPrefix(key, "sprites/") {
			t.Errorf("a failed backup left %s behind", key)
		}
	}
}

func TestPruneCollectsUnreferencedChunks(t *testing.T) {
	srv := stubServer(t)
	r := testRepo(t, srv, "")
	ctx := context.Background()

	dir := t.TempDir()
	disk := filepath.Join(dir, "disk.ext4")
	sparseDisk(t, disk, 8<<20, map[int64]string{0: "a", 4 << 20: "b"})
	refs := []FileRef{{Name: "disk.ext4", Path: disk}}
	if _, _, err := r.Save(ctx, sprite("dev"), refs, "first"); err != nil {
		t.Fatal(err)
	}

	// An orphan: a chunk from a backup that died before writing its manifest.
	orphan := strings.Repeat("f0", 32)
	srv.Put(chunkKey(orphan), []byte("orphaned"))

	// Within the grace period nothing is collected, however unreferenced.
	if stats, err := r.Prune(ctx, PruneOptions{Grace: time.Hour}, nil); err != nil || stats.Chunks != 0 {
		t.Fatalf("prune inside the grace period removed %d chunks (%v)", stats.Chunks, err)
	}
	if _, ok := srv.Get(chunkKey(orphan)); !ok {
		t.Fatal("the orphan was deleted despite the grace period")
	}

	// Backdate it and it goes.
	srv.SetModTime(chunkKey(orphan), time.Now().Add(-2*time.Hour))
	stats, err := r.Prune(ctx, PruneOptions{Grace: time.Hour}, nil)
	if err != nil || stats.Chunks != 1 {
		t.Fatalf("prune removed %d chunks, want 1 (%v)", stats.Chunks, err)
	}
	if _, ok := srv.Get(chunkKey(orphan)); ok {
		t.Fatal("the orphan survived")
	}
	// The live backup is untouched and still restores.
	m, err := r.Latest(ctx, "abc123")
	if err != nil {
		t.Fatal(err)
	}
	into := t.TempDir()
	if _, err := r.Restore(ctx, m, into, nil); err != nil {
		t.Fatalf("restore after prune: %v", err)
	}
	sameFile(t, disk, filepath.Join(into, "disk.ext4"))
}

func TestPruneRetiresDeletedSprites(t *testing.T) {
	srv := stubServer(t)
	r := testRepo(t, srv, "")
	ctx := context.Background()

	dir := t.TempDir()
	disk := filepath.Join(dir, "disk.ext4")
	sparseDisk(t, disk, 4<<20, map[int64]string{0: "a"})
	if _, _, err := r.Save(ctx, sprite("dev"), []FileRef{{Name: "disk.ext4", Path: disk}}, "suspend"); err != nil {
		t.Fatal(err)
	}
	if err := r.MarkDeleted(ctx, "abc123", "dev"); err != nil {
		t.Fatal(err)
	}

	// A tombstone alone keeps the backup: losing the host and deleting a sprite
	// must not look the same.
	sprites, err := r.Sprites(ctx)
	if err != nil || len(sprites) != 1 || sprites[0].Deleted == nil || sprites[0].Latest == nil {
		t.Fatalf("sprites = %+v (%v)", sprites, err)
	}
	if stats, err := r.Prune(ctx, PruneOptions{Retention: time.Hour, Grace: time.Nanosecond}, nil); err != nil || stats.SpritesRetired != 0 {
		t.Fatalf("a freshly deleted sprite was retired (%+v %v)", stats, err)
	}

	// Backdate the tombstone past the retention and it is retired, chunks and all.
	tomb, _ := json.Marshal(Tombstone{SpriteID: "abc123", Name: "dev",
		DeletedAt: time.Now().Add(-48 * time.Hour)})
	srv.Put(deletedKey("abc123"), tomb)
	for _, key := range srv.Keys() {
		srv.SetModTime(key, time.Now().Add(-48*time.Hour))
	}
	stats, err := r.Prune(ctx, PruneOptions{Retention: time.Hour, Grace: time.Hour}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if stats.SpritesRetired != 1 || stats.Manifests != 1 || stats.Chunks != 1 {
		t.Fatalf("prune retired %+v, want 1 sprite, 1 manifest, 1 chunk", stats)
	}
	if left, err := r.Sprites(ctx); err != nil || len(left) != 0 {
		t.Fatalf("sprites left after retirement: %+v (%v)", left, err)
	}
}

func TestFindByNameAndStamps(t *testing.T) {
	srv := stubServer(t)
	r := testRepo(t, srv, "")
	ctx := context.Background()

	dir := t.TempDir()
	disk := filepath.Join(dir, "disk.ext4")
	sparseDisk(t, disk, 4<<20, map[int64]string{0: "a"})
	refs := []FileRef{{Name: "disk.ext4", Path: disk}}
	first, _, err := r.Save(ctx, sprite("dev"), refs, "first")
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(2 * time.Millisecond) // distinct stamps
	if _, _, err := r.Save(ctx, sprite("dev"), refs, "second"); err != nil {
		t.Fatal(err)
	}

	info, others, err := r.Find(ctx, "dev")
	if err != nil || len(others) != 0 || info.ID != "abc123" {
		t.Fatalf("find: %+v %+v %v", info, others, err)
	}
	if len(info.Stamps) != 2 {
		t.Fatalf("stamps = %v, want 2", info.Stamps)
	}
	if got, err := r.LoadManifest(ctx, "abc123", first.Stamp()); err != nil || got.Reason != "first" {
		t.Fatalf("load the older manifest: %+v %v", got, err)
	}
	if _, _, err := r.Find(ctx, "nope"); err == nil {
		t.Fatal("want an error for a sprite with no backup")
	}
}

func TestChunkerSkipsHolesAndZeroes(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "disk.ext4")
	// A hole, then a chunk that was written and then zeroed: both cost nothing.
	sparseDisk(t, path, 12<<20, map[int64]string{0: "data", 4 << 20: string(make([]byte, 4096))})

	var offs []int64
	read, err := walkChunks(context.Background(), path, nil, func(off int64, buf []byte) error {
		offs = append(offs, off)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(offs) != 1 || offs[0] != 0 {
		t.Fatalf("chunks at %v, want only offset 0", offs)
	}
	if read > 8<<20 {
		t.Errorf("read %d bytes of a 12 MiB sparse file", read)
	}
}

func TestLimiterPacesUploads(t *testing.T) {
	lim := newLimiter(1 << 20) // 1 MiB/s
	start := time.Now()
	// The bucket starts full, so the first MiB is free and the second costs a second.
	for range 2 {
		if err := lim.wait(context.Background(), 1<<20); err != nil {
			t.Fatal(err)
		}
	}
	if took := time.Since(start); took < 500*time.Millisecond {
		t.Errorf("2 MiB at 1 MiB/s took %s; the limiter is not limiting", took)
	}
	if err := newLimiter(0).wait(context.Background(), 1<<30); err != nil {
		t.Errorf("an unlimited limiter should never block or fail: %v", err)
	}
}

func TestManifestBytesAndKeys(t *testing.T) {
	m := &Manifest{CreatedAt: time.Date(2026, 9, 20, 22, 46, 1, 123456789, time.UTC),
		Files: map[string]File{"disk.ext4": {Chunks: []Chunk{{Len: 100}, {Len: 50}}}}}
	if m.Bytes() != 150 {
		t.Errorf("Bytes = %d", m.Bytes())
	}
	// No colons: the key needs no percent-encoding, which keeps the signed path and
	// the wire path identical.
	stamp := m.Stamp()
	if strings.ContainsAny(stamp, ":+/") {
		t.Errorf("stamp %q contains a character that has to be escaped", stamp)
	}
	if got := manifestKey("abc", stamp); got != "sprites/abc/manifests/20260920T224601.123456789Z.json" {
		t.Errorf("manifestKey = %q", got)
	}
	if got := chunkKey("ab12cd"); got != "chunks/ab/ab12cd" {
		t.Errorf("chunkKey = %q", got)
	}
}

func TestOpenReportsAnUnreachableBucket(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "GKtest")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "secret")
	_, err := Open(context.Background(), Config{Endpoint: "http://127.0.0.1:1", Bucket: "buck",
		Region: "home-cloud", Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err == nil {
		t.Fatal("want an error for an endpoint nothing is listening on")
	}
}

func TestOpenNeedsCredentials(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "")
	if _, err := Open(context.Background(), Config{Endpoint: "http://localhost:3900", Bucket: "buck"}); err == nil {
		t.Fatal("want an error when there are no credentials")
	}
}

// `spritesd backups prune` runs in another process from the daemon, whose chunk
// index would otherwise go on promising chunks the prune has collected. The next
// backup of identical content must put them back, or its manifest cannot restore.
func TestABackupAfterAPruneElsewhereStillRestores(t *testing.T) {
	srv := stubServer(t)
	daemon, cli := testRepo(t, srv, ""), testRepo(t, srv, "")
	ctx := context.Background()

	dir := t.TempDir()
	disk := filepath.Join(dir, "disk.ext4")
	sparseDisk(t, disk, 8<<20, map[int64]string{0: "same bytes", 4 << 20: "in both sprites"})
	refs := []FileRef{{Name: "disk.ext4", Path: disk}}

	old := sprite("old")
	if _, _, err := daemon.Save(ctx, old, refs, "suspend"); err != nil {
		t.Fatal(err)
	}
	// The sprite is deleted, outlives its retention, and is pruned from the CLI.
	tomb, _ := json.Marshal(Tombstone{SpriteID: old.ID, Name: old.Name, DeletedAt: time.Now().Add(-48 * time.Hour)})
	srv.Put(deletedKey(old.ID), tomb)
	if stats, err := cli.Prune(ctx, PruneOptions{Retention: time.Hour}, nil); err != nil || stats.Chunks != 2 {
		t.Fatalf("prune = %+v (%v), want both chunks collected", stats, err)
	}

	// A new sprite cloned from the same image has the same chunks.
	fresh := sprite("fresh")
	fresh.ID = "def456"
	m, stats, err := daemon.Save(ctx, fresh, refs, "suspend")
	if err != nil {
		t.Fatal(err)
	}
	if stats.NewChunks != 2 {
		t.Errorf("the daemon trusted a stale index: uploaded %d chunks, want 2", stats.NewChunks)
	}
	out := t.TempDir()
	if _, err := daemon.Restore(ctx, m, out, nil); err != nil {
		t.Fatalf("restore after a prune elsewhere: %v", err)
	}
	sameFile(t, disk, filepath.Join(out, "disk.ext4"))
}

func TestABackupStandsAsideWhileAPruneRuns(t *testing.T) {
	srv := stubServer(t)
	r := testRepo(t, srv, "")
	ctx := context.Background()
	disk := filepath.Join(t.TempDir(), "disk.ext4")
	sparseDisk(t, disk, 4<<20, map[int64]string{0: "a"})
	refs := []FileRef{{Name: "disk.ext4", Path: disk}}

	running, _ := json.Marshal(pruneMarker{StartedAt: time.Now().UTC()})
	srv.Put(pruneMarkerKey, running)
	if _, _, err := r.Save(ctx, sprite("dev"), refs, "suspend"); !errors.Is(err, ErrPruneRunning) {
		t.Fatalf("save during a prune = %v, want ErrPruneRunning", err)
	}

	// A prune that died leaves its marker open; it must not hold backups off for ever.
	dead, _ := json.Marshal(pruneMarker{StartedAt: time.Now().Add(-2 * pruneDeadline).UTC()})
	srv.Put(pruneMarkerKey, dead)
	if _, _, err := r.Save(ctx, sprite("dev"), refs, "suspend"); err != nil {
		t.Fatalf("save after a prune that died: %v", err)
	}

	// And a prune closes its own marker, even a dry run leaves none.
	if _, err := r.Prune(ctx, PruneOptions{}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := r.lastPrune(ctx); err != nil {
		t.Fatalf("marker after a finished prune: %v", err)
	}
}

// With a key, nothing that describes a sprite is readable in the bucket: the
// manifest carries its record, and that holds its environment.
func TestEncryptionCoversManifestsAndTombstones(t *testing.T) {
	srv := stubServer(t)
	r := testRepo(t, srv, writeKeyFile(t, strings.Repeat("ab", 32)))
	ctx := context.Background()
	disk := filepath.Join(t.TempDir(), "disk.ext4")
	sparseDisk(t, disk, 4<<20, map[int64]string{0: "a"})

	sp := sprite("payroll-db")
	sp.Environment = map[string]string{"API_TOKEN": "hunter2-hunter2"}
	if _, _, err := r.Save(ctx, sp, []FileRef{{Name: "disk.ext4", Path: disk}}, "suspend"); err != nil {
		t.Fatal(err)
	}
	if err := r.MarkDeleted(ctx, sp.ID, sp.Name); err != nil {
		t.Fatal(err)
	}
	for _, key := range srv.Keys() {
		body, _ := srv.Get(key)
		for _, leak := range []string{"payroll-db", "hunter2-hunter2"} {
			if bytes.Contains(body, []byte(leak)) {
				t.Errorf("%s holds %q in the clear", key, leak)
			}
		}
	}
	// It still reads back, and not from under another key's name.
	got, err := r.Latest(ctx, sp.ID)
	if err != nil || got.Sprite.Environment["API_TOKEN"] != "hunter2-hunter2" {
		t.Fatalf("latest = %+v (%v)", got, err)
	}
	sealed, _ := srv.Get(latestKey(sp.ID))
	srv.Put(latestKey("someone-else"), sealed)
	if _, err := r.Latest(ctx, "someone-else"); err == nil {
		t.Error("a manifest copied over another sprite's opened")
	}
}

func TestForgetDropsASpriteButNotItsChunks(t *testing.T) {
	srv := stubServer(t)
	r := testRepo(t, srv, "")
	ctx := context.Background()
	disk := filepath.Join(t.TempDir(), "disk.ext4")
	sparseDisk(t, disk, 4<<20, map[int64]string{0: "a"})
	sp := sprite("dev")
	for range 2 {
		if _, _, err := r.Save(ctx, sp, []FileRef{{Name: "disk.ext4", Path: disk}}, "suspend"); err != nil {
			t.Fatal(err)
		}
	}
	if n, err := r.Forget(ctx, sp.ID); err != nil || n != 2 {
		t.Fatalf("forget = %d manifests (%v), want 2", n, err)
	}
	for _, key := range srv.Keys() {
		if strings.HasPrefix(key, "sprites/") {
			t.Errorf("forget left %s", key)
		}
	}
	if left, _ := r.Sprites(ctx); len(left) != 0 {
		t.Errorf("sprites after forget: %+v", left)
	}
	// Whether anything else shares the chunk is prune's question, not forget's.
	if _, ok := srv.Get(chunkKey(r.cr.id(append([]byte("a"), make([]byte, 4<<20-1)...)))); !ok {
		t.Error("forget deleted a chunk")
	}
}

func TestRestoreRecordDropsCheckpointsWithNoImage(t *testing.T) {
	sp := sprite("dev")
	sp.Checkpoints = []store.Checkpoint{{ID: "v1"}, {ID: "v2"}}
	sp.Mounts = map[int]string{0: "v1"}
	m := &Manifest{Sprite: sp, Files: map[string]File{"disk.ext4": {}, "checkpoints/v2.ext4": {}}}
	rec := RestoreRecord(m)
	if len(rec.Checkpoints) != 1 || rec.Checkpoints[0].ID != "v2" {
		t.Errorf("checkpoints = %+v, want only v2", rec.Checkpoints)
	}
	if rec.NetIndex != 0 || rec.BootIP != "" || rec.Mounts != nil {
		t.Errorf("host-specific fields survived: %+v", rec)
	}
}
