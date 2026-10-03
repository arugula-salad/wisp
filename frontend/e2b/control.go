package e2b

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/arugula-salad/wisp/engine"
	"github.com/arugula-salad/wisp/internal/store"
)

// The control plane: E2B's REST API, as the SDKs use it (docs/providers/e2b.md
// section 2). Shapes, status codes and messages are hosted E2B's, from its
// OpenAPI spec, its API server's handlers and the golden traces.

func (f *Frontend) controlPlane() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		io.WriteString(w, "Health check successful")
	})
	mux.HandleFunc("POST /v2/sandboxes", f.auth(true, func(w http.ResponseWriter, r *http.Request) { f.create(w, r, defaultTimeout) }))
	mux.HandleFunc("POST /sandboxes", f.auth(true, func(w http.ResponseWriter, r *http.Request) { f.create(w, r, defaultTimeoutV1) }))
	mux.HandleFunc("GET /v2/sandboxes", f.auth(false, f.list))
	mux.HandleFunc("GET /sandboxes", f.auth(false, f.listV1))
	mux.HandleFunc("GET /sandboxes/{id}", f.auth(false, f.sandbox(f.get)))
	mux.HandleFunc("DELETE /sandboxes/{id}", f.auth(true, f.sandbox(f.kill)))
	mux.HandleFunc("POST /sandboxes/{id}/timeout", f.auth(true, f.sandbox(f.setTimeout)))
	mux.HandleFunc("POST /sandboxes/{id}/refreshes", f.auth(true, f.sandbox(f.refresh)))
	mux.HandleFunc("POST /sandboxes/{id}/pause", f.auth(true, f.sandbox(f.pause)))
	mux.HandleFunc("POST /v2/sandboxes/{id}/connect", f.auth(true, f.sandbox(f.connect)))
	mux.HandleFunc("POST /sandboxes/{id}/resume", f.auth(true, f.sandbox(f.resume)))
	mux.HandleFunc("GET /sandboxes/{id}/metrics", f.auth(false, f.sandbox(f.metrics)))
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeErr(w, http.StatusNotFound, "Not found: this E2B-compatible server does not implement "+r.Method+" "+r.URL.Path)
	})
	return mux
}

// writeErr is E2B's error body, {"code": <status>, "message": ...}. The SDKs
// map errors by status and show the message.
func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]any{"code": status, "message": msg})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

// auth checks X-API-Key against the daemon's keys. A read key may look but
// not change anything, as on the Sprites API.
func (f *Frontend) auth(write bool, h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		admin, ok := false, false
		if key := r.Header.Get("X-API-Key"); key != "" && f.opts.CheckKey != nil {
			admin, ok = f.opts.CheckKey(key)
		}
		if !ok {
			writeErr(w, http.StatusUnauthorized, "Invalid API key: use this daemon's root token or one of its API keys (wispd keys)")
			return
		}
		if write && !admin {
			writeErr(w, http.StatusForbidden, "This API key is read-only")
			return
		}
		h(w, r)
	}
}

// notFound is hosted E2B's answer for an ID that is not a sandbox of ours.
func notFound(w http.ResponseWriter, id string) {
	writeErr(w, http.StatusNotFound, fmt.Sprintf("Sandbox %q doesn't exist or you don't have access to it", id))
}

// sandbox looks up {id} as an E2B sandbox.
func (f *Frontend) sandbox(h func(http.ResponseWriter, *http.Request, store.Record, meta)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		rec, err := f.store.GetRecord(id)
		m, ok := metaOf(rec)
		if err != nil || !ok {
			notFound(w, id)
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
		writeErr(w, http.StatusBadRequest, "Error when parsing request: "+err.Error())
		return false
	}
	return true
}

// timeoutOf is a request's timeout in seconds, or def; ok is false (and the
// answer written) for one out of range.
func (f *Frontend) timeoutOf(w http.ResponseWriter, secs *int, def time.Duration) (time.Duration, bool) {
	if secs == nil {
		return def, true
	}
	d := time.Duration(*secs) * time.Second
	if *secs < 0 {
		writeErr(w, http.StatusBadRequest, "Timeout must not be negative")
		return 0, false
	}
	if d > f.opts.MaxTimeout {
		writeErr(w, http.StatusBadRequest, fmt.Sprintf("Timeout cannot be greater than %s", f.opts.MaxTimeout))
		return 0, false
	}
	return d, true
}

