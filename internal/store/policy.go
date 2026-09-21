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
