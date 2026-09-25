package backup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/arugula-salad/wisp/internal/s3"
)

// PruneOptions controls garbage collection.
type PruneOptions struct {
	// Retention is how long a deleted sprite's backups are kept. Zero keeps them
	// for ever, which is the safe default for a command that deletes data.
	Retention time.Duration
	// Keep is how many manifests to keep per live sprite (0 = all). The newest is
	// always kept.
	Keep int
	// Grace protects chunks younger than this from collection, so a prune cannot
	// race a backup that has uploaded chunks but not yet written its manifest. The
	// command line defaults it to an hour; zero here really is zero.
	Grace time.Duration
	// Settle is how long to wait between announcing the prune and reading the
	// manifests; see Prune. The command line uses a few seconds.
	Settle time.Duration
	// DryRun reports what would go without deleting anything.
	DryRun bool
}

type PruneStats struct {
	SpritesRetired int
	Manifests      int
	Chunks         int
	Bytes          int64
	ChunksKept     int
}

// Prune is mark and sweep. It retires the manifests it is allowed to, collects
// every chunk ID that the surviving manifests reference, and then deletes chunks
// nothing references that are older than the grace period.
//
// Order matters: manifests go first, so a chunk is only ever deleted after the
// manifest that referenced it is gone. The grace period covers the other
// direction, a chunk uploaded by a backup whose manifest does not exist yet.
//
// A running wispd keeps an index of the chunks it believes are in the bucket,
// and this runs in another process. The marker written before anything is deleted
// is how that daemon finds out: a backup that sees it move relists the bucket and
// goes round again rather than commit a manifest naming a chunk that has gone,
// and one that finds a prune unfinished stands aside until it is. Settle is for
// the backup that read the old marker a moment before it changed: it has that
// long to land its manifest where the mark phase will see it.
func (r *Repo) Prune(ctx context.Context, opts PruneOptions, info func(string, ...any)) (PruneStats, error) {
	var stats PruneStats
	if info == nil {
		info = func(string, ...any) {}
	}
	if opts.Grace < 0 {
		opts.Grace = 0
	}

	ctx, cancel := context.WithTimeout(ctx, pruneDeadline)
	defer cancel()

	if !opts.DryRun {
		marker := pruneMarker{StartedAt: time.Now().UTC()}
		if err := r.putMarker(ctx, marker); err != nil {
			return stats, err
		}
		// Finished or failed, the marker is closed: an unfinished one holds every
		// backup off until it goes stale. The context may be what failed, so the
		// closing write gets its own.
		defer func() {
			ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
			defer cancel()
			now := time.Now().UTC()
			marker.FinishedAt = &now
			if err := r.putMarker(ctx, marker); err != nil {
				r.log.Warn("could not close the prune marker; backups wait until it goes stale",
					"after", pruneDeadline, "err", err)
			}
		}()
		select {
		case <-ctx.Done():
			return stats, ctx.Err()
		case <-time.After(opts.Settle):
		}
	}

	sprites, err := r.Sprites(ctx)
	if err != nil {
		return stats, err
	}

	// Phase 1: retire manifests.
	for _, sp := range sprites {
		drop, retire := r.manifestsToDrop(sp, opts)
		for _, stamp := range drop {
			info("dropping manifest %s/%s", sp.ID, stamp)
			if !opts.DryRun {
				if err := r.cl.Delete(ctx, manifestKey(sp.ID, stamp)); err != nil {
					return stats, err
				}
			}
			stats.Manifests++
		}
		if retire {
			info("retiring deleted sprite %s (%s)", sp.Name, sp.ID)
			if !opts.DryRun {
				for _, key := range []string{latestKey(sp.ID), deletedKey(sp.ID)} {
					if err := r.cl.Delete(ctx, key); err != nil {
						return stats, err
					}
				}
			}
			stats.SpritesRetired++
		}
	}

	// Phase 2: mark. Re-read the manifests that are left, so a manifest written
	// while phase 1 ran is still counted.
	live := map[string]bool{}
	after, err := r.Sprites(ctx)
	if err != nil {
		return stats, err
	}
	for _, sp := range after {
		for _, stamp := range sp.Stamps {
			m, err := r.LoadManifest(ctx, sp.ID, stamp)
			if errors.Is(err, s3.ErrNotFound) {
				continue // dropped in phase 1
			}
			if err != nil {
				return stats, err
			}
			for id := range m.chunkIDs() {
				live[id] = true
			}
		}
		// latest.json can outlive its manifest if the two writes were interrupted.
		if sp.Latest != nil {
			for id := range sp.Latest.chunkIDs() {
				live[id] = true
			}
		}
	}
	stats.ChunksKept = len(live)

	// Phase 3: sweep.
	cutoff := time.Now().Add(-opts.Grace)
	var doomed []s3.Object
	if err := r.cl.List(ctx, "chunks/", func(o s3.Object) error {
		id, ok := chunkIDFromKey(o.Key)
		if !ok {
			return nil // not a chunk key; leave whatever it is alone
		}
		if live[id] || o.LastModified.After(cutoff) {
			return nil
		}
		doomed = append(doomed, o)
		return nil
	}); err != nil {
		return stats, err
	}
	for _, o := range doomed {
		stats.Chunks++
		stats.Bytes += o.Size
	}
	if opts.DryRun {
		info("%d unreferenced chunks (%s) would be deleted", stats.Chunks, human(stats.Bytes))
		return stats, nil
	}
	if err := r.deleteAll(ctx, doomed); err != nil {
		return stats, err
	}
	info("deleted %d unreferenced chunks (%s), kept %d", stats.Chunks, human(stats.Bytes), stats.ChunksKept)
	return stats, nil
}

