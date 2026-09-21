package backup

import (
	"crypto/sha256"
	"strings"
	"time"

	"github.com/jhgaylor/mini-sprites/internal/store"
)

// A manifest is everything needed to rebuild one sprite's machine directory:
// its store record, and, for each file, the content-addressed chunks that make it
// up. Offsets are explicit, so a hole is simply an offset nobody mentions and a
// restored disk is as sparse as the one that was backed up.
//
// Warm memory state (snap.vmstate, snap.mem) is deliberately absent. It is
// guest-RAM-sized and only valid on the same CPU model, so a restored sprite
// starts cold with its filesystem intact.

const manifestVersion = 1

// stampFormat names a manifest. It carries no colons on purpose: keys that need
// no percent-encoding keep the SigV4 canonical path and the wire path identical.
const stampFormat = "20060102T150405.000000000Z"

type Manifest struct {
	Version   int          `json:"version"`
	CreatedAt time.Time    `json:"created_at"`
	ChunkSize int          `json:"chunk_size"`
	Encrypted bool         `json:"encrypted"`
	Sprite    store.Sprite `json:"sprite"`
	// Files is keyed by the path relative to the machine directory, e.g.
	// "disk.ext4" or "checkpoints/v1.ext4".
	Files map[string]File `json:"files"`
	// Reason records what triggered this backup, for `spritesd backups list`.
	Reason string `json:"reason,omitempty"`
}

type File struct {
	Size   int64   `json:"size"`
	Chunks []Chunk `json:"chunks"`
}

type Chunk struct {
	Off int64  `json:"off"`
	Len int    `json:"len"`
	ID  string `json:"id"`
}

// Stamp is this manifest's key component, derived from its timestamp.
func (m *Manifest) Stamp() string { return m.CreatedAt.UTC().Format(stampFormat) }

// Bytes is the manifest's logical size: what a full restore would write.
func (m *Manifest) Bytes() int64 {
	var n int64
	for _, f := range m.Files {
		for _, c := range f.Chunks {
			n += int64(c.Len)
		}
	}
	return n
}

// chunkIDs is the set of chunks this manifest keeps alive, for garbage collection.
func (m *Manifest) chunkIDs() map[string]bool {
	out := map[string]bool{}
	for _, f := range m.Files {
		for _, c := range f.Chunks {
			out[c.ID] = true
		}
	}
	return out
}

// Object keys. Chunks are fanned out one level so that no single S3 "directory"
// holds every chunk in the repository.
func chunkKey(id string) string { return "chunks/" + id[:2] + "/" + id }

// chunkIDFromKey is chunkKey in reverse, and says no for anything that is not
// shaped like one, so garbage collection never acts on a key it cannot parse.
func chunkIDFromKey(key string) (string, bool) {
	rest, ok := strings.CutPrefix(key, "chunks/")
	if !ok {
		return "", false
	}
	fan, id, ok := strings.Cut(rest, "/")
	if !ok || len(fan) != 2 || len(id) != 2*sha256.Size || !strings.HasPrefix(id, fan) {
		return "", false
	}
	return id, true
}
func spritePrefix(id string) string   { return "sprites/" + id + "/" }
func manifestPrefix(id string) string { return spritePrefix(id) + "manifests/" }

func manifestKey(spriteID, stamp string) string { return manifestPrefix(spriteID) + stamp + ".json" }
func latestKey(spriteID string) string          { return spritePrefix(spriteID) + "latest.json" }
func deletedKey(spriteID string) string         { return spritePrefix(spriteID) + "deleted.json" }

const repoConfigKey = "repository.json"

// pruneMarkerKey is written by every prune before it deletes anything, and again
// when it has finished. A backup compares it before and after its uploads, and
// stands aside while one is unfinished; see Repo.seed and Prune.
const pruneMarkerKey = "prune.json"

// pruneDeadline is the longest a prune may run; Prune enforces it, and a
// marker left unfinished for longer belongs to a prune that died.
const pruneDeadline = time.Hour

type pruneMarker struct {
	StartedAt  time.Time  `json:"started_at"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
}

// repoConfig is written once, on the first backup into a bucket. It exists to
// catch the two ways a second machine can quietly ruin a repository: a different
// chunk size, and a different (or missing) encryption key.
type repoConfig struct {
	Version   int    `json:"version"`
	ChunkSize int    `json:"chunk_size"`
	Encrypted bool   `json:"encrypted"`
	KeyCheck  string `json:"key_check,omitempty"`
	CreatedAt string `json:"created_at"`
}

// Tombstone marks a sprite that was deleted on the host. Deleting a sprite and
// losing the machine it lived on must not look the same to the bucket, so the
// backup outlives the sprite until `spritesd backups prune` retires it.
type Tombstone struct {
	SpriteID  string    `json:"sprite_id"`
	Name      string    `json:"name"`
	DeletedAt time.Time `json:"deleted_at"`
}
