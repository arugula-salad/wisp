package vercel

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"syscall"

	"github.com/arugula-salad/wisp/engine"
	"github.com/arugula-salad/wisp/internal/store"
	"github.com/arugula-salad/wisp/internal/vmm"
)

// Sessions: one run of a sandbox's VM each, addressed by session ID. A call on
// a session that is not running is 410 sandbox_stopped, on which the SDKs
// resume the sandbox (GET /v2/sandboxes/{name}?resume=true) and retry once.

// sessionHandler is a handler for one session of one sandbox.
type sessionHandler func(w http.ResponseWriter, r *http.Request, rec store.Record, m meta, s session)

// findSession finds the sandbox a session belongs to, settled.
func (f *Frontend) findSession(sid string) (store.Record, meta, session, bool) {
	look := func(id string) (store.Record, meta, *session, bool) {
		rec, err := f.store.GetRecord(id)
		m, ok := metaOf(rec)
		if err != nil || !ok {
			return rec, m, nil, false
		}
		s := m.findSession(sid)
		return rec, m, s, s != nil
	}
	if id, ok := f.sessions.Load(sid); ok {
		if rec, m, _, ok := look(id.(string)); ok {
			rec, m = f.settle(rec, m)
			return rec, m, *m.findSession(sid), true
		}
	}
	for _, sp := range f.sandboxes() {
		if rec, m, _, ok := look(sp.ID); ok {
			f.sessions.Store(sid, sp.ID)
			rec, m = f.settle(rec, m)
			return rec, m, *m.findSession(sid), true
		}
	}
	return store.Record{}, meta{}, session{}, false
}

// stopped is hosted's answer for a call on a session that is not running.
func stopped(w http.ResponseWriter) {
	writeErr(w, http.StatusGone, "sandbox_stopped", "Sandbox has stopped execution and is no longer available")
}

// stopping is hosted's answer for a call on a session on its way to stopped;
// the SDKs resume and retry on it as on 410.
func stopping(w http.ResponseWriter) {
	writeErr(w, http.StatusUnprocessableEntity, "sandbox_stopping", "Sandbox is stopping")
}

// acquireRunning holds the VM of session sid, which must be the sandbox's
// current, running session: it decides on metadata re-read under the
// sandbox's lock and takes the VM before letting go of it, so a stop (which
// marks the session stopping under the same lock before it stops the VM)
// cannot be overtaken by traffic that would wake the VM behind it. A session
// past its timeout is ended here. On failure the answer is written.
func (f *Frontend) acquireRunning(w http.ResponseWriter, ctx context.Context, id, sid string) (*vmm.Machine, func(), bool) {
	unlock := f.lock(id)
	defer unlock()
	rec, err := f.store.GetRecord(id)
	m, ok := metaOf(rec)
	if err != nil || !ok {
		writeErr(w, http.StatusNotFound, "not_found", "Sandbox not found.")
		return nil, nil, false
	}
	c := m.current()
	if c.ID == sid && c.Status == "running" && !f.now().Before(c.deadline()) {
		f.stopSessionLocked(id, c.ID, c.StartedAt+c.Timeout, false, 0)
		stopped(w)
		return nil, nil, false
	}
	if c.ID != sid || c.Status != "running" {
		if c.ID == sid && c.Status == "stopping" {
			stopping(w)
		} else {
			stopped(w)
		}
		return nil, nil, false
	}
	mach, release, err := f.acquire(ctx, rec)
	if err != nil {
		var lim *engine.LimitError
		if errors.As(err, &lim) {
			f.bootFailed(w, err)
		} else {
			f.log.Warn("could not reach a sandbox's VM", "name", m.Name, "err", err)
			stopped(w)
		}
		return nil, nil, false
	}
	return mach, release, true
}

// session looks up {sid}. With running, a session that is not the sandbox's
// current running one is 410.
func (f *Frontend) session(running bool, h sessionHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sid := r.PathValue("sid")
		rec, m, s, ok := f.findSession(sid)
		if !ok {
			writeErr(w, http.StatusNotFound, "not_found", fmt.Sprintf("Session '%s' not found.", sid))
			return
		}
		if running && s.Status == "stopping" && m.current().ID == sid {
			stopping(w)
			return
		}
		if running && (s.Status != "running" || m.current().ID != sid) {
			stopped(w)
			return
		}
		h(w, r, rec, m, s)
	}
}

