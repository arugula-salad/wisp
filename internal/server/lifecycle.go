// Package server is the wispd control plane: the Sprites REST/WebSocket API,
// and the lifecycle engine that wakes sprites on demand and suspends idle ones.
package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/arugula-salad/wisp/internal/store"
	"github.com/arugula-salad/wisp/internal/vmm"
)

const (
	agentPort  = 1024
	bridgeName = "msbr0"
	tapPrefix  = "mstap"
)

type Options struct {
	DataDir      string
	Host         vmm.Host
	BaseImage    string
	IdleTimeout  time.Duration // no activity for this long => suspend (warm)
	WarmTTL      time.Duration // suspended this long => drop memory state (cold)
	LeaseWarning time.Duration // lead time on sprite.expiring (leases.go); 0 = defaultLeaseWarning
	// URLReadyWait is how long a sprite URL waits for the app inside to accept a
	// connection before answering 503 (urlproxy.go); 0 fails on the first refusal.
	URLReadyWait  time.Duration
	DefaultVCPUs  int
	DefaultMemMiB int
	DNS           string
	// NoControl answers 404 on /control, which makes SDKs fall back to one
	// WebSocket per operation.
	NoControl bool
	// ControlForGoSDK offers /control to the official Go SDK too. See offersControl.
	ControlForGoSDK bool
	// NoNetwork boots every sprite without a NIC and leaves the shared tap pool
	// alone, so several wispd instances (dev, tests) can coexist on one host.
	NoNetwork bool
	// Automatic checkpoints (checkpoints.go): the background interval (0 = only
	// before restores) and how many to keep per sprite (0 = none at all).
	AutoCheckpointInterval time.Duration
	AutoCheckpointKeep     int
	// GuestCheckpointLimit caps the manual checkpoints a sprite can hold when the
	// request to create one comes from inside it (0 = no limit).
	GuestCheckpointLimit int
	// NetdSocket is where wisp-netd listens; empty means its default.
	NetdSocket string
	// Backup is the object-storage backup tier (internal/backup). An empty Bucket
	// disables it entirely and nothing in the lifecycle changes.
	Backup BackupOptions
	// Operator ceilings (limits.go, admission.go); 0 means no limit.
	MaxSprites int // sprites that may exist
	MaxRunning int // VMs that may run at once
	// MaxRunningMemoryMiB is the aggregate guest RAM running and starting VMs may
	// reserve, and MaxConcurrentBoots how many cold boots may be in flight.
	MaxRunningMemoryMiB int
	MaxConcurrentBoots  int
	// The disk guard (diskguard.go): bytes that creates, checkpoints and restores
	// must leave free, and the share of the volume below which the log warns.
	DiskReserve     int64
	DiskWarnPercent int
	// Listen is the API address, reported by the status views.
	Listen string
	// APIHosts are names a reverse proxy in front of the API listener serves it
	// under, for the public. They are the bearer API alone: never a sprite URL,
	// even under a URL domain (wisp.widgets.wtf with --url-domain widgets.wtf),
	// and never the dashboard, which stays on the names the proxy does not serve.
	APIHosts []string
	// Webhooks receive every event (webhooks.go).
	Webhooks WebhookOptions
}

// BackupOptions configures the object-storage backup tier.
type BackupOptions struct {
	Endpoint        string
	Bucket          string
	Region          string
	CredentialsFile string
	KeyFile         string
	Parallel        int
	RateLimit       int64
	// Interval re-backs-up a sprite whose disk has changed since its last
	// successful upload, and is the retry path for one that failed (0 = only on
	// suspend).
	Interval time.Duration
	// Retention and Keep are defaults for `wispd backups prune`.
	Retention time.Duration
	Keep      int
}

