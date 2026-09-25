package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/arugula-salad/wisp/internal/store"
	"github.com/arugula-salad/wisp/internal/vmm"
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
// The *Locked functions are the core. They are shared by the public API, the
// in-guest channel (guestapi.go) and the background loop; callers hold rt.mu.

var errNoCheckpoint = errors.New("checkpoint not found")

// progress receives human-readable status lines while an operation runs.
type progress func(format string, a ...any)

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

func (s *Server) checkpointPath(id, checkpoint string) string {
	return filepath.Join(s.store.Dir(id), "checkpoints", checkpoint+".ext4")
}

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

// createCheckpointLocked clones the live disk. name is re-read under the lock so
// concurrent creates get distinct IDs.
func (s *Server) createCheckpointLocked(rt *runtime, name, comment string, auto bool, info progress) (store.Checkpoint, error) {
	sp, err := s.store.Get(name)
	if err != nil {
		return store.Checkpoint{}, errors.New("sprite was deleted")
	}
	cp := store.Checkpoint{ID: fmt.Sprintf("v%d", sp.NextCheckpoint+1), Comment: comment, History: sp.Lineage}
	if auto {
		cp.ID, cp.IsAuto = fmt.Sprintf("auto-%d", sp.NextAuto+1), true
	}
	live := filepath.Join(s.store.Dir(sp.ID), vmm.DiskFile)
	if err := s.life.disk.admit(sp, "a checkpoint", s.cloneCost(live)); err != nil {
		return cp, err
	}
	info("Creating checkpoint %s...", cp.ID)
	dst := s.checkpointPath(sp.ID, cp.ID)
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return cp, err
	}

	// Detached from the request: a client hanging up must not leave the VM paused.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	if rt.m != nil {
		// Quiesce: flush guest caches, then freeze vCPUs so the clone is a consistent point in time.
		if err := agentCall(ctx, rt.m, http.MethodPost, "/internal/presuspend", nil, nil); err != nil {
			return cp, fmt.Errorf("sync guest filesystem: %w", err)
		}
		if err := rt.m.Pause(ctx); err != nil {
			return cp, fmt.Errorf("pause VM: %w", err)
		}
	}
	start := time.Now()
	err = cloneFile(ctx, live, dst)
	if rt.m != nil {
		if rerr := rt.m.Resume(ctx); rerr != nil {
			s.log.Error("resume after checkpoint failed", "sprite", sp.Name, "err", rerr)
		}
	}
	if err != nil {
		return cp, fmt.Errorf("clone disk: %w", err)
	}
	info("Filesystem captured in %s", time.Since(start).Round(time.Millisecond))
	// The moment captured, not the moment the copy finished: the background loop
	// compares this with the disk's mtime to tell whether anything changed since.
	cp.CreateTime = start.UTC()
	s.store.Update(sp.Name, func(sp *store.Sprite) {
		if auto {
			sp.NextAuto++
		} else {
			sp.NextCheckpoint++
			// Autos stay out of the lineage: they get pruned, and history should not dangle.
			sp.Lineage = append([]string{cp.ID}, sp.Lineage...)
		}
		sp.Checkpoints = append(sp.Checkpoints, cp)
	})
	s.log.Info("checkpoint created", "sprite", sp.Name, "checkpoint", cp.ID)
	s.life.emit(sp, "checkpoint.created", map[string]any{"checkpoint": cp.ID, "auto": auto})
	return cp, nil
}

func (s *Server) deleteCheckpointLocked(name, id string, pruned bool) error {
	found := false
	sp, _ := s.store.Update(name, func(sp *store.Sprite) {
		n := len(sp.Checkpoints)
		sp.Checkpoints = slices.DeleteFunc(sp.Checkpoints, func(cp store.Checkpoint) bool { return cp.ID == id })
		found = len(sp.Checkpoints) < n
	})
	if !found {
		return errNoCheckpoint
	}
	if err := os.Remove(s.checkpointPath(sp.ID, id)); err != nil {
		return err
	}
	s.life.emit(sp, "checkpoint.deleted", map[string]any{"checkpoint": id, "pruned": pruned})
	return nil
}

