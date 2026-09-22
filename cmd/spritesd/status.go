package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/jhgaylor/mini-sprites/internal/server"
)

// `spritesd status`: what is on this host. It asks the running daemon over the
// unix socket in the data directory, and falls back to reading the files when
// there is none, so it needs neither the API token nor a daemon.

// listenStatus opens the operator socket. A daemon already answering there
// owns this data directory; two of them would fight over every sprite in it.
func listenStatus(dataDir string) (net.Listener, error) {
	path := filepath.Join(dataDir, server.StatusSocket)
	if c, err := net.DialTimeout("unix", path, time.Second); err == nil {
		c.Close()
		return nil, fmt.Errorf("another spritesd is already running on %s (its socket %s answers)", dataDir, path)
	}
	os.Remove(path) // left behind by a daemon that died
	old := syscall.Umask(0o177)
	defer syscall.Umask(old)
	return net.Listen("unix", path)
}

func fetchStatus(dataDir string) (server.Status, error) {
	path := filepath.Join(dataDir, server.StatusSocket)
	client := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", path)
		}}}
	var st server.Status
	resp, err := client.Get("http://spritesd/status")
	if err != nil {
		return st, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return st, fmt.Errorf("daemon answered %s", resp.Status)
	}
	return st, json.NewDecoder(resp.Body).Decode(&st)
}

func runStatus(args []string) int {
	fs := flag.NewFlagSet("status", flag.ExitOnError)
	data := fs.String("data", defaultDataDir(), "data directory")
	asJSON := fs.Bool("json", false, "print the full status as JSON")
	netdSocket := fs.String("netd-socket", "", "mini-sprites-netd socket to look for when no daemon is running (default /run/mini-sprites/netd.sock)")
	fs.Parse(args)
	abs, err := filepath.Abs(*data)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	st, err := fetchStatus(abs)
	if err != nil {
		// No socket, or one nobody listens on, means no daemon. Anything else (a
		// permission problem, a daemon that hangs) should not pass for that.
		if !errors.Is(err, syscall.ENOENT) && !errors.Is(err, syscall.ECONNREFUSED) {
			fmt.Fprintf(os.Stderr, "asking the daemon at %s: %v\n", filepath.Join(abs, server.StatusSocket), err)
			return 1
		}
		if st, err = server.OfflineStatus(abs, *netdSocket); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
	}
	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		enc.Encode(st)
		return 0
	}
	printStatus(os.Stdout, st)
	return 0
}

// daemonDataDir works out which data directory another spritesd serves, from
// its command line. (Its environment may differ from ours; that much is a guess.)
func daemonDataDir(d server.OtherDaemon) string {
	dir := defaultDataDir()
	for i, a := range d.Cmd {
		a = strings.TrimLeft(a, "-")
		if v, ok := strings.CutPrefix(a, "data="); ok {
			dir = v
		} else if a == "data" && i+1 < len(d.Cmd) {
			dir = d.Cmd[i+1]
		}
	}
	if !filepath.IsAbs(dir) {
		dir = filepath.Join(d.Cwd, dir)
	}
	return filepath.Clean(dir)
}

