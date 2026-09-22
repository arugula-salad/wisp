package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jhgaylor/mini-sprites/internal/ociimage"
)

// A sprite can start from a container image instead of the base image
// (`"from": {"image": "docker.io/library/node:22"}`, our extension). spritesd
// pulls the image with rootless podman, flattens it into an ext4 disk with the
// sprite account added (ociimage.Rewrite), and keeps that disk, keyed by image
// ID, in a cache on the sprite volume. Every later create from the same image
// clones the cached disk exactly as a checkpoint clone does: a reflink where
// the volume supports it, a sparse copy otherwise.
//
// A create that misses the cache pulls; a pull can take minutes, so it runs
// detached from the request, and a client that gives up can retry and find it
// done. From inside a sprite only cached images are accepted, so a guest can
// never make the host fetch anything. `spritesd images pull|list|rm` manages
// the cache over the operator socket.

// imagesDirName is the cache under vm/. The store ignores directories without
// a sprite.json, and on the sprite volume the disks can be reflinked.
const imagesDirName = ".images"

// buildTimeout bounds one pull-and-flatten.
const buildTimeout = 30 * time.Minute

// defaultImageDisk is the disk size when the base image cannot be read.
const defaultImageDisk = 20 << 30

// CachedImage is one flattened image; <id>.ext4 is the disk and <id>.json this record.
type CachedImage struct {
	// ID is podman's image ID (the digest of the image config), the cache key.
	ID string `json:"id"`
	// Refs are the canonical references that resolve to this disk. A reference
	// moves to a newer disk when a pull finds the tag has moved on.
	Refs []string `json:"refs"`
	// Digest is the manifest digest the registry served; RepoDigests every
	// repository@digest podman knows the image by.
	Digest       string   `json:"digest,omitempty"`
	RepoDigests  []string `json:"repo_digests,omitempty"`
	Architecture string   `json:"architecture,omitempty"`
	OS           string   `json:"os,omitempty"`
	// Env is the image's environment, which the agent gives every command.
	Env []string `json:"env,omitempty"`
	// Account is what the rewrite found and added: shell, sudo, the sprite user.
	Account    ociimage.Result `json:"account"`
	ImageBytes int64           `json:"image_bytes"`
	BuiltAt    time.Time       `json:"built_at"`
	PulledAt   time.Time       `json:"pulled_at"`
	LastUsedAt *time.Time      `json:"last_used_at,omitempty"`
	// Disk figures are read from the file when listed, not stored.
	DiskApparent int64 `json:"disk_apparent_bytes"`
	DiskBytes    int64 `json:"disk_bytes"`
}

type imageCache struct {
	dir      string
	podman   ociimage.Podman
	log      *slog.Logger
	diskSize func() int64
	admit    func(what string, need int64) error

	// mu is held for reading while a disk is cloned from, and for writing to
	// change the index or delete a disk.
	mu     sync.RWMutex
	images map[string]*CachedImage // by ID

	flightMu sync.Mutex
	flights  map[string]*imageFlight // pulls in progress, by canonical ref
	builds   chan struct{}           // one pull-and-flatten at a time
}

type imageFlight struct {
	done chan struct{}
	img  CachedImage
	err  error
}

var (
	errImageNotCached = errors.New("image not cached")
	errImageNotFound  = errors.New("no such image in the cache")
)

func newImageCache(vmRoot, baseImage string, admit func(string, int64) error, log *slog.Logger) *imageCache {
	c := &imageCache{dir: filepath.Join(vmRoot, imagesDirName), log: log, admit: admit,
		images: map[string]*CachedImage{}, flights: map[string]*imageFlight{}, builds: make(chan struct{}, 1)}
	c.diskSize = func() int64 {
		if fi, err := os.Stat(baseImage); err == nil && fi.Size() > 0 {
			return fi.Size() // the same size as a sprite made from the base image
		}
		return defaultImageDisk
	}
	c.load()
	return c
}

// load reads the index from disk and sweeps what an interrupted build left.
func (c *imageCache) load() {
	entries, err := os.ReadDir(c.dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, ".tmp-") {
			os.Remove(filepath.Join(c.dir, name))
			continue
		}
		if !strings.HasSuffix(name, ".json") {
			continue
		}
		img, err := readImageRecord(filepath.Join(c.dir, name))
		if err != nil {
			c.log.Warn("unreadable image cache record", "file", name, "err", err)
			continue
		}
		if _, err := os.Stat(c.diskPath(img.ID)); err != nil {
			continue
		}
		c.images[img.ID] = &img
	}
}