// pruneAutosLocked keeps the newest AutoCheckpointKeep automatic checkpoints.
// spare, the one a restore is about to read, is neither deleted nor counted.
func (s *Server) pruneAutosLocked(name, spare string) {
	sp, err := s.store.Get(name)
	if err != nil {
		return
	}
	var autos []string
	for _, cp := range sp.Checkpoints {
		// One that is mounted inside the sprite is in use: it neither goes nor counts.
		if cp.IsAuto && cp.ID != spare && !s.checkpointMounted(sp, s.life.rt(sp.ID), cp.ID) {
			autos = append(autos, cp.ID)
		}
	}
	for i := 0; i < len(autos)-s.opts.AutoCheckpointKeep; i++ {
		if err := s.deleteCheckpointLocked(name, autos[i], true); err != nil {
			s.log.Warn("prune auto checkpoint", "sprite", name, "checkpoint", autos[i], "err", err)
			continue
		}
		s.log.Info("auto checkpoint pruned", "sprite", name, "checkpoint", autos[i])
	}
}

// autoCheckpointLocked is a no-op when autos are disabled (keep < 1).
func (s *Server) autoCheckpointLocked(rt *runtime, name, comment, spare string, info progress) error {
	if s.opts.AutoCheckpointKeep < 1 {
		return nil
	}
	if _, err := s.createCheckpointLocked(rt, name, comment, true, info); err != nil {
		return err
	}
	s.pruneAutosLocked(name, spare)
	return nil
}

