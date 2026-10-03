package server

import (
	"errors"
	"net/http"

	"github.com/arugula-salad/wisp/internal/store"
)

// A sprite can browse an old checkpoint without restoring it: the checkpoint's
// disk image is attached read-only and the agent mounts it under
// /.sprite/checkpoints/<id>. Firecracker cannot hot-plug a drive, so every VM
// boots with a few placeholder drives ("slots") and mounting swaps the file
// behind one. The image is served in place: no copy is made.
//
// Only the guest can ask (over its own channel), so there is no public route.
// The slot bookkeeping is the lifecycle's (lifecycle_mounts.go).

type mountReply struct {
	Slot int `json:"slot"` // the guest derives the block device from this
}

func (s *Server) mountCheckpoint(w http.ResponseWriter, r *http.Request, sp store.Sprite, from *guestChan) {
	slot, err := s.life.MountCheckpoint(r.Context(), sp.Record, from, r.PathValue("id"))
	switch {
	case err == nil:
		writeJSON(w, http.StatusOK, mountReply{Slot: slot})
	case errors.Is(err, errStaleGuest):
		writeErr(w, http.StatusConflict, "stale", errStaleGuest.Error())
	case errors.Is(err, errNoCheckpoint):
		writeErr(w, http.StatusNotFound, "not_found", "checkpoint not found")
	case errors.Is(err, errMountsFull):
		writeErr(w, http.StatusConflict, "mounts_full", err.Error())
	case errors.Is(err, errNoSlots):
		writeErr(w, http.StatusConflict, "no_slots", err.Error())
	default:
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
	}
}

func (s *Server) unmountCheckpoint(w http.ResponseWriter, r *http.Request, sp store.Sprite, from *guestChan) {
	switch err := s.life.UnmountCheckpoint(r.Context(), sp.Record, from, r.PathValue("id")); {
	case err == nil:
		w.WriteHeader(http.StatusNoContent) // unmounting what is not mounted is not an error
	case errors.Is(err, errStaleGuest):
		writeErr(w, http.StatusConflict, "stale", errStaleGuest.Error())
	case errors.Is(err, store.ErrNotFound):
		writeErr(w, http.StatusNotFound, "not_found", "sprite not found")
	default:
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
	}
}

var errCheckpointMounted = errors.New("the checkpoint is mounted inside the sprite; unmount it first")
