// Package backup is the object-storage backup tier: it cuts a sprite's disk and
// checkpoints into content-addressed chunks, uploads the ones the bucket does not
// already have, and can rebuild a machine directory from a manifest on a fresh
// host.
//
// It is a backup tier, not upstream's storage architecture: the recovery point is
// the last completed upload, and a cold wake still reads from local disk. See
// internal/server/backup.go for when uploads are triggered.
package backup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jhgaylor/wisp/internal/s3"
)

type Config struct {
	Endpoint        string
	Bucket          string
	Region          string
	CredentialsFile string
	KeyFile         string
	// Parallel chunk uploads. Each in-flight chunk holds ChunkSize in memory.
	Parallel int
	// RateLimit caps bytes/second read and uploaded; 0 is unlimited.
	RateLimit int64
	Log       *slog.Logger
}

type Repo struct {
	cl    *s3.Client
	cr    *crypter
	log   *slog.Logger
	lim   *limiter
	par   int
	debug bool

	mu     sync.Mutex
	have   map[string]bool // chunk IDs known to be in the bucket
	seeded bool
	// seededAsOf is the prune marker the index was listed under; see seed.
	seededAsOf string
}

// Open connects to the bucket and reconciles the repository's configuration with
// this host's. It does not list the chunk index yet; the first backup does that.
func Open(ctx context.Context, cfg Config) (*Repo, error) {
	access, secret, err := s3.LoadCredentials(cfg.CredentialsFile)
	if err != nil {
		return nil, fmt.Errorf("backup credentials: %w", err)
	}
	cl, err := s3.New(s3.Config{Endpoint: cfg.Endpoint, Bucket: cfg.Bucket, Region: cfg.Region,
		AccessKey: access, SecretKey: secret})
	if err != nil {
		return nil, err
	}
	log := cfg.Log
	if log == nil {
		log = slog.Default()
	}
	par := cfg.Parallel
	if par < 1 {
		par = 4
	}
	r := &Repo{cl: cl, log: log, lim: newLimiter(cfg.RateLimit), par: par, have: map[string]bool{}}

	if cfg.KeyFile != "" {
		key, err := loadKey(cfg.KeyFile)
		if err != nil {
			return nil, fmt.Errorf("backup key: %w", err)
		}
		if r.cr, err = newCrypter(key); err != nil {
			return nil, err
		}
	}
	if err := r.checkConfig(ctx); err != nil {
		return nil, err
	}
	return r, nil
}

func (r *Repo) Encrypted() bool { return r.cr != nil }
func (r *Repo) Bucket() string  { return r.cl.Bucket() }

// checkConfig writes repository.json on a fresh bucket, and refuses to touch one
// whose chunk size or encryption does not match this host's.
func (r *Repo) checkConfig(ctx context.Context) error {
	b, err := r.cl.Get(ctx, repoConfigKey)
	if errors.Is(err, s3.ErrNotFound) {
		cfg := repoConfig{Version: manifestVersion, ChunkSize: ChunkSize,
			Encrypted: r.cr != nil, KeyCheck: r.cr.keyCheck(),
			CreatedAt: time.Now().UTC().Format(time.RFC3339)}
		body, _ := json.MarshalIndent(cfg, "", "  ")
		if err := r.cl.Put(ctx, repoConfigKey, body); err != nil {
			return fmt.Errorf("initialise backup repository: %w", err)
		}
		r.log.Info("backup repository initialised", "bucket", r.cl.Bucket(),
			"endpoint", r.cl.Endpoint(), "encrypted", r.cr != nil)
		return nil
	}
	if err != nil {
		return fmt.Errorf("read %s: %w", repoConfigKey, err)
	}
	var cfg repoConfig
	if err := json.Unmarshal(b, &cfg); err != nil {
		return fmt.Errorf("%s is not valid JSON: %w", repoConfigKey, err)
	}
	if cfg.ChunkSize != ChunkSize {
		return fmt.Errorf("bucket %s was written with %d-byte chunks, this build uses %d",
			r.cl.Bucket(), cfg.ChunkSize, ChunkSize)
	}
	if cfg.Encrypted != (r.cr != nil) {
		if cfg.Encrypted {
			return fmt.Errorf("bucket %s holds encrypted backups; --backup-key-file is required", r.cl.Bucket())
		}
		return fmt.Errorf("bucket %s holds unencrypted backups; remove --backup-key-file or use another bucket", r.cl.Bucket())
	}
	if !r.cr.sameKey(cfg.KeyCheck) {
		return fmt.Errorf("bucket %s was written with a different key", r.cl.Bucket())
	}
	return nil
}

