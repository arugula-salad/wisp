package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/arugula-salad/wisp/engine"
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
// lifecycle's, under the sprite's lock (engine/lifecycle_checkpoints.go); what is
// reported while that runs comes back through a progress callback, and how it
// is streamed to the client is decided here.

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

// The handlers below take the sprite as an argument, and the in-guest channel
// the request arrived on (nil for the public API). A guest request is only
// honoured while the VM that sent it is still the running one; the engine
// answers engine.ErrStaleGuest otherwise.

func (s *Server) createCheckpoint(w http.ResponseWriter, r *http.Request, sp store.Sprite, from *engine.GuestChan) {
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
	if limit := s.opts.GuestCheckpointLimit; from != nil && limit > 0 && len(engine.ListCheckpoints(sp.Record, "", false)) >= limit {
		s.life.Emit(sp.Record, "limit.refused", map[string]any{"limit": "guest_checkpoints", "max": limit, "current": len(engine.ListCheckpoints(sp.Record, "", false))})
		writeErr(w, http.StatusConflict, "checkpoint_limit", fmt.Sprintf(
			"this sprite already has %d checkpoints, the most it may create from inside; delete some first", limit))
		return
	}

	out := newNDJSON(w)
	cp, err := s.life.CreateCheckpoint(sp.Record, from, req.Comment, out.info)
	if err != nil {
		out.fail("%v", err)
		return
	}
	out.complete("Checkpoint %s created", cp.ID)
}

func (s *Server) listCheckpoints(w http.ResponseWriter, r *http.Request, sp store.Sprite, _ *engine.GuestChan) {
	// Always JSON: the SDK rejects the text/plain form upstream has for history listings.
	q := r.URL.Query()
	writeJSON(w, http.StatusOK, engine.ListCheckpoints(sp.Record, q.Get("history"), q.Get("includeAuto") == "true"))
}

func (s *Server) getCheckpoint(w http.ResponseWriter, r *http.Request, sp store.Sprite, _ *engine.GuestChan) {
	cp := engine.FindCheckpoint(sp.Record, r.PathValue("id"))
	if cp == nil {
		writeErr(w, http.StatusNotFound, "not_found", "checkpoint not found")
		return
	}
	writeJSON(w, http.StatusOK, cp)
}

func (s *Server) deleteCheckpoint(w http.ResponseWriter, r *http.Request, sp store.Sprite, _ *engine.GuestChan) {
	switch err := s.life.DeleteCheckpoint(sp.Record, r.PathValue("id")); {
	case err == nil:
		w.WriteHeader(http.StatusNoContent)
	case errors.Is(err, engine.ErrCheckpointMounted):
		writeErr(w, http.StatusConflict, "checkpoint_mounted", engine.ErrCheckpointMounted.Error())
	case errors.Is(err, engine.ErrNoCheckpoint):
		writeErr(w, http.StatusNotFound, "not_found", "checkpoint not found")
	default:
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
	}
}

// guestRestoreGrace lets the last progress line travel vsock -> agent ->
// sprite-env -> the user's terminal before the VM carrying it is killed.
const guestRestoreGrace = 300 * time.Millisecond

func (s *Server) restoreCheckpoint(w http.ResponseWriter, r *http.Request, sp store.Sprite, from *engine.GuestChan) {
	id := r.PathValue("id")
	if engine.FindCheckpoint(sp.Record, id) == nil {
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
	if err := s.life.RestoreCheckpoint(sp.Record, from, id, out.info, beforeStop); err != nil {
		if from != nil && !errors.Is(err, engine.ErrStaleGuest) {
			s.log.Error("in-guest restore failed", "sprite", sp.Name, "checkpoint", id, "err", err)
		}
		out.fail("%v", err)
		return
	}
	out.complete("Restored to %s", id)
}
