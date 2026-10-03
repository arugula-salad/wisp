package vercel

import (
	"errors"
	"fmt"
	"net/http"
	"sort"

	"github.com/arugula-salad/wisp/engine"
	"github.com/arugula-salad/wisp/internal/store"
)

// Snapshots: a sandbox's filesystem at a stop or a POST …/snapshot, kept as an
// engine checkpoint of the sandbox's record. They belong to the sandbox they
// were taken of: deleting the sandbox deletes them (hosted keeps them until
// they expire unless deleteOrphanSnapshots=true).

// findSnapshot finds the sandbox a snapshot belongs to.
func (f *Frontend) findSnapshot(id string) (store.Record, meta, snapshot, bool) {
	for _, sp := range f.sandboxes() {
		m, ok := metaOf(sp.Record)
		if !ok {
			continue
		}
		if s := m.findSnapshot(id); s != nil {
			return sp.Record, m, *s, true
		}
	}
	return store.Record{}, meta{}, snapshot{}, false
}

func snapshotNotFound(w http.ResponseWriter, id string) {
	writeErr(w, http.StatusNotFound, "not_found", fmt.Sprintf("Snapshot '%s' not found.", id))
}

func (f *Frontend) getSnapshot(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	_, _, s, ok := f.findSnapshot(id)
	if !ok {
		snapshotNotFound(w, id)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"snapshot": f.snapshotOf(s, "created")})
}

// deleteSnapshot deletes the checkpoint. A sandbox whose current snapshot it
// was can no longer resume from it (unless its disk is still that state).
func (f *Frontend) deleteSnapshot(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	rec, _, s, ok := f.findSnapshot(id)
	if !ok {
		snapshotNotFound(w, id)
		return
	}
	unlock := f.lock(rec.ID)
	defer unlock()
	cur, err := f.store.GetRecord(rec.ID)
	if err != nil {
		snapshotNotFound(w, id)
		return
	}
	if err := f.life.DeleteCheckpoint(cur, s.Checkpoint); err != nil && !errors.Is(err, engine.ErrNoCheckpoint) {
		writeErr(w, http.StatusInternalServerError, "internal_server_error", "Error deleting snapshot: "+err.Error())
		return
	}
	f.updateMeta(rec.ID, func(m *meta) {
		m.Snapshots = filterSnapshots(m.Snapshots, func(x snapshot) bool { return x.ID != id })
		if m.CurrentSnapshotID == id && m.DiskIs != id {
			m.CurrentSnapshotID = ""
		}
	})
	f.log.Info("snapshot deleted", "snapshot", id)
	writeJSON(w, http.StatusOK, map[string]any{"snapshot": f.snapshotOf(s, "deleted")})
}

// listSnapshots is GET /v2/sandboxes/snapshots: the project's snapshots (of
// one sandbox with name=), newest first unless sortOrder=asc.
func (f *Frontend) listSnapshots(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit, ok := limitOf(w, q.Get("limit"), 50)
	if !ok {
		return
	}
	offset, ok := cursorOf(w, q.Get("cursor"))
	if !ok {
		return
	}
	var items []snapshot
	for _, sp := range f.sandboxes() {
		m, ok := metaOf(sp.Record)
		if !ok || (q.Get("name") != "" && m.Name != q.Get("name")) ||
			(q.Get("project") != "" && m.ProjectID != "" && m.ProjectID != q.Get("project")) {
			continue
		}
		items = append(items, m.Snapshots...)
	}
	asc := q.Get("sortOrder") == "asc"
	sort.Slice(items, func(i, j int) bool { return (items[i].CreatedAt < items[j].CreatedAt) == asc })
	page, next := paginate(len(items), offset, limit)
	out := []snapshotJSON{}
	for _, s := range items[page[0]:page[1]] {
		out = append(out, f.snapshotOf(s, "created"))
	}
	writeJSON(w, http.StatusOK, map[string]any{"snapshots": out, "pagination": paginationJSON{Count: len(out), Next: next}})
}