// restoreCheckpointLocked replaces the live disk with a checkpoint and, if the
// sprite was running, restarts it. beforeStop runs once nothing can fail short
// of the disk copy itself, just before the VM is killed.
func (s *Server) restoreCheckpointLocked(rt *runtime, name, id string, info progress, beforeStop func()) error {
	sp, err := s.store.Get(name)
	if err != nil {
		return errors.New("sprite was deleted")
	}
	target := findCheckpoint(sp, id)
	if target == nil {
		return errNoCheckpoint
	}
	// Upstream warns that a restore discards the current state for good. A full
	// clone is cheap enough here to make every restore undoable instead.
	// Checked before anything is stopped: the copy lands beside the disk it replaces.
	if err := s.life.disk.admit(sp, "a restore", s.cloneCost(s.checkpointPath(sp.ID, id))); err != nil {
		return err
	}
	if err := s.autoCheckpointLocked(rt, name, "before restore to "+id, id, info); err != nil {
		return fmt.Errorf("save current state first: %w", err)
	}
	if beforeStop != nil {
		beforeStop()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	wasRunning := rt.m != nil
	if wasRunning {
		info("Stopping services...")
		rt.m.Kill() // the running filesystem is about to be replaced; nothing in it is worth flushing
		s.life.cleanupLocked(rt)
	}
	dir := s.store.Dir(sp.ID)
	vmm.DiscardSnapshot(dir) // memory state belongs to the filesystem being replaced
	info("Restoring filesystem...")
	if err := cloneFile(ctx, s.checkpointPath(sp.ID, id), filepath.Join(dir, vmm.DiskFile)); err != nil {
		return fmt.Errorf("restore disk: %w", err)
	}
	lineage := append([]string{id}, target.History...)
	if target.IsAuto {
		lineage = target.History
	}
	s.store.Update(name, func(sp *store.Sprite) { sp.Lineage = lineage })
	s.log.Info("checkpoint restored", "sprite", sp.Name, "checkpoint", id)
	s.life.emit(sp, "checkpoint.restored", map[string]any{"checkpoint": id})
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
	return nil
}

// autoCheckpoints is one pass of the background loop: a sprite that has run, whose disk was
// written since its newest checkpoint of any kind, and whose newest checkpoint
// is older than the interval, gets an auto. It never wakes a sprite (a
// suspended disk is cloned as it lies) and never counts as activity.
func (s *Server) autoCheckpoints() {
	every := s.opts.AutoCheckpointInterval
	for _, sp := range s.store.List("") {
		if sp.LastRunningAt == nil {
			continue
		}
		last := sp.CreatedAt
		if n := len(sp.Checkpoints); n > 0 {
			last = sp.Checkpoints[n-1].CreateTime
		}
		st, err := os.Stat(filepath.Join(s.store.Dir(sp.ID), vmm.DiskFile))
		if err != nil || time.Since(last) < every || !st.ModTime().After(last) {
			continue
		}
		rt := s.life.rt(sp.ID)
		rt.mu.Lock()
		err = s.autoCheckpointLocked(rt, sp.Name, "", "", func(string, ...any) {})
		rt.mu.Unlock()
		// A full volume is already in the log (diskguard.go); not once per sprite per tick too.
		if err != nil && !errors.Is(err, errNoRoom) {
			s.log.Warn("auto checkpoint failed", "sprite", sp.Name, "err", err)
		}
	}
}

// The handlers below take the sprite as an argument, and the in-guest channel
// the request arrived on (nil for the public API). A guest request is only
// honoured while the VM that sent it is still the running one: if the sprite
// was suspended while the request waited for the lock, nobody is listening.

var errStaleGuest = errors.New("the sprite was suspended before the request ran; retry")

func (s *Server) createCheckpoint(w http.ResponseWriter, r *http.Request, sp store.Sprite, from *guestChan) {
	var req struct {
		Comment string `json:"comment"`
	}
	json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req) // body is optional

	// Whoever holds the API token may fill this machine's disk; code inside a
	// sprite may not. Every checkpoint is a full clone, so without a ceiling a
	// loop in one guest would starve every other sprite. (Approximate under
	// concurrent creates, which is fine for a ceiling.)
	if limit := s.opts.GuestCheckpointLimit; from != nil && limit > 0 && len(filterCheckpoints(sp, "", false)) >= limit {
		s.life.emit(sp, "limit.refused", map[string]any{"limit": "guest_checkpoints", "max": limit, "current": len(filterCheckpoints(sp, "", false))})
		writeErr(w, http.StatusConflict, "checkpoint_limit", fmt.Sprintf(
			"this sprite already has %d checkpoints, the most it may create from inside; delete some first", limit))
		return
	}

	out := newNDJSON(w)
	rt := s.life.rt(sp.ID)
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if from != nil && rt.guest != from {
		out.fail("%v", errStaleGuest)
		return
	}
	cp, err := s.createCheckpointLocked(rt, sp.Name, req.Comment, false, out.info)
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
	rt := s.life.rt(sp.ID)
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if s.checkpointMounted(sp, rt, r.PathValue("id")) {
		writeErr(w, http.StatusConflict, "checkpoint_mounted", errCheckpointMounted.Error())
		return
	}
	if err := s.deleteCheckpointLocked(sp.Name, r.PathValue("id"), false); errors.Is(err, errNoCheckpoint) {
		writeErr(w, http.StatusNotFound, "not_found", "checkpoint not found")
		return
	} else if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
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

	rt := s.life.rt(sp.ID)
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if from != nil && rt.guest != from {
		out.fail("%v", errStaleGuest)
		return
	}
	var beforeStop func()
	if from != nil {
		// The requester dies with the VM, so this is the last thing it hears:
		// everything that could still be reported as a failure has passed.
		beforeStop = func() {
			out.info("Restarting the sprite from %s; this session ends now", id)
			time.Sleep(guestRestoreGrace)
		}
	}
	if err := s.restoreCheckpointLocked(rt, sp.Name, id, out.info, beforeStop); err != nil {
		if from != nil {
			s.log.Error("in-guest restore failed", "sprite", sp.Name, "checkpoint", id, "err", err)
		}
		out.fail("%v", err)
		return
	}
	out.complete("Restored to %s", id)
}
