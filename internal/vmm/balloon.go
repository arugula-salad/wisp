package vmm

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Every VM boots with a virtio-balloon device. It does three things:
//
//   - Free page reporting: the guest kernel hands pages it has freed back to
//     Firecracker, which MADV_DONTNEEDs them. They stop costing host RAM and read
//     back as zeros, so the next suspend's memory file has holes there (see
//     sparsify). Reporting runs continuously in the guest (in 2 MiB blocks, about
//     2 s after the pages are freed) with no host involvement.
//   - Statistics, which the autoscale controller reads to see memory pressure.
//   - An inflatable balloon, which autoscale uses to hold a sprite below its RAM
//     ceiling. deflate_on_oom lets the guest take pages back from the balloon
//     rather than OOM-kill when the controller has not deflated in time.
//
// Free page hinting (host-triggered reporting) is deliberately off: Firecracker
// ships it as a developer preview with a documented race that can discard a page
// the guest has already reused.
const (
	balloonStatsInterval = 1 // seconds; stats cannot be switched on after boot
)

func balloonConfig(reporting bool) obj {
	return obj{"amount_mib": 0, "deflate_on_oom": true,
		"stats_polling_interval_s": balloonStatsInterval, "free_page_reporting": reporting}
}

// BalloonStats is the subset of Firecracker's /balloon/statistics we use.
// Memory figures are in bytes as the guest reports them; the guest is not
// trusted, so they steer autoscale but bound nothing.
type BalloonStats struct {
	TargetMiB   int   `json:"target_mib"`
	ActualMiB   int   `json:"actual_mib"`
	FreeMemory  int64 `json:"free_memory"`
	TotalMemory int64 `json:"total_memory"`
	Available   int64 `json:"available_memory"`
	DiskCaches  int64 `json:"disk_caches"`
}

// ErrNoBalloon is returned for a VM restored from a snapshot taken before VMs
// had a balloon; it has one from the sprite's next cold boot.
var ErrNoBalloon = fmt.Errorf("vm has no balloon device")

// SetBalloon sets the balloon's target size: MiB of guest RAM taken away from
// the guest. The guest driver inflates or deflates towards it on its own time.
func (m *Machine) SetBalloon(ctx context.Context, mib int) error {
	return m.api(ctx, http.MethodPatch, "/balloon", obj{"amount_mib": mib})
}

// Balloon reads the balloon's current target, actual size and guest memory statistics.
func (m *Machine) Balloon(ctx context.Context) (BalloonStats, error) {
	var st BalloonStats
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://firecracker/balloon/statistics", nil)
	if err != nil {
		return st, err
	}
	resp, err := m.http.Do(req)
	if err != nil {
		return st, fmt.Errorf("firecracker GET /balloon/statistics: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusBadRequest {
		return st, ErrNoBalloon
	}
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return st, fmt.Errorf("firecracker GET /balloon/statistics: %s: %s", resp.Status, strings.TrimSpace(string(msg)))
	}
	return st, json.NewDecoder(resp.Body).Decode(&st)
}

// squeezeMarginMiB is free memory left to the guest by Squeeze, for whatever
// runs between the squeeze and the pause, and right after a resume.
const squeezeMarginMiB = 64

// Squeeze inflates the balloon over the guest's free memory just before a
// snapshot, so that memory is zero (a hole) in the memory file. Free page
// reporting gets there too, but only for 2 MiB blocks and only some seconds
// after they are freed. The balloon takes free 4 KiB pages at once and does not
// push out the guest's page cache. Whoever resumes the snapshot sets the balloon
// back (SetBalloon); until then the guest can take pages back under pressure
// (deflate_on_oom). It returns the balloon size reached, in MiB.
func (m *Machine) Squeeze(ctx context.Context) (int, error) {
	// The guest's free memory figure excludes the balloon, so it only adds up
	// once the balloon has stopped moving (it may still be deflating from the
	// last resume).
	st, err := m.settleBalloon(ctx, -1)
	if err != nil {
		return 0, err
	}
	// The guest refreshes its statistics once per interval; re-setting the
	// interval asks for a refresh now, as the ones we have may predate a free.
	m.api(ctx, http.MethodPatch, "/balloon/statistics", obj{"stats_polling_interval_s": balloonStatsInterval})
	time.Sleep(20 * time.Millisecond)
	if st, err = m.Balloon(ctx); err != nil {
		return 0, err
	}
	target := min(st.ActualMiB+int(st.FreeMemory>>20), m.cfg.MemMiB) - squeezeMarginMiB
	if target <= st.TargetMiB {
		return st.ActualMiB, nil
	}
	if err := m.SetBalloon(ctx, target); err != nil {
		return 0, err
	}
	st, err = m.settleBalloon(ctx, target)
	return st.ActualMiB, err
}

// settleBalloon waits, briefly, for the balloon to reach target (-1: its
// current target) or to stop moving. The driver moves ~8 GiB/s when the pages
// are there; when they are not it retries every 200 ms, which reads as a stall.
func (m *Machine) settleBalloon(ctx context.Context, target int) (BalloonStats, error) {
	st, err := m.Balloon(ctx)
	last, stalled := st.ActualMiB, 0
	for deadline := time.Now().Add(2 * time.Second); err == nil && time.Now().Before(deadline) && stalled < 5; {
		want := target
		if want < 0 {
			want = st.TargetMiB
		}
		if st.ActualMiB == want {
			break
		}
		time.Sleep(10 * time.Millisecond)
		if st, err = m.Balloon(ctx); st.ActualMiB == last {
			stalled++
		} else {
			last, stalled = st.ActualMiB, 0
		}
	}
	return st, err
}
