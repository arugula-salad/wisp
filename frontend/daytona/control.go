package daytona

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/arugula-salad/wisp/engine"
	"github.com/arugula-salad/wisp/internal/store"
)

// The control plane: Daytona's REST API under /api, as the SDKs use it
// (docs/providers/daytona.md section 4). Shapes are the generated API
// client's models (daytona_api_client), which validate strictly: every
// required field is present and every integer is an integer.

func (f *Frontend) controlPlane() http.Handler {
	mux := http.NewServeMux()
	// The SDK's Socket.IO event stream: answering 404 makes it fall back to
	// polling GET /sandbox/{id}, which is all this server offers.
	mux.HandleFunc("/socket.io/", func(w http.ResponseWriter, r *http.Request) {
		writeErr(w, r, http.StatusNotFound, "", "This Daytona-compatible server has no event stream; poll GET /sandbox/{id}")
	})
	mux.HandleFunc("POST /sandbox", f.auth(true, f.create))
	mux.HandleFunc("GET /sandbox", f.auth(false, f.list))
	mux.HandleFunc("GET /sandbox/{id}", f.auth(false, f.sandbox(f.get)))
	mux.HandleFunc("DELETE /sandbox/{id}", f.auth(true, f.sandbox(f.del)))
	mux.HandleFunc("POST /sandbox/{id}/start", f.auth(true, f.sandbox(f.start)))
	mux.HandleFunc("POST /sandbox/{id}/stop", f.auth(true, f.sandbox(f.stopSandbox)))
	mux.HandleFunc("GET /sandbox/{id}/toolbox-proxy-url", f.auth(false, f.sandbox(f.toolboxProxyURL)))
	mux.HandleFunc("GET /sandbox/{id}/ports/{port}/preview-url", f.auth(false, f.sandbox(f.previewURL)))
	mux.HandleFunc("PUT /sandbox/{id}/labels", f.auth(true, f.sandbox(f.setLabels)))
	mux.HandleFunc("POST /sandbox/{id}/autostop/{interval}", f.auth(true, f.sandbox(f.setAutoStop)))
	mux.HandleFunc("POST /sandbox/{id}/autoarchive/{interval}", f.auth(true, f.sandbox(f.setInterval(func(m *meta, n int) { m.AutoArchive = n }))))
	mux.HandleFunc("POST /sandbox/{id}/autodelete/{interval}", f.auth(true, f.sandbox(f.setInterval(func(m *meta, n int) { m.AutoDelete = n }))))
	mux.HandleFunc("POST /sandbox/{id}/last-activity", f.auth(true, f.sandbox(func(w http.ResponseWriter, r *http.Request, rec store.Record, m meta) {
		f.life.BeginUse(rec.ID)() // counts as activity, without waking it
		w.WriteHeader(http.StatusCreated)
	})))
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeErr(w, r, http.StatusNotFound, "", "Not found: this Daytona-compatible server does not implement "+r.Method+" /api"+r.URL.Path)
	})
	return mux
}

// errorJSON is the error envelope both planes answer with: the toolbox's
// ErrorResponse (message, statusCode, path, timestamp required), with the
// source that tells the SDK which plane it came from, and a code it maps to a
// typed error where there is one (FILE_NOT_FOUND, ...).
type errorJSON struct {
	StatusCode int    `json:"statusCode"`
	Message    string `json:"message"`
	Error      string `json:"error"`
	Code       string `json:"code,omitempty"`
	Source     string `json:"source"`
	Path       string `json:"path"`
	Method     string `json:"method"`
	Timestamp  string `json:"timestamp"`
}

