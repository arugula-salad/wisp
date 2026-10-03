package server

import (
	"net/http"
	"regexp"

	"github.com/arugula-salad/wisp/engine"
	"github.com/arugula-salad/wisp/internal/store"
)

// Events and the guest channel, both ways:
//
//   - A spawner watches its children: GET eventsPath from inside streams the
//     events whose parent_id is the asking sprite, and nothing else. A lobby
//     can show live game status without a token, and without seeing a thing
//     about any other sprite. Like the rest of spawning it needs the policy.
//   - The agent reports its services' starts, crashes and stops, which only
//     it sees. Those come from inside the guest, so they are taken as claims
//     about that one sprite, checked for shape, and rate limited: a guest can
//     at worst fill the stream with lies about itself, slowly.

// serviceReport is what the agent posts to /internal/service-event
// (agent.ServiceReport).
type serviceReport struct {
	Type         string `json:"type"` // started, crashed, stopped, failed
	Service      string `json:"service"`
	PID          int    `json:"pid,omitempty"`
	ExitCode     *int   `json:"exit_code,omitempty"`
	RestartCount int    `json:"restart_count,omitempty"`
	RestartInMS  int64  `json:"restart_in_ms,omitempty"`
	Error        string `json:"error,omitempty"`
}

var reportServiceRE = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,62}$`)

// Guest-reported events: a burst of guestEventBurst, then guestEventRate a second.
const (
	guestEventBurst = 30
	guestEventRate  = 5
)

// serviceEvent turns a report into an event, or refuses it.
func serviceEvent(sp store.Sprite, r serviceReport) (engine.Event, bool) {
	if !reportServiceRE.MatchString(r.Service) {
		return engine.Event{}, false
	}
	d := map[string]any{"service": r.Service}
	switch r.Type {
	case "started":
		d["pid"] = r.PID
		if r.RestartCount > 0 {
			d["restart_count"] = r.RestartCount
		}
	case "crashed":
		if r.ExitCode != nil {
			d["exit_code"] = *r.ExitCode
		}
		d["restart_count"], d["restart_in_ms"] = r.RestartCount, r.RestartInMS
	case "stopped":
		if r.ExitCode != nil {
			d["exit_code"] = *r.ExitCode
		}
	case "failed":
		if len(r.Error) > 200 {
			r.Error = r.Error[:200]
		}
		d["error"], d["restart_in_ms"] = r.Error, r.RestartInMS
	default:
		return engine.Event{}, false
	}
	return spriteEvent(sp, "service."+r.Type, d), true
}

// registerGuestEvents adds the event routes to one sprite's guest channel.
// Neither is pinned by bind: a stream held open from inside must not keep the
// sprite awake. Suspending cuts it (vsock does not survive a snapshot) and the
// reader resumes with Last-Event-ID after the wake.
func (s *Server) registerGuestEvents(mux *http.ServeMux, sp store.Sprite) {
	current := func(w http.ResponseWriter) (store.Sprite, bool) {
		cur, err := s.store.Get(sp.ID)
		if err != nil {
			writeErr(w, http.StatusNotFound, "not_found", "sprite not found")
			return cur, false
		}
		return cur, true
	}
	mux.HandleFunc("GET "+eventsPath, func(w http.ResponseWriter, r *http.Request) {
		self, ok := current(w)
		if !ok {
			return
		}
		if !spawnPolicy(self).Enabled {
			s.spawnRefused(w, self)
			return
		}
		id := self.ID
		serveEvents(s.life.Events(), w, r, func(e engine.Event) bool { return e.ParentID == id }, s.eventHeartbeat())
	})
	mux.HandleFunc("POST /internal/service-event", func(w http.ResponseWriter, r *http.Request) {
		self, ok := current(w)
		if !ok {
			return
		}
		var rep serviceReport
		if !readJSON(w, r, 4096, &rep) {
			return
		}
		e, ok := serviceEvent(self, rep)
		if !ok {
			writeErr(w, http.StatusBadRequest, "bad_request", "not a service event")
			return
		}
		if !s.guestEvents.Allow(self.ID) {
			writeErr(w, http.StatusTooManyRequests, "rate_limited", "too many events from this sprite")
			return
		}
		s.life.Events().Publish(e)
		w.WriteHeader(http.StatusNoContent)
	})
}
