package vercel

import (
	"cmp"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/arugula-salad/wisp/engine"
	"github.com/arugula-salad/wisp/internal/store"
)

// The API: Vercel's sandbox endpoints as the SDKs use them
// (docs/providers/vercel.md, "Endpoints the SDKs call"). Shapes, status codes
// and messages are hosted's, from the SDKs' validators and the golden traces.

func (f *Frontend) apiMux() http.Handler {
	mux := http.NewServeMux()
	// The daemon's own liveness check for probes and uptime monitors, the same
	// on every front end. Route hosts never get here, so it can't shadow an
	// app's /healthz.
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		io.WriteString(w, "ok\n")
	})
	// Sandboxes, by name.
	mux.HandleFunc("POST /v3/sandboxes", f.auth(true, f.create))
	mux.HandleFunc("POST /v2/sandboxes", f.auth(true, f.create))
	mux.HandleFunc("GET /v2/sandboxes", f.auth(false, f.list))
	mux.HandleFunc("GET /v2/sandboxes/{name}", f.auth(false, f.get))
	mux.HandleFunc("DELETE /v2/sandboxes/{name}", f.auth(true, f.delete))
	// Sessions, by session ID.
	mux.HandleFunc("GET /v2/sandboxes/sessions", f.auth(false, f.listSessions))
	mux.HandleFunc("GET /v2/sandboxes/sessions/{sid}", f.auth(false, f.session(false, f.getSession)))
	mux.HandleFunc("POST /v2/sandboxes/sessions/{sid}/stop", f.auth(true, f.session(false, f.stop)))
	mux.HandleFunc("POST /v2/sandboxes/sessions/{sid}/extend-timeout", f.auth(true, f.session(true, f.extend)))
	mux.HandleFunc("POST /v2/sandboxes/sessions/{sid}/snapshot", f.auth(true, f.session(true, f.snapshot)))
	mux.HandleFunc("POST /v2/sandboxes/sessions/{sid}/cmd", f.auth(true, f.session(true, f.runCommand)))
	mux.HandleFunc("GET /v2/sandboxes/sessions/{sid}/cmd", f.auth(false, f.session(true, f.listCommands)))
	mux.HandleFunc("GET /v2/sandboxes/sessions/{sid}/cmd/{cid}", f.auth(false, f.session(false, f.getCommand)))
	mux.HandleFunc("GET /v2/sandboxes/sessions/{sid}/cmd/{cid}/logs", f.auth(false, f.session(false, f.commandLogs)))
	mux.HandleFunc("POST /v2/sandboxes/sessions/{sid}/cmd/{cid}/kill", f.auth(true, f.session(true, f.killCommand)))
	mux.HandleFunc("POST /v2/sandboxes/sessions/{sid}/fs/write", f.auth(true, f.session(true, f.writeFiles)))
	mux.HandleFunc("POST /v2/sandboxes/sessions/{sid}/fs/read", f.auth(false, f.session(true, f.readFile)))
	mux.HandleFunc("POST /v2/sandboxes/sessions/{sid}/fs/mkdir", f.auth(true, f.session(true, f.mkdir)))
	// Snapshots, by ID.
	mux.HandleFunc("GET /v2/sandboxes/snapshots", f.auth(false, f.listSnapshots))
	mux.HandleFunc("GET /v2/sandboxes/snapshots/{id}", f.auth(false, f.getSnapshot))
	mux.HandleFunc("DELETE /v2/sandboxes/snapshots/{id}", f.auth(true, f.deleteSnapshot))
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeErr(w, http.StatusNotFound, "not_found",
			"This Vercel Sandbox-compatible server does not implement "+r.Method+" "+r.URL.Path)
	})
	return mux
}

// writeErr is Vercel's error body: {"error": {"code", "message"}}. The SDKs
// put the message into the error they throw and branch on status and code.
func writeErr(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]any{"error": map[string]string{"code": code, "message": msg}})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

