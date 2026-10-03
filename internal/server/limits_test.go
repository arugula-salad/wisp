package server

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/arugula-salad/wisp/engine"
	"github.com/arugula-salad/wisp/internal/store"
)

// The engine's ceilings reach the official SDK in upstream's shape: a
// retryable concurrency error, whichever ceiling it was.
func TestWakeRefusalsInUpstreamsShape(t *testing.T) {
	s, h := newOperatorServer(t, Options{Options: engine.Options{MaxRunningMemoryMiB: 1000, DefaultMemMiB: 2048, IdleTimeout: 30 * time.Second}})
	apiCall(t, h, "POST", "/v1/sprites", `{"name":"big"}`)
	sp, err := s.store.GetByName(store.Sprites, "big")
	if err != nil {
		t.Fatal(err)
	}
	// Admission refuses before anything is started, so this never touches a VM.
	_, _, err = s.life.Acquire(context.Background(), sp.Record)
	if err == nil {
		t.Fatal("2048 MiB was admitted into a 1000 MiB budget")
	}
	rec := httptest.NewRecorder()
	s.writeWakeErr(rec, "big", err)
	e := apiError(t, rec.Result())
	if !e.IsRateLimitError() || !e.IsConcurrentLimitExceeded() || e.Limit != 1000 || e.CurrentCount != 0 ||
		e.GetRetryAfterSeconds() != 30 || e.RetryAfterHeader != 30 {
		t.Fatalf("memory budget = %+v", e)
	}

	// --max-running, as reserveRun reports it.
	rec = httptest.NewRecorder()
	s.writeWakeErr(rec, "x", &engine.LimitError{Which: "max_running", Limit: 1, Current: 1, RetryAfter: 30, Message: "full"})
	if e := apiError(t, rec.Result()); !e.IsRateLimitError() || !e.IsConcurrentLimitExceeded() || e.Limit != 1 || e.CurrentCount != 1 ||
		e.GetRetryAfterSeconds() != 30 || e.RetryAfterHeader != 30 {
		t.Fatalf("max running = %+v", e)
	}

	// The boot cap, as engine/admission.go reports it.
	rec = httptest.NewRecorder()
	s.writeWakeErr(rec, "third", &engine.LimitError{Which: "max_concurrent_boots", Limit: 2, Current: 2, RetryAfter: 5, Message: "busy"})
	if e := apiError(t, rec.Result()); !e.IsRateLimitError() || !e.IsConcurrentLimitExceeded() || e.Limit != 2 || e.GetRetryAfterSeconds() != 5 {
		t.Fatalf("boot cap = %+v", e)
	}
}

// Status reports the admission limits it was given and what is held against them.
func TestStatusReportsTheAdmissionBudget(t *testing.T) {
	s, _ := newOperatorServer(t, Options{Options: engine.Options{MaxRunningMemoryMiB: 8192, MaxConcurrentBoots: 4, DefaultMemMiB: 2048}})
	st := s.status(context.Background(), time.Now(), "127.0.0.1:0")
	if st.Host.MaxRunningMemoryMiB != 8192 || st.Host.ReservedMemoryMiB != 0 ||
		st.Host.MaxConcurrentBoots != 4 || st.Host.BootsInFlight != 0 {
		t.Fatalf("host = %+v", st.Host)
	}
}
