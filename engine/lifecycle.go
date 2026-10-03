package engine

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

	"github.com/arugula-salad/wisp/internal/backup"
	"github.com/arugula-salad/wisp/internal/httpstats"
	"github.com/arugula-salad/wisp/internal/netd"
	"github.com/arugula-salad/wisp/internal/ratelimit"
	"github.com/arugula-salad/wisp/internal/store"
	"github.com/arugula-salad/wisp/internal/vmm"
)

// Locking. The Engine is the only code that touches a sprite's runtime:
// its lock (rt.mu), its VM (rt.m) and the guest agent behind it (agentCall).
// Everything else, a front end's HTTP handlers above all, goes through *Engine
// methods that take the lock themselves. The order is:
//
//  1. rt.mu, one sprite's transition lock, is outermost. It is held across
//     slow work: a boot, a suspend, a disk clone, a guest round trip, and the
//     progress callbacks those report through, which may write to a client.
//  2. At most one rt.mu is waited for at a time. Code holding one sprite's
//     lock may look at another's only with TryLock (makeRoom), and code that
//     must wait for a second lock lets go of its own first (leases.reap
//     before the delete).
//  3. Everything else is a leaf, taken under rt.mu or alone and held only
//     for bookkeeping: l.mu (the runtime table, the tap pool, the loops, the
//     delete and boot hooks), rt.useMu, the store's own lock, and the mutexes of the
//     disk guard, admission, egress, backups and leases. None is ever held while
//     waiting for rt.mu, so l.rt(id) may be called with or without a sprite
//     locked, and the store may be read and written under rt.mu. Naming a
//     record for an event or the log (describe) reads the store and then
//     l.mu, one after the other, so it may be done under any lock but those
//     two; nothing emits or logs a label holding either.
//
// Background passes that must never wait behind a transition (Status, peek,
// autoscale, makeRoom) use TryLock and treat a held lock as "busy".

const agentPort = 1024

