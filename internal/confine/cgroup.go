package confine

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// Per-VM cgroup limits. Everything is derived from the VM shape the lifecycle
// already computed, so the resources policy drives the host limits through the
// same numbers it uses to size the guest.
const (
	// cpuPeriodUs is the cgroup v2 default scheduling period.
	cpuPeriodUs = 100000
	// cpuSlackCores is CPU the VMM's own threads get on top of the guest's
	// vCPUs: virtio I/O, the API server and the vsock muxer are not vCPUs, and
	// capping the lot at exactly vcpu_count throttles disk-heavy guests.
	cpuSlackCores = 1
	// memHeadroomMiB covers Firecracker itself (~30 MiB) plus page cache for the
	// sprite's disk on top of the guest's RAM, and memHardSlackMiB sits between
	// memory.high and memory.max. Reclaim (high) rather than an OOM kill (max)
	// has to be what a busy sprite hits: an OOM kill of the VMM destroys the
	// guest's memory state, and a suspend writes a whole snapshot through page
	// cache charged to this cgroup.
	memHeadroomMiB   = 192
	memHardSlackMiB  = 256
	pidsBase         = 32
	pidsPerVCPU      = 4
	cgroupRemoveTry  = 20
	cgroupRemoveWait = 25 * time.Millisecond
)

// wantControllers are the cgroup v2 controllers a VM leaf needs. All three must
// be delegated to us, or we do not claim to enforce any of them.
var wantControllers = []string{"cpu", "memory", "pids"}

// Cgroups is a delegated cgroup v2 subtree that holds one leaf per running VMM.
// It needs no root: on a systemd host the user's own `user@<uid>.service` slice
// already has cpu, memory and pids delegated.
type Cgroups struct {
	root string // absolute path of our subtree, e.g. <mount>/user.slice/.../mini-sprites
}

// Limits is one VM's share of the host.
type Limits struct {
	VCPUs  int
	MemMiB int // guest RAM; the cgroup adds headroom for the VMM on top
}

// Cgroup is one VM's leaf. Its fd goes to clone3(CLONE_INTO_CGROUP) so the VMM
// is born inside the limits rather than moved into them afterwards.
type Cgroup struct {
	path string
	fd   int
}

// openCgroups finds a delegated subtree we may write to and creates `name`
// inside it. It walks up from our own cgroup because delegation can be granted
// at any level: on Ubuntu 25.10, `app.slice` has memory and pids but not cpu,
// while `user@1000.service` one level up has all three.
func openCgroups(name string) (*Cgroups, error) {
	mount, err := cgroup2Mount()
	if err != nil {
		return nil, err
	}
	rel, err := ownCgroup()
	if err != nil {
		return nil, err
	}
	var tried []string
	for dir := filepath.Dir(filepath.Join(mount, rel)); strings.HasPrefix(dir, mount); dir = filepath.Dir(dir) {
		if missing := missingControllers(filepath.Join(dir, "cgroup.subtree_control")); len(missing) > 0 {
			tried = append(tried, fmt.Sprintf("%s (no %s)", dir, strings.Join(missing, ",")))
			continue
		}
		root := filepath.Join(dir, name)
		if err := os.Mkdir(root, 0o755); err != nil && !errors.Is(err, os.ErrExist) {
			tried = append(tried, fmt.Sprintf("%s (%v)", dir, err))
			continue
		}
		// Enable the controllers for our own children. Harmless if already set.
		if err := os.WriteFile(filepath.Join(root, "cgroup.subtree_control"), []byte("+cpu +memory +pids"), 0o644); err != nil {
			os.Remove(root)
			tried = append(tried, fmt.Sprintf("%s (subtree_control: %v)", root, err))
			continue
		}
		return &Cgroups{root: root}, nil
	}
	return nil, fmt.Errorf("%w: no delegated cgroup v2 subtree with %s; tried %s",
		errUnsupported, strings.Join(wantControllers, ","), strings.Join(tried, "; "))
}

// cgroup2Mount finds where cgroup2 is mounted (/sys/fs/cgroup on any current host).
func cgroup2Mount() (string, error) {
	f, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return "", err
	}
	defer f.Close()
	for sc := bufio.NewScanner(f); sc.Scan(); {
		// ... <mountpoint> ... - <fstype> <source> <opts>
		fields := strings.Fields(sc.Text())
		for i, fl := range fields {
			if fl == "-" && i+1 < len(fields) && fields[i+1] == "cgroup2" && i >= 4 {
				return fields[4], nil
			}
		}
	}
	return "", fmt.Errorf("%w: no cgroup2 mount", errUnsupported)
}

