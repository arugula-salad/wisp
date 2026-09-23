package server

import (
	"crypto/hmac"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func quietLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// waitFor polls cond for up to 5s.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		if cond() {
			return
		}
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestWebhookSignsAndRetries(t *testing.T) {
	var mu sync.Mutex
	var got []Event
	var calls atomic.Int32
	recv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		// What a receiver does: recompute the signature over timestamp.body.
		want := signWebhook([]byte("s3cret"), r.Header.Get(tsHeader), body)
		if !hmac.Equal([]byte(r.Header.Get(sigHeader)), []byte(want)) {
			t.Errorf("bad signature %q, want %q", r.Header.Get(sigHeader), want)
		}
		if calls.Add(1) <= 2 {
			http.Error(w, "busy", http.StatusServiceUnavailable)
			return
		}
		var e Event
		json.Unmarshal(body, &e)
		if r.Header.Get("X-Wisp-Event") != e.Type {
			t.Errorf("type header %q for %q", r.Header.Get("X-Wisp-Event"), e.Type)
		}
		mu.Lock()
		got = append(got, e)
		mu.Unlock()
	}))
	defer recv.Close()

	bus := newEventBus()
	hooks := startWebhooks(bus, WebhookOptions{URLs: []string{recv.URL}, Secret: "s3cret", Types: []string{"sprite."}}, quietLog())
	hooks[0].backoff = time.Millisecond
	bus.Publish(Event{Type: "disk.low"}) // filtered out
	bus.Publish(Event{Type: "sprite.woke", Sprite: "a", Detail: map[string]any{"mode": "warm"}})
	waitFor(t, "delivery", func() bool { return hooks[0].delivered.Load() == 1 })
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 1 || got[0].Sprite != "a" || got[0].Detail["mode"] != "warm" || calls.Load() != 3 {
		t.Fatalf("got %+v after %d calls", got, calls.Load())
	}
	if st := hooks[0].status(); st.Failed != 0 || st.LastError == "" {
		t.Fatalf("status %+v", st)
	}
}

func TestWebhookGivesUp(t *testing.T) {
	var calls atomic.Int32
	recv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path == "/gone" {
			http.Error(w, "no", http.StatusGone)
			return
		}
		http.Error(w, "down", http.StatusBadGateway)
	}))
	defer recv.Close()
	bus := newEventBus()
	hooks := startWebhooks(bus, WebhookOptions{URLs: []string{recv.URL + "/gone", recv.URL + "/down"}}, quietLog())
	for _, h := range hooks {
		h.backoff = time.Millisecond
	}
	bus.Publish(Event{Type: "x"})
	waitFor(t, "both to give up", func() bool { return hooks[0].failed.Load() == 1 && hooks[1].failed.Load() == 1 })
	// A 4xx is final; a 5xx is tried webhookAttempts times.
	if n := calls.Load(); n != 1+webhookAttempts {
		t.Fatalf("%d calls, want %d", n, 1+webhookAttempts)
	}
}

func TestWebhookQueueDropsInsteadOfBlocking(t *testing.T) {
	release := make(chan struct{})
	recv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-release }))
	defer recv.Close()
	defer close(release)
	bus := newEventBus()
	hooks := startWebhooks(bus, WebhookOptions{URLs: []string{recv.URL}}, quietLog())
	done := make(chan struct{})
	go func() {
		for i := 0; i < webhookQueue+50; i++ {
			bus.Publish(Event{Type: "x"})
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("a stuck webhook blocked Publish")
	}
	// One is in flight, webhookQueue wait, the rest were dropped.
	if d := hooks[0].dropped.Load(); d < 49 || d > 50 {
		t.Fatalf("dropped %d", d)
	}
}

func TestRedactURL(t *testing.T) {
	for in, want := range map[string]string{
		"https://hooks.example/x":             "https://hooks.example/x",
		"https://user:pw@hooks.example/x":     "https://***@hooks.example/x",
		"https://hooks.example/x?token=abc#f": "https://hooks.example/x?***",
	} {
		if got := redactURL(in); got != want {
			t.Errorf("%s: %s", in, got)
		}
	}
}
