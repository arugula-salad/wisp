package server

import (
	"context"
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

// The engine half of checkpoints (checkpoints.go has the routes and the
// format of what they stream back).
//
// The exported methods take the sprite's lock themselves. The *Locked ones
// below them are the core, shared by those methods and by the background
// loop; callers hold rt.mu.

func (l *Lifecycle) checkpointPath(id, checkpoint string) string {
	return filepath.Join(l.store.Dir(id), "checkpoints", checkpoint+".ext4")
}

// progress receives human-readable status lines while an operation runs.
type progress func(format string, a ...any)

// A request from inside a sprite carries the guest channel it arrived on (from,
// nil for the public API). It is only honoured while the VM that sent it is
// still the running one: if the sprite was suspended while the request waited
// for the lock, nobody is listening, and the answer is errStaleGuest.

// CreateCheckpoint takes a manual checkpoint of sp's disk, reporting progress
// through info.
func (l *Lifecycle) CreateCheckpoint(sp store.Sprite, from *guestChan, comment string, info progress) (store.Checkpoint, error) {
	rt := l.rt(sp.ID)
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if from != nil && rt.guest != from {
		return store.Checkpoint{}, errStaleGuest
	}
	return l.createCheckpointLocked(rt, sp.ID, comment, false, info)
}

// DeleteCheckpoint deletes one of sp's checkpoints, unless the sprite has it
// mounted (errCheckpointMounted). A missing one is errNoCheckpoint.
func (l *Lifecycle) DeleteCheckpoint(sp store.Sprite, id string) error {
	rt := l.rt(sp.ID)
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if l.checkpointMounted(sp, rt, id) {
		return errCheckpointMounted
	}
	return l.deleteCheckpointLocked(sp.ID, id, false)
}

// RestoreCheckpoint replaces sp's disk with checkpoint id; see
// restoreCheckpointLocked for info and beforeStop.
func (l *Lifecycle) RestoreCheckpoint(sp store.Sprite, from *guestChan, id string, info progress, beforeStop func()) error {
	rt := l.rt(sp.ID)
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if from != nil && rt.guest != from {
		return errStaleGuest
	}
	return l.restoreCheckpointLocked(rt, sp.ID, id, info, beforeStop)
}

// HoldCheckpoint locks sp and resolves one of its checkpoints, id or, when id
// is "", the newest manual one. The record is re-read under the lock and
// returned. Until release is called the sprite stays locked, so the checkpoint
// cannot be deleted (nor the disk restored) while its file is read, as a clone
// into a new sprite does. Errors: store.ErrNotFound, and errNoCheckpoint with
// the re-read record.
func (l *Lifecycle) HoldCheckpoint(sp store.Sprite, id string) (cur store.Sprite, checkpoint string, release func(), err error) {
	rt := l.rt(sp.ID)
	rt.mu.Lock()
	if cur, err = l.store.Get(sp.ID); err != nil {
		rt.mu.Unlock()
		return cur, "", nil, err
	}
	checkpoint = id
	if checkpoint == "" {
		if cps := filterCheckpoints(cur, "", false); len(cps) > 0 {
			checkpoint = cps[0].ID
		}
	}
	if checkpoint == "" || findCheckpoint(cur, checkpoint) == nil {
		rt.mu.Unlock()
		return cur, "", nil, errNoCheckpoint
	}
	return cur, checkpoint, rt.mu.Unlock, nil
}

// createCheckpointLocked clones the live disk. The record is re-read under the
// lock so concurrent creates get distinct IDs.
func (l *Lifecycle) createCheckpointLocked(rt *runtime, sid, comment string, auto bool, info progress) (store.Checkpoint, error) {
	sp, err := l.store.Get(sid)
	if err != nil {
		return store.Checkpoint{}, errors.New("sprite was deleted")
	}
	cp := store.Checkpoint{ID: fmt.Sprintf("v%d", sp.NextCheckpoint+1), Comment: comment, History: sp.Lineage}
	if auto {
		cp.ID, cp.IsAuto = fmt.Sprintf("auto-%d", sp.NextAuto+1), true
	}
	live := filepath.Join(l.store.Dir(sp.ID), vmm.DiskFile)
	if err := l.disk.admit(sp.Record, "a checkpoint", l.cloneCost(live)); err != nil {
		return cp, err
	}
	info("Creating checkpoint %s...", cp.ID)
	dst := l.checkpointPath(sp.ID, cp.ID)
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
			l.log.Error("resume after checkpoint failed", "sprite", sp.Name, "err", rerr)
		}
	}
	if err != nil {
		return cp, fmt.Errorf("clone disk: %w", err)
	}
	info("Filesystem captured in %s", time.Since(start).Round(time.Millisecond))
	// The moment captured, not the moment the copy finished: the background loop
	// compares this with the disk's mtime to tell whether anything changed since.
	cp.CreateTime = start.UTC()
	l.store.Update(sp.ID, func(sp *store.Sprite) {
		if auto {
			sp.NextAuto++
		} else {
			sp.NextCheckpoint++
			// Autos stay out of the lineage: they get pruned, and history should not dangle.
			sp.Lineage = append([]string{cp.ID}, sp.Lineage...)
		}
		sp.Checkpoints = append(sp.Checkpoints, cp)
	})
	l.log.Info("checkpoint created", "sprite", sp.Name, "checkpoint", cp.ID)
	l.emit(sp.Record, "checkpoint.created", map[string]any{"checkpoint": cp.ID, "auto": auto})
	return cp, nil
}

