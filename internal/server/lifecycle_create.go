package server

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/arugula-salad/wisp/internal/store"
	"github.com/arugula-salad/wisp/internal/vmm"
)

// The engine half of creating a sprite: the disk it starts as, the disk
// guard, the record (and with it the sprite's network index) and the clone.
// api.go has the request: names, URL settings, limits, where a clone may come
// from, and the HTTP answers.

// CreateSpec is a sprite to create and what its disk starts as: the base image
// unless ImageDisk or Checkpoint says otherwise.
type CreateSpec struct {
	// Sprite is the record to create, complete but for its NetIndex, which the
	// store assigns.
	Sprite store.Sprite
	// ImageDisk is the disk of the container image Sprite.Image names
	// (images.go). The caller holds it until Create returns, so the cached
	// image cannot be removed under the copy.
	ImageDisk string
	// Checkpoint is a checkpoint to clone. The caller holds it with
	// HoldCheckpoint until Create returns, so it cannot be deleted under the copy.
	Checkpoint *CheckpointRef
}

// CheckpointRef names one checkpoint of a sprite.
type CheckpointRef struct {
	Sprite store.Sprite
	ID     string
}

// errProvision is a disk that could not be made: the base image, or the copy.
var errProvision = errors.New("provision disk")

// Create makes a sprite: it admits the disk it will write, writes the record
// and clones the disk, then reports sprite.created (and sprite.expiring, for
// a lease that is already inside its warning window). Errors: errNoRoom from
// the disk guard, store.ErrExists for a name already taken, errProvision.
func (l *Lifecycle) Create(ctx context.Context, spec CreateSpec) (store.Sprite, error) {
	sp := spec.Sprite
	// Described from here, so that a refusal before the record is written
	// names the sprite it refused.
	defer l.holdUnstored(sp)()
	var image string
	var detail map[string]any
	switch {
	case spec.Checkpoint != nil:
		src := spec.Checkpoint
		image = l.checkpointPath(src.Sprite.ID, src.ID)
		detail = map[string]any{"from": map[string]string{"sprite": src.Sprite.Name, "checkpoint": src.ID}}
	case spec.ImageDisk != "":
		image = spec.ImageDisk
		detail = map[string]any{"from": map[string]string{"image": sp.Image}}
	default:
		base, err := l.storage.base(ctx)
		if err != nil {
			return sp, fmt.Errorf("%w: %v", errProvision, err)
		}
		image = base
	}
	if err := l.disk.admit(sp.Record, "a new sprite", l.cloneCost(image)); err != nil {
		return sp, err
	}
	if err := l.store.Create(&sp); err != nil {
		return sp, err
	}
	disk := filepath.Join(l.store.Dir(sp.ID), vmm.DiskFile)
	if err := cloneFile(ctx, image, disk); err != nil {
		l.store.Delete(sp.ID)
		return sp, fmt.Errorf("%w: %v", errProvision, err)
	}
	l.log.Info("sprite created", "sprite", sp.Name, "id", sp.ID, "net_index", sp.NetIndex, "parent", sp.ParentID, "cloned", spec.Checkpoint != nil, "image", sp.Image)
	l.emit(sp.Record, "sprite.created", detail)
	// A sprite can be born already inside the warning window -- a lobby child with
	// a two-minute lease, say, under a five-minute --lease-warning. The janitor
	// would never get to warn about it, so the warning is evaluated here too,
	// after sprite.created, keeping the stream's order honest.
	l.leases.warn(sp, time.Now())
	return sp, nil
}

// cloneFile copies a disk image, as a reflink where the filesystem supports
// it (instant, copy-on-write) and as a sparse copy otherwise.
func cloneFile(ctx context.Context, src, dst string) error {
	tmp := dst + ".tmp"
	out, err := exec.CommandContext(ctx, "cp", "--reflink=auto", "--sparse=always", src, tmp).CombinedOutput()
	if err != nil {
		os.Remove(tmp)
		return fmt.Errorf("%v: %s", err, strings.TrimSpace(string(out)))
	}
	return os.Rename(tmp, dst)
}

// Delete deletes a sprite whole: its VM and memory state, its record, disk
// and checkpoints, its runtime, its network index and egress policy, and its
// backup state, which becomes a tombstone in the bucket rather than a
// deletion, so losing this machine and deleting a sprite do not look the same
// to it. It is the only deletion there is: a DELETE from outside, a spawner's
// DELETE of a child and an expired lease (leases.go) all come here. What the
// front end keeps beside the record (custom domains) it hears about through
// OnDelete, before sprite.deleted goes out.
//
// It takes and drops the sprite's lock to stop it, so the caller must not
// hold it.
func (l *Lifecycle) Delete(sp store.Sprite) error {
	l.Stop(sp.Record, false)
	// The record as it is just before it goes, to describe sprite.deleted by.
	if cur, err := l.store.Get(sp.ID); err == nil {
		defer l.holdUnstored(cur)()
	}
	if err := l.store.Delete(sp.ID); err != nil {
		return err
	}
	l.Forget(sp.ID)
	l.egress.forget(sp.Record)
	l.mu.Lock()
	hooks := l.onDelete
	l.mu.Unlock()
	for _, f := range hooks {
		f(sp)
	}
	// `wispd backups prune` retires the tombstone later.
	l.backups.MarkDeleted(sp)
	l.leases.forget(sp.ID) // a warning already sent belongs to the sprite
	l.log.Info("sprite deleted", "sprite", sp.Name)
	l.emit(sp.Record, "sprite.deleted", nil)
	return nil
}

// OnDelete registers f to run for every sprite Delete deletes, once its
// record is gone and before sprite.deleted is published.
func (l *Lifecycle) OnDelete(f func(store.Sprite)) {
	l.mu.Lock()
	l.onDelete = append(l.onDelete, f)
	l.mu.Unlock()
}
