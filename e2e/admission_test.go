//go:build e2e

package e2e

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"sync"
	"testing"
	"time"

	sprites "github.com/superfly/sprites-go"
)

// Host admission (internal/server/admission.go) against a real daemon: the
// aggregate running-memory budget and the concurrent cold-boot cap. Both are
// operator flags with no default, so each subtest needs a daemon started with
// the limit and the same number in the environment:
//
//	spritesd --mem-mib 1024 --max-running-memory-mib 1024 --max-concurrent-boots 1 ...
//	SPRITES_E2E_MEM_MIB=1024 SPRITES_E2E_MAX_RUNNING_MEMORY_MIB=1024 \
//	  SPRITES_E2E_MAX_CONCURRENT_BOOTS=1 go test -tags e2e -run Admission ./e2e/
//
// on a daemon with no other sprites running, since the budget is host-wide.

func envInt(name string) int {
	n, _ := strconv.Atoi(os.Getenv(name))
	return n
}

// admissionSprite creates a sprite that is deleted with the test.
func admissionSprite(t *testing.T, c *sprites.Client, ctx context.Context, name string) *sprites.Sprite {
	t.Helper()
	sp, err := c.CreateSprite(ctx, name, nil)
	if err != nil {
		t.Fatalf("create %s: %v", name, err)
	}
	t.Cleanup(func() { c.DeleteSprite(context.Background(), name) })
	return sp
}

func TestAdmissionRunningMemoryBudget(t *testing.T) {
	budget, mem := envInt("SPRITES_E2E_MAX_RUNNING_MEMORY_MIB"), envInt("SPRITES_E2E_MEM_MIB")
	if budget == 0 || mem == 0 {
		t.Skip("needs a daemon with --max-running-memory-mib and --mem-mib, and SPRITES_E2E_MAX_RUNNING_MEMORY_MIB / SPRITES_E2E_MEM_MIB set to match")
	}
	if budget >= 2*mem {
		t.Skipf("a budget of %d MiB holds two %d MiB sprites; set --max-running-memory-mib below twice --mem-mib", budget, mem)
	}
	c := client(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	stamp := time.Now().UnixNano() % 1e9

	// Watch the operator stream, so the refusal is checked where an operator
	// would see it as well as where the caller does.
	events := openEvents(t, "type=limit.refused", nil)

	first := admissionSprite(t, c, ctx, fmt.Sprintf("e2e-mem-%d-a", stamp))
	second := admissionSprite(t, c, ctx, fmt.Sprintf("e2e-mem-%d-b", stamp))

	// Hold the whole budget: the first sprite stays awake for as long as this runs.
	hold := first.CommandContext(ctx, "sleep", "30")
	if err := hold.Start(); err != nil {
		t.Fatalf("start the first sprite: %v", err)
	}
	defer hold.Wait()
	if out, err := first.CommandContext(ctx, "echo", "up").Output(); err != nil || string(out) != "up\n" {
		t.Fatalf("first sprite: %q %v", out, err)
	}

	_, err := second.CommandContext(ctx, "echo", "hi").Output()
	var apiErr *sprites.APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("waking past the memory budget: want an APIError, got %T: %v", err, err)
	}
	// Reusing upstream's concurrency shape is deliberate: it is the only
	// retryable limit the SDKs know, and the units here are MiB.
	if !apiErr.IsConcurrentLimitExceeded() || !apiErr.IsRateLimitError() {
		t.Fatalf("limit error = %+v", apiErr)
	}
	if apiErr.Limit != budget || apiErr.CurrentCount < mem || apiErr.GetRetryAfterSeconds() < 1 {
		t.Fatalf("limit error = %+v (budget %d MiB, one sprite %d MiB)", apiErr, budget, mem)
	}

	deadline := time.After(20 * time.Second)
	for {
		select {
		case f := <-events:
			if f.ev.Type != "limit.refused" || f.ev.Sprite != second.Name() {
				continue
			}
			if f.ev.Detail["limit"] != "max_running_memory" {
				t.Fatalf("limit.refused names %v, want max_running_memory", f.ev.Detail["limit"])
			}
			if n, _ := f.ev.Detail["max"].(float64); int(n) != budget {
				t.Fatalf("limit.refused detail = %v", f.ev.Detail)
			}
			return
		case <-deadline:
			t.Fatal("no limit.refused event for the refused wake")
		}
	}
}

