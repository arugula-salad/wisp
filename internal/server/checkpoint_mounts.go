package server

import (
	"context"
	"errors"
	"net/http"
	"path/filepath"
	"time"

	"github.com/jhgaylor/mini-sprites/internal/store"
	"github.com/jhgaylor/mini-sprites/internal/vmm"
)

// A sprite can browse an old checkpoint without restoring it: the checkpoint's
// disk image is attached read-only and the agent mounts it under
// /.sprite/checkpoints/<id>. Firecracker cannot hot-plug a drive, so every VM
// boots with a few placeholder drives ("slots") and mounting swaps the file
// behind one. The image is served in place: no copy is made.
//
// Only the guest can ask (over its own channel), so there is no public route.

// liveMounts is the sprite's slot table if it still means anything. Drive
// paths live in the VM and in its warm snapshot; once both are gone (a kill, a
// restore, going cold) the next boot starts from placeholders again.
func (s *Server) liveMounts(sp store.Sprite, rt *runtime) map[int]string {
	if rt.m == nil && !vmm.HasSnapshot(s.store.Dir(sp.ID)) {
		return nil
	}
	return sp.Mounts
}

// checkpointMounted reports whether a checkpoint's image backs a drive right now,
// in which case the file must not be deleted from under the guest.
func (s *Server) checkpointMounted(sp store.Sprite, rt *runtime, id string) bool {
	for _, mounted := range s.liveMounts(sp, rt) {
		if mounted == id {
			return true
		}
	}
	return false
}

type mountReply struct {
	Slot int `json:"slot"` // the guest derives the block device from this
}

func (s *Server) mountCheckpoint(w http.ResponseWriter, r *http.Request, sp store.Sprite, from *guestChan) {
	id := r.PathValue("id")
	rt := s.life.rt(sp.ID)
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if rt.m == nil || rt.guest != from {
		writeErr(w, http.StatusConflict, "stale", errStaleGuest.Error())
		return
	}
	sp, err := s.store.Get(sp.Name) // re-read under the lock
	if err != nil || findCheckpoint(sp, id) == nil {
		writeErr(w, http.StatusNotFound, "not_found", "checkpoint not found")
		return
	}
	mounts := s.liveMounts(sp, rt)
	slot := -1
	for i := vmm.CheckpointSlots - 1; i >= 0; i-- {
		switch mounts[i] {
		case id: // already attached: mounting is idempotent
			writeJSON(w, http.StatusOK, mountReply{Slot: i})
			return
		case "":
			slot = i
		}
	}
	if slot < 0 {
		writeErr(w, http.StatusConflict, "mounts_full", "all checkpoint mount slots are in use; unmount one first")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	rel, _ := filepath.Rel(s.store.Dir(sp.ID), s.checkpointPath(sp.ID, id))
	if err := rt.m.SwapDrive(ctx, vmm.SlotDrive(slot), rel); err != nil {
		// A VM resumed from a snapshot taken before slots existed has no such drive.
		writeErr(w, http.StatusConflict, "no_slots", "this sprite was booted without checkpoint slots; they appear after its next cold boot ("+err.Error()+")")
		return
	}
	s.store.Update(sp.Name, func(sp *store.Sprite) {
		next := map[int]string{slot: id}
		for k, v := range mounts {
			next[k] = v
		}
		sp.Mounts = next
	})
	writeJSON(w, http.StatusOK, mountReply{Slot: slot})
}

func (s *Server) unmountCheckpoint(w http.ResponseWriter, r *http.Request, sp store.Sprite, from *guestChan) {
	id := r.PathValue("id")
	rt := s.life.rt(sp.ID)
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if rt.m == nil || rt.guest != from {
		writeErr(w, http.StatusConflict, "stale", errStaleGuest.Error())
		return
	}
	sp, err := s.store.Get(sp.Name)
	if err != nil {
		writeErr(w, http.StatusNotFound, "not_found", "sprite not found")
		return
	}
	mounts := s.liveMounts(sp, rt)
	for slot, mounted := range mounts {
		if mounted != id {
			continue
		}
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()
		if err := rt.m.SwapDrive(ctx, vmm.SlotDrive(slot), ""); err != nil {
			writeErr(w, http.StatusInternalServerError, "internal", err.Error())
			return
		}
		s.store.Update(sp.Name, func(sp *store.Sprite) {
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
	w.WriteHeader(http.StatusNoContent) // unmounting what is not mounted is not an error
}

var errCheckpointMounted = errors.New("the checkpoint is mounted inside the sprite; unmount it first")
