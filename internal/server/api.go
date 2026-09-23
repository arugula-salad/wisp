package server

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/jhgaylor/wisp/internal/backup"
	"github.com/jhgaylor/wisp/internal/store"
	"github.com/jhgaylor/wisp/internal/vmm"
)

// apiVersion is reported in Sprite-Version; SDKs use it to pick endpoint
// variants (e.g. path-based exec attach needs >= rc30).
const apiVersion = "v0.0.1-rc48"

var nameRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

type Server struct {
	opts   Options
	store  *store.Store
	life   *Lifecycle
	log    *slog.Logger
	token  string
	org    string
	urlFmt string // fmt pattern taking the sprite name

	urlDomain string // sprite URLs are <name>.<urlDomain>
	storage   *storage
	images    *imageCache    // disks built from container images (images.go)
	backups   *backupManager // nil when no backup bucket is configured
	metrics   *metrics       // history for the web UI (ui.go)
	httpStats *httpStats     // request counts and latency for the web UI (httpstats.go)
	webhooks  []*webhook     // webhooks.go
	leases    *leases        // expiring workspaces (leases.go)
	// guestEvents limits the events a guest may report about itself (guestevents.go).
	guestEvents *rateLimiter
	heartbeat   time.Duration // SSE keepalive; 0 is eventHeartbeat. Tests shorten it.
	domains     *domains      // custom domains (domains.go); nil without a public listener
	started     time.Time
}

// New takes urlFmt, the pattern for the URL a sprite is reported to have: where
// clients reach it, which only the operator knows once a router is involved.
func New(opts Options, st *store.Store, life *Lifecycle, log *slog.Logger, token, org, urlDomain, urlFmt string) *Server {
	s := &Server{opts: opts, store: st, life: life, log: log, token: token, org: org,
		urlDomain: urlDomain, urlFmt: urlFmt, started: time.Now()}
	life.guestAPI = s.guestAPI
	s.storage = newStorage(filepath.Join(opts.DataDir, "vm"), opts.BaseImage)
	if s.storage.reflink {
		log.Info("sprite volume supports reflinks: new sprites and checkpoints are instant copy-on-write clones")
	} else {
		log.Info("sprite volume has no reflink support: new sprites and checkpoints are full sparse copies (see scripts/setup-storage.sh)")
	}
	s.images = newImageCache(filepath.Join(opts.DataDir, "vm"), opts.BaseImage, life.disk.admitHost, log)
	s.metrics = newMetrics(s)
	s.httpStats = newHTTPStats()
	s.guestEvents = newRateLimiter(guestEventBurst, guestEventRate)
	s.webhooks = startWebhooks(life.events, opts.Webhooks, log)
	if opts.AutoCheckpointInterval > 0 && opts.AutoCheckpointKeep > 0 {
		go s.autoCheckpoints()
	}
	if opts.Backup.Bucket != "" {
		s.backups = newBackupManager(s, backup.Config{Endpoint: opts.Backup.Endpoint,
			Bucket: opts.Backup.Bucket, Region: opts.Backup.Region,
			CredentialsFile: opts.Backup.CredentialsFile, KeyFile: opts.Backup.KeyFile,
			Parallel: opts.Backup.Parallel, RateLimit: opts.Backup.RateLimit, Log: log})
		life.backups = s.backups
	}
	s.leases = newLeases(s)
	life.setLeases(s.leases)
	// Once here, before anything is served: a lease that ran out while the
	// daemon was down has still run out, and the sprite should not come back.
	s.leases.sweep()
	return s
}

