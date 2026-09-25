package server

import (
	"context"
	"errors"
	"fmt"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	sprites "github.com/superfly/sprites-go"

	"github.com/arugula-salad/wisp/internal/store"
)

// newSprite puts a cold sprite in the store and returns it with its runtime, so
// admission can be exercised without anything that would start a VM.
func newSprite(t *testing.T, s *Server, name string, ramMiB int) (store.Sprite, *runtime) {
	t.Helper()
	sp := &store.Sprite{ID: store.NewID(), Name: name, CreatedAt: time.Now()}
	sp.Config.RamMB = ramMiB
	if err := s.store.Create(sp); err != nil {
		t.Fatal(err)
	}
	return *sp, s.life.rt(sp.ID)
}

func TestSpriteRAMIsTheCeilingThePolicyAsksFor(t *testing.T) {
	opts := Options{DefaultMemMiB: 2048}
	var sp store.Sprite
	if got := spriteRAMMiB(sp, opts); got != 2048 {
		t.Fatalf("default = %d, want 2048", got)
	}
	sp.Config.RamMB = 512
	if got := spriteRAMMiB(sp, opts); got != 512 {
		t.Fatalf("config = %d, want 512", got)
	}
	// A memory policy wins, and it is the ceiling that is reserved: the limit
	// plus the VM headroom, never the autoscale grant.
	sp.Resources = &store.ResourcesPolicy{Memory: &store.MemoryPolicy{LimitMB: 1024, Autoscale: true}}
	if got, want := spriteRAMMiB(sp, opts), 1024+vmHeadroomMiB; got != want {
		t.Fatalf("policy ceiling = %d, want %d", got, want)
	}
}

func TestMemoryBudgetRefusesAWakeInUpstreamsShape(t *testing.T) {
	s, _ := newOperatorServer(t, Options{MaxRunningMemoryMiB: 3000, DefaultMemMiB: 2048, IdleTimeout: 30 * time.Second})
	a, spA := s.life.admit, store.Sprite{Name: "a"}
	_, rtA := newSprite(t, s, "a", 0)
	_, rtB := newSprite(t, s, "b", 0)

	if err := a.reserveMemory(rtA, spA.Name, spriteRAMMiB(spA, s.opts)); err != nil {
		t.Fatalf("first 2048 of 3000: %v", err)
	}
	err := a.reserveMemory(rtB, "b", 2048)
	var lim *LimitError
	if !errors.As(err, &lim) {
		t.Fatalf("second reservation: %v", err)
	}
	if lim.Which != "max_running_memory" {
		t.Fatalf("event limit name = %q", lim.Which)
	}
	rec := httptest.NewRecorder()
	s.writeWakeErr(rec, "b", err)
	e := apiError(t, rec.Result())
	if !e.IsRateLimitError() || !e.IsConcurrentLimitExceeded() || e.Limit != 3000 || e.CurrentCount != 2048 ||
		e.GetRetryAfterSeconds() != 30 || e.RetryAfterHeader != 30 {
		t.Fatalf("limit error = %+v", e)
	}

	// The budget is in MiB, so a smaller sprite still fits where a default one did not.
	if err := a.reserveMemory(rtB, "b", 900); err != nil {
		t.Fatalf("900 into the remaining 952: %v", err)
	}
	if got, _ := a.usage(); got != 2948 {
		t.Fatalf("reserved = %d, want 2948", got)
	}
	a.releaseMemory(rtA)
	if got, _ := a.usage(); got != 900 {
		t.Fatalf("after release reserved = %d, want 900", got)
	}
	// Releasing twice must not hand back memory nobody holds.
	a.releaseMemory(rtA)
	if got, _ := a.usage(); got != 900 {
		t.Fatalf("double release reserved = %d, want 900", got)
	}
}

