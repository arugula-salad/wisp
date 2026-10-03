// Package store persists sandbox records (sprites, and the sandboxes of any
// other API, which are Sprites with no SpriteMeta) as one JSON file each under
// <data>/vm/<id>/sprite.json, next to that sprite's disk and snapshots.
package store

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

var (
	ErrNotFound = errors.New("sprite not found")
	ErrExists   = errors.New("sprite already exists")
)

type Config struct {
	RamMB     int    `json:"ram_mb,omitempty"`
	CPUs      int    `json:"cpus,omitempty"`
	Region    string `json:"region,omitempty"`
	StorageGB int    `json:"storage_gb,omitempty"`
}

type URLSettings struct {
	Auth string `json:"auth,omitempty"`
}

// NetworkRule is one egress policy rule exactly as the API carries it: either a
// domain with an action, or an include of a named bundle.
type NetworkRule struct {
	Domain  string `json:"domain,omitempty"`
	Action  string `json:"action,omitempty"`
	Include string `json:"include,omitempty"`
}

type Checkpoint struct {
	ID         string    `json:"id"`
	CreateTime time.Time `json:"create_time"`
	Comment    string    `json:"comment,omitempty"`
	// History is the chain of checkpoints this one descends from, nearest first.
	History []string `json:"history,omitempty"`
	IsAuto  bool     `json:"is_auto,omitempty"`
}

// Record is what the engine needs to run and persist a sandbox, whichever API
// created it. Runtime status is not stored here.
type Record struct {
	ID string `json:"id"`
	// API is the front end the sandbox belongs to, which is also its name
	// namespace. Empty is the Sprites API: every record from before there could
	// be another.
	API string `json:"api,omitempty"`
	// Hostname is the guest's hostname, which the front end chooses at create.
	// A Sprites record's is its name, and on disk it is left out when it is
	// (see Sprite.MarshalJSON), so sprite.json has no such key.
	Hostname      string            `json:"hostname,omitempty"`
	Config        Config            `json:"config"`
	Environment   map[string]string `json:"environment,omitempty"`
	CreatedAt     time.Time         `json:"created_at"`
	UpdatedAt     time.Time         `json:"updated_at"`
	LastRunningAt *time.Time        `json:"last_running_at,omitempty"`
	LastWarmingAt *time.Time        `json:"last_warming_at,omitempty"`
	// NetIndex is this sprite's host number within the sprite network (whose
	// prefix belongs to the host bridge, not to us). 0 means unassigned.
	NetIndex int `json:"net_index"`
	// BootIP is the address the guest configured at its last cold boot. A warm
	// snapshot taken under a different address is useless and gets discarded.
	BootIP         string       `json:"boot_ip,omitempty"`
	Checkpoints    []Checkpoint `json:"checkpoints,omitempty"`
	NextCheckpoint int          `json:"next_checkpoint"`
	// NextAuto numbers auto-<n> checkpoints, which never consume a v<n>.
	NextAuto int `json:"next_auto,omitempty"`
	// Lineage is the History a checkpoint taken now would get: the checkpoint
	// the live filesystem was last saved as or restored from, then its ancestors.
	Lineage []string `json:"lineage,omitempty"`

	// Image is the container image the sprite's disk was made from, pinned
	// by digest where the registry gave one; empty for the base image or a clone.
	Image string `json:"image,omitempty"`

	// Mounts maps a checkpoint slot to the checkpoint whose image backs it, for as
	// long as the VM (or its warm snapshot, which records the drive paths) lives.
	Mounts map[int]string `json:"mounts,omitempty"`

	// NetworkRules is the egress policy as the client wrote it. Empty means unrestricted.
	NetworkRules []NetworkRule `json:"network_rules,omitempty"`

	// A nil policy is upstream's default: unrestricted.
	Privileges *PrivilegesPolicy `json:"privileges_policy,omitempty"`
	Resources  *ResourcesPolicy  `json:"resources_policy,omitempty"`

	// ExpiresAt is the workspace lease: when it passes, the sprite is deleted,
	// disk and all. nil is the default and means the sprite lives until someone
	// deletes it, because losing a workspace to an expiry nobody asked for would
	// be worse than leaving a stale one on the volume. Persisted like the rest,
	// so a lease outlives the daemon that granted it.
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
	// Protected holds off that deletion without forgetting the deadline, for the
	// sprite somebody turns out to still be using.
	Protected bool `json:"protected,omitempty"`

	// Ext is where a front end other than Sprites keeps its own metadata, by
	// front end. The engine never reads it.
	Ext map[string]json.RawMessage `json:"ext,omitempty"`
}