// auth checks the bearer token against the daemon's keys. A read key may look
// but not change anything, as on the other APIs. teamId (on every request) and
// the project are accepted whatever they are: one daemon is one team.
func (f *Frontend) auth(write bool, h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		admin, ok := false, false
		if tok, found := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); found && tok != "" && f.opts.CheckKey != nil {
			admin, ok = f.opts.CheckKey(strings.TrimSpace(tok))
		}
		if !ok {
			writeErr(w, http.StatusForbidden, "forbidden",
				"Not authorized: use this daemon's root token or one of its API keys (wispd keys) as the bearer token.")
			return
		}
		if write && !admin {
			writeErr(w, http.StatusForbidden, "forbidden", "This API key is read-only.")
			return
		}
		h(w, r)
	}
}

// readBody decodes an optional JSON body into v: an empty body leaves v as it is.
func readBody(w http.ResponseWriter, r *http.Request, v any) bool {
	b, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err == nil && len(strings.TrimSpace(string(b))) > 0 {
		err = json.Unmarshal(b, v)
	}
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request", "Invalid request: "+err.Error())
		return false
	}
	return true
}

// The wire shapes (the JS SDK's validators.ts; every timestamp in ms).

type sandboxJSON struct {
	Name                     string            `json:"name"`
	CurrentSnapshotID        string            `json:"currentSnapshotId,omitempty"`
	CurrentSessionID         string            `json:"currentSessionId"`
	Status                   string            `json:"status"`
	StatusUpdatedAt          int64             `json:"statusUpdatedAt"`
	Persistent               bool              `json:"persistent"`
	Region                   string            `json:"region"`
	FailoverRegions          []string          `json:"failoverRegions"`
	VCPUs                    int               `json:"vcpus"`
	Memory                   int               `json:"memory"`
	Runtime                  string            `json:"runtime"`
	Architecture             string            `json:"architecture"`
	Image                    string            `json:"image"`
	Timeout                  int64             `json:"timeout"`
	TotalEgressBytes         *int64            `json:"totalEgressBytes,omitempty"`
	TotalIngressBytes        *int64            `json:"totalIngressBytes,omitempty"`
	TotalActiveCPUDurationMs *int64            `json:"totalActiveCpuDurationMs,omitempty"`
	TotalDurationMs          *int64            `json:"totalDurationMs,omitempty"`
	Cwd                      string            `json:"cwd"`
	Tags                     map[string]string `json:"tags,omitempty"`
	CreatedAt                int64             `json:"createdAt"`
	UpdatedAt                int64             `json:"updatedAt"`
	ExpiresAt                int64             `json:"expiresAt,omitempty"`
}

type sessionJSON struct {
	ID                  string        `json:"id"`
	Runtime             string        `json:"runtime"`
	Architecture        string        `json:"architecture"`
	RequestedAt         int64         `json:"requestedAt"`
	StartedAt           int64         `json:"startedAt"`
	RequestedStopAt     int64         `json:"requestedStopAt,omitempty"`
	StoppedAt           int64         `json:"stoppedAt,omitempty"`
	Status              string        `json:"status"`
	Duration            *int64        `json:"duration,omitempty"`
	VCPUs               int           `json:"vcpus"`
	Memory              int           `json:"memory"`
	Timeout             int64         `json:"timeout"`
	Region              string        `json:"region"`
	Cwd                 string        `json:"cwd"`
	SourceSnapshotID    string        `json:"sourceSnapshotId,omitempty"`
	SnapshottedAt       int64         `json:"snapshottedAt,omitempty"`
	CreatedAt           int64         `json:"createdAt"`
	UpdatedAt           int64         `json:"updatedAt"`
	InteractivePort     int           `json:"interactivePort"`
	ActiveCPUDurationMs *int64        `json:"activeCpuDurationMs,omitempty"`
	NetworkTransfer     *transferJSON `json:"networkTransfer,omitempty"`
	SourceSandboxName   string        `json:"sourceSandboxName"`
	ProjectID           string        `json:"projectId,omitempty"`
}

type transferJSON struct {
	Ingress int64 `json:"ingress"`
	Egress  int64 `json:"egress"`
}

type routeJSON struct {
	URL       string `json:"url"`
	Subdomain string `json:"subdomain"`
	Port      int    `json:"port"`
}

