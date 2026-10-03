package engine

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"time"

	"github.com/arugula-salad/wisp/internal/store"
	"github.com/arugula-salad/wisp/internal/vmm"
)

// The engine half of checkpoint mounts (internal/server/checkpoint_mounts.go has the routes).

// Mount errors.
var (
	// ErrMountsFull is a mount with every checkpoint slot taken.
	ErrMountsFull = errors.New("all checkpoint mount slots are in use; unmount one first")
	// ErrNoSlots is a VM resumed from a snapshot taken before slots existed.
	ErrNoSlots = errors.New("this sprite was booted without checkpoint slots; they appear after its next cold boot")
)

// liveMounts is the sprite's slot table if it still means anything. Drive
// paths live in the VM and in its warm snapshot; once both are gone (a kill, a
// restore, going cold) the next boot starts from placeholders again. Callers
// hold rt.mu.
func (l *Engine) liveMounts(sp store.Record, rt *runtime) map[int]string {
	if rt.m == nil && !vmm.HasSnapshot(l.store.Dir(sp.ID)) {
		return nil
	}
	return sp.Mounts
}

// checkpointMounted reports whether a checkpoint's image backs a drive right now,
// in which case the file must not be deleted from under the guest. Callers hold rt.mu.
func (l *Engine) checkpointMounted(sp store.Record, rt *runtime, id string) bool {
	for _, mounted := range l.liveMounts(sp, rt) {
		if mounted == id {
			return true
		}
	}
	return false
}

// MountCheckpoint attaches checkpoint id behind a free slot of the VM that from
// is the channel of, and returns the slot. Mounting what is already mounted is
// idempotent. Errors: ErrStaleGuest, ErrNoCheckpoint, ErrMountsFull, ErrNoSlots.
func (l *Engine) MountCheckpoint(ctx context.Context, sp store.Record, from *GuestChan, id string) (int, error) {
	rt := l.rt(sp.ID)
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if rt.m == nil || rt.guest != from {
		return -1, ErrStaleGuest
	}
	sp, err := l.store.GetRecord(sp.ID) // re-read under the lock
	if err != nil || FindCheckpoint(sp, id) == nil {
		return -1, ErrNoCheckpoint
	}
	mounts := l.liveMounts(sp, rt)
	slot := -1
	for i := vmm.CheckpointSlots - 1; i >= 0; i-- {
		switch mounts[i] {
		case id: // already attached: mounting is idempotent
			return i, nil
		case "":
			slot = i
		}
	}
	if slot < 0 {
		return -1, ErrMountsFull
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	rel, _ := filepath.Rel(l.store.Dir(sp.ID), l.checkpointPath(sp.ID, id))
	if err := rt.m.SwapDrive(ctx, vmm.SlotDrive(slot), rel); err != nil {
		return -1, fmt.Errorf("%w (%v)", ErrNoSlots, err)
	}
	l.store.UpdateRecord(sp.ID, func(sp *store.Record) {
		next := map[int]string{slot: id}
		for k, v := range mounts {
			next[k] = v
		}
		sp.Mounts = next
	})
	return slot, nil
}

// UnmountCheckpoint detaches checkpoint id from the VM that from is the channel
// of. Unmounting what is not mounted is not an error. Errors: ErrStaleGuest,
// store.ErrNotFound, or the VM's own.
func (l *Engine) UnmountCheckpoint(ctx context.Context, sp store.Record, from *GuestChan, id string) error {
	rt := l.rt(sp.ID)
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if rt.m == nil || rt.guest != from {
		return ErrStaleGuest
	}
	sp, err := l.store.GetRecord(sp.ID)
	if err != nil {
		return err
	}
	mounts := l.liveMounts(sp, rt)
	for slot, mounted := range mounts {
		if mounted != id {
			continue
		}
		ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		if err := rt.m.SwapDrive(ctx, vmm.SlotDrive(slot), ""); err != nil {
			return err
		}
		l.store.UpdateRecord(sp.ID, func(sp *store.Record) {
			next := map[int]string{}
			for k, v := range mounts {
				if k != slot {
					next[k] = v
				}
			}
			sp.Mounts = next
		})
		break
	}
	return nil
}