// runtime is the in-memory lifecycle state for one sprite.
type runtime struct {
	// mu serializes lifecycle transitions (boot, restore, suspend, checkpoint, delete).
	mu  sync.Mutex
	m   *vmm.Machine // nil unless running
	tap string
	// gen moves whenever a VM is about to open the disk; see diskGen.
	gen atomic.Uint64
	// guest is the running VM's channel to us (guestapi.go); set and cleared with m.
	guest *guestChan
	// grantMiB is the guest RAM memory autoscale grants (autoscale.go); 0 when
	// not autoscaling. It survives a suspend, as the memory state does.
	grantMiB int

	useMu    sync.Mutex
	inflight int
	lastUse  time.Time
}

func (rt *runtime) begin() {
	rt.useMu.Lock()
	rt.inflight++
	rt.useMu.Unlock()
}

func (rt *runtime) end() {
	rt.useMu.Lock()
	rt.inflight--
	rt.lastUse = time.Now()
	rt.useMu.Unlock()
}

func (rt *runtime) idleFor() (time.Duration, bool) {
	rt.useMu.Lock()
	defer rt.useMu.Unlock()
	if rt.inflight > 0 {
		return 0, false
	}
	return time.Since(rt.lastUse), true
}

type Lifecycle struct {
	opts  Options
	store *store.Store
	log   *slog.Logger

	mu       sync.Mutex
	runtimes map[string]*runtime // by sprite ID
	freeTaps []string
	taps     int    // size of the pool, free or not
	running  int    // VMs started or starting, counted against MaxRunning
	gateway  net.IP // the bridge's address; sprites live in its /16. nil = no networking

	// guestAPI builds the handler served on a VM's guest channel. Set by the Server.
	guestAPI func(store.Sprite, *guestChan) http.Handler
	egress   *egress
	disk     *diskGuard
	// admit is the host memory budget and the concurrent-boot cap (admission.go).
	admit *admission
	// backups is the backup tier, nil when no bucket is configured. A nil manager's
	// methods are no-ops, so the lifecycle needs no conditionals.
	backups *backupManager
	// leases reaps sprites whose workspace lease ran out (leases.go). The Server
	// installs it, since deleting a sprite is the API's path; nil until then.
	leases *leases
	// events is where everything below reports what it did (events.go).
	events *eventBus
	// denials rate-limits policy.denied events for the network policy.
	denials *rateLimiter

	// quit is closed by Shutdown to stop the loops started with every; loops
	// is how Shutdown waits for the pass in flight before it suspends anything.
	quit     chan struct{}
	quitting bool // guarded by mu
	loops    sync.WaitGroup
}

func NewLifecycle(opts Options, st *store.Store, log *slog.Logger) *Lifecycle {
	l := &Lifecycle{opts: opts, store: st, log: log, runtimes: map[string]*runtime{}, disk: newDiskGuard(opts, log), events: newEventBus(),
		admit: newAdmission(opts, log), denials: newRateLimiter(guestEventBurst, guestEventRate), quit: make(chan struct{})}
	l.disk.events = l.events
	if opts.NoNetwork {
		log.Info("guest networking disabled by --net=false")
	} else if gw, err := bridgeAddr(); err != nil {
		log.Warn("guest networking disabled", "reason", err)
	} else {
		l.gateway = gw
		entries, _ := os.ReadDir("/sys/class/net")
		for _, e := range entries {
			if strings.HasPrefix(e.Name(), tapPrefix) {
				l.freeTaps = append(l.freeTaps, e.Name())
			}
		}
	}
	l.taps = len(l.freeTaps)
	if len(l.freeTaps) == 0 && !opts.NoNetwork {
		log.Warn("no tap devices found: sprites will boot without networking (run scripts/setup-host.sh once)")
	} else if !opts.NoNetwork {
		log.Info("guest networking enabled", "taps", len(l.freeTaps), "bridge", bridgeName, "gateway", l.gateway)
	}
	for _, sp := range st.List("") {
		vmm.ReapOrphan(st.Dir(sp.ID))
	}
	// Only now: a cgroup that still holds a live orphan cannot be removed, so
	// sweeping before the reaping above would leave every stale leaf behind.
	opts.Host.Confine.SweepStale()
	l.egress = newEgress(opts, st, log, l.gateway, l.networkDenied)
	l.every(30*time.Second, l.janitor)
	return l
}

