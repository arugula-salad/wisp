package vmm

import (
	"bytes"
	"errors"
	"io"
	"os"
	"sync"
	"sync/atomic"
)

const (
	sparsePage    = 4096
	sparseChunk   = 4 << 20 // read size; also the unit the file is split into
	sparseWorkers = 4       // memory bandwidth, not cores, is the limit
)

// errDense is sparseCopy giving up on a file that is mostly data.
var errDense = errors.New("memory file is mostly data; kept as written")

// sparseCopy copies src to a new file dst, leaving a hole wherever a whole page
// is zero, and returns the bytes of data it wrote. It gives up (errDense, and no
// dst) once that passes half of src: the copy would save little and cost the
// disk nearly a second full file for a moment. Firecracker writes a full
// snapshot's memory file byte for byte, zeros included, so that file costs the
// guest's whole RAM. Pages the guest never touched, pages it freed and
// reported, and pages held by the balloon are all zero in it, and a hole reads
// back the same: the copy has the same contents but holds only what the guest
// was using. Restore maps the file privately, so a hole costs nothing until the
// guest touches that page again.
//
// Why a copy rather than punching holes in src: ext4 and XFS write a range's
// dirty pages to disk before punching it, so every zero would still be written
// once. src was just written, unsynced, and is read back from the page cache;
// unlinking it afterwards drops its dirty pages unwritten. dst is synced.
func sparseCopy(src, dst string) (int64, error) {
	in, err := os.Open(src)
	if err != nil {
		return 0, err
	}
	defer in.Close()
	fi, err := in.Stat()
	if err != nil {
		return 0, err
	}
	size := fi.Size()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return 0, err
	}
	if err := out.Truncate(size); err != nil {
		out.Close()
		return 0, err
	}
	var (
		next, written atomic.Int64
		firstErr      error
		errOnce       sync.Once
		failed        atomic.Bool
		wg            sync.WaitGroup
	)
	fail := func(err error) { errOnce.Do(func() { firstErr = err; failed.Store(true) }) }
	for range sparseWorkers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			buf := make([]byte, sparseChunk)
			for {
				off := next.Add(sparseChunk) - sparseChunk
				if off >= size || failed.Load() {
					return
				}
				n, err := in.ReadAt(buf, off)
				if err != nil && err != io.EOF {
					fail(err)
					return
				}
				got, err := writeDataPages(out, buf[:n], off)
				if err != nil {
					fail(err)
					return
				}
				if written.Add(got) > size/2 {
					fail(errDense)
					return
				}
			}
		}()
	}
	wg.Wait()
	if firstErr == nil {
		firstErr = out.Sync()
	}
	if err := out.Close(); err != nil && firstErr == nil {
		firstErr = err
	}
	if firstErr != nil {
		os.Remove(dst)
	}
	return written.Load(), firstErr
}

var zeroPage = make([]byte, sparsePage)

// writeDataPages writes each run of non-zero pages in buf, which holds the
// file's bytes from off on, to the same offset in out. A trailing partial page
// is always written.
func writeDataPages(out *os.File, buf []byte, off int64) (int64, error) {
	var written int64
	start := -1
	flush := func(end int) error {
		if start < 0 {
			return nil
		}
		n, err := out.WriteAt(buf[start:end], off+int64(start))
		written += int64(n)
		start = -1
		return err
	}
	for p := 0; p < len(buf); p += sparsePage {
		end := min(p+sparsePage, len(buf))
		if end-p == sparsePage && bytes.Equal(buf[p:end], zeroPage) {
			if err := flush(p); err != nil {
				return written, err
			}
		} else if start < 0 {
			start = p
		}
	}
	return written, flush(len(buf))
}