// named adapts a handler that takes its sprite as an argument to the public
// API, where the sprite comes from {name}. (The in-guest channel supplies it differently.)
func (s *Server) named(h func(http.ResponseWriter, *http.Request, store.Sprite, *guestChan)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if sp, ok := s.lookup(w, r); ok {
			h(w, r, sp, nil)
		}
	}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/sprites", s.createSprite)
	mux.HandleFunc("GET /v1/sprites", s.listSprites)
	mux.HandleFunc("GET /v1/sprites/{name}", s.getSprite)
	mux.HandleFunc("PUT /v1/sprites/{name}", s.updateSprite)
	mux.HandleFunc("DELETE /v1/sprites/{name}", s.deleteSprite)
	mux.HandleFunc("/v1/sprites/{name}/exec", s.proxyAgent)
	mux.HandleFunc("POST /v1/sprites/{name}/exec", s.execPost)
	mux.HandleFunc("/v1/sprites/{name}/exec/{rest...}", s.proxyAgent)
	mux.HandleFunc("GET /v1/sprites/{name}/proxy", s.proxyAgent)
	if !s.opts.NoControl {
		mux.HandleFunc("GET /v1/sprites/{name}/control", func(w http.ResponseWriter, r *http.Request) {
			if !s.offersControl(r) {
				writeErr(w, http.StatusNotFound, "not_found", "no such endpoint")
				return
			}
			s.controlRelay(w, r)
		})
	}
	mux.HandleFunc("GET /v1/sprites/{name}/ports/watch", s.proxyAgentSocket)
	mux.HandleFunc("/v1/sprites/{name}/fs/{rest...}", s.proxyAgent)
	mux.HandleFunc("/v1/sprites/{name}/services", s.proxyAgent)
	mux.HandleFunc("/v1/sprites/{name}/services/{rest...}", s.proxyAgent)
	mux.HandleFunc("POST /v1/sprites/{name}/checkpoint", s.named(s.createCheckpoint))
	mux.HandleFunc("GET /v1/sprites/{name}/checkpoints", s.named(s.listCheckpoints))
	mux.HandleFunc("GET /v1/sprites/{name}/checkpoints/{id}", s.named(s.getCheckpoint))
	mux.HandleFunc("DELETE /v1/sprites/{name}/checkpoints/{id}", s.named(s.deleteCheckpoint))
	mux.HandleFunc("POST /v1/sprites/{name}/checkpoints/{id}/restore", s.named(s.restoreCheckpoint))
	mux.HandleFunc("GET /v1/sprites/{name}/policy/network", s.getNetworkPolicy)
	mux.HandleFunc("POST /v1/sprites/{name}/policy/network", s.setNetworkPolicy)
	s.registerTasks(mux)
	s.registerPolicyLimits(mux)
	s.registerSpawnPolicy(mux)
	s.registerDomains(mux)
	// Ours, outside /v1 (events.go, webhooks.go, leases.go).
	s.registerLeases(mux)
	mux.HandleFunc("GET "+eventsPath, s.serveAPIEvents)
	mux.HandleFunc("GET /wisp/v1/webhooks", s.serveWebhookStatus)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeErr(w, http.StatusNotFound, "not_found", "no such endpoint")
	})
	ui := s.uiHandler()

	return s.instrument(s.kindOf, false, "", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch s.kindOf(r) {
		case kindSprite:
			name, _ := s.spriteForHost(r.Host)
			s.serveSpriteURL(w, r, name, false)
			return
		case kindUI:
			ui.ServeHTTP(w, r)
			return
		}
		w.Header().Set("Sprite-Version", apiVersion)
		got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if subtle.ConstantTimeCompare([]byte(got), []byte(s.token)) != 1 && !s.uiAuthorized(r) {
			writeErr(w, http.StatusUnauthorized, "unauthorized", "missing or invalid bearer token")
			return
		}
		mux.ServeHTTP(w, r)
	}))
}

// kindOf sorts a request on the API listener for the request metrics.
func (s *Server) kindOf(r *http.Request) string {
	if _, ok := s.spriteForHost(r.Host); ok {
		return kindSprite
	}
	if r.URL.Path == "/" || r.URL.Path == "/ui" || strings.HasPrefix(r.URL.Path, "/ui/") {
		return kindUI
	}
	return kindAPI
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]string{"error": code, "message": msg})
}