// every runs pass each period until Shutdown. A loop started once Shutdown
// has begun never runs.
func (l *Lifecycle) every(period time.Duration, pass func()) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.quitting {
		return
	}
	l.loops.Add(1)
	go func() {
		defer l.loops.Done()
		t := time.NewTicker(period)
		defer t.Stop()
		for {
			select {
			case <-l.quit:
				return
			case <-t.C:
				pass()
			}
		}
	}()
}

// bridgeAddr returns the sprite bridge's IPv4 address. setup-host.sh owns the
// choice of network; reading it back keeps the two from drifting apart.
func bridgeAddr() (net.IP, error) {
	ifc, err := net.InterfaceByName(bridgeName)
	if err != nil {
		return nil, fmt.Errorf("no %s bridge (run scripts/setup-host.sh once)", bridgeName)
	}
	addrs, _ := ifc.Addrs()
	for _, a := range addrs {
		if n, ok := a.(*net.IPNet); ok && n.IP.To4() != nil {
			if ones, _ := n.Mask.Size(); ones != 16 {
				return nil, fmt.Errorf("%s has %s; expected a /16", bridgeName, n)
			}
			return n.IP.To4(), nil
		}
	}
	return nil, fmt.Errorf("%s has no IPv4 address", bridgeName)
}

// spriteIP is the sprite's address within the bridge's /16.
func (l *Lifecycle) spriteIP(sp store.Sprite) net.IP {
	if l.gateway == nil || sp.NetIndex == 0 {
		return nil
	}
	return net.IPv4(l.gateway[0], l.gateway[1], byte(sp.NetIndex>>8), byte(sp.NetIndex))
}

func (l *Lifecycle) rt(id string) *runtime {
	l.mu.Lock()
	defer l.mu.Unlock()
	rt, ok := l.runtimes[id]
	if !ok {
		rt = &runtime{lastUse: time.Now()}
		l.runtimes[id] = rt
	}
	return rt
}

func (l *Lifecycle) takeTap() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.freeTaps) == 0 {
		return ""
	}
	t := l.freeTaps[0]
	l.freeTaps = l.freeTaps[1:]
	return t
}

func (l *Lifecycle) returnTap(t string) {
	if t == "" {
		return
	}
	l.mu.Lock()
	l.freeTaps = append(l.freeTaps, t)
	l.mu.Unlock()
}

// Status is the API-visible state: running, warm (suspended in memory snapshot) or cold.
func (l *Lifecycle) Status(sp store.Sprite) string {
	rt := l.rt(sp.ID)
	// Don't block on rt.mu: a transition may be in flight. Reading m racily under useMu is not
	// possible either, so peek with TryLock and call an in-progress transition "running".
	if !rt.mu.TryLock() {
		return "running"
	}
	defer rt.mu.Unlock()
	switch {
	case rt.m != nil:
		return "running"
	case vmm.HasSnapshot(l.store.Dir(sp.ID)):
		return "warm"
	}
	return "cold"
}

func (l *Lifecycle) vmConfig(sp store.Sprite, tap string) vmm.Config {
	cfg := vmm.Config{
		Dir: l.store.Dir(sp.ID), Hostname: sp.Name, AgentPort: agentPort,
		VCPUs: l.opts.DefaultVCPUs, MemMiB: l.opts.DefaultMemMiB,
	}
	if sp.Config.CPUs > 0 {
		cfg.VCPUs = sp.Config.CPUs
	}
	if sp.Config.RamMB > 0 {
		cfg.MemMiB = sp.Config.RamMB
	}
	if ip := l.spriteIP(sp).To4(); tap != "" && ip != nil {
		cfg.Tap, cfg.IPCIDR, cfg.Gateway, cfg.DNS = tap, ip.String()+"/16", l.gateway.String(), l.opts.DNS
		cfg.MAC = fmt.Sprintf("06:00:%02x:%02x:%02x:%02x", ip[0], ip[1], ip[2], ip[3])
	}
	applyPolicy(&cfg, sp)
	return cfg
}