func readImageRecord(path string) (CachedImage, error) {
	var img CachedImage
	b, err := os.ReadFile(path)
	if err == nil {
		err = json.Unmarshal(b, &img)
	}
	if err == nil && !validImageID(img.ID) {
		err = fmt.Errorf("bad image id %q", img.ID)
	}
	return img, err
}

func validImageID(id string) bool {
	if len(id) != 64 {
		return false
	}
	for _, r := range id {
		if !strings.ContainsRune("0123456789abcdef", r) {
			return false
		}
	}
	return true
}

func (c *imageCache) diskPath(id string) string { return filepath.Join(c.dir, id+".ext4") }

func (c *imageCache) save(img *CachedImage) error {
	b, _ := json.MarshalIndent(img, "", "  ")
	tmp := filepath.Join(c.dir, ".tmp-"+img.ID+".json")
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(c.dir, img.ID+".json"))
}

// find returns the cached image ref resolves to. The caller holds mu.
func (c *imageCache) find(ref ociimage.Ref) *CachedImage {
	want := ref.String()
	for _, img := range c.images {
		if slices.Contains(img.Refs, want) {
			return img
		}
	}
	if ref.Digest == "" {
		return nil
	}
	// A pinned reference matches an image pulled by tag with that digest.
	for _, img := range c.images {
		if img.Digest == ref.Digest && slices.ContainsFunc(img.Refs, func(r string) bool { return sameRepo(r, ref) }) ||
			slices.Contains(img.RepoDigests, ref.Name()+"@"+ref.Digest) {
			return img
		}
	}
	return nil
}

func sameRepo(s string, ref ociimage.Ref) bool {
	r, err := ociimage.ParseRef(s)
	return err == nil && r.Name() == ref.Name()
}

// acquire returns the cached disk for ref, pulling it first when pull is set,
// and holds it against deletion until release.
func (c *imageCache) acquire(ctx context.Context, ref ociimage.Ref, pull bool) (disk string, img CachedImage, release func(), err error) {
	for attempt := 0; ; attempt++ {
		c.mu.Lock()
		found := c.find(ref)
		if found != nil {
			now := time.Now().UTC()
			found.LastUsedAt = &now
			c.save(found) // advisory; a failed write loses nothing
			img = *found
		}
		c.mu.Unlock()
		if found != nil {
			c.mu.RLock()
			if c.images[img.ID] == nil { // removed in the window between the locks
				c.mu.RUnlock()
				continue
			}
			return c.diskPath(img.ID), img, c.mu.RUnlock, nil
		}
		if !pull || attempt > 0 {
			return "", img, nil, errImageNotCached
		}
		if _, err := c.pull(ctx, ref, nil); err != nil {
			return "", img, nil, err
		}
	}
}

// pull fetches ref and builds its disk, or joins a pull of the same reference
// already running. The work is detached from ctx: a caller that stops waiting
// leaves it to finish, and the next create finds the disk ready.
func (c *imageCache) pull(ctx context.Context, ref ociimage.Ref, progress io.Writer) (CachedImage, error) {
	key := ref.String()
	c.flightMu.Lock()
	f := c.flights[key]
	if f == nil {
		f = &imageFlight{done: make(chan struct{})}
		c.flights[key] = f
		go func() {
			bctx, cancel := context.WithTimeout(context.Background(), buildTimeout)
			defer cancel()
			f.img, f.err = c.build(bctx, ref, progress)
			c.flightMu.Lock()
			delete(c.flights, key)
			c.flightMu.Unlock()
			close(f.done)
		}()
	} else if progress != nil {
		fmt.Fprintf(progress, "joining a pull of %s already in progress\n", key)
	}
	c.flightMu.Unlock()
	select {
	case <-f.done:
		return f.img, f.err
	case <-ctx.Done():
		return CachedImage{}, ctx.Err()
	}
}