type spriteJSON struct {
	ID            string            `json:"id"`
	Name          string            `json:"name"`
	Organization  string            `json:"organization"`
	Status        string            `json:"status"`
	Config        store.Config      `json:"config"`
	Environment   map[string]string `json:"environment,omitempty"`
	URL           string            `json:"url"`
	URLSettings   store.URLSettings `json:"url_settings"`
	Labels        []string          `json:"labels,omitempty"`
	CreatedAt     time.Time         `json:"created_at"`
	UpdatedAt     time.Time         `json:"updated_at"`
	LastRunningAt *time.Time        `json:"last_running_at,omitempty"`
	LastWarmingAt *time.Time        `json:"last_warming_at,omitempty"`
	// ParentID is ours: the sprite that created this one from inside.
	ParentID string `json:"parent_id,omitempty"`
	// SourceImage is ours: the container image the sprite was created from.
	SourceImage string `json:"source_image,omitempty"`
	// ExpiresAt and Protected are ours: the workspace lease (leases.go). Absent
	// on the sprites that have none, which is most of them.
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
	Protected bool       `json:"protected,omitempty"`
	// Backup is ours, not upstream's: where this sprite's durability stands. The
	// SDKs ignore fields they do not know, and it is absent entirely when no bucket
	// is configured.
	Backup *backupState `json:"backup,omitempty"`
}

func (s *Server) render(sp store.Sprite) spriteJSON {
	return spriteJSON{
		ID: sp.ID, Name: sp.Name, Organization: s.org, Status: s.life.Status(sp),
		Config: sp.Config, Environment: sp.Environment, URL: fmt.Sprintf(s.urlFmt, sp.Name),
		URLSettings: sp.URLSettings, Labels: sp.Labels, CreatedAt: sp.CreatedAt, UpdatedAt: sp.UpdatedAt,
		LastRunningAt: sp.LastRunningAt, LastWarmingAt: sp.LastWarmingAt, ParentID: sp.ParentID,
		SourceImage: sp.Image, Backup: s.backups.State(sp.ID),
		ExpiresAt: sp.ExpiresAt, Protected: sp.Protected,
	}
}

// lookup resolves {name}, writing the 404 itself when absent.
func (s *Server) lookup(w http.ResponseWriter, r *http.Request) (store.Sprite, bool) {
	sp, err := s.store.Get(r.PathValue("name"))
	if err != nil {
		writeErr(w, http.StatusNotFound, "not_found", "sprite not found")
		return sp, false
	}
	return sp, true
}

func validAuth(a string) bool { return a == "sprite" || a == "public" }

// createRequest is POST /v1/sprites. From is our extension.
type createRequest struct {
	Name        string             `json:"name"`
	Config      *store.Config      `json:"config"`
	Environment map[string]string  `json:"environment"`
	Labels      []string           `json:"labels"`
	URLSettings *store.URLSettings `json:"url_settings"`
	// From starts the sprite as a clone of a checkpoint, or from a container
	// image, instead of the base image.
	From *cloneFrom `json:"from"`
	// The workspace lease, ours (leases.go); no lease without one of its fields.
	leaseRequest
}

func (s *Server) createSprite(w http.ResponseWriter, r *http.Request) { s.create(w, r, nil) }

