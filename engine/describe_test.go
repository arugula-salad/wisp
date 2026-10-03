package engine

import (
	"reflect"
	"testing"

	"github.com/arugula-salad/wisp/internal/store"
)

// The engine publishes by record; the front end's Describer puts the name and
// the parent back, so an event reads as it did when the lifecycle had the
// sprite in hand -- including about a sprite the store no longer (or does not
// yet) hold, while Delete (or Create) holds it.
func TestEventsAreDescribedByTheFrontEnd(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	l := &Engine{store: st, runtimes: map[string]*runtime{}}
	child := store.Sprite{Record: store.Record{ID: store.NewID()}, SpriteMeta: store.SpriteMeta{Name: "child", ParentID: "p1"}}
	other := store.Sprite{Record: store.Record{ID: store.NewID(), API: "e2b"}}
	for _, sp := range []*store.Sprite{&child, &other} {
		if err := st.Create(sp); err != nil {
			t.Fatal(err)
		}
	}
	detail := map[string]any{"k": 1}
	if e := l.event(child.Record, "sprite.woke", detail); e.Sprite != "" || e.SpriteID != child.ID {
		t.Fatalf("undescribed: %+v", e)
	}
	l.SetDescriber(spriteName)
	want := Event{Type: "sprite.woke", Sprite: "child", SpriteID: child.ID, ParentID: "p1", Detail: detail}
	if got := l.event(child.Record, "sprite.woke", detail); !reflect.DeepEqual(got, want) {
		t.Fatalf("event %+v, want %+v", got, want)
	}
	if e := l.event(other.Record, "sprite.woke", nil); e.Sprite != "" || e.ParentID != "" || e.SpriteID != other.ID {
		t.Fatalf("another API's record: %+v", e)
	}
	if got := l.label(child.Record); got != "child" {
		t.Fatalf("label %q", got)
	}
	if got := l.label(other.Record); got != other.ID {
		t.Fatalf("label %q, want the ID", got)
	}

	// Gone from the store but held: still described, until released.
	cur, _ := st.Get(child.ID)
	release := l.holdUnstored(cur)
	st.Delete(child.ID)
	if e := l.event(child.Record, "sprite.deleted", nil); e.Sprite != "child" || e.ParentID != "p1" {
		t.Fatalf("held: %+v", e)
	}
	release()
	if e := l.event(child.Record, "sprite.deleted", nil); e.Sprite != "" || e.SpriteID != child.ID {
		t.Fatalf("released: %+v", e)
	}
	// A host-wide event has no sprite at all.
	if e := l.event(store.Record{}, "disk.refused", nil); e.Sprite != "" || e.SpriteID != "" || e.ParentID != "" {
		t.Fatalf("host event: %+v", e)
	}
}
