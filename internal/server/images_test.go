package server

import (
	"archive/tar"
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jhgaylor/mini-sprites/internal/ociimage"
	"github.com/jhgaylor/mini-sprites/internal/vmm"
)

// fakePodman is a script standing in for podman: it serves one image, whose
// exported filesystem is rootfs, and logs every call to calls.
const fakePodman = `#!/bin/sh
state=$(dirname "$0")
echo "$*" >> "$state/calls"
id=` + fakeImageID + `
case "$1" in
image)
  ref=$4
  if [ "$ref" = "$id" ] || grep -qxF -- "$ref" "$state/present" 2>/dev/null; then
    printf '[{"Id":"%s","Digest":"sha256:%s","RepoDigests":["docker.io/library/alpine@sha256:%s"],"Os":"linux","Architecture":"amd64","Size":4096,"Config":{"Env":["PATH=/usr/bin:/bin","FOO=bar"]}}]' $id $id $id
  else
    echo "Error: $ref: image not known" >&2; exit 125
  fi ;;
pull)
  ref=$4
  case "$ref" in *nosuch*) echo "Error: initializing source $ref: reading manifest: manifest unknown" >&2; exit 125 ;; esac
  echo "Copying blob 123 done" >&2
  echo "$ref" >> "$state/present"
  echo $id ;;
create) echo ctr ;;
export) cat "$state/rootfs.tar" ;;
rm) ;;
rmi) grep -vxF -- "$3" "$state/present" > "$state/p2"; mv "$state/p2" "$state/present" ;;
esac
`

const fakeImageID = "5c4f1b7e3a2d9c8b6a5f4e3d2c1b0a9f8e7d6c5b4a3f2e1d0c9b8a7f6e5d4c3b"

func newImageServer(t *testing.T) (*Server, http.Handler, string) {
	t.Helper()
	mkfs, err := findMkfs()
	if err != nil {
		t.Skip(err)
	}
	s, h := newOperatorServer(t, Options{})
	state := t.TempDir()
	bin := filepath.Join(state, "podman")
	if err := os.WriteFile(bin, []byte(fakePodman), 0o755); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, f := range []struct{ name, body string }{{"bin/sh", "#!"}, {"etc/passwd", "root:x:0:0:root:/root:/bin/sh\n"}} {
		tw.WriteHeader(&tar.Header{Name: f.name, Mode: 0o755, Size: int64(len(f.body)), Typeflag: tar.TypeReg})
		tw.Write([]byte(f.body))
	}
	tw.Close()
	os.WriteFile(filepath.Join(state, "rootfs.tar"), buf.Bytes(), 0o644)
	// mke2fs reads tar input only from 1.47.1 on.
	var probeTar bytes.Buffer
	tw = tar.NewWriter(&probeTar)
	tw.WriteHeader(&tar.Header{Name: "f", Mode: 0o644, Typeflag: tar.TypeReg})
	tw.Close()
	os.WriteFile(filepath.Join(state, "probe.tar"), probeTar.Bytes(), 0o644)
	probe := filepath.Join(state, "probe.ext4")
	os.WriteFile(probe, nil, 0o644)
	os.Truncate(probe, 8<<20)
	if out, err := exec.Command(mkfs, "-q", "-F", "-d", filepath.Join(state, "probe.tar"), probe).CombinedOutput(); err != nil {
		t.Skipf("this mke2fs cannot build from a tar: %s", out)
	}
	s.images.podman = ociimage.Podman{Bin: bin}
	s.images.diskSize = func() int64 { return 32 << 20 }
	return s, h, state
}

func calls(t *testing.T, state, verb string) []string {
	b, _ := os.ReadFile(filepath.Join(state, "calls"))
	var out []string
	for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if strings.HasPrefix(l, verb+" ") {
			out = append(out, l)
		}
	}
	return out
}

