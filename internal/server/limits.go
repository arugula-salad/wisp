package server

import (
	"errors"
	"fmt"
	"math"
	"net/http"
	"strconv"
)

// Operator ceilings (Options.MaxSprites, Options.MaxRunning) and the org block
// of the list response. Errors use upstream's shape, which the SDKs parse into
// their APIError: {error, message, limit, current_count, retry_after_seconds}.
// The host memory budget and the concurrent-boot cap are in admission.go and
// report themselves through the same LimitError.

const (
	// codeConcurrentLimit is upstream's code for too many sprites running at once.
	codeConcurrentLimit = "concurrent_sprite_limit_exceeded"
	codeSpriteLimit     = "sprite_limit_exceeded"
)

// LimitError is a configured ceiling being hit.
type LimitError struct {
	Code    string
	Message string
	// Which names the ceiling for the limit.refused event (docs/events.md).
	// Several ceilings share one upstream Code, so this is what tells them apart.
	Which      string
	Limit      int
	Current    int
	RetryAfter int // seconds; 0 when waiting will not help
}

func (e *LimitError) Error() string { return e.Message }

func writeLimitErr(w http.ResponseWriter, e *LimitError) {
	status := http.StatusForbidden
	if e.RetryAfter > 0 {
		// Only a limit that clears by itself is worth a client's retry loop.
		status = http.StatusTooManyRequests
		w.Header().Set("Retry-After", strconv.Itoa(e.RetryAfter))
	}
	writeJSON(w, status, map[string]any{"error": e.Code, "message": e.Message,
		"limit": e.Limit, "current_count": e.Current, "retry_after_seconds": e.RetryAfter})
}

// writeWakeErr answers a request whose sprite could not be made to run.
func (s *Server) writeWakeErr(w http.ResponseWriter, sprite string, err error) {
	var lim *LimitError
	if errors.As(err, &lim) {
		s.log.Warn("wake refused", "sprite", sprite, "reason", err)
		writeLimitErr(w, lim)
		return
	}
	s.log.Error("wake failed", "sprite", sprite, "err", err)
	writeErr(w, http.StatusServiceUnavailable, "wake_failed", err.Error())
}

// reserveRun claims one of MaxRunning slots for a VM about to start. Counting
// here, under one lock, is what keeps concurrent wakes from overshooting.
func (l *Lifecycle) reserveRun() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if limit := l.opts.MaxRunning; limit > 0 && l.running >= limit {
		// An idle sprite frees its slot after IdleTimeout, so that is when to look again.
		retry := max(int(math.Ceil(l.opts.IdleTimeout.Seconds())), 1)
		return &LimitError{Code: codeConcurrentLimit, Which: "max_running", Limit: limit, Current: l.running, RetryAfter: retry,
			Message: fmt.Sprintf("%d sprites are already running, the most this host allows (--max-running); one frees up when a sprite goes idle", l.running)}
	}
	l.running++
	return nil
}

func (l *Lifecycle) releaseRun() {
	l.mu.Lock()
	l.running--
	l.mu.Unlock()
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
	for _, sp := range s.store.List("") {
		switch s.life.Status(sp) {
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
