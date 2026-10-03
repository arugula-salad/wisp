package server

import (
	"errors"
	"net/http"
	"time"

	"github.com/arugula-salad/wisp/internal/store"
)

// The Sprites side of leases (leases.go has the reaper): the lease fields of a
// create or an update, and the renewal endpoint.
//
// The endpoint lives outside /v1, like the event stream and the webhook status,
// so it cannot collide with anything upstream has or adds; the fields on a
// sprite ride along in upstream's shape, where an SDK that does not know them
// ignores them.

// leasePath is the renewal endpoint, ours, outside /v1.
const leasePath = "/wisp/v1/sprites/{name}/lease"

// leaseRequest is the lease in a create, an update or a renewal. There are two
// ways to name the moment because callers differ: an operator has a date in
// mind, a lobby handing out a sprite knows only how long it should live. A
// field left out changes nothing, so a caller that knows nothing of leases
// cannot clear one by accident; expires_at "" or ttl_seconds 0 clears it.
type leaseRequest struct {
	ExpiresAt  *string `json:"expires_at"`
	TTLSeconds *int64  `json:"ttl_seconds"`
	Protected  *bool   `json:"protected"`
}

func (q leaseRequest) touchesExpiry() bool { return q.ExpiresAt != nil || q.TTLSeconds != nil }

// expiry resolves the deadline asked for: nil with an empty message means no
// lease, and a non-empty message is what the caller returns as a 400.
func (q leaseRequest) expiry(now time.Time) (*time.Time, string) {
	switch {
	case q.ExpiresAt != nil && q.TTLSeconds != nil:
		return nil, "give either expires_at or ttl_seconds, not both"
	case q.TTLSeconds != nil:
		if *q.TTLSeconds < 0 {
			return nil, "ttl_seconds must not be negative"
		}
		if *q.TTLSeconds == 0 {
			return nil, ""
		}
		t := now.Add(time.Duration(*q.TTLSeconds) * time.Second).UTC()
		return &t, ""
	case q.ExpiresAt != nil:
		if *q.ExpiresAt == "" {
			return nil, ""
		}
		t, err := time.Parse(time.RFC3339, *q.ExpiresAt)
		if err != nil {
			return nil, "expires_at must be an RFC3339 time, e.g. 2026-09-23T10:00:00Z"
		}
		if t.Before(now) {
			// A deadline already past would be reaped within the half minute,
			// which is a lot of deletion for a mistyped year.
			return nil, "expires_at is in the past"
		}
		t = t.UTC()
		return &t, ""
	}
	return nil, ""
}

// apply writes the lease onto a record that is not in the store yet (a create,
// where there is nothing to race with).
func (q leaseRequest) apply(sp *store.Sprite, now time.Time) string {
	exp, msg := q.expiry(now)
	if msg != "" {
		return msg
	}
	if q.touchesExpiry() {
		sp.ExpiresAt = exp
	}
	if q.Protected != nil {
		sp.Protected = *q.Protected
	}
	return ""
}

// leaseJSON is the lease on its own. ExpiresAt is explicitly null rather than
// absent when there is none, so a client can tell "no lease" from an old daemon.
type leaseJSON struct {
	ExpiresAt *time.Time `json:"expires_at"`
	Protected bool       `json:"protected"`
	// ExpiresIn saves every caller the clock comparison, and is negative for a
	// deadline that has passed on a protected sprite.
	ExpiresIn *int64 `json:"expires_in_seconds,omitempty"`
}

func leaseOf(sp store.Sprite) leaseJSON {
	out := leaseJSON{ExpiresAt: sp.ExpiresAt, Protected: sp.Protected}
	if sp.ExpiresAt != nil {
		in := int64(time.Until(*sp.ExpiresAt).Round(time.Second) / time.Second)
		out.ExpiresIn = &in
	}
	return out
}

func (s *Server) registerLeases(mux *http.ServeMux) {
	mux.HandleFunc("GET "+leasePath, func(w http.ResponseWriter, r *http.Request) {
		if sp, ok := s.lookup(w, r); ok {
			writeJSON(w, http.StatusOK, leaseOf(sp))
		}
	})
	mux.HandleFunc("POST "+leasePath, func(w http.ResponseWriter, r *http.Request) {
		var req leaseRequest
		if !readJSON(w, r, 1<<16, &req) {
			return
		}
		if sp, ok := s.applyLease(w, r, req); ok {
			writeJSON(w, http.StatusOK, leaseOf(sp))
		}
	})
	// Dropping the lease drops the protection with it: protection only ever
	// meant "not this deadline", and leaving it set on a sprite with no deadline
	// would read as a promise nothing enforces.
	mux.HandleFunc("DELETE "+leasePath, func(w http.ResponseWriter, r *http.Request) {
		none, off := "", false
		if _, ok := s.applyLease(w, r, leaseRequest{ExpiresAt: &none, Protected: &off}); ok {
			w.WriteHeader(http.StatusNoContent)
		}
	})
}

// applyLease is every lease change from outside: the renewal endpoint and the
// lease fields of PUT /v1/sprites/{name}. A lease is the engine's deadline with
// its default action, delete; the write is Lifecycle.ChangeDeadline,
// serialized against a reap in flight.
func (s *Server) applyLease(w http.ResponseWriter, r *http.Request, req leaseRequest) (store.Sprite, bool) {
	sp, ok := s.lookup(w, r)
	if !ok {
		return sp, false
	}
	exp, msg := req.expiry(time.Now())
	if msg != "" {
		writeErr(w, http.StatusBadRequest, "bad_request", msg)
		return sp, false
	}
	cur, err := s.life.ChangeDeadline(sp.ID, func(d *Deadline) {
		if req.touchesExpiry() {
			d.At = exp
		}
		if req.Protected != nil {
			d.Protected = *req.Protected
		}
	})
	switch {
	case errors.Is(err, errLeaseReaping):
		writeErr(w, http.StatusConflict, "expired",
			"this sprite's lease ran out and it is being deleted; create a new sprite")
		return sp, false
	case err != nil:
		writeErr(w, http.StatusNotFound, "not_found", "sprite not found")
		return sp, false
	}
	sp.Record = cur
	return sp, true
}
