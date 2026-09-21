package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/jhgaylor/mini-sprites/internal/store"
	"github.com/jhgaylor/mini-sprites/internal/vmm"
)

// Checkpoints are whole-disk clones under <machine dir>/checkpoints/<id>.ext4.
// They capture the filesystem only, never processes or memory.

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

func (s *Server) checkpointPath(sp store.Sprite, id string) string {
	return filepath.Join(s.store.Dir(sp.ID), "checkpoints", id+".ext4")
}

func (s *Server) createCheckpoint(w http.ResponseWriter, r *http.Request) {
	sp, ok := s.lookup(w, r)
	if !ok {
		return
	}
	var req struct {
		Comment string `json:"comment"`
	}
	json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req) // body is optional

	out := newNDJSON(w)
	rt := s.life.rt(sp.ID)
	rt.mu.Lock()
	defer rt.mu.Unlock()
	// Re-read under the lock so concurrent creates get distinct IDs.
	sp, err := s.store.Get(sp.Name)
	if err != nil {
		out.fail("sprite was deleted")
		return
	}
	id := fmt.Sprintf("v%d", sp.NextCheckpoint+1)
	out.info("Creating checkpoint %s...", id)
	if err := os.MkdirAll(filepath.Dir(s.checkpointPath(sp, id)), 0o755); err != nil {
		out.fail("%v", err)
		return
	}

	// Detached from the request: a client hanging up must not leave the VM paused.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	if rt.m != nil {
		// Quiesce: flush guest caches, then freeze vCPUs so the clone is a consistent point in time.
		if err := agentCall(ctx, rt.m, http.MethodPost, "/internal/presuspend", nil, nil); err != nil {
			out.fail("sync guest filesystem: %v", err)
			return
		}
		if err := rt.m.Pause(ctx); err != nil {
			out.fail("pause VM: %v", err)
			return
		}
	}
	start := time.Now()
	err = cloneFile(ctx, filepath.Join(s.store.Dir(sp.ID), vmm.DiskFile), s.checkpointPath(sp, id))
	if rt.m != nil {
		if rerr := rt.m.Resume(ctx); rerr != nil {
			s.log.Error("resume after checkpoint failed", "sprite", sp.Name, "err", rerr)
		}
	}
	if err != nil {
		out.fail("clone disk: %v", err)
		return
	}
	out.info("Filesystem captured in %s", time.Since(start).Round(time.Millisecond))
	s.store.Update(sp.Name, func(sp *store.Sprite) {
		sp.NextCheckpoint++
		sp.Checkpoints = append(sp.Checkpoints, store.Checkpoint{ID: id, CreateTime: time.Now().UTC(), Comment: req.Comment})
	})
	s.log.Info("checkpoint created", "sprite", sp.Name, "checkpoint", id)
	out.complete("Checkpoint %s created", id)
}

func (s *Server) listCheckpoints(w http.ResponseWriter, r *http.Request) {
	sp, ok := s.lookup(w, r)
	if !ok {
		return
	}
	out := []store.Checkpoint{}
	for i := len(sp.Checkpoints) - 1; i >= 0; i-- { // newest first
		out = append(out, sp.Checkpoints[i])
	}
	writeJSON(w, http.StatusOK, out)
}

func findCheckpoint(sp store.Sprite, id string) *store.Checkpoint {
	for i := range sp.Checkpoints {
		if sp.Checkpoints[i].ID == id {
			return &sp.Checkpoints[i]
		}
	}
	return nil
}

func (s *Server) getCheckpoint(w http.ResponseWriter, r *http.Request) {
	sp, ok := s.lookup(w, r)
	if !ok {
		return
	}
	cp := findCheckpoint(sp, r.PathValue("id"))
	if cp == nil {
		writeErr(w, http.StatusNotFound, "not_found", "checkpoint not found")
		return
	}
	writeJSON(w, http.StatusOK, cp)
}

func (s *Server) restoreCheckpoint(w http.ResponseWriter, r *http.Request) {
	sp, ok := s.lookup(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	if findCheckpoint(sp, id) == nil {
		writeErr(w, http.StatusNotFound, "not_found", "checkpoint not found")
		return
	}
	out := newNDJSON(w)
	out.info("Restoring to checkpoint %s...", id)

	rt := s.life.rt(sp.ID)
	rt.mu.Lock()
	defer rt.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	wasRunning := rt.m != nil
	if wasRunning {
		out.info("Stopping services...")
		rt.m.Kill() // the running filesystem is about to be replaced; nothing in it is worth flushing
		s.life.cleanupLocked(rt)
	}
	dir := s.store.Dir(sp.ID)
	vmm.DiscardSnapshot(dir) // memory state belongs to the filesystem being replaced
	out.info("Restoring filesystem...")
	if err := cloneFile(ctx, s.checkpointPath(sp, id), filepath.Join(dir, vmm.DiskFile)); err != nil {
		out.fail("restore disk: %v", err)
		return
	}
	s.log.Info("checkpoint restored", "sprite", sp.Name, "checkpoint", id)
	if wasRunning {
		// The environment restarts on its own, so services come back from the
		// restored disk without waiting for the next request.
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			if _, release, err := s.life.Acquire(ctx, sp); err == nil {
				release()
			}
		}()
	}
	out.complete("Restored to %s", id)
}