func writeErr(w http.ResponseWriter, r *http.Request, status int, code, msg string) {
	source := "DAYTONA_API"
	if strings.HasPrefix(r.URL.Path, "/toolbox/") {
		source = "DAYTONA_DAEMON"
	}
	writeJSON(w, status, errorJSON{StatusCode: status, Message: msg, Error: http.StatusText(status), Code: code,
		Source: source, Path: r.URL.Path, Method: r.Method, Timestamp: stamp(time.Now())})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func stamp(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05.000Z") }

// bearer is the request's API key.
func bearer(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if len(h) > 7 && strings.EqualFold(h[:7], "Bearer ") {
		return strings.TrimSpace(h[7:])
	}
	return ""
}

// checkKey authenticates r's bearer key: ok for one the daemon accepts, admin
// for one that may change things.
func (f *Frontend) checkKey(r *http.Request) (admin, ok bool) {
	if key := bearer(r); key != "" && f.opts.CheckKey != nil {
		return f.opts.CheckKey(key)
	}
	return false, false
}

// auth checks the bearer key against the daemon's keys. A read key may look
// but not change anything, as on the Sprites API.
func (f *Frontend) auth(write bool, h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		admin, ok := f.checkKey(r)
		if !ok {
			writeErr(w, r, http.StatusUnauthorized, "UNAUTHORIZED", "Invalid API key: use this daemon's root token or one of its API keys (wispd keys)")
			return
		}
		if write && !admin {
			writeErr(w, r, http.StatusForbidden, "FORBIDDEN", "This API key is read-only")
			return
		}
		h(w, r)
	}
}

// find looks a sandbox up by ID or by name.
func (f *Frontend) find(idOrName string) (store.Record, meta, bool) {
	if rec, err := f.store.GetRecord(idOrName); err == nil {
		if m, ok := metaOf(rec); ok {
			return rec, m, true
		}
	}
	for _, sp := range f.store.All() {
		if m, ok := metaOf(sp.Record); ok && m.Name == idOrName {
			return sp.Record, m, true
		}
	}
	return store.Record{}, meta{}, false
}

func notFound(w http.ResponseWriter, r *http.Request, idOrName string) {
	writeErr(w, r, http.StatusNotFound, "NOT_FOUND", fmt.Sprintf("Sandbox with ID or name %s not found", idOrName))
}

// sandbox looks up {id} as a Daytona sandbox, by ID or name.
func (f *Frontend) sandbox(h func(http.ResponseWriter, *http.Request, store.Record, meta)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		rec, m, ok := f.find(r.PathValue("id"))
		if !ok {
			notFound(w, r, r.PathValue("id"))
			return
		}
		h(w, r, rec, m)
	}
}

// readBody decodes an optional JSON body into v: an empty body leaves v as it is.
func readBody(w http.ResponseWriter, r *http.Request, v any) bool {
	b, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err == nil && len(strings.TrimSpace(string(b))) > 0 {
		err = json.Unmarshal(b, v)
	}
	if err != nil {
		writeErr(w, r, http.StatusBadRequest, "INVALID_REQUEST_BODY", "Invalid request body: "+err.Error())
		return false
	}
	return true
}

// state is the sandbox's Daytona state: stopped through the API, or with
// its VM down for any other reason (auto-stop, a daemon restart), is
// "stopped"; a VM in a transition is taken for running, because the only
// transitions this front end starts it waits out before answering.
func (f *Frontend) state(rec store.Record, m meta) string {
	if m.Stopped {
		return "stopped"
	}
	if up, busy := f.running(rec.ID); up || busy {
		return "started"
	}
	return "stopped"
}

// sandboxJSON is Sandbox (and, with the same fields, SandboxListItem).
type sandboxJSON struct {
	ID                  string            `json:"id"`
	OrganizationID      string            `json:"organizationId"`
	Name                string            `json:"name"`
	Snapshot            string            `json:"snapshot"`
	User                string            `json:"user"`
	Env                 map[string]string `json:"env"`
	Labels              map[string]string `json:"labels"`
	Public              bool              `json:"public"`
	NetworkBlockAll     bool              `json:"networkBlockAll"`
	NetworkAllowList    *string           `json:"networkAllowList,omitempty"`
	Kvm                 bool              `json:"kvm"`
	Target              string            `json:"target"`
	CPU                 int               `json:"cpu"`
	GPU                 int               `json:"gpu"`
	Memory              int               `json:"memory"`
	Disk                int               `json:"disk"`
	State               string            `json:"state"`
	DesiredState        string            `json:"desiredState"`
	ErrorReason         *string           `json:"errorReason"`
	Recoverable         bool              `json:"recoverable"`
	BackupState         string            `json:"backupState"`
	AutoStopInterval    int               `json:"autoStopInterval"`
	AutoArchiveInterval int               `json:"autoArchiveInterval"`
	AutoDeleteInterval  int               `json:"autoDeleteInterval"`
	AutoPauseInterval   *int              `json:"autoPauseInterval,omitempty"`
	Volumes             []any             `json:"volumes"`
	CreatedAt           string            `json:"createdAt"`
	UpdatedAt           string            `json:"updatedAt"`
	LastActivityAt      string            `json:"lastActivityAt,omitempty"`
	DaemonVersion       string            `json:"daemonVersion"`
	RunnerID            string            `json:"runnerId"`
	ToolboxProxyURL     string            `json:"toolboxProxyUrl"`
}

