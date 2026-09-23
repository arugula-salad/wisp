package server

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/jhgaylor/wisp/internal/store"
	"github.com/jhgaylor/wisp/internal/vmm"
)

// The privileges and resources policies. wispd is the source of truth (a
// sprite cannot rewrite its own policy); the guest agent enforces them on the
// processes it launches, and the memory limit also sizes the VM.
//
// Enforced: the capability profile, noNewPrivileges, memory.limit_mb, and
// memory.autoscale (by the balloon; see autoscale.go). Stored and returned but
// not enforced: devices (there is no device cgroup filter in the guest).

const (
	// vmHeadroomMiB is guest RAM beyond the workload's limit, for the kernel and
	// the agent, so that the workload hits its own limit before a global OOM.
	vmHeadroomMiB = 128
	maxDevices    = 64
)

func (s *Server) registerPolicyLimits(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/sprites/{name}/policy/privileges", func(w http.ResponseWriter, r *http.Request) {
		if sp, ok := s.lookup(w, r); ok {
			writeJSON(w, http.StatusOK, orZero(sp.Privileges))
		}
	})
	mux.HandleFunc("GET /v1/sprites/{name}/policy/resources", func(w http.ResponseWriter, r *http.Request) {
		if sp, ok := s.lookup(w, r); ok {
			writeJSON(w, http.StatusOK, orZero(sp.Resources))
		}
	})
	mux.HandleFunc("POST /v1/sprites/{name}/policy/privileges", s.setPrivileges)
	mux.HandleFunc("POST /v1/sprites/{name}/policy/resources", s.setResources)
	mux.HandleFunc("DELETE /v1/sprites/{name}/policy/privileges", func(w http.ResponseWriter, r *http.Request) {
		s.storePolicy(w, r, "privileges", func(sp *store.Sprite) { sp.Privileges = nil })
	})
	mux.HandleFunc("DELETE /v1/sprites/{name}/policy/resources", func(w http.ResponseWriter, r *http.Request) {
		s.storePolicy(w, r, "resources", func(sp *store.Sprite) { sp.Resources = nil })
	})
}

// orZero renders an unset policy as an empty object rather than null.
func orZero[T any](p *T) *T {
	if p == nil {
		return new(T)
	}
	return p
}

func (s *Server) setPrivileges(w http.ResponseWriter, r *http.Request) {
	var p store.PrivilegesPolicy
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&p); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request", "invalid JSON body")
		return
	}
	switch p.Profile {
	case "", "minimal", "standard", "privileged":
	default:
		writeErr(w, http.StatusBadRequest, "bad_request", `profile must be "minimal", "standard" or "privileged"`)
		return
	}
	if len(p.Devices) > maxDevices {
		writeErr(w, http.StatusBadRequest, "bad_request", fmt.Sprintf("at most %d devices", maxDevices))
		return
	}
	if len(p.Devices) > 0 {
		s.log.Warn("privileges policy lists devices, which are stored but not enforced", "sprite", r.PathValue("name"))
	}
	s.storePolicy(w, r, "privileges", func(sp *store.Sprite) { sp.Privileges = &p })
}

func (s *Server) setResources(w http.ResponseWriter, r *http.Request) {
	var p store.ResourcesPolicy
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&p); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request", "invalid JSON body")
		return
	}
	if m := p.Memory; m != nil {
		if max := hostMemMiB() - vmHeadroomMiB; m.LimitMB < 1 || m.LimitMB > max {
			writeErr(w, http.StatusBadRequest, "bad_request", fmt.Sprintf("memory.limit_mb must be between 1 and %d on this host", max))
			return
		}
	}
	s.storePolicy(w, r, "resources", func(sp *store.Sprite) { sp.Resources = &p })
}

