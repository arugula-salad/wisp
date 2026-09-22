package server

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/jhgaylor/mini-sprites/internal/netd"
	"github.com/jhgaylor/mini-sprites/internal/store"
	"github.com/jhgaylor/mini-sprites/internal/vmm"
)

// The operator's view of one host (`spritesd status`). A running daemon serves
// it on a unix socket in the data directory, where the filesystem permission is
// the authentication; with no daemon up, OfflineStatus answers from the files.
// The JSON field names are an interface: scripts read them.

// StatusSocket is the socket's name inside the data directory.
const StatusSocket = "spritesd.sock"

type Status struct {
	// Daemon is nil when the answer was read from the files, with no daemon running.
	Daemon  *DaemonStatus  `json:"daemon"`
	Host    HostStatus     `json:"host"`
	Sprites []SpriteStatus `json:"sprites"`
	// Orphans are Firecracker processes of this user that no running spritesd
	// is the parent of. They are reported, never killed: another data directory
	// or somebody's experiment may own them.
	Orphans []VMProcess `json:"orphans"`
	// OtherDaemons are spritesd processes of this user besides the one answering.
	OtherDaemons []OtherDaemon `json:"other_daemons"`
}

type DaemonStatus struct {
	Pid       int       `json:"pid"`
	StartedAt time.Time `json:"started_at"`
	Listen    string    `json:"listen"`
}

type HostStatus struct {
	DataDir string   `json:"data_dir"`
	Volume  Headroom `json:"volume"`
	// Reflink says whether clones on the sprite volume are copy-on-write.
	Reflink bool `json:"reflink"`
	// DiskReserve is what creates and checkpoints must leave free (daemon only).
	DiskReserve int64 `json:"disk_reserve_bytes"`
	// Networking is false for a daemon run with --net=false or without the bridge.
	Networking bool `json:"networking"`
	TapsTotal  int  `json:"taps_total"`
	TapsUsed   int  `json:"taps_used"`
	// PolicyHelper is mini-sprites-netd, without which restrictive network policies are refused.
	PolicyHelper HelperStatus `json:"policy_helper"`
	Running      int          `json:"running"`
	Warm         int          `json:"warm"`
	Cold         int          `json:"cold"`
	// Limits of 0 mean none is configured.
	MaxRunning int `json:"max_running"`
	MaxSprites int `json:"max_sprites"`
	// Images is the cache of disks built from container images (images.go).
	Images ImageCacheStatus `json:"images"`
}

type ImageCacheStatus struct {
	Count int `json:"count"`
	// Bytes is what the cached disks occupy on the sprite volume.
	Bytes int64 `json:"bytes"`
}

func imageCacheStatus(vmRoot string) ImageCacheStatus {
	var st ImageCacheStatus
	for _, f := range imageDisks(vmRoot) {
		st.Count++
		st.Bytes += allocated(f)
	}
	return st
}

type HelperStatus struct {
	Reachable bool   `json:"reachable"`
	Detail    string `json:"detail,omitempty"`
}

type SpriteStatus struct {
	Name  string `json:"name"`
	ID    string `json:"id"`
	State string `json:"state"` // running, warm or cold
	// Busy marks a sprite in the middle of a transition (booting, suspending,
	// checkpointing); what could not be read without waiting for it is left out.
	Busy   bool  `json:"busy,omitempty"`
	VMMPid int   `json:"vmm_pid,omitempty"`
	VMMRSS int64 `json:"vmm_rss_bytes,omitempty"`
	// DiskApparent is the size the guest sees. DiskUsed is what the sprite's disk
	// and checkpoints occupy, each shared block counted once; DiskExclusive is the
	// part nothing else on the volume shares, i.e. what deleting the sprite frees.
	DiskApparent  int64 `json:"disk_apparent_bytes"`
	DiskUsed      int64 `json:"disk_used_bytes"`
	DiskExclusive int64 `json:"disk_exclusive_bytes"`
	// SnapshotBytes is the memory snapshot of a warm sprite.
	SnapshotBytes int64 `json:"snapshot_bytes"`
	Checkpoints   int   `json:"checkpoints"`
	// MountedCheckpoints maps a checkpoint slot to the checkpoint mounted in it.
	MountedCheckpoints map[int]string `json:"mounted_checkpoints,omitempty"`
	// TaskHolds counts live tasks keeping the sprite awake; nil when the guest was not asked.
	TaskHolds   *int   `json:"task_holds"`
	APIInflight int    `json:"api_inflight"`
	NetIndex    int    `json:"net_index"`
	IP          string `json:"ip,omitempty"`
	Tap         string `json:"tap,omitempty"`
	// PolicyRestricted says the sprite's egress goes through the policy enforcer.
	PolicyRestricted bool       `json:"policy_restricted"`
	LastRunningAt    *time.Time `json:"last_running_at,omitempty"`
	LastWarmingAt    *time.Time `json:"last_warming_at,omitempty"`
	// Image is the container image the disk was made from, if any.
	Image string `json:"image,omitempty"`
	// ExpiresAt is the workspace lease: when this sprite is deleted, disk and
	// all (leases.go). Absent on a sprite with no lease, which is the default.
	// Protected holds the deletion off without clearing the deadline, so an
	// operator can see both that the lease ran out and why the sprite is still here.
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
	Protected bool       `json:"protected,omitempty"`
}