// The wire shapes (OpenAPI components.schemas).

// sandboxJSON is Sandbox: the answer to a create or a connect.
type sandboxJSON struct {
	Alias           string `json:"alias,omitempty"`
	ClientID        string `json:"clientID"`
	Domain          string `json:"domain,omitempty"`
	EnvdAccessToken string `json:"envdAccessToken,omitempty"`
	EnvdVersion     string `json:"envdVersion"`
	SandboxID       string `json:"sandboxID"`
	TemplateID      string `json:"templateID"`
}

// detailJSON is SandboxDetail (GET /sandboxes/{id}) and, without the token and
// the lifecycle, ListedSandbox (GET /v2/sandboxes).
type detailJSON struct {
	Alias           string            `json:"alias,omitempty"`
	ClientID        string            `json:"clientID"`
	CPUCount        int               `json:"cpuCount"`
	DiskSizeMB      int               `json:"diskSizeMB"`
	Domain          string            `json:"domain,omitempty"`
	EndAt           string            `json:"endAt"`
	EnvdAccessToken string            `json:"envdAccessToken,omitempty"`
	EnvdVersion     string            `json:"envdVersion"`
	Lifecycle       *lifecycleJSON    `json:"lifecycle,omitempty"`
	MemoryMB        int               `json:"memoryMB"`
	Metadata        map[string]string `json:"metadata,omitempty"`
	SandboxID       string            `json:"sandboxID"`
	StartedAt       string            `json:"startedAt"`
	State           string            `json:"state"`
	TemplateID      string            `json:"templateID"`
	// VolumeMounts is [] for a running sandbox and absent for a paused one, as hosted.
	VolumeMounts *[]any `json:"volumeMounts,omitempty"`
}

type lifecycleJSON struct {
	AutoResume bool   `json:"autoResume"`
	OnTimeout  string `json:"onTimeout"`
}