// Acquire makes sure the sprite is running and pins it awake until release is called.
func (l *Lifecycle) Acquire(ctx context.Context, sp store.Sprite) (m *vmm.Machine, release func(), err error) {
	rt := l.rt(sp.ID)
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if rt.m != nil {
		select {
		case <-rt.m.Exited():
			l.cleanupLocked(rt)
		default:
		}
	}
	if rt.m == nil {
		from, start := "cold", time.Now()
		if vmm.HasSnapshot(l.store.Dir(sp.ID)) {
			from = "warm"
		}
		if err := l.startLocked(ctx, sp, rt); err != nil {
			var lim *LimitError
			if errors.As(err, &lim) {
				l.emit(sp, "limit.refused", map[string]any{"limit": lim.Which, "max": lim.Limit, "current": lim.Current})
			} else {
				l.emit(sp, "sprite.wake_failed", map[string]any{"error": clipErr(err)})
			}
			return nil, nil, err
		}
		noteWake(ctx, time.Since(start), from)
	}
	rt.begin()
	var once sync.Once
	return rt.m, func() { once.Do(rt.end) }, nil
}

func (l *Lifecycle) cleanupLocked(rt *runtime) {
	rt.m = nil
	rt.guest.close()
	rt.guest = nil
	l.returnTap(rt.tap)
	rt.tap = ""
	l.releaseStart(rt)
}

// startLocked boots or resumes the sprite, within the host's admission limits:
// the MaxRunning count (limits.go), the running-memory budget and the
// concurrent-boot cap (admission.go).
func (l *Lifecycle) startLocked(ctx context.Context, sp store.Sprite, rt *runtime) error {
	booted, err := l.admitStart(sp, rt)
	if err != nil {
		return err
	}
	defer booted()
	if err := l.bootLocked(ctx, sp, rt); err != nil {
		l.releaseStart(rt)
		return err
	}
	return nil
}

func (l *Lifecycle) bootLocked(ctx context.Context, sp store.Sprite, rt *runtime) error {
	start := time.Now()
	rt.gen.Add(1)
	dir := l.store.Dir(sp.ID)
	tap, gateErr := l.tapFor(sp)
	if gateErr != nil {
		return gateErr
	}
	cfg := l.vmConfig(sp, tap)
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()

	// The guest may call the host as soon as it runs, so its channel comes first.
	guest, err := l.openGuestChan(sp)
	if err != nil {
		l.returnTap(tap)
		return fmt.Errorf("guest channel: %w", err)
	}
	defer func() {
		if rt.guest != guest {
			guest.close() // the VM did not come up
		}
	}()

	var m *vmm.Machine
	mode, discarded := "cold", ""
	if vmm.HasSnapshot(dir) && sp.BootIP != cfg.IPCIDR {
		discarded = "network changed"
		// The guest configured its address at boot; a snapshot from before the
		// network changed (or appeared) would resume with the wrong one.
		l.log.Info("network changed since boot; discarding warm state", "sprite", sp.Name, "was", sp.BootIP, "now", cfg.IPCIDR)
		vmm.DiscardSnapshot(dir)
	}
	if vmm.HasSnapshot(dir) {
		mode = "warm"
		if m, err = vmm.Restore(ctx, l.opts.Host, cfg); err != nil {
			// A snapshot we can't load is just lost memory state; the disk is intact.
			l.log.Warn("restore failed, falling back to cold boot", "sprite", sp.Name, "err", err)
			vmm.DiscardSnapshot(dir)
			mode, m, discarded = "cold", nil, "snapshot restore failed"
		}
	}
	if m == nil {
		if m, err = vmm.Boot(ctx, l.opts.Host, cfg); err != nil {
			l.returnTap(tap)
			return err
		}
	}
	if err := l.waitAgent(ctx, m, mode == "warm"); err != nil {
		m.Kill()
		l.returnTap(tap)
		return fmt.Errorf("guest agent did not come up: %w%s", err, consoleTail(dir))
	}
	if mode == "warm" && !l.policyResumed(ctx, m, sp) {
		m.Kill()
		l.returnTap(tap)
		vmm.DiscardSnapshot(dir)
		// Close before recursing: the retry listens on the same socket path, and
		// closing this listener later would unlink the new one's socket.
		guest.close()
		return l.bootLocked(ctx, sp, rt)
	}
	if mode == "cold" {
		rt.grantMiB = 0
	}
	l.setBalloon(ctx, sp, rt, m, mode == "warm")
	rt.m, rt.tap, rt.guest = m, tap, guest
	if cur, err := l.store.Get(sp.Name); err == nil {
		l.publishNetworkPolicy(ctx, m, cur) // it may have changed while the sprite slept
	}
	rt.useMu.Lock()
	rt.lastUse = time.Now()
	rt.useMu.Unlock()
	now := time.Now()
	l.store.Update(sp.Name, func(s *store.Sprite) {
		s.LastRunningAt = &now
		if mode == "cold" {
			s.BootIP = cfg.IPCIDR
			s.Mounts = nil // a fresh VM has placeholders behind every checkpoint slot
		}
	})
	took := time.Since(start)
	l.log.Info("sprite running", "sprite", sp.Name, "wake", mode, "took", took.Round(time.Millisecond), "net", tap != "")
	woke := map[string]any{"mode": mode, "ms": took.Milliseconds()}
	if discarded != "" {
		woke["warm_discarded"] = discarded
	}
	l.emit(sp, "sprite.woke", woke)
	go l.watch(sp, rt, m)
	go l.autoscale(sp, rt, m)
	return nil
}