// seed makes sure the chunk index is loaded and current, and returns the prune
// marker it is current as of.
//
// The index is listed once per process. At a 30 GiB quota that is under 8k keys,
// eight list requests, after which a repeat backup costs no requests at all for
// chunks it already has. An index that omits a chunk (a concurrent writer's) costs
// a redundant PUT of an identical object. An index that still lists a chunk a
// prune has since collected would cost the backup, so every Save reads the marker
// `wispd backups prune` leaves (one small GET) and relists when it has moved.
func (r *Repo) seed(ctx context.Context) (pruned string, err error) {
	if pruned, err = r.lastPrune(ctx); err != nil {
		return "", err
	}
	r.mu.Lock()
	if r.seeded && r.seededAsOf == pruned {
		r.mu.Unlock()
		return pruned, nil
	}
	r.mu.Unlock()

	start := time.Now()
	have := map[string]bool{}
	if err := r.cl.List(ctx, "chunks/", func(o s3.Object) error {
		if id, ok := chunkIDFromKey(o.Key); ok {
			have[id] = true
		}
		return nil
	}); err != nil {
		return "", fmt.Errorf("list the chunk index: %w", err)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.have, r.seeded, r.seededAsOf = have, true, pruned
	r.log.Info("backup chunk index loaded", "chunks", len(have), "took", time.Since(start).Round(time.Millisecond))
	return pruned, nil
}

// invalidate drops the chunk index, so that the next Save lists it again.
func (r *Repo) invalidate() {
	r.mu.Lock()
	r.seeded = false
	r.mu.Unlock()
}

// ErrPruneRunning means `wispd backups prune` is at work on this bucket. A
// backup cannot know which chunks it is about to collect, so it stands aside; it
// is a reason to try again later, not a failure.
var ErrPruneRunning = errors.New("a prune of this bucket is in progress")

// lastPrune reads the prune marker; "" means the bucket has never been pruned.
func (r *Repo) lastPrune(ctx context.Context) (string, error) {
	b, err := r.cl.Get(ctx, pruneMarkerKey)
	if errors.Is(err, s3.ErrNotFound) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read %s: %w", pruneMarkerKey, err)
	}
	var m pruneMarker
	if err := json.Unmarshal(b, &m); err != nil {
		return "", fmt.Errorf("%s is not valid JSON: %w", pruneMarkerKey, err)
	}
	// A marker with no finish is a prune at work, unless it is older than any
	// prune is allowed to run, in which case it is one that died.
	if m.FinishedAt == nil && time.Since(m.StartedAt) < pruneDeadline {
		return "", ErrPruneRunning
	}
	return string(b), nil
}

func (r *Repo) known(id string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.have[id]
}

func (r *Repo) remember(id string) {
	r.mu.Lock()
	r.have[id] = true
	r.mu.Unlock()
}

func (r *Repo) forget(id string) {
	r.mu.Lock()
	delete(r.have, id)
	r.mu.Unlock()
}

// putChunk uploads one chunk unless the bucket already has it.
func (r *Repo) putChunk(ctx context.Context, id string, plain []byte) (uploaded int64, err error) {
	if r.known(id) {
		return 0, nil
	}
	body, err := r.cr.seal(id, plain)
	if err != nil {
		return 0, err
	}
	if err := r.lim.wait(ctx, len(body)); err != nil {
		return 0, err
	}
	if err := r.cl.Put(ctx, chunkKey(id), body); err != nil {
		return 0, err
	}
	r.remember(id)
	return int64(len(body)), nil
}

// getChunk fetches and verifies one chunk.
func (r *Repo) getChunk(ctx context.Context, id string) ([]byte, error) {
	body, err := r.cl.Get(ctx, chunkKey(id))
	if err != nil {
		return nil, err
	}
	return r.cr.open(id, body)
}

func (r *Repo) putJSON(ctx context.Context, key string, v any) error {
	body, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	if body, err = r.cr.sealBlob(key, body); err != nil {
		return err
	}
	return r.cl.Put(ctx, key, body)
}

