package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/arugula-salad/wisp/engine"
	"github.com/arugula-salad/wisp/internal/store"
)

// The Sprites API's side of the event stream (engine/events.go has the bus):
// it is served as server-sent events on eventsPath, and handed to webhooks
// (webhooks.go). A stream that falls a buffer behind is cut off by the bus,
// and resumes from its ring with Last-Event-ID.

// eventsPath is where the stream is served, on the API and on the guest
// socket alike. The prefix keeps it clear of anything upstream has, or might
// add, under /v1.
const eventsPath = "/wisp/v1/events"

const eventHeartbeat = 15 * time.Second

// spriteEvent is an Event about sp.
func spriteEvent(sp store.Sprite, typ string, detail map[string]any) engine.Event {
	name, parent := describeSprite(sp)
	return engine.Event{Type: typ, Sprite: name, SpriteID: sp.ID, ParentID: parent, Detail: detail}
}

// describeSprite is the Server's Describer: a sprite is known by its name, and
// a spawned one by its parent too. Another API's record has neither.
func describeSprite(sp store.Sprite) (name, parentID string) {
	if sp.API != store.Sprites {
		return "", ""
	}
	return sp.Name, sp.ParentID
}

// eventFilter is what a client asked for: sprite names and type prefixes,
// each repeatable or comma-separated. Empty means everything.
type eventFilter struct {
	sprites []string
	types   []string
}

func parseEventFilter(r *http.Request) eventFilter {
	split := func(vs []string) []string {
		var out []string
		for _, v := range vs {
			for _, x := range strings.Split(v, ",") {
				if x = strings.TrimSpace(x); x != "" {
					out = append(out, x)
				}
			}
		}
		return out
	}
	q := r.URL.Query()
	return eventFilter{sprites: split(q["sprite"]), types: split(q["type"])}
}

func (f eventFilter) match(e engine.Event) bool {
	if len(f.sprites) > 0 && !slices.Contains(f.sprites, e.Sprite) {
		return false
	}
	if len(f.types) == 0 {
		return true
	}
	for _, p := range f.types {
		if strings.HasPrefix(e.Type, p) {
			return true
		}
	}
	return false
}

// lastEventID is where a client resumes: the Last-Event-ID header an
// EventSource sends on reconnect, or ?last_event_id= for the first connect
// (0 replays the whole buffer).
func lastEventID(r *http.Request) (uint64, bool) {
	v := r.Header.Get("Last-Event-ID")
	if v == "" {
		v = r.URL.Query().Get("last_event_id")
	}
	if v == "" {
		return 0, false
	}
	n, err := strconv.ParseUint(strings.TrimSpace(v), 10, 64)
	return n, err == nil
}

// serveEvents streams the bus as server-sent events. scope is what this caller
// may see at all; the client's filter narrows it further. Each event is one
// unnamed SSE message (so EventSource.onmessage gets all of them) whose id is
// the event's ID. Notices about the stream itself carry no id, so they never
// move a client's resume point: stream.gap, and stream.lagged or
// stream.shutdown just before the server ends the stream.
func serveEvents(b *engine.Bus, w http.ResponseWriter, r *http.Request, scope func(engine.Event) bool, heartbeat time.Duration) {
	f := parseEventFilter(r)
	after, resume := lastEventID(r)
	sub, replay, gap, pos := b.Subscribe(func(e engine.Event) bool { return scope(e) && f.match(e) }, after, resume)
	defer b.Unsubscribe(sub)

	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no") // nginx and friends: do not buffer this response
	w.WriteHeader(http.StatusOK)
	rc := http.NewResponseController(w)
	// A client that stops reading costs only its own goroutine, and not for long.
	write := func(s string) bool {
		rc.SetWriteDeadline(time.Now().Add(30 * time.Second))
		if _, err := fmt.Fprint(w, s); err != nil {
			return false
		}
		return rc.Flush() == nil
	}
	send := func(e engine.Event) bool {
		data, _ := json.Marshal(e)
		return write(fmt.Sprintf("id: %d\ndata: %s\n\n", e.ID, data))
	}
	notice := func(typ string, detail any) bool {
		data, _ := json.Marshal(map[string]any{"type": typ, "time": time.Now().UTC(), "detail": detail})
		return write(fmt.Sprintf("data: %s\n\n", data))
	}

	if !write("retry: 2000\n: wisp event stream\n\n") {
		return
	}
	if gap != nil && !notice("stream.gap", gap) {
		return
	}
	sent := uint64(0)
	for _, e := range replay {
		if !send(e) {
			return
		}
		sent = e.ID
	}
	// The position, as an id with no data: EventSource (and sprite-env) take it
	// as the place to resume from without dispatching anything. A client whose
	// first connection is cut before any event matched then still resumes
	// where it was, instead of silently starting over from "now".
	if sent < pos && !write(fmt.Sprintf("id: %d\n\n", pos)) {
		return
	}
	tick := time.NewTicker(heartbeat)
	defer tick.Stop()
	for {
		select {
		case e, ok := <-sub.Events():
			if !ok {
				msg := "the daemon is shutting down"
				if sub.Why() == "lagged" {
					msg = "this stream fell too far behind and was closed; reconnect with Last-Event-ID to resume"
				}
				notice("stream."+sub.Why(), map[string]string{"message": msg})
				return
			}
			if !send(e) {
				return
			}
		case <-tick.C:
			if !write(": ping\n\n") {
				return
			}
		case <-r.Context().Done():
			return
		}
	}
}

// serveAPIEvents is GET eventsPath on the API: every event, for the operator.
func (s *Server) serveAPIEvents(w http.ResponseWriter, r *http.Request) {
	serveEvents(s.life.Events(), w, r, func(engine.Event) bool { return true }, s.eventHeartbeat())
}

func (s *Server) eventHeartbeat() time.Duration {
	if s.heartbeat > 0 {
		return s.heartbeat
	}
	return eventHeartbeat
}

// CloseEvents ends every event stream; the daemon calls it as it shuts down.
func (s *Server) CloseEvents() { s.life.Events().Close() }
