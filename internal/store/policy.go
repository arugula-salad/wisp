package store

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
