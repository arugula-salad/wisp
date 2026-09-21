package server

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"syscall"

	"golang.org/x/sys/unix"
)

// Disk clones (new sprites, checkpoints, restores) are reflinks when the sprite
// directory is on a filesystem that supports them (scripts/setup-storage.sh
// makes one): instant, and sharing blocks until written. A reflink cannot cross
// filesystems, so the base image, which lives outside the sprite directory and
// may be a symlink into another data dir, is mirrored onto the sprites' volume.

const localBaseName = ".base.ext4" // a file, not a directory: the store ignores it

type storage struct {
	vmRoot, baseImage string
	reflink           bool

	mu sync.Mutex // serializes refreshing the mirror
}

func newStorage(vmRoot, baseImage string) *storage {
	st := &storage{vmRoot: vmRoot, baseImage: baseImage}
	st.reflink = probeReflink(vmRoot)
	return st
}

// probeReflink asks the filesystem directly rather than guessing from its type.
func probeReflink(dir string) bool {
	src, err := os.CreateTemp(dir, ".reflink-probe-*")
	if err != nil {
		return false
	}
	defer os.Remove(src.Name())
	defer src.Close()
	dst, err := os.CreateTemp(dir, ".reflink-probe-*")
	if err != nil {
		return false
	}
	defer os.Remove(dst.Name())
	defer dst.Close()
	if _, err := src.Write(make([]byte, 4096)); err != nil {
		return false
	}
	return unix.IoctlFileClone(int(dst.Fd()), int(src.Fd())) == nil
}

func device(path string) (uint64, error) {
	var st syscall.Stat_t
	if err := syscall.Stat(path, &st); err != nil {
		return 0, err
	}
	return st.Dev, nil
}

// base returns the image to clone new sprites from: the base image itself when
// it shares a filesystem with the sprites, otherwise an up-to-date mirror that does.
func (st *storage) base(ctx context.Context) (string, error) {
	if !st.reflink {
		return st.baseImage, nil // a mirror buys nothing where every clone is a copy anyway
	}
	baseDev, err := device(st.baseImage)
	if err != nil {
		return "", err
	}
	if rootDev, err := device(st.vmRoot); err == nil && rootDev == baseDev {
		return st.baseImage, nil
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	mirror := filepath.Join(st.vmRoot, localBaseName)
	src, err := os.Stat(st.baseImage)
	if err != nil {
		return "", err
	}
	// The mirror carries the source's mtime, so a rebuilt base image is noticed.
	if have, err := os.Stat(mirror); err == nil && have.Size() == src.Size() && have.ModTime().Equal(src.ModTime()) {
		return mirror, nil
	}
	if err := cloneFile(ctx, st.baseImage, mirror); err != nil {
		return "", fmt.Errorf("mirror base image onto the sprite volume: %w", err)
	}
	if err := os.Chtimes(mirror, src.ModTime(), src.ModTime()); err != nil {
		return "", err
	}
	return mirror, nil
}
