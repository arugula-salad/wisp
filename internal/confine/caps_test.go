package confine

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// Value parsing needs no cgroups.
func TestParseMemoryMax(t *testing.T) {
	for _, c := range []struct {
		in   string
		want int64
	}{
		{"", 0},
		{"  ", 0},
		{"max", MemoryUnlimited},
		{"MAX", MemoryUnlimited},
		{"32G", 32 << 30},
		{"32g", 32 << 30},
		{"32GiB", 32 << 30},
		{"32GB", 32 << 30},
		{"512M", 512 << 20},
		{"1T", 1 << 40},
		{"262144K", 256 << 20},
		{"268435456", 256 << 20},
		{"34359738368", 32 << 30},
	} {
		got, err := ParseMemoryMax(c.in)
		if err != nil || got != c.want {
			t.Errorf("ParseMemoryMax(%q) = %d, %v; want %d", c.in, got, err, c.want)
		}
	}
	for _, in := range []string{"0", "-1G", "+4G", "G", "32X", "1.5G", "32 G", "abc", "100M", "32", "99999999999T"} {
		if got, err := ParseMemoryMax(in); err == nil {
			t.Errorf("ParseMemoryMax(%q) = %d, want an error", in, got)
		}
	}
}

func TestCapsValidate(t *testing.T) {
	for _, c := range []Caps{{}, {CPUWeight: 1}, {CPUWeight: 10000}, {MemoryMax: MemoryUnlimited}, {MemoryMax: 1 << 30, CPUWeight: 50}} {
		if err := c.Validate(); err != nil {
			t.Errorf("%+v: %v", c, err)
		}
	}
	for _, c := range []Caps{{CPUWeight: -1}, {CPUWeight: 10001}, {MemoryMax: 1 << 20}} {
		if err := c.Validate(); err == nil {
			t.Errorf("%+v validated", c)
		}
	}
	if (Caps{}).Set() {
		t.Error("zero Caps claims to be set")
	}
	if got := (Caps{MemoryMax: 32 << 30, CPUWeight: 50}).String(); got != "memory.max=32G cpu.weight=50" {
		t.Errorf("String() = %q", got)
	}
}

// Without a cgroup subtree, a requested cap is fatal in strict mode and only a
// warning otherwise; with no cap requested, best-effort is unchanged.
func TestCapsWithoutCgroups(t *testing.T) {
	saved := openSubtree
	openSubtree = func(string) (*Cgroups, error) { return nil, fmt.Errorf("%w: test host has none", errUnsupported) }
	t.Cleanup(func() { openSubtree = saved })

	caps := Caps{MemoryMax: 2 << 30, CPUWeight: 50}
	_, err := Open(ModeStrict, "wisp-test-nocg", caps, nil)
	if err == nil || !strings.Contains(err.Error(), "cgroup caps requested") {
		t.Errorf("strict with caps and no cgroups: err = %v, want a refusal naming the caps", err)
	}
	c, err := Open(ModeBestEffort, "wisp-test-nocg", caps, nil)
	if err != nil || c == nil {
		t.Fatalf("best-effort with caps and no cgroups: %v, %v; want a confiner and a warning", c, err)
	}
	if !strings.Contains(c.Describe(), "cgroup caps requested") {
		t.Errorf("Describe() does not mention the missing caps: %s", c.Describe())
	}
	if c.Subtree() != nil {
		t.Error("Subtree() reports a subtree that does not exist")
	}
	if _, err := Open(ModeBestEffort, "wisp-test-nocg", Caps{CPUWeight: 20000}, nil); err == nil {
		t.Error("an out-of-range cap was accepted")
	}
	if c, err := Open(ModeOff, "wisp-test-nocg", caps, nil); c != nil || err != nil {
		t.Errorf("confine=off with caps = %v, %v; want nil, nil (and a warning)", c, err)
	}
}

func readCg(t *testing.T, dir, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return strings.TrimSpace(string(b))
}

