// Package confine sandboxes each Firecracker VMM without root.
//
// Firecracker already runs behind its own seccomp filter, which is what keeps a
// compromised VMM from reaching other processes (no ptrace, no
// process_vm_readv). This package adds the two things that filter does not
// cover, using only what an unprivileged user is given:
//
//   - A Landlock domain limited to the VM's own machine directory plus the
//     handful of read-only files Firecracker opens. Another sprite's disk, the
//     API token and the rest of $HOME become EACCES.
//   - A delegated cgroup v2 leaf per VM, so CPU, memory and process count are
//     capped by the kernel.
//
// Landlock has to be applied between fork and exec, and Go's os/exec offers no
// hook there (nothing may run in the child of a threaded runtime). So wispd
// re-execs itself as a shim: the parent builds the argv, the shim applies the
// domain and then execve's Firecracker, which inherits it. The shim must exec
// rather than spawn, so the pid the parent knows stays the Firecracker pid —
// vmm.ReapOrphan, cmd.Wait and the process group all depend on that.
package confine

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
)

// ShimArg is the hidden first argument that turns a wispd exec into the
// confinement shim.
const ShimArg = "__confine"

// errUnsupported marks a confinement feature this kernel or host does not
// offer. In ModeBestEffort it is reported and skipped; in ModeStrict it is fatal.
var errUnsupported = errors.New("unsupported")

// Mode selects how hard a missing confinement feature is.
type Mode int

const (
	// ModeBestEffort applies everything the kernel supports and logs the rest.
	ModeBestEffort Mode = iota
	// ModeStrict refuses to start unless both Landlock and cgroup limits work.
	ModeStrict
	// ModeOff runs Firecracker directly, as before this package existed.
	ModeOff
)

// ParseMode reads the WISP_CONFINE setting.
func ParseMode(s string) (Mode, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "best-effort":
		return ModeBestEffort, nil
	case "strict":
		return ModeStrict, nil
	case "off":
		return ModeOff, nil
	}
	return 0, fmt.Errorf("confine mode must be \"strict\", \"best-effort\" or \"off\", not %q", s)
}

// Spec is the set of paths one VMM may touch. Everything else is denied.
type Spec struct {
	// Dir is the machine directory: the VMM's cwd and the only place it may
	// write. It holds the disk, the console log, the snapshot files and the
	// API and vsock unix sockets.
	Dir string `json:"dir"`
	// Exec is the Firecracker binary. The shim needs to execute it from inside
	// the domain, so it is granted read+execute.
	Exec string `json:"exec"`
	// ReadOnly are files the VMM opens for reading: the guest kernel, the
	// initrd, and /etc/localtime (Firecracker reads it to stamp its logs).
	ReadOnly []string `json:"read_only,omitempty"`
	// Devices are the character devices it opens and ioctls: /dev/kvm always,
	// and /dev/net/tun when the sprite has a NIC.
	Devices []string `json:"devices,omitempty"`
	// Strict fails the launch if the domain cannot be applied, instead of
	// running Firecracker unconfined.
	Strict bool `json:"strict,omitempty"`
}

// Confiner applies the confinement this host actually supports.
type Confiner struct {
	mode Mode
	abi  int      // Landlock ABI, 0 if unavailable
	cg   *Cgroups // nil if no delegated cgroup subtree
	// notes records what could not be enabled, for the startup log.
	notes []string
}

// Open probes the host and prepares the cgroup subtree. It returns nil for
// ModeOff. In ModeStrict a missing feature is an error; in ModeBestEffort it is
// recorded in Describe.
func Open(mode Mode, name string) (*Confiner, error) {
	if mode == ModeOff {
		return nil, nil
	}
	c := &Confiner{mode: mode, abi: abiVersion()}
	if c.abi < 1 {
		c.notes = append(c.notes, "landlock unavailable (kernel lacks it, or it is not in the boot-time LSM list)")
	} else if c.abi < abiScope {
		c.notes = append(c.notes, fmt.Sprintf("landlock abi %d predates signal/unix scoping (needs %d)", c.abi, abiScope))
	}
	cg, err := openCgroups(name)
	if err != nil {
		c.notes = append(c.notes, err.Error())
	} else {
		c.cg = cg
	}
	if mode == ModeStrict && len(c.notes) > 0 {
		return nil, fmt.Errorf("confine=strict: %s", strings.Join(c.notes, "; "))
	}
	return c, nil
}