// Options configures an Engine. A front end's own settings live beside them
// (server.Options embeds these).
type Options struct {
	DataDir       string
	Host          vmm.Host
	BaseImage     string
	IdleTimeout   time.Duration // no activity for this long => suspend (warm)
	WarmTTL       time.Duration // suspended this long => drop memory state (cold)
	LeaseWarning  time.Duration // lead time on sprite.expiring (leases.go); 0 = defaultLeaseWarning
	DefaultVCPUs  int
	DefaultMemMiB int
	DNS           string
	// NoNetwork boots every sprite without a NIC and leaves the shared tap pool
	// alone, so several wispd instances (dev, tests) can coexist on one host.
	NoNetwork bool
	// NetPool is the host network pool (bridge, taps, nft table, wisp-netd) this
	// daemon owns; see netd.Pool. 0 is the one setup-host.sh makes by default.
	NetPool int
	// Automatic checkpoints (lifecycle_checkpoints.go): the background interval
	// (0 = only before restores) and how many to keep per sprite (0 = none at all).
	AutoCheckpointInterval time.Duration
	AutoCheckpointKeep     int
	// NetdSocket is where wisp-netd listens; empty means NetPool's default.
	NetdSocket string
	// Backup is the object-storage backup tier (internal/backup). An empty Bucket
	// disables it entirely and nothing in the lifecycle changes.
	Backup BackupOptions
	// Operator ceilings (admission.go); 0 means no limit.
	MaxRunning int // VMs that may run at once
	// MaxRunningMemoryMiB is the aggregate guest RAM running and starting VMs may
	// reserve, and MaxConcurrentBoots how many cold boots may be in flight.
	MaxRunningMemoryMiB int
	MaxConcurrentBoots  int
	// The disk guard (diskguard.go): bytes that creates, checkpoints and restores
	// must leave free, and the share of the volume below which the log warns.
	DiskReserve     int64
	DiskWarnPercent int
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
	// guest is the running VM's channel to us (guestchan.go); set and cleared with m.
	guest *GuestChan
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

// Engine runs sandboxes: their VMs, disks, checkpoints, network policy,
// backups and deadlines. See the lock order above.
type Engine struct {
	opts  Options
	store *store.Store
	log   *slog.Logger

	mu       sync.Mutex
	runtimes map[string]*runtime // by sprite ID
	freeTaps []string
	taps     int    // size of the pool, free or not
	running  int    // VMs started or starting, counted against MaxRunning
	gateway  net.IP // the bridge's address; sprites live in its /16. nil = no networking

	// guestAPI builds the handler served on a VM's guest channel (SetGuestAPI).
	guestAPI func(store.Record, *GuestChan) http.Handler
	egress   *egress
	disk     *diskGuard
	// storage is the sprite volume (storage.go). nil (an Engine built by
	// hand in tests) means no reflinks.
	storage *storage
	// images is the cache of disks built from container images (images.go).
	images *ImageCache
	// admit is the host memory budget and the concurrent-boot cap (admission.go).
	admit *admission
	// backups is the backup tier, nil when no bucket is configured. A nil manager's
	// methods are no-ops, so the lifecycle needs no conditionals.
	backups *backupManager
	// leases reaps sprites whose workspace lease ran out (leases.go). nil only
	// in an Engine built by hand in tests: its methods are then no-ops, except
	// set, which has no store to write to and refuses.
	leases *leases
	// reapStarted is set by StartReaping: until then the janitor reaps nothing.
	// Guarded by mu.
	reapStarted bool
	// onDelete is what the front end does when a sprite is deleted (OnDelete);
	// guarded by mu.
	onDelete []func(store.Sprite)
	// onBoot prepares a guest before anything else gets its VM (OnBoot);
	// guarded by mu.
	onBoot []BootHook
	// describer names records in events and logs (SetDescriber); unstored are
	// the records Create and Delete hold while the store does not (guarded by
	// mu), so that what they report about is described too.
	describer atomic.Pointer[Describer]
	// backupFilter says which records the backup tier skips (SetBackupFilter).
	backupFilter atomic.Pointer[func(store.Sprite) bool]
	unstored     map[string]store.Sprite
	// events is where everything below reports what it did (events.go).
	events *Bus
	// denials rate-limits policy.denied events for the network policy.
	denials *ratelimit.Limiter

	// quit is closed by Shutdown to stop the loops started with every; loops
	// is how Shutdown waits for the pass in flight before it suspends anything.
	quit     chan struct{}
	quitting bool // guarded by mu
	loops    sync.WaitGroup
}

// New starts an engine over the records in st: it reaps VMs a previous
// daemon left behind, finds the tap pool, and starts the background loops
// (the janitor, automatic checkpoints, backups). Shutdown stops them.
func New(opts Options, st *store.Store, log *slog.Logger) *Engine {
	l := &Engine{opts: opts, store: st, log: log, runtimes: map[string]*runtime{}, unstored: map[string]store.Sprite{}, disk: newDiskGuard(opts, log), events: newBus(),
		admit: newAdmission(opts, log), denials: ratelimit.New(denialBurst, denialRate), quit: make(chan struct{})}
	l.disk.events, l.disk.event = l.events, l.event
	l.storage = newStorage(filepath.Join(opts.DataDir, "vm"), opts.BaseImage)
	if l.storage.reflink {
		log.Info("sprite volume supports reflinks: new sprites and checkpoints are instant copy-on-write clones")
	} else {
		log.Info("sprite volume has no reflink support: new sprites and checkpoints are full sparse copies (see scripts/setup-storage.sh)")
	}
	pool := netd.Pool(opts.NetPool)
	if opts.NoNetwork {
		log.Info("guest networking disabled by --net=false")
	} else if gw, err := bridgeAddr(pool); err != nil {
		log.Warn("guest networking disabled", "reason", err)
	} else {
		l.gateway = gw
		entries, _ := os.ReadDir("/sys/class/net")
		for _, e := range entries {
			if strings.HasPrefix(e.Name(), pool.TapPrefix()) {
				l.freeTaps = append(l.freeTaps, e.Name())
			}
		}
	}
	l.taps = len(l.freeTaps)
	if len(l.freeTaps) == 0 && !opts.NoNetwork {
		log.Warn("no tap devices found: sprites will boot without networking (run " + setupCommand(pool) + " once)")
	} else if !opts.NoNetwork {
		log.Info("guest networking enabled", "taps", len(l.freeTaps), "bridge", pool.Bridge(), "gateway", l.gateway)
	}
	for _, sp := range st.Records() {
		vmm.ReapOrphan(st.Dir(sp.ID))
	}
	// Only now: a cgroup that still holds a live orphan cannot be removed, so
	// sweeping before the reaping above would leave every stale leaf behind.
	opts.Host.Confine.SweepStale()
	l.egress = newEgress(opts, st, log, l.gateway, l.label, l.networkDenied)
	if b := opts.Backup; b.Bucket != "" {
		l.backups = newBackupManager(backup.Config{Endpoint: b.Endpoint, Bucket: b.Bucket, Region: b.Region,
			CredentialsFile: b.CredentialsFile, KeyFile: b.KeyFile, Parallel: b.Parallel, RateLimit: b.RateLimit, Log: log},
			b, st, log, l, l.storage)
	}
	l.leases = newLeases(st, log, l, opts.LeaseWarning)
	l.images = newImageCache(filepath.Join(opts.DataDir, "vm"), opts.BaseImage, l.disk.admitHost, log)
	l.every(30*time.Second, l.janitor)
	if opts.AutoCheckpointInterval > 0 && opts.AutoCheckpointKeep > 0 {
		l.every(min(max(opts.AutoCheckpointInterval/10, time.Second), time.Minute), l.autoCheckpoints)
	}
	return l
}

// Images is the cache of disks built from container images, which Create
// clones a disk from (CreateSpec.ImageDisk).
func (l *Engine) Images() *ImageCache { return l.images }

// every runs pass each period until Shutdown. A loop started once Shutdown
// has begun never runs.
func (l *Engine) every(period time.Duration, pass func()) {
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
func bridgeAddr(pool netd.Pool) (net.IP, error) {
	bridgeName := pool.Bridge()
	ifc, err := net.InterfaceByName(bridgeName)
	if err != nil {
		return nil, fmt.Errorf("no %s bridge (run %s once)", bridgeName, setupCommand(pool))
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

// setupCommand is how to create pool's bridge and taps.
func setupCommand(pool netd.Pool) string {
	if pool == 0 {
		return "scripts/setup-host.sh"
	}
	return fmt.Sprintf("WISP_POOL=%d scripts/setup-host.sh", pool)
}

// Networking says whether sprites get a NIC at all: false with --net=false or
// without the bridge.
func (l *Engine) Networking() bool { return l.gateway != nil }

// SandboxIP is the sprite's address within the bridge's /16, nil without one.
func (l *Engine) SandboxIP(sp store.Record) net.IP {
	if l.gateway == nil || sp.NetIndex == 0 {
		return nil
	}
	return net.IPv4(l.gateway[0], l.gateway[1], byte(sp.NetIndex>>8), byte(sp.NetIndex))
}

func (l *Engine) rt(id string) *runtime {
	l.mu.Lock()
	defer l.mu.Unlock()
	rt, ok := l.runtimes[id]
	if !ok {
		rt = &runtime{lastUse: time.Now()}
		l.runtimes[id] = rt
	}
	return rt
}

// VMView is what can be read about a sprite's VM without waiting for a
// transition in flight (Peek). Callers outside the engine read its fields and
// ask the VM things through its methods.
type VMView struct {
	Busy     bool         // a transition is in flight; Tap and Pid are unknown
	Tap      string       // "" when not running or without networking
	Pid      int          // the VMM's; 0 unless running
	Inflight int          // API requests pinning the sprite awake
	m        *vmm.Machine // nil unless running; for the methods below only
}

// Running says whether the VM was up.
func (v VMView) Running() bool { return v.m != nil }

// TaskHolds asks the guest how many tasks are holding it awake. It is the same
// question the idle watcher asks, so it does not count as activity.
func (v VMView) TaskHolds(ctx context.Context) (int, bool) {
	var act struct {
		Tasks int `json:"tasks"`
	}
	if v.m == nil || agentCall(ctx, v.m, http.MethodGet, "/internal/activity", nil, &act) != nil {
		return 0, false
	}
	return act.Tasks, true
}

// Peek reads a sprite's runtime state without waiting for a transition in flight.
func (l *Engine) Peek(id string) VMView {
	rt := l.rt(id)
	rt.useMu.Lock()
	v := VMView{Inflight: rt.inflight}
	rt.useMu.Unlock()
	if !rt.mu.TryLock() {
		v.Busy = true
		return v
	}
	defer rt.mu.Unlock()
	v.Tap, v.m = rt.tap, rt.m
	if rt.m != nil {
		v.Pid = rt.m.Pid()
	}
	return v
}

// TapUsage is the size of the tap pool and how much of it is taken.
func (l *Engine) TapUsage() (total, used int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.taps, l.taps - len(l.freeTaps)
}

func (l *Engine) takeTap() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.freeTaps) == 0 {
		return ""
	}
	t := l.freeTaps[0]
	l.freeTaps = l.freeTaps[1:]
	return t
}

func (l *Engine) returnTap(t string) {
	if t == "" {
		return
	}
	l.mu.Lock()
	l.freeTaps = append(l.freeTaps, t)
	l.mu.Unlock()
}

// Status is the API-visible state: running, warm (suspended in memory snapshot) or cold.
func (l *Engine) Status(sp store.Record) string {
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

func (l *Engine) vmConfig(sp store.Record, tap string) vmm.Config {
	cfg := vmm.Config{
		Dir: l.store.Dir(sp.ID), Hostname: sp.Hostname, AgentPort: agentPort,
		VCPUs: l.opts.DefaultVCPUs, MemMiB: l.opts.DefaultMemMiB,
	}
	if sp.Config.CPUs > 0 {
		cfg.VCPUs = sp.Config.CPUs
	}
	if sp.Config.RamMB > 0 {
		cfg.MemMiB = sp.Config.RamMB
	}
	if ip := l.SandboxIP(sp).To4(); tap != "" && ip != nil {
		cfg.Tap, cfg.IPCIDR, cfg.Gateway, cfg.DNS = tap, ip.String()+"/16", l.gateway.String(), l.opts.DNS
		cfg.MAC = fmt.Sprintf("06:00:%02x:%02x:%02x:%02x", ip[0], ip[1], ip[2], ip[3])
	}
	applyPolicy(&cfg, sp)
	return cfg
}

// Acquire makes sure the sprite is running and pins it awake until release is called.
func (l *Engine) Acquire(ctx context.Context, sp store.Record) (m *vmm.Machine, release func(), err error) {
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
				l.Emit(sp, "limit.refused", map[string]any{"limit": lim.Which, "max": lim.Limit, "current": lim.Current})
			} else {
				l.Emit(sp, "sprite.wake_failed", map[string]any{"error": clipErr(err)})
			}
			return nil, nil, err
		}
		httpstats.NoteWake(ctx, time.Since(start), from)
	}
	rt.begin()
	var once sync.Once
	return rt.m, func() { once.Do(rt.end) }, nil
}