type snapshotJSON struct {
	ID              string   `json:"id"`
	SourceSessionID string   `json:"sourceSessionId"`
	Architecture    string   `json:"architecture"`
	Region          string   `json:"region"`
	Regions         []string `json:"regions"`
	Status          string   `json:"status"`
	SizeBytes       int64    `json:"sizeBytes"`
	ExpiresAt       int64    `json:"expiresAt,omitempty"`
	CreatedAt       int64    `json:"createdAt"`
	UpdatedAt       int64    `json:"updatedAt"`
	LastUsedAt      int64    `json:"lastUsedAt,omitempty"`
	CreationMethod  string   `json:"creationMethod"`
}

type paginationJSON struct {
	Count int     `json:"count"`
	Next  *string `json:"next"`
}

func zero() *int64 { var z int64; return &z }

// sandboxOf is a sandbox; with totals, as listed and got, which is when hosted
// reports the totals (zero here, but for the duration) and expiresAt.
func (f *Frontend) sandboxOf(m meta, totals bool) sandboxJSON {
	cur := m.current()
	s := sandboxJSON{Name: m.Name, CurrentSnapshotID: m.CurrentSnapshotID, CurrentSessionID: cur.ID,
		Status: cur.Status, StatusUpdatedAt: m.StatusUpdatedAt, Persistent: m.Persistent, Region: f.opts.Region,
		FailoverRegions: []string{}, VCPUs: m.VCPUs, Memory: m.Memory, Runtime: m.Runtime, Architecture: architecture,
		Image: imageName, Timeout: m.Timeout, Cwd: f.opts.Home, Tags: m.Tags, CreatedAt: m.CreatedAt, UpdatedAt: m.UpdatedAt}
	if totals {
		d := m.TotalDurationMs
		s.TotalEgressBytes, s.TotalIngressBytes, s.TotalActiveCPUDurationMs, s.TotalDurationMs = zero(), zero(), zero(), &d
	}
	if totals && cur.Status == "running" {
		s.ExpiresAt = cur.StartedAt + cur.Timeout
	}
	return s
}

func (f *Frontend) sessionOf(rec store.Record, m meta, s session) sessionJSON {
	j := sessionJSON{ID: s.ID, Runtime: m.Runtime, Architecture: architecture, RequestedAt: s.RequestedAt,
		StartedAt: s.StartedAt, RequestedStopAt: s.RequestedStopAt, StoppedAt: s.StoppedAt, Status: s.Status,
		VCPUs: m.VCPUs, Memory: m.Memory, Timeout: s.Timeout, Region: f.opts.Region, Cwd: f.opts.Home,
		SourceSnapshotID: s.SourceSnapshotID, SnapshottedAt: s.SnapshottedAt, CreatedAt: s.RequestedAt,
		UpdatedAt: s.StartedAt, InteractivePort: interactivePort, SourceSandboxName: m.Name, ProjectID: m.ProjectID}
	if s.Status == "stopped" {
		d := s.StoppedAt - s.StartedAt
		j.Duration, j.ActiveCPUDurationMs, j.NetworkTransfer = &d, zero(), &transferJSON{}
	}
	return j
}

func (f *Frontend) routesOf(m meta) []routeJSON {
	out := make([]routeJSON, 0, len(m.Routes))
	for _, r := range m.Routes {
		out = append(out, routeJSON{URL: f.opts.RouteURL(r.Subdomain), Subdomain: r.Subdomain, Port: r.Port})
	}
	return out
}

func (f *Frontend) snapshotOf(s snapshot, status string) snapshotJSON {
	return snapshotJSON{ID: s.ID, SourceSessionID: s.SourceSessionID, Architecture: architecture, Region: f.opts.Region,
		Regions: []string{f.opts.Region}, Status: status, SizeBytes: s.SizeBytes, ExpiresAt: s.ExpiresAt,
		CreatedAt: s.CreatedAt, UpdatedAt: s.CreatedAt, LastUsedAt: s.LastUsedAt, CreationMethod: s.Method}
}

// sandboxes is every Vercel sandbox, as the store has it.
func (f *Frontend) sandboxes() []store.Sprite {
	return f.store.List(API, "")
}

