package server

import (
	"context"
	"errors"
	"time"

	"github.com/arugula-salad/wisp/internal/store"
	"github.com/arugula-salad/wisp/internal/vmm"
)

// Memory autoscale (resources.memory.autoscale). Upstream describes it as a
// sprite that starts with some memory and grows towards a ceiling under
// pressure. Here the VM boots with the ceiling (limit_mb + vmHeadroomMiB, as
// without autoscale) and the balloon holds everything above the grant, which
// starts at autoscaleStartMiB. Once a second the controller reads the guest's
// balloon statistics: when the memory available to the guest runs low it
// deflates the balloon, and when the guest has used much less than its grant
// for a while it inflates it again, never below the start.
//
// What that buys is host memory: pages in the balloon are handed back to the
// host, so a sprite costs at most its grant rather than its ceiling, page cache
// included, and a warm snapshot is squeezed from a smaller base. What it does
// not change: the guest sees the ceiling as MemTotal (the balloon runs with
// deflate_on_oom, and Linux then counts ballooned pages as used rather than
// removing them), the workload's own limit is still limit_mb, and the host
// cgroup is still sized for the ceiling, because the guest may deflate.
//
// The guest cannot wait for us: a burst faster than a second can run a guest
// out of its grant. Then deflate_on_oom hands it pages from the balloon rather
// than invoking the OOM killer, in 1 MiB steps, and the controller sees the
// balloon shrink under its target and adopts that as the new grant.
const (
	autoscaleStartMiB  = 1024
	autoscaleStepMiB   = 256
	autoscaleShrinkAge = 30 // seconds of low use before the grant shrinks
)

// autoscaler is one VM's controller state between ticks.
type autoscaler struct {
	calm       int // consecutive ticks with plenty to spare
	lastActual int // balloon size at the previous tick
	settle     int // ticks to let a deflate land before judging pressure again
}

// next decides one tick: ram is the VM's RAM, grant the current grant (0 for
// none yet), st the guest's statistics. It returns the new grant.
func (a *autoscaler) next(ram, grant int, st vmm.BalloonStats) int {
	start := min(ram, autoscaleStartMiB)
	if grant <= 0 {
		grant = start
	}
	// Short of its target and not getting closer: the guest took pages back
	// (deflate_on_oom) or cannot give up as many as asked. What it holds is its
	// grant now.
	if st.ActualMiB < st.TargetMiB && st.ActualMiB <= a.lastActual {
		grant = max(grant, ram-st.ActualMiB)
	}
	a.lastActual = st.ActualMiB
	avail := int(st.Available >> 20)
	if a.settle > 0 {
		// The statistics are up to a second old, and the guest deflates on its
		// own time: judging now would grow again for the same pressure.
		a.settle--
		return min(max(grant, start), ram)
	}
	switch {
	case avail < max(autoscaleStepMiB/2, grant/5):
		// Double (at least a step): pressure tends to keep coming, and a grant
		// costs the host nothing until the guest touches it.
		grant += max(autoscaleStepMiB, grant)
		a.calm, a.settle = 0, 1
	case avail > grant/2+autoscaleStepMiB:
		a.calm++
		if a.calm >= autoscaleShrinkAge {
			a.calm = 0
			used := grant - avail
			if want := max(start, used+max(autoscaleStepMiB, used/2)); want <= grant-autoscaleStepMiB {
				grant = want
			}
		}
	default:
		a.calm = 0
	}
	return min(max(grant, start), ram)
}

// autoscaleOn reports whether sp's policy asks for autoscale.
func autoscaleOn(sp store.Sprite) bool {
	return sp.Resources != nil && sp.Resources.Memory != nil && sp.Resources.Memory.Autoscale
}

// balloonTarget is the balloon size a VM should have right now: the RAM above
// the grant under autoscale, nothing otherwise.
func balloonTarget(rt *runtime, sp store.Sprite, ram int) int {
	if !autoscaleOn(sp) {
		return 0
	}
	if rt.grantMiB <= 0 {
		rt.grantMiB = min(ram, autoscaleStartMiB)
	}
	return ram - rt.grantMiB
}

// setBalloon puts the balloon where it belongs after a boot or a resume (a
// suspend squeezes it; see vmm.Squeeze). Callers hold rt.mu.
func (l *Lifecycle) setBalloon(ctx context.Context, sp store.Sprite, rt *runtime, m *vmm.Machine, resumed bool) {
	target := balloonTarget(rt, sp, m.MemMiB())
	if target == 0 && !resumed {
		return // a fresh VM's balloon is empty
	}
	if err := m.SetBalloon(ctx, target); err != nil {
		// A VM resumed from a snapshot taken before VMs had a balloon has none.
		l.log.Info("balloon not set (a VM from before balloons has none until its next cold boot)", "sprite", sp.Name, "err", err)
	}
}

// autoscale runs the controller for one VM until it exits. It only acts while
// the policy asks for autoscale; turning it off hands everything back.
func (l *Lifecycle) autoscale(sp store.Sprite, rt *runtime, m *vmm.Machine) {
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	var a autoscaler
	// The boot may have set the balloon for a policy that has changed since:
	// look at it once whatever the policy says now.
	was := true
	for {
		select {
		case <-m.Exited():
			return
		case <-tick.C:
		}
		cur, err := l.store.Get(sp.Name)
		if err != nil {
			return
		}
		on := autoscaleOn(cur)
		if !on && !was {
			continue
		}
		// Suspends and checkpoints hold rt.mu and may be moving the balloon
		// themselves; skip the tick rather than wait.
		if !rt.mu.TryLock() {
			continue
		}
		if rt.m != m {
			rt.mu.Unlock()
			return
		}
		stop := l.autoscaleTick(cur, rt, m, on, &a)
		rt.mu.Unlock()
		if stop {
			return
		}
		was = on
	}
}

// autoscaleTick is one controller step; callers hold rt.mu. It reports true
// when the VM cannot be autoscaled at all.
func (l *Lifecycle) autoscaleTick(sp store.Sprite, rt *runtime, m *vmm.Machine, on bool, a *autoscaler) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if !on {
		if err := m.SetBalloon(ctx, 0); err == nil && rt.grantMiB > 0 {
			l.log.Info("memory autoscale off; balloon emptied", "sprite", sp.Name)
		}
		rt.grantMiB = 0
		return false
	}
	st, err := m.Balloon(ctx)
	if errors.Is(err, vmm.ErrNoBalloon) {
		l.log.Warn("memory autoscale needs a cold boot: this VM was resumed from a snapshot without a balloon", "sprite", sp.Name)
		return true
	}
	if err != nil {
		return false
	}
	ram := m.MemMiB()
	old := rt.grantMiB
	rt.grantMiB = a.next(ram, old, st)
	if rt.grantMiB == old && st.TargetMiB == ram-old {
		return false
	}
	if err := m.SetBalloon(ctx, ram-rt.grantMiB); err != nil {
		l.log.Warn("memory autoscale: balloon update failed", "sprite", sp.Name, "err", err)
		return false
	}
	if rt.grantMiB != old {
		l.log.Info("memory autoscale", "sprite", sp.Name, "grant_mib", rt.grantMiB, "was", old,
			"ceiling_mib", ram, "available_mib", st.Available>>20)
	}
	return false
}