// ownCgroup is this process's cgroup path, relative to the mount.
func ownCgroup() (string, error) {
	b, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(string(b), "\n") {
		// cgroup v2 is always the "0::<path>" entry.
		if rest, ok := strings.CutPrefix(line, "0::"); ok {
			return strings.TrimSpace(rest), nil
		}
	}
	return "", fmt.Errorf("%w: not in a cgroup v2 hierarchy", errUnsupported)
}

// missingControllers reports which of wantControllers a subtree_control file
// does not enable for its children.
func missingControllers(path string) []string {
	b, err := os.ReadFile(path)
	if err != nil {
		return wantControllers
	}
	have := map[string]bool{}
	for _, c := range strings.Fields(string(b)) {
		have[c] = true
	}
	var missing []string
	for _, c := range wantControllers {
		if !have[c] {
			missing = append(missing, c)
		}
	}
	return missing
}

// Create makes the leaf cgroup for one VM and writes its limits. The caller
// must Close it once the child is started and Remove it when the VMM exits.
func (c *Cgroups) Create(id string, lim Limits) (*Cgroup, error) {
	path := filepath.Join(c.root, id)
	if err := os.Mkdir(path, 0o755); err != nil && !errors.Is(err, os.ErrExist) {
		return nil, err
	}
	g := &Cgroup{path: path, fd: -1}
	limits := map[string]string{
		"cpu.max":  fmt.Sprintf("%d %d", (lim.VCPUs+cpuSlackCores)*cpuPeriodUs, cpuPeriodUs),
		"pids.max": strconv.Itoa(pidsBase + pidsPerVCPU*lim.VCPUs),
		// Order matters: memory.high must not be left above a lower memory.max.
		"memory.max":  strconv.Itoa((lim.MemMiB + memHeadroomMiB + memHardSlackMiB) << 20),
		"memory.high": strconv.Itoa((lim.MemMiB + memHeadroomMiB) << 20),
	}
	for _, k := range []string{"cpu.max", "pids.max", "memory.max", "memory.high"} {
		if err := os.WriteFile(filepath.Join(path, k), []byte(limits[k]), 0o644); err != nil {
			g.Remove()
			return nil, fmt.Errorf("cgroup %s: %w", k, err)
		}
	}
	fd, err := unix.Open(path, unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		g.Remove()
		return nil, fmt.Errorf("cgroup open %s: %w", path, err)
	}
	g.fd = fd
	return g, nil
}

// FD is the directory fd for SysProcAttr.CgroupFD.
func (g *Cgroup) FD() int { return g.fd }

// Close releases the directory fd. The cgroup itself stays until Remove.
func (g *Cgroup) Close() {
	if g != nil && g.fd >= 0 {
		unix.Close(g.fd)
		g.fd = -1
	}
}

// Remove deletes the leaf. A cgroup cannot be removed while it still holds a
// process, and the kernel takes a moment to reap one that has just been killed,
// so retry briefly; Sweep is the backstop for whatever is left behind.
func (g *Cgroup) Remove() {
	if g == nil {
		return
	}
	g.Close()
	removeCgroup(g.path)
}

// removeCgroup rmdirs a cgroup, waiting out the EBUSY window while the kernel
// finishes reaping a process that has just been killed.
func removeCgroup(path string) bool {
	for i := 0; i < cgroupRemoveTry; i++ {
		if err := os.Remove(path); err == nil || errors.Is(err, os.ErrNotExist) {
			return true
		}
		time.Sleep(cgroupRemoveWait)
	}
	return false
}

// Sweep removes leaves left by a spritesd that died without cleaning up. A leaf
// whose VMM is still running cannot be removed and is left alone, so callers
// must reap orphaned VMMs first — see Confiner.SweepStale.
func (c *Cgroups) Sweep() {
	entries, err := os.ReadDir(c.root)
	if err != nil {
		return
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		leaf := filepath.Join(c.root, e.Name())
		// A leaf that still holds a VMM is somebody's live sprite; do not spend
		// the retry window rmdir'ing it only to fail with EBUSY.
		if procs, err := os.ReadFile(filepath.Join(leaf, "cgroup.procs")); err == nil && len(procs) > 0 {
			continue
		}
		removeCgroup(leaf)
	}
}