// build pulls ref and flattens it into a cached disk.
func (c *imageCache) build(ctx context.Context, ref ociimage.Ref, progress io.Writer) (CachedImage, error) {
	say := func(format string, args ...any) {
		if progress != nil {
			fmt.Fprintf(progress, format+"\n", args...)
		}
	}
	if progress == nil {
		progress = io.Discard
	}
	select {
	case c.builds <- struct{}{}:
	case <-ctx.Done():
		return CachedImage{}, ctx.Err()
	}
	defer func() { <-c.builds }()
	start := time.Now()
	if err := os.MkdirAll(c.dir, 0o755); err != nil {
		return CachedImage{}, err
	}

	// A reference podman held before is the operator's; one this pull added is
	// ours to drop again once the disk is built.
	_, errBefore := c.podman.Inspect(ctx, ref.String())
	hadRef := errBefore == nil
	say("pulling %s", ref)
	c.log.Info("pulling image", "ref", ref.String())
	id, err := c.podman.Pull(ctx, ref, progress)
	if err != nil {
		return CachedImage{}, err
	}
	if !hadRef && !ref.Local() {
		defer func() {
			if err := c.podman.Untag(context.WithoutCancel(ctx), ref); err != nil {
				c.log.Warn("could not drop the pulled image from podman storage", "ref", ref.String(), "err", err)
			}
		}()
	}
	info, err := c.podman.Inspect(ctx, id)
	if err != nil {
		return CachedImage{}, err
	}
	if !validImageID(info.ID) {
		return CachedImage{}, fmt.Errorf("podman reported an unexpected image id %q", info.ID)
	}
	if info.Os != "" && info.Os != "linux" {
		return CachedImage{}, fmt.Errorf("%s is a %s image; sprites run linux", ref, info.Os)
	}

	c.mu.Lock()
	if have := c.images[info.ID]; have != nil {
		// Already flattened (under this reference or another): point ref at it.
		c.pointRef(ref.String(), have)
		have.PulledAt = time.Now().UTC()
		err := c.save(have)
		img := *have
		c.mu.Unlock()
		say("%s is image %.12s, already in the cache", ref, info.ID)
		return img, err
	}
	c.mu.Unlock()

	// The temporary tar and the disk's blocks each come to about the image's size.
	if err := c.admit("an image disk", 2*info.Size); err != nil {
		return CachedImage{}, err
	}
	img := CachedImage{ID: info.ID, Refs: []string{ref.String()}, Digest: info.Digest, RepoDigests: info.RepoDigests,
		Architecture: info.Architecture, OS: info.Os, Env: info.Config.Env, ImageBytes: info.Size}
	meta, _ := json.MarshalIndent(map[string]any{"ref": ref.String(), "id": info.ID, "digest": info.Digest, "env": info.Config.Env}, "", "  ")

	say("flattening image %.12s into a sprite disk", info.ID)
	tarPath := filepath.Join(c.dir, ".tmp-"+info.ID+".tar")
	defer os.Remove(tarPath)
	if img.Account, err = c.flatten(ctx, info.ID, tarPath, meta); err != nil {
		return CachedImage{}, err
	}
	diskTmp := filepath.Join(c.dir, ".tmp-"+info.ID+".ext4")
	defer os.Remove(diskTmp)
	if err := makeExt4(ctx, tarPath, diskTmp, c.diskSize()); err != nil {
		return CachedImage{}, err
	}
	os.Remove(tarPath)
	now := time.Now().UTC()
	img.BuiltAt, img.PulledAt = now, now

	c.mu.Lock()
	defer c.mu.Unlock()
	if err := os.Rename(diskTmp, c.diskPath(img.ID)); err != nil {
		return CachedImage{}, err
	}
	if err := c.save(&img); err != nil {
		os.Remove(c.diskPath(img.ID))
		return CachedImage{}, err
	}
	c.pointRef(ref.String(), &img)
	c.images[img.ID] = &img
	shell := img.Account.Shell
	if shell == "" {
		shell = "none"
	}
	c.log.Info("image cached", "ref", ref.String(), "id", img.ID[:12], "shell", shell, "sudo", img.Account.Sudo,
		"uid", img.Account.UID, "took", time.Since(start).Round(time.Millisecond))
	say("cached %s as image %.12s in %s (shell: %s, own sudo: %t)", ref, img.ID, time.Since(start).Round(time.Second), shell, img.Account.Sudo)
	return img, nil
}

// pointRef moves ref onto img. An older disk left with no reference at all is
// deleted: nothing can ask for it any more, and sprites made from it are
// copies that do not need it. The caller holds mu for writing.
func (c *imageCache) pointRef(ref string, img *CachedImage) {
	for _, other := range c.images {
		if other == img || !slices.Contains(other.Refs, ref) {
			continue
		}
		other.Refs = slices.DeleteFunc(other.Refs, func(r string) bool { return r == ref })
		if len(other.Refs) == 0 {
			c.log.Info("image replaced by a newer pull; removing the old disk", "ref", ref, "old", other.ID[:12])
			c.deleteLocked(other.ID)
		} else {
			c.save(other)
		}
	}
	if !slices.Contains(img.Refs, ref) {
		img.Refs = append(img.Refs, ref)
	}
}

