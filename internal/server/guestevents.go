package server

import (
	"encoding/json"
	"net/http"
	"regexp"
	"sync"
	"time"

	"github.com/jhgaylor/wisp/internal/store"
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

// rateLimiter is a token bucket per key.
type rateLimiter struct {
	burst, rate float64
	now         func() time.Time

	mu      sync.Mutex
	buckets map[string]*bucket
}

type bucket struct {
	tokens float64
	at     time.Time
}

func newRateLimiter(burst, perSecond float64) *rateLimiter {
	return &rateLimiter{burst: burst, rate: perSecond, now: time.Now, buckets: map[string]*bucket{}}
}

func (rl *rateLimiter) allow(key string) bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	now := rl.now()
	b, ok := rl.buckets[key]
	if !ok {
		if len(rl.buckets) > 10000 { // keys are sprite IDs; this only bounds memory against churn
			clear(rl.buckets)
		}
		b = &bucket{tokens: rl.burst, at: now}
		rl.buckets[key] = b
	}
	b.tokens = min(rl.burst, b.tokens+now.Sub(b.at).Seconds()*rl.rate)
	b.at = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// serviceEvent turns a report into an event, or refuses it.
func serviceEvent(sp store.Sprite, r serviceReport) (Event, bool) {
	if !reportServiceRE.MatchString(r.Service) {
		return Event{}, false
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
		return Event{}, false
	}
	return spriteEvent(sp, "service."+r.Type, d), true
}

// registerGuestEvents adds the event routes to one sprite's guest channel.
// Neither is pinned by bind: a stream held open from inside must not keep the
// sprite awake. Suspending cuts it (vsock does not survive a snapshot) and the
// reader resumes with Last-Event-ID after the wake.
func (s *Server) registerGuestEvents(mux *http.ServeMux, sp store.Sprite) {
	current := func(w http.ResponseWriter) (store.Sprite, bool) {
		cur, err := s.store.Get(sp.Name)
		if err != nil || cur.ID != sp.ID {
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
		s.life.events.serveEvents(w, r, func(e Event) bool { return e.ParentID == id }, s.eventHeartbeat())
	})
	mux.HandleFunc("POST /internal/service-event", func(w http.ResponseWriter, r *http.Request) {
		self, ok := current(w)
		if !ok {
			return
		}
		var rep serviceReport
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&rep); err != nil {
			writeErr(w, http.StatusBadRequest, "bad_request", "invalid JSON body")
			return
		}
		e, ok := serviceEvent(self, rep)
		if !ok {
			writeErr(w, http.StatusBadRequest, "bad_request", "not a service event")
			return
		}
		if !s.guestEvents.allow(self.ID) {
			writeErr(w, http.StatusTooManyRequests, "rate_limited", "too many events from this sprite")
			return
		}
		s.life.events.Publish(e)
		w.WriteHeader(http.StatusNoContent)
	})
}
