package server

import (
	"bufio"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/jhgaylor/mini-sprites/internal/store"
	"github.com/jhgaylor/mini-sprites/internal/vmm"
)

// The sprite volume holds disks, checkpoints and one guest-RAM-sized snapshot
// per warm sprite. As set up by scripts/setup-storage.sh it is a sparse image on
// a loop device, so running out is not a clean ENOSPC: once the image, or the
// host filesystem under it, is full, guests see I/O errors. The guard refuses
// what cannot fit before it starts, and makes room for the one thing that must
// not fail, a suspend, by turning the oldest warm sprites cold.

// snapshotSlack covers what a suspend writes besides guest RAM (vmstate, metadata).
const snapshotSlack = 64 << 20

// errNoRoom is a create, checkpoint or restore refused for lack of space.
var errNoRoom = errors.New("not enough free space on the sprite volume")

// Headroom is how much can still be written under the sprite directory.
type Headroom struct {
	// Volume is the filesystem holding the sprite directory.
	VolumeTotal int64 `json:"volume_total_bytes"`
	VolumeFree  int64 `json:"volume_free_bytes"`
	// Image is the file behind the volume when it is a loop mount, and HostFree
	// what is left on the filesystem holding it. A sparse image only gets its
	// blocks as they are written, so the host can run out first.
	Image       string `json:"image,omitempty"`
	ImageSparse bool   `json:"image_sparse,omitempty"`
	HostFree    int64  `json:"host_free_bytes,omitempty"`
	// Free is the smaller of the two: what a write can actually count on.
	Free int64 `json:"free_bytes"`
}

type diskGuard struct {
	log     *slog.Logger
	dir     string
	reserve int64 // creates, checkpoints and restores must leave this much free
	warnPct int   // warn below this share of the volume (or of the image, for the host)
	probe   func() (Headroom, error)

	mu       sync.Mutex
	low      bool
	lastWarn time.Time
}

func newDiskGuard(opts Options, log *slog.Logger) *diskGuard {
	g := &diskGuard{log: log, dir: filepath.Join(opts.DataDir, "vm"), reserve: opts.DiskReserve, warnPct: opts.DiskWarnPercent}
	g.probe = func() (Headroom, error) { return probeHeadroom(g.dir) }
	return g
}

func fsFree(path string) (total, free int64, err error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, 0, err
	}
	return int64(st.Blocks) * st.Bsize, int64(st.Bavail) * st.Bsize, nil
}

// allocated is the space a file really occupies, which for a sparse image or a
// fresh reflink is far below its size.
func allocated(path string) int64 {
	var st syscall.Stat_t
	if syscall.Stat(path, &st) != nil {
		return 0
	}
	return st.Blocks * 512
}

// loopImage returns the file behind dir's filesystem when that is a loop mount.
func loopImage(dir string) string {
	f, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return ""
	}
	defer f.Close()
	var source string
	best := -1
	for sc := bufio.NewScanner(f); sc.Scan(); {
		// "36 35 98:0 /root /mount/point opts ... - fstype source superopts"
		pre, post, ok := strings.Cut(sc.Text(), " - ")
		fields, tail := strings.Fields(pre), strings.Fields(post)
		if !ok || len(fields) < 5 || len(tail) < 2 {
			continue
		}
		mnt := strings.ReplaceAll(fields[4], `\040`, " ")
		if (dir == mnt || strings.HasPrefix(dir, strings.TrimSuffix(mnt, "/")+"/")) && len(mnt) > best {
			best, source = len(mnt), tail[1]
		}
	}
	if !strings.HasPrefix(source, "/dev/loop") {
		return ""
	}
	b, err := os.ReadFile("/sys/block/" + filepath.Base(source) + "/loop/backing_file")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func probeHeadroom(dir string) (Headroom, error) {
	var h Headroom
	var err error
	if h.VolumeTotal, h.VolumeFree, err = fsFree(dir); err != nil {
		return h, err
	}
	h.Free = h.VolumeFree
	if h.Image = loopImage(dir); h.Image == "" {
		return h, nil
	}
	if st, err := os.Stat(h.Image); err == nil {
		h.ImageSparse = allocated(h.Image) < st.Size()
	}
	if _, free, err := fsFree(filepath.Dir(h.Image)); err == nil {
		h.HostFree = free
		if h.ImageSparse {
			// Blocks freed inside the volume go back to the host (it is mounted with
			// discard), so whichever is smaller is the real ceiling.
			h.Free = min(h.Free, free)
		}
	}
	return h, nil
}