func (f *Frontend) dto(r *http.Request, rec store.Record, m meta, state string) sandboxJSON {
	desired := state
	if state != "started" && state != "stopped" {
		desired = "destroyed"
	}
	d := sandboxJSON{ID: rec.ID, OrganizationID: organizationID, Name: m.Name, Snapshot: m.Snapshot, User: m.User,
		Env: m.Env, Labels: m.Labels, Public: m.Public, NetworkBlockAll: m.NetworkBlockAll, Target: m.Target,
		CPU: m.CPU, GPU: m.GPU, Memory: m.MemGiB, Disk: f.diskGiB, State: state, DesiredState: desired,
		BackupState: "None", AutoStopInterval: m.AutoStop, AutoArchiveInterval: m.AutoArchive,
		AutoDeleteInterval: m.AutoDelete, AutoPauseInterval: m.AutoPause, Volumes: []any{},
		CreatedAt: stamp(rec.CreatedAt), UpdatedAt: stamp(rec.UpdatedAt), DaemonVersion: DaemonVersion,
		RunnerID: "wisp", ToolboxProxyURL: f.toolboxBase(r)}
	if d.Env == nil {
		d.Env = map[string]string{}
	}
	if d.Labels == nil {
		d.Labels = map[string]string{}
	}
	if m.NetworkAllowList != "" {
		d.NetworkAllowList = &m.NetworkAllowList
	}
	if rec.LastRunningAt != nil {
		d.LastActivityAt = stamp(*rec.LastRunningAt)
	}
	return d
}

// createReq is CreateSandbox, the parts of it this server honours or keeps.
type createReq struct {
	Name                string            `json:"name"`
	Snapshot            string            `json:"snapshot"`
	User                string            `json:"user"`
	Env                 map[string]string `json:"env"`
	Labels              map[string]string `json:"labels"`
	Public              bool              `json:"public"`
	NetworkBlockAll     bool              `json:"networkBlockAll"`
	NetworkAllowList    string            `json:"networkAllowList"`
	Target              string            `json:"target"`
	CPU                 *int              `json:"cpu"`
	GPU                 *int              `json:"gpu"`
	Memory              *int              `json:"memory"`
	Disk                *int              `json:"disk"`
	AutoStopInterval    *int              `json:"autoStopInterval"`
	AutoArchiveInterval *int              `json:"autoArchiveInterval"`
	AutoDeleteInterval  *int              `json:"autoDeleteInterval"`
	AutoPauseInterval   *int              `json:"autoPauseInterval"`
	BuildInfo           json.RawMessage   `json:"buildInfo"`
	Volumes             []json.RawMessage `json:"volumes"`
}

