// Package store persists sprite metadata as one JSON file per sprite under
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

type Checkpoint struct {
	ID         string    `json:"id"`
	CreateTime time.Time `json:"create_time"`
	Comment    string    `json:"comment,omitempty"`
	// History is the chain of checkpoints this one descends from, nearest first.
	History []string `json:"history,omitempty"`
	IsAuto  bool     `json:"is_auto,omitempty"`
}

// Sprite is the persisted record. Runtime status is not stored here.
type Sprite struct {
	ID            string            `json:"id"`
	Name          string            `json:"name"`
	Config        Config            `json:"config"`
	Environment   map[string]string `json:"environment,omitempty"`
	URLSettings   URLSettings       `json:"url_settings"`
	Labels        []string          `json:"labels,omitempty"`
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
}

type Store struct {
	root string // <data>/vm

	mu     sync.Mutex
	byName map[string]*Sprite
}

func Open(dataDir string) (*Store, error) {
	s := &Store{root: filepath.Join(dataDir, "vm"), byName: map[string]*Sprite{}}
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
		s.byName[sp.Name] = &sp
	}
	return s, nil
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

// Create reserves the name, allocates an address and writes the record. The
// caller populates the machine directory (which exists on return).
func (s *Store) Create(sp *Sprite) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.byName[sp.Name]; ok {
		return ErrExists
	}
	used := map[int]bool{}
	for _, o := range s.byName {
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
	if err := os.MkdirAll(s.Dir(sp.ID), 0o755); err != nil {
		return err
	}
	if err := s.save(sp); err != nil {
		return err
	}
	s.byName[sp.Name] = sp
	return nil
}

// Get returns a copy of the record.
func (s *Store) Get(name string) (Sprite, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sp, ok := s.byName[name]
	if !ok {
		return Sprite{}, ErrNotFound
	}
	return *sp, nil
}

// Update applies fn to the record and persists it.
func (s *Store) Update(name string, fn func(*Sprite)) (Sprite, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sp, ok := s.byName[name]
	if !ok {
		return Sprite{}, ErrNotFound
	}
	fn(sp)
	return *sp, s.save(sp)
}

// List returns sprites whose names start with prefix, sorted by name.
func (s *Store) List(prefix string) []Sprite {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []Sprite{}
	for _, sp := range s.byName {
		if strings.HasPrefix(sp.Name, prefix) {
			out = append(out, *sp)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Delete removes the record and the whole machine directory.
func (s *Store) Delete(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	sp, ok := s.byName[name]
	if !ok {
		return ErrNotFound
	}
	delete(s.byName, name)
	return os.RemoveAll(s.Dir(sp.ID))
}