// Forget removes every manifest of one sprite, and its tombstone, now, whatever
// the retention says. The chunks are left for the next Prune, which is the only
// thing that knows whether another sprite shares them.
func (r *Repo) Forget(ctx context.Context, spriteID string) (manifests int, err error) {
	stamps, err := r.Stamps(ctx, spriteID)
	if err != nil {
		return 0, err
	}
	// The pointer first: a sprite with manifests and no latest.json is merely
	// untidy, one with a latest.json naming a manifest that is gone is misleading.
	for _, key := range []string{latestKey(spriteID), deletedKey(spriteID)} {
		if err := r.cl.Delete(ctx, key); err != nil {
			return 0, err
		}
	}
	for _, stamp := range stamps {
		if err := r.cl.Delete(ctx, manifestKey(spriteID, stamp)); err != nil {
			return manifests, err
		}
		manifests++
	}
	return manifests, nil
}

func (r *Repo) putMarker(ctx context.Context, m pruneMarker) error {
	body, _ := json.Marshal(m)
	if err := r.cl.Put(ctx, pruneMarkerKey, body); err != nil {
		return fmt.Errorf("write %s: %w", pruneMarkerKey, err)
	}
	return nil
}

// manifestsToDrop decides what a sprite loses: everything, if it was deleted
// longer ago than the retention; otherwise the oldest manifests beyond Keep.
func (r *Repo) manifestsToDrop(sp SpriteInfo, opts PruneOptions) (drop []string, retire bool) {
	if sp.Deleted != nil && opts.Retention > 0 && time.Since(sp.Deleted.DeletedAt) > opts.Retention {
		return sp.Stamps, true
	}
	if opts.Keep > 0 && len(sp.Stamps) > opts.Keep {
		return sp.Stamps[:len(sp.Stamps)-opts.Keep], false
	}
	return nil, false
}

// deleteAll removes objects with the same parallelism as an upload. Garage has no
// batch delete in the subset this client implements, and single DELETEs are cheap.
func (r *Repo) deleteAll(ctx context.Context, objs []s3.Object) error {
	var (
		wg    sync.WaitGroup
		mu    sync.Mutex
		first error
	)
	sem := make(chan struct{}, r.par)
	for _, o := range objs {
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
			wg.Wait()
			return ctx.Err()
		}
		wg.Add(1)
		go func(o s3.Object) {
			defer wg.Done()
			defer func() { <-sem }()
			if err := r.cl.Delete(ctx, o.Key); err != nil {
				mu.Lock()
				if first == nil {
					first = fmt.Errorf("delete %s: %w", o.Key, err)
				}
				mu.Unlock()
				return
			}
			if id, ok := chunkIDFromKey(o.Key); ok {
				r.forget(id)
			}
		}(o)
	}
	wg.Wait()
	return first
}
