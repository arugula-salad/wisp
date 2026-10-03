package server

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/arugula-salad/wisp/engine"
	"github.com/arugula-salad/wisp/internal/store"
)

func TestEventFilter(t *testing.T) {
	req := httptest.NewRequest("GET", "/?sprite=a,b&sprite=c&type=sprite.&type=checkpoint.created", nil)
	f := parseEventFilter(req)
	for _, c := range []struct {
		e    engine.Event
		want bool
	}{
		{engine.Event{Sprite: "a", Type: "sprite.woke"}, true},
		{engine.Event{Sprite: "c", Type: "checkpoint.created"}, true},
		{engine.Event{Sprite: "c", Type: "checkpoint.deleted"}, false},
		{engine.Event{Sprite: "d", Type: "sprite.woke"}, false},
		{engine.Event{Type: "disk.low"}, false},
	} {
		if got := f.match(c.e); got != c.want {
			t.Errorf("%+v: %v", c.e, got)
		}
	}
	if !(eventFilter{}).match(engine.Event{Type: "disk.low"}) {
		t.Error("an empty filter must match everything")
	}
}

// sseFrame is one message off a stream: id is empty for notices.
type sseFrame struct {
	id   string
	ev   engine.Event
	ping bool
}

// readSSE parses a stream into frames until the body ends.
func readSSE(body io.Reader) <-chan sseFrame {
	out := make(chan sseFrame, 1024)
	go func() {
		defer close(out)
		sc := bufio.NewScanner(body)
		var f sseFrame
		var data string
		for sc.Scan() {
			line := sc.Text()
			switch {
			case line == ": ping":
				out <- sseFrame{ping: true}
			case strings.HasPrefix(line, "id: "):
				f.id = strings.TrimPrefix(line, "id: ")
			case strings.HasPrefix(line, "data: "):
				data = strings.TrimPrefix(line, "data: ")
			case line == "" && data != "":
				json.Unmarshal([]byte(data), &f.ev)
				out <- f
				f, data = sseFrame{}, ""
			}
		}
	}()
	return out
}

// next returns the next frame that is not a heartbeat.
func next(t *testing.T, frames <-chan sseFrame) sseFrame {
	t.Helper()
	timeout := time.After(5 * time.Second)
	for {
		select {
		case f, ok := <-frames:
			if !ok {
				t.Fatal("stream ended")
			}
			if !f.ping {
				return f
			}
		case <-timeout:
			t.Fatal("no event within 5s")
		}
	}
}

func openStream(t *testing.T, base, path string, header http.Header) (<-chan sseFrame, func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, "GET", base+path, nil)
	req.Header.Set("Authorization", "Bearer tok")
	for k, v := range header {
		req.Header[k] = v
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "text/event-stream" {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("%s: %d %s", path, resp.StatusCode, b)
	}
	return readSSE(resp.Body), func() { cancel(); resp.Body.Close() }
}

