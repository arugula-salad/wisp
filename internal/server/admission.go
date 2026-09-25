package server

import (
	"fmt"
	"log/slog"
	"math"
	"sync"

	"github.com/arugula-salad/wisp/internal/store"
	"github.com/arugula-salad/wisp/internal/vmm"
)

// Host admission for the two things a count ceiling cannot express.
//
// --max-running counts VMs, and ten 256 MiB sprites are not ten 4 GiB ones, so
// Options.MaxRunningMemoryMiB is an aggregate budget in MiB that every boot and
// every resume is measured against. Options.MaxConcurrentBoots caps cold boots
// in flight, because that is the expensive moment: Firecracker start, guest
// init, services coming up, all of it CPU and page faults at once. A resume is
// not capped; it costs a fraction of a boot, and a warm sprite that cannot be
// resumed on demand is a sprite nobody can use.
//
// Both refuse with the same retryable LimitError the SDKs already parse, and
// neither queues: a caller is told to come back, not made to wait. A bounded
// wake queue would be the next step and is deliberately not this one.
//
// What is reserved, and why it is the ceiling. A sprite's RAM is
// resources.memory.limit_mb + vmHeadroomMiB (policy_limits.go), or the daemon
// default. Under memory autoscale the balloon holds back everything above the
// grant, so a sprite's *current* cost to the host is far below that ceiling --
// and reserving the grant would still be wrong, because autoscale.go's whole
// premise is that the guest may deflate at will: deflate_on_oom hands pages
// back faster than the controller ticks, and the host cgroup is sized for the
// ceiling for exactly that reason. A budget that admitted against the grant
// would admit sprites the host cannot hold the moment they get busy. So the
// ceiling is what is reserved.
//
// The cost of that choice is honest under-subscription: a host running mostly
// idle autoscaled sprites will refuse wakes while it still has free memory.
// An operator who wants the density sets the budget above physical RAM and
// accepts the swap or the OOM killer; there is no half-measure that is also
// safe.
//
// What this does NOT promise. It is admission accounting, not a guarantee
// against host memory pressure:
//   - A guest may deflate its balloon at any time, up to its ceiling. That is
//     within the reservation, which is the point, but it means "reserved" is
//     not "in use" in either direction.
//   - Host page cache for sprite disks is not accounted, and on a busy volume
//     it is not small.
//   - Firecracker's own overhead beyond guest RAM, the snapshot written during
//     a suspend, wispd itself and anything else on the host are not
//     accounted either.
//   - A sprite already running is never evicted to fit a new one. The budget
//     only ever refuses arrivals.
//
// Set the budget below physical RAM with room for all of that, and treat it as
// a brake on overcommit rather than a guarantee.

// bootRetrySeconds is the Retry-After on a refused cold boot: a boot in flight
// finishes in seconds, so a client that waits this long usually gets in.
const bootRetrySeconds = 5

// admission is the host-wide memory and concurrent-boot accounting. Every field
// under mu is the truth about reservations, not about the host: see above.
type admission struct {
	budgetMiB int // 0 = no budget
	maxBoots  int // 0 = no cap
	idleRetry int // seconds until an idle sprite might free memory

	mu sync.Mutex
	// held is the MiB reserved per starting-or-running VM, keyed by its runtime,
	// which is what cleanupLocked has in hand. Keeping the per-VM figure here
	// rather than recomputing it on release means a policy change mid-flight
	// cannot leak or over-return a reservation.
	held     map[*runtime]int
	reserved int // sum of held
	boots    int // cold boots in flight
}

func newAdmission(opts Options, log *slog.Logger) *admission {
	a := &admission{budgetMiB: opts.MaxRunningMemoryMiB, maxBoots: opts.MaxConcurrentBoots,
		idleRetry: max(int(math.Ceil(opts.IdleTimeout.Seconds())), 1), held: map[*runtime]int{}}
	if a.budgetMiB > 0 {
		if host := hostMemMiB(); host > 0 && a.budgetMiB > host {
			log.Warn("the running-memory budget is larger than this host's RAM, so it will not refuse anything before the host is under pressure",
				"budget_mib", a.budgetMiB, "host_mib", host)
		}
		log.Info("host memory admission on", "budget_mib", a.budgetMiB)
	}
	if a.maxBoots > 0 {
		log.Info("concurrent cold boots capped", "max", a.maxBoots)
	}
	return a
}