func (l *Lifecycle) deleteCheckpointLocked(sid, id string, pruned bool) error {
	found := false
	sp, _ := l.store.Update(sid, func(sp *store.Sprite) {
		n := len(sp.Checkpoints)
		sp.Checkpoints = slices.DeleteFunc(sp.Checkpoints, func(cp store.Checkpoint) bool { return cp.ID == id })
		found = len(sp.Checkpoints) < n
	})
	if !found {
		return errNoCheckpoint
	}
	if err := os.Remove(l.checkpointPath(sp.ID, id)); err != nil {
		return err
	}
	l.emit(sp.Record, "checkpoint.deleted", map[string]any{"checkpoint": id, "pruned": pruned})
	return nil
}

// pruneAutosLocked keeps the newest AutoCheckpointKeep automatic checkpoints.
// spare, the one a restore is about to read, is neither deleted nor counted.
func (l *Lifecycle) pruneAutosLocked(sid, spare string) {
	sp, err := l.store.Get(sid)
	if err != nil {
		return
	}
	var autos []string
	for _, cp := range sp.Checkpoints {
		// One that is mounted inside the sprite is in use: it neither goes nor counts.
		if cp.IsAuto && cp.ID != spare && !l.checkpointMounted(sp, l.rt(sp.ID), cp.ID) {
			autos = append(autos, cp.ID)
		}
	}
	for i := 0; i < len(autos)-l.opts.AutoCheckpointKeep; i++ {
		if err := l.deleteCheckpointLocked(sid, autos[i], true); err != nil {
			l.log.Warn("prune auto checkpoint", "sprite", sp.Name, "checkpoint", autos[i], "err", err)
			continue
		}
		l.log.Info("auto checkpoint pruned", "sprite", sp.Name, "checkpoint", autos[i])
	}
}

// autoCheckpointLocked is a no-op when autos are disabled (keep < 1).
func (l *Lifecycle) autoCheckpointLocked(rt *runtime, sid, comment, spare string, info progress) error {
	if l.opts.AutoCheckpointKeep < 1 {
		return nil
	}
	if _, err := l.createCheckpointLocked(rt, sid, comment, true, info); err != nil {
		return err
	}
	l.pruneAutosLocked(sid, spare)
	return nil
}

// restoreCheckpointLocked replaces the live disk with a checkpoint and, if the
// sprite was running, restarts it. beforeStop runs once nothing can fail short
// of the disk copy itself, just before the VM is killed.
func (l *Lifecycle) restoreCheckpointLocked(rt *runtime, sid, id string, info progress, beforeStop func()) error {
	sp, err := l.store.Get(sid)
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
	if err := l.disk.admit(sp.Record, "a restore", l.cloneCost(l.checkpointPath(sp.ID, id))); err != nil {
		return err
	}
	if err := l.autoCheckpointLocked(rt, sid, "before restore to "+id, id, info); err != nil {
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
		l.cleanupLocked(rt)
	}
	dir := l.store.Dir(sp.ID)
	vmm.DiscardSnapshot(dir) // memory state belongs to the filesystem being replaced
	info("Restoring filesystem...")
	if err := cloneFile(ctx, l.checkpointPath(sp.ID, id), filepath.Join(dir, vmm.DiskFile)); err != nil {
		return fmt.Errorf("restore disk: %w", err)
	}
	lineage := append([]string{id}, target.History...)
	if target.IsAuto {
		lineage = target.History
	}
	l.store.Update(sid, func(sp *store.Sprite) { sp.Lineage = lineage })
	l.log.Info("checkpoint restored", "sprite", sp.Name, "checkpoint", id)
	l.emit(sp.Record, "checkpoint.restored", map[string]any{"checkpoint": id})
	if wasRunning {
		// The environment restarts on its own, so services come back from the
		// restored disk without waiting for the next request.
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			if _, release, err := l.Acquire(ctx, sp); err == nil {
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
func (l *Lifecycle) autoCheckpoints() {
	every := l.opts.AutoCheckpointInterval
	for _, sp := range l.store.All() {
		if sp.LastRunningAt == nil {
			continue
		}
		last := sp.CreatedAt
		if n := len(sp.Checkpoints); n > 0 {
			last = sp.Checkpoints[n-1].CreateTime
		}
		st, err := os.Stat(filepath.Join(l.store.Dir(sp.ID), vmm.DiskFile))
		if err != nil || time.Since(last) < every || !st.ModTime().After(last) {
			continue
		}
		rt := l.rt(sp.ID)
		rt.mu.Lock()
		err = l.autoCheckpointLocked(rt, sp.ID, "", "", func(string, ...any) {})
		rt.mu.Unlock()
		// A full volume is already in the log (diskguard.go); not once per sprite per tick too.
		if err != nil && !errors.Is(err, errNoRoom) {
			l.log.Warn("auto checkpoint failed", "sprite", sp.Name, "err", err)
		}
	}
}