func TestConcurrentBootCapRefusesRatherThanQueues(t *testing.T) {
	s, _ := newOperatorServer(t, Options{MaxConcurrentBoots: 2})
	a := s.life.admit
	for i := 0; i < 2; i++ {
		if err := a.reserveBoot("x"); err != nil {
			t.Fatalf("boot %d of 2: %v", i, err)
		}
	}
	err := a.reserveBoot("third")
	var lim *LimitError
	if !errors.As(err, &lim) {
		t.Fatalf("third boot: %v", err)
	}
	if lim.Which != "max_concurrent_boots" || lim.Limit != 2 || lim.Current != 2 || lim.RetryAfter != bootRetrySeconds {
		t.Fatalf("limit error = %+v", lim)
	}
	rec := httptest.NewRecorder()
	s.writeWakeErr(rec, "third", err)
	if e := apiError(t, rec.Result()); !e.IsRateLimitError() || !e.IsConcurrentLimitExceeded() {
		t.Fatalf("SDK view = %+v", e)
	}
	a.releaseBoot()
	if err := a.reserveBoot("third"); err != nil {
		t.Fatalf("a finished boot should free its slot: %v", err)
	}
	if _, boots := a.usage(); boots != 2 {
		t.Fatalf("boots in flight = %d, want 2", boots)
	}
}

// TestAdmitStartIsNeverOversubscribed is the one that matters: many goroutines
// race through the whole admission path at once, and the budget, the boot cap
// and the running count must all hold at every moment.
func TestAdmitStartIsNeverOversubscribed(t *testing.T) {
	const (
		ram     = 512
		fits    = 4 // 4 * 512 MiB of the 2048 MiB budget
		workers = 64
		rounds  = 40
	)
	s, _ := newOperatorServer(t, Options{MaxRunningMemoryMiB: ram * fits, MaxConcurrentBoots: 3,
		MaxRunning: 6, DefaultMemMiB: ram, IdleTimeout: time.Second})
	a := s.life.admit

	sprites := make([]store.Sprite, workers)
	rts := make([]*runtime, workers)
	for i := range sprites {
		sprites[i], rts[i] = newSprite(t, s, "s"+string(rune('a'+i%26))+string(rune('a'+i/26)), ram)
	}

	var admitted, refused atomic.Int64
	var bad atomic.Value // first violation seen
	note := func(format string, args ...any) {
		bad.CompareAndSwap(nil, fmt.Sprintf(format, args...))
	}
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for r := 0; r < rounds; r++ {
				booted, err := s.life.admitStart(sprites[i], rts[i])
				if err != nil {
					var lim *LimitError
					if !errors.As(err, &lim) {
						note("refusal is not a LimitError: %v", err)
					} else if lim.Which == "" || lim.Message == "" {
						note("refusal has no shape: %+v", lim)
					}
					refused.Add(1)
					continue
				}
				admitted.Add(1)
				// Anything read here was true at some instant while this VM held
				// its reservation, so the invariants must hold for it.
				mem, boots := a.usage()
				if mem > ram*fits {
					note("reserved %d MiB, budget %d", mem, ram*fits)
				}
				if boots > 3 {
					note("%d cold boots in flight, cap 3", boots)
				}
				s.life.mu.Lock()
				running := s.life.running
				s.life.mu.Unlock()
				if running > 6 {
					note("%d running, --max-running 6", running)
				}
				booted()
				booted() // the boot slot is freed once, however often it is reported done
				s.life.releaseStart(rts[i])
			}
		}()
	}
	wg.Wait()
	if v := bad.Load(); v != nil {
		t.Fatal(v)
	}
	if admitted.Load() == 0 || refused.Load() == 0 {
		t.Fatalf("the race proved nothing: %d admitted, %d refused", admitted.Load(), refused.Load())
	}
	// Everything that was taken was given back.
	mem, boots := a.usage()
	s.life.mu.Lock()
	running := s.life.running
	s.life.mu.Unlock()
	if mem != 0 || boots != 0 || running != 0 || len(a.held) != 0 {
		t.Fatalf("leaked after %d starts: %d MiB in %d reservations, %d boots, %d running",
			admitted.Load(), mem, len(a.held), boots, running)
	}
}