// flatten exports the image and rewrites it into a sprite-shaped tar at dst.
func (c *imageCache) flatten(ctx context.Context, id, dst string, meta []byte) (ociimage.Result, error) {
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return ociimage.Result{}, err
	}
	defer out.Close()
	pr, pw := io.Pipe()
	exported := make(chan error, 1)
	go func() {
		err := c.podman.Export(ctx, id, pw)
		pw.CloseWithError(err)
		exported <- err
	}()
	res, err := ociimage.Rewrite(pr, out, meta)
	// Unblocks the export if the rewrite stopped reading early.
	pr.CloseWithError(errors.New("rewrite stopped"))
	if xerr := <-exported; xerr != nil {
		return res, xerr
	}
	if err != nil {
		return res, err
	}
	return res, out.Close()
}

// makeExt4 builds an ext4 filesystem of size bytes at dst from the tar at src,
// the way scripts/build-image.sh builds the base image, except that mke2fs
// reads the tar itself (e2fsprogs >= 1.47.1), so the ownership recorded in it
// comes out right without a user namespace.
func makeExt4(ctx context.Context, src, dst string, size int64) error {
	mkfs, err := findMkfs()
	if err != nil {
		return err
	}
	os.Remove(dst)
	f, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	err = f.Truncate(size)
	f.Close()
	if err != nil {
		return err
	}
	out, err := exec.CommandContext(ctx, mkfs, "-q", "-F", "-L", "sprite", "-d", src,
		"-E", "lazy_itable_init=1,lazy_journal_init=1,root_owner=0:0", dst).CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if strings.Contains(msg, "tar") || strings.Contains(msg, "Not a directory") {
			msg += " (building a disk from an image needs e2fsprogs 1.47.1 or newer, whose mke2fs reads tar files)"
		}
		return fmt.Errorf("mkfs.ext4: %v: %s", err, msg)
	}
	return nil
}

func findMkfs() (string, error) {
	if p, err := exec.LookPath("mkfs.ext4"); err == nil {
		return p, nil
	}
	for _, p := range []string{"/usr/sbin/mkfs.ext4", "/sbin/mkfs.ext4"} {
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
	}
	return "", errors.New("mkfs.ext4 not found (install e2fsprogs)")
}

// list returns the cache, newest first, with disk figures read from the files.
func (c *imageCache) list() []CachedImage {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return listImages(c.dir, c.images)
}

func listImages(dir string, images map[string]*CachedImage) []CachedImage {
	out := []CachedImage{}
	for _, img := range images {
		out = append(out, withDiskFigures(dir, *img))
	}
	sort.Slice(out, func(a, b int) bool { return out[a].BuiltAt.After(out[b].BuiltAt) })
	return out
}

func withDiskFigures(dir string, img CachedImage) CachedImage {
	disk := filepath.Join(dir, img.ID+".ext4")
	if fi, err := os.Stat(disk); err == nil {
		img.DiskApparent = fi.Size()
	}
	img.DiskBytes = allocated(disk)
	return img
}

// OfflineImages reads the cache with no daemon running.
func OfflineImages(dataDir string) []CachedImage {
	dir := filepath.Join(dataDir, "vm", imagesDirName)
	images := map[string]*CachedImage{}
	files, _ := filepath.Glob(filepath.Join(dir, "*.json"))
	for _, f := range files {
		if img, err := readImageRecord(f); err == nil {
			images[img.ID] = &img
		}
	}
	return listImages(dir, images)
}

// remove deletes the disk key names: a reference, an image ID, or an ID
// prefix of at least 12 characters. Sprites made from it are unaffected.
func (c *imageCache) remove(key string) (CachedImage, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	var match []*CachedImage
	if ref, err := ociimage.ParseRef(key); err == nil {
		if img := c.find(ref); img != nil {
			match = append(match, img)
		}
	}
	if len(match) == 0 && len(key) >= 12 {
		for id, img := range c.images {
			if strings.HasPrefix(id, strings.TrimPrefix(key, "sha256:")) {
				match = append(match, img)
			}
		}
	}
	switch len(match) {
	case 0:
		return CachedImage{}, errImageNotFound
	case 1:
	default:
		return CachedImage{}, fmt.Errorf("%q matches %d images; give more of the id", key, len(match))
	}
	img := *match[0]
	return img, c.deleteLocked(img.ID)
}