// SpriteMeta is what the Sprites API keeps about a sprite beside its Record.
// The engine does not read it.
type SpriteMeta struct {
	// Name is unique within the Sprites namespace (API "").
	Name        string      `json:"name"`
	URLSettings URLSettings `json:"url_settings"`
	Labels      []string    `json:"labels,omitempty"`

	// URLDomain is the domain this sprite's URL is under (<name>.<URLDomain>),
	// one of wispd's --url-domain list. Empty is the first of them, which is
	// what every sprite made before there could be several has.
	URLDomain string `json:"url_domain,omitempty"`

	// ParentID is the sprite that created this one from inside. An ID rather than
	// a name, because names are reusable after a delete.
	ParentID string `json:"parent_id,omitempty"`
	// A nil spawn policy is the default: the sprite cannot create sprites.
	Spawn *SpawnPolicy `json:"spawn_policy,omitempty"`

	// Domains are custom hostnames served as this sprite's URL (domains.go). A
	// domain belongs to at most one sprite; clones do not inherit them.
	Domains []string `json:"domains,omitempty"`
}

// Sprite is the persisted record: the engine's Record and the Sprites
// metadata, as one sprite.json. A record of another API has an empty
// SpriteMeta.
type Sprite struct {
	Record
	SpriteMeta
}

// Sprites is the Sprites API's namespace, the API of every record from
// before there could be another.
const Sprites = ""

// Store holds every record by ID. Names are a front end's business and are
// unique within its API's namespace; a record without a name (another API's
// sandbox) is reachable by ID alone.
type Store struct {
	root string // <data>/vm

	mu     sync.Mutex
	byID   map[string]*Sprite
	byName map[string]map[string]*Sprite // API -> name -> record
}

func Open(dataDir string) (*Store, error) {
	s := &Store{root: filepath.Join(dataDir, "vm"), byID: map[string]*Sprite{}, byName: map[string]map[string]*Sprite{}}
	if err := os.MkdirAll(s.root, 0o755); err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(s.root)
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		b, err := os.ReadFile(filepath.Join(s.root, e.Name(), "sprite.json"))
		if err != nil {
			continue // half-created or foreign directory
		}
		var sp Sprite
		if err := json.Unmarshal(b, &sp); err != nil {
			return nil, fmt.Errorf("%s: %w", e.Name(), err)
		}
		s.indexLocked(&sp)
	}
	return s, nil
}

func (s *Store) indexLocked(sp *Sprite) {
	s.byID[sp.ID] = sp
	if sp.Name != "" {
		ns := s.byName[sp.API]
		if ns == nil {
			ns = map[string]*Sprite{}
			s.byName[sp.API] = ns
		}
		ns[sp.Name] = sp
	}
}

func (s *Store) unindexLocked(sp *Sprite) {
	delete(s.byID, sp.ID)
	if ns := s.byName[sp.API]; ns != nil && ns[sp.Name] == sp {
		delete(ns, sp.Name)
	}
}

// Dir is the sprite's machine directory.
func (s *Store) Dir(id string) string { return filepath.Join(s.root, id) }