func (f *Frontend) create(w http.ResponseWriter, r *http.Request) {
	var req createReq
	if !readBody(w, r, &req) {
		return
	}
	if len(req.BuildInfo) > 0 && string(req.BuildInfo) != "null" {
		writeErr(w, r, http.StatusBadRequest, "BAD_REQUEST", "Creating a sandbox from an image is not supported by this server: create it from the default snapshot")
		return
	}
	if len(req.Volumes) > 0 {
		writeErr(w, r, http.StatusBadRequest, "BAD_REQUEST", "Volumes are not supported by this server")
		return
	}
	if req.Snapshot != "" && req.Snapshot != defaultSnapshot {
		writeErr(w, r, http.StatusNotFound, "NOT_FOUND", fmt.Sprintf("Snapshot %s not found. This server has one snapshot: %q, the default", req.Snapshot, defaultSnapshot))
		return
	}
	if req.User != "" && req.User != defaultUser {
		writeErr(w, r, http.StatusBadRequest, "BAD_REQUEST", fmt.Sprintf("User %q is not supported: sandboxes run as %q", req.User, defaultUser))
		return
	}
	if req.GPU != nil && *req.GPU > 0 {
		writeErr(w, r, http.StatusBadRequest, "BAD_REQUEST", "GPUs are not supported by this server")
		return
	}
	m := meta{Name: req.Name, Labels: req.Labels, Env: req.Env, User: defaultUser, Public: req.Public,
		Target: req.Target, Snapshot: defaultSnapshot, CPU: defaultCPU, MemGiB: defaultMemGiB,
		AutoStop: defaultAutoStop, AutoArchive: 7 * 24 * 60, AutoDelete: -1, AutoPause: req.AutoPauseInterval,
		NetworkBlockAll: req.NetworkBlockAll, NetworkAllowList: req.NetworkAllowList, PreviewToken: newToken()}
	if m.Target == "" {
		m.Target = defaultTarget
	}
	for _, v := range []struct {
		in   *int
		out  *int
		max  int
		what string
	}{{req.CPU, &m.CPU, f.opts.MaxCPU, "cpu"}, {req.Memory, &m.MemGiB, f.opts.MaxMemGiB, "memory"}} {
		if v.in == nil {
			continue
		}
		if *v.in < 1 || (v.max > 0 && *v.in > v.max) {
			writeErr(w, r, http.StatusBadRequest, "BAD_REQUEST", fmt.Sprintf("%s must be between 1 and %d", v.what, max(v.max, *v.in)))
			return
		}
		*v.out = *v.in
	}
	if req.Disk != nil && f.diskGiB > 0 && *req.Disk > f.diskGiB {
		writeErr(w, r, http.StatusBadRequest, "BAD_REQUEST", fmt.Sprintf("disk must be at most %d GiB on this server", f.diskGiB))
		return
	}
	for _, v := range []struct {
		in  *int
		out *int
	}{{req.AutoStopInterval, &m.AutoStop}, {req.AutoArchiveInterval, &m.AutoArchive}, {req.AutoDeleteInterval, &m.AutoDelete}} {
		if v.in != nil {
			*v.out = *v.in
		}
	}
	if m.AutoStop < 0 {
		writeErr(w, r, http.StatusBadRequest, "BAD_REQUEST", "autoStopInterval must not be negative")
		return
	}
	if m.AutoPause != nil && *m.AutoPause > 0 {
		// Pause (keeping memory) is not offered; an auto-pause is taken for an auto-stop.
		m.AutoStop = 0
	}

	now := time.Now().UTC()
	id := newSandboxID()
	if m.Name == "" {
		m.Name = id
	}
	policy := idlePolicy(m.AutoStop)
	sp := store.Sprite{Record: store.Record{ID: id, API: API, Hostname: hostname, CreatedAt: now, UpdatedAt: now,
		Config: store.Config{CPUs: m.CPU, RamMB: m.MemGiB * 1024}, Lifecycle: &policy}}
	setMeta(&sp.Record, m)
	f.names.Lock()
	if limit, n := f.opts.MaxSandboxes, f.store.Count(); limit > 0 && n >= limit {
		f.names.Unlock()
		writeErr(w, r, http.StatusTooManyRequests, "", fmt.Sprintf("You have reached the maximum number of sandboxes (%d) on this server", limit))
		return
	}
	if _, _, taken := f.find(m.Name); taken {
		f.names.Unlock()
		writeErr(w, r, http.StatusConflict, "CONFLICT", fmt.Sprintf("Sandbox with name %s already exists", m.Name))
		return
	}
	created, err := f.life.Create(r.Context(), engine.CreateSpec{Sprite: sp, ImageDisk: f.opts.Disk})
	f.names.Unlock()
	if errors.Is(err, engine.ErrNoRoom) {
		writeErr(w, r, http.StatusInsufficientStorage, "", err.Error())
		return
	}
	if err != nil {
		f.log.Error("create failed", "err", err)
		writeErr(w, r, http.StatusInternalServerError, "", "Failed to create sandbox: "+err.Error())
		return
	}
	// The SDK waits for "started": boot it now, so the answer already says so.
	_, release, err := f.acquire(r.Context(), created.Record)
	if err != nil {
		f.life.Delete(created.Record)
		f.bootFailed(w, r, err)
		return
	}
	release()
	f.log.Info("sandbox created", "id", id, "name", m.Name, "autostop", m.AutoStop)
	writeJSON(w, http.StatusOK, f.dto(r, created.Record, m, "started"))
}