func (c *imageCache) deleteLocked(id string) error {
	delete(c.images, id)
	err := os.Remove(c.diskPath(id))
	if errors.Is(err, os.ErrNotExist) {
		err = nil
	}
	os.Remove(filepath.Join(c.dir, id+".json"))
	return err
}

// imageDisks lists the cached disks, for the disk accounting in status.go.
func imageDisks(vmRoot string) []string {
	files, _ := filepath.Glob(filepath.Join(vmRoot, imagesDirName, "*.ext4"))
	return files
}

// imageSource resolves from.image for a create. From inside a sprite
// (parent set) only a cached image is accepted.
func (s *Server) imageSource(ctx context.Context, raw string, parent bool) (disk string, img CachedImage, ref ociimage.Ref, release func(), _ *createError) {
	ref, err := ociimage.ParseRef(raw)
	if err != nil {
		return "", img, ref, nil, &createError{http.StatusBadRequest, "bad_request", "from.image: " + err.Error()}
	}
	disk, img, release, err = s.images.acquire(ctx, ref, !parent)
	switch {
	case err == nil:
		return disk, img, ref, release, nil
	case errors.Is(err, errImageNotCached):
		return "", img, ref, nil, &createError{http.StatusNotFound, "image_not_cached",
			fmt.Sprintf("image %s is not in this host's image cache; from inside a sprite only cached images can be used (the operator adds them with `spritesd images pull`)", ref)}
	case errors.Is(err, errNoRoom):
		return "", img, ref, nil, &createError{http.StatusInsufficientStorage, "insufficient_storage", err.Error()}
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return "", img, ref, nil, &createError{http.StatusGatewayTimeout, "image_pull_pending",
			fmt.Sprintf("still pulling %s; the pull continues in the background, retry the create shortly", ref)}
	}
	return "", img, ref, nil, &createError{http.StatusBadGateway, "image_pull_failed", err.Error()}
}

// Operator socket routes (status.go mounts them): the cache is managed by
// whoever owns the data directory, never through the API token.
func (s *Server) registerImageOps(mux *http.ServeMux) {
	mux.HandleFunc("GET /images", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, s.images.list())
	})
	// POST /images/pull?ref=... streams podman's progress as plain text, then a
	// final line: "ok <json>" or "error <message>".
	mux.HandleFunc("POST /images/pull", func(w http.ResponseWriter, r *http.Request) {
		ref, err := ociimage.ParseRef(r.URL.Query().Get("ref"))
		if err != nil {
			writeErr(w, http.StatusBadRequest, "bad_request", err.Error())
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		fw := &flushWriter{w: w}
		defer fw.close() // the pull may outlive this request and keep writing
		img, err := s.images.pull(r.Context(), ref, fw)
		if err != nil {
			fmt.Fprintf(fw, "error %s\n", strings.ReplaceAll(err.Error(), "\n", " "))
			return
		}
		b, _ := json.Marshal(withDiskFigures(s.images.dir, img))
		fmt.Fprintf(fw, "ok %s\n", b)
	})
	mux.HandleFunc("DELETE /images", func(w http.ResponseWriter, r *http.Request) {
		img, err := s.images.remove(r.URL.Query().Get("key"))
		if errors.Is(err, errImageNotFound) {
			writeErr(w, http.StatusNotFound, "not_found", err.Error())
			return
		}
		if err != nil {
			writeErr(w, http.StatusBadRequest, "bad_request", err.Error())
			return
		}
		s.log.Info("image removed from the cache", "id", img.ID[:12], "refs", strings.Join(img.Refs, ","))
		writeJSON(w, http.StatusOK, img)
	})
}

// flushWriter pushes each write to the client at once; podman's progress
// arrives in small pieces worth seeing as they come.
type flushWriter struct {
	mu     sync.Mutex
	w      http.ResponseWriter
	closed bool
}

func (f *flushWriter) close() {
	f.mu.Lock()
	f.closed = true
	f.mu.Unlock()
}

func (f *flushWriter) Write(p []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return len(p), nil
	}
	n, err := f.w.Write(p)
	if fl, ok := f.w.(http.Flusher); ok {
		fl.Flush()
	}
	return n, err
}