func TestEventStreamOverTheAPI(t *testing.T) {
	s, h := newOperatorServer(t, Options{MaxSprites: 3})
	s.heartbeat = 20 * time.Millisecond
	ts := httptest.NewServer(h)
	defer ts.Close()

	// Unauthenticated, nothing.
	if resp, _ := http.Get(ts.URL + eventsPath); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no token: %d", resp.StatusCode)
	}

	everything, stop := openStream(t, ts.URL, eventsPath, nil)
	defer stop()
	onlyB, stopB := openStream(t, ts.URL, eventsPath+"?sprite=b&type=policy.,sprite.deleted", nil)
	defer stopB()

	for _, name := range []string{"a", "b", "c"} {
		status(t, apiCall(t, h, "POST", "/v1/sprites", `{"name":"`+name+`"}`), http.StatusCreated)
	}
	status(t, apiCall(t, h, "POST", "/v1/sprites", `{"name":"d"}`), http.StatusForbidden)
	status(t, apiCall(t, h, "POST", "/v1/sprites/b/policy/privileges", `{"profile":"minimal"}`), http.StatusNoContent)
	status(t, apiCall(t, h, "DELETE", "/v1/sprites/b", ""), http.StatusNoContent)

	var got []string
	var ids []string
	for i := 0; i < 6; i++ {
		f := next(t, everything)
		got = append(got, f.ev.Type+":"+f.ev.Sprite)
		ids = append(ids, f.id)
		if f.id != fmt.Sprint(f.ev.ID) {
			t.Errorf("SSE id %q does not match the event's %d", f.id, f.ev.ID)
		}
	}
	want := "sprite.created:a sprite.created:b sprite.created:c limit.refused:d policy.changed:b sprite.deleted:b"
	if strings.Join(got, " ") != want {
		t.Fatalf("stream:\n got %s\nwant %s", strings.Join(got, " "), want)
	}
	if f := next(t, onlyB); f.ev.Type != "policy.changed" || f.ev.Detail["policy"] != "privileges" {
		t.Fatalf("filtered stream: %+v", f.ev)
	}
	if f := next(t, onlyB); f.ev.Type != "sprite.deleted" {
		t.Fatalf("filtered stream: %+v", f.ev)
	}
	// Heartbeats keep an idle stream talking.
	select {
	case f := <-onlyB:
		if !f.ping {
			t.Fatalf("unexpected %+v", f)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no heartbeat")
	}

	// Resuming: Last-Event-ID picks up right after the event named.
	resumed, stopR := openStream(t, ts.URL, eventsPath, http.Header{"Last-Event-Id": {ids[3]}})
	defer stopR()
	if f := next(t, resumed); f.id != ids[4] || f.ev.Type != "policy.changed" {
		t.Fatalf("resumed at %+v, want id %s", f, ids[4])
	}
	// And an ID the ring no longer holds is owned up to before the replay.
	old, stopO := openStream(t, ts.URL, eventsPath+"?last_event_id=5", nil)
	defer stopO()
	if f := next(t, old); f.id != "" || f.ev.Type != "stream.gap" {
		t.Fatalf("want a gap notice first, got %+v", f)
	}
	if f := next(t, old); f.id != ids[0] {
		t.Fatalf("after the gap, the oldest event: got %+v", f)
	}

	// Shutting down ends streams instead of leaving the server waiting on them.
	s.CloseEvents()
	for f := range everything {
		if f.ev.Type == "stream.shutdown" {
			return
		}
	}
	t.Fatal("stream ended without a shutdown notice")
}