func NewID() string {
	b := make([]byte, 6)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func (s *Store) save(sp *Sprite) error {
	b, err := json.MarshalIndent(sp, "", "  ")
	if err != nil {
		return err
	}
	path := filepath.Join(s.Dir(sp.ID), "sprite.json")
	if err := os.WriteFile(path+".tmp", b, 0o644); err != nil {
		return err
	}
	return os.Rename(path+".tmp", path)
}

// Create reserves the ID and the name (within the record's API), allocates an
// address and writes the record. The caller populates the machine directory
// (which exists on return). ErrExists is an ID or a name already taken.
func (s *Store) Create(sp *Sprite) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.byID[sp.ID]; ok || sp.ID == "" {
		return ErrExists
	}
	if _, ok := s.byName[sp.API][sp.Name]; ok && sp.Name != "" {
		return ErrExists
	}
	used := map[int]bool{}
	for _, o := range s.byID {
		used[o.NetIndex] = true
	}
	// Host .0.1 is the bridge; hand out the rest of the /16, skipping .0 and .255 octets.
	for n := 2; n < 65534 && sp.NetIndex == 0; n++ {
		if !used[n] && n&0xff != 0 && n&0xff != 255 {
			sp.NetIndex = n
		}
	}
	if sp.NetIndex == 0 {
		return errors.New("address pool exhausted")
	}
	// A restored record may name domains another sprite has taken since.
	sp.Domains = s.unclaimedLocked(sp.Domains)
	if err := os.MkdirAll(s.Dir(sp.ID), 0o755); err != nil {
		return err
	}
	if err := s.save(sp); err != nil {
		return err
	}
	s.indexLocked(sp)
	return nil
}

// Get returns a copy of the record with that ID.
func (s *Store) Get(id string) (Sprite, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sp, ok := s.byID[id]
	if !ok {
		return Sprite{}, ErrNotFound
	}
	return *sp, nil
}

// GetByName returns a copy of the record named name in api's namespace.
func (s *Store) GetByName(api, name string) (Sprite, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sp, ok := s.byName[api][name]
	if !ok {
		return Sprite{}, ErrNotFound
	}
	return *sp, nil
}

// GetRecord is Get for the engine, which has no use for the rest.
func (s *Store) GetRecord(id string) (Record, error) {
	sp, err := s.Get(id)
	return sp.Record, err
}

// Update applies fn to the record with that ID and persists it. fn must not
// change the ID, the API or the name.
func (s *Store) Update(id string, fn func(*Sprite)) (Sprite, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sp, ok := s.byID[id]
	if !ok {
		return Sprite{}, ErrNotFound
	}
	fn(sp)
	return *sp, s.save(sp)
}

// UpdateByName is Update for the record named name in api's namespace.
func (s *Store) UpdateByName(api, name string, fn func(*Sprite)) (Sprite, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sp, ok := s.byName[api][name]
	if !ok {
		return Sprite{}, ErrNotFound
	}
	fn(sp)
	return *sp, s.save(sp)
}

// UpdateRecord is Update for the engine, which changes the Record alone.
func (s *Store) UpdateRecord(id string, fn func(*Record)) (Record, error) {
	sp, err := s.Update(id, func(sp *Sprite) { fn(&sp.Record) })
	return sp.Record, err
}

// List returns the records in api's namespace whose names start with prefix,
// sorted by name.
func (s *Store) List(api, prefix string) []Sprite {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []Sprite{}
	for name, sp := range s.byName[api] {
		if strings.HasPrefix(name, prefix) {
			out = append(out, *sp)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// All returns every record of every API, sorted by API, then name, then ID:
// for sprites alone, the order List has.
func (s *Store) All() []Sprite {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Sprite, 0, len(s.byID))
	for _, sp := range s.byID {
		out = append(out, *sp)
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.API != b.API {
			return a.API < b.API
		}
		if a.Name != b.Name {
			return a.Name < b.Name
		}
		return a.ID < b.ID
	})
	return out
}

// Records is All for the engine.
func (s *Store) Records() []Record {
	all := s.All()
	out := make([]Record, len(all))
	for i, sp := range all {
		out[i] = sp.Record
	}
	return out
}

// Count is how many records there are, of every API.
func (s *Store) Count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.byID)
}

// Delete removes the record with that ID and the whole machine directory.
func (s *Store) Delete(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	sp, ok := s.byID[id]
	if !ok {
		return ErrNotFound
	}
	s.unindexLocked(sp)
	return os.RemoveAll(s.Dir(sp.ID))
}
