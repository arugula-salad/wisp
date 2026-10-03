package engine

import (
	"sync"
	"time"
)

// The event stream is ours, not upstream's: the Sprites API pushes nothing, so
// anything that wants to follow sandboxes (a dashboard, a lobby showing its
// games, an alert) would otherwise have to poll. Everything that changes a
// sandbox publishes to one in-process Bus, which keeps the newest eventRing
// events for resuming and fans them out to subscribers (a front end's event
// streams) and sinks (its webhooks).
//
// Publishing never waits on a reader. A subscriber that falls a buffer behind
// is cut off, and resumes from the ring by ID; a sink must not block at all.
// The engine holds a sandbox's lock while it publishes, so anything slower
// would stall the sandbox.

const (
	eventRing      = 1024 // events kept for resuming
	eventSubBuffer = 256  // events a subscriber may be behind before it is cut off
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

// Bus is the event bus: one per Engine (Engine.Events), which front ends
// publish to as well.
type Bus struct {
	now func() time.Time

	mu     sync.Mutex
	next   uint64
	ring   []Event // oldest first, at most eventRing
	subs   map[*Subscription]struct{}
	sinks  []func(Event) // must not block: called with mu held
	closed bool
}

// Subscription is one subscriber's feed (Bus.Subscribe).
type Subscription struct {
	ch    chan Event
	match func(Event) bool
	// why is set when the bus closed ch: "lagged" or "shutdown".
	why string
}

// Events delivers the subscriber's events. The bus closes it when the
// subscriber falls too far behind or the bus closes; Why says which.
func (s *Subscription) Events() <-chan Event { return s.ch }

// Why is why the bus closed Events: "lagged" or "shutdown". It is only
// meaningful once Events is closed.
func (s *Subscription) Why() string { return s.why }

// newBus returns an empty bus whose IDs start from the clock.
func newBus() *Bus {
	return &Bus{now: time.Now, next: uint64(time.Now().UnixMilli()) * 1000, subs: map[*Subscription]struct{}{}}
}

// Publish stamps e with its ID and time and delivers it. Safe on a nil bus, so
// an Engine assembled by hand in tests needs none.
func (b *Bus) Publish(e Event) {
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

func (b *Bus) dropLocked(s *Subscription, why string) {
	delete(b.subs, s)
	s.why = why
	close(s.ch)
}

// AddSink registers a consumer of every event. f is called with the bus
// locked, in publishing order, and must not block.
func (b *Bus) AddSink(f func(Event)) {
	b.mu.Lock()
	b.sinks = append(b.sinks, f)
	b.mu.Unlock()
}

// Gap tells a subscriber that resumed from an ID the ring no longer holds.
type Gap struct {
	Requested uint64 `json:"requested"`
	Oldest    uint64 `json:"oldest"`
}

// Subscribe registers a subscriber for the events match accepts. With resume
// set it also returns the buffered events after the ID `after` (all of them
// for 0), and a gap when events after it have already been dropped from the
// ring. Replay and registration happen under one lock, so nothing falls
// between them. pos is the newest ID published so far: every event the
// subscription delivers is newer. Unsubscribe when done.
func (b *Bus) Subscribe(match func(Event) bool, after uint64, resume bool) (s *Subscription, replay []Event, gap *Gap, pos uint64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	s = &Subscription{ch: make(chan Event, eventSubBuffer), match: match}
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
	// An ID ahead of ours is from before a restart whose clock went backwards,
	// or from another daemon: either way we cannot say what was missed.
	if after != 0 && (after+1 < oldest || after > b.next) {
		gap = &Gap{Requested: after, Oldest: oldest}
	}
	for _, e := range b.ring {
		if (gap != nil || e.ID > after) && match(e) {
			replay = append(replay, e)
		}
	}
	return s, replay, gap, b.next
}

// Unsubscribe removes s.
func (b *Bus) Unsubscribe(s *Subscription) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.subs, s)
}

// Close ends every subscription, so a graceful HTTP shutdown does not wait on
// the streams fed by them.
func (b *Bus) Close() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.closed = true
	for s := range b.subs {
		b.dropLocked(s, "shutdown")
	}
}