// settle notices a session whose timeout has run out: the engine stops the VM
// at the deadline (the sandbox's deadline action is stop) without telling us,
// so the session is ended here, the first time anything looks, as if it had
// been stopped at its deadline (and a persistent sandbox snapshotted).
func (f *Frontend) settle(rec store.Record, m meta) (store.Record, meta) {
	c := m.current()
	if c.Status != "running" || f.now().Before(c.deadline()) {
		return rec, m
	}
	unlock := f.lock(rec.ID)
	defer unlock()
	r2, m2, _, err := f.stopSessionLocked(rec.ID, c.ID, c.StartedAt+c.Timeout, false, 0)
	if err != nil {
		f.log.Warn("ending a timed-out session failed", "name", m.Name, "err", err)
		if cur, err := f.store.GetRecord(rec.ID); err == nil {
			if cm, ok := metaOf(cur); ok {
				return cur, cm
			}
		}
		return rec, m
	}
	return r2, m2
}

// errSessionOver is a stop or snapshot of a session that, by the time the
// sandbox's lock was had, is no longer the sandbox's current running one: a
// stop or a resume got there first.
var errSessionOver = errors.New("the session is no longer running")

// errSnapshotFailed is a snapshot whose checkpoint failed: the session is
// stopped all the same (its VM is), and the snapshot does not exist.
var errSnapshotFailed = errors.New("snapshot failed")

// stopSessionLocked ends session sid, which must be the sandbox's current
// one, at stoppedAt: the VM is stopped cold, its commands end, and the disk is
// checkpointed as a new snapshot when the sandbox is persistent or this is a
// snapshot (manual, with that expiration in ms). A session that is no longer
// current, or (for a snapshot) no longer running, is errSessionOver; a stop of
// one already stopped is nil and changes nothing. Every path that marks the
// session stopping ends with it stopped, or running again (undo) when the
// VM could not be stopped. The caller holds f.lock(id).
func (f *Frontend) stopSessionLocked(id, sid string, stoppedAt int64, manual bool, expiration int64) (store.Record, meta, *snapshot, error) {
	cur, err := f.store.GetRecord(id)
	m, ok := metaOf(cur)
	if err != nil || !ok {
		return cur, m, nil, store.ErrNotFound
	}
	c := *m.current()
	if c.ID != sid || (manual && c.Status != "running") {
		return cur, m, nil, errSessionOver
	}
	if c.Status != "running" {
		return cur, m, nil, nil
	}
	now := f.ms()
	// The session is marked stopping before anything is stopped, so that
	// nothing deciding on the metadata (acquireRunning, routes, the SDKs'
	// resume) can wake the VM behind the stop; undone if the stop fails.
	if cur, m, err = f.updateMeta(id, func(m *meta) {
		m.current().Status, m.current().RequestedStopAt = "stopping", now
		m.StatusUpdatedAt = now
	}); err != nil {
		return cur, m, nil, err
	}
	undo := func(err error) (store.Record, meta, *snapshot, error) {
		cur, m, _ = f.updateMeta(id, func(m *meta) { m.current().Status = "running" })
		if end := m.current().deadline(); end.After(f.now()) {
			f.life.SetDeadline(id, &end, store.DeadlineStop)
		}
		return cur, m, nil, err
	}
	// The deadline goes next, so the engine does not act on it while this
	// stops the VM itself.
	if _, err := f.life.SetDeadline(id, nil, ""); err != nil && !errors.Is(err, engine.ErrLeaseReaping) {
		return undo(err)
	}
	if err := f.life.Stop(cur); err != nil {
		return undo(err)
	}
	f.cmds.endSession(c.ID)
	// From here the VM is stopped: whatever happens, the session ends stopped.
	var snap *snapshot
	var snapErr error
	if m.Persistent || manual {
		comment := "vercel: stop of session " + c.ID
		method := "stop"
		if manual {
			comment, method = "vercel: snapshot of session "+c.ID, "manual"
		}
		cp, err := f.checkpoint(cur, comment)
		if err != nil {
			f.log.Error("snapshot failed", "name", m.Name, "err", err)
			if manual {
				snapErr = fmt.Errorf("%w: %v", errSnapshotFailed, err)
			}
		} else {
			s := snapshot{ID: newSnapshotID(), Checkpoint: cp.ID, SourceSessionID: c.ID, CreatedAt: now, LastUsedAt: now,
				SizeBytes: f.checkpointBytes(id, cp.ID), Method: method}
			if expiration > 0 {
				s.ExpiresAt = now + expiration
			}
			snap = &s
		}
	}
	var drop []snapshot
	cur, m, err = f.updateMeta(id, func(m *meta) {
		c := m.current()
		c.Status, c.StoppedAt = "stopped", max(c.StartedAt, min(stoppedAt, now))
		m.TotalDurationMs += c.StoppedAt - c.StartedAt
		m.StatusUpdatedAt = now
		m.DiskIs = ""
		if snap != nil {
			if manual {
				c.SnapshottedAt = now
			} else {
				// A persistent sandbox keeps the snapshot of its last stop, not every one.
				for _, s := range m.Snapshots {
					if s.Method == "stop" {
						drop = append(drop, s)
					}
				}
				m.Snapshots = filterSnapshots(m.Snapshots, func(s snapshot) bool { return s.Method != "stop" })
			}
			m.Snapshots = append(m.Snapshots, *snap)
			m.CurrentSnapshotID, m.DiskIs = snap.ID, snap.ID
		}
	})
	for _, s := range drop {
		if err := f.life.DeleteCheckpoint(cur, s.Checkpoint); err != nil && !errors.Is(err, engine.ErrNoCheckpoint) {
			f.log.Warn("could not delete a superseded stop snapshot", "name", m.Name, "snapshot", s.ID, "err", err)
		}
	}
	f.log.Info("session stopped", "name", m.Name, "session", c.ID, "snapshot", snap != nil)
	if err == nil {
		err = snapErr
	}
	return cur, m, snap, err
}

