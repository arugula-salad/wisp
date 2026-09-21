// Package server is the spritesd control plane: the Sprites REST/WebSocket API,
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
	"time"

	"github.com/jhgaylor/mini-sprites/internal/store"
	"github.com/jhgaylor/mini-sprites/internal/vmm"
)

const (
	agentPort  = 1024
	bridgeName = "msbr0"
	tapPrefix  = "mstap"
)

type Options struct {
	DataDir       string
	Host          vmm.Host
	BaseImage     string
	IdleTimeout   time.Duration // no activity for this long => suspend (warm)
	WarmTTL       time.Duration // suspended this long => drop memory state (cold)
	DefaultVCPUs  int
	DefaultMemMiB int
	DNS           string
	// NoNetwork boots every sprite without a NIC and leaves the shared tap pool
	// alone, so several spritesd instances (dev, tests) can coexist on one host.
	NoNetwork bool
}

// runtime is the in-memory lifecycle state for one sprite.
type runtime struct {
	// mu serializes lifecycle transitions (boot, restore, suspend, checkpoint, delete).
	mu  sync.Mutex
	m   *vmm.Machine // nil unless running
	tap string

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
	gateway  net.IP // the bridge's address; sprites live in its /16. nil = no networking
}

func NewLifecycle(opts Options, st *store.Store, log *slog.Logger) *Lifecycle {
	l := &Lifecycle{opts: opts, store: st, log: log, runtimes: map[string]*runtime{}}
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
	if len(l.freeTaps) == 0 && !opts.NoNetwork {
		log.Warn("no tap devices found: sprites will boot without networking (run scripts/setup-host.sh once)")
	} else if !opts.NoNetwork {
		log.Info("guest networking enabled", "taps", len(l.freeTaps), "bridge", bridgeName, "gateway", l.gateway)
	}
	for _, sp := range st.List("") {
		vmm.ReapOrphan(st.Dir(sp.ID))
	}
	go l.janitor()
	return l
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
		if err := l.startLocked(ctx, sp, rt); err != nil {
			return nil, nil, err
		}
	}
	rt.begin()
	var once sync.Once
	return rt.m, func() { once.Do(rt.end) }, nil
}

func (l *Lifecycle) cleanupLocked(rt *runtime) {
	rt.m = nil
	l.returnTap(rt.tap)
	rt.tap = ""
}

func (l *Lifecycle) startLocked(ctx context.Context, sp store.Sprite, rt *runtime) error {
	start := time.Now()
	dir := l.store.Dir(sp.ID)
	tap := l.takeTap()
	cfg := l.vmConfig(sp, tap)
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()

	var m *vmm.Machine
	var err error
	mode := "cold"
	if vmm.HasSnapshot(dir) && sp.BootIP != cfg.IPCIDR {
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
			mode, m = "cold", nil
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
		return l.startLocked(ctx, sp, rt)
	}
	rt.m, rt.tap = m, tap
	rt.useMu.Lock()
	rt.lastUse = time.Now()
	rt.useMu.Unlock()
	now := time.Now()
	l.store.Update(sp.Name, func(s *store.Sprite) {
		s.LastRunningAt = &now
		if mode == "cold" {
			s.BootIP = cfg.IPCIDR
		}
	})
	l.log.Info("sprite running", "sprite", sp.Name, "wake", mode, "took", time.Since(start).Round(time.Millisecond), "net", tap != "")
	go l.watch(sp, rt, m)
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
		err = l.suspendLocked(sp, rt)
		rt.mu.Unlock()
		if err == nil {
			return
		}
		l.log.Error("suspend failed; sprite left running", "sprite", sp.Name, "err", err)
	}
}

func (l *Lifecycle) suspendLocked(sp store.Sprite, rt *runtime) error {
	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	// Flush the guest page cache first so that dropping the snapshot later
	// (warm -> cold) is no worse than a clean power cut after sync.
	if err := agentCall(ctx, rt.m, http.MethodPost, "/internal/presuspend", nil, nil); err != nil {
		return err
	}
	if err := rt.m.Suspend(ctx); err != nil {
		return err
	}
	l.cleanupLocked(rt)
	now := time.Now()
	l.store.Update(sp.Name, func(s *store.Sprite) { s.LastWarmingAt = &now })
	l.log.Info("sprite suspended", "sprite", sp.Name, "took", time.Since(start).Round(time.Millisecond))
	return nil
}

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
		return l.suspendLocked(sp, rt)
	}
	rt.m.Kill()
	l.cleanupLocked(rt)
	vmm.DiscardSnapshot(l.store.Dir(sp.ID))
	return nil
}

// Forget drops runtime state for a deleted sprite.
func (l *Lifecycle) Forget(id string) {
	l.mu.Lock()
	delete(l.runtimes, id)
	l.mu.Unlock()
}

// Shutdown suspends every running sprite so they come back warm.
func (l *Lifecycle) Shutdown() {
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

// janitor turns long-suspended sprites cold by dropping their memory snapshot.
func (l *Lifecycle) janitor() {
	for range time.Tick(30 * time.Second) {
		for _, sp := range l.store.List("") {
			if sp.LastWarmingAt == nil || time.Since(*sp.LastWarmingAt) < l.opts.WarmTTL {
				continue
			}
			rt := l.rt(sp.ID)
			rt.mu.Lock()
			if rt.m == nil && vmm.HasSnapshot(l.store.Dir(sp.ID)) {
				vmm.DiscardSnapshot(l.store.Dir(sp.ID))
				l.log.Info("sprite went cold", "sprite", sp.Name)
			}
			rt.mu.Unlock()
		}
	}
}