// byName finds a sandbox, settled (settle). A projectId other than the one it
// was made in does not find it.
func (f *Frontend) byName(name, project string) (store.Record, meta, bool) {
	sp, err := f.store.GetByName(API, name)
	if err != nil {
		return store.Record{}, meta{}, false
	}
	m, ok := metaOf(sp.Record)
	if !ok || (project != "" && m.ProjectID != "" && project != m.ProjectID) {
		return store.Record{}, meta{}, false
	}
	rec, m := f.settle(sp.Record, m)
	return rec, m, true
}

func notFoundName(w http.ResponseWriter, name string) {
	writeErr(w, http.StatusNotFound, "not_found", fmt.Sprintf("Named sandbox '%s' not found for this project.", name))
}

// createReq is the create body, the parts of it this server honours.
type createReq struct {
	ProjectID string `json:"projectId"`
	Name      string `json:"name"`
	Ports     []int  `json:"ports"`
	Source    *struct {
		Type       string `json:"type"`
		SnapshotID string `json:"snapshotId"`
		URL        string `json:"url"`
	} `json:"source"`
	Timeout   *int64 `json:"timeout"`
	Resources *struct {
		VCPUs int `json:"vcpus"`
	} `json:"resources"`
	Runtime    string            `json:"runtime"`
	Persistent *bool             `json:"persistent"`
	Env        map[string]string `json:"env"`
	Tags       map[string]string `json:"tags"`
}

