package confine

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

// The cgroup tests need a real delegated cgroup v2 subtree, which a systemd
// host gives an ordinary user and a container often does not. They place a
// stand-in (sleep) rather than a VM, because cgroup placement has nothing to do
// with KVM.

func testCgroups(t *testing.T) *Cgroups {
	t.Helper()
	c, err := openCgroups("wisp-test-" + strconv.Itoa(os.Getpid()))
	if err != nil {
		t.Skipf("no delegated cgroup v2 subtree here: %v", err)
	}
	t.Cleanup(func() {
		c.Sweep()
		os.Remove(c.root)
	})
	return c
}

// TestCgroupLimits is the issue's "per-VM CPU/memory/pids limits are enforced
// by cgroup" check: the numbers the kernel holds must be the ones the VM shape
// implies.
func TestCgroupLimits(t *testing.T) {
	c := testCgroups(t)
	lim := Limits{VCPUs: 4, MemMiB: 2048}
	g, err := c.Create("limits", lim)
	if err != nil {
		t.Fatal(err)
	}
	defer g.Remove()

	read := func(name string) string {
		b, err := os.ReadFile(filepath.Join(g.path, name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		return strings.TrimSpace(string(b))
	}
	want := map[string]string{
		"cpu.max":     strconv.Itoa((4+cpuSlackCores)*cpuPeriodUs) + " " + strconv.Itoa(cpuPeriodUs),
		"pids.max":    strconv.Itoa(pidsBase + pidsPerVCPU*4),
		"memory.high": strconv.Itoa((2048 + memHeadroomMiB) << 20),
		"memory.max":  strconv.Itoa((2048 + memHeadroomMiB + memHardSlackMiB) << 20),
	}
	for k, v := range want {
		if got := read(k); got != v {
			t.Errorf("%s = %q, want %q", k, got, v)
		}
	}
	// memory.high must stay below memory.max, or the reclaim-before-OOM design
	// is inverted and a busy sprite gets killed instead of throttled.
	high, _ := strconv.Atoi(read("memory.high"))
	max, _ := strconv.Atoi(read("memory.max"))
	if high >= max {
		t.Errorf("memory.high (%d) must be below memory.max (%d)", high, max)
	}
}

// TestCgroupPlacesChild checks the thing that actually matters: the process is
// born inside the cgroup via clone3(CLONE_INTO_CGROUP), not moved there after
// it has already had a chance to run.
func TestCgroupPlacesChild(t *testing.T) {
	c := testCgroups(t)
	conf := &Confiner{mode: ModeBestEffort, abi: 0, cg: c} // cgroup only, no Landlock
	cmd := exec.Command("/bin/sleep", "30")
	g, err := conf.Start(cmd, Spec{}, "placed", Limits{VCPUs: 2, MemMiB: 256})
	if err != nil {
		t.Fatal(err)
	}
	if g == nil {
		t.Fatal("no cgroup created")
	}
	if err := cmd.Start(); err != nil {
		g.Remove()
		t.Fatal(err)
	}
	g.Close()
	defer func() {
		cmd.Process.Kill()
		cmd.Wait()
		g.Remove()
	}()

	// With abi 0 the command must not have been rewritten to the shim.
	if cmd.Path != "/bin/sleep" {
		t.Errorf("command rewritten without Landlock: %v", cmd.Args)
	}
	b, err := os.ReadFile("/proc/" + strconv.Itoa(cmd.Process.Pid) + "/cgroup")
	if err != nil {
		t.Fatal(err)
	}
	got := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(string(b)), "0::"))
	want := strings.TrimPrefix(g.path, mustMount(t))
	if got != want {
		t.Errorf("child is in cgroup %q, want %q", got, want)
	}
}

// TestCgroupRemovedWhenProcessExits: leaves must not pile up, because the
// startup Sweep is only a backstop.
func TestCgroupRemovedWhenProcessExits(t *testing.T) {
	c := testCgroups(t)
	g, err := c.Create("transient", Limits{VCPUs: 1, MemMiB: 128})
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("/bin/true")
	cmd.SysProcAttr = inCgroup(g)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	g.Close()
	cmd.Wait()
	g.Remove()
	if _, err := os.Stat(g.path); !os.IsNotExist(err) {
		t.Errorf("cgroup %s still exists after the process exited (%v)", g.path, err)
	}
}

// TestSweepLeavesLiveCgroups: a second wispd's Sweep must not delete a
// cgroup that still holds a running VMM. (rmdir on a populated cgroup is EBUSY,
// so this is really a check that Sweep does not try to empty one first.)
func TestSweepLeavesLiveCgroups(t *testing.T) {
	c := testCgroups(t)
	g, err := c.Create("live", Limits{VCPUs: 1, MemMiB: 128})
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("/bin/sleep", "30")
	cmd.SysProcAttr = inCgroup(g)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	g.Close()
	defer func() {
		cmd.Process.Kill()
		cmd.Wait()
		g.Remove()
	}()

	c.Sweep()
	if _, err := os.Stat(g.path); err != nil {
		t.Errorf("Sweep removed a cgroup with a live process: %v", err)
	}
}

// TestOpenDoesNotSweep pins down an ordering that is easy to get wrong and
// silent when it is: Open must leave stale leaves alone, because at startup the
// orphaned VMMs from the previous wispd are still running and a populated
// cgroup cannot be removed. Only SweepStale, called after vmm.ReapOrphan, may
// clear them.
func TestOpenDoesNotSweep(t *testing.T) {
	name := "wisp-test-sweep-" + strconv.Itoa(os.Getpid())
	first, err := openCgroups(name)
	if err != nil {
		t.Skipf("no delegated cgroup v2 subtree here: %v", err)
	}
	defer os.Remove(first.root)
	stale := filepath.Join(first.root, "left-behind")
	if err := os.Mkdir(stale, 0o755); err != nil {
		t.Fatal(err)
	}
	defer os.Remove(stale)

	c, err := Open(ModeBestEffort, name, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stale); err != nil {
		t.Fatalf("Open swept a stale leaf; an orphan's cgroup would be lost before it is reaped: %v", err)
	}
	c.SweepStale()
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Errorf("SweepStale left %s behind (%v)", stale, err)
	}
}

// TestOpenCgroupsWalksUp documents the discovery rule that matters on a real
// desktop: the nearest ancestor is not necessarily the delegated one. On
// Ubuntu 25.10 app.slice has memory and pids but not cpu, so openCgroups has
// to keep walking to user@<uid>.service.
func TestOpenCgroupsWalksUp(t *testing.T) {
	c := testCgroups(t)
	if missing := missingControllers(filepath.Join(filepath.Dir(c.root), "cgroup.subtree_control")); len(missing) > 0 {
		t.Errorf("chosen parent %s is missing %v", filepath.Dir(c.root), missing)
	}
	if missing := missingControllers(filepath.Join(c.root, "cgroup.subtree_control")); len(missing) > 0 {
		t.Errorf("our own subtree %s does not enable %v for its children", c.root, missing)
	}
}

func mustMount(t *testing.T) string {
	t.Helper()
	m, err := cgroup2Mount()
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// inCgroup places a child directly in g, the way Start does.
func inCgroup(g *Cgroup) *syscall.SysProcAttr {
	return &syscall.SysProcAttr{UseCgroupFD: true, CgroupFD: g.FD()}
}