// admit refuses an operation that would write need bytes unless the reserve
// survives it. A probe that fails is not a reason to refuse work.
func (g *diskGuard) admit(what string, need int64) error {
	h, err := g.probe()
	if err != nil || g.reserve <= 0 || h.Free-need >= g.reserve {
		return nil
	}
	where := "the sprite volume"
	if h.Free < h.VolumeFree {
		where = "the filesystem holding " + h.Image
	}
	return fmt.Errorf("%w: %s needs %s and %s has %s free, of which %s is kept in reserve (--disk-reserve-mib); delete sprites or checkpoints",
		errNoRoom, what, mib(need), where, mib(h.Free), mib(g.reserve))
}

func mib(n int64) string { return fmt.Sprintf("%d MiB", n>>20) }

// watch logs when headroom drops below the warning share, again every ten
// minutes while it stays there, and once when it recovers.
func (g *diskGuard) watch() {
	h, err := g.probe()
	if err != nil || g.warnPct <= 0 || h.VolumeTotal == 0 {
		return
	}
	low := h.VolumeFree < h.VolumeTotal/100*int64(g.warnPct)
	if h.ImageSparse {
		low = low || h.HostFree < h.VolumeTotal/100*int64(g.warnPct)
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	switch {
	case low && (!g.low || time.Since(g.lastWarn) > 10*time.Minute):
		g.lastWarn = time.Now()
		g.log.Warn("sprite volume is running out of space: creates and checkpoints will be refused, and warm sprites turned cold, before it fills",
			"volume_free", mib(h.VolumeFree), "volume_size", mib(h.VolumeTotal), "image", h.Image, "host_free", mib(h.HostFree))
	case !low && g.low:
		g.log.Info("sprite volume has headroom again", "volume_free", mib(h.VolumeFree))
	}
	g.low = low
}

// cloneCost is what cloning src onto the sprite volume writes up front: nothing
// for a reflink, the file's allocated blocks for a copy.
func (s *Server) cloneCost(src string) int64 {
	if s.storage.reflink {
		return 0
	}
	return allocated(src)
}

func writeNoRoom(w http.ResponseWriter, err error) {
	writeErr(w, http.StatusInsufficientStorage, "insufficient_storage", err.Error())
}

// makeRoom is asked before a suspend writes need bytes of snapshot. When that
// does not fit it turns warm sprites cold, longest-suspended first, which costs
// them only memory state. It reports whether the snapshot now fits.
func (l *Lifecycle) makeRoom(sp store.Sprite, need int64) bool {
	h, err := l.disk.probe()
	if err != nil || h.Free >= need {
		return true
	}
	warm := []store.Sprite{}
	for _, o := range l.store.List("") {
		if o.ID != sp.ID && o.LastWarmingAt != nil {
			warm = append(warm, o)
		}
	}
	sort.Slice(warm, func(i, j int) bool { return warm[i].LastWarmingAt.Before(*warm[j].LastWarmingAt) })
	free := h.Free
	for _, o := range warm {
		if free >= need {
			break
		}
		// The caller holds its own sprite's lock, so never wait for another's.
		rt := l.rt(o.ID)
		if !rt.mu.TryLock() {
			continue
		}
		if dir := l.store.Dir(o.ID); rt.m == nil && vmm.HasSnapshot(dir) {
			got := vmm.SnapshotBytes(dir)
			vmm.DiscardSnapshot(dir)
			free += got
			l.log.Warn("sprite turned cold to make room for another's memory snapshot", "sprite", o.Name, "for", sp.Name, "freed", mib(got))
		}
		rt.mu.Unlock()
	}
	return free >= need
}