// bootFailed answers a start the engine refused or could not complete.
func (f *Frontend) bootFailed(w http.ResponseWriter, r *http.Request, err error) {
	var lim *engine.LimitError
	if errors.As(err, &lim) {
		if lim.RetryAfter > 0 {
			w.Header().Set("Retry-After", strconv.Itoa(lim.RetryAfter))
		}
		writeErr(w, r, http.StatusTooManyRequests, "", lim.Message)
		return
	}
	f.log.Error("sandbox failed to start", "err", err)
	writeErr(w, r, http.StatusInternalServerError, "", "Failed to start sandbox: "+err.Error())
}

func (f *Frontend) get(w http.ResponseWriter, r *http.Request, rec store.Record, m meta) {
	writeJSON(w, http.StatusOK, f.dto(r, rec, m, f.state(rec, m)))
}

func (f *Frontend) del(w http.ResponseWriter, r *http.Request, rec store.Record, m meta) {
	defer f.lock(rec.ID)()
	if err := f.life.Delete(rec); errors.Is(err, store.ErrNotFound) {
		notFound(w, r, rec.ID)
		return
	} else if err != nil {
		writeErr(w, r, http.StatusInternalServerError, "", "Failed to delete sandbox: "+err.Error())
		return
	}
	f.forget(rec.ID)
	f.log.Info("sandbox deleted", "id", rec.ID)
	writeJSON(w, http.StatusOK, f.dto(r, rec, m, "destroyed"))
}

// start boots a stopped sandbox afresh (or answers at once for a started
// one): the SDK then waits for "started", which this already is.
func (f *Frontend) start(w http.ResponseWriter, r *http.Request, rec store.Record, m meta) {
	defer f.lock(rec.ID)()
	cur, err := f.store.GetRecord(rec.ID)
	if _, ok := metaOf(cur); err != nil || !ok {
		notFound(w, r, rec.ID)
		return
	}
	_, release, err := f.acquire(r.Context(), cur)
	if err != nil {
		f.bootFailed(w, r, err)
		return
	}
	release()
	cur, m, err = f.updateMeta(rec.ID, func(m *meta) { m.Stopped = false })
	if err != nil {
		notFound(w, r, rec.ID)
		return
	}
	f.log.Info("sandbox started", "id", rec.ID)
	writeJSON(w, http.StatusOK, f.dto(r, cur, m, "started"))
}