func size(n int64) string {
	switch {
	case n >= 10<<30:
		return fmt.Sprintf("%dG", n>>30)
	case n >= 1<<30:
		return fmt.Sprintf("%.1fG", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%dM", n>>20)
	case n > 0:
		return fmt.Sprintf("%dK", max(n>>10, 1))
	}
	return "0"
}

// lease renders a workspace lease for the table: nothing for the persistent
// sprites, which are still the default, and a countdown for the rest.
func lease(expires *time.Time, protected bool) string {
	if expires == nil {
		return "-"
	}
	d := time.Until(*expires)
	switch {
	case protected:
		return "held"
	case d <= 0:
		return "due"
	case d < time.Hour:
		return d.Round(time.Second).String()
	}
	return d.Round(time.Minute).String()
}

// limitMiB is limit() for the memory budget, which is counted in MiB.
func limitMiB(n int) string {
	if n <= 0 {
		return "no limit"
	}
	return fmt.Sprintf("%d MiB", n)
}

func limit(n int) string {
	if n == 0 {
		return "no limit"
	}
	return fmt.Sprintf("limit %d", n)
}

func printStatus(w io.Writer, st server.Status) {
	h := st.Host
	if d := st.Daemon; d != nil {
		fmt.Fprintf(w, "spritesd   pid %d, up %s, API on %s\n", d.Pid, time.Since(d.StartedAt).Round(time.Second), d.Listen)
	} else {
		fmt.Fprintln(w, "spritesd   NOT RUNNING on this data directory; read from its files")
		for _, d := range st.OtherDaemons {
			if daemonDataDir(d) == h.DataDir {
				fmt.Fprintf(w, "           ...except that pid %d looks like it serves this directory without the status socket (an older build?).\n", d.Pid)
				fmt.Fprintln(w, "           States below are then only what the files say: a sprite shown warm or cold may be running.")
			}
		}
	}
	fmt.Fprintf(w, "data       %s\n", h.DataDir)
	mode := "full copies (no reflink)"
	if h.Reflink {
		mode = "reflink clones"
	}
	v := h.Volume
	fmt.Fprintf(w, "volume     %s used, %s free of %s, %s", size(v.VolumeTotal-v.VolumeFree), size(v.VolumeFree), size(v.VolumeTotal), mode)
	if h.DiskReserve > 0 {
		fmt.Fprintf(w, "; %s kept in reserve", size(h.DiskReserve))
	}
	fmt.Fprintln(w)
	if v.Image != "" {
		note := ""
		if v.ImageSparse && v.HostFree < v.VolumeFree {
			note = "  <- less than the volume thinks it has: the host fills first"
		}
		fmt.Fprintf(w, "image      %s; %s free on its filesystem%s\n", v.Image, size(v.HostFree), note)
	}
	if h.Images.Count > 0 {
		fmt.Fprintf(w, "images     %d cached disk(s) from container images, %s on the volume (spritesd images list)\n", h.Images.Count, size(h.Images.Bytes))
	}
	fmt.Fprintf(w, "sprites    %d running (%s), %d warm, %d cold; %d in all (%s)\n",
		h.Running, limit(h.MaxRunning), h.Warm, h.Cold, len(st.Sprites), limit(h.MaxSprites))
	if st.Daemon != nil {
		// Reserved memory is tracked whether or not a budget is set, so it answers
		// "what would a budget have to be to hold what is running now?".
		fmt.Fprintf(w, "memory     %d MiB reserved by running sprites of %s", h.ReservedMemoryMiB, limitMiB(h.MaxRunningMemoryMiB))
		if h.MaxConcurrentBoots > 0 || h.BootsInFlight > 0 {
			fmt.Fprintf(w, "; %d cold boot(s) in flight (%s)", h.BootsInFlight, limit(h.MaxConcurrentBoots))
		}
		fmt.Fprintln(w)
	}
	if st.Daemon != nil {
		if h.Networking {
			fmt.Fprintf(w, "network    %d of %d taps in use\n", h.TapsUsed, h.TapsTotal)
		} else {
			fmt.Fprintln(w, "network    off for this daemon (--net=false, or no msbr0 bridge)")
		}
	}
	helper := "reachable"
	if !h.PolicyHelper.Reachable {
		helper = "NOT reachable"
	}
	if h.PolicyHelper.Detail != "" {
		helper += " (" + h.PolicyHelper.Detail + ")"
	}
	fmt.Fprintf(w, "policy     helper %s\n\n", helper)

	if len(st.Sprites) == 0 {
		fmt.Fprintln(w, "no sprites")
	} else {
		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "NAME\tID\tSTATE\tPID\tRSS\tDISK\tOWN\tSNAP\tCKPTS\tHOLDS\tLEASE\tIP\tPOLICY")
		for _, s := range st.Sprites {
			state := s.State
			if s.Busy {
				state += "*"
			}
			pid, rss, holds, ip, policy := "-", "-", "-", "-", "open"
			if s.VMMPid != 0 {
				pid, rss = fmt.Sprint(s.VMMPid), size(s.VMMRSS)
			}
			if s.TaskHolds != nil {
				holds = fmt.Sprint(*s.TaskHolds)
			}
			if s.IP != "" {
				ip = s.IP
			}
			if s.PolicyRestricted {
				policy = "restricted"
			}
			ckpts := fmt.Sprint(s.Checkpoints)
			if n := len(s.MountedCheckpoints); n > 0 {
				slots := []string{}
				for slot, id := range s.MountedCheckpoints {
					slots = append(slots, fmt.Sprintf("%d=%s", slot, id))
				}
				sort.Strings(slots)
				ckpts += " (mounted " + strings.Join(slots, ",") + ")"
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", s.Name, s.ID, state, pid, rss,
				size(s.DiskUsed), size(s.DiskExclusive), size(s.SnapshotBytes), ckpts, holds, lease(s.ExpiresAt, s.Protected), ip, policy)
		}
		tw.Flush()
		fmt.Fprintln(w, "\nDISK is what the sprite's disk and checkpoints occupy, shared blocks counted once; OWN is the part")
		fmt.Fprintln(w, "nothing else shares, which is what deleting it frees. SNAP is a warm sprite's memory snapshot.")
		fmt.Fprintln(w, "HOLDS counts live tasks keeping a sprite awake. LEASE is when an expiring workspace is deleted")
		fmt.Fprintln(w, "(\"held\" = protected from it). A * marks a transition in progress.")
	}

	for _, d := range st.OtherDaemons {
		fmt.Fprintf(w, "\nother spritesd: pid %d with %d VMs: %s\n", d.Pid, d.VMs, strings.Join(d.Cmd, " "))
	}
	if len(st.Orphans) > 0 {
		fmt.Fprintf(w, "\nORPHANED VMs: %d firecracker process(es) of yours that no running spritesd started. Not touched; kill them yourself if they are stale.\n", len(st.Orphans))
		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "  PID\tRSS\tPARENT\tCWD\t")
		for _, o := range st.Orphans {
			note := ""
			if o.InDataDir {
				note = "in this data directory: the next spritesd to start here reaps it"
			}
			fmt.Fprintf(tw, "  %d\t%s\t%s (%d)\t%s\t%s\n", o.Pid, size(o.RSS), o.ParentName, o.ParentPid, o.Cwd, note)
		}
		tw.Flush()
	}
}