// A reservation that cannot be taken must leave the ones before it untouched.
func TestARefusedStartReservesNothing(t *testing.T) {
	s, _ := newOperatorServer(t, Options{MaxRunningMemoryMiB: 1024, MaxConcurrentBoots: 1, DefaultMemMiB: 512})
	spA, rtA := newSprite(t, s, "a", 0)
	spB, rtB := newSprite(t, s, "b", 0)
	spC, rtC := newSprite(t, s, "c", 0)

	bootedA, err := s.life.admitStart(spA, rtA)
	if err != nil {
		t.Fatal(err)
	}
	// B fits the budget but not the boot cap: its memory must not stay reserved.
	if _, err := s.life.admitStart(spB, rtB); err == nil {
		t.Fatal("the boot cap admitted a second boot")
	}
	if mem, boots := s.life.admit.usage(); mem != 512 || boots != 1 {
		t.Fatalf("after a refused boot: %d MiB, %d boots", mem, boots)
	}
	s.life.mu.Lock()
	running := s.life.running
	s.life.mu.Unlock()
	if running != 1 {
		t.Fatalf("a refused start kept a running slot: %d", running)
	}
	bootedA()

	// C now clears the boot cap but not the budget, once B and a third are in.
	if _, err := s.life.admitStart(spB, rtB); err != nil {
		t.Fatal(err)
	}
	if _, err := s.life.admitStart(spC, rtC); err == nil {
		t.Fatal("1024 MiB held three 512 MiB sprites")
	}
	if mem, _ := s.life.admit.usage(); mem != 1024 {
		t.Fatalf("reserved = %d MiB, want 1024", mem)
	}
}

// A refused wake says so on the event stream, naming the limit that refused it.
func TestWakeRefusalPublishesLimitRefused(t *testing.T) {
	s, _ := newOperatorServer(t, Options{MaxRunningMemoryMiB: 256, DefaultMemMiB: 2048})
	var mu sync.Mutex
	var got []Event
	s.life.events.addSink(func(e Event) {
		if e.Type == "limit.refused" {
			mu.Lock()
			got = append(got, e)
			mu.Unlock()
		}
	})
	sp, _ := newSprite(t, s, "big", 0)
	// Admission refuses before anything is started, so this never touches a VM.
	if _, _, err := s.life.Acquire(context.Background(), sp); err == nil {
		t.Fatal("2048 MiB was admitted into a 256 MiB budget")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 1 {
		t.Fatalf("events = %+v", got)
	}
	if d := got[0].Detail; got[0].Sprite != "big" || d["limit"] != "max_running_memory" || d["max"] != 256 || d["current"] != 0 {
		t.Fatalf("event = %+v", got[0])
	}
}

func TestStatusReportsTheAdmissionBudget(t *testing.T) {
	s, _ := newOperatorServer(t, Options{MaxRunningMemoryMiB: 8192, MaxConcurrentBoots: 4, DefaultMemMiB: 2048})
	sp, rt := newSprite(t, s, "one", 0)
	booted, err := s.life.admitStart(sp, rt)
	if err != nil {
		t.Fatal(err)
	}
	st := s.status(context.Background(), time.Now(), "127.0.0.1:0")
	if st.Host.MaxRunningMemoryMiB != 8192 || st.Host.ReservedMemoryMiB != 2048 ||
		st.Host.MaxConcurrentBoots != 4 || st.Host.BootsInFlight != 1 {
		t.Fatalf("host = %+v", st.Host)
	}
	booted()
	s.life.releaseStart(rt)
	if st := s.status(context.Background(), time.Now(), "127.0.0.1:0"); st.Host.ReservedMemoryMiB != 0 || st.Host.BootsInFlight != 0 {
		t.Fatalf("after release host = %+v", st.Host)
	}
}

// The official SDK must keep parsing a memory refusal as a retryable
// concurrency error, since that is the only retryable shape it knows.
func TestSDKSeesAMemoryRefusalAsRetryable(t *testing.T) {
	s, _ := newOperatorServer(t, Options{MaxRunningMemoryMiB: 1, DefaultMemMiB: 2048, IdleTimeout: 45 * time.Second})
	_, rt := newSprite(t, s, "a", 0)
	err := s.life.admit.reserveMemory(rt, "a", 2048)
	rec := httptest.NewRecorder()
	s.writeWakeErr(rec, "a", err)
	var e *sprites.APIError = apiError(t, rec.Result())
	if e.GetRetryAfterSeconds() != 45 {
		t.Fatalf("retry after = %d, want the idle timeout", e.GetRetryAfterSeconds())
	}
}
