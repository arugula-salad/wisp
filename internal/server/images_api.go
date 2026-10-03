package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"

	"github.com/arugula-salad/wisp/engine"
	"github.com/arugula-salad/wisp/internal/ociimage"
)

// The Sprites side of the image cache (engine/images.go): resolving from.image for a
// create, and the operator socket's routes.

// imageSource resolves from.image for a create. From inside a sprite
// (parent set) only a cached image is accepted.
func (s *Server) imageSource(ctx context.Context, raw string, parent bool) (disk string, img engine.CachedImage, ref ociimage.Ref, release func(), _ *createError) {
	ref, err := ociimage.ParseRef(raw)
	if err != nil {
		return "", img, ref, nil, &createError{http.StatusBadRequest, "bad_request", "from.image: " + err.Error()}
	}
	disk, img, release, err = s.images.Acquire(ctx, ref, !parent)
	switch {
	case err == nil:
		return disk, img, ref, release, nil
	case errors.Is(err, engine.ErrImageNotCached):
		return "", img, ref, nil, &createError{http.StatusNotFound, "image_not_cached",
			fmt.Sprintf("image %s is not in this host's image cache; from inside a sprite only cached images can be used (the operator adds them with `wispd images pull`)", ref)}
	case errors.Is(err, engine.ErrNoRoom):
		return "", img, ref, nil, &createError{http.StatusInsufficientStorage, "insufficient_storage", err.Error()}
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return "", img, ref, nil, &createError{http.StatusGatewayTimeout, "image_pull_pending",
			fmt.Sprintf("still pulling %s; the pull continues in the background, retry the create shortly", ref)}
	}
	return "", img, ref, nil, &createError{http.StatusBadGateway, "image_pull_failed", err.Error()}
}

// Operator socket routes (status.go mounts them): the cache is managed by
// whoever owns the data directory, never through the API token.
func (s *Server) registerImageOps(mux *http.ServeMux) {
	mux.HandleFunc("GET /images", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, s.images.List())
	})
	// POST /images/pull?ref=... streams podman's progress as plain text, then a
	// final line: "ok <json>" or "error <message>".
	mux.HandleFunc("POST /images/pull", func(w http.ResponseWriter, r *http.Request) {
		ref, err := ociimage.ParseRef(r.URL.Query().Get("ref"))
		if err != nil {
			writeErr(w, http.StatusBadRequest, "bad_request", err.Error())
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		fw := &flushWriter{w: w}
		defer fw.close() // the pull may outlive this request and keep writing
		img, err := s.images.Pull(r.Context(), ref, fw)
		if err != nil {
			fmt.Fprintf(fw, "error %s\n", strings.ReplaceAll(err.Error(), "\n", " "))
			return
		}
		b, _ := json.Marshal(s.images.DiskFigures(img))
		fmt.Fprintf(fw, "ok %s\n", b)
	})
	mux.HandleFunc("DELETE /images", func(w http.ResponseWriter, r *http.Request) {
		img, err := s.images.Remove(r.URL.Query().Get("key"))
		if errors.Is(err, engine.ErrImageNotFound) {
			writeErr(w, http.StatusNotFound, "not_found", err.Error())
			return
		}
		if err != nil {
			writeErr(w, http.StatusBadRequest, "bad_request", err.Error())
			return
		}
		s.log.Info("image removed from the cache", "id", img.ID[:12], "refs", strings.Join(img.Refs, ","))
		writeJSON(w, http.StatusOK, img)
	})
}

// flushWriter pushes each write to the client at once; podman's progress
// arrives in small pieces worth seeing as they come.
type flushWriter struct {
	mu     sync.Mutex
	w      http.ResponseWriter
	closed bool
}

func (f *flushWriter) close() {
	f.mu.Lock()
	f.closed = true
	f.mu.Unlock()
}

func (f *flushWriter) Write(p []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return len(p), nil
	}
	n, err := f.w.Write(p)
	if fl, ok := f.w.(http.Flusher); ok {
		fl.Flush()
	}
	return n, err
}