func TestCreateFromImage(t *testing.T) {
	s, h, state := newImageServer(t)

	// A guest cannot make the host pull.
	status(t, apiCall(t, h, "POST", "/v1/sprites", `{"name":"lobby"}`), http.StatusCreated)
	status(t, apiCall(t, h, "POST", "/v1/sprites/lobby/policy/spawn", `{"enabled":true}`), http.StatusNoContent)
	body := status(t, fromInside(t, s, "lobby", "POST", "/v1/sprites", `{"name":"kid","from":{"image":"alpine:3.20"}}`), http.StatusNotFound)
	if !strings.Contains(string(body), "image_not_cached") {
		t.Fatalf("guest create: %s", body)
	}
	if p := calls(t, state, "pull"); len(p) != 0 {
		t.Fatalf("a guest request pulled: %q", p)
	}

	// Bad references, ambiguous sources and taken names are refused before anything runs.
	status(t, apiCall(t, h, "POST", "/v1/sprites", `{"name":"lobby","from":{"image":"alpine"}}`), http.StatusBadRequest)
	for _, from := range []string{`{"image":"oci:/etc"}`, `{"image":"alpine","sprite":"lobby"}`} {
		status(t, apiCall(t, h, "POST", "/v1/sprites", `{"name":"bad","from":`+from+`}`), http.StatusBadRequest)
	}
	if p := calls(t, state, "pull"); len(p) != 0 {
		t.Fatalf("a refused request pulled: %q", p)
	}
	body = status(t, apiCall(t, h, "POST", "/v1/sprites", `{"name":"bad","from":{"image":"nosuch/thing"}}`), http.StatusBadGateway)
	if !strings.Contains(string(body), "manifest unknown") {
		t.Errorf("failed pull: %s", body)
	}
	if _, err := s.store.Get("bad"); err == nil {
		t.Error("a failed create left a sprite behind")
	}

	// The API pulls on a miss and builds the disk.
	body = status(t, apiCall(t, h, "POST", "/v1/sprites", `{"name":"a1","from":{"image":"alpine:3.20"}}`), http.StatusCreated)
	var got struct {
		SourceImage string `json:"source_image"`
	}
	json.Unmarshal(body, &got)
	if got.SourceImage != "docker.io/library/alpine@sha256:"+fakeImageID {
		t.Errorf("source_image = %q", got.SourceImage)
	}
	if p := calls(t, state, "pull"); len(p) != 2 || p[1] != "pull --policy=always -- docker.io/library/alpine:3.20" {
		t.Fatalf("pulls = %q", p)
	}
	// The pull added the image to podman's storage, so the build took it out again.
	if r := calls(t, state, "rmi"); len(r) != 1 {
		t.Errorf("rmi calls = %q", r)
	}
	a1, _ := s.store.Get("a1")
	disk := filepath.Join(s.store.Dir(a1.ID), vmm.DiskFile)
	if fi, err := os.Stat(disk); err != nil || fi.Size() != 32<<20 {
		t.Fatalf("disk: %v %v", fi, err)
	}
	if debugfs, err := exec.LookPath("debugfs"); err == nil || fileExists("/usr/sbin/debugfs") {
		if err != nil {
			debugfs = "/usr/sbin/debugfs"
		}
		out, _ := exec.Command(debugfs, "-R", "cat /etc/passwd", disk).Output()
		if !strings.Contains(string(out), "sprite:x:1000:1000::/home/sprite:/bin/sh") {
			t.Errorf("passwd on the disk: %q", out)
		}
		out, _ = exec.Command(debugfs, "-R", "cat /.sprite/image.json", disk).Output()
		if !strings.Contains(string(out), "FOO=bar") {
			t.Errorf("image.json on the disk: %q", out)
		}
	}

	// A second create, and one from inside, clone the cached disk without pulling.
	status(t, apiCall(t, h, "POST", "/v1/sprites", `{"name":"a2","from":{"image":"docker.io/library/alpine:3.20"}}`), http.StatusCreated)
	status(t, fromInside(t, s, "lobby", "POST", "/v1/sprites", `{"name":"kid","from":{"image":"alpine:3.20"}}`), http.StatusCreated)
	// So does a reference pinned to the digest it was pulled at.
	status(t, apiCall(t, h, "POST", "/v1/sprites", `{"name":"a3","from":{"image":"alpine@sha256:`+fakeImageID+`"}}`), http.StatusCreated)
	if p := calls(t, state, "pull"); len(p) != 2 {
		t.Fatalf("pulls = %q", p)
	}

	// The operator socket lists and removes the cache.
	op := s.StatusHandler("x")
	rec := httptest.NewRecorder()
	op.ServeHTTP(rec, httptest.NewRequest("GET", "/images", nil))
	var imgs []CachedImage
	json.Unmarshal(rec.Body.Bytes(), &imgs)
	if len(imgs) != 1 || imgs[0].ID != fakeImageID || imgs[0].Account.Shell != "/bin/sh" || imgs[0].LastUsedAt == nil {
		t.Fatalf("images = %+v", imgs)
	}
	if st := s.status(t.Context(), s.started, "x"); st.Host.Images.Count != 1 {
		t.Errorf("status images = %+v", st.Host.Images)
	}
	rec = httptest.NewRecorder()
	op.ServeHTTP(rec, httptest.NewRequest("DELETE", "/images?key=alpine:3.20", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("rm: %d %s", rec.Code, rec.Body)
	}
	if len(s.images.list()) != 0 || len(imageDisks(filepath.Join(s.opts.DataDir, "vm"))) != 0 {
		t.Fatal("rm left the image")
	}
	if _, err := os.Stat(disk); err != nil {
		t.Fatal("removing the image took a sprite's disk with it")
	}
	status(t, fromInside(t, s, "lobby", "POST", "/v1/sprites", `{"name":"kid2","from":{"image":"alpine:3.20"}}`), http.StatusNotFound)

	// An explicit pull streams progress and ends with the record.
	rec = httptest.NewRecorder()
	op.ServeHTTP(rec, httptest.NewRequest("POST", "/images/pull?ref=alpine:3.20", nil))
	if out := rec.Body.String(); !strings.Contains(out, "Copying blob") || !strings.Contains(out, "\nok {") {
		t.Fatalf("pull output: %s", out)
	}
	// A restarted daemon finds the cache again.
	if c := newImageCache(filepath.Join(s.opts.DataDir, "vm"), "", s.life.disk.admit, s.log); len(c.images) != 1 {
		t.Fatalf("reloaded cache holds %d images", len(c.images))
	}
}

func fileExists(p string) bool { _, err := os.Stat(p); return err == nil }

func TestMovedTagReplacesTheOldDisk(t *testing.T) {
	dir := t.TempDir()
	c := &imageCache{dir: dir, log: slog.New(slog.NewTextHandler(io.Discard, nil)), images: map[string]*CachedImage{}}
	old := strings.Repeat("a", 64)
	shared := strings.Repeat("b", 64)
	for _, img := range []*CachedImage{
		{ID: old, Refs: []string{"docker.io/library/node:22"}},
		{ID: shared, Refs: []string{"docker.io/library/node:22-bookworm", "docker.io/library/node:lts"}},
	} {
		c.images[img.ID] = img
		os.WriteFile(c.diskPath(img.ID), []byte("disk"), 0o644)
		c.save(img)
	}
	fresh := &CachedImage{ID: strings.Repeat("c", 64)}
	c.images[fresh.ID] = fresh
	c.pointRef("docker.io/library/node:22", fresh)
	c.pointRef("docker.io/library/node:lts", fresh)
	if _, err := os.Stat(c.diskPath(old)); !os.IsNotExist(err) || c.images[old] != nil {
		t.Error("the disk no reference points at any more should be gone")
	}
	if img := c.images[shared]; img == nil || len(img.Refs) != 1 || img.Refs[0] != "docker.io/library/node:22-bookworm" {
		t.Errorf("a disk still referenced must stay: %+v", img)
	}
	if len(fresh.Refs) != 2 {
		t.Errorf("fresh refs = %q", fresh.Refs)
	}
	r, _ := ociimage.ParseRef("node:22")
	if c.find(r) != fresh {
		t.Error("node:22 should resolve to the fresh disk")
	}
}
