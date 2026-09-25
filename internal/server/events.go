package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/arugula-salad/wisp/internal/store"
)

// The event stream is ours, not upstream's: the Sprites API pushes nothing, so
// anything that wants to follow sprites (a dashboard, a lobby showing its games,
// an alert) would otherwise have to poll. Everything that changes a sprite
// publishes to one in-process bus, which keeps the newest eventRing events for
// resuming, fans them out to SSE streams (eventsPath) and hands them to
// webhooks (webhooks.go).
//
// Publishing never waits on a reader. A stream that falls a buffer behind is
// cut off, and resumes from the ring with Last-Event-ID; a webhook whose queue
// is full drops the event and counts it. The lifecycle holds a sprite's lock
// while it publishes, so anything slower would stall the sprite.

// eventsPath is where the stream is served, on the API and on the guest
// socket alike. The prefix keeps it clear of anything upstream has, or might
// add, under /v1.
const eventsPath = "/wisp/v1/events"

const (
	eventRing      = 1024 // events kept for resuming
	eventSubBuffer = 256  // events a stream may be behind before it is cut off
	eventHeartbeat = 15 * time.Second
)

// Event is one thing that happened. The ID increases by one per event and is
// seeded from the clock at startup, so IDs keep increasing across restarts.
// Detail is small and depends on Type (see docs/events.md).
type Event struct {
	ID       uint64    `json:"id"`
	Type     string    `json:"type"`
	Time     time.Time `json:"time"`
	Sprite   string    `json:"sprite,omitempty"`
	SpriteID string    `json:"sprite_id,omitempty"`
	// ParentID is the sprite that created this one from inside, which is also
	// what decides whether a spawner may see the event (guestEvents).
	ParentID string         `json:"parent_id,omitempty"`
	Detail   map[string]any `json:"detail,omitempty"`
}

// spriteEvent is an Event about sp.
func spriteEvent(sp store.Sprite, typ string, detail map[string]any) Event {
	return Event{Type: typ, Sprite: sp.Name, SpriteID: sp.ID, ParentID: sp.ParentID, Detail: detail}
}

type eventBus struct {
	now func() time.Time

	mu     sync.Mutex
	next   uint64
	ring   []Event // oldest first, at most eventRing
	subs   map[*eventSub]struct{}
	sinks  []func(Event) // must not block: called with mu held
	closed bool
}

type eventSub struct {
	ch    chan Event
	match func(Event) bool
	// why is set when the bus closed ch: "lagged" or "shutdown".
	why string
}

func newEventBus() *eventBus {
	return &eventBus{now: time.Now, next: uint64(time.Now().UnixMilli()) * 1000, subs: map[*eventSub]struct{}{}}
}

// Publish stamps e with its ID and time and delivers it. Safe on a nil bus, so
// a Lifecycle assembled by hand in tests needs none.
func (b *eventBus) Publish(e Event) {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.next++
	e.ID, e.Time = b.next, b.now().UTC()
	if len(b.ring) == eventRing {
		copy(b.ring, b.ring[1:])
		b.ring = b.ring[:eventRing-1]
	}
	b.ring = append(b.ring, e)
	for s := range b.subs {
		if !s.match(e) {
			continue
		}
		select {
		case s.ch <- e:
		default:
			b.dropLocked(s, "lagged")
		}
	}
	for _, sink := range b.sinks {
		sink(e)
	}
}

func (b *eventBus) dropLocked(s *eventSub, why string) {
	delete(b.subs, s)
	s.why = why
	close(s.ch)
}

// addSink registers a non-blocking consumer of every event.
func (b *eventBus) addSink(f func(Event)) {
	b.mu.Lock()
	b.sinks = append(b.sinks, f)
	b.mu.Unlock()
}

// gapNotice tells a client that resumed from an ID the ring no longer holds.
type gapNotice struct {
	Requested uint64 `json:"requested"`
	Oldest    uint64 `json:"oldest"`
}

// subscribe registers a stream. With resume set it also returns the buffered
// events after the ID `after` (all of them for 0), and a gap notice when
// events after it have already been dropped from the ring. Replay and
// registration happen under one lock, so nothing falls between them.
func (b *eventBus) subscribe(match func(Event) bool, after uint64, resume bool) (*eventSub, []Event, *gapNotice) {
	s, replay, gap, _ := b.subscribeAt(match, after, resume)
	return s, replay, gap
}

// subscribeAt is subscribe that also returns the newest ID published so far:
// every event the stream delivers live is newer.
func (b *eventBus) subscribeAt(match func(Event) bool, after uint64, resume bool) (*eventSub, []Event, *gapNotice, uint64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	s := &eventSub{ch: make(chan Event, eventSubBuffer), match: match}
	if b.closed {
		s.why = "shutdown"
		close(s.ch)
		return s, nil, nil, b.next
	}
	b.subs[s] = struct{}{}
	if !resume {
		return s, nil, nil, b.next
	}
	oldest := b.next + 1
	if len(b.ring) > 0 {
		oldest = b.ring[0].ID
	}
	var gap *gapNotice
	// An ID ahead of ours is from before a restart whose clock went backwards,
	// or from another daemon: either way we cannot say what was missed.
	if after != 0 && (after+1 < oldest || after > b.next) {
		gap = &gapNotice{Requested: after, Oldest: oldest}
	}
	var replay []Event
	for _, e := range b.ring {
		if (gap != nil || e.ID > after) && match(e) {
			replay = append(replay, e)
		}
	}
	return s, replay, gap, b.next
}

func (b *eventBus) unsubscribe(s *eventSub) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.subs, s)
}

// Close ends every stream, so a graceful HTTP shutdown does not wait on them.
func (b *eventBus) Close() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.closed = true
	for s := range b.subs {
		b.dropLocked(s, "shutdown")
	}
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

func (f eventFilter) match(e Event) bool {
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
func (b *eventBus) serveEvents(w http.ResponseWriter, r *http.Request, scope func(Event) bool, heartbeat time.Duration) {
	f := parseEventFilter(r)
	after, resume := lastEventID(r)
	sub, replay, gap, pos := b.subscribeAt(func(e Event) bool { return scope(e) && f.match(e) }, after, resume)
	defer b.unsubscribe(sub)

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
	send := func(e Event) bool {
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
		case e, ok := <-sub.ch:
			if !ok {
				msg := "the daemon is shutting down"
				if sub.why == "lagged" {
					msg = "this stream fell too far behind and was closed; reconnect with Last-Event-ID to resume"
				}
				notice("stream."+sub.why, map[string]string{"message": msg})
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
	s.life.events.serveEvents(w, r, func(Event) bool { return true }, s.eventHeartbeat())
}

func (s *Server) eventHeartbeat() time.Duration {
	if s.heartbeat > 0 {
		return s.heartbeat
	}
	return eventHeartbeat
}

// CloseEvents ends every event stream; the daemon calls it as it shuts down.
func (s *Server) CloseEvents() { s.life.events.Close() }

// emit publishes an event about sp.
func (l *Lifecycle) emit(sp store.Sprite, typ string, detail map[string]any) {
	l.events.Publish(spriteEvent(sp, typ, detail))
}
