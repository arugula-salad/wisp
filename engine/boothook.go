package engine

import (
	"context"

	"github.com/arugula-salad/wisp/internal/store"
	"github.com/arugula-salad/wisp/internal/vmm"
)

// Boot is a VM that has just started for a sandbox, as a boot hook sees it:
// the guest agent answers, and nothing else has been given the VM yet.
type Boot struct {
	// Record is the sandbox, as the store had it when the start began.
	Record store.Record
	// Warm is a resume from the sandbox's memory snapshot, where the guest's
	// processes carry on as they were. Otherwise it is a cold boot: a fresh
	// kernel and fresh services on the sandbox's disk, which is also what every
	// checkpoint restore is, and what a wake becomes when its snapshot could
	// not be used.
	Warm bool
	// Machine is the VM, for DialPort. It is only the hook's until it returns.
	Machine *vmm.Machine
}

// A BootHook prepares a guest a front end runs a service of its own in (E2B's
// envd, which is handed its access token after every start) before the VM is
// anyone else's. See OnBoot.
type BootHook func(ctx context.Context, b Boot) error

// OnBoot registers f to run every time a VM starts for a sandbox, cold or
// warm, after the guest agent answers and before the start completes: before
// Acquire returns the VM, the idle watcher sees it, or sprite.woke goes out.
// So no request reaches a guest its front end has not prepared. Hooks run in
// the order they were registered, for every record of every API; a hook skips
// the records that are not its front end's.
//
// f runs holding the sandbox's transition lock, so it must not call an Engine
// method that takes it (Acquire, Suspend, Delete, the deadline and policy
// setters, ...). It may dial the guest (DialPort on b.Machine) and read the
// store. ctx bounds the whole start. An error fails the start: the VM is
// killed and Acquire returns the error, so a hook that can live without its
// work (a warm resume whose state survived) logs and returns nil instead.
// Register hooks before anything boots.
func (l *Engine) OnBoot(f BootHook) {
	l.mu.Lock()
	l.onBoot = append(l.onBoot, f)
	l.mu.Unlock()
}

// runBootHooks runs the OnBoot hooks on a VM bootLocked has just started.
func (l *Engine) runBootHooks(ctx context.Context, b Boot) error {
	l.mu.Lock()
	hooks := l.onBoot
	l.mu.Unlock()
	for _, f := range hooks {
		if err := f(ctx, b); err != nil {
			return err
		}
	}
	return nil
}