// names are what hosted accepts, as far as the SDKs tell (letters, digits,
// dashes and underscores); the store needs nothing more than non-empty.
var validName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$`)

func (f *Frontend) timeoutOf(w http.ResponseWriter, ms *int64) (int64, bool) {
	if ms == nil {
		return defaultTimeout.Milliseconds(), true
	}
	if *ms <= 0 {
		writeErr(w, http.StatusBadRequest, "bad_request", "Invalid request: `timeout` must be a positive number of milliseconds.")
		return 0, false
	}
	// Compared in ms: a huge value would overflow a time.Duration.
	if *ms > f.opts.MaxTimeout.Milliseconds() {
		writeErr(w, http.StatusBadRequest, "bad_request",
			fmt.Sprintf("Invalid request: `timeout` must be at most %d ms.", f.opts.MaxTimeout.Milliseconds()))
		return 0, false
	}
	return *ms, true
}

func (f *Frontend) create(w http.ResponseWriter, r *http.Request) {
	var req createReq
	if !readBody(w, r, &req) {
		return
	}
	// The daemon's sandbox limit counts every API's. 429 is hosted's status for
	// its limits ("The concurrency limit has been exceeded."); a Retry-After past
	// 20 s makes the JS SDK give up at once rather than retry a limit that
	// waiting will not lift.
	if limit, n := f.opts.MaxSandboxes, f.store.Count(); limit > 0 && n >= limit {
		w.Header().Set("Retry-After", "60")
		writeErr(w, http.StatusTooManyRequests, "too_many_sandboxes",
			fmt.Sprintf("The concurrency limit has been exceeded: this server holds at most %d sandboxes.", limit))
		return
	}
	name := req.Name
	if name == "" {
		name = newSandboxName()
	}
	if !validName.MatchString(name) {
		writeErr(w, http.StatusBadRequest, "bad_request", "Invalid request: `name` may only contain letters, digits, '-', '_' and '.'.")
		return
	}
	timeout, ok := f.timeoutOf(w, req.Timeout)
	if !ok {
		return
	}
	vcpus := defaultVCPUs
	if req.Resources != nil && req.Resources.VCPUs != 0 {
		vcpus = req.Resources.VCPUs
	}
	if vcpus < 1 || vcpus > f.opts.MaxVCPUs || (vcpus > 1 && vcpus%2 != 0) {
		writeErr(w, http.StatusBadRequest, "bad_request",
			fmt.Sprintf("Invalid request: `resources.vcpus` must be 1 or an even number up to %d.", f.opts.MaxVCPUs))
		return
	}
	if len(req.Ports) > maxPorts {
		writeErr(w, http.StatusBadRequest, "bad_request", fmt.Sprintf("Invalid request: at most %d ports.", maxPorts))
		return
	}
	if len(req.Tags) > 5 {
		writeErr(w, http.StatusBadRequest, "bad_request", "Invalid request: at most 5 tags.")
		return
	}
	var routes []route
	seen := map[int]bool{}
	for _, p := range req.Ports {
		if _, ok := portString(p); !ok || seen[p] {
			writeErr(w, http.StatusBadRequest, "bad_request", fmt.Sprintf("Invalid request: invalid or repeated port %d.", p))
			return
		}
		seen[p] = true
		routes = append(routes, route{Port: p, Subdomain: newSubdomain()})
	}
	// Where the disk comes from: the image, or another sandbox's snapshot.
	var from *engine.CheckpointRef
	var sourceSnap string
	release := func() {}
	if req.Source != nil {
		switch req.Source.Type {
		case "snapshot":
			src, _, s, found := f.findSnapshot(req.Source.SnapshotID)
			if !found {
				writeErr(w, http.StatusNotFound, "snapshot_not_found", fmt.Sprintf("Snapshot '%s' not found.", req.Source.SnapshotID))
				return
			}
			// Held until the clone is made, so it cannot be deleted under the copy.
			cur, cp, rel, err := f.life.HoldCheckpoint(src, s.Checkpoint)
			if err != nil {
				writeErr(w, http.StatusNotFound, "snapshot_not_found", fmt.Sprintf("Snapshot '%s' not found.", req.Source.SnapshotID))
				return
			}
			release = rel
			defer func() { release() }()
			from, sourceSnap = &engine.CheckpointRef{Sprite: cur, ID: cp}, s.ID
		case "git", "tarball":
			writeErr(w, http.StatusBadRequest, "bad_request",
				fmt.Sprintf("Invalid request: source type '%s' is not supported by this server; use a snapshot or write files after create.", req.Source.Type))
			return
		default:
			writeErr(w, http.StatusBadRequest, "bad_request", fmt.Sprintf("Invalid request: unknown source type '%s'.", req.Source.Type))
			return
		}
	}

	now := f.ms()
	persistent := req.Persistent == nil || *req.Persistent
	runtime := runtimeName
	if req.Runtime != "" {
		runtime = req.Runtime
	}
	sess := session{ID: newSessionID(), RequestedAt: now, StartedAt: now, Timeout: timeout, Status: "running", SourceSnapshotID: sourceSnap}
	m := meta{Name: name, ProjectID: req.ProjectID, Persistent: persistent, Timeout: timeout, VCPUs: vcpus,
		Memory: vcpus * f.opts.MemPerVCPU, Runtime: runtime, Env: req.Env, Tags: req.Tags, Routes: routes,
		CreatedAt: now, UpdatedAt: now, StatusUpdatedAt: now, Sessions: []session{sess}}
	if sourceSnap != "" {
		m.CurrentSnapshotID = sourceSnap
	}
	end := sess.deadline()
	created := f.now().UTC()
	sp := store.Sprite{Record: store.Record{ID: store.NewID(), API: API, Hostname: hostname, CreatedAt: created, UpdatedAt: created,
		Config: store.Config{CPUs: vcpus, RamMB: m.Memory},
		// A session runs until its timeout, whatever it is doing, and is then stopped.
		Lifecycle: &store.LifecyclePolicy{IdleAction: store.IdleNone, DeadlineAction: store.DeadlineStop},
		ExpiresAt: &end}, SpriteMeta: store.SpriteMeta{Name: name}}
	setMeta(&sp.Record, m)
	spec := engine.CreateSpec{Sprite: sp, ImageDisk: f.opts.Disk, Checkpoint: from}
	if from != nil {
		spec.ImageDisk = ""
	}
	rec, err := f.life.Create(r.Context(), spec)
	release()
	release = func() {}
	switch {
	case errors.Is(err, store.ErrExists):
		writeErr(w, http.StatusConflict, "sandbox_exists", fmt.Sprintf("A sandbox named '%s' already exists in this project.", name))
		return
	case errors.Is(err, engine.ErrNoRoom):
		writeErr(w, http.StatusInsufficientStorage, "no_room", err.Error())
		return
	case err != nil:
		f.log.Error("create failed", "name", name, "err", err)
		writeErr(w, http.StatusInternalServerError, "internal_server_error", "Failed to create sandbox: "+err.Error())
		return
	}
	// Hosted returns a running session: boot it now. The idle rule is none,
	// so it stays up until its timeout.
	_, done, err := f.acquire(r.Context(), rec.Record)
	if err != nil {
		f.life.Delete(rec.Record)
		f.bootFailed(w, err)
		return
	}
	done()
	f.sessions.Store(sess.ID, rec.ID)
	// The boot is the session's real start.
	started := f.ms()
	rec2, m, err := f.updateMeta(rec.ID, func(m *meta) { m.current().StartedAt = started })
	if err == nil {
		end := m.current().deadline()
		f.life.SetDeadline(rec.ID, &end, store.DeadlineStop)
	}
	f.log.Info("sandbox created", "name", name, "id", rec.ID, "session", sess.ID, "timeout_ms", timeout, "from_snapshot", sourceSnap)
	writeJSON(w, http.StatusOK, map[string]any{"sandbox": f.sandboxOf(m, false),
		"session": f.sessionOf(rec2, m, *m.current()), "routes": f.routesOf(m)})
}

// bootFailed answers a start the engine refused or could not complete.
func (f *Frontend) bootFailed(w http.ResponseWriter, err error) {
	var lim *engine.LimitError
	if errors.As(err, &lim) {
		// 429 is retried by the SDKs, honouring Retry-After.
		if lim.RetryAfter > 0 {
			w.Header().Set("Retry-After", strconv.Itoa(lim.RetryAfter))
		}
		writeErr(w, http.StatusTooManyRequests, "too_many_requests", lim.Message)
		return
	}
	f.log.Error("sandbox failed to start", "err", err)
	writeErr(w, http.StatusInternalServerError, "internal_server_error", "Failed to start sandbox: "+err.Error())
}

// get is GET /v2/sandboxes/{name}: the sandbox, its current session and
// routes; with resume=true a stopped sandbox gets a new session first.
func (f *Frontend) get(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	rec, m, ok := f.byName(name, r.URL.Query().Get("projectId"))
	if !ok {
		notFoundName(w, name)
		return
	}
	resumed := false
	if r.URL.Query().Get("resume") == "true" && m.current().Status != "running" {
		var status int
		var code, msg string
		rec, m, status, code, msg = f.resume(r, rec.ID)
		if status != 0 {
			if status == http.StatusTooManyRequests || status >= 500 {
				w.Header().Set("Retry-After", "1")
			}
			writeErr(w, status, code, msg)
			return
		}
		resumed = true
	}
	writeJSON(w, http.StatusOK, map[string]any{"sandbox": f.sandboxOf(m, true),
		"session": f.sessionOf(rec, m, *m.current()), "routes": f.routesOf(m), "resumed": resumed})
}

// delete is DELETE /v2/sandboxes/{name}: the sandbox, its disk and its
// snapshots (which are checkpoints of its record) go.
func (f *Frontend) delete(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	rec, _, ok := f.byName(name, r.URL.Query().Get("projectId"))
	if !ok {
		notFoundName(w, name)
		return
	}
	unlock := f.lock(rec.ID)
	defer unlock()
	cur, err := f.store.GetRecord(rec.ID)
	m, ok := metaOf(cur)
	if err != nil || !ok {
		notFoundName(w, name)
		return
	}
	if c := m.current(); c.Status == "running" {
		now := f.ms()
		c.Status, c.RequestedStopAt, c.StoppedAt = "stopped", now, now
	}
	if err := f.life.Delete(cur); errors.Is(err, store.ErrNotFound) {
		notFoundName(w, name)
		return
	} else if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal_server_error", "Error deleting sandbox: "+err.Error())
		return
	}
	f.log.Info("sandbox deleted", "name", name, "id", rec.ID)
	writeJSON(w, http.StatusOK, map[string]any{"sandbox": f.sandboxOf(m, true)})
}

// list is GET /v2/sandboxes: the project's sandboxes, sorted by createdAt
// (default), name or statusUpdatedAt, newest/last first unless
// sortOrder=asc, filtered by namePrefix (sortBy=name only) and tags (k:v), in
// pages of limit (default 50, at most 100) with an opaque cursor.
func (f *Frontend) list(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	sortBy := q.Get("sortBy")
	switch sortBy {
	case "", "createdAt", "name", "statusUpdatedAt":
	default:
		writeErr(w, http.StatusBadRequest, "bad_request", "Invalid request: `sortBy` must be one of createdAt, name, statusUpdatedAt.")
		return
	}
	prefix := q.Get("namePrefix")
	if prefix != "" && sortBy != "name" {
		writeErr(w, http.StatusBadRequest, "bad_request", "Invalid request: `namePrefix` is only valid when `sortBy` is `name`")
		return
	}
	asc := false
	switch q.Get("sortOrder") {
	case "", "desc":
	case "asc":
		asc = true
	default:
		writeErr(w, http.StatusBadRequest, "bad_request", "Invalid request: `sortOrder` must be asc or desc.")
		return
	}
	limit, ok := limitOf(w, q.Get("limit"), 50)
	if !ok {
		return
	}
	offset, ok := cursorOf(w, q.Get("cursor"))
	if !ok {
		return
	}
	tags := map[string]string{}
	for _, t := range q["tags"] {
		k, v, _ := strings.Cut(t, ":")
		tags[k] = v
	}
	project := q.Get("project")
	var items []meta
	for _, sp := range f.sandboxes() {
		m, ok := metaOf(sp.Record)
		if !ok || !strings.HasPrefix(m.Name, prefix) || (project != "" && m.ProjectID != "" && m.ProjectID != project) {
			continue
		}
		match := true
		for k, v := range tags {
			if m.Tags[k] != v {
				match = false
			}
		}
		if !match {
			continue
		}
		_, m = f.settle(sp.Record, m)
		items = append(items, m)
	}
	key := func(m meta) string {
		switch sortBy {
		case "name":
			return m.Name
		case "statusUpdatedAt":
			return fmt.Sprintf("%020d %s", m.StatusUpdatedAt, m.Name)
		}
		return fmt.Sprintf("%020d %s", m.CreatedAt, m.Name)
	}
	// Keys end with the name, which is unique: a strict order, stable across pages.
	sort.Slice(items, func(i, j int) bool { return ordered(key(items[i]), key(items[j]), "", "", asc) })
	out := []sandboxJSON{}
	page, next := paginate(len(items), offset, limit)
	for _, m := range items[page[0]:page[1]] {
		out = append(out, f.sandboxOf(m, true))
	}
	writeJSON(w, http.StatusOK, map[string]any{"sandboxes": out, "pagination": paginationJSON{Count: len(out), Next: next}})
}

// ordered is a strict "a before b" for a listing sorted by key (ascending
// or not), ties broken by a unique ID in the same direction, so that offset
// pages neither repeat nor skip items with equal keys.
func ordered[K cmp.Ordered](ka, kb K, ida, idb string, asc bool) bool {
	if !asc { // descending is ascending with the operands swapped, never a negation
		ka, kb, ida, idb = kb, ka, idb, ida
	}
	if ka != kb {
		return ka < kb
	}
	return ida < idb
}

func limitOf(w http.ResponseWriter, s string, def int) (int, bool) {
	if s == "" {
		return def, true
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 1 || n > 100 {
		writeErr(w, http.StatusBadRequest, "bad_request", "Invalid request: `limit` must be between 1 and 100.")
		return 0, false
	}
	return n, true
}

// A cursor is the offset of the next page, opaque to the SDKs.
func cursorOf(w http.ResponseWriter, s string) (int, bool) {
	if s == "" {
		return 0, true
	}
	b, err := base64.RawURLEncoding.DecodeString(s)
	n, err2 := strconv.Atoi(string(b))
	if err != nil || err2 != nil || n < 0 {
		writeErr(w, http.StatusBadRequest, "bad_request", "Invalid request: invalid `cursor`.")
		return 0, false
	}
	return n, true
}

// paginate is the [start, end) of the page at offset, and the next page's cursor.
func paginate(n, offset, limit int) ([2]int, *string) {
	start := min(offset, n)
	end := min(start+limit, n)
	if end < n {
		c := base64.RawURLEncoding.EncodeToString([]byte(strconv.Itoa(end)))
		return [2]int{start, end}, &c
	}
	return [2]int{start, end}, nil
}
