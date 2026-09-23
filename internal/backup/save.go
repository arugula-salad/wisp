package backup

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/jhgaylor/wisp/internal/store"
)

// FileRef is one file to back up: the name it takes in the manifest (relative to
// the machine directory) and where to read it from now. The two differ when the
// disk is read through an instant reflink snapshot instead of in place.
type FileRef struct {
	Name string
	Path string
}

// Stats is what one backup moved.
type Stats struct {
	Read        int64 // bytes read off local disk
	Uploaded    int64 // bytes sent to the bucket
	Chunks      int   // chunks the manifest references
	NewChunks   int   // chunks that were not already in the bucket
	Took        time.Duration
	ManifestKey string
}

func (s Stats) String() string {
	return fmt.Sprintf("%d chunks (%d new, %s uploaded of %s read) in %s",
		s.Chunks, s.NewChunks, human(s.Uploaded), human(s.Read), s.Took.Round(time.Millisecond))
}

func human(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GiB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KiB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}

// SaveOptions are the parts of a backup only its caller knows.
type SaveOptions struct {
	// Reason records what triggered the backup.
	Reason string
	// At is the moment the files were captured, when that is earlier than now
	// because they are being read from a snapshot. It becomes the manifest's
	// timestamp: the recovery point is when the data was frozen, not when the
	// upload finished. Zero means now.
	At time.Time
	// Precommit runs once every chunk is in the bucket and before the manifest is
	// written. An error abandons the backup without a manifest. It is how a caller
	// reading a disk in place says "that disk was written to while you read it".
	Precommit func() error
}

// Save is SaveWith for a caller with nothing to add but the reason.
func (r *Repo) Save(ctx context.Context, sp store.Sprite, refs []FileRef, reason string) (*Manifest, Stats, error) {
	return r.SaveWith(ctx, sp, refs, SaveOptions{Reason: reason})
}

// SaveWith chunks every file in refs, uploads what the bucket lacks, and writes a
// manifest plus the latest.json pointer. The manifest is written last, so a
// backup interrupted part way leaves chunks nobody references (which prune
// collects) rather than a manifest that cannot be restored.
func (r *Repo) SaveWith(ctx context.Context, sp store.Sprite, refs []FileRef, opts SaveOptions) (*Manifest, Stats, error) {
	// A prune that ran while we worked may have collected a chunk this backup
	// skipped as already present. Going round again, against a fresh index, puts
	// it back; the second pass uploads only what is missing.
	for attempt := 1; ; attempt++ {
		m, stats, err := r.saveOnce(ctx, sp, refs, opts)
		if !errors.Is(err, errPruned) || attempt == 3 {
			return m, stats, err
		}
		r.log.Info("a prune ran during this backup; checking its chunks again", "sprite", sp.Name)
	}
}

var errPruned = errors.New("the bucket was pruned during this backup")