// stopSandbox stops the VM cold, keeping its disk; its processes and
// sessions end. Toolbox and preview traffic do not wake it: only a start does.
// An ephemeral sandbox (auto-delete 0) is deleted here and then, and answered
// as destroyed, which the SDKs' stop takes for stopped.
func (f *Frontend) stopSandbox(w http.ResponseWriter, r *http.Request, rec store.Record, m meta) {
	defer f.lock(rec.ID)()
	// Stopped first, under the gate, so that no toolbox or preview request can
	// wake it on its way down; the metadata is re-read in the same write.
	g := f.gate(rec.ID)
	g.Lock()
	cur, m, err := f.updateMeta(rec.ID, func(m *meta) { m.Stopped = true })
	g.Unlock()
	if err != nil {
		notFound(w, r, rec.ID)
		return
	}
	if m.AutoDelete == 0 {
		if err := f.life.Delete(cur); err != nil && !errors.Is(err, store.ErrNotFound) {
			writeErr(w, r, http.StatusInternalServerError, "", "Failed to delete ephemeral sandbox: "+err.Error())
			return
		}
		f.forget(rec.ID)
		f.log.Info("ephemeral sandbox deleted on stop", "id", rec.ID)
		writeJSON(w, http.StatusOK, f.dto(r, cur, m, "destroyed"))
		return
	}
	f.sessions.dropSandbox(rec.ID)
	if err := f.stop(cur); err != nil {
		f.log.Error("stop failed", "id", rec.ID, "err", err)
		writeErr(w, r, http.StatusInternalServerError, "", "Failed to stop sandbox: "+err.Error())
		return
	}
	f.log.Info("sandbox stopped", "id", rec.ID)
	writeJSON(w, http.StatusOK, f.dto(r, cur, m, "stopped"))
}

// onEvent deletes an ephemeral sandbox (auto-delete interval 0) once its
// auto-stop has stopped it (an API stop deletes it itself). Called with the
// bus locked, so the deletion runs on its own.
func (f *Frontend) onEvent(e engine.Event) {
	if e.Type != "sprite.stopped" || e.SpriteID == "" {
		return
	}
	go func() {
		rec, err := f.store.GetRecord(e.SpriteID)
		m, ok := metaOf(rec)
		if err != nil || !ok || m.AutoDelete != 0 {
			return
		}
		defer f.lock(rec.ID)()
		if err := f.life.Delete(rec); err == nil {
			f.forget(rec.ID)
			f.log.Info("ephemeral sandbox deleted on stop", "id", rec.ID)
		}
	}()
}

func (f *Frontend) toolboxProxyURL(w http.ResponseWriter, r *http.Request, rec store.Record, m meta) {
	writeJSON(w, http.StatusOK, map[string]string{"url": f.toolboxBase(r)})
}

// previewURL is a port's preview URL, <port>-<id>.<domain> on this listener,
// and the token a private sandbox's preview wants (x-daytona-preview-token).
func (f *Frontend) previewURL(w http.ResponseWriter, r *http.Request, rec store.Record, m meta) {
	port, err := strconv.Atoi(r.PathValue("port"))
	if err != nil || port < 1 || port > 65535 {
		writeErr(w, r, http.StatusBadRequest, "BAD_REQUEST", "Invalid port: "+r.PathValue("port"))
		return
	}
	scheme := "http"
	if strings.HasPrefix(f.opts.BaseURL, "https://") {
		scheme = "https"
	}
	writeJSON(w, http.StatusOK, map[string]string{"sandboxId": rec.ID,
		"url": fmt.Sprintf("%s://%d-%s.%s", scheme, port, rec.ID, f.opts.Domain), "token": m.PreviewToken})
}

func (f *Frontend) setLabels(w http.ResponseWriter, r *http.Request, rec store.Record, m meta) {
	var req struct {
		Labels map[string]string `json:"labels"`
	}
	if !readBody(w, r, &req) {
		return
	}
	if req.Labels == nil {
		writeErr(w, r, http.StatusBadRequest, "BAD_REQUEST", "labels is required")
		return
	}
	if _, m, err := f.updateMeta(rec.ID, func(m *meta) { m.Labels = req.Labels }); err != nil {
		notFound(w, r, rec.ID)
	} else {
		writeJSON(w, http.StatusOK, map[string]any{"labels": m.Labels})
	}
}

// interval is the {interval} path value, in minutes.
func interval(w http.ResponseWriter, r *http.Request, min int) (int, bool) {
	n, err := strconv.Atoi(r.PathValue("interval"))
	if err != nil || n < min {
		writeErr(w, r, http.StatusBadRequest, "BAD_REQUEST", "Invalid interval: "+r.PathValue("interval"))
		return 0, false
	}
	return n, true
}

