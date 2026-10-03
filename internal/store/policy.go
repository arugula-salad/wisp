package store

import "time"

// The policy types double as the API's wire format, which is why their JSON
// naming is upstream's and not ours (camelCase in one, snake_case in the other).

type PrivilegesPolicy struct {
	Profile         string   `json:"profile,omitempty"` // "", "minimal", "standard" or "privileged"
	Devices         []string `json:"devices,omitempty"`
	NoNewPrivileges bool     `json:"noNewPrivileges,omitempty"`
}

type MemoryPolicy struct {
	LimitMB   int  `json:"limit_mb"`
	Autoscale bool `json:"autoscale,omitempty"`
}

type ResourcesPolicy struct {
	Memory *MemoryPolicy `json:"memory,omitempty"`
}

// SpawnPolicy is ours, not upstream's: it lets code inside a sprite create and
// manage sprites of its own over /.sprite/api.sock.
type SpawnPolicy struct {
	Enabled bool `json:"enabled"`
	// MaxChildren caps the sprites it may hold at once. 0 means the default.
	MaxChildren int `json:"max_children,omitempty"`
	// Sources names sprites whose checkpoints it may clone, besides its own and
	// its children's.
	Sources []string `json:"sources,omitempty"`
	// ChildTTLSeconds gives every child a lease of that length at birth, so a
	// slot under MaxChildren comes back on its own (leases.go). 0, the default,
	// means children never expire and the spawner is expected to delete them.
	ChildTTLSeconds int64 `json:"child_ttl_seconds,omitempty"`
}

// LifecyclePolicy is a sandbox's own lifecycle rules, which the front end that
// owns it sets: E2B's sandboxes run to a deadline whatever they are doing,
// Daytona's stop when idle, and so on. A nil policy, and every zero field, is
// the daemon's default and what every sprite has: suspend warm after
// --idle-timeout of idleness, and delete when the lease (ExpiresAt) runs out.
//
// The deadline itself is not here: it is Record.ExpiresAt and Record.Protected,
// the Sprites lease, which this generalises. Only what happens at it is.
type LifecyclePolicy struct {
	// IdleTimeout replaces --idle-timeout for this sandbox; 0 is the daemon's.
	IdleTimeout time.Duration `json:"idle_timeout_ns,omitempty"`
	IdleAction  IdleAction    `json:"idle_action,omitempty"`
	// DeadlineAction is what happens when ExpiresAt passes.
	DeadlineAction DeadlineAction `json:"deadline_action,omitempty"`
}

// IdleAction is what the idle watcher does to a running sandbox that has been
// idle for its timeout.
type IdleAction string

const (
	IdleSuspend IdleAction = "suspend" // warm: a memory snapshot (the default)
	IdleStop    IdleAction = "stop"    // cold: the VM is stopped, the disk kept
	IdleNone    IdleAction = "none"    // never stopped for being idle
)

// DeadlineAction is what happens to a sandbox whose deadline passes.
type DeadlineAction string

const (
	DeadlineDelete  DeadlineAction = "delete"  // the sandbox is deleted, disk and all (the default: a lease)
	DeadlineStop    DeadlineAction = "stop"    // cold, disk kept; the deadline is then cleared
	DeadlineSuspend DeadlineAction = "suspend" // warm; the deadline is then cleared
)

// Valid reports whether every field holds a known value.
func (p LifecyclePolicy) Valid() bool {
	switch p.IdleAction {
	case "", IdleSuspend, IdleStop, IdleNone:
	default:
		return false
	}
	switch p.DeadlineAction {
	case "", DeadlineDelete, DeadlineStop, DeadlineSuspend:
	default:
		return false
	}
	return p.IdleTimeout >= 0
}

// Idle is the idle rule with the defaults filled in: the timeout (def where
// the policy names none) and the action.
func (p *LifecyclePolicy) Idle(def time.Duration) (time.Duration, IdleAction) {
	if p == nil {
		return def, IdleSuspend
	}
	d, a := p.IdleTimeout, p.IdleAction
	if d == 0 {
		d = def
	}
	if a == "" {
		a = IdleSuspend
	}
	return d, a
}

// OnDeadline is the deadline action with the default filled in.
func (p *LifecyclePolicy) OnDeadline() DeadlineAction {
	if p == nil || p.DeadlineAction == "" {
		return DeadlineDelete
	}
	return p.DeadlineAction
}
