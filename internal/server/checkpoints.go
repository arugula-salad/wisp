package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"time"

	"github.com/arugula-salad/wisp/internal/store"
)

// Checkpoints are whole-disk clones under <machine dir>/checkpoints/<id>.ext4.
// They capture the filesystem only, never processes or memory.
//
// Manual checkpoints are v1, v2, ...; automatic ones are auto-1, auto-2, ... on
// their own counter, hidden from the default listing, and pruned to the newest
// Options.AutoCheckpointKeep. Autos are taken before every restore (so a restore
// can itself be undone) and in the background, at most every
// Options.AutoCheckpointInterval, for a sprite whose disk changed since its
// newest checkpoint.
//
// This file is the routes. Taking, restoring and pruning checkpoints is the
// lifecycle's, under the sprite's lock (lifecycle_checkpoints.go); what is
// reported while that runs comes back through a progress callback, and how it
// is streamed to the client is decided here.

var errNoCheckpoint = errors.New("checkpoint not found")

type ndjson struct {
	w  http.ResponseWriter
	fl http.Flusher
}

func newNDJSON(w http.ResponseWriter) *ndjson {
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.WriteHeader(http.StatusOK)
	fl, _ := w.(http.Flusher)
	return &ndjson{w, fl}
}

func (n *ndjson) emit(typ, field, msg string) {
	json.NewEncoder(n.w).Encode(map[string]string{"type": typ, field: msg, "time": time.Now().UTC().Format(time.RFC3339Nano)})
	if n.fl != nil {
		n.fl.Flush()
	}
}
func (n *ndjson) info(f string, a ...any)     { n.emit("info", "data", fmt.Sprintf(f, a...)) }
func (n *ndjson) fail(f string, a ...any)     { n.emit("error", "error", fmt.Sprintf(f, a...)) }
func (n *ndjson) complete(f string, a ...any) { n.emit("complete", "data", fmt.Sprintf(f, a...)) }

func findCheckpoint(sp store.Sprite, id string) *store.Checkpoint {
	for i := range sp.Checkpoints {
		if sp.Checkpoints[i].ID == id {
			return &sp.Checkpoints[i]
		}
	}
	return nil
}

// filterCheckpoints is the listing, newest first. Autos appear only on request;
// history keeps the checkpoints that descend from that one.
func filterCheckpoints(sp store.Sprite, history string, includeAuto bool) []store.Checkpoint {
	out := []store.Checkpoint{}
	for _, cp := range slices.Backward(sp.Checkpoints) {
		if cp.IsAuto && !includeAuto {
			continue
		}
		if history != "" && !slices.Contains(cp.History, history) {
			continue
		}
		out = append(out, cp)
	}
	return out
}

// The handlers below take the sprite as an argument, and the in-guest channel
// the request arrived on (nil for the public API). A guest request is only
// honoured while the VM that sent it is still the running one; the lifecycle
// answers errStaleGuest otherwise.

var errStaleGuest = errors.New("the sprite was suspended before the request ran; retry")

func (s *Server) createCheckpoint(w http.ResponseWriter, r *http.Request, sp store.Sprite, from *guestChan) {
	var req struct {
		Comment string `json:"comment"`
	}
	// The body is optional; one that is there has to be JSON.
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
		writeErr(w, http.StatusBadRequest, "bad_request", "invalid JSON body")
		return
	}

	// Whoever holds the API token may fill this machine's disk; code inside a
	// sprite may not. Every checkpoint is a full clone, so without a ceiling a
	// loop in one guest would starve every other sprite. (Approximate under
	// concurrent creates, which is fine for a ceiling.)
	if limit := s.opts.GuestCheckpointLimit; from != nil && limit > 0 && len(filterCheckpoints(sp, "", false)) >= limit {
		s.life.emit(sp.Record, "limit.refused", map[string]any{"limit": "guest_checkpoints", "max": limit, "current": len(filterCheckpoints(sp, "", false))})
		writeErr(w, http.StatusConflict, "checkpoint_limit", fmt.Sprintf(
			"this sprite already has %d checkpoints, the most it may create from inside; delete some first", limit))
		return
	}

	out := newNDJSON(w)
	cp, err := s.life.CreateCheckpoint(sp, from, req.Comment, out.info)
	if err != nil {
		out.fail("%v", err)
		return
	}
	out.complete("Checkpoint %s created", cp.ID)
}

func (s *Server) listCheckpoints(w http.ResponseWriter, r *http.Request, sp store.Sprite, _ *guestChan) {
	// Always JSON: the SDK rejects the text/plain form upstream has for history listings.
	q := r.URL.Query()
	writeJSON(w, http.StatusOK, filterCheckpoints(sp, q.Get("history"), q.Get("includeAuto") == "true"))
}

func (s *Server) getCheckpoint(w http.ResponseWriter, r *http.Request, sp store.Sprite, _ *guestChan) {
	cp := findCheckpoint(sp, r.PathValue("id"))
	if cp == nil {
		writeErr(w, http.StatusNotFound, "not_found", "checkpoint not found")
		return
	}
	writeJSON(w, http.StatusOK, cp)
}

func (s *Server) deleteCheckpoint(w http.ResponseWriter, r *http.Request, sp store.Sprite, _ *guestChan) {
	switch err := s.life.DeleteCheckpoint(sp, r.PathValue("id")); {
	case err == nil:
		w.WriteHeader(http.StatusNoContent)
	case errors.Is(err, errCheckpointMounted):
		writeErr(w, http.StatusConflict, "checkpoint_mounted", errCheckpointMounted.Error())
	case errors.Is(err, errNoCheckpoint):
		writeErr(w, http.StatusNotFound, "not_found", "checkpoint not found")
	default:
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
	}
}

// guestRestoreGrace lets the last progress line travel vsock -> agent ->
// sprite-env -> the user's terminal before the VM carrying it is killed.
const guestRestoreGrace = 300 * time.Millisecond

func (s *Server) restoreCheckpoint(w http.ResponseWriter, r *http.Request, sp store.Sprite, from *guestChan) {
	id := r.PathValue("id")
	if findCheckpoint(sp, id) == nil {
		writeErr(w, http.StatusNotFound, "not_found", "checkpoint not found")
		return
	}
	out := newNDJSON(w)
	out.info("Restoring to checkpoint %s...", id)

	var beforeStop func()
	if from != nil {
		// The requester dies with the VM, so this is the last thing it hears:
		// everything that could still be reported as a failure has passed.
		beforeStop = func() {
			out.info("Restarting the sprite from %s; this session ends now", id)
			time.Sleep(guestRestoreGrace)
		}
	}
	if err := s.life.RestoreCheckpoint(sp, from, id, out.info, beforeStop); err != nil {
		if from != nil && !errors.Is(err, errStaleGuest) {
			s.log.Error("in-guest restore failed", "sprite", sp.Name, "checkpoint", id, "err", err)
		}
		out.fail("%v", err)
		return
	}
	out.complete("Restored to %s", id)
}