// Describe is the one-line startup summary: exactly what is enforced, so the
// claim in docs/security.md can be checked against a running wispd.
func (c *Confiner) Describe() string {
	if c == nil {
		return "off (firecracker runs with only its own seccomp filter)"
	}
	cgroups := "unavailable"
	if c.cg != nil {
		cgroups = strings.Join(wantControllers, ",") + " at " + c.cg.root
	}
	s := fmt.Sprintf("landlock: %s; cgroup: %s", landlockSummary(c.abi), cgroups)
	if len(c.notes) > 0 {
		s += "; degraded: " + strings.Join(c.notes, "; ")
	}
	return s
}

// Enforced reports whether anything is actually being enforced, so callers can
// avoid claiming confinement they do not have.
func (c *Confiner) Enforced() bool {
	return c != nil && (c.abi >= 1 || c.cg != nil)
}

// SweepStale removes cgroup leaves left by a wispd that died without
// cleaning up. It must be called *after* orphaned VMMs have been reaped: a
// cgroup that still holds a process cannot be removed, so an earlier sweep
// silently leaves every leaf whose VMM outlived its wispd.
func (c *Confiner) SweepStale() {
	if c != nil && c.cg != nil {
		c.cg.Sweep()
	}
}

// Start rewrites cmd to launch through the confinement shim and, if a cgroup
// subtree is available, creates the VM's leaf and has the child born directly
// inside it. The returned Cgroup must be Removed when the VMM exits; it is nil
// when cgroup limits are unavailable.
//
// cmd.Path/cmd.Args must already be the Firecracker command line; Start
// replaces them with the shim's, leaving Dir, Env and the file handles alone.
func (c *Confiner) Start(cmd *exec.Cmd, spec Spec, id string, lim Limits) (*Cgroup, error) {
	if c == nil {
		return nil, nil
	}
	if c.abi >= 1 {
		spec.Exec = cmd.Path
		spec.Strict = c.mode == ModeStrict
		blob, err := json.Marshal(spec)
		if err != nil {
			return nil, err
		}
		self, err := os.Executable()
		if err != nil {
			return nil, fmt.Errorf("confine: locate wispd: %w", err)
		}
		cmd.Path = self
		cmd.Args = append([]string{"wispd", ShimArg, string(blob)}, cmd.Args...)
	}
	if c.cg == nil {
		return nil, nil
	}
	g, err := c.cg.Create(id, lim)
	if err != nil {
		if c.mode == ModeStrict {
			return nil, err
		}
		return nil, nil
	}
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	// CLONE_INTO_CGROUP: the VMM is born inside its limits. Go uses clone3 for
	// this and reports a failure rather than silently starting it unconfined.
	cmd.SysProcAttr.UseCgroupFD = true
	cmd.SysProcAttr.CgroupFD = g.FD()
	return g, nil
}

// RunShim is the child half: it applies the Landlock domain described by args[0]
// and execs args[1:]. It never returns.
func RunShim(args []string) {
	fail := func(err error) {
		fmt.Fprintf(os.Stderr, "wisp confine: %v\n", err)
		os.Exit(126)
	}
	if len(args) < 2 {
		fail(errors.New("usage: wispd " + ShimArg + " <spec-json> <argv...>"))
	}
	var spec Spec
	if err := json.Unmarshal([]byte(args[0]), &spec); err != nil {
		fail(fmt.Errorf("bad spec: %w", err))
	}
	if err := applyLandlock(spec, abiVersion()); err != nil {
		// Without Strict, a kernel that cannot sandbox still gets its VM; the
		// startup log has already said so.
		if spec.Strict {
			fail(err)
		}
		fmt.Fprintf(os.Stderr, "wisp confine: running unconfined: %v\n", err)
	}
	if err := syscall.Exec(args[1], args[1:], os.Environ()); err != nil {
		fail(fmt.Errorf("exec %s: %w", args[1], err))
	}
}