func (r *Repo) getJSON(ctx context.Context, key string, v any) error {
	body, err := r.cl.Get(ctx, key)
	if err != nil {
		return err
	}
	if body, err = r.cr.openBlob(key, body); err != nil {
		return err
	}
	if err := json.Unmarshal(body, v); err != nil {
		return fmt.Errorf("%s: %w", key, err)
	}
	return nil
}

// Latest returns a sprite's newest manifest.
func (r *Repo) Latest(ctx context.Context, spriteID string) (*Manifest, error) {
	var m Manifest
	if err := r.getJSON(ctx, latestKey(spriteID), &m); err != nil {
		return nil, err
	}
	return &m, nil
}

// LoadManifest returns one manifest by its stamp.
func (r *Repo) LoadManifest(ctx context.Context, spriteID, stamp string) (*Manifest, error) {
	var m Manifest
	if err := r.getJSON(ctx, manifestKey(spriteID, stamp), &m); err != nil {
		return nil, err
	}
	return &m, nil
}

// Stamps lists a sprite's manifests, oldest first.
func (r *Repo) Stamps(ctx context.Context, spriteID string) ([]string, error) {
	var out []string
	err := r.cl.List(ctx, manifestPrefix(spriteID), func(o s3.Object) error {
		out = append(out, strings.TrimSuffix(o.Key[len(manifestPrefix(spriteID)):], ".json"))
		return nil
	})
	sort.Strings(out)
	return out, err
}

// SpriteInfo is one sprite as the bucket sees it.
type SpriteInfo struct {
	ID      string
	Name    string
	Latest  *Manifest
	Deleted *Tombstone
	Stamps  []string
}

// Sprites lists every sprite in the bucket, including deleted ones, newest
// backup first.
func (r *Repo) Sprites(ctx context.Context) ([]SpriteInfo, error) {
	dirs, err := r.cl.ListDirs(ctx, "sprites/")
	if err != nil {
		return nil, err
	}
	out := make([]SpriteInfo, 0, len(dirs))
	for _, d := range dirs {
		id := strings.TrimSuffix(strings.TrimPrefix(d, "sprites/"), "/")
		info := SpriteInfo{ID: id}
		if m, err := r.Latest(ctx, id); err == nil {
			info.Latest, info.Name = m, m.Sprite.Name
		} else if !errors.Is(err, s3.ErrNotFound) {
			return nil, err
		}
		var tomb Tombstone
		if err := r.getJSON(ctx, deletedKey(id), &tomb); err == nil {
			info.Deleted = &tomb
			if info.Name == "" {
				info.Name = tomb.Name
			}
		} else if !errors.Is(err, s3.ErrNotFound) {
			return nil, err
		}
		if info.Stamps, err = r.Stamps(ctx, id); err != nil {
			return nil, err
		}
		out = append(out, info)
	}
	sort.Slice(out, func(i, j int) bool {
		ti, tj := latestTime(out[i]), latestTime(out[j])
		if !ti.Equal(tj) {
			return ti.After(tj)
		}
		return out[i].Name < out[j].Name
	})
	return out, nil
}

func latestTime(s SpriteInfo) time.Time {
	if s.Latest != nil {
		return s.Latest.CreatedAt
	}
	return time.Time{}
}

// Find resolves a sprite name to its bucket entry. Names are unique on a host but
// a bucket can hold two sprites that had the same name at different times, so the
// most recently backed up one wins and the caller is told.
func (r *Repo) Find(ctx context.Context, name string) (SpriteInfo, []SpriteInfo, error) {
	all, err := r.Sprites(ctx)
	if err != nil {
		return SpriteInfo{}, nil, err
	}
	var matches []SpriteInfo
	for _, s := range all {
		if s.Name == name || s.ID == name {
			matches = append(matches, s)
		}
	}
	if len(matches) == 0 {
		return SpriteInfo{}, nil, fmt.Errorf("no backup of %q in %s", name, r.cl.Bucket())
	}
	return matches[0], matches[1:], nil
}

// MarkDeleted records that a sprite was deleted on the host. The backup stays
// until it is pruned.
func (r *Repo) MarkDeleted(ctx context.Context, spriteID, name string) error {
	return r.putJSON(ctx, deletedKey(spriteID), Tombstone{
		SpriteID: spriteID, Name: name, DeletedAt: time.Now().UTC()})
}