// spriteRAMMiB is the guest RAM the sprite's VM will be given, which is what the
// budget reserves. It mirrors vmConfig + applyPolicy, in that order of
// precedence: a memory policy wins over the sprite's own config, which wins
// over the daemon default.
func spriteRAMMiB(sp store.Sprite, opts Options) int {
	if sp.Resources != nil && sp.Resources.Memory != nil && sp.Resources.Memory.LimitMB > 0 {
		return sp.Resources.Memory.LimitMB + vmHeadroomMiB
	}
	if sp.Config.RamMB > 0 {
		return sp.Config.RamMB
	}
	return opts.DefaultMemMiB
}

// reserveMemory claims mib for a VM about to start. Taking the reservation here,
// under one lock and before anything is started, is what stops two simultaneous
// wakes from both being told they fit into room for one.
func (a *admission) reserveMemory(rt *runtime, name string, mib int) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.budgetMiB > 0 && a.reserved+mib > a.budgetMiB {
		return &LimitError{Code: codeConcurrentLimit, Which: "max_running_memory",
			Limit: a.budgetMiB, Current: a.reserved, RetryAfter: a.idleRetry,
			Message: fmt.Sprintf("running sprites already hold %d MiB of the host's %d MiB memory budget (--max-running-memory-mib) and %s needs %d MiB more; one frees up when a sprite goes idle",
				a.reserved, a.budgetMiB, name, mib)}
	}
	// Recorded even with no budget configured, so `wispd status` can report
	// what a budget would have to be to hold what is running now.
	a.held[rt] = mib
	a.reserved += mib
	return nil
}

// releaseMemory gives a stopped VM's reservation back. It is idempotent, since
// a failed start and the cleanup of a VM that exited both pass through here.
func (a *admission) releaseMemory(rt *runtime) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.reserved -= a.held[rt]
	delete(a.held, rt)
}

// reserveBoot claims one of the concurrent cold-boot slots.
func (a *admission) reserveBoot(name string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.maxBoots > 0 && a.boots >= a.maxBoots {
		return &LimitError{Code: codeConcurrentLimit, Which: "max_concurrent_boots",
			Limit: a.maxBoots, Current: a.boots, RetryAfter: bootRetrySeconds,
			Message: fmt.Sprintf("%d sprites are already cold booting, the most this host starts at once (--max-concurrent-boots); %s can boot when one of them is up", a.boots, name)}
	}
	a.boots++
	return nil
}

func (a *admission) releaseBoot() {
	a.mu.Lock()
	a.boots--
	a.mu.Unlock()
}

// usage is the live view for `wispd status`.
func (a *admission) usage() (reservedMiB, boots int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.reserved, a.boots
}

// admitStart takes every host reservation a VM needs before it is started: the
// MaxRunning count slot (limits.go), the memory budget, and for a cold boot a
// concurrent-boot slot. The returned booted is called once the boot attempt is
// over, whether it worked or not, and frees only the boot slot; the count and
// the memory stay reserved for as long as the VM exists and are given back by
// releaseStart. On error nothing is left reserved.
func (l *Lifecycle) admitStart(sp store.Sprite, rt *runtime) (booted func(), err error) {
	if err := l.reserveRun(); err != nil {
		return nil, err
	}
	if err := l.admit.reserveMemory(rt, sp.Name, spriteRAMMiB(sp, l.opts)); err != nil {
		l.releaseRun()
		return nil, err
	}
	// A resume is not capped; only a cold boot is. A warm restore that falls
	// back to a cold boot (bootLocked) therefore slips past the cap: it is rare,
	// and refusing a sprite whose memory state has just been lost would be a
	// second insult rather than a protection.
	if vmm.HasSnapshot(l.store.Dir(sp.ID)) {
		return func() {}, nil
	}
	if err := l.admit.reserveBoot(sp.Name); err != nil {
		l.admit.releaseMemory(rt)
		l.releaseRun()
		return nil, err
	}
	var once sync.Once
	return func() { once.Do(l.admit.releaseBoot) }, nil
}

// releaseStart gives back what admitStart reserved for the life of the VM.
func (l *Lifecycle) releaseStart(rt *runtime) {
	l.admit.releaseMemory(rt)
	l.releaseRun()
}
