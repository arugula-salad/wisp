package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// Checkpoints can be browsed without restoring them: the host points one of the
// VM's spare read-only drives at the checkpoint's disk image, and we mount it at
// <state dir>/checkpoints/<id>. See internal/server/checkpoint_mounts.go.

const placeholderSectors = 1 << 11 // the 1 MiB file behind an empty slot, in 512-byte sectors

var checkpointIDRE = regexp.MustCompile(`^(v|auto-)[0-9]+$`)

func (s *Server) stateDir() string {
	if s.StateDir != "" {
		return s.StateDir
	}
	return "/.sprite"
}

func (s *Server) registerCheckpointMounts(mux *http.ServeMux, hostDial func(context.Context) (net.Conn, error)) {
	host := &http.Client{Timeout: 15 * time.Second, Transport: &http.Transport{DisableKeepAlives: true,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) { return hostDial(ctx) }}}
	// askHost relays the host's refusal (unknown checkpoint, no free slot...) to the caller as is.
	askHost := func(w http.ResponseWriter, r *http.Request, verb string, out any) bool {
		req, _ := http.NewRequestWithContext(r.Context(), http.MethodPost, "http://host/v1/checkpoints/"+r.PathValue("id")+"/"+verb, nil)
		resp, err := host.Do(req)
		if err != nil {
			writeErr(w, http.StatusBadGateway, "host_unreachable", err.Error())
			return false
		}
		defer resp.Body.Close()
		if resp.StatusCode >= 300 {
			w.Header().Set("Content-Type", resp.Header.Get("Content-Type"))
			w.WriteHeader(resp.StatusCode)
			io.Copy(w, resp.Body)
			return false
		}
		if out != nil {
			json.NewDecoder(resp.Body).Decode(out)
		}
		return true
	}

	mux.HandleFunc("POST /v1/checkpoints/{id}/mount", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if !checkpointIDRE.MatchString(id) { // it becomes a directory name
			writeErr(w, http.StatusBadRequest, "bad_request", "invalid checkpoint id")
			return
		}
		dir := filepath.Join(s.stateDir(), "checkpoints", id)
		if isMountPoint(dir) {
			writeJSON(w, http.StatusOK, map[string]string{"id": id, "path": dir})
			return
		}
		slot := struct {
			Slot int `json:"slot"`
		}{Slot: -1}
		if !askHost(w, r, "mount", &slot) {
			return
		}
		if slot.Slot < 0 {
			writeErr(w, http.StatusBadGateway, "host_unreachable", "the host did not say which slot it used")
			return
		}
		if err := mountSlot(slot.Slot, dir); err != nil {
			askHost(discard{}, r, "unmount", nil) // give the slot back
			writeErr(w, http.StatusInternalServerError, "mount_failed", err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"id": id, "path": dir})
	})

	mux.HandleFunc("POST /v1/checkpoints/{id}/unmount", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if !checkpointIDRE.MatchString(id) {
			writeErr(w, http.StatusBadRequest, "bad_request", "invalid checkpoint id")
			return
		}
		dir := filepath.Join(s.stateDir(), "checkpoints", id)
		if isMountPoint(dir) {
			if err := unix.Unmount(dir, 0); err != nil {
				// Busy means something has a file or its cwd in there. Detaching lazily would let
				// the host swap the image out from under that process, so refuse instead.
				writeErr(w, http.StatusConflict, "busy", fmt.Sprintf("%s is in use: %v", dir, err))
				return
			}
		}
		os.Remove(dir)
		if askHost(w, r, "unmount", nil) {
			w.WriteHeader(http.StatusNoContent)
		}
	})
}

// discard is a ResponseWriter for a host call whose answer nobody is waiting for.
type discard struct{}

func (discard) Header() http.Header         { return http.Header{} }
func (discard) Write(b []byte) (int, error) { return len(b), nil }
func (discard) WriteHeader(int)             {}

func isMountPoint(dir string) bool {
	b, err := os.ReadFile("/proc/self/mounts")
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(b), "\n") {
		if f := strings.Fields(line); len(f) > 1 && f[1] == dir {
			return true
		}
	}
	return false
}

// slotDevice names the block device behind a checkpoint slot. Firecracker attaches
// drives in the order they were configured, the root disk first, so slot n is the
// (n+1)th disk after vda. (The virtio serial cannot be used: Firecracker derives it
// from the backing file, so every empty slot reports the same one.)
func slotDevice(slot int) string { return "vd" + string(rune('b'+slot)) }

func sectors(dev string) int64 {
	b, _ := os.ReadFile("/sys/block/" + dev + "/size")
	n, _ := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
	return n
}

func mountSlot(slot int, dir string) error {
	dev := slotDevice(slot)
	if _, err := os.Stat("/sys/block/" + dev); err != nil {
		return fmt.Errorf("no /dev/%s: a sprite gains checkpoint slots at its next cold boot", dev)
	}
	// The host swapped the file behind the drive a moment ago; the kernel learns the new
	// size from a config-change interrupt. Waiting for it also proves this really is the
	// drive the host changed: were the naming assumption wrong, this device would stay
	// placeholder-sized and we fail here rather than mount some other disk.
	deadline := time.Now().Add(3 * time.Second)
	for sectors(dev) <= placeholderSectors {
		if time.Now().After(deadline) {
			return fmt.Errorf("/dev/%s never grew to the checkpoint's size", dev)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	// noload: the image was captured after a sync with the VM paused, but its journal may
	// still look dirty, and a read-only device cannot replay one.
	if err := unix.Mount("/dev/"+dev, dir, "ext4", unix.MS_RDONLY|unix.MS_NOSUID|unix.MS_NODEV, "noload"); err != nil {
		os.Remove(dir)
		return fmt.Errorf("mount /dev/%s: %w", dev, err)
	}
	return nil
}

// handleNetPolicyFile publishes the sprite's network policy where upstream puts it,
// for the benefit of tools inside the sprite. It is information only: enforcement
// happens on the host, and nothing here can loosen it.
func (s *Server) handleNetPolicyFile(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil || !json.Valid(body) {
		writeErr(w, http.StatusBadRequest, "bad_request", "invalid JSON body")
		return
	}
	dir := filepath.Join(s.stateDir(), "policy")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	tmp := filepath.Join(dir, ".network.json.tmp")
	if err := os.WriteFile(tmp, append(body, '\n'), 0o444); err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	os.Chmod(tmp, 0o444)
	if err := os.Rename(tmp, filepath.Join(dir, "network.json")); err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