func consoleTail(dir string) string {
	b, err := os.ReadFile(filepath.Join(dir, "console.log"))
	if err != nil || len(b) == 0 {
		return ""
	}
	if len(b) > 1500 {
		b = b[len(b)-1500:]
	}
	return "\n--- console ---\n" + string(b)
}

// errGuestBusy is the agent declining an idle suspend: work arrived over a
// socket wispd does not pin (a control channel) after the last activity check.
var errGuestBusy = errors.New("guest became active")

// agentCall makes one HTTP request to the guest agent over a fresh vsock stream.
func agentCall(ctx context.Context, m *vmm.Machine, method, path string, body any, out any) error {
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://agent"+path, rd)
	if err != nil {
		return err
	}
	tr := &http.Transport{DisableKeepAlives: true,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) { return m.Dial(ctx) }}
	resp, err := tr.RoundTrip(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusConflict {
		return errGuestBusy
	}
	if resp.StatusCode >= 300 {
		return fmt.Errorf("agent %s %s: %s", method, path, resp.Status)
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

func (l *Lifecycle) waitAgent(ctx context.Context, m *vmm.Machine, resumed bool) error {
	for {
		cctx, cancel := context.WithTimeout(ctx, 2*time.Second)
		var err error
		if resumed {
			// Doubles as the readiness probe: steps the guest clock past the time it spent suspended.
			err = agentCall(cctx, m, http.MethodPost, "/internal/resumed", map[string]int64{"unix_nano": time.Now().UnixNano()}, nil)
		} else {
			err = agentCall(cctx, m, http.MethodGet, "/healthz", nil, nil)
		}
		cancel()
		if err == nil {
			return nil
		}
		select {
		case <-m.Exited():
			return errors.New("VM exited during startup")
		case <-ctx.Done():
			return fmt.Errorf("%w (last error: %v)", ctx.Err(), err)
		case <-time.After(5 * time.Millisecond):
		}
	}
}

// watch suspends the sprite once both the API and the guest have been idle for IdleTimeout.
func (l *Lifecycle) watch(sp store.Sprite, rt *runtime, m *vmm.Machine) {
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	held := false
	for {
		select {
		case <-m.Exited():
			rt.mu.Lock()
			if rt.m == m {
				l.cleanupLocked(rt)
				l.log.Info("sprite VM exited", "sprite", sp.Name)
				l.emit(sp, "sprite.exited", nil)
			}
			rt.mu.Unlock()
			return
		case <-tick.C:
		}
		if idle, ok := rt.idleFor(); !ok || idle < l.opts.IdleTimeout {
			continue
		}
		var act struct {
			IdleMS   int64 `json:"idle_ms"`
			Attached int   `json:"attached_sessions"`
			Tasks    int   `json:"tasks"`
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		err := agentCall(ctx, m, http.MethodGet, "/internal/activity", nil, &act)
		cancel()
		if err == nil {
			l.noteHold(sp, &held, act.Tasks)
		}
		if err != nil || act.Tasks > 0 || act.Attached > 0 || time.Duration(act.IdleMS)*time.Millisecond < l.opts.IdleTimeout {
			continue
		}

		rt.mu.Lock()
		if _, ok := rt.idleFor(); !ok || rt.m != m {
			rt.mu.Unlock()
			continue
		}
		err = l.suspendLocked(sp, rt, true)
		rt.mu.Unlock()
		if err == nil {
			return
		}
		if errors.Is(err, errGuestBusy) {
			continue
		}
		l.log.Error("suspend failed; sprite left running", "sprite", sp.Name, "err", err)
	}
}

// suspendLocked snapshots the sprite to disk. idle marks a suspend the guest may
// still veto with errGuestBusy; one the operator asked for goes ahead regardless.
func (l *Lifecycle) suspendLocked(sp store.Sprite, rt *runtime, idle bool) error {
	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	// Flush the guest page cache first so that dropping the snapshot later
	// (warm -> cold) is no worse than a clean power cut after sync.
	path := "/internal/presuspend"
	if idle {
		path += "?idle=1"
	}
	if err := agentCall(ctx, rt.m, http.MethodPost, path, nil, nil); err != nil {
		return err
	}
	// A snapshot that does not fit would fail half-written and leave the VM
	// running for good. The guest has just synced, so stopping it cold instead
	// is no worse than the warm -> cold drop every sprite gets eventually.
	// Firecracker writes the whole RAM, and the sparse copy of it can take up
	// to half as much again before the whole-RAM file is deleted.
	need := int64(rt.m.MemMiB())<<20*3/2 + snapshotSlack
	release, fits := l.makeRoom(sp, need)
	defer release()
	if !fits {
		rt.m.Kill()
		l.cleanupLocked(rt)
		l.log.Warn("no room for a memory snapshot even with every other sprite cold; sprite stopped cold instead", "sprite", sp.Name, "needed", mib(need))
		l.emit(sp, "sprite.stopped", map[string]any{"reason": "no room for a memory snapshot"})
		return nil
	}
	// Balloon the guest's free memory so that it is a hole in the snapshot, not
	// zeros on disk. Best effort: an old VM may have no balloon.
	if _, err := rt.m.Squeeze(ctx); err != nil && !errors.Is(err, vmm.ErrNoBalloon) {
		l.log.Warn("could not squeeze free memory before suspend", "sprite", sp.Name, "err", err)
	}
	if err := rt.m.Suspend(ctx); err != nil {
		l.setBalloon(ctx, sp, rt, rt.m, true) // it is running on: give the memory back
		return err
	}
	l.cleanupLocked(rt)
	now := time.Now()
	l.store.Update(sp.Name, func(s *store.Sprite) { s.LastWarmingAt = &now })
	took := time.Since(start)
	snap := vmm.SnapshotBytes(l.store.Dir(sp.ID))
	l.log.Info("sprite suspended", "sprite", sp.Name, "took", took.Round(time.Millisecond), "snapshot", mib(snap))
	l.emit(sp, "sprite.suspended", map[string]any{"ms": took.Milliseconds(), "idle": idle, "snapshot_bytes": snap})
	// The disk is quiescent exactly here: the guest has synced and the VM is
	// paused. Enqueueing is non-blocking and cannot fail, so a bucket that is
	// unreachable never turns a good suspend into a bad one.
	l.backups.Enqueue(sp, "suspend")
	return nil
}

// diskGen counts the VMs that have been started on a sprite's disk. A backup
// reading that disk in place, because the volume has no reflink support, notes it
// first and gives up when it moves: a wake never waits for an upload, so the
// upload has to notice the wake. Lock-free for the same reason.
func (l *Lifecycle) diskGen(id string) uint64 { return l.rt(id).gen.Load() }

// Stop halts a sprite. With keepWarm it is suspended so it can resume later;
// otherwise the VM is killed and memory state discarded.
func (l *Lifecycle) Stop(sp store.Sprite, keepWarm bool) error {
	rt := l.rt(sp.ID)
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if rt.m == nil {
		if !keepWarm {
			vmm.DiscardSnapshot(l.store.Dir(sp.ID))
		}
		return nil
	}
	if keepWarm {
		return l.suspendLocked(sp, rt, false)
	}
	rt.m.Kill()
	l.cleanupLocked(rt)
	vmm.DiscardSnapshot(l.store.Dir(sp.ID))
	l.emit(sp, "sprite.stopped", nil)
	return nil
}

// Cool drops a suspended sprite's memory snapshot, as the warm TTL would. It
// reports false, and does nothing, when the sprite is not warm.
func (l *Lifecycle) Cool(sp store.Sprite) bool {
	rt := l.rt(sp.ID)
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if rt.m != nil || !vmm.HasSnapshot(l.store.Dir(sp.ID)) {
		return false
	}
	vmm.DiscardSnapshot(l.store.Dir(sp.ID))
	l.emit(sp, "sprite.cold", map[string]any{"reason": "operator"})
	return true
}

// Forget drops runtime state for a deleted sprite.
func (l *Lifecycle) Forget(id string) {
	l.mu.Lock()
	delete(l.runtimes, id)
	l.mu.Unlock()
}

// Shutdown suspends every running sprite so they come back warm. The
// background loops stop first, so none is cooling, reaping or checkpointing a
// sprite while it suspends.
func (l *Lifecycle) Shutdown() {
	l.mu.Lock()
	if !l.quitting {
		l.quitting = true
		close(l.quit)
	}
	l.mu.Unlock()
	l.loops.Wait()
	var wg sync.WaitGroup
	for _, sp := range l.store.List("") {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := l.Stop(sp, true); err != nil {
				l.log.Error("suspend on shutdown failed", "sprite", sp.Name, "err", err)
				l.Stop(sp, false)
			}
		}()
	}
	wg.Wait()
}

// janitor turns long-suspended sprites cold by dropping their memory snapshot,
// and deletes the sprites whose workspace lease has run out.
func (l *Lifecycle) janitor() {
	l.disk.watch()
	l.reapLeases() // before cooling: a sprite on its way out needs no snapshot work
	for _, sp := range l.store.List("") {
		if l.warmExpired(sp) {
			l.coolIfExpired(sp)
		}
	}
}

func (l *Lifecycle) warmExpired(sp store.Sprite) bool {
	return sp.LastWarmingAt != nil && time.Since(*sp.LastWarmingAt) >= l.opts.WarmTTL
}

// coolIfExpired drops the snapshot of a sprite that has been warm longer than
// the TTL. sp is only a candidate from an earlier read: waiting for the lock
// can outlast a suspend in flight, which renews LastWarmingAt, so the decision
// is taken again on a fresh record. Deciding on the old one threw away snapshots
// seconds old, every sprite's at once when a daemon stopped.
func (l *Lifecycle) coolIfExpired(sp store.Sprite) {
	rt := l.rt(sp.ID)
	rt.mu.Lock()
	defer rt.mu.Unlock()
	cur, err := l.store.Get(sp.Name)
	if err != nil || cur.ID != sp.ID || !l.warmExpired(cur) {
		return
	}
	if rt.m == nil && vmm.HasSnapshot(l.store.Dir(sp.ID)) {
		vmm.DiscardSnapshot(l.store.Dir(sp.ID))
		l.log.Info("sprite went cold", "sprite", sp.Name)
		l.emit(cur, "sprite.cold", map[string]any{"reason": "warm ttl"})
	}
}

// clipErr keeps an error short enough for an event.
func clipErr(err error) string {
	msg := err.Error()
	if len(msg) > 300 {
		msg = msg[:300] + "..."
	}
	return msg
}
