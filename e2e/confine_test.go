//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The host side of confinement: that wispd really did put each Firecracker
// in its own cgroup with the limits the sprite's shape implies, and that the
// VMM still looks the way vmm.ReapOrphan expects after going through the
// Landlock shim. These read /proc and /sys, so they only mean anything when the
// tests run on the wispd host — which is the only way wisp is
// deployed. They skip otherwise.

// dataDir is where wispd keeps machine directories.
func dataDir(t *testing.T) string {
	t.Helper()
	if d := os.Getenv("WISP_DATA"); d != "" {
		return d
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("cannot locate the data directory")
	}
	return filepath.Join(home, ".local", "share", "wisp")
}

// spriteID reads a sprite's id, which names its machine directory.
func spriteID(t *testing.T, name string) string {
	t.Helper()
	var got struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(want(t, "GET", "/v1/sprites/"+name, "", 200)), &got); err != nil {
		t.Fatal(err)
	}
	return got.ID
}

// vmmPID is the running Firecracker's pid, from the machine directory.
func vmmPID(t *testing.T, id string) int {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dataDir(t), "vm", id, "fc.pid"))
	if err != nil {
		t.Skipf("no fc.pid for %s (not running on the wispd host?): %v", id, err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		t.Fatal(err)
	}
	return pid
}

// procCgroup is the absolute path of a process's cgroup v2 directory.
func procCgroup(t *testing.T, pid int) string {
	t.Helper()
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/cgroup", pid))
	if err != nil {
		t.Skipf("cannot read the VMM's cgroup: %v", err)
	}
	rel := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(string(b)), "0::"))
	if rel == "" || rel == "/" {
		t.Skipf("VMM %d is not in a cgroup of its own (%q): no delegation on this host", pid, string(b))
	}
	return filepath.Join("/sys/fs/cgroup", rel)
}

func cgVal(t *testing.T, dir, file string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, file))
	if err != nil {
		t.Fatalf("read %s: %v", file, err)
	}
	return strings.TrimSpace(string(b))
}

func cgInt(t *testing.T, dir, file string) int {
	t.Helper()
	v, err := strconv.Atoi(cgVal(t, dir, file))
	if err != nil {
		t.Fatalf("%s is %q, not a number", file, cgVal(t, dir, file))
	}
	return v
}

// bootedSprite creates a sprite, optionally sets a policy while it is still
// cold (so the first boot picks it up), wakes it, and returns its cgroup.
func bootedSprite(t *testing.T, name, resourcesPolicy string) string {
	t.Helper()
	c := client(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	sp, err := c.CreateSprite(ctx, name, nil)
	if err != nil {
		t.Fatalf("create %s: %v", name, err)
	}
	t.Cleanup(func() { c.DeleteSprite(context.Background(), name) })
	if resourcesPolicy != "" {
		// The sprite is still cold, so this rides the next (first) boot, where
		// a memory limit also sizes the VM.
		want(t, "POST", "/v1/sprites/"+name+"/policy/resources", resourcesPolicy, 204)
	}
	if out, err := sp.CommandContext(ctx, "true").CombinedOutput(); err != nil {
		t.Fatalf("wake %s: %v (%s)", name, err, out)
	}
	return procCgroup(t, vmmPID(t, spriteID(t, name)))
}

// TestVMMIsConfined: a running sprite's VMM sits in its own cgroup with real
// limits, and the confinement shim has left no trace that would confuse orphan
// reaping.
func TestVMMIsConfined(t *testing.T) {
	name := fmt.Sprintf("confined-%d", time.Now().UnixNano()%1e9)
	cg := bootedSprite(t, name, "")
	id := spriteID(t, name)
	pid := vmmPID(t, id)

	// vmm.ReapOrphan identifies an orphan by exe and cwd. The shim execs
	// Firecracker in place, so both must still look exactly as they did before
	// confinement existed. If the shim is ever changed to fork, this fails.
	exe, err := os.Readlink(fmt.Sprintf("/proc/%d/exe", pid))
	if err != nil {
		t.Skipf("cannot read the VMM's exe: %v", err)
	}
	if !strings.Contains(filepath.Base(exe), "firecracker") {
		t.Errorf("VMM exe is %q, not firecracker: ReapOrphan would not recognise it", exe)
	}
	cwd, err := os.Readlink(fmt.Sprintf("/proc/%d/cwd", pid))
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(cwd) != id {
		t.Errorf("VMM cwd is %q, want the machine directory for %s", cwd, id)
	}

	for _, f := range []string{"cpu.max", "memory.max", "memory.high", "pids.max"} {
		if v := cgVal(t, cg, f); v == "max" || strings.HasPrefix(v, "max ") {
			t.Errorf("%s is %q: the VMM is in a cgroup but unlimited", f, v)
		}
	}
	// memory.high must sit below memory.max, so a sprite that fills its RAM is
	// reclaimed rather than OOM-killed; an OOM kill would lose the guest state.
	if high, max := cgInt(t, cg, "memory.high"), cgInt(t, cg, "memory.max"); high <= 0 || high >= max {
		t.Errorf("memory.high=%d must be positive and below memory.max=%d", high, max)
	}
	// The VMM is a handful of threads; if it is near the cap, pids.max is too low.
	if cur, max := cgInt(t, cg, "pids.current"), cgInt(t, cg, "pids.max"); cur*2 > max {
		t.Errorf("pids.current=%d is close to pids.max=%d", cur, max)
	}
}

// TestResourcesPolicyDrivesCgroup is the issue's "a resources policy can drive
// them": a memory limit set through the API has to show up as a smaller host
// cgroup limit, not only as a bound inside the guest.
func TestResourcesPolicyDrivesCgroup(t *testing.T) {
	stamp := time.Now().UnixNano() % 1e9
	defaultCG := bootedSprite(t, fmt.Sprintf("cgdefault-%d", stamp), "")
	limitedCG := bootedSprite(t, fmt.Sprintf("cglimited-%d", stamp), `{"memory":{"limit_mb":512}}`)

	unlimited, limited := cgInt(t, defaultCG, "memory.max"), cgInt(t, limitedCG, "memory.max")
	if limited >= unlimited {
		t.Errorf("memory.max did not shrink with the policy: %d with a 512 MB limit, %d without", limited, unlimited)
	}
	// 512 MiB of workload plus the guest's headroom plus the VMM's. The bound
	// is loose on purpose: the point is that the policy reaches the host, not
	// the exact arithmetic, which the unit tests pin down.
	if lo, hi := 512<<20, 1400<<20; limited < lo || limited > hi {
		t.Errorf("memory.max = %d, want between %d and %d for a 512 MB limit", limited, lo, hi)
	}
}
