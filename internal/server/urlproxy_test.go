package server

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// refused is what a guest port that nothing has bound yet looks like from here:
// the agent answers the tunnel request with a 502 and dialGuestTCP turns that
// into an error.
var refused = errors.New("nothing is listening on the http port inside the sprite")

// appAt returns a dialer onto a backend that only starts accepting after the
// nth attempt, and a counter of the attempts made.
func appAt(t *testing.T, ready int32) (func(context.Context) (net.Conn, error), *atomic.Int32) {
	t.Helper()
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("the app answered"))
	}))
	t.Cleanup(backend.Close)
	var attempts atomic.Int32
	return func(ctx context.Context) (net.Conn, error) {
		if attempts.Add(1) < ready {
			return nil, refused
		}
		var d net.Dialer
		return d.DialContext(ctx, "tcp", backend.Listener.Addr().String())
	}, &attempts
}

func spriteRequest() (*httptest.ResponseRecorder, *http.Request) {
	r := httptest.NewRequest(http.MethodGet, "http://app.widgets.test/", nil)
	return httptest.NewRecorder(), r
}

// The cold-boot case: the sprite is awake, its app binds its port a moment
// later, and the visitor gets the page rather than a proxy error.
func TestURLProxyWaitsForTheAppToListen(t *testing.T) {
	dial, attempts := appAt(t, 4)
	w, r := spriteRequest()
	spriteURLProxy(dial, 5*time.Second, false).ServeHTTP(w, r)
	if w.Code != http.StatusOK || w.Body.String() != "the app answered" {
		t.Fatalf("got %d %q, want 200 from the app once it was listening", w.Code, w.Body)
	}
	if n := attempts.Load(); n < 4 {
		t.Fatalf("%d dial attempts, want the gate to have retried", n)
	}
}

// When the wait expires the visitor is told the truth: temporary, come back,
// and not the 502 that says the site is broken.
func TestURLProxyAnswers503WhenTheAppNeverComesUp(t *testing.T) {
	dial, attempts := appAt(t, 1<<30) // never ready
	w, r := spriteRequest()
	start := time.Now()
	spriteURLProxy(dial, 600*time.Millisecond, false).ServeHTTP(w, r)
	elapsed := time.Since(start)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("got %d, want 503", w.Code)
	}
	if got := w.Header().Get("Retry-After"); got != "5" {
		t.Errorf("Retry-After = %q, want 5", got)
	}
	if body := w.Body.String(); !strings.Contains(body, "starting up") {
		t.Errorf("body %q does not say the sprite is starting", body)
	}
	// Bounded: a visitor never waits indefinitely, and the retries are paced
	// rather than a spin.
	if elapsed > 3*time.Second {
		t.Errorf("waited %v on a 600ms budget", elapsed)
	}
	if n := attempts.Load(); n < 2 || n > 30 {
		t.Errorf("%d dial attempts in 600ms: want a few, paced", n)
	}
}

// A visitor who gives up takes the wait with them: nothing keeps dialing a
// sprite that is idle again the moment the request is over.
func TestURLReadyGateStopsWhenTheVisitorLeaves(t *testing.T) {
	dial, attempts := appAt(t, 1<<30)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := dialWhenReady(ctx, time.Minute, dial)
		done <- err
	}()
	time.Sleep(250 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the gate outlived the request")
	}
	after := attempts.Load()
	time.Sleep(300 * time.Millisecond)
	if now := attempts.Load(); now != after {
		t.Fatalf("dialling continued after the request went away: %d -> %d", after, now)
	}
}

// Whatever --url-ready-wait says, the ceiling stands, and an operator slip
// (a negative duration) means the default rather than the gate turning off.
func TestURLReadyGateCeiling(t *testing.T) {
	for _, tc := range []struct{ in, want time.Duration }{
		{10 * time.Minute, maxURLReadyWait},
		{3 * time.Second, 3 * time.Second},
		{0, 0}, // explicitly off: fail on the first refused connection
		{-2 * time.Second, defaultURLReadyWait},
	} {
		if got := clampReadyWait(tc.in); got != tc.want {
			t.Errorf("clampReadyWait(%v) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

// With the gate off the proxy behaves exactly as it did before it existed: one
// attempt, and the dial error reported as a bad gateway.
func TestURLProxyWithoutTheGateFailsAtOnce(t *testing.T) {
	dial, attempts := appAt(t, 1<<30)
	w, r := spriteRequest()
	spriteURLProxy(dial, 0, false).ServeHTTP(w, r)
	if w.Code != http.StatusBadGateway {
		t.Fatalf("got %d, want 502", w.Code)
	}
	if n := attempts.Load(); n != 1 {
		t.Fatalf("%d dial attempts with the gate off, want 1", n)
	}
}

// Each attempt is bounded too: a dial that hangs inside the guest must not
// stretch the visitor's wait past the budget.
func TestURLReadyGateBoundsASingleSlowAttempt(t *testing.T) {
	var attempts atomic.Int32
	hang := func(ctx context.Context) (net.Conn, error) {
		attempts.Add(1)
		<-ctx.Done() // as the agent would, waiting for a service that never listens
		return nil, ctx.Err()
	}
	start := time.Now()
	_, err := dialWhenReady(context.Background(), 400*time.Millisecond, hang)
	if !errors.Is(err, errSpriteNotReady) {
		t.Fatalf("err = %v, want errSpriteNotReady", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("one hanging attempt took %v on a 400ms budget", elapsed)
	}
	if n := attempts.Load(); n != 1 {
		t.Fatalf("%d attempts, want the single hanging one", n)
	}
}