type VMProcess struct {
	Pid int    `json:"pid"`
	Cwd string `json:"cwd"`
	RSS int64  `json:"rss_bytes"`
	// Parent is what the process hangs off now: init or a systemd user manager
	// once its starter died, otherwise whatever started it by hand.
	ParentPid  int    `json:"parent_pid"`
	ParentName string `json:"parent_name"`
	// InDataDir marks a VM inside this data directory: left by a spritesd that
	// died, and reaped by the next one to start.
	InDataDir bool `json:"in_data_dir"`
}

type OtherDaemon struct {
	Pid int      `json:"pid"`
	Cmd []string `json:"cmd"`
	Cwd string   `json:"cwd"`
	VMs int      `json:"vms"`
}

type proc struct {
	pid, ppid int
	exe, cwd  string
	rss       int64
	cpuTicks  int64 // utime + stime, in clock ticks
}

func readProc(pid int) (p proc, ok bool) {
	dir := "/proc/" + strconv.Itoa(pid)
	b, err := os.ReadFile(dir + "/stat")
	if err != nil {
		return p, false
	}
	// "pid (comm) state ppid ...": comm may hold spaces and parentheses, so cut at the last one.
	i := strings.LastIndex(string(b), ") ")
	if i < 0 {
		return p, false
	}
	f := strings.Fields(string(b)[i+2:])
	if len(f) < 22 {
		return p, false
	}
	p.pid = pid
	p.ppid, _ = strconv.Atoi(f[1])
	pages, _ := strconv.ParseInt(f[21], 10, 64)
	p.rss = pages * int64(os.Getpagesize())
	ut, _ := strconv.ParseInt(f[11], 10, 64)
	st, _ := strconv.ParseInt(f[12], 10, 64)
	p.cpuTicks = ut + st
	p.exe, _ = os.Readlink(dir + "/exe")
	p.cwd, _ = os.Readlink(dir + "/cwd")
	return p, true
}

func procName(pid int) string {
	b, _ := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/comm")
	return strings.TrimSpace(string(b))
}

// isExe tolerates the " (deleted)" suffix of a binary rebuilt under a running process.
func isExe(path, name string) bool {
	return filepath.Base(strings.TrimSuffix(path, " (deleted)")) == name
}

// scanProcs sorts this user's Firecrackers into orphans and other daemons'
// VMs. self is the answering daemon's pid, whose own VMs are neither (0 offline).
func scanProcs(vmRoot string, self int) (orphans []VMProcess, others []OtherDaemon) {
	orphans, others = []VMProcess{}, []OtherDaemon{}
	entries, _ := os.ReadDir("/proc")
	uid := uint32(os.Getuid())
	daemons := map[int]*OtherDaemon{}
	var vms []proc
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		var st syscall.Stat_t
		if syscall.Stat("/proc/"+e.Name(), &st) != nil || st.Uid != uid {
			continue
		}
		p, ok := readProc(pid)
		switch {
		case !ok:
		case isExe(p.exe, "firecracker"):
			vms = append(vms, p)
		case isExe(p.exe, "spritesd") && pid != self && pid != os.Getpid():
			b, _ := os.ReadFile("/proc/" + e.Name() + "/cmdline")
			cmd := strings.Split(strings.TrimRight(string(b), "\x00"), "\x00")
			// `spritesd status` and friends are not daemons.
			if len(cmd) > 1 && !strings.HasPrefix(cmd[1], "-") {
				continue
			}
			daemons[pid] = &OtherDaemon{Pid: pid, Cmd: cmd, Cwd: p.cwd}
		}
	}
	for _, p := range vms {
		if p.ppid == self && self != 0 {
			continue
		}
		if d, ok := daemons[p.ppid]; ok {
			d.VMs++
			continue
		}
		orphans = append(orphans, VMProcess{Pid: p.pid, Cwd: p.cwd, RSS: p.rss, ParentPid: p.ppid, ParentName: procName(p.ppid),
			InDataDir: strings.HasPrefix(p.cwd, vmRoot+"/")})
	}
	for _, d := range daemons {
		others = append(others, *d)
	}
	return orphans, others
}