// create serves the public API and, with parent set, a sprite creating one from
// inside (see spawn.go for what that changes).
func (s *Server) create(w http.ResponseWriter, r *http.Request, parent *store.Sprite) {
	var req createRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request", "invalid JSON body")
		return
	}
	if !nameRE.MatchString(req.Name) {
		writeErr(w, http.StatusBadRequest, "invalid_name", "name must be 1-63 chars of lowercase letters, digits and hyphens")
		return
	}
	// A refused create is reported under the name it asked for, and to the
	// spawner that asked, if one did.
	refused := func(lim *LimitError, which string) {
		e := Event{Type: "limit.refused", Sprite: req.Name, Detail: map[string]any{"limit": which, "max": lim.Limit, "current": lim.Current}}
		if parent != nil {
			e.ParentID = parent.ID
		}
		s.life.events.Publish(e)
		writeLimitErr(w, lim)
	}
	if limit, n := s.opts.MaxSprites, len(s.store.List("")); limit > 0 && n >= limit {
		refused(&LimitError{Code: codeSpriteLimit, Limit: limit, Current: n,
			Message: fmt.Sprintf("this host already holds %d sprites, the most it allows (--max-sprites); delete one first", n)}, "max_sprites")
		return
	}
	if parent != nil {
		if lim := s.childLimit(*parent); lim != nil {
			refused(lim, "max_children")
			return
		}
	}
	if req.URLSettings != nil && req.URLSettings.Auth != "" && !validAuth(req.URLSettings.Auth) {
		writeErr(w, http.StatusBadRequest, "bad_request", `url_settings.auth must be "sprite" or "public"`)
		return
	}

	now := time.Now().UTC()
	sp := &store.Sprite{ID: store.NewID(), Name: req.Name, Environment: req.Environment, Labels: req.Labels,
		URLSettings: store.URLSettings{Auth: "sprite"}, CreatedAt: now, UpdatedAt: now}
	if req.URLSettings != nil && req.URLSettings.Auth != "" {
		sp.URLSettings = *req.URLSettings
	}
	if msg := req.apply(sp, now); msg != "" {
		writeErr(w, http.StatusBadRequest, "bad_request", msg)
		return
	}

	var image string
	cloned := false
	var detail map[string]any
	switch {
	case req.From != nil && req.From.Image != "":
		if req.From.Sprite != "" || req.From.Checkpoint != "" {
			writeErr(w, http.StatusBadRequest, "bad_request", "from: give either image, or sprite and checkpoint, not both")
			return
		}
		// Checked again by the store; this only saves a pull that would be for nothing.
		if _, err := s.store.Get(req.Name); err == nil {
			writeErr(w, http.StatusBadRequest, "name_taken", "a sprite with that name already exists")
			return
		}
		disk, img, ref, release, err := s.imageSource(r.Context(), req.From.Image, parent != nil)
		if err != nil {
			writeErr(w, err.status, err.code, err.msg)
			return
		}
		// Held until the disk is cloned, so the cached image cannot be removed under the copy.
		defer release()
		image = disk
		sp.Image = ref.Name() + "@" + img.Digest
		if img.Digest == "" {
			sp.Image = ref.String()
		}
		detail = map[string]any{"from": map[string]string{"image": sp.Image}}
	case req.From != nil:
		src, cp, unlock, err := s.cloneSource(*req.From, parent)
		if err != nil {
			writeErr(w, err.status, err.code, err.msg)
			return
		}
		// Held until the image is cloned, so the checkpoint cannot be deleted under the copy.
		defer unlock()
		image = s.checkpointPath(src.ID, cp)
		detail = map[string]any{"from": map[string]string{"sprite": src.Name, "checkpoint": cp}}
		// A clone is the source's machine as well as its disk.
		sp.Config, sp.NetworkRules, sp.Privileges, sp.Resources = src.Config, src.NetworkRules, src.Privileges, src.Resources
		sp.Image = src.Image // the disk still descends from it
		cloned = true
	default:
		base, err := s.storage.base(r.Context())
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "internal", "provision disk: "+err.Error())
			return
		}
		image = base
	}
	if parent != nil {
		inherit(sp, *parent, cloned)
	} else if req.Config != nil {
		sp.Config = *req.Config
	}
	if err := s.life.disk.admit(*sp, "a new sprite", s.cloneCost(image)); err != nil {
		writeNoRoom(w, err)
		return
	}
	if err := s.store.Create(sp); err != nil {
		if errors.Is(err, store.ErrExists) {
			writeErr(w, http.StatusBadRequest, "name_taken", "a sprite with that name already exists")
			return
		}
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	disk := filepath.Join(s.store.Dir(sp.ID), vmm.DiskFile)
	if err := cloneFile(r.Context(), image, disk); err != nil {
		s.store.Delete(sp.Name)
		writeErr(w, http.StatusInternalServerError, "internal", "provision disk: "+err.Error())
		return
	}
	s.log.Info("sprite created", "sprite", sp.Name, "id", sp.ID, "net_index", sp.NetIndex, "parent", sp.ParentID, "cloned", cloned, "image", sp.Image)
	s.life.emit(*sp, "sprite.created", detail)
	// A sprite can be born already inside the warning window -- a lobby child with
	// a two-minute lease, say, under a five-minute --lease-warning. The janitor
	// would never get to warn about it, so the warning is evaluated here too,
	// after sprite.created, keeping the stream's order honest.
	s.leases.warn(*sp, time.Now())
	writeJSON(w, http.StatusCreated, s.render(*sp))
}