// setAutoStop changes the auto-stop interval, and with it the engine's idle
// rule, which a running sandbox's idle watcher reads on its next tick.
func (f *Frontend) setAutoStop(w http.ResponseWriter, r *http.Request, rec store.Record, m meta) {
	n, ok := interval(w, r, 0)
	if !ok {
		return
	}
	defer f.lock(rec.ID)()
	if _, err := f.life.SetPolicy(rec.ID, idlePolicy(n)); err != nil {
		notFound(w, r, rec.ID)
		return
	}
	cur, m, err := f.updateMeta(rec.ID, func(m *meta) {
		m.AutoStop = n
		if n > 0 {
			m.AutoPause = nil
		}
	})
	if err != nil {
		notFound(w, r, rec.ID)
		return
	}
	writeJSON(w, http.StatusOK, f.dto(r, cur, m, f.state(cur, m)))
}

// setInterval keeps an interval this server reports but does not act on
// (auto-archive; auto-delete, apart from 0, which onEvent acts on).
func (f *Frontend) setInterval(set func(*meta, int)) func(http.ResponseWriter, *http.Request, store.Record, meta) {
	return func(w http.ResponseWriter, r *http.Request, rec store.Record, m meta) {
		n, ok := interval(w, r, -1)
		if !ok {
			return
		}
		cur, m, err := f.updateMeta(rec.ID, func(m *meta) { set(m, n) })
		if err != nil {
			notFound(w, r, rec.ID)
			return
		}
		writeJSON(w, http.StatusOK, f.dto(r, cur, m, f.state(cur, m)))
	}
}

// list is GET /sandbox: Daytona sandboxes, newest first, filtered by labels
// (a JSON object, every pair of which must match), states, name and ID, in
// pages of limit (default 100) with an opaque nextCursor (null on the last).
func (f *Frontend) list(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	var labels map[string]string
	if s := q.Get("labels"); s != "" {
		if err := json.Unmarshal([]byte(s), &labels); err != nil {
			writeErr(w, r, http.StatusBadRequest, "BAD_REQUEST", "labels must be a JSON object of strings")
			return
		}
	}
	var states []string
	for _, s := range q["states"] {
		for _, st := range strings.Split(s, ",") {
			if st = strings.TrimSpace(st); st != "" {
				states = append(states, st)
			}
		}
	}
	limit := 100
	if s := q.Get("limit"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n < 1 || n > 200 {
			writeErr(w, r, http.StatusBadRequest, "BAD_REQUEST", "limit must be between 1 and 200")
			return
		}
		limit = n
	}
	offset := 0
	if c := q.Get("cursor"); c != "" {
		b, err := base64.RawURLEncoding.DecodeString(c)
		n, err2 := strconv.Atoi(string(b))
		if err != nil || err2 != nil || n < 0 {
			writeErr(w, r, http.StatusBadRequest, "BAD_REQUEST", "Invalid cursor")
			return
		}
		offset = n
	}

	type item struct {
		rec   store.Record
		m     meta
		state string
	}
	var items []item
	for _, sp := range f.store.All() {
		m, ok := metaOf(sp.Record)
		if !ok || (q.Get("name") != "" && m.Name != q.Get("name")) || (q.Get("id") != "" && sp.ID != q.Get("id")) {
			continue
		}
		match := true
		for k, v := range labels {
			if m.Labels[k] != v {
				match = false
			}
		}
		st := f.state(sp.Record, m)
		if !match || (len(states) > 0 && !slices.Contains(states, st)) {
			continue
		}
		items = append(items, item{sp.Record, m, st})
	}
	sort.Slice(items, func(i, j int) bool {
		if !items[i].rec.CreatedAt.Equal(items[j].rec.CreatedAt) {
			return items[i].rec.CreatedAt.After(items[j].rec.CreatedAt)
		}
		return items[i].rec.ID < items[j].rec.ID
	})
	items = items[min(offset, len(items)):]
	var next *string
	if len(items) > limit {
		items = items[:limit]
		c := base64.RawURLEncoding.EncodeToString([]byte(strconv.Itoa(offset + limit)))
		next = &c
	}
	out := make([]sandboxJSON, len(items))
	for i, it := range items {
		out[i] = f.dto(r, it.rec, it.m, it.state)
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": out, "nextCursor": next})
}
