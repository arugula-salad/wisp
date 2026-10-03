package engine

import (
	"testing"
	"time"
)

// subscribe is Subscribe without the position.
func (b *Bus) subscribe(match func(Event) bool, after uint64, resume bool) (*Subscription, []Event, *Gap) {
	s, replay, gap, _ := b.Subscribe(match, after, resume)
	return s, replay, gap
}

func TestEventBusIDsRingAndResume(t *testing.T) {
	b := newBus()
	all := func(Event) bool { return true }
	b.Publish(Event{Type: "a"})
	first := b.ring[0].ID
	b.Publish(Event{Type: "b"})
	b.Publish(Event{Type: "c"})
	if b.ring[1].ID != first+1 || b.ring[2].ID != first+2 || b.ring[0].Time.IsZero() {
		t.Fatalf("ids/times not stamped in order: %+v", b.ring)
	}

	_, replay, gap := b.subscribe(all, first, true)
	if gap != nil || len(replay) != 2 || replay[0].Type != "b" || replay[1].Type != "c" {
		t.Fatalf("resume after the first: gap %v, replay %+v", gap, replay)
	}
	if _, replay, gap = b.subscribe(all, 0, true); gap != nil || len(replay) != 3 {
		t.Fatalf("0 replays everything: gap %v, %d events", gap, len(replay))
	}
	if _, replay, _ = b.subscribe(all, 0, false); len(replay) != 0 {
		t.Fatalf("no resume, no replay: %+v", replay)
	}
	// Caught up exactly: nothing to replay, and no gap either.
	if _, replay, gap = b.subscribe(all, first+2, true); gap != nil || len(replay) != 0 {
		t.Fatalf("caught up: gap %v, replay %+v", gap, replay)
	}
	// An ID ahead of anything published: say so rather than wait for it.
	if _, _, gap = b.subscribe(all, first+100, true); gap == nil {
		t.Fatal("an ID from the future was not reported as a gap")
	}

	for i := 0; i < eventRing; i++ {
		b.Publish(Event{Type: "fill"})
	}
	if len(b.ring) != eventRing || b.ring[0].ID != first+3 {
		t.Fatalf("ring holds %d, oldest %d; want %d from %d", len(b.ring), b.ring[0].ID, eventRing, first+3)
	}
	_, replay, gap = b.subscribe(all, first+1, true)
	if gap == nil || gap.Requested != first+1 || gap.Oldest != first+3 || len(replay) != eventRing {
		t.Fatalf("fell out of the ring: gap %+v, %d replayed", gap, len(replay))
	}
	// Right at the edge: first+2 was dropped, but everything after it is still here.
	if _, _, gap = b.subscribe(all, first+2, true); gap != nil {
		t.Fatalf("no event was missed, yet: %+v", gap)
	}
}

func TestEventBusCutsASlowStreamWithoutWaiting(t *testing.T) {
	b := newBus()
	slow, _, _ := b.subscribe(func(Event) bool { return true }, 0, false)
	other, _, _ := b.subscribe(func(e Event) bool { return e.Type == "rare" }, 0, false)
	done := make(chan struct{})
	go func() {
		for i := 0; i < eventSubBuffer+10; i++ {
			b.Publish(Event{Type: "x"})
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Publish blocked on a reader")
	}
	n := 0
	for range slow.ch {
		n++
	}
	if n != eventSubBuffer || slow.why != "lagged" {
		t.Fatalf("slow reader got %d events and %q, want %d and lagged", n, slow.why, eventSubBuffer)
	}
	// A stream the flood did not match is untouched.
	b.Publish(Event{Type: "rare"})
	if e := <-other.ch; e.Type != "rare" {
		t.Fatalf("got %+v", e)
	}
	b.Close()
	if _, ok := <-other.ch; ok || other.why != "shutdown" {
		t.Fatalf("Close left a stream open (%q)", other.why)
	}
	if s, _, _ := b.subscribe(func(Event) bool { return true }, 0, false); s.why != "shutdown" {
		t.Fatal("subscribing after Close must end at once")
	}
}
