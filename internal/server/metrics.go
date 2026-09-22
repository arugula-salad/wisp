package server

import (
	"bufio"
	"os"
	"path/filepath"
	goruntime "runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

// The web UI's history: a sampler keeps the last hour of host and per-sprite
// figures in memory, so the dashboard can draw charts without a time-series
// database. Nothing here is persisted; a restart starts the history over.

const (
	metricsEvery  = 5 * time.Second
	metricsKeep   = time.Hour
	diskEvery     = time.Minute // disk figures read extent maps, so they are refreshed less often
	eventsKeep    = 200
	clockTicksHz  = 100 // USER_HZ, which Linux fixes at 100 on every architecture it reports it for
	maxHistoryLen = int(metricsKeep / metricsEvery)
)

// MetricPoint is one sample. Every sprite has an entry in Sprites; CPU and
// RSS are zero unless it has a live VM.
type MetricPoint struct {
	T       time.Time `json:"t"`
	Running int       `json:"running"`
	Warm    int       `json:"warm"`
	Cold    int       `json:"cold"`
	// VMMRSS and CPU add up every sprite's Firecracker process. CPU is in cores
	// (1.0 = one core busy for the whole interval).
	VMMRSS       int64   `json:"vmm_rss_bytes"`
	CPU          float64 `json:"cpu_cores"`
	HostMemUsed  int64   `json:"host_mem_used_bytes"`
	HostMemTotal int64   `json:"host_mem_total_bytes"`
	Load1        float64 `json:"load1"`
	VolumeUsed   int64   `json:"volume_used_bytes"`
	VolumeTotal  int64   `json:"volume_total_bytes"`
	// Sprites is keyed by name.
	Sprites map[string]SpritePoint `json:"sprites,omitempty"`
}

type SpritePoint struct {
	State string  `json:"state"`
	CPU   float64 `json:"cpu_cores,omitempty"`
	RSS   int64   `json:"rss_bytes,omitempty"`
}

// SpriteEvent is a state change the sampler saw between two samples. A sprite
// that woke and went back to sleep within one interval shows no event.
type SpriteEvent struct {
	T    time.Time `json:"t"`
	Name string    `json:"name"`
	From string    `json:"from"` // "" for a sprite that appeared
	To   string    `json:"to"`   // "" for a sprite that was deleted
}

// SpriteDisk is the slow half of the per-sprite figures.
type SpriteDisk struct {
	Used      int64 `json:"disk_used_bytes"`
	Exclusive int64 `json:"disk_exclusive_bytes"`
	Apparent  int64 `json:"disk_apparent_bytes"`
	Snapshot  int64 `json:"snapshot_bytes"`
}

type Metrics struct {
	Cores    int                   `json:"host_cores"`
	Interval float64               `json:"interval_seconds"`
	Points   []MetricPoint         `json:"points"`
	Events   []SpriteEvent         `json:"events"`
	Disk     map[string]SpriteDisk `json:"disk"`
	DiskAt   time.Time             `json:"disk_at"`
}

type metrics struct {
	s *Server

	// sampling serializes whole samples; ticks, states and last belong to it.
	sampling sync.Mutex

	mu     sync.Mutex
	points []MetricPoint
	events []SpriteEvent
	disk   map[string]SpriteDisk
	diskAt time.Time

	// Between samples: the last CPU tick count per VMM pid, and each sprite's state.
	ticks  map[int]int64
	states map[string]string
	last   time.Time
}

func newMetrics(s *Server) *metrics {
	return &metrics{s: s, disk: map[string]SpriteDisk{}, ticks: map[int]int64{}, states: map[string]string{}}
}

// StartMetrics begins sampling for the web UI's charts. The daemon calls it;
// tests take samples by hand instead.
func (s *Server) StartMetrics() {
	go s.metrics.run()
	go s.runHTTPStats()
}

func (m *metrics) run() {
	m.sample(time.Now())
	t := time.NewTicker(metricsEvery)
	defer t.Stop()
	for now := range t.C {
		m.sample(now)
	}
}

// sample reads what it can without waiting on a sprite: a transition in
// flight counts as running, as in the API, and wakes nothing.
func (m *metrics) sample(now time.Time) {
	m.sampling.Lock()
	defer m.sampling.Unlock()
	l := m.s.life
	p := MetricPoint{T: now.UTC().Truncate(time.Second), Sprites: map[string]SpritePoint{}}
	elapsed := now.Sub(m.last).Seconds()
	ticks := map[int]int64{}
	states := map[string]string{}
	sprites := m.s.store.List("")
	for _, sp := range sprites {
		state := l.Status(sp)
		states[sp.Name] = state
		switch state {
		case "running":
			p.Running++
		case "warm":
			p.Warm++
		default:
			p.Cold++
		}
		pt := SpritePoint{State: state}
		p.Sprites[sp.Name] = pt
		vm, _, _ := l.peek(sp.ID)
		if vm == nil {
			continue
		}
		pr, ok := readProc(vm.Pid())
		if !ok {
			continue
		}
		pt.RSS = pr.rss
		ticks[pr.pid] = pr.cpuTicks
		if prev, ok := m.ticks[pr.pid]; ok && elapsed > 0 && pr.cpuTicks >= prev {
			pt.CPU = float64(pr.cpuTicks-prev) / clockTicksHz / elapsed
		}
		p.Sprites[sp.Name] = pt
		p.VMMRSS += pt.RSS
		p.CPU += pt.CPU
	}
	p.HostMemTotal, p.HostMemUsed = readMeminfo()
	p.Load1 = readLoad1()
	if h, err := l.disk.probe(); err == nil {
		p.VolumeTotal, p.VolumeUsed = h.VolumeTotal, h.VolumeTotal-h.VolumeFree
	}

	// Refresh disk figures on their own clock, and whenever the set of sprites
	// changed, so a new one does not go a minute without any. (m.disk is only
	// written here, under sampling, so reading it needs no m.mu.)
	stale := now.Sub(m.diskAt) >= diskEvery || len(m.disk) != len(sprites)
	for _, sp := range sprites {
		if _, ok := m.disk[sp.Name]; !ok {
			stale = true
		}
	}
	var disk map[string]SpriteDisk
	if stale {
		st := make([]SpriteStatus, len(sprites))
		for i, sp := range sprites {
			st[i] = SpriteStatus{Name: sp.Name, ID: sp.ID}
		}
		diskUsage(m.s.store, filepath.Join(m.s.opts.DataDir, "vm"), st)
		disk = map[string]SpriteDisk{}
		for _, s := range st {
			disk[s.Name] = SpriteDisk{Used: s.DiskUsed, Exclusive: s.DiskExclusive, Apparent: s.DiskApparent, Snapshot: s.SnapshotBytes}
		}
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.last.IsZero() {
		for name, to := range states {
			if from := m.states[name]; from != to {
				m.events = append(m.events, SpriteEvent{T: p.T, Name: name, From: from, To: to})
			}
		}
		for name, from := range m.states {
			if _, ok := states[name]; !ok {
				m.events = append(m.events, SpriteEvent{T: p.T, Name: name, From: from})
			}
		}
		if n := len(m.events) - eventsKeep; n > 0 {
			m.events = append([]SpriteEvent(nil), m.events[n:]...)
		}
	}
	m.states, m.ticks, m.last = states, ticks, now
	m.points = append(m.points, p)
	if n := len(m.points) - maxHistoryLen; n > 0 {
		m.points = append([]MetricPoint(nil), m.points[n:]...)
	}
	if disk != nil {
		m.disk, m.diskAt = disk, now
	}
}

// Snapshot returns the history since the given time (zero: all of it).
func (m *metrics) snapshot(since time.Time) Metrics {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := Metrics{Cores: goruntime.NumCPU(), Interval: metricsEvery.Seconds(),
		Points: []MetricPoint{}, Events: []SpriteEvent{}, Disk: m.disk, DiskAt: m.diskAt}
	for _, p := range m.points {
		if p.T.After(since) {
			out.Points = append(out.Points, p)
		}
	}
	for _, e := range m.events {
		if e.T.After(since) {
			out.Events = append(out.Events, e)
		}
	}
	return out
}

func readMeminfo() (total, used int64) {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0, 0
	}
	defer f.Close()
	var avail int64
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		k, v, ok := strings.Cut(sc.Text(), ":")
		if !ok {
			continue
		}
		n, _ := strconv.ParseInt(strings.TrimSuffix(strings.TrimSpace(v), " kB"), 10, 64)
		switch k {
		case "MemTotal":
			total = n << 10
		case "MemAvailable":
			avail = n << 10
		}
	}
	return total, total - avail
}

func readLoad1() float64 {
	b, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		return 0
	}
	f := strings.Fields(string(b))
	if len(f) == 0 {
		return 0
	}
	v, _ := strconv.ParseFloat(f[0], 64)
	return v
}