// diskUsage fills in the three disk figures for every sprite at once, since
// what is exclusive to one depends on what the others hold.
func diskUsage(st *store.Store, vmRoot string, sprites []SpriteStatus) {
	owners := make([][]span, len(sprites), len(sprites)+1)
	mapped := true
	for i := range sprites {
		dir := st.Dir(sprites[i].ID)
		files, _ := filepath.Glob(filepath.Join(dir, "checkpoints", "*.ext4"))
		files = append(files, filepath.Join(dir, vmm.DiskFile))
		if fi, err := os.Stat(filepath.Join(dir, vmm.DiskFile)); err == nil {
			sprites[i].DiskApparent = fi.Size()
		}
		var plain int64
		for _, f := range files {
			plain += allocated(f)
			spans, ok := fileSpans(f)
			mapped = mapped && ok
			owners[i] = append(owners[i], spans...)
		}
		owners[i] = merge(owners[i])
		sprites[i].DiskUsed, sprites[i].DiskExclusive = plain, plain
		sprites[i].SnapshotBytes = vmm.SnapshotBytes(dir)
	}
	if !mapped {
		return // no extent maps here, hence no sharing to account for either
	}
	// The base image's mirror shares blocks with every disk cloned from it, and
	// a cached image disk with every sprite made from that image.
	for _, f := range append([]string{filepath.Join(vmRoot, localBaseName)}, imageDisks(vmRoot)...) {
		if spans, ok := fileSpans(f); ok {
			owners = append(owners, merge(spans))
		}
	}
	for i, own := range exclusive(owners)[:len(sprites)] {
		sprites[i].DiskUsed, sprites[i].DiskExclusive = total(owners[i]), own
	}
}

func spriteBase(sp store.Sprite) SpriteStatus {
	return SpriteStatus{Name: sp.Name, ID: sp.ID, Checkpoints: len(sp.Checkpoints), MountedCheckpoints: sp.Mounts,
		NetIndex: sp.NetIndex, LastRunningAt: sp.LastRunningAt, LastWarmingAt: sp.LastWarmingAt, Image: sp.Image,
		ExpiresAt: sp.ExpiresAt, Protected: sp.Protected}
}

func (h *HostStatus) count(state string) {
	switch state {
	case "running":
		h.Running++
	case "warm":
		h.Warm++
	default:
		h.Cold++
	}
}

// OfflineStatus is the answer when no daemon is running on dataDir: everything
// the files and /proc can tell. A sprite can only be warm or cold then; a VM
// still alive in its directory shows up among the orphans.
func OfflineStatus(dataDir, netdSocket string) (Status, error) {
	vmRoot := filepath.Join(dataDir, "vm")
	if _, err := os.Stat(vmRoot); err != nil {
		return Status{}, fmt.Errorf("no sprite directory at %s (is --data right?)", vmRoot)
	}
	st, err := store.Open(dataDir)
	if err != nil {
		return Status{}, err
	}
	out := Status{Host: HostStatus{DataDir: dataDir, Reflink: probeReflink(vmRoot)}, Sprites: []SpriteStatus{}}
	out.Host.Volume, _ = probeHeadroom(vmRoot)
	out.Host.Images = imageCacheStatus(vmRoot)
	if netdSocket == "" {
		netdSocket = netd.DefaultSocket
	}
	// Connecting would make the helper log a refused request, so only look.
	if fi, err := os.Stat(netdSocket); err == nil && fi.Mode()&os.ModeSocket != 0 {
		out.Host.PolicyHelper = HelperStatus{Reachable: true, Detail: "socket present at " + netdSocket + "; not probed without a daemon"}
	} else {
		out.Host.PolicyHelper.Detail = "no socket at " + netdSocket
	}
	for _, sp := range st.List("") {
		s := spriteBase(sp)
		s.State = "cold"
		if vmm.HasSnapshot(st.Dir(sp.ID)) {
			s.State = "warm"
		}
		s.PolicyRestricted = len(sp.NetworkRules) > 0 // the daemon knows better: it compiles the rules
		out.Host.count(s.State)
		out.Sprites = append(out.Sprites, s)
	}
	diskUsage(st, vmRoot, out.Sprites)
	out.Orphans, out.OtherDaemons = scanProcs(vmRoot, 0)
	return out, nil
}

