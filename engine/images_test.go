package engine

import (
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"

	"github.com/arugula-salad/wisp/internal/ociimage"
)

func TestMovedTagReplacesTheOldDisk(t *testing.T) {
	dir := t.TempDir()
	c := &ImageCache{dir: dir, log: slog.New(slog.NewTextHandler(io.Discard, nil)), images: map[string]*CachedImage{}}
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