func filterSnapshots(in []snapshot, keep func(snapshot) bool) []snapshot {
	out := in[:0:0]
	for _, s := range in {
		if keep(s) {
			out = append(out, s)
		}
	}
	return out
}

// checkpointBytes is the space a checkpoint takes (its allocated blocks; on a
// reflink volume much of it is shared). The engine keeps a sandbox's
// checkpoints under its machine directory.
func (f *Frontend) checkpointBytes(id, cp string) int64 {
	fi, err := os.Stat(filepath.Join(f.store.Dir(id), "checkpoints", cp+".ext4"))
	if err != nil {
		return 0
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return st.Blocks * 512
	}
	return fi.Size()
}

// resume starts a new session on a stopped sandbox: on its disk as it was left
// (a persistent sandbox, or one whose disk is still its last snapshot), or on
// its last snapshot restored. A non-persistent sandbox that never had a
// snapshot cannot be resumed: 400 snapshot_not_found, as hosted answers.
// status is 0 on success, else the error to answer.
func (f *Frontend) resume(r *http.Request, id string) (rec store.Record, m meta, status int, code, msg string) {
	unlock := f.lock(id)
	defer unlock()
	rec, err := f.store.GetRecord(id)
	m, ok := metaOf(rec)
	if err != nil || !ok {
		return rec, m, http.StatusNotFound, "not_found", "Sandbox not found."
	}
	if m.current().Status == "running" {
		return rec, m, 0, "", ""
	}
	source := m.DiskIs
	switch {
	case m.DiskIs != "" || m.Persistent:
	case m.CurrentSnapshotID != "":
		s := m.findSnapshot(m.CurrentSnapshotID)
		if s == nil {
			return rec, m, http.StatusBadRequest, "snapshot_not_found", "Cannot resume sandbox: no snapshot available."
		}
		if err := f.life.RestoreCheckpoint(rec, nil, s.Checkpoint, func(string, ...any) {}, nil); err != nil {
			f.log.Error("restoring a snapshot to resume failed", "name", m.Name, "snapshot", s.ID, "err", err)
			return rec, m, http.StatusInternalServerError, "internal_server_error", "Failed to restore the sandbox's snapshot: " + err.Error()
		}
		source = s.ID
	default:
		return rec, m, http.StatusBadRequest, "snapshot_not_found", "Cannot resume sandbox: no snapshot available."
	}
	now := f.ms()
	sess := session{ID: newSessionID(), RequestedAt: now, StartedAt: now, Timeout: m.Timeout, Status: "running", SourceSnapshotID: source}
	if rec, m, err = f.updateMeta(id, func(m *meta) {
		m.addSession(sess)
		m.DiskIs = ""
		m.StatusUpdatedAt = now
		if s := m.findSnapshot(source); s != nil {
			s.LastUsedAt = now
		}
	}); err != nil {
		return rec, m, http.StatusNotFound, "not_found", "Sandbox not found."
	}
	f.sessions.Store(sess.ID, id)
	end := sess.deadline()
	if _, err := f.life.SetDeadline(id, &end, store.DeadlineStop); err != nil {
		return rec, m, http.StatusInternalServerError, "internal_server_error", err.Error()
	}
	_, release, err := f.acquire(r.Context(), rec)
	if err != nil {
		// The session never ran: it is over before it began.
		f.life.SetDeadline(id, nil, "")
		rec, m, _ = f.updateMeta(id, func(m *meta) {
			c := m.current()
			c.Status, c.StoppedAt = "stopped", f.ms()
		})
		var lim *engine.LimitError
		if errors.As(err, &lim) {
			return rec, m, http.StatusTooManyRequests, "too_many_requests", lim.Message
		}
		return rec, m, http.StatusInternalServerError, "internal_server_error", "Failed to start sandbox: " + err.Error()
	}
	release()
	started := f.ms()
	rec, m, _ = f.updateMeta(id, func(m *meta) { m.current().StartedAt = started })
	end = m.current().deadline()
	f.life.SetDeadline(id, &end, store.DeadlineStop)
	f.log.Info("sandbox resumed", "name", m.Name, "session", sess.ID, "from", source)
	return rec, m, 0, "", ""
}

