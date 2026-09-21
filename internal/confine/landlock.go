package confine

import (
	"debug/elf"
	"fmt"
	"os"
	"runtime"
	"strings"
	"unsafe"

	"golang.org/x/sys/unix"
)

// Landlock rule types (enum landlock_rule_type); x/sys has the access-right
// constants but not these.
const (
	ruleTypePathBeneath = 1
	ruleTypeNetPort     = 2
)

// Landlock grew one access right at a time, and a ruleset that names a right
// the running kernel does not know is rejected outright. Each entry is the
// first ABI version that supports the feature, so a ruleset can be trimmed to
// whatever this kernel actually implements.
const (
	abiRefer     = 2 // LANDLOCK_ACCESS_FS_REFER
	abiTruncate  = 3 // LANDLOCK_ACCESS_FS_TRUNCATE
	abiNet       = 4 // LANDLOCK_ACCESS_NET_*
	abiIoctlDev  = 5 // LANDLOCK_ACCESS_FS_IOCTL_DEV
	abiScope     = 6 // LANDLOCK_SCOPE_*
	abiThreadSet = 8 // LANDLOCK_RESTRICT_SELF_TSYNC
)

// abiVersion reports the Landlock ABI this kernel implements, or 0 if Landlock
// is unavailable (not built in, or not in the boot-time LSM list).
func abiVersion() int {
	v, _, errno := unix.Syscall(unix.SYS_LANDLOCK_CREATE_RULESET, 0, 0, unix.LANDLOCK_CREATE_RULESET_VERSION)
	if errno != 0 {
		return 0
	}
	return int(v)
}

// fsRights is every filesystem access right up to abi. A sandbox must *handle*
// all of them and then grant back the few it needs: a right left unhandled is
// not restricted at all, so an incomplete mask is an open door, not a tighter
// one.
func fsRights(abi int) uint64 {
	r := uint64(unix.LANDLOCK_ACCESS_FS_EXECUTE |
		unix.LANDLOCK_ACCESS_FS_WRITE_FILE |
		unix.LANDLOCK_ACCESS_FS_READ_FILE |
		unix.LANDLOCK_ACCESS_FS_READ_DIR |
		unix.LANDLOCK_ACCESS_FS_REMOVE_DIR |
		unix.LANDLOCK_ACCESS_FS_REMOVE_FILE |
		unix.LANDLOCK_ACCESS_FS_MAKE_CHAR |
		unix.LANDLOCK_ACCESS_FS_MAKE_DIR |
		unix.LANDLOCK_ACCESS_FS_MAKE_REG |
		unix.LANDLOCK_ACCESS_FS_MAKE_SOCK |
		unix.LANDLOCK_ACCESS_FS_MAKE_FIFO |
		unix.LANDLOCK_ACCESS_FS_MAKE_BLOCK |
		unix.LANDLOCK_ACCESS_FS_MAKE_SYM)
	if abi >= abiRefer {
		r |= unix.LANDLOCK_ACCESS_FS_REFER
	}
	if abi >= abiTruncate {
		r |= unix.LANDLOCK_ACCESS_FS_TRUNCATE
	}
	if abi >= abiIoctlDev {
		r |= unix.LANDLOCK_ACCESS_FS_IOCTL_DEV
	}
	return r
}

// The rights each kind of path in a Spec is granted. They are supersets trimmed
// against the kernel's ABI in ruleset(); see Spec for what needs what.
const (
	rightsDir = unix.LANDLOCK_ACCESS_FS_READ_FILE | unix.LANDLOCK_ACCESS_FS_WRITE_FILE |
		unix.LANDLOCK_ACCESS_FS_READ_DIR | unix.LANDLOCK_ACCESS_FS_MAKE_REG |
		unix.LANDLOCK_ACCESS_FS_MAKE_SOCK | unix.LANDLOCK_ACCESS_FS_TRUNCATE
	rightsReadOnly = unix.LANDLOCK_ACCESS_FS_READ_FILE
	rightsExec     = unix.LANDLOCK_ACCESS_FS_READ_FILE | unix.LANDLOCK_ACCESS_FS_EXECUTE
	rightsDevice   = unix.LANDLOCK_ACCESS_FS_READ_FILE | unix.LANDLOCK_ACCESS_FS_WRITE_FILE |
		unix.LANDLOCK_ACCESS_FS_IOCTL_DEV
)

// applyLandlock puts the calling process in a Landlock domain that allows only
// the paths in spec, denies all TCP, and cannot signal or reach abstract unix
// sockets outside itself. Everything else on the filesystem — other sprites'
// machine directories, the API token, $HOME — becomes EACCES.
//
// The domain is inherited across execve, which is the whole point: the caller
// is a shim that exec's Firecracker immediately afterwards.
func applyLandlock(spec Spec, abi int) error {
	if abi < 1 {
		return errUnsupported
	}
	rulesetFD, err := ruleset(spec, abi)
	if err != nil {
		return err
	}
	defer unix.Close(rulesetFD)

	// A Landlock domain applies to the calling thread. Go can migrate a
	// goroutine between threads at any time, so pin to one; the execve that
	// follows discards every other thread and keeps this one, which carries the
	// domain. On ABI 8 TSYNC applies it to the whole process, which is stricter
	// and survives a caller that does not exec.
	runtime.LockOSThread()
	if err := unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0); err != nil {
		return fmt.Errorf("landlock: set no_new_privs: %w", err)
	}
	var flags uintptr
	if abi >= abiThreadSet {
		flags = unix.LANDLOCK_RESTRICT_SELF_TSYNC
	}
	if _, _, errno := unix.Syscall(unix.SYS_LANDLOCK_RESTRICT_SELF, uintptr(rulesetFD), flags, 0); errno != 0 {
		return fmt.Errorf("landlock: restrict_self: %w", errno)
	}
	return nil
}

