package server

import (
	"testing"

	"github.com/jhgaylor/mini-sprites/internal/vmm"
)

func stats(target, actual, availMiB int) vmm.BalloonStats {
	return vmm.BalloonStats{TargetMiB: target, ActualMiB: actual, Available: int64(availMiB) << 20}
}

func TestAutoscaleStartsAtStartAndGrowsUnderPressure(t *testing.T) {
	const ram = 4224
	var a autoscaler
	g := a.next(ram, 0, stats(0, 0, 3000))
	if g != autoscaleStartMiB {
		t.Fatalf("start grant %d", g)
	}
	// Little available: double.
	if g = a.next(ram, g, stats(ram-g, ram-g, 50)); g != 2*autoscaleStartMiB {
		t.Fatalf("grew to %d", g)
	}
	// The next tick waits for the deflate to land, even if the stale figure is low.
	if g2 := a.next(ram, g, stats(ram-g, ram-g, 10)); g2 != g {
		t.Fatalf("grew again on stale statistics: %d", g2)
	}
	// Never past the RAM.
	for range 10 {
		g = a.next(ram, g, stats(ram-g, ram-g, 0))
	}
	if g != ram {
		t.Fatalf("grant %d, want the ceiling %d", g, ram)
	}
}

func TestAutoscaleAdoptsWhatTheGuestTookBack(t *testing.T) {
	const ram = 4224
	a := autoscaler{lastActual: ram - 1024}
	// deflate_on_oom let 500 MiB out of the balloon; the guest has room again.
	g := a.next(ram, 1024, stats(ram-1024, ram-1524, 400))
	if g != 1524 {
		t.Fatalf("grant %d, want 1524", g)
	}
	// Still inflating towards a new target: not adopted.
	a = autoscaler{lastActual: 1000}
	if g = a.next(ram, 1024, stats(ram-1024, 2000, 400)); g != 1024 {
		t.Fatalf("grant %d while inflating", g)
	}
}

func TestAutoscaleShrinksAfterCalm(t *testing.T) {
	const ram = 8320
	var a autoscaler
	g := 4096
	for i := 0; i < autoscaleShrinkAge-1; i++ {
		if g2 := a.next(ram, g, stats(ram-g, ram-g, 3500)); g2 != g {
			t.Fatalf("shrank early, tick %d", i)
		}
	}
	// 596 MiB used: keep that plus half, at least a step, never below the start.
	if g = a.next(ram, g, stats(ram-g, ram-g, 3500)); g != autoscaleStartMiB {
		t.Fatalf("shrank to %d", g)
	}
	// A small VM: the start is the RAM, so there is nothing to balloon.
	if g = (&autoscaler{}).next(600, 0, stats(0, 0, 400)); g != 600 {
		t.Fatalf("small VM grant %d", g)
	}
}