func (f *Frontend) getSession(w http.ResponseWriter, r *http.Request, rec store.Record, m meta, s session) {
	writeJSON(w, http.StatusOK, map[string]any{"session": f.sessionOf(rec, m, s), "routes": f.routesOf(m)})
}

// stop is POST …/stop: it returns once the session has stopped (and a
// persistent sandbox has been snapshotted). Stopping a stopped session is
// the same answer again.
func (f *Frontend) stop(w http.ResponseWriter, r *http.Request, rec store.Record, m meta, s session) {
	var snap *snapshot
	// One already stopping waits for that stop (its lock) and answers its outcome.
	if (s.Status == "running" || s.Status == "stopping") && m.current().ID == s.ID {
		unlock := f.lock(rec.ID)
		var err error
		rec, m, snap, err = f.stopSessionLocked(rec.ID, s.ID, f.ms(), false, 0)
		unlock()
		// One a resume replaced meanwhile is stopped: the answer is that session's.
		if errors.Is(err, errSessionOver) {
			err = nil
		}
		if errors.Is(err, store.ErrNotFound) {
			writeErr(w, http.StatusNotFound, "not_found", "Sandbox not found.")
			return
		} else if err != nil {
			writeErr(w, http.StatusInternalServerError, "internal_server_error", "Error stopping sandbox: "+err.Error())
			return
		}
		if p := m.findSession(s.ID); p != nil {
			s = *p
		}
	}
	out := map[string]any{"sandbox": f.sandboxOf(m, true), "session": f.sessionOf(rec, m, s)}
	if snap != nil {
		out["snapshot"] = f.snapshotOf(*snap, "created")
	}
	writeJSON(w, http.StatusOK, out)
}