// The budget must hold under simultaneous wakes, not only sequential ones: two
// requests that each fit on their own must not both be admitted into room for
// one. This is the case a check that only looked at running VMs would get wrong.
func TestAdmissionConcurrentWakesDoNotBothFit(t *testing.T) {
	budget, mem := envInt("SPRITES_E2E_MAX_RUNNING_MEMORY_MIB"), envInt("SPRITES_E2E_MEM_MIB")
	if budget == 0 || mem == 0 || budget >= 2*mem {
		t.Skip("needs a daemon whose --max-running-memory-mib holds one --mem-mib sprite but not two")
	}
	c := client(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	stamp := time.Now().UnixNano() % 1e9

	sps := make([]*sprites.Sprite, 3)
	for i := range sps {
		sps[i] = admissionSprite(t, c, ctx, fmt.Sprintf("e2e-race-%d-%d", stamp, i))
	}

	start := make(chan struct{})
	var wg sync.WaitGroup
	errs := make([]error, len(sps))
	for i, sp := range sps {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, errs[i] = sp.CommandContext(ctx, "echo", "hi").Output()
		}()
	}
	close(start)
	wg.Wait()

	woke, refused := 0, 0
	for i, err := range errs {
		var apiErr *sprites.APIError
		switch {
		case err == nil:
			woke++
		case errors.As(err, &apiErr) && apiErr.IsConcurrentLimitExceeded():
			refused++
		default:
			t.Fatalf("sprite %d: want a wake or a limit error, got %T: %v", i, err, err)
		}
	}
	if woke > 1 {
		t.Fatalf("%d of %d sprites woke into a budget that holds one", woke, len(sps))
	}
	if refused == 0 {
		t.Fatalf("nothing was refused: %d woke", woke)
	}
}

func TestAdmissionConcurrentBootCap(t *testing.T) {
	maxBoots := envInt("SPRITES_E2E_MAX_CONCURRENT_BOOTS")
	if maxBoots == 0 {
		t.Skip("needs a daemon with --max-concurrent-boots N and SPRITES_E2E_MAX_CONCURRENT_BOOTS=N")
	}
	c := client(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	stamp := time.Now().UnixNano() % 1e9

	// Enough at once that some must collide with the cap; they are all cold,
	// having just been created.
	n := maxBoots + 3
	sps := make([]*sprites.Sprite, n)
	for i := range sps {
		sps[i] = admissionSprite(t, c, ctx, fmt.Sprintf("e2e-boot-%d-%d", stamp, i))
	}

	start := make(chan struct{})
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i, sp := range sps {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, errs[i] = sp.CommandContext(ctx, "echo", "hi").Output()
		}()
	}
	close(start)
	wg.Wait()

	refused := 0
	for i, err := range errs {
		if err == nil {
			continue
		}
		var apiErr *sprites.APIError
		if !errors.As(err, &apiErr) {
			t.Fatalf("sprite %d: want a wake or a limit error, got %T: %v", i, err, err)
		}
		// The memory budget may be what refused it, if one is configured too;
		// either way it must be retryable and carry a Retry-After.
		if !apiErr.IsConcurrentLimitExceeded() || !apiErr.IsRateLimitError() || apiErr.GetRetryAfterSeconds() < 1 {
			t.Fatalf("sprite %d: limit error = %+v", i, apiErr)
		}
		refused++
	}
	if refused == 0 {
		t.Skipf("%d simultaneous cold boots never overlapped past a cap of %d; nothing was proved", n, maxBoots)
	}
	if envInt("SPRITES_E2E_MAX_RUNNING_MEMORY_MIB") > 0 {
		return // a memory budget would refuse the retries below for its own reasons
	}
	// A refusal is a "come back", not a failure: with the rush over, the ones
	// that were turned away must boot.
	for i, err := range errs {
		if err == nil {
			continue
		}
		if out, err := sps[i].CommandContext(ctx, "echo", "hi").Output(); err != nil || string(out) != "hi\n" {
			t.Fatalf("sprite %d after the cap cleared: %q %v", i, out, err)
		}
	}
}
