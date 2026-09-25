package server

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"io/fs"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/arugula-salad/wisp/internal/webui"
	"github.com/gorilla/websocket"
)

// The web UI: a single page served from the API listener at /ui/, talking to
// the ordinary /v1 API plus a few /ui/api endpoints for what the API has no
// place for (the operator status, metrics history, suspend and wake).
//
// A browser cannot put a bearer token on a WebSocket, so the page trades the
// token for a cookie. The cookie alone authorizes nothing: a request must also
// carry the X-Wisp-UI header, which another origin cannot add without
// a CORS preflight we never answer, or be a WebSocket whose Origin is this very
// host. That keeps a page elsewhere (a sprite's own URL included) from riding
// the cookie.

const (
	uiCookie = "wisp_ui"
	uiHeader = "X-Wisp-UI"
)

// uiSession is the cookie value: derived from the token, so rotating the token
// signs every browser out and there is no session state to keep.
func (s *Server) uiSession() string {
	mac := hmac.New(sha256.New, []byte(s.token))
	mac.Write([]byte("wisp web ui v1"))
	return hex.EncodeToString(mac.Sum(nil))
}

func (s *Server) uiAuthorized(r *http.Request) bool {
	c, err := r.Cookie(uiCookie)
	if err != nil || subtle.ConstantTimeCompare([]byte(c.Value), []byte(s.uiSession())) != 1 {
		return false
	}
	if r.Header.Get(uiHeader) == "1" {
		return true
	}
	return websocket.IsWebSocketUpgrade(r) && sameOrigin(r)
}

func sameOrigin(r *http.Request) bool {
	u, err := url.Parse(r.Header.Get("Origin"))
	return err == nil && u.Host != "" && strings.EqualFold(u.Host, r.Host)
}

func (s *Server) uiHandler() http.Handler {
	mux := http.NewServeMux()
	static, _ := fs.Sub(webui.Static, "static")
	files := http.StripPrefix("/ui/", http.FileServer(http.FS(static)))
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/ui/", http.StatusFound)
	})
	mux.HandleFunc("GET /ui", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/ui/", http.StatusFound)
	})
	mux.HandleFunc("GET /ui/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; style-src 'self' 'unsafe-inline'; connect-src 'self'; img-src 'self' data:; frame-ancestors 'none'")
		files.ServeHTTP(w, r)
	})

	mux.HandleFunc("POST /ui/login", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Token string `json:"token"`
		}
		if json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req) != nil ||
			subtle.ConstantTimeCompare([]byte(strings.TrimSpace(req.Token)), []byte(s.token)) != 1 {
			time.Sleep(500 * time.Millisecond) // guessing costs something
			writeErr(w, http.StatusUnauthorized, "unauthorized", "that is not this host's API token")
			return
		}
		http.SetCookie(w, &http.Cookie{Name: uiCookie, Value: s.uiSession(), Path: "/", HttpOnly: true,
			SameSite: http.SameSiteStrictMode, Secure: r.TLS != nil, MaxAge: 30 * 24 * 3600})
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /ui/logout", func(w http.ResponseWriter, r *http.Request) {
		http.SetCookie(w, &http.Cookie{Name: uiCookie, Path: "/", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteStrictMode})
		w.WriteHeader(http.StatusNoContent)
	})

	api := http.NewServeMux()
	api.HandleFunc("GET /ui/api/status", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, s.status(r.Context(), s.started, s.opts.Listen))
	})
	api.HandleFunc("GET /ui/api/metrics", func(w http.ResponseWriter, r *http.Request) {
		since, _ := time.Parse(time.RFC3339, r.URL.Query().Get("since"))
		writeJSON(w, http.StatusOK, s.metrics.snapshot(since))
	})
	api.HandleFunc("GET /ui/api/http", func(w http.ResponseWriter, r *http.Request) {
		v := r.URL.Query()
		q := httpQuery{Range: time.Hour, Sprite: v.Get("sprite"), Public: v.Get("listener"), Minute: v.Get("res") == "minute"}
		if n, err := strconv.Atoi(v.Get("range")); err == nil {
			q.Range = min(max(time.Duration(n)*time.Second, 5*time.Minute), httpCoarseKeep)
		}
		if k := v.Get("kinds"); k != "" {
			q.Kinds = map[string]bool{}
			for _, kind := range strings.Split(k, ",") {
				q.Kinds[kind] = true
			}
		}
		writeJSON(w, http.StatusOK, s.httpStats.query(q, time.Now()))
	})
	api.HandleFunc("POST /ui/api/sprites/{name}/wake", func(w http.ResponseWriter, r *http.Request) {
		sp, ok := s.lookup(w, r)
		if !ok {
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), time.Minute)
		defer cancel()
		_, release, err := s.life.Acquire(ctx, sp)
		if err != nil {
			s.writeWakeErr(w, sp.Name, err)
			return
		}
		release()
		writeJSON(w, http.StatusOK, s.render(sp))
	})
	// suspend keeps memory (warm); cool drops a warm sprite's snapshot. A running
	// sprite is never killed from here: that would lose whatever it had not synced.
	api.HandleFunc("POST /ui/api/sprites/{name}/suspend", func(w http.ResponseWriter, r *http.Request) {
		sp, ok := s.lookup(w, r)
		if !ok {
			return
		}
		if err := s.life.Stop(sp, true); err != nil {
			writeErr(w, http.StatusInternalServerError, "suspend_failed", err.Error())
			return
		}
		writeJSON(w, http.StatusOK, s.render(sp))
	})
	api.HandleFunc("POST /ui/api/sprites/{name}/cool", func(w http.ResponseWriter, r *http.Request) {
		sp, ok := s.lookup(w, r)
		if !ok {
			return
		}
		if !s.life.Cool(sp) {
			writeErr(w, http.StatusConflict, "not_warm", "only a suspended (warm) sprite can be sent cold")
			return
		}
		writeJSON(w, http.StatusOK, s.render(sp))
	})
	api.HandleFunc("/ui/api/", func(w http.ResponseWriter, r *http.Request) {
		writeErr(w, http.StatusNotFound, "not_found", "no such endpoint")
	})
	guarded := func(w http.ResponseWriter, r *http.Request) {
		if !s.uiAuthorized(r) {
			writeErr(w, http.StatusUnauthorized, "unauthorized", "sign in to the web UI first")
			return
		}
		api.ServeHTTP(w, r)
	}
	// By method, since a bare "/ui/api/" would conflict with "GET /ui/".
	mux.HandleFunc("GET /ui/api/", guarded)
	mux.HandleFunc("POST /ui/api/", guarded)
	return mux
}