// extend is POST …/extend-timeout: the session's timeout grows by duration
// ms; the sandbox's own timeout (each new session's) is unchanged.
func (f *Frontend) extend(w http.ResponseWriter, r *http.Request, rec store.Record, m meta, s session) {
	var req struct {
		Duration *int64 `json:"duration"`
	}
	if !readBody(w, r, &req) {
		return
	}
	if req.Duration == nil || *req.Duration <= 0 {
		writeErr(w, http.StatusBadRequest, "bad_request", "Invalid request: `duration` must be a positive number of milliseconds.")
		return
	}
	unlock := f.lock(rec.ID)
	defer unlock()
	cur, err := f.store.GetRecord(rec.ID)
	m, ok := metaOf(cur)
	if err != nil || !ok {
		writeErr(w, http.StatusNotFound, "not_found", "Sandbox not found.")
		return
	}
	c := m.current()
	if c.ID != s.ID || c.Status != "running" {
		stopped(w)
		return
	}
	// In ms, and without the sum: a huge duration must not overflow past the check.
	maxMs := f.opts.MaxTimeout.Milliseconds()
	total := c.Timeout + *req.Duration
	if *req.Duration > maxMs || c.Timeout > maxMs-*req.Duration {
		writeErr(w, http.StatusBadRequest, "bad_request",
			fmt.Sprintf("Invalid request: the session's timeout cannot exceed %d ms.", f.opts.MaxTimeout.Milliseconds()))
		return
	}
	rec, m, err = f.updateMeta(rec.ID, func(m *meta) { m.current().Timeout = total })
	if err == nil {
		end := m.current().deadline()
		_, err = f.life.SetDeadline(rec.ID, &end, store.DeadlineStop)
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal_server_error", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"session": f.sessionOf(rec, m, *m.current())})
}

// snapshot is POST …/snapshot: the session stops and its filesystem becomes
// a snapshot (an engine checkpoint of the disk), which later sessions of this
// sandbox resume from and other sandboxes can be created from. 201, with the
// session reported as snapshotting, as hosted reports it.
func (f *Frontend) snapshot(w http.ResponseWriter, r *http.Request, rec store.Record, m meta, s session) {
	var req struct {
		Expiration *int64 `json:"expiration"`
	}
	if !readBody(w, r, &req) {
		return
	}
	var exp int64
	if req.Expiration != nil {
		exp = *req.Expiration
		if exp != 0 && exp < 86400000 {
			writeErr(w, http.StatusBadRequest, "bad_request", "`expiration` must be 0 (no expiration) or >= 86400000 ms (1 day).")
			return
		}
	}
	unlock := f.lock(rec.ID)
	rec, m, snap, err := f.stopSessionLocked(rec.ID, s.ID, f.ms(), true, exp)
	unlock()
	switch {
	case errors.Is(err, errSessionOver):
		stopped(w)
		return
	case errors.Is(err, store.ErrNotFound):
		writeErr(w, http.StatusNotFound, "not_found", "Sandbox not found.")
		return
	case err != nil || snap == nil:
		// The session is stopped either way (a failed checkpoint leaves it
		// stopped with no snapshot); the snapshot is what failed.
		msg := "snapshot failed"
		if err != nil {
			msg = err.Error()
		}
		writeErr(w, http.StatusInternalServerError, "internal_server_error", "Error snapshotting sandbox: "+msg)
		return
	}
	sj := f.sessionOf(rec, m, *m.findSession(s.ID))
	sj.Status, sj.StoppedAt, sj.RequestedStopAt, sj.Duration, sj.ActiveCPUDurationMs, sj.NetworkTransfer = "snapshotting", 0, 0, nil, nil, nil
	writeJSON(w, http.StatusCreated, map[string]any{"snapshot": f.snapshotOf(*snap, "created"), "session": sj})
}

// listSessions is GET /v2/sandboxes/sessions: the sessions of one sandbox
// (name=) or of all of them, newest first unless sortOrder=asc.
func (f *Frontend) listSessions(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit, ok := limitOf(w, q.Get("limit"), 50)
	if !ok {
		return
	}
	offset, ok := cursorOf(w, q.Get("cursor"))
	if !ok {
		return
	}
	type item struct {
		rec store.Record
		m   meta
		s   session
	}
	var items []item
	for _, sp := range f.sandboxes() {
		m, ok := metaOf(sp.Record)
		if !ok || (q.Get("name") != "" && m.Name != q.Get("name")) ||
			(q.Get("project") != "" && m.ProjectID != "" && m.ProjectID != q.Get("project")) {
			continue
		}
		rec, m := f.settle(sp.Record, m)
		for _, s := range m.Sessions {
			items = append(items, item{rec, m, s})
		}
	}
	asc := q.Get("sortOrder") == "asc"
	sort.Slice(items, func(i, j int) bool {
		return ordered(items[i].s.RequestedAt, items[j].s.RequestedAt, items[i].s.ID, items[j].s.ID, asc)
	})
	page, next := paginate(len(items), offset, limit)
	out := []sessionJSON{}
	for _, it := range items[page[0]:page[1]] {
		out = append(out, f.sessionOf(it.rec, it.m, it.s))
	}
	writeJSON(w, http.StatusOK, map[string]any{"sessions": out, "pagination": paginationJSON{Count: len(out), Next: next}})
}