// ruleset builds the Landlock ruleset fd for spec.
func ruleset(spec Spec, abi int) (int, error) {
	attr := unix.LandlockRulesetAttr{Access_fs: fsRights(abi)}
	// Firecracker speaks no TCP at all, so handling both network rights and
	// granting no port denies the lot.
	if abi >= abiNet {
		attr.Access_net = unix.LANDLOCK_ACCESS_NET_BIND_TCP | unix.LANDLOCK_ACCESS_NET_CONNECT_TCP
	}
	if abi >= abiScope {
		attr.Scoped = unix.LANDLOCK_SCOPE_ABSTRACT_UNIX_SOCKET | unix.LANDLOCK_SCOPE_SIGNAL
	}
	// The attr struct grew with the ABI; a kernel that predates a field rejects
	// the larger size with E2BIG.
	size := unsafe.Sizeof(attr)
	switch {
	case abi < abiNet:
		size = 8 // access_fs only
	case abi < abiScope:
		size = 16 // + access_net
	}

	fd, _, errno := unix.Syscall(unix.SYS_LANDLOCK_CREATE_RULESET, uintptr(unsafe.Pointer(&attr)), size, 0)
	if errno != 0 {
		return -1, fmt.Errorf("landlock: create_ruleset: %w", errno)
	}
	rulesetFD := int(fd)

	handled := attr.Access_fs
	add := func(path string, rights uint64) error {
		// O_PATH is enough to name a file for Landlock and does not need the
		// read permission the rule is about to grant.
		pathFD, err := unix.Open(path, unix.O_PATH|unix.O_CLOEXEC, 0)
		if err != nil {
			return fmt.Errorf("landlock: open %s: %w", path, err)
		}
		defer unix.Close(pathFD)
		rule := unix.LandlockPathBeneathAttr{
			// Granting a right the ruleset does not handle is EINVAL.
			Allowed_access: rights & handled,
			Parent_fd:      int32(pathFD),
		}
		if _, _, errno := unix.Syscall6(unix.SYS_LANDLOCK_ADD_RULE, uintptr(rulesetFD),
			ruleTypePathBeneath, uintptr(unsafe.Pointer(&rule)), 0, 0, 0); errno != 0 {
			return fmt.Errorf("landlock: add rule for %s: %w", path, errno)
		}
		return nil
	}

	for path, rights := range spec.rules() {
		if err := add(path, rights); err != nil {
			unix.Close(rulesetFD)
			return -1, err
		}
	}
	return rulesetFD, nil
}

// rules is every path the domain allows, with the rights it gets. Paths that do
// not exist are skipped: /dev/net/tun is absent on a host without the network
// setup, and /etc/localtime is not universal.
func (s Spec) rules() map[string]uint64 {
	out := map[string]uint64{}
	put := func(path string, rights uint64, required bool) {
		if path == "" {
			return
		}
		if _, err := os.Stat(path); err != nil && !required {
			return
		}
		out[path] |= rights
	}
	put(s.Dir, rightsDir, true)
	put(s.Exec, rightsExec, true)
	for _, p := range s.ReadOnly {
		put(p, rightsReadOnly, false)
	}
	for _, p := range s.Devices {
		put(p, rightsDevice, false)
	}
	// A dynamically linked VMM needs its loader and libraries, or execve itself
	// fails with EACCES. Firecracker's own release build is static-pie and adds
	// nothing here; a distro-packaged build is not.
	for _, p := range loaderPaths(s.Exec) {
		put(p, rightsExec, false)
	}
	return out
}

// libDirs are where an ELF interpreter looks for shared objects. They are
// read-only system paths: widening the domain to them exposes no sprite data,
// no token and nothing under $HOME.
var libDirs = []string{"/lib", "/lib64", "/usr/lib", "/usr/lib64"}

// loaderPaths is the ELF interpreter of a dynamically linked binary plus the
// library directories it will search, or nothing at all if the binary is
// static.
func loaderPaths(binary string) []string {
	f, err := elf.Open(binary)
	if err != nil {
		return nil
	}
	defer f.Close()
	sec := f.Section(".interp")
	if sec == nil {
		return nil // statically linked
	}
	data, err := sec.Data()
	if err != nil {
		return nil
	}
	return append([]string{strings.TrimRight(string(data), "\x00")}, libDirs...)
}

// landlockSummary describes what a domain at this ABI actually enforces, for the
// startup log and the README's claim.
func landlockSummary(abi int) string {
	if abi < 1 {
		return "unavailable"
	}
	feats := []string{"fs"}
	if abi >= abiNet {
		feats = append(feats, "no-tcp")
	}
	if abi >= abiIoctlDev {
		feats = append(feats, "ioctl")
	}
	if abi >= abiScope {
		feats = append(feats, "scoped-signal", "scoped-unix")
	}
	if abi >= abiThreadSet {
		feats = append(feats, "tsync")
	}
	return fmt.Sprintf("abi=%d %s", abi, strings.Join(feats, "+"))
}
