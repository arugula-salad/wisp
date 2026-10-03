package engine

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/arugula-salad/wisp/internal/store"
)

// Boot hooks run in order and the first error stops the rest.
func TestBootHooksOrder(t *testing.T) {
	l := newTestEngine(t, Options{})
	var ran []string
	l.OnBoot(func(ctx context.Context, b Boot) error { ran = append(ran, "a"); return nil })
	l.OnBoot(func(ctx context.Context, b Boot) error { ran = append(ran, "b"); return errors.New("no") })
	l.OnBoot(func(ctx context.Context, b Boot) error { ran = append(ran, "c"); return nil })
	if err := l.runBootHooks(context.Background(), Boot{}); err == nil || len(ran) != 2 || ran[1] != "b" {
		t.Fatalf("err %v, ran %v", err, ran)
	}
}

// A boot hook sees every start of a VM, cold and then warm, before Acquire
// returns it (the sandbox's lock is still held: Peek finds it busy), and one
// that fails fails the start and leaves no VM running.
func TestBootHookOnRunningVMs(t *testing.T) {
	l, base := vmEngine(t, Options{IdleTimeout: time.Hour})
	r := vmSandbox(t, l, base, "hooked", &store.LifecyclePolicy{IdleAction: store.IdleNone})
	var boots []Boot
	var fail error
	l.OnBoot(func(ctx context.Context, b Boot) error {
		if b.Record.ID != r.ID || b.Machine == nil {
			t.Errorf("hook given %+v", b)
		}
		if !l.Peek(r.ID).Busy {
			t.Error("the VM was someone else's before the hook returned")
		}
		boots = append(boots, b)
		return fail
	})
	wake(t, l, r)()
	if len(boots) != 1 || boots[0].Warm {
		t.Fatalf("first start: %+v", boots)
	}
	if err := l.Suspend(r); err != nil {
		t.Fatal(err)
	}
	wake(t, l, r)()
	if len(boots) != 2 || !boots[1].Warm {
		t.Fatalf("resume: %+v", boots)
	}
	if err := l.Suspend(r); err != nil {
		t.Fatal(err)
	}
	fail = errors.New("guest not ready")
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if _, _, err := l.Acquire(ctx, r); err == nil || !errors.Is(err, fail) {
		t.Fatalf("Acquire with a failing hook: %v", err)
	}
	if st := l.Status(r); st == "running" {
		t.Fatalf("a VM whose hook failed is %s", st)
	}
}