// The caps land on the subtree root, the parent of every VM leaf.
func TestOpenWritesCaps(t *testing.T) {
	name := "wisp-test-caps-" + strconv.Itoa(os.Getpid())
	probe, err := openCgroups(name)
	if err != nil {
		t.Skipf("no delegated cgroup v2 subtree here: %v", err)
	}
	t.Cleanup(func() { os.Remove(probe.root) })

	// No caps: nothing written, the kernel's defaults stay.
	c, err := Open(ModeBestEffort, name, Caps{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := readCg(t, probe.root, "memory.max"); got != "max" {
		t.Errorf("memory.max = %q with no cap requested", got)
	}
	if got := readCg(t, probe.root, "cpu.weight"); got != "100" {
		t.Errorf("cpu.weight = %q with no cap requested", got)
	}
	if st := c.Subtree(); st == nil || st.MemoryMax != 0 || st.CPUWeight != 100 || st.Path != probe.root {
		t.Errorf("Subtree() = %+v", st)
	}

	c, err = Open(ModeBestEffort, name, Caps{MemoryMax: 2 << 30, CPUWeight: 50}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := readCg(t, probe.root, "memory.max"); got != strconv.Itoa(2<<30) {
		t.Errorf("memory.max = %q, want %d", got, 2<<30)
	}
	if got := readCg(t, probe.root, "cpu.weight"); got != "50" {
		t.Errorf("cpu.weight = %q, want 50", got)
	}
	if st := c.Subtree(); st == nil || st.MemoryMax != 2<<30 || st.CPUWeight != 50 {
		t.Errorf("Subtree() = %+v", st)
	}
	if !strings.Contains(c.Describe(), "memory.max=2G cpu.weight=50") {
		t.Errorf("Describe() = %s", c.Describe())
	}

	// A VM leaf under a capped subtree still gets its own limits.
	g, err := c.cg.Create("leaf", Limits{VCPUs: 1, MemMiB: 128})
	if err != nil {
		t.Fatal(err)
	}
	g.Remove()

	// "max" clears; an unset flag leaves what is there.
	if _, err := Open(ModeBestEffort, name, Caps{CPUWeight: 100}, nil); err != nil {
		t.Fatal(err)
	}
	if got := readCg(t, probe.root, "memory.max"); got != strconv.Itoa(2<<30) {
		t.Errorf("memory.max = %q after an Open that did not ask to change it", got)
	}
	if _, err := Open(ModeBestEffort, name, Caps{MemoryMax: MemoryUnlimited}, nil); err != nil {
		t.Fatal(err)
	}
	if got := readCg(t, probe.root, "memory.max"); got != "max" {
		t.Errorf("memory.max = %q after asking for max", got)
	}
}

// A cap that cannot be written is fatal under strict and a warning otherwise.
func TestCapsWriteFailure(t *testing.T) {
	gone := &Cgroups{root: filepath.Join(t.TempDir(), "missing")}
	saved := openSubtree
	openSubtree = func(string) (*Cgroups, error) { return gone, nil }
	t.Cleanup(func() { openSubtree = saved })

	caps := Caps{MemoryMax: 1 << 30}
	if c, err := Open(ModeBestEffort, "x", caps, nil); err != nil || !c.capsFailed {
		t.Errorf("best-effort: %v, %+v; want a confiner noting the failure", err, c)
	}
	if abiVersion() >= abiScope {
		// Only the caps can make strict fail here.
		if _, err := Open(ModeStrict, "x", caps, nil); err == nil || !strings.Contains(err.Error(), "memory.max") {
			t.Errorf("strict: err = %v, want a refusal about memory.max", err)
		}
	}
	if _, err := Open(ModeStrict, "x", Caps{}, nil); err != nil && strings.Contains(err.Error(), "memory.max") {
		t.Errorf("strict with no caps tried to write one: %v", err)
	}
}