// cloneFile copies a disk image, as a reflink where the filesystem supports
// it (instant, copy-on-write) and as a sparse copy otherwise.
func cloneFile(ctx context.Context, src, dst string) error {
	tmp := dst + ".tmp"
	out, err := exec.CommandContext(ctx, "cp", "--reflink=auto", "--sparse=always", src, tmp).CombinedOutput()
	if err != nil {
		os.Remove(tmp)
		return fmt.Errorf("%v: %s", err, strings.TrimSpace(string(out)))
	}
	return os.Rename(tmp, dst)
}

func (s *Server) listSprites(w http.ResponseWriter, r *http.Request) {
	s.list(w, r, func(store.Sprite) bool { return true })
}

func (s *Server) list(w http.ResponseWriter, r *http.Request, keep func(store.Sprite) bool) {
	q := r.URL.Query()
	max := 50
	if n, err := strconv.Atoi(q.Get("max_results")); err == nil && n >= 1 && n <= 50 {
		max = n
	}
	after := q.Get("continuation_token")
	resp := struct {
		Sprites               []spriteJSON `json:"sprites"`
		Org                   orgJSON      `json:"org"`
		HasMore               bool         `json:"has_more"`
		NextContinuationToken string       `json:"next_continuation_token,omitempty"`
	}{Sprites: []spriteJSON{}, Org: s.orgInfo()}
	for _, sp := range s.store.List(q.Get("prefix")) {
		if sp.Name <= after || !keep(sp) {
			continue
		}
		if len(resp.Sprites) == max {
			resp.HasMore = true
			resp.NextContinuationToken = resp.Sprites[max-1].Name
			break
		}
		resp.Sprites = append(resp.Sprites, s.render(sp))
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) getSprite(w http.ResponseWriter, r *http.Request) {
	if sp, ok := s.lookup(w, r); ok {
		writeJSON(w, http.StatusOK, s.render(sp))
	}
}

func (s *Server) updateSprite(w http.ResponseWriter, r *http.Request) {
	var req struct {
		URLSettings *store.URLSettings `json:"url_settings"`
		Labels      []string           `json:"labels"`
		ClearLabels bool               `json:"clear_labels"`
		// The lease, ours (leases.go). It is not written here: it goes through
		// the one path that is serialized against the reaper.
		leaseRequest
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request", "invalid JSON body")
		return
	}
	if req.URLSettings != nil && !validAuth(req.URLSettings.Auth) {
		writeErr(w, http.StatusBadRequest, "bad_request", `url_settings.auth must be "sprite" or "public"`)
		return
	}
	// The lease first: a sprite the reaper has taken is not one to relabel
	// either, and this is where that is noticed.
	if req.touchesExpiry() || req.Protected != nil {
		if _, ok := s.applyLease(w, r, req.leaseRequest); !ok {
			return
		}
	}
	sp, err := s.store.Update(r.PathValue("name"), func(sp *store.Sprite) {
		if req.URLSettings != nil {
			sp.URLSettings = *req.URLSettings
		}
		if req.Labels != nil {
			sp.Labels = req.Labels
		}
		if req.ClearLabels {
			sp.Labels = nil
		}
		sp.UpdatedAt = time.Now().UTC()
	})
	if err != nil {
		writeErr(w, http.StatusNotFound, "not_found", "sprite not found")
		return
	}
	writeJSON(w, http.StatusOK, s.render(sp))
}

func (s *Server) deleteSprite(w http.ResponseWriter, r *http.Request) {
	if sp, ok := s.lookup(w, r); ok {
		s.remove(w, sp)
	}
}

func (s *Server) remove(w http.ResponseWriter, sp store.Sprite) {
	if err := s.destroy(sp); err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// destroy is the deletion itself, with no request behind it: an expiring lease
// (leases.go) frees exactly what a DELETE frees — net index, tap, disk,
// checkpoints, domains — because it is the same code and not a second copy of
// the list.
func (s *Server) destroy(sp store.Sprite) error {
	s.life.Stop(sp, false)
	if err := s.store.Delete(sp.Name); err != nil {
		return err
	}
	s.life.Forget(sp.ID)
	s.life.egress.forget(sp)
	s.syncDomains() // its custom domains go with it
	// Tombstone rather than delete: losing this machine and deleting a sprite must
	// not look the same to the bucket. `wispd backups prune` retires it later.
	s.backups.MarkDeleted(sp)
	s.log.Info("sprite deleted", "sprite", sp.Name)
	s.life.emit(sp, "sprite.deleted", nil)
	return nil
}

// proxyAgent wakes the sprite and forwards the request (HTTP or WebSocket) to
// the guest agent over vsock. The sprite is pinned awake until it completes.
func (s *Server) proxyAgent(w http.ResponseWriter, r *http.Request) { s.proxy(w, r, true) }

// proxyAgentSocket is proxyAgent for WebSockets that clients hold open while
// doing nothing (pooled control channels, port watchers). Pinning those would
// keep the sprite awake forever, so the pin is dropped once the agent accepts
// the socket; from then on the guest's own activity report decides, and a
// suspend simply closes the socket.
func (s *Server) proxyAgentSocket(w http.ResponseWriter, r *http.Request) { s.proxy(w, r, false) }

// withSpriteEnv puts the sprite-level environment first so per-exec env can override it.
func withSpriteEnv(q url.Values, sp store.Sprite) url.Values {
	env := []string{}
	for k, v := range sp.Environment {
		env = append(env, k+"="+v)
	}
	if len(env) > 0 {
		q["env"] = append(env, q["env"]...)
	}
	return q
}

func (s *Server) proxy(w http.ResponseWriter, r *http.Request, pin bool) {
	sp, ok := s.lookup(w, r)
	if !ok {
		return
	}
	m, release, err := s.life.Acquire(r.Context(), sp)
	if err != nil {
		s.writeWakeErr(w, sp.Name, err)
		return
	}
	defer release()

	path := strings.TrimPrefix(r.URL.Path, "/v1/sprites/"+sp.Name)
	proxy := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL.Scheme, pr.Out.URL.Host, pr.Out.URL.Path = "http", "agent", path
			pr.Out.Header.Del("Authorization")
			pr.Out.Header.Del("Cookie") // the web UI's session is ours too
			if path == "/exec" || path == "/control" {
				pr.Out.URL.RawQuery = withSpriteEnv(pr.Out.URL.Query(), sp).Encode()
			}
		},
		Transport: &http.Transport{DisableKeepAlives: true,
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) { return m.Dial(ctx) }},
		FlushInterval: -1,
		ModifyResponse: func(resp *http.Response) error {
			if !pin && resp.StatusCode == http.StatusSwitchingProtocols {
				release()
			}
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
			writeErr(w, http.StatusBadGateway, "agent_unreachable", err.Error())
		},
	}
	proxy.ServeHTTP(w, r)
}