// storePolicy persists a policy change and, if the sprite is running, hands it
// to the guest now. A sleeping sprite is not woken: it gets the policy on wake.
func (s *Server) storePolicy(w http.ResponseWriter, r *http.Request, policy string, change func(*store.Sprite)) {
	sp, err := s.store.Update(r.PathValue("name"), func(sp *store.Sprite) {
		change(sp)
		sp.UpdatedAt = time.Now().UTC()
	})
	if err != nil {
		writeErr(w, http.StatusNotFound, "not_found", "sprite not found")
		return
	}
	s.life.emit(sp, "policy.changed", map[string]any{"policy": policy})
	rt := s.life.rt(sp.ID)
	rt.mu.Lock()
	if rt.m != nil {
		err = s.life.pushPolicy(r.Context(), rt.m, sp.Name)
	}
	rt.mu.Unlock()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "policy saved but not applied to the running sprite: "+err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// guestPolicy is agent.Policy: the part of the policies the guest enforces.
type guestPolicy struct {
	Profile       string `json:"profile,omitempty"`
	NoNewPrivs    bool   `json:"no_new_privs,omitempty"`
	MemoryLimitMB int    `json:"memory_limit_mb,omitempty"`
}

func guestPolicyFor(sp store.Sprite) guestPolicy {
	var p guestPolicy
	if sp.Privileges != nil {
		p.Profile, p.NoNewPrivs = sp.Privileges.Profile, sp.Privileges.NoNewPrivileges
	}
	if sp.Resources != nil && sp.Resources.Memory != nil {
		p.MemoryLimitMB = sp.Resources.Memory.LimitMB
	}
	return p
}

// pushPolicy sends the stored policy to a running guest. It reads the store
// rather than take a Sprite, because callers' copies can predate a policy change.
func (l *Lifecycle) pushPolicy(ctx context.Context, m *vmm.Machine, name string) error {
	sp, err := l.store.Get(name)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return agentCall(ctx, m, http.MethodPost, "/internal/policy", guestPolicyFor(sp), nil)
}

// policyResumed brings a sprite that just woke from a snapshot up to date: the
// policy may have changed while it slept. (A cold boot has it on the kernel
// command line.) False means the guest cannot be trusted to enforce the policy
// and must be cold booted instead, at the cost of its memory state. A guest
// that fails to take an empty policy is let through: that is a snapshot from
// before agents knew about policies, and it has nothing to enforce.
func (l *Lifecycle) policyResumed(ctx context.Context, m *vmm.Machine, sp store.Sprite) bool {
	err := l.pushPolicy(ctx, m, sp.Name)
	if err == nil {
		return true
	}
	if cur, gerr := l.store.Get(sp.Name); gerr == nil && guestPolicyFor(cur) == (guestPolicy{}) {
		l.log.Warn("guest did not accept the (empty) policy", "sprite", sp.Name, "err", err)
		return true
	}
	l.log.Warn("guest cannot enforce the policy; discarding warm state to boot a current agent", "sprite", sp.Name, "err", err)
	return false
}

// applyPolicy shapes a cold boot: the policy rides the kernel command line so
// it is in force before the guest starts its services, and a memory limit
// sizes the VM, which is the one bound that root inside the guest cannot lift.
func applyPolicy(cfg *vmm.Config, sp store.Sprite) {
	p := guestPolicyFor(sp)
	if p.Profile != "" {
		cfg.BootArgs = append(cfg.BootArgs, "sprite.profile="+p.Profile)
	}
	if p.NoNewPrivs {
		cfg.BootArgs = append(cfg.BootArgs, "sprite.nnp=1")
	}
	if p.MemoryLimitMB > 0 {
		cfg.BootArgs = append(cfg.BootArgs, "sprite.memlimit="+strconv.Itoa(p.MemoryLimitMB))
		cfg.MemMiB = p.MemoryLimitMB + vmHeadroomMiB
	}
}

func hostMemMiB() int {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0
	}
	defer f.Close()
	for sc := bufio.NewScanner(f); sc.Scan(); {
		if fields := strings.Fields(sc.Text()); len(fields) >= 2 && fields[0] == "MemTotal:" {
			kb, _ := strconv.Atoi(fields[1])
			return kb / 1024
		}
	}
	return 0
}