func TestGuestSeesOnlyItsChildrensEvents(t *testing.T) {
	s, h := newOperatorServer(t, Options{})
	s.heartbeat = time.Hour
	for _, name := range []string{"lobby", "other-lobby", "bystander"} {
		status(t, apiCall(t, h, "POST", "/v1/sprites", `{"name":"`+name+`"}`), http.StatusCreated)
	}
	lobby, _ := s.store.GetByName(store.Sprites, "lobby")
	guest := httptest.NewServer(s.guestAPI(lobby.Record, &guestChan{}))
	defer guest.Close()

	// No spawn policy, no stream; and the refusal is itself an event.
	resp, err := http.Get(guest.URL + eventsPath)
	if err != nil {
		t.Fatal(err)
	}
	if body := status(t, resp, http.StatusForbidden); !strings.Contains(string(body), "spawn_disabled") {
		t.Fatalf("got %s", body)
	}
	for _, name := range []string{"lobby", "other-lobby"} {
		status(t, apiCall(t, h, "POST", "/v1/sprites/"+name+"/policy/spawn", `{"enabled":true}`), http.StatusNoContent)
	}

	frames, stop := openStream(t, guest.URL, eventsPath+"?last_event_id=0", nil)
	defer stop()
	status(t, fromInside(t, s, "other-lobby", "POST", "/v1/sprites", `{"name":"theirs"}`), http.StatusCreated)
	status(t, fromInside(t, s, "lobby", "POST", "/v1/sprites", `{"name":"game-1"}`), http.StatusCreated)
	status(t, apiCall(t, h, "POST", "/v1/sprites/bystander/policy/privileges", `{"profile":"minimal"}`), http.StatusNoContent)
	status(t, apiCall(t, h, "DELETE", "/v1/sprites/theirs", ""), http.StatusNoContent)
	status(t, fromInside(t, s, "lobby", "DELETE", "/v1/sprites/game-1", ""), http.StatusNoContent)

	// Everything the lobby could see, replay included: the refusal and the
	// other sprites' events are not among it.
	for _, want := range []string{"sprite.created", "sprite.deleted"} {
		if f := next(t, frames); f.ev.Type != want || f.ev.Sprite != "game-1" || f.ev.ParentID != lobby.ID {
			t.Fatalf("want %s of game-1, got %+v", want, f.ev)
		}
	}
	// Anything further would be a leak; publish a marker to prove the stream is live and nothing came before it.
	status(t, fromInside(t, s, "lobby", "POST", "/v1/sprites", `{"name":"game-2"}`), http.StatusCreated)
	if f := next(t, frames); f.ev.Sprite != "game-2" {
		t.Fatalf("leaked %+v", f.ev)
	}

	// Revoking the policy ends access for new streams.
	status(t, apiCall(t, h, "DELETE", "/v1/sprites/lobby/policy/spawn", ""), http.StatusNoContent)
	resp, _ = http.Get(guest.URL + eventsPath)
	status(t, resp, http.StatusForbidden)
}

func TestGuestServiceReports(t *testing.T) {
	s, h := newOperatorServer(t, Options{})
	status(t, apiCall(t, h, "POST", "/v1/sprites", `{"name":"a"}`), http.StatusCreated)
	sub, _, _, _ := s.life.events.Subscribe(func(e engine.Event) bool { return strings.HasPrefix(e.Type, "service.") }, 0, false)

	status(t, fromInside(t, s, "a", "POST", "/internal/service-event", `{"type":"crashed","service":"web","exit_code":1,"restart_count":2,"restart_in_ms":2000}`), http.StatusNoContent)
	e := <-sub.Events()
	if e.Type != "service.crashed" || e.Sprite != "a" || e.Detail["service"] != "web" || e.Detail["exit_code"] != 1 || e.Detail["restart_in_ms"] != int64(2000) {
		t.Fatalf("event = %+v", e)
	}
	for _, bad := range []string{`{"type":"exploded","service":"web"}`, `{"type":"started","service":"../x"}`, `{"type":"started"}`, `not json`} {
		status(t, fromInside(t, s, "a", "POST", "/internal/service-event", bad), http.StatusBadRequest)
	}
	// A guest reporting in a loop is cut off, not echoed.
	limited := 0
	for i := 0; i < guestEventBurst+5; i++ {
		if fromInside(t, s, "a", "POST", "/internal/service-event", `{"type":"started","service":"web","pid":7}`).StatusCode == http.StatusTooManyRequests {
			limited++
		}
	}
	if limited == 0 {
		t.Fatal("no rate limit on guest reports")
	}
}

// The lifecycle publishes by record; the Server's Describer puts the name and
// the parent back, so an event reads as it did when the lifecycle had the
// sprite in hand -- including about a sprite the store no longer (or does not
// yet) hold, while Delete (or Create) holds it.
func TestEventsAreDescribedByTheFrontEnd(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	l := &Lifecycle{store: st, runtimes: map[string]*runtime{}}
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
	l.SetDescriber(describeSprite)
	if got, want := l.event(child.Record, "sprite.woke", detail), spriteEvent(child, "sprite.woke", detail); !reflect.DeepEqual(got, want) || got.Sprite != "child" || got.ParentID != "p1" {
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
