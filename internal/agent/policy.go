package agent

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"

	"golang.org/x/sys/unix"
)

// Policy is the part of a sprite's privileges and resources policies that the
// guest enforces. spritesd owns it (a sprite must not be able to rewrite its
// own policy): it arrives on the kernel command line at boot, so services are
// confined from their first start, and through /internal/policy afterwards.
//
// It binds processes started after it was set. It does not bind the API
// caller: the filesystem API still acts as root.
type Policy struct {
	// Profile bounds the capabilities a process can ever hold, which is what
	// `sudo` hands out: "minimal" none, "standard" the usual container set,
	// "privileged" or "" all of them.
	Profile string `json:"profile,omitempty"`
	// NoNewPrivs sets no_new_privs, so setuid binaries (sudo, su) stop working.
	NoNewPrivs bool `json:"no_new_privs,omitempty"`
	// MemoryLimitMB caps the memory of all exec sessions and services together.
	MemoryLimitMB int `json:"memory_limit_mb,omitempty"`
}

// profileCaps lists the capabilities each restricting profile keeps.
var profileCaps = map[string][]uintptr{
	"minimal": {},
	"standard": {unix.CAP_CHOWN, unix.CAP_DAC_OVERRIDE, unix.CAP_FSETID, unix.CAP_FOWNER, unix.CAP_MKNOD,
		unix.CAP_NET_RAW, unix.CAP_SETGID, unix.CAP_SETUID, unix.CAP_SETFCAP, unix.CAP_SETPCAP,
		unix.CAP_NET_BIND_SERVICE, unix.CAP_SYS_CHROOT, unix.CAP_KILL, unix.CAP_AUDIT_WRITE},
}

// There is one agent per guest, and both exec sessions and services launch
// through it, so the policy is package state rather than threaded through both.
var confine struct {
	mu        sync.Mutex
	policy    Policy
	cgroup    *os.File // the workload cgroup's directory; nil where there is none (tests on the host)
	statePath string
}

const workloadCgroup = "/sys/fs/cgroup/sprite"

// InitPolicy must run before anything is launched. statePath (on a tmpfs)
// remembers the last pushed policy across a restart of the agent process, which
// would otherwise fall back to the possibly laxer one from boot.
func InitPolicy(boot Policy, statePath string) {
	confine.mu.Lock()
	confine.statePath = statePath
	if os.Getuid() == 0 {
		// The memory controller must be delegated before the child cgroup has a memory.max.
		os.WriteFile("/sys/fs/cgroup/cgroup.subtree_control", []byte("+memory"), 0o644)
		os.Mkdir(workloadCgroup, 0o755)
		if f, err := os.Open(workloadCgroup); err == nil {
			confine.cgroup = f
		} else {
			log.Printf("no workload cgroup; memory limits will not be enforced: %v", err)
		}
	}
	confine.mu.Unlock()

	p := boot
	if b, err := os.ReadFile(statePath); err == nil {
		json.Unmarshal(b, &p)
	}
	if err := SetPolicy(p); err != nil {
		log.Printf("policy: %v", err)
	}
}

func SetPolicy(p Policy) error {
	if _, ok := profileCaps[p.Profile]; !ok && p.Profile != "" && p.Profile != "privileged" {
		return fmt.Errorf("unknown privileges profile %q", p.Profile)
	}
	confine.mu.Lock()
	defer confine.mu.Unlock()
	if confine.cgroup != nil {
		limit := "max"
		if p.MemoryLimitMB > 0 {
			limit = strconv.Itoa(p.MemoryLimitMB << 20)
		}
		if err := os.WriteFile(filepath.Join(workloadCgroup, "memory.max"), []byte(limit), 0o644); err != nil {
			return fmt.Errorf("memory limit: %w", err)
		}
	}
	confine.policy = p
	if confine.statePath != "" {
		b, _ := json.Marshal(p)
		os.WriteFile(confine.statePath, b, 0o600)
	}
	return nil
}

func (s *Server) handlePolicy(w http.ResponseWriter, r *http.Request) {
	var p Policy
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&p); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request", "invalid JSON body")
		return
	}
	if err := SetPolicy(p); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// launch calls start, which must fork the child configured by attr, under the
// current policy. A policy that cannot be applied fails the launch: running
// the command unconfined instead would be worse than not running it.
func launch(attr *syscall.SysProcAttr, start func() error) error {
	confine.mu.Lock()
	p, cg := confine.policy, confine.cgroup
	confine.mu.Unlock()
	if cg != nil {
		// Born inside the cgroup (clone3), so there is no window in which the
		// child could allocate, or fork, outside the limit.
		attr.UseCgroupFD, attr.CgroupFD = true, int(cg.Fd())
	}
	keep, bounded := profileCaps[p.Profile]
	if !bounded && !p.NoNewPrivs {
		return start()
	}
	errc := make(chan error, 1)
	go func() {
		// no_new_privs and the bounding set are per-thread, inherited by what the
		// thread forks, and irreversible. So taint a thread of our own and never
		// unlock it: the runtime destroys it when this goroutine returns.
		runtime.LockOSThread()
		err := restrictThread(keep, bounded, p.NoNewPrivs)
		if err == nil {
			err = start()
		}
		errc <- err
	}()
	return <-errc
}

func restrictThread(keep []uintptr, bounded, noNewPrivs bool) error {
	if bounded {
		last := 40
		if b, err := os.ReadFile("/proc/sys/kernel/cap_last_cap"); err == nil {
			last, _ = strconv.Atoi(strings.TrimSpace(string(b)))
		}
		kept := map[uintptr]bool{}
		for _, c := range keep {
			kept[c] = true
		}
		for c := uintptr(0); c <= uintptr(last); c++ {
			if !kept[c] {
				if err := unix.Prctl(unix.PR_CAPBSET_DROP, c, 0, 0, 0); err != nil {
					return fmt.Errorf("privileges policy: drop capability %d: %w", c, err)
				}
			}
		}
	}
	if noNewPrivs {
		if err := unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0); err != nil {
			return fmt.Errorf("privileges policy: no_new_privs: %w", err)
		}
	}
	return nil
}
