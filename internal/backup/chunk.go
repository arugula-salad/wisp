package backup

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

// ChunkSize is the slice a disk image is cut into. Fixed size at fixed offsets,
// not content-defined: ext4 writes in place, so nothing ever shifts and a rolling
// hash would buy nothing for a lot of CPU. It also means the Nth chunk of a
// checkpoint and the Nth chunk of the disk it was cloned from are byte-identical
// wherever the guest has not written, which is where nearly all the dedup comes
// from.
const ChunkSize = 4 << 20

// walkChunks calls fn for every chunk-aligned slice of path that holds data,
// skipping sparse holes and slices that are entirely zero. It returns the bytes
// actually read. buf is reused between calls, so fn must copy what it keeps.
func walkChunks(ctx context.Context, path string, lim *limiter, fn func(off int64, buf []byte) error) (int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return 0, err
	}
	size := st.Size()

	buf := make([]byte, ChunkSize)
	var read int64
	for off := int64(0); off < size; off += ChunkSize {
		if err := ctx.Err(); err != nil {
			return read, err
		}
		// Jump straight to the next region that holds data. A 20 GB sparse disk
		// with 2 GB written costs 2 GB of reads, not 20.
		if next, ok := nextData(f, off); ok {
			if next >= size {
				break
			}
			off = next - next%ChunkSize
		}
		n, err := readChunkAt(f, buf, off, size)
		if err != nil {
			return read, err
		}
		read += int64(n)
		if err := lim.wait(ctx, n); err != nil {
			return read, err
		}
		if allZero(buf[:n]) {
			continue // restores as a hole
		}
		if err := fn(off, buf[:n]); err != nil {
			return read, err
		}
	}
	return read, nil
}

// nextData returns the offset of the next data region at or after off. ok is
// false when the filesystem cannot answer, in which case every chunk is read.
func nextData(f *os.File, off int64) (int64, bool) {
	next, err := unix.Seek(int(f.Fd()), off, unix.SEEK_DATA)
	if err == nil {
		return next, true
	}
	if errors.Is(err, unix.ENXIO) {
		// No data left at all: point the caller past the end.
		if st, serr := f.Stat(); serr == nil {
			return st.Size(), true
		}
	}
	return 0, false
}

// readChunkAt fills buf from off, returning a short slice only for the final
// chunk of the file.
func readChunkAt(f *os.File, buf []byte, off, size int64) (int, error) {
	want := int64(len(buf))
	if rest := size - off; rest < want {
		want = rest
	}
	n, err := f.ReadAt(buf[:want], off)
	if err != nil && !errors.Is(err, io.EOF) {
		return n, fmt.Errorf("read %s at %d: %w", f.Name(), off, err)
	}
	return n, nil
}

func allZero(b []byte) bool {
	for _, c := range b {
		if c != 0 {
			return false
		}
	}
	return true
}

// limiter is a byte/second token bucket. A nil limiter is unlimited, which keeps
// the call sites free of conditionals.
type limiter struct {
	rate float64

	mu     sync.Mutex
	tokens float64
	last   time.Time
}

func newLimiter(bytesPerSec int64) *limiter {
	if bytesPerSec <= 0 {
		return nil
	}
	return &limiter{rate: float64(bytesPerSec), tokens: float64(bytesPerSec), last: time.Now()}
}

func (l *limiter) wait(ctx context.Context, n int) error {
	if l == nil || n <= 0 {
		return nil
	}
	l.mu.Lock()
	now := time.Now()
	l.tokens += now.Sub(l.last).Seconds() * l.rate
	l.last = now
	// One chunk may exceed a second's budget; allow it to go negative and pay the
	// debt off rather than deadlocking on a rate below the chunk size.
	if l.tokens > l.rate {
		l.tokens = l.rate
	}
	l.tokens -= float64(n)
	var sleep time.Duration
	if l.tokens < 0 {
		sleep = time.Duration(-l.tokens / l.rate * float64(time.Second))
	}
	l.mu.Unlock()

	if sleep <= 0 {
		return nil
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(sleep):
		return nil
	}
}
