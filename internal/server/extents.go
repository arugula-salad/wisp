package server

import (
	"os"
	"path/filepath"
	"sort"
	"unsafe"

	"github.com/arugula-salad/wisp/internal/store"
	"github.com/arugula-salad/wisp/internal/vmm"
	"golang.org/x/sys/unix"
)

// On a reflink volume a file's allocated size says little: a fresh sprite's
// 20 GB disk shares every block with the base image, and a checkpoint shares
// most of its blocks with the disk it was cloned from. The honest numbers come
// from the extent maps: which physical ranges a sprite's files cover, and which
// of those nobody else covers.

const (
	fsIocFiemap      = 0xC020660B // FS_IOC_FIEMAP, which x/sys/unix does not carry
	fiemapExtentLast = 0x1
	fiemapBatch      = 512
)

type fiemapExtent struct {
	Logical, Physical, Length uint64
	_                         [2]uint64
	Flags                     uint32
	_                         [3]uint32
}

type fiemapReq struct {
	Start, Length                 uint64
	Flags, Mapped, ExtentCount, _ uint32
	Extents                       [fiemapBatch]fiemapExtent
}

// span is a range of physical bytes on the volume.
type span struct{ start, end uint64 }

// fileSpans returns the physical ranges behind path, or ok=false where the
// filesystem cannot say (tmpfs, for one).
func fileSpans(path string) (spans []span, ok bool) {
	f, err := os.Open(path)
	if err != nil {
		return nil, false
	}
	defer f.Close()
	var req fiemapReq
	for start := uint64(0); ; {
		req = fiemapReq{Start: start, Length: ^uint64(0) - start, ExtentCount: fiemapBatch}
		if _, _, errno := unix.Syscall(unix.SYS_IOCTL, f.Fd(), fsIocFiemap, uintptr(unsafe.Pointer(&req))); errno != 0 {
			return nil, false
		}
		if req.Mapped == 0 {
			return spans, true
		}
		for _, e := range req.Extents[:req.Mapped] {
			// Delayed allocations have no place on disk yet.
			if e.Physical != 0 {
				spans = append(spans, span{e.Physical, e.Physical + e.Length})
			}
			start = e.Logical + e.Length
			if e.Flags&fiemapExtentLast != 0 {
				return spans, true
			}
		}
	}
}

// merge sorts spans and joins the overlapping ones, so that each byte counts once.
func merge(spans []span) []span {
	sort.Slice(spans, func(i, j int) bool { return spans[i].start < spans[j].start })
	out := spans[:0]
	for _, s := range spans {
		if n := len(out); n > 0 && s.start <= out[n-1].end {
			out[n-1].end = max(out[n-1].end, s.end)
			continue
		}
		out = append(out, s)
	}
	return out
}

func total(spans []span) (n int64) {
	for _, s := range spans {
		n += int64(s.end - s.start)
	}
	return n
}

// exclusive returns, for each owner, the bytes no other owner covers: what
// deleting that owner would give back. Every owner's spans must be merged.
func exclusive(owners [][]span) []int64 {
	type edge struct {
		at    uint64
		open  bool
		owner int
	}
	var edges []edge
	for i, spans := range owners {
		for _, s := range spans {
			edges = append(edges, edge{s.start, true, i}, edge{s.end, false, i})
		}
	}
	sort.Slice(edges, func(i, j int) bool { return edges[i].at < edges[j].at })
	out := make([]int64, len(owners))
	// With one span per owner at any point, the sum of the open owners' indexes
	// names the owner whenever exactly one is open.
	var depth, sum int
	var last uint64
	for _, e := range edges {
		if depth == 1 {
			out[sum] += int64(e.at - last)
		}
		last = e.at
		if e.open {
			depth, sum = depth+1, sum+e.owner
		} else {
			depth, sum = depth-1, sum-e.owner
		}
	}
	return out
}

// diskFigures is what one sprite's files occupy on the volume.
type diskFigures struct {
	// Apparent is the size the guest sees. Used is what the sprite's disk and
	// checkpoints occupy, each shared block counted once; Exclusive is the part
	// nothing else on the volume shares, i.e. what deleting the sprite frees.
	Apparent, Used, Exclusive int64
	// Snapshot is the memory snapshot of a warm sprite.
	Snapshot int64
}

// measureDisks reads the disk figures of the sprites with these IDs, all at
// once, since what is exclusive to one depends on what the others hold.
func measureDisks(st *store.Store, vmRoot string, ids []string) []diskFigures {
	sprites := make([]diskFigures, len(ids))
	owners := make([][]span, len(ids), len(ids)+1)
	mapped := true
	for i := range sprites {
		dir := st.Dir(ids[i])
		files, _ := filepath.Glob(filepath.Join(dir, "checkpoints", "*.ext4"))
		files = append(files, filepath.Join(dir, vmm.DiskFile))
		if fi, err := os.Stat(filepath.Join(dir, vmm.DiskFile)); err == nil {
			sprites[i].Apparent = fi.Size()
		}
		var plain int64
		for _, f := range files {
			plain += allocated(f)
			spans, ok := fileSpans(f)
			mapped = mapped && ok
			owners[i] = append(owners[i], spans...)
		}
		owners[i] = merge(owners[i])
		sprites[i].Used, sprites[i].Exclusive = plain, plain
		sprites[i].Snapshot = vmm.SnapshotBytes(dir)
	}
	if !mapped {
		return sprites // no extent maps here, hence no sharing to account for either
	}
	// The base image's mirror shares blocks with every disk cloned from it, and
	// a cached image disk with every sprite made from that image.
	for _, f := range append([]string{filepath.Join(vmRoot, localBaseName)}, imageDisks(vmRoot)...) {
		if spans, ok := fileSpans(f); ok {
			owners = append(owners, merge(spans))
		}
	}
	for i, own := range exclusive(owners)[:len(sprites)] {
		sprites[i].Used, sprites[i].Exclusive = total(owners[i]), own
	}
	return sprites
}