func (r *Repo) saveOnce(ctx context.Context, sp store.Sprite, refs []FileRef, opts SaveOptions) (*Manifest, Stats, error) {
	start := time.Now()
	var stats Stats
	pruned, err := r.seed(ctx)
	if err != nil {
		return nil, stats, err
	}

	at := opts.At
	if at.IsZero() {
		at = start
	}
	m := &Manifest{Version: manifestVersion, CreatedAt: at.UTC(), ChunkSize: ChunkSize,
		Encrypted: r.cr != nil, Sprite: sp, Files: map[string]File{}, Reason: opts.Reason}

	up := newUploader(ctx, r)
	for _, ref := range refs {
		st, err := os.Stat(ref.Path)
		if errors.Is(err, os.ErrNotExist) {
			continue // a checkpoint deleted while we were working on the list
		}
		if err != nil {
			up.stop()
			return nil, stats, err
		}
		f := File{Size: st.Size()}
		read, err := walkChunks(ctx, ref.Path, r.lim, func(off int64, buf []byte) error {
			id := r.cr.id(buf)
			f.Chunks = append(f.Chunks, Chunk{Off: off, Len: len(buf), ID: id})
			return up.add(id, buf)
		})
		stats.Read += read
		if err != nil {
			up.stop()
			return nil, stats, fmt.Errorf("chunk %s: %w", ref.Name, err)
		}
		stats.Chunks += len(f.Chunks)
		m.Files[ref.Name] = f
	}
	uploaded, newChunks, err := up.wait()
	stats.Uploaded, stats.NewChunks = uploaded, newChunks
	if err != nil {
		return nil, stats, err
	}

	if opts.Precommit != nil {
		if err := opts.Precommit(); err != nil {
			return nil, stats, err
		}
	}
	if now, err := r.lastPrune(ctx); err != nil {
		return nil, stats, err
	} else if now != pruned {
		r.invalidate()
		return nil, stats, errPruned
	}

	stamp := m.Stamp()
	if err := r.putJSON(ctx, manifestKey(sp.ID, stamp), m); err != nil {
		return nil, stats, fmt.Errorf("write manifest: %w", err)
	}
	if err := r.putJSON(ctx, latestKey(sp.ID), m); err != nil {
		return nil, stats, fmt.Errorf("write latest pointer: %w", err)
	}
	stats.Took, stats.ManifestKey = time.Since(start), manifestKey(sp.ID, stamp)
	return m, stats, nil
}

// uploader runs the PUTs for one backup, bounded by Repo.par, so that chunking
// and uploading overlap without holding the whole disk in memory.
type uploader struct {
	r    *Repo
	ctx  context.Context
	sem  chan struct{}
	wg   sync.WaitGroup
	seen map[string]bool // deduped within this backup, before the bucket is asked

	mu       sync.Mutex
	uploaded int64
	new      int
	err      error
}

func newUploader(ctx context.Context, r *Repo) *uploader {
	return &uploader{r: r, ctx: ctx, sem: make(chan struct{}, r.par), seen: map[string]bool{}}
}

// add queues a chunk. buf belongs to the chunker and is copied only when the
// chunk really has to be uploaded, which is the uncommon case after the first
// backup.
func (u *uploader) add(id string, buf []byte) error {
	if u.failed() != nil {
		return u.failed()
	}
	if u.seen[id] || u.r.known(id) {
		return nil
	}
	u.seen[id] = true

	body := make([]byte, len(buf))
	copy(body, buf)
	select {
	case u.sem <- struct{}{}:
	case <-u.ctx.Done():
		return u.ctx.Err()
	}
	u.wg.Add(1)
	go func() {
		defer u.wg.Done()
		defer func() { <-u.sem }()
		n, err := u.r.putChunk(u.ctx, id, body)
		u.mu.Lock()
		defer u.mu.Unlock()
		if err != nil {
			if u.err == nil {
				u.err = fmt.Errorf("upload chunk %s: %w", id, err)
			}
			return
		}
		u.uploaded += n
		if n > 0 {
			u.new++
		}
	}()
	return nil
}

func (u *uploader) failed() error {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.err
}

// wait blocks for the in-flight uploads and reports the totals.
func (u *uploader) wait() (uploaded int64, newChunks int, err error) {
	u.wg.Wait()
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.uploaded, u.new, u.err
}

// stop waits for the in-flight uploads without caring about the result, so that a
// failure part way through a file does not leave goroutines writing.
func (u *uploader) stop() { u.wg.Wait() }

// SpriteFiles lists what to back up for a sprite in dir: the disk (read from
// diskPath, which may be a snapshot elsewhere) and every checkpoint image.
// snap.mem and snap.vmstate are left out on purpose.
func SpriteFiles(dir, diskPath, diskName string) ([]FileRef, error) {
	refs := []FileRef{{Name: diskName, Path: diskPath}}
	entries, err := os.ReadDir(filepath.Join(dir, "checkpoints"))
	if errors.Is(err, os.ErrNotExist) {
		return refs, nil
	}
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() && filepath.Ext(e.Name()) == ".ext4" {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	for _, n := range names {
		refs = append(refs, FileRef{Name: "checkpoints/" + n, Path: filepath.Join(dir, "checkpoints", n)})
	}
	return refs, nil
}