func stamp(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

func (f *Frontend) sandboxOf(rec store.Record, m meta) sandboxJSON {
	return sandboxJSON{Alias: m.Alias, ClientID: clientID, Domain: f.opts.Domain, EnvdAccessToken: m.AccessToken,
		EnvdVersion: EnvdVersion, SandboxID: rec.ID, TemplateID: m.TemplateID}
}

func (f *Frontend) detailOf(rec store.Record, m meta, now time.Time, listed bool) detailJSON {
	d := detailJSON{Alias: m.Alias, ClientID: clientID, CPUCount: f.opts.DefaultCPUs, DiskSizeMB: f.diskMB, Domain: f.opts.Domain,
		EndAt: stamp(m.EndAt), EnvdVersion: EnvdVersion, MemoryMB: f.opts.DefaultMemMiB, Metadata: m.Metadata,
		SandboxID: rec.ID, StartedAt: stamp(m.StartedAt), State: m.state(now), TemplateID: m.TemplateID}
	if rec.Config.CPUs > 0 {
		d.CPUCount = rec.Config.CPUs
	}
	if rec.Config.RamMB > 0 {
		d.MemoryMB = rec.Config.RamMB
	}
	if d.State == "running" {
		d.VolumeMounts = &[]any{}
	}
	if !listed {
		d.EnvdAccessToken = m.AccessToken
		onTimeout := "kill"
		if m.AutoPause {
			onTimeout = "pause"
		}
		d.Lifecycle = &lifecycleJSON{AutoResume: m.AutoResume, OnTimeout: onTimeout}
	}
	return d
}

// deadlineAction is what the engine does at EndAt.
func deadlineAction(m meta) store.DeadlineAction {
	if m.AutoPause {
		return store.DeadlineSuspend
	}
	return store.DeadlineDelete
}

// createReq is NewSandboxV2, the parts of it this server honours.
type createReq struct {
	TemplateID string            `json:"templateID"`
	Timeout    *int              `json:"timeout"`
	Metadata   map[string]string `json:"metadata"`
	EnvVars    map[string]string `json:"envVars"`
	AutoPause  bool              `json:"autoPause"`
	AutoResume *struct {
		Enabled bool `json:"enabled"`
	} `json:"autoResume"`
	Secure *bool `json:"secure"`
}

// template resolves a template name or ID. Only the base template exists:
// E2B's base image, with envd (images/e2b).
func template(ref string) (id, alias string, ok bool) {
	if ref == "" {
		ref = baseAlias
	}
	name, tag, _ := strings.Cut(ref, ":")
	if (name == baseAlias || name == baseTemplate) && (tag == "" || tag == "default") {
		return baseTemplate, baseAlias, true
	}
	return "", "", false
}

func (f *Frontend) create(w http.ResponseWriter, r *http.Request, def time.Duration) {
	var req createReq
	if !readBody(w, r, &req) {
		return
	}
	tmpl, alias, ok := template(req.TemplateID)
	if !ok {
		writeErr(w, http.StatusNotFound, fmt.Sprintf("template '%s' not found", req.TemplateID))
		return
	}
	timeout, ok := f.timeoutOf(w, req.Timeout, def)
	if !ok {
		return
	}
	if limit, n := f.opts.MaxSandboxes, f.store.Count(); limit > 0 && n >= limit {
		writeErr(w, http.StatusTooManyRequests, fmt.Sprintf("You have reached the maximum number of sandboxes (%d)", limit))
		return
	}
	now := time.Now().UTC()
	m := meta{TemplateID: tmpl, Alias: alias, Metadata: req.Metadata, EnvVars: req.EnvVars,
		AccessToken: newAccessToken(), StartedAt: now, EndAt: now.Add(timeout),
		AutoPause: req.AutoPause, AutoResume: req.AutoResume != nil && req.AutoResume.Enabled}
	end := m.EndAt
	sp := store.Sprite{Record: store.Record{ID: newSandboxID(), API: API, Hostname: hostname, CreatedAt: now, UpdatedAt: now,
		Config: store.Config{CPUs: f.opts.CPUs, RamMB: f.opts.MemMiB},
		// E2B sandboxes are never suspended for being idle: they run until their
		// timeout, and then die (or pause, with autoPause).
		Lifecycle: &store.LifecyclePolicy{IdleAction: store.IdleNone, DeadlineAction: deadlineAction(m)},
		ExpiresAt: &end}}
	setMeta(&sp.Record, m)
	created, err := f.life.Create(r.Context(), engine.CreateSpec{Sprite: sp, ImageDisk: f.opts.Disk})
	if errors.Is(err, engine.ErrNoRoom) {
		writeErr(w, http.StatusInsufficientStorage, err.Error())
		return
	}
	if err != nil {
		f.log.Error("create failed", "err", err)
		writeErr(w, http.StatusInternalServerError, "Failed to create sandbox: "+err.Error())
		return
	}
	// Hosted create returns once envd answers: boot it now. The boot hook
	// hands envd its token; the idle rule is none, so it stays up.
	_, release, err := f.acquire(r.Context(), created.Record)
	if err != nil {
		f.life.Delete(created.Record)
		f.bootFailed(w, err)
		return
	}
	release()
	f.log.Info("sandbox created", "id", created.ID, "template", alias, "timeout", timeout)
	writeJSON(w, http.StatusCreated, f.sandboxOf(created.Record, m))
}

// bootFailed answers a start the engine refused or could not complete.
func (f *Frontend) bootFailed(w http.ResponseWriter, err error) {
	var lim *engine.LimitError
	if errors.As(err, &lim) {
		// 429 is the SDKs' RateLimitError, and it retries one with an integer Retry-After.
		if lim.RetryAfter > 0 {
			w.Header().Set("Retry-After", strconv.Itoa(lim.RetryAfter))
		}
		writeErr(w, http.StatusTooManyRequests, lim.Message)
		return
	}
	f.log.Error("sandbox failed to start", "err", err)
	writeErr(w, http.StatusInternalServerError, "Failed to start sandbox: "+err.Error())
}

func (f *Frontend) get(w http.ResponseWriter, r *http.Request, rec store.Record, m meta) {
	writeJSON(w, http.StatusOK, f.detailOf(rec, m, time.Now(), false))
}

func (f *Frontend) kill(w http.ResponseWriter, r *http.Request, rec store.Record, m meta) {
	defer f.lock(rec.ID)()
	if err := f.life.Delete(rec); errors.Is(err, store.ErrNotFound) {
		notFound(w, rec.ID)
		return
	} else if err != nil {
		writeErr(w, http.StatusInternalServerError, "Error killing sandbox: "+err.Error())
		return
	}
	f.locks.Delete(rec.ID)
	f.log.Info("sandbox killed", "id", rec.ID)
	w.WriteHeader(http.StatusNoContent)
}

// setDeadline moves the sandbox's end to end, in its metadata and in the
// engine's deadline rule, which is what acts on it.
func (f *Frontend) setDeadline(id string, end time.Time, change func(*meta)) (store.Record, meta, error) {
	rec, m, err := f.updateMeta(id, func(m *meta) {
		m.EndAt = end
		if change != nil {
			change(m)
		}
	})
	if err != nil {
		return rec, m, err
	}
	rec, err = f.life.SetDeadline(id, &end, deadlineAction(m))
	return rec, m, err
}

// deadlineErr answers a failure to set a deadline.
func deadlineErr(w http.ResponseWriter, id string, err error) {
	if errors.Is(err, store.ErrNotFound) || errors.Is(err, engine.ErrLeaseReaping) {
		notFound(w, id)
		return
	}
	writeErr(w, http.StatusInternalServerError, err.Error())
}

func (f *Frontend) setTimeout(w http.ResponseWriter, r *http.Request, rec store.Record, m meta) {
	var req struct {
		Timeout *int `json:"timeout"`
	}
	if !readBody(w, r, &req) {
		return
	}
	if req.Timeout == nil {
		writeErr(w, http.StatusBadRequest, "Error when parsing request: timeout is required")
		return
	}
	timeout, ok := f.timeoutOf(w, req.Timeout, 0)
	if !ok {
		return
	}
	defer f.lock(rec.ID)()
	if cur, err := f.store.GetRecord(rec.ID); err == nil {
		m, _ = metaOf(cur)
	}
	if m.paused(time.Now()) {
		writeErr(w, http.StatusNotFound, fmt.Sprintf("Sandbox %q is paused; resume it with connect first", rec.ID))
		return
	}
	if _, _, err := f.setDeadline(rec.ID, time.Now().UTC().Add(timeout), nil); err != nil {
		deadlineErr(w, rec.ID, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// refresh is the deprecated keep-alive: it extends the timeout, never shortens it.
func (f *Frontend) refresh(w http.ResponseWriter, r *http.Request, rec store.Record, m meta) {
	var req struct {
		Duration *int `json:"duration"`
	}
	if !readBody(w, r, &req) {
		return
	}
	d, ok := f.timeoutOf(w, req.Duration, defaultTimeout)
	if !ok {
		return
	}
	defer f.lock(rec.ID)()
	// Decide on the metadata as it is now: a pause that landed while this
	// waited for the lock cleared the deadline, and must not get one back.
	cur, err := f.store.GetRecord(rec.ID)
	if err != nil {
		notFound(w, rec.ID)
		return
	}
	m, _ = metaOf(cur)
	if end := time.Now().UTC().Add(d); !m.paused(time.Now()) && end.After(m.EndAt) {
		if _, _, err := f.setDeadline(rec.ID, end, nil); err != nil {
			deadlineErr(w, rec.ID, err)
			return
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

// pause suspends the VM: by default warm, a memory snapshot, from which a
// connect resumes it with its processes; with {"memory": false} cold, so a
// connect boots it afresh on its disk. A paused sandbox has no deadline.
func (f *Frontend) pause(w http.ResponseWriter, r *http.Request, rec store.Record, m meta) {
	var req struct {
		Memory *bool `json:"memory"`
	}
	if !readBody(w, r, &req) {
		return
	}
	defer f.lock(rec.ID)()
	cur, err := f.store.GetRecord(rec.ID)
	if err != nil {
		notFound(w, rec.ID)
		return
	}
	m, _ = metaOf(cur)
	if m.paused(time.Now()) {
		writeErr(w, http.StatusConflict, fmt.Sprintf("Error pausing sandbox - sandbox '%s' is already paused", rec.ID))
		return
	}
	// The deadline goes first: a sandbox on its way to a pause must not be
	// killed by a timeout that passes while it suspends.
	if _, err := f.life.SetDeadline(rec.ID, nil, ""); err != nil {
		deadlineErr(w, rec.ID, err)
		return
	}
	// Then it is marked paused, before the VM is suspended: traffic that
	// arrives in between must be refused, not wake the VM back up behind the
	// pause (it would then run with no deadline and no idle rule, for good).
	now := time.Now().UTC()
	if _, _, err := f.updateMeta(rec.ID, func(m *meta) { m.Paused, m.EndAt = true, now }); err != nil {
		notFound(w, rec.ID)
		return
	}
	if err := f.life.Suspend(cur); err != nil {
		f.log.Error("pause failed", "id", rec.ID, "err", err)
		// Undo both: the sandbox is still running, with its old timeout.
		f.updateMeta(rec.ID, func(n *meta) { n.Paused, n.EndAt = false, m.EndAt })
		f.life.SetDeadline(rec.ID, &m.EndAt, deadlineAction(m))
		writeErr(w, http.StatusInternalServerError, "Error pausing sandbox: "+err.Error())
		return
	}
	if req.Memory != nil && !*req.Memory {
		f.life.Cool(cur)
	}
	f.log.Info("sandbox paused", "id", rec.ID)
	w.WriteHeader(http.StatusNoContent)
}

// connect resumes a paused sandbox (201), with its timeout reset to the one
// asked for (default 300 s) as hosted does, or extends a running one's (200):
// connecting never shortens a running sandbox's timeout.
func (f *Frontend) connect(w http.ResponseWriter, r *http.Request, rec store.Record, m meta) {
	var req struct {
		Timeout *int `json:"timeout"`
	}
	if !readBody(w, r, &req) {
		return
	}
	timeout, ok := f.timeoutOf(w, req.Timeout, defaultTimeout)
	if !ok {
		return
	}
	f.resumeOrExtend(w, r, rec.ID, timeout, false)
}

// resume is the deprecated POST /sandboxes/{id}/resume: connect, but 409 for a
// sandbox that is already running.
func (f *Frontend) resume(w http.ResponseWriter, r *http.Request, rec store.Record, m meta) {
	var req struct {
		Timeout *int `json:"timeout"`
	}
	if !readBody(w, r, &req) {
		return
	}
	timeout, ok := f.timeoutOf(w, req.Timeout, defaultTimeoutV1)
	if !ok {
		return
	}
	f.resumeOrExtend(w, r, rec.ID, timeout, true)
}

func (f *Frontend) resumeOrExtend(w http.ResponseWriter, r *http.Request, id string, timeout time.Duration, onlyPaused bool) {
	defer f.lock(id)()
	cur, err := f.store.GetRecord(id)
	m, ok := metaOf(cur)
	if err != nil || !ok {
		notFound(w, id)
		return
	}
	now := time.Now().UTC()
	end := now.Add(timeout)
	if !m.paused(now) {
		if onlyPaused {
			writeErr(w, http.StatusConflict, fmt.Sprintf("Sandbox %s is already running", id))
			return
		}
		if end.After(m.EndAt) {
			if cur, m, err = f.setDeadline(id, end, nil); err != nil {
				deadlineErr(w, id, err)
				return
			}
		}
		writeJSON(w, http.StatusOK, f.sandboxOf(cur, m))
		return
	}
	// Wake it: the boot hook re-initializes envd (the same token) before
	// Acquire returns, so the sandbox is usable the moment this answers.
	_, release, err := f.acquire(r.Context(), cur)
	if err != nil {
		f.bootFailed(w, err)
		return
	}
	release()
	if cur, m, err = f.setDeadline(id, end, func(m *meta) { m.Paused, m.StartedAt = false, now }); err != nil {
		deadlineErr(w, id, err)
		return
	}
	f.log.Info("sandbox resumed", "id", id, "timeout", timeout)
	writeJSON(w, http.StatusCreated, f.sandboxOf(cur, m))
}

// metricJSON is SandboxMetric.
type metricJSON struct {
	CPUCount      int     `json:"cpuCount"`
	CPUUsedPct    float64 `json:"cpuUsedPct"`
	DiskTotal     int64   `json:"diskTotal"`
	DiskUsed      int64   `json:"diskUsed"`
	MemCache      int64   `json:"memCache"`
	MemTotal      int64   `json:"memTotal"`
	MemUsed       int64   `json:"memUsed"`
	Timestamp     string  `json:"timestamp"`
	TimestampUnix int64   `json:"timestampUnix"`
}

// metrics is one sample, envd's own /metrics, for a sandbox whose VM is up;
// [] otherwise (hosted answers [] too until its first sample). There is no
// history: start and end are accepted and ignored.
func (f *Frontend) metrics(w http.ResponseWriter, r *http.Request, rec store.Record, m meta) {
	out := []metricJSON{}
	if !m.paused(time.Now()) && f.life.Peek(rec.ID).Running() {
		if s, err := f.sampleEnvd(r.Context(), rec, m); err == nil {
			out = append(out, s)
		} else {
			f.log.Debug("no metrics from envd", "id", rec.ID, "err", err)
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func (f *Frontend) sampleEnvd(ctx context.Context, rec store.Record, m meta) (metricJSON, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	mach, release, err := f.acquire(ctx, rec)
	if err != nil {
		return metricJSON{}, err
	}
	defer release()
	var s struct {
		TS         int64   `json:"ts"`
		CPUCount   int     `json:"cpu_count"`
		CPUUsedPct float64 `json:"cpu_used_pct"`
		MemTotal   int64   `json:"mem_total"`
		MemUsed    int64   `json:"mem_used"`
		MemCache   int64   `json:"mem_cache"`
		DiskUsed   int64   `json:"disk_used"`
		DiskTotal  int64   `json:"disk_total"`
	}
	if err := f.envdCall(ctx, f.dialEnvd(mach), http.MethodGet, "/metrics", m.AccessToken, nil, &s); err != nil {
		return metricJSON{}, err
	}
	t := time.Unix(s.TS, 0).UTC()
	return metricJSON{CPUCount: s.CPUCount, CPUUsedPct: s.CPUUsedPct, DiskTotal: s.DiskTotal, DiskUsed: s.DiskUsed,
		MemCache: s.MemCache, MemTotal: s.MemTotal, MemUsed: s.MemUsed,
		Timestamp: t.Format(time.RFC3339), TimestampUnix: s.TS}, nil
}

// listed is one sandbox in a listing, with what it is sorted and filtered by.
type listed struct {
	rec store.Record
	m   meta
}

// sandboxes is every E2B sandbox.
func (f *Frontend) sandboxes() []listed {
	var out []listed
	for _, sp := range f.store.All() {
		if m, ok := metaOf(sp.Record); ok {
			out = append(out, listed{sp.Record, m})
		}
	}
	return out
}

// parseMetadata reads the metadata filter: a URL-encoded k=v&k2=v2 string,
// as the SDKs send it and the API server parses it (utils.parseFilters).
func parseMetadata(q string) (map[string]string, error) {
	q, err := url.QueryUnescape(q)
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, kv := range strings.Split(q, "&") {
		parts := strings.Split(kv, "=")
		if len(parts) != 2 {
			return nil, errors.New("invalid key value pair in query")
		}
		k, err1 := url.QueryUnescape(parts[0])
		v, err2 := url.QueryUnescape(parts[1])
		if err1 != nil || err2 != nil {
			return nil, errors.New("invalid escape in query")
		}
		out[k] = v
	}
	return out, nil
}

func matches(m meta, filter map[string]string) bool {
	for k, v := range filter {
		if got, ok := m.Metadata[k]; !ok || got != v {
			return false
		}
	}
	return true
}

// A page cursor is the last item's start time and ID, base64url, as hosted's
// (utils.ParseCursor) are; the SDKs only pass it back.
func cursorOf(l listed) string {
	return base64.URLEncoding.EncodeToString([]byte(l.m.StartedAt.UTC().Format(time.RFC3339Nano) + "__" + l.rec.ID))
}

func parseCursor(tok string) (time.Time, string, error) {
	b, err := base64.URLEncoding.DecodeString(tok)
	if err != nil {
		return time.Time{}, "", err
	}
	ts, id, ok := strings.Cut(string(b), "__")
	if !ok {
		return time.Time{}, "", errors.New("malformed cursor")
	}
	t, err := time.Parse(time.RFC3339Nano, ts)
	return t, id, err
}

// list is GET /v2/sandboxes: running and paused sandboxes (or the states
// asked for), filtered by metadata, template and start time, newest first (or
// oldest, with order=asc), in pages of limit (default and most 100) with the
// next page's cursor in X-Next-Token, absent on the last.
func (f *Frontend) list(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	states := []string{"running", "paused"}
	if s := q.Get("state"); s != "" {
		states = strings.Split(s, ",")
		for _, st := range states {
			if st != "running" && st != "paused" {
				writeErr(w, http.StatusBadRequest, fmt.Sprintf("Invalid state: %q", st))
				return
			}
		}
	}
	var filter map[string]string
	if s := q.Get("metadata"); s != "" {
		var err error
		if filter, err = parseMetadata(s); err != nil {
			writeErr(w, http.StatusBadRequest, "Error parsing metadata")
			return
		}
	}
	limit := 100
	if s := q.Get("limit"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n < 1 || n > 100 {
			writeErr(w, http.StatusBadRequest, "limit must be between 1 and 100")
			return
		}
		limit = n
	}
	asc := false
	switch q.Get("order") {
	case "", "desc":
	case "asc":
		asc = true
	default:
		writeErr(w, http.StatusBadRequest, "Invalid order parameter: want asc or desc")
		return
	}
	var after time.Time
	if s := q.Get("startedAfter"); s != "" {
		t, err := time.Parse(time.RFC3339Nano, s)
		if err != nil {
			writeErr(w, http.StatusBadRequest, "Invalid startedAfter: want an RFC 3339 time")
			return
		}
		after = t
	}
	var tmplFilter string
	if s := q.Get("template"); s != "" {
		id, _, ok := template(s)
		if !ok {
			if slices.Contains(states, "running") {
				w.Header().Set("X-Total-Running", "0")
			}
			writeJSON(w, http.StatusOK, []detailJSON{})
			return
		}
		tmplFilter = id
	}
	var curT time.Time
	var curID string
	if tok := q.Get("nextToken"); tok != "" {
		var err error
		if curT, curID, err = parseCursor(tok); err != nil {
			writeErr(w, http.StatusBadRequest, "Invalid next token")
			return
		}
	}

	now := time.Now()
	var items []listed
	running := 0
	for _, l := range f.sandboxes() {
		st := l.m.state(now)
		if !slices.Contains(states, st) || !matches(l.m, filter) ||
			(tmplFilter != "" && l.m.TemplateID != tmplFilter) || (!after.IsZero() && !l.m.StartedAt.After(after)) {
			continue
		}
		if st == "running" {
			running++
		}
		items = append(items, l)
	}
	if slices.Contains(states, "running") {
		w.Header().Set("X-Total-Running", strconv.Itoa(running))
	}
	less := func(a, b listed) bool { // a comes before b
		if !a.m.StartedAt.Equal(b.m.StartedAt) {
			return a.m.StartedAt.After(b.m.StartedAt) != asc
		}
		return (a.rec.ID > b.rec.ID) != asc
	}
	sort.Slice(items, func(i, j int) bool { return less(items[i], items[j]) })
	if curID != "" {
		mark := listed{rec: store.Record{ID: curID}, m: meta{StartedAt: curT}}
		i := sort.Search(len(items), func(i int) bool { return less(mark, items[i]) })
		items = items[i:]
	}
	if len(items) > limit {
		items = items[:limit]
		w.Header().Set("X-Next-Token", cursorOf(items[limit-1]))
	}
	out := make([]detailJSON, len(items))
	for i, l := range items {
		out[i] = f.detailOf(l.rec, l.m, now, true)
	}
	writeJSON(w, http.StatusOK, out)
}

// listV1 is the deprecated GET /sandboxes: running sandboxes, unpaged.
func (f *Frontend) listV1(w http.ResponseWriter, r *http.Request) {
	var filter map[string]string
	if s := r.URL.Query().Get("metadata"); s != "" {
		var err error
		if filter, err = parseMetadata(s); err != nil {
			writeErr(w, http.StatusBadRequest, "Error parsing metadata")
			return
		}
	}
	now := time.Now()
	out := []detailJSON{}
	for _, l := range f.sandboxes() {
		if l.m.state(now) == "running" && matches(l.m, filter) {
			out = append(out, f.detailOf(l.rec, l.m, now, true))
		}
	}
	writeJSON(w, http.StatusOK, out)
}
