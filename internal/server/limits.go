package server

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/arugula-salad/wisp/engine"
	"github.com/arugula-salad/wisp/internal/store"
)

// Operator ceilings (Options.MaxSprites, Options.MaxRunning) and the org block
// of the list response. Errors use upstream's shape, which the SDKs parse into
// their APIError: {error, message, limit, current_count, retry_after_seconds}.
// The host memory budget and the concurrent-boot cap are in admission.go and
// report themselves through the same engine.LimitError.

const (
	// codeConcurrentLimit is upstream's code for too many sprites running at once; every engine ceiling (limitCode).
	codeConcurrentLimit = "concurrent_sprite_limit_exceeded"
	codeSpriteLimit     = "sprite_limit_exceeded"
)

// limitCode is upstream's error code for e. The engine's ceilings leave Code
// empty, and every one of them keeps a sprite from running, which upstream
// reports as codeConcurrentLimit.
func limitCode(e *engine.LimitError) string {
	if e.Code != "" {
		return e.Code
	}
	return codeConcurrentLimit
}

func writeLimitErr(w http.ResponseWriter, e *engine.LimitError) {
	status := http.StatusForbidden
	if e.RetryAfter > 0 {
		// Only a limit that clears by itself is worth a client's retry loop.
		status = http.StatusTooManyRequests
		w.Header().Set("Retry-After", strconv.Itoa(e.RetryAfter))
	}
	writeJSON(w, status, map[string]any{"error": limitCode(e), "message": e.Message,
		"limit": e.Limit, "current_count": e.Current, "retry_after_seconds": e.RetryAfter})
}

// writeNoRoom answers a create, checkpoint or restore the disk guard refused.
func writeNoRoom(w http.ResponseWriter, err error) {
	writeErr(w, http.StatusInsufficientStorage, "insufficient_storage", err.Error())
}

// writeWakeErr answers a request whose sprite could not be made to run.
func (s *Server) writeWakeErr(w http.ResponseWriter, sprite string, err error) {
	var lim *engine.LimitError
	if errors.As(err, &lim) {
		s.log.Warn("wake refused", "sprite", sprite, "reason", err)
		writeLimitErr(w, lim)
		return
	}
	s.log.Error("wake failed", "sprite", sprite, "err", err)
	writeErr(w, http.StatusServiceUnavailable, "wake_failed", err.Error())
}

type orgJSON struct {
	Name    string `json:"name"`
	Running int    `json:"running"`
	Warm    int    `json:"warm"`
	Cold    int    `json:"cold"`
	// A limit of 0 means none is configured. There is no warm limit here: what
	// bounds warm sprites is the volume, and the disk guard turns the oldest cold.
	RunningLimit int `json:"running_limit"`
	WarmLimit    int `json:"warm_limit"`
}

// orgInfo counts every sprite, not just the page being listed.
func (s *Server) orgInfo() orgJSON {
	org := orgJSON{Name: s.org, RunningLimit: s.opts.MaxRunning}
	for _, sp := range s.store.List(store.Sprites, "") {
		switch s.life.Status(sp.Record) {
		case "running":
			org.Running++
		case "warm":
			org.Warm++
		default:
			org.Cold++
		}
	}
	return org
}
