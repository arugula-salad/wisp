package server

import (
	"github.com/arugula-salad/wisp/engine"
	"github.com/arugula-salad/wisp/internal/store"
)

// A Describer is how the front end that owns a record names it, in the
// "sprite" and "parent_id" fields of an event and in the log. The engine
// publishes by ID and knows no names; the Server installs one (SetDescriber).
type Describer func(store.Sprite) (name, parentID string)

// SetDescriber installs f. Until then, and for a record nobody describes,
// events carry the ID alone and the log says the ID.
func (l *Lifecycle) SetDescriber(f Describer) { l.describer.Store(&f) }

// describe names the record with this ID: the one in the store, or one the
// lifecycle is creating or deleting and so holds while the store does not.
func (l *Lifecycle) describe(id string) (name, parentID string) {
	f := l.describer.Load()
	if f == nil || id == "" {
		return "", ""
	}
	sp, err := l.store.Get(id)
	if err != nil {
		l.mu.Lock()
		held, ok := l.unstored[id]
		l.mu.Unlock()
		if !ok {
			return "", ""
		}
		sp = held
	}
	return (*f)(sp)
}

// nameOf is what the Describer calls a record in hand: "" without one.
func (l *Lifecycle) nameOf(sp store.Sprite) string {
	f := l.describer.Load()
	if f == nil {
		return ""
	}
	name, _ := (*f)(sp)
	return name
}

// holdUnstored makes sp describable while the store does not have it: from
// before Create writes it, or once Delete has removed it, until release.
func (l *Lifecycle) holdUnstored(sp store.Sprite) (release func()) {
	l.mu.Lock()
	if l.unstored == nil { // a Lifecycle built by hand in a test
		l.unstored = map[string]store.Sprite{}
	}
	l.unstored[sp.ID] = sp
	l.mu.Unlock()
	return func() {
		l.mu.Lock()
		delete(l.unstored, sp.ID)
		l.mu.Unlock()
	}
}

// label is how the log refers to a record: its name, or its ID without one.
func (l *Lifecycle) label(rec store.Record) string {
	if name, _ := l.describe(rec.ID); name != "" {
		return name
	}
	return rec.ID
}

// emit publishes an event about rec, which the Describer names.
func (l *Lifecycle) emit(rec store.Record, typ string, detail map[string]any) {
	l.events.Publish(l.event(rec, typ, detail))
}

func (l *Lifecycle) event(rec store.Record, typ string, detail map[string]any) engine.Event {
	name, parent := l.describe(rec.ID)
	return engine.Event{Type: typ, Sprite: name, SpriteID: rec.ID, ParentID: parent, Detail: detail}
}