// BeginUse counts as activity on a sprite that may be running, without waking
// one that is not: the idle watcher does not suspend it until end is called.
// (Acquire is the same for a request that needs the VM up.)
func (l *Engine) BeginUse(id string) (end func()) {
	rt := l.rt(id)
	rt.begin()
	return rt.end
}

func (l *Engine) cleanupLocked(rt *runtime) {
	rt.m = nil
	rt.guest.close()
	rt.guest = nil
	l.returnTap(rt.tap)
	rt.tap = ""
	l.releaseStart(rt)
}

// startLocked boots or resumes the sprite, within the host's admission limits:
// the MaxRunning count (reserveRun), the running-memory budget and the
// concurrent-boot cap (admission.go).
func (l *Engine) startLocked(ctx context.Context, sp store.Record, rt *runtime) error {
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

func (l *Engine) bootLocked(ctx context.Context, sp store.Record, rt *runtime) error {
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
		l.log.Info("network changed since boot; discarding warm state", "sprite", l.label(sp), "was", sp.BootIP, "now", cfg.IPCIDR)
		vmm.DiscardSnapshot(dir)
	}
	if vmm.HasSnapshot(dir) {
		mode = "warm"
		if m, err = vmm.Restore(ctx, l.opts.Host, cfg); err != nil {
			// A snapshot we can't load is just lost memory state; the disk is intact.
			l.log.Warn("restore failed, falling back to cold boot", "sprite", l.label(sp), "err", err)
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
	if err := l.runBootHooks(ctx, Boot{Record: sp, Warm: mode == "warm", Machine: m}); err != nil {
		m.Kill()
		l.returnTap(tap)
		return fmt.Errorf("preparing the guest: %w", err)
	}
	l.setBalloon(ctx, sp, rt, m, mode == "warm")
	rt.m, rt.tap, rt.guest = m, tap, guest
	if cur, err := l.store.GetRecord(sp.ID); err == nil {
		l.publishNetworkPolicy(ctx, m, cur) // it may have changed while the sprite slept
	}
	rt.useMu.Lock()
	rt.lastUse = time.Now()
	rt.useMu.Unlock()
	now := time.Now()
	l.store.UpdateRecord(sp.ID, func(s *store.Record) {
		s.LastRunningAt = &now
		if mode == "cold" {
			s.BootIP = cfg.IPCIDR
			s.Mounts = nil // a fresh VM has placeholders behind every checkpoint slot
		}
	})
	took := time.Since(start)
	l.log.Info("sprite running", "sprite", l.label(sp), "wake", mode, "took", took.Round(time.Millisecond), "net", tap != "")
	woke := map[string]any{"mode": mode, "ms": took.Milliseconds()}
	if discarded != "" {
		woke["warm_discarded"] = discarded
	}
	l.Emit(sp, "sprite.woke", woke)
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

// AgentDial reaches m's guest agent over vsock.
func AgentDial(m *vmm.Machine) func(context.Context, string, string) (net.Conn, error) {
	return func(ctx context.Context, _, _ string) (net.Conn, error) { return m.Dial(ctx) }
}

// AgentTransport carries HTTP to m's guest agent, a fresh vsock stream per
// request: there is nothing to keep alive across a suspend.
func AgentTransport(m *vmm.Machine) *http.Transport {
	return &http.Transport{DisableKeepAlives: true, DialContext: AgentDial(m)}
}

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
	resp, err := AgentTransport(m).RoundTrip(req)
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

func (l *Engine) waitAgent(ctx context.Context, m *vmm.Machine, resumed bool) error {
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

// watch applies the idle rule: it suspends the sprite (or stops it, or leaves
// it be, as its lifecycle policy says) once both the API and the guest have
// been idle for the timeout. The rule is read from the store on every tick,
// so a policy change reaches a running sprite within a second.
func (l *Engine) watch(sp store.Record, rt *runtime, m *vmm.Machine) {
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	held := false
	for {
		select {
		case <-m.Exited():
			rt.mu.Lock()
			if rt.m == m {
				l.cleanupLocked(rt)
				l.log.Info("sprite VM exited", "sprite", l.label(sp))
				l.Emit(sp, "sprite.exited", nil)
			}
			rt.mu.Unlock()
			return
		case <-tick.C:
		}
		timeout, action := l.idleRule(sp)
		if action == store.IdleNone {
			continue
		}
		if idle, ok := rt.idleFor(); !ok || idle < timeout {
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
		if err != nil || act.Tasks > 0 || act.Attached > 0 || time.Duration(act.IdleMS)*time.Millisecond < timeout {
			continue
		}

		rt.mu.Lock()
		if _, ok := rt.idleFor(); !ok || rt.m != m {
			rt.mu.Unlock()
			continue
		}
		// Again under the lock: SetPolicy takes it too, so this is the rule now.
		switch _, action = l.idleRule(sp); action {
		case store.IdleNone:
			err = errGuestBusy // the policy changed while we looked: go on watching
		case store.IdleStop:
			err = l.stopLocked(sp, rt, true, "idle")
		default:
			err = l.suspendLocked(sp, rt, suspendIdle)
		}
		rt.mu.Unlock()
		if err == nil {
			return
		}
		if errors.Is(err, errGuestBusy) {
			continue
		}
		l.log.Error("suspend failed; sprite left running", "sprite", l.label(sp), "err", err)
	}
}

// noteHold logs when a sprite starts and stops being held awake by tasks, so
// "why is this VM still running" has an answer in the log.
func (l *Engine) noteHold(sp store.Record, held *bool, tasks int) {
	if now := tasks > 0; now != *held {
		*held = now
		if now {
			l.log.Info("sprite held awake by tasks", "sprite", l.label(sp), "tasks", tasks)
		} else {
			l.log.Info("sprite no longer held by tasks", "sprite", l.label(sp))
		}
	}
}

// Why a sprite is suspended, which sprite.suspended reports.
const (
	suspendIdle     = "idle"     // the idle rule
	suspendOperator = "operator" // Suspend, stop(keepWarm), shutdown
	suspendDeadline = "deadline" // a deadline whose action is suspend
)

// suspendLocked snapshots the sprite to disk. An idle suspend is one the guest
// may still veto with errGuestBusy; any other goes ahead regardless.
func (l *Engine) suspendLocked(sp store.Record, rt *runtime, why string) error {
	idle := why == suspendIdle
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
		l.log.Warn("no room for a memory snapshot even with every other sprite cold; sprite stopped cold instead", "sprite", l.label(sp), "needed", mib(need))
		l.Emit(sp, "sprite.stopped", map[string]any{"reason": "no room for a memory snapshot"})
		return nil
	}
	// Balloon the guest's free memory so that it is a hole in the snapshot, not
	// zeros on disk. Best effort: an old VM may have no balloon.
	if _, err := rt.m.Squeeze(ctx); err != nil && !errors.Is(err, vmm.ErrNoBalloon) {
		l.log.Warn("could not squeeze free memory before suspend", "sprite", l.label(sp), "err", err)
	}
	if err := rt.m.Suspend(ctx); err != nil {
		select {
		case <-rt.m.Exited(): // the snapshot was taken but not written: watch reports the exit
		default:
			l.setBalloon(ctx, sp, rt, rt.m, true) // it is running on: give the memory back
		}
		return err
	}
	l.cleanupLocked(rt)
	now := time.Now()
	l.store.UpdateRecord(sp.ID, func(s *store.Record) { s.LastWarmingAt = &now })
	took := time.Since(start)
	snap := vmm.SnapshotBytes(l.store.Dir(sp.ID))
	l.log.Info("sprite suspended", "sprite", l.label(sp), "took", took.Round(time.Millisecond), "snapshot", mib(snap))
	detail := map[string]any{"ms": took.Milliseconds(), "idle": idle, "snapshot_bytes": snap}
	if why == suspendDeadline {
		detail["reason"] = why
	}
	l.Emit(sp, "sprite.suspended", detail)
	// The disk is quiescent exactly here: the guest has synced and the VM is
	// paused. Enqueueing is non-blocking and cannot fail, so a bucket that is
	// unreachable never turns a good suspend into a bad one.
	l.backups.Enqueue(sp.ID, "suspend")
	return nil
}

// diskGen counts the VMs that have been started on a sprite's disk. A backup
// reading that disk in place, because the volume has no reflink support, notes it
// first and gives up when it moves: a wake never waits for an upload, so the
// upload has to notice the wake. Lock-free for the same reason.
func (l *Engine) diskGen(id string) uint64 { return l.rt(id).gen.Load() }

// stop halts a sprite. With keepWarm it is suspended so it can resume later
// (Suspend); otherwise the VM is killed and memory state discarded.
func (l *Engine) stop(sp store.Record, keepWarm bool) error {
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
		return l.suspendLocked(sp, rt, suspendOperator)
	}
	rt.m.Kill()
	l.cleanupLocked(rt)
	vmm.DiscardSnapshot(l.store.Dir(sp.ID))
	l.Emit(sp, "sprite.stopped", nil)
	return nil
}

// Cool drops a suspended sprite's memory snapshot, as the warm TTL would. It
// reports false, and does nothing, when the sprite is not warm.
func (l *Engine) Cool(sp store.Record) bool {
	rt := l.rt(sp.ID)
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if rt.m != nil || !vmm.HasSnapshot(l.store.Dir(sp.ID)) {
		return false
	}
	vmm.DiscardSnapshot(l.store.Dir(sp.ID))
	l.Emit(sp, "sprite.cold", map[string]any{"reason": "operator"})
	return true
}

// withLocked runs fn holding the sprite's transition lock, which orders it
// against every boot, suspend, checkpoint, restore and delete of that sprite.
// fn sees nothing of the runtime: this is for state kept outside the lifecycle
// that has to be decided atomically with respect to those transitions, and
// fn must not call an Engine method that takes the same lock. Prefer a
// specific method; every use is listed here:
//   - leases.reap and leases.set (leases.go) decide a sprite's lease
//     against each other under it, so that a renewal and a reap in flight
//     cannot both win.
//   - SetPolicy (lifecycle_rules.go) changes a sandbox's deadline action
//     under it, for the same reason.
func (l *Engine) withLocked(id string, fn func() error) error {
	rt := l.rt(id)
	rt.mu.Lock()
	defer rt.mu.Unlock()
	return fn()
}

// forget drops runtime state for a deleted sprite.
func (l *Engine) forget(id string) {
	l.mu.Lock()
	delete(l.runtimes, id)
	l.mu.Unlock()
}

// Shutdown suspends every running sprite so they come back warm. The
// background loops stop first, so none is cooling, reaping or checkpointing a
// sprite while it suspends.
func (l *Engine) Shutdown() {
	l.mu.Lock()
	if !l.quitting {
		l.quitting = true
		close(l.quit)
	}
	l.mu.Unlock()
	l.loops.Wait()
	var wg sync.WaitGroup
	for _, sp := range l.store.Records() {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := l.stop(sp, true); err != nil {
				l.log.Error("suspend on shutdown failed", "sprite", l.label(sp), "err", err)
				l.stop(sp, false)
			}
		}()
	}
	wg.Wait()
}

// janitor turns long-suspended sprites cold by dropping their memory snapshot,
// and deletes the sprites whose workspace lease has run out.
func (l *Engine) janitor() {
	l.disk.watch()
	l.reapLeases() // before cooling: a sprite on its way out needs no snapshot work
	for _, sp := range l.store.Records() {
		if l.warmExpired(sp) {
			l.coolIfExpired(sp)
		}
	}
}

func (l *Engine) warmExpired(sp store.Record) bool {
	return sp.LastWarmingAt != nil && time.Since(*sp.LastWarmingAt) >= l.opts.WarmTTL
}

// coolIfExpired drops the snapshot of a sprite that has been warm longer than
// the TTL. sp is only a candidate from an earlier read: waiting for the lock
// can outlast a suspend in flight, which renews LastWarmingAt, so the decision
// is taken again on a fresh record. Deciding on the old one threw away snapshots
// seconds old, every sprite's at once when a daemon stopped.
func (l *Engine) coolIfExpired(sp store.Record) {
	rt := l.rt(sp.ID)
	rt.mu.Lock()
	defer rt.mu.Unlock()
	cur, err := l.store.GetRecord(sp.ID)
	if err != nil || !l.warmExpired(cur) {
		return
	}
	if rt.m == nil && vmm.HasSnapshot(l.store.Dir(sp.ID)) {
		vmm.DiscardSnapshot(l.store.Dir(sp.ID))
		l.log.Info("sprite went cold", "sprite", l.label(sp))
		l.Emit(cur, "sprite.cold", map[string]any{"reason": "warm ttl"})
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
