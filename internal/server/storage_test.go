package server

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestBaseImageIsUsedDirectlyWithoutReflinks(t *testing.T) {
	dir := t.TempDir()
	base := filepath.Join(dir, "base.ext4")
	os.WriteFile(base, []byte("image"), 0o644)
	st := &storage{vmRoot: dir, baseImage: base, reflink: false}
	got, err := st.base(context.Background())
	if err != nil || got != base {
		t.Fatalf("base() = %q, %v; want the image itself, since a mirror buys nothing here", got, err)
	}
}

// With reflinks available, an image on the same filesystem needs no mirror, and
// the mirror logic is exercised by pointing the comparison at a different device.
func TestBaseImageMirrorTracksTheSource(t *testing.T) {
	vmRoot := t.TempDir()
	base := filepath.Join(t.TempDir(), "base.ext4")
	os.WriteFile(base, []byte("v1"), 0o644)
	st := &storage{vmRoot: vmRoot, baseImage: base, reflink: true}

	// Same filesystem (both under the test's temp root): used in place.
	if got, _ := st.base(context.Background()); got != base {
		t.Fatalf("same-filesystem base should be used in place, got %q", got)
	}

	// /dev/shm is a different filesystem from the temp dir on any normal Linux box.
	shm, err := os.MkdirTemp("/dev/shm", "ms-storage-test-")
	if err != nil {
		t.Skipf("no second filesystem to test against: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(shm) })
	if a, _ := device(shm); func() bool { b, _ := device(vmRoot); return a == b }() {
		t.Skip("/dev/shm is on the same device as the temp dir")
	}
	far := filepath.Join(shm, "base.ext4")
	os.WriteFile(far, []byte("v1"), 0o644)
	st.baseImage = far

	mirror, err := st.base(context.Background())
	if err != nil || mirror != filepath.Join(vmRoot, localBaseName) {
		t.Fatalf("base() = %q, %v; want a mirror inside the sprite volume", mirror, err)
	}
	if b, _ := os.ReadFile(mirror); string(b) != "v1" {
		t.Fatalf("mirror content %q", b)
	}
	first, _ := os.Stat(mirror)

	// Unchanged source: the mirror is reused, not recopied.
	st.base(context.Background())
	if again, _ := os.Stat(mirror); !again.ModTime().Equal(first.ModTime()) {
		t.Fatal("mirror was rewritten although the base image had not changed")
	}

	// A rebuilt base image (new content, new mtime) refreshes the mirror.
	os.WriteFile(far, []byte("v2-rebuilt"), 0o644)
	os.Chtimes(far, time.Now().Add(time.Hour), time.Now().Add(time.Hour))
	st.base(context.Background())
	if b, _ := os.ReadFile(mirror); string(b) != "v2-rebuilt" {
		t.Fatalf("mirror not refreshed after the base image changed: %q", b)
	}
}

func TestProbeReflinkReportsWhatTheFilesystemDoes(t *testing.T) {
	// Not asserting a value: it depends on the filesystem under the temp dir. It must
	// simply not leave probe files behind, whichever way it answers.
	dir := t.TempDir()
	t.Logf("reflink support under %s: %v", dir, probeReflink(dir))
	if left, _ := filepath.Glob(filepath.Join(dir, ".reflink-probe-*")); len(left) != 0 {
		t.Fatalf("probe files left behind: %v", left)
	}
}
