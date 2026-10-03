package store

import (
	"encoding/json"
	"time"
)

// spriteJSON is sprite.json as it has always been laid out: Record and
// SpriteMeta interleaved, in the order of the single struct they were split
// from, so a file decodes and encodes back to the same bytes and a wispd from
// before the split reads what this one writes. The fields only a newer front
// end sets (api, hostname, ext) come last and are left out when empty.
// TestSpriteJSONFieldsMatch keeps it in step with the two halves.
type spriteJSON struct {
	ID             string                     `json:"id"`
	Name           string                     `json:"name"`
	Config         Config                     `json:"config"`
	Environment    map[string]string          `json:"environment,omitempty"`
	URLSettings    URLSettings                `json:"url_settings"`
	Labels         []string                   `json:"labels,omitempty"`
	CreatedAt      time.Time                  `json:"created_at"`
	UpdatedAt      time.Time                  `json:"updated_at"`
	LastRunningAt  *time.Time                 `json:"last_running_at,omitempty"`
	LastWarmingAt  *time.Time                 `json:"last_warming_at,omitempty"`
	NetIndex       int                        `json:"net_index"`
	BootIP         string                     `json:"boot_ip,omitempty"`
	Checkpoints    []Checkpoint               `json:"checkpoints,omitempty"`
	NextCheckpoint int                        `json:"next_checkpoint"`
	NextAuto       int                        `json:"next_auto,omitempty"`
	Lineage        []string                   `json:"lineage,omitempty"`
	Image          string                     `json:"image,omitempty"`
	Mounts         map[int]string             `json:"mounts,omitempty"`
	NetworkRules   []NetworkRule              `json:"network_rules,omitempty"`
	Privileges     *PrivilegesPolicy          `json:"privileges_policy,omitempty"`
	Resources      *ResourcesPolicy           `json:"resources_policy,omitempty"`
	URLDomain      string                     `json:"url_domain,omitempty"`
	ParentID       string                     `json:"parent_id,omitempty"`
	Spawn          *SpawnPolicy               `json:"spawn_policy,omitempty"`
	Domains        []string                   `json:"domains,omitempty"`
	ExpiresAt      *time.Time                 `json:"expires_at,omitempty"`
	Protected      bool                       `json:"protected,omitempty"`
	API            string                     `json:"api,omitempty"`
	Hostname       string                     `json:"hostname,omitempty"`
	Ext            map[string]json.RawMessage `json:"ext,omitempty"`
}

// defaultHostname is the hostname a record gets when it names none: a
// sprite's is its name.
func (sp *Sprite) defaultHostname() string {
	if sp.API == "" {
		return sp.Name
	}
	return ""
}

func (sp Sprite) MarshalJSON() ([]byte, error) {
	j := spriteJSON{
		ID: sp.ID, Name: sp.Name, Config: sp.Config, Environment: sp.Environment,
		URLSettings: sp.URLSettings, Labels: sp.Labels, CreatedAt: sp.CreatedAt, UpdatedAt: sp.UpdatedAt,
		LastRunningAt: sp.LastRunningAt, LastWarmingAt: sp.LastWarmingAt, NetIndex: sp.NetIndex,
		BootIP: sp.BootIP, Checkpoints: sp.Checkpoints, NextCheckpoint: sp.NextCheckpoint,
		NextAuto: sp.NextAuto, Lineage: sp.Lineage, Image: sp.Image, Mounts: sp.Mounts,
		NetworkRules: sp.NetworkRules, Privileges: sp.Privileges, Resources: sp.Resources,
		URLDomain: sp.URLDomain, ParentID: sp.ParentID, Spawn: sp.Spawn, Domains: sp.Domains,
		ExpiresAt: sp.ExpiresAt, Protected: sp.Protected, API: sp.API, Hostname: sp.Hostname, Ext: sp.Ext,
	}
	if j.Hostname == sp.defaultHostname() {
		j.Hostname = ""
	}
	return json.Marshal(j)
}

func (sp *Sprite) UnmarshalJSON(b []byte) error {
	var j spriteJSON
	if err := json.Unmarshal(b, &j); err != nil {
		return err
	}
	*sp = Sprite{
		Record: Record{
			ID: j.ID, API: j.API, Hostname: j.Hostname, Config: j.Config, Environment: j.Environment,
			CreatedAt: j.CreatedAt, UpdatedAt: j.UpdatedAt, LastRunningAt: j.LastRunningAt,
			LastWarmingAt: j.LastWarmingAt, NetIndex: j.NetIndex, BootIP: j.BootIP,
			Checkpoints: j.Checkpoints, NextCheckpoint: j.NextCheckpoint, NextAuto: j.NextAuto,
			Lineage: j.Lineage, Image: j.Image, Mounts: j.Mounts, NetworkRules: j.NetworkRules,
			Privileges: j.Privileges, Resources: j.Resources, ExpiresAt: j.ExpiresAt,
			Protected: j.Protected, Ext: j.Ext,
		},
		SpriteMeta: SpriteMeta{
			Name: j.Name, URLSettings: j.URLSettings, Labels: j.Labels, URLDomain: j.URLDomain,
			ParentID: j.ParentID, Spawn: j.Spawn, Domains: j.Domains,
		},
	}
	if sp.Hostname == "" {
		sp.Hostname = sp.defaultHostname()
	}
	return nil
}
