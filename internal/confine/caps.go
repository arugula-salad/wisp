package confine

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Caps are hard limits on the daemon's whole cgroup subtree, i.e. on every VM
// it runs, taken together. The per-VM leaves (Create) bound one VM each; these
// bound their sum. The subtree is a sibling of the daemon's own (systemd unit)
// cgroup, so a MemoryMax= on the unit does not reach the VMs: this does.
//
// The zero value writes nothing, so an install that does not ask for caps is
// left exactly as it was. The subtree outlives the daemon, though, so a cap
// once written stays until it is overwritten: clear it with MemoryMax
// MemoryUnlimited ("max") and CPUWeight 100, the kernel's defaults.
type Caps struct {
	// MemoryMax is memory.max in bytes: 0 leaves it alone, MemoryUnlimited
	// writes "max".
	MemoryMax int64
	// CPUWeight is cpu.weight, 1-10000 (the kernel's default is 100): 0 leaves it alone.
	CPUWeight int
}

// MemoryUnlimited is Caps.MemoryMax for an explicit "max" (no limit).
const MemoryUnlimited int64 = -1

// minMemoryMax keeps a typo ("32" for 32 GiB) from starving every VM. One VM's
// own leaf is already guest RAM + 448 MiB.
const minMemoryMax = 256 << 20

// Set reports whether any cap is requested.
func (c Caps) Set() bool { return c.MemoryMax != 0 || c.CPUWeight != 0 }

// Validate checks the ranges the kernel accepts.
func (c Caps) Validate() error {
	if c.MemoryMax != 0 && c.MemoryMax != MemoryUnlimited && c.MemoryMax < minMemoryMax {
		return fmt.Errorf("cgroup memory max %d bytes is below the 256M minimum", c.MemoryMax)
	}
	if c.CPUWeight < 0 || c.CPUWeight > 10000 {
		return fmt.Errorf("cgroup cpu weight must be 1-10000 (0 = leave unset), not %d", c.CPUWeight)
	}
	return nil
}

// String is the human summary used in Describe and the startup log.
func (c Caps) String() string {
	var parts []string
	switch {
	case c.MemoryMax == MemoryUnlimited:
		parts = append(parts, "memory.max=max")
	case c.MemoryMax > 0:
		parts = append(parts, "memory.max="+FormatBytes(c.MemoryMax))
	}
	if c.CPUWeight > 0 {
		parts = append(parts, "cpu.weight="+strconv.Itoa(c.CPUWeight))
	}
	return strings.Join(parts, " ")
}

// ParseMemoryMax reads a --cgroup-memory-max value: bytes, or a number with a
// binary suffix K, M, G or T (optionally followed by "i", "iB" or "B", any
// case), or "max" for no limit. Empty is 0: leave memory.max as it is.
func ParseMemoryMax(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, nil
	}
	if strings.EqualFold(s, "max") {
		return MemoryUnlimited, nil
	}
	u := strings.ToUpper(s)
	u = strings.TrimSuffix(u, "B")
	u = strings.TrimSuffix(u, "I")
	shift := 0
	if n := len(u); n > 0 {
		switch u[n-1] {
		case 'K':
			shift = 10
		case 'M':
			shift = 20
		case 'G':
			shift = 30
		case 'T':
			shift = 40
		}
		if shift > 0 {
			u = u[:n-1]
		}
	}
	if u == "" || strings.ContainsAny(u, "+-") {
		return 0, fmt.Errorf("cgroup memory max %q: want bytes or a size like 32G, or \"max\"", s)
	}
	n, err := strconv.ParseInt(u, 10, 64)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("cgroup memory max %q: want bytes or a size like 32G, or \"max\"", s)
	}
	if n > (1<<62)>>shift {
		return 0, fmt.Errorf("cgroup memory max %q is too large", s)
	}
	n <<= shift
	if err := (Caps{MemoryMax: n}).Validate(); err != nil {
		return 0, err
	}
	return n, nil
}

// FormatBytes renders a byte count with the largest binary suffix that divides it.
func FormatBytes(n int64) string {
	for _, u := range []struct {
		s     string
		shift uint
	}{{"T", 40}, {"G", 30}, {"M", 20}, {"K", 10}} {
		if n > 0 && n%(1<<u.shift) == 0 {
			return strconv.FormatInt(n>>u.shift, 10) + u.s
		}
	}
	return strconv.FormatInt(n, 10)
}

// apply writes the caps on the subtree root. memory.max goes first: if the
// write of cpu.weight then fails, the memory bound is still in force.
func (c *Cgroups) apply(caps Caps) error {
	if caps.MemoryMax != 0 {
		v := "max"
		if caps.MemoryMax > 0 {
			v = strconv.FormatInt(caps.MemoryMax, 10)
		}
		if err := os.WriteFile(filepath.Join(c.root, "memory.max"), []byte(v), 0o644); err != nil {
			return fmt.Errorf("cgroup cap memory.max on %s: %w", c.root, err)
		}
	}
	if caps.CPUWeight != 0 {
		if err := os.WriteFile(filepath.Join(c.root, "cpu.weight"), []byte(strconv.Itoa(caps.CPUWeight)), 0o644); err != nil {
			return fmt.Errorf("cgroup cap cpu.weight on %s: %w", c.root, err)
		}
	}
	return nil
}

// SubtreeStatus is what the kernel holds for the daemon's subtree, for
// `wispd status`: whatever wrote it, so a cap left by an earlier run shows.
type SubtreeStatus struct {
	Path string `json:"path"`
	// MemoryMax is memory.max in bytes, 0 for "max" (no limit).
	MemoryMax int64 `json:"memory_max_bytes"`
	// MemoryCurrent is what every VM under it is charged together.
	MemoryCurrent int64 `json:"memory_current_bytes"`
	CPUWeight     int   `json:"cpu_weight"`
}

// Subtree reads the subtree's caps and usage; nil when there is no subtree.
func (c *Confiner) Subtree() *SubtreeStatus {
	if c == nil || c.cg == nil {
		return nil
	}
	read := func(name string) string {
		b, err := os.ReadFile(filepath.Join(c.cg.root, name))
		if err != nil {
			return ""
		}
		return strings.TrimSpace(string(b))
	}
	if _, err := os.Stat(c.cg.root); err != nil {
		return nil
	}
	st := &SubtreeStatus{Path: c.cg.root}
	if v := read("memory.max"); v != "max" {
		st.MemoryMax, _ = strconv.ParseInt(v, 10, 64)
	}
	st.MemoryCurrent, _ = strconv.ParseInt(read("memory.current"), 10, 64)
	st.CPUWeight, _ = strconv.Atoi(read("cpu.weight"))
	return st
}

var errCapsWithoutCgroups = errors.New("cgroup caps requested but no cgroup subtree is available")
