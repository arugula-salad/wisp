package server

import (
	"context"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/arugula-salad/wisp/internal/store"
	"github.com/arugula-salad/wisp/internal/vmm"
)

// The lifecycle rules on running sandboxes. These boot real microVMs, as the
// vmm package's tests do: they need /dev/kvm and the firecracker, kernel,
// initrd and base image under $WISP_DATA (or the default data directory, which
// they only read), and skip otherwise.

// vmServer is a daemon that can boot VMs, with no network.
func vmServer(t *testing.T, opts Options) (*Server, string) {
	t.Helper()
	if testing.Short() {
		t.Skip("boots VMs")
	}
	data := os.Getenv("WISP_DATA")
	if data == "" {
		home, _ := os.UserHomeDir()
		data = filepath.Join(home, ".local", "share", "wisp")
	}
	h := vmm.Host{Firecracker: filepath.Join(data, "bin", "firecracker"),
		Kernel: filepath.Join(data, "kernel", "vmlinux"), Initrd: filepath.Join(data, "initrd.cpio")}
	base := filepath.Join(data, "images", "base.ext4")
	for _, p := range []string{"/dev/kvm", h.Firecracker, h.Kernel, h.Initrd, base} {
		f, err := os.OpenFile(p, os.O_RDONLY, 0)
		if err != nil {
			t.Skipf("skipping VM test: %v", err)
		}
		f.Close()
	}
	opts.Host, opts.DataDir, opts.NoNetwork = h, t.TempDir(), true
	opts.DefaultVCPUs, opts.DefaultMemMiB, opts.WarmTTL = 1, 512, time.Hour
	st, err := store.Open(opts.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	s := New(testURLs(opts, "org", "0"), st, NewLifecycle(opts, st, log), log, "tok")
	t.Cleanup(func() {
		for _, r := range st.Records() {
			s.life.Stop(r, false)
		}
	})
	return s, base
}

// vmSandbox is a stored sandbox with its own copy of the base disk.
func vmSandbox(t *testing.T, s *Server, base, name string, p *store.LifecyclePolicy) store.Record {
	t.Helper()
	sp := &store.Sprite{Record: store.Record{ID: store.NewID(), CreatedAt: time.Now(), Lifecycle: p}, SpriteMeta: store.SpriteMeta{Name: name}}
	sp.Hostname = name
	if err := s.store.Create(sp); err != nil {
		t.Fatal(err)
	}
	disk := filepath.Join(s.store.Dir(sp.ID), vmm.DiskFile)
	if out, err := exec.Command("cp", "--reflink=auto", "--sparse=always", base, disk).CombinedOutput(); err != nil {
		t.Fatalf("clone disk: %v: %s", err, out)
	}
	return sp.Record
}

// wake boots a sandbox and returns its release.
func wake(t *testing.T, s *Server, r store.Record) func() {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	_, release, err := s.life.Acquire(ctx, r)
	if err != nil {
		t.Fatalf("wake %s: %v", r.Hostname, err)
	}
	return release
}

// eventually waits for r to reach state.
func eventually(t *testing.T, s *Server, r store.Record, state string, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for s.life.Status(r) != state {
		if time.Now().After(deadline) {
			t.Fatalf("%s is %s after %v, want %s", r.Hostname, s.life.Status(r), within, state)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// eventFor is the first event of that type about r so far.
func eventFor(es []Event, r store.Record, typ string) (Event, bool) {
	for _, e := range es {
		if e.SpriteID == r.ID && e.Type == typ {
			return e, true
		}
	}
	return Event{}, false
}

// Three sandboxes idle side by side under one daemon timeout: the one without
// a policy suspends warm, as every sprite does; the one whose policy says stop
// goes cold; the one whose policy says none stays up. That one is then
// suspended on demand and resumed warm by Acquire, and a policy change reaches
// its watcher while it runs.
func TestIdleRulesOnRunningVMs(t *testing.T) {
	s, base := vmServer(t, Options{IdleTimeout: time.Second})
	plain := vmSandbox(t, s, base, "plain", nil)
	stops := vmSandbox(t, s, base, "stops", &store.LifecyclePolicy{IdleAction: store.IdleStop})
	stays := vmSandbox(t, s, base, "stays", &store.LifecyclePolicy{IdleAction: store.IdleNone})
	sub, _, _ := s.life.events.subscribe(func(Event) bool { return true }, 0, false)
	for _, r := range []store.Record{plain, stops, stays} {
		wake(t, s, r)()
	}
	eventually(t, s, plain, "warm", 20*time.Second)
	eventually(t, s, stops, "cold", 20*time.Second)
	time.Sleep(2 * time.Second) // well past the timeout
	if st := s.life.Status(stays); st != "running" {
		t.Fatalf("a sandbox whose idle action is none is %s", st)
	}
	es := collect(sub)
	if e, ok := eventFor(es, plain, "sprite.suspended"); !ok || e.Detail["idle"] != true || e.Detail["reason"] != nil {
		t.Errorf("plain: sprite.suspended %v %v", ok, e.Detail)
	}
	if e, ok := eventFor(es, stops, "sprite.stopped"); !ok || e.Detail["reason"] != "idle" {
		t.Errorf("stops: sprite.stopped %v %v", ok, e.Detail)
	}
	if _, ok := eventFor(es, stops, "sprite.suspended"); ok {
		t.Error("stops: an idle stop also reported a suspend")
	}
	if _, err := os.Stat(filepath.Join(s.store.Dir(stops.ID), vmm.DiskFile)); err != nil {
		t.Errorf("stops: the disk went with the VM: %v", err)
	}

	// Pause and resume, on demand.
	if err := s.life.Suspend(stays); err != nil {
		t.Fatal(err)
	}
	if st := s.life.Status(stays); st != "warm" {
		t.Fatalf("after Suspend: %s", st)
	}
	release := wake(t, s, stays)
	es = collect(sub)
	if e, ok := eventFor(es, stays, "sprite.suspended"); !ok || e.Detail["idle"] != false {
		t.Errorf("stays: sprite.suspended %v %v", ok, e.Detail)
	}
	if e, ok := eventFor(es, stays, "sprite.woke"); !ok || e.Detail["mode"] != "warm" {
		t.Errorf("stays: Acquire after Suspend woke %v %v, want warm", ok, e.Detail)
	}
	release()
	// Back to the default rule while it runs: the watcher it has now suspends it.
	withPolicy(t, s, stays.ID, store.LifecyclePolicy{})
	eventually(t, s, stays, "warm", 20*time.Second)
}

// At a stop or suspend deadline a sandbox goes down whatever it is doing: an
// API request is in flight on both of these. Neither is deleted, nothing
// delete-specific is reported, and the deadline is spent.
func TestDeadlineActionsOnRunningVMs(t *testing.T) {
	s, base := vmServer(t, Options{IdleTimeout: time.Hour})
	suspends := vmSandbox(t, s, base, "suspends", nil)
	stops := vmSandbox(t, s, base, "stops", nil)
	sub := ruleEvents(s)
	for _, r := range []store.Record{suspends, stops} {
		defer wake(t, s, r)() // held awake throughout
	}
	soon := time.Now().Add(time.Second)
	if _, err := s.life.SetDeadline(suspends.ID, &soon, store.DeadlineSuspend); err != nil {
		t.Fatal(err)
	}
	if _, err := s.life.SetDeadline(stops.ID, &soon, store.DeadlineStop); err != nil {
		t.Fatal(err)
	}
	s.leases.sweep()
	if s.life.Status(suspends) != "running" || s.life.Status(stops) != "running" {
		t.Fatal("a deadline was acted on before it passed")
	}
	time.Sleep(time.Until(soon) + 50*time.Millisecond)
	s.leases.sweep()
	if st := s.life.Status(suspends); st != "warm" {
		t.Errorf("suspend deadline: %s", st)
	}
	if st := s.life.Status(stops); st != "cold" {
		t.Errorf("stop deadline: %s", st)
	}
	es := collect(sub)
	if e, ok := eventFor(es, suspends, "sprite.suspended"); !ok || e.Detail["reason"] != "deadline" || e.Detail["idle"] != false {
		t.Errorf("suspends: sprite.suspended %v %v", ok, e.Detail)
	}
	if e, ok := eventFor(es, stops, "sprite.stopped"); !ok || e.Detail["reason"] != "deadline" {
		t.Errorf("stops: sprite.stopped %v %v", ok, e.Detail)
	}
	for _, e := range es {
		if e.Type == "sprite.expiring" || e.Type == "sprite.expired" || e.Type == "sprite.deleted" {
			t.Errorf("a lease event for a non-delete deadline: %s", e.Type)
		}
	}
	for _, r := range []store.Record{suspends, stops} {
		if cur := record(t, s, r.ID); cur.ExpiresAt != nil {
			t.Errorf("%s: deadline not spent", r.Hostname)
		}
	}
}
