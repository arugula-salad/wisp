package backup

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/jhgaylor/mini-sprites/internal/store"
)

// Restore rebuilds a machine directory from a manifest. Each file is created,
// truncated to its recorded size and then written chunk by chunk, so every offset
// the manifest does not mention stays a hole and the result is as sparse as the
// original. Every chunk is verified against its content address on the way in.
func (r *Repo) Restore(ctx context.Context, m *Manifest, dir string, info func(string, ...any)) (Stats, error) {
	start := time.Now()
	var stats Stats
	if info == nil {
		info = func(string, ...any) {}
	}
	if m.ChunkSize != ChunkSize {
		return stats, fmt.Errorf("manifest uses %d-byte chunks, this build uses %d", m.ChunkSize, ChunkSize)
	}
	if m.Encrypted != (r.cr != nil) {
		return stats, fmt.Errorf("manifest is encrypted=%v but this host is configured encrypted=%v", m.Encrypted, r.cr != nil)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return stats, err
	}

	names := make([]string, 0, len(m.Files))
	for name := range m.Files {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		file := m.Files[name]
		path := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return stats, err
		}
		info("restoring %s (%s, %d chunks)", name, human(file.Size), len(file.Chunks))
		n, err := r.restoreFile(ctx, path, file)
		stats.Read += n
		stats.Chunks += len(file.Chunks)
		if err != nil {
			return stats, fmt.Errorf("%s: %w", name, err)
		}
	}
	stats.Took = time.Since(start)
	return stats, nil
}

// restoreFile writes one file. Chunks are fetched in parallel and written with
// WriteAt, so the download is not serialised behind the disk.
func (r *Repo) restoreFile(ctx context.Context, path string, file File) (int64, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	// Set the length first: the holes are the parts nothing is written to.
	if err := f.Truncate(file.Size); err != nil {
		return 0, err
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		firstErr error
		written  int64
	)
	sem := make(chan struct{}, r.par)
	for _, c := range file.Chunks {
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
			wg.Wait()
			return written, ctx.Err()
		}
		wg.Add(1)
		go func(c Chunk) {
			defer wg.Done()
			defer func() { <-sem }()
			body, err := r.getChunk(ctx, c.ID)
			if err == nil && len(body) != c.Len {
				err = fmt.Errorf("chunk %s is %d bytes, manifest says %d", c.ID, len(body), c.Len)
			}
			if err == nil {
				_, err = f.WriteAt(body, c.Off)
			}
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				if firstErr == nil {
					firstErr = err
					cancel() // stop the rest: a partial disk is not worth downloading
				}
				return
			}
			written += int64(c.Len)
		}(c)
	}
	wg.Wait()
	mu.Lock()
	defer mu.Unlock()
	if firstErr != nil {
		return written, firstErr
	}
	return written, f.Sync()
}

// RestoreRecord is the store record to write for a restored sprite, with the
// fields that belong to the host it came from cleared: the address is reallocated
// here, and there is no memory snapshot, so it boots cold.
func RestoreRecord(m *Manifest) store.Sprite {
	sp := m.Sprite
	sp.NetIndex = 0 // store.Create allocates one that is free on this host
	sp.BootIP = ""  // no snapshot to be consistent with
	sp.LastWarmingAt = nil
	sp.Mounts = nil // checkpoint slots are backed by placeholders in a fresh VM
	// The record and the files were not captured in the same instant. A checkpoint
	// the record lists but the manifest has no image for would restore as one that
	// fails when it is used, so it is not restored at all.
	var kept []store.Checkpoint
	for _, cp := range sp.Checkpoints {
		if _, ok := m.Files["checkpoints/"+cp.ID+".ext4"]; ok {
			kept = append(kept, cp)
		}
	}
	sp.Checkpoints = kept
	return sp
}
