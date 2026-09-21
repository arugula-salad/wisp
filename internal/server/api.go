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

	"github.com/jhgaylor/mini-sprites/internal/store"
	"github.com/jhgaylor/mini-sprites/internal/vmm"
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
}

func New(opts Options, st *store.Store, life *Lifecycle, log *slog.Logger, token, org, urlDomain, port string) *Server {
	s := &Server{opts: opts, store: st, life: life, log: log, token: token, org: org,
		urlDomain: urlDomain, urlFmt: "http://%s." + urlDomain + ":" + port}
	life.guestAPI = s.guestAPI
	if opts.AutoCheckpointInterval > 0 && opts.AutoCheckpointKeep > 0 {
		go s.autoCheckpoints()
	}
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
		mux.HandleFunc("GET /v1/sprites/{name}/control", s.proxyAgentSocket)
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
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeErr(w, http.StatusNotFound, "not_found", "no such endpoint")
	})

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if name, ok := s.spriteForHost(r.Host); ok {
			s.serveSpriteURL(w, r, name)
			return
		}
		w.Header().Set("Sprite-Version", apiVersion)
		got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if subtle.ConstantTimeCompare([]byte(got), []byte(s.token)) != 1 {
			writeErr(w, http.StatusUnauthorized, "unauthorized", "missing or invalid bearer token")
			return
		}
		mux.ServeHTTP(w, r)
	})
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
}

func (s *Server) render(sp store.Sprite) spriteJSON {
	return spriteJSON{
		ID: sp.ID, Name: sp.Name, Organization: s.org, Status: s.life.Status(sp),
		Config: sp.Config, Environment: sp.Environment, URL: fmt.Sprintf(s.urlFmt, sp.Name),
		URLSettings: sp.URLSettings, Labels: sp.Labels, CreatedAt: sp.CreatedAt, UpdatedAt: sp.UpdatedAt,
		LastRunningAt: sp.LastRunningAt, LastWarmingAt: sp.LastWarmingAt,
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

func (s *Server) createSprite(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name        string             `json:"name"`
		Config      *store.Config      `json:"config"`
		Environment map[string]string  `json:"environment"`
		Labels      []string           `json:"labels"`
		URLSettings *store.URLSettings `json:"url_settings"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request", "invalid JSON body")
		return
	}
	if !nameRE.MatchString(req.Name) {
		writeErr(w, http.StatusBadRequest, "invalid_name", "name must be 1-63 chars of lowercase letters, digits and hyphens")
		return
	}
	now := time.Now().UTC()
	sp := &store.Sprite{ID: store.NewID(), Name: req.Name, Environment: req.Environment, Labels: req.Labels,
		URLSettings: store.URLSettings{Auth: "sprite"}, CreatedAt: now, UpdatedAt: now}
	if req.Config != nil {
		sp.Config = *req.Config
	}
	if req.URLSettings != nil && req.URLSettings.Auth != "" {
		if !validAuth(req.URLSettings.Auth) {
			writeErr(w, http.StatusBadRequest, "bad_request", `url_settings.auth must be "sprite" or "public"`)
			return
		}
		sp.URLSettings = *req.URLSettings
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
	if err := cloneFile(r.Context(), s.opts.BaseImage, disk); err != nil {
		s.store.Delete(sp.Name)
		writeErr(w, http.StatusInternalServerError, "internal", "provision disk: "+err.Error())
		return
	}
	s.log.Info("sprite created", "sprite", sp.Name, "id", sp.ID, "net_index", sp.NetIndex)
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
	q := r.URL.Query()
	max := 50
	if n, err := strconv.Atoi(q.Get("max_results")); err == nil && n >= 1 && n <= 50 {
		max = n
	}
	after := q.Get("continuation_token")
	resp := struct {
		Sprites               []spriteJSON `json:"sprites"`
		HasMore               bool         `json:"has_more"`
		NextContinuationToken string       `json:"next_continuation_token,omitempty"`
	}{Sprites: []spriteJSON{}}
	for _, sp := range s.store.List(q.Get("prefix")) {
		if sp.Name <= after {
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
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request", "invalid JSON body")
		return
	}
	if req.URLSettings != nil && !validAuth(req.URLSettings.Auth) {
		writeErr(w, http.StatusBadRequest, "bad_request", `url_settings.auth must be "sprite" or "public"`)
		return
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
	sp, ok := s.lookup(w, r)
	if !ok {
		return
	}
	s.life.Stop(sp, false)
	if err := s.store.Delete(sp.Name); err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	s.life.Forget(sp.ID)
	s.life.egress.forget(sp)
	s.log.Info("sprite deleted", "sprite", sp.Name)
	w.WriteHeader(http.StatusNoContent)
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
		s.log.Error("wake failed", "sprite", sp.Name, "err", err)
		writeErr(w, http.StatusServiceUnavailable, "wake_failed", err.Error())
		return
	}
	defer release()

	path := strings.TrimPrefix(r.URL.Path, "/v1/sprites/"+sp.Name)
	proxy := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL.Scheme, pr.Out.URL.Host, pr.Out.URL.Path = "http", "agent", path
			pr.Out.Header.Del("Authorization")
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