// peek reads a sprite's runtime state without waiting for a transition in flight.
func (l *Lifecycle) peek(id string) (m *vmm.Machine, tap string, settled bool) {
	rt := l.rt(id)
	if !rt.mu.TryLock() {
		return nil, "", false
	}
	defer rt.mu.Unlock()
	return rt.m, rt.tap, true
}

func (e *egress) helperStatus() HelperStatus {
	e.mu.Lock()
	defer e.mu.Unlock()
	switch {
	case !e.gateway.IsValid():
		return HelperStatus{Detail: "not used: " + e.down}
	case e.down != "":
		return HelperStatus{Detail: e.down}
	case e.lastPush == "ok":
		return HelperStatus{Reachable: true}
	}
	return HelperStatus{Detail: e.lastPush}
}

// status is the live view.
func (s *Server) status(ctx context.Context, started time.Time, listen string) Status {
	l := s.life
	vmRoot := filepath.Join(s.opts.DataDir, "vm")
	out := Status{Daemon: &DaemonStatus{Pid: os.Getpid(), StartedAt: started, Listen: listen},
		Host: HostStatus{DataDir: s.opts.DataDir, Reflink: s.storage.reflink, DiskReserve: s.opts.DiskReserve,
			Networking: l.gateway != nil, PolicyHelper: l.egress.helperStatus(),
			MaxRunning: s.opts.MaxRunning, MaxSprites: s.opts.MaxSprites},
		Sprites: []SpriteStatus{}}
	out.Host.Volume, _ = l.disk.probe()
	out.Host.Images = imageCacheStatus(vmRoot)
	l.mu.Lock()
	out.Host.TapsTotal, out.Host.TapsUsed = l.taps, l.taps-len(l.freeTaps)
	l.mu.Unlock()

	var wg sync.WaitGroup
	sprites := s.store.List("")
	out.Sprites = make([]SpriteStatus, len(sprites))
	for i, sp := range sprites {
		st := spriteBase(sp)
		st.State = l.Status(sp)
		st.PolicyRestricted = l.egress.compile(sp).Restrictive()
		if ip := l.spriteIP(sp); ip != nil {
			st.IP = ip.String()
		}
		rt := l.rt(sp.ID)
		rt.useMu.Lock()
		st.APIInflight = rt.inflight
		rt.useMu.Unlock()
		m, tap, settled := l.peek(sp.ID)
		st.Busy, st.Tap = !settled, tap
		out.Host.count(st.State)
		out.Sprites[i] = st
		if m != nil {
			out.Sprites[i].VMMPid = m.Pid()
			if p, ok := readProc(m.Pid()); ok {
				out.Sprites[i].VMMRSS = p.rss
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				var act struct {
					Tasks int `json:"tasks"`
				}
				// The same question the idle watcher asks, so it does not count as activity.
				cctx, cancel := context.WithTimeout(ctx, time.Second)
				defer cancel()
				if agentCall(cctx, m, http.MethodGet, "/internal/activity", nil, &act) == nil {
					out.Sprites[i].TaskHolds = &act.Tasks
				}
			}()
		}
	}
	wg.Wait()
	diskUsage(s.store, vmRoot, out.Sprites)
	out.Orphans, out.OtherDaemons = scanProcs(vmRoot, os.Getpid())
	return out
}

// StatusHandler serves the operator socket. It carries no token check: whoever
// can open the socket can already read the token file beside it.
func (s *Server) StatusHandler(listen string) http.Handler {
	started := time.Now()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /status", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, s.status(r.Context(), started, listen))
	})
	s.registerImageOps(mux)
	return mux
}
