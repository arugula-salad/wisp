// Package vmm drives one Firecracker microVM per sprite: cold boot, suspend to
// a snapshot, restore from it, and vsock connections into the guest.
package vmm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// File names inside a machine directory. Firecracker runs with the directory
// as its cwd and only ever sees these relative names: that keeps unix socket
// paths under the 108-byte sun_path limit and makes snapshots relocatable.
const (
	apiSock    = "fc.sock"
	vsockSock  = "v.sock"
	DiskFile   = "disk.ext4"
	consoleLog = "console.log"
	snapState  = "snap.vmstate"
	snapMem    = "snap.mem"
	pidFile    = "fc.pid"
)

// Host paths shared by all machines.
type Host struct {
	Firecracker string
	Kernel      string
	Initrd      string
}

// Config is the per-sprite VM shape.
type Config struct {
	Dir      string // machine directory; must be short (holds unix sockets)
	Hostname string
	VCPUs    int
	MemMiB   int
	// Network is optional: a VM without a tap boots with no NIC.
	Tap, MAC, IPCIDR, Gateway, DNS string
	AgentPort                      uint32
}

// Machine is a running Firecracker process.
type Machine struct {
	host Host
	cfg  Config
	cmd  *exec.Cmd
	http *http.Client

	exitOnce sync.Once
	exited   chan struct{}
}

// HasSnapshot reports whether dir holds a complete suspend snapshot.
func HasSnapshot(dir string) bool {
	for _, f := range []string{snapState, snapMem} {
		if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
			return false
		}
	}
	return true
}

// ReapOrphan kills a Firecracker left behind in dir by a previous spritesd that
// died without cleaning up. Its memory state is unrecoverable, so the sprite goes cold.
func ReapOrphan(dir string) {
	b, err := os.ReadFile(filepath.Join(dir, pidFile))
	if err != nil {
		return
	}
	defer os.Remove(filepath.Join(dir, pidFile))
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		return
	}
	// Guard against pid reuse: only signal it if it really is a firecracker in this dir.
	exe, _ := os.Readlink(fmt.Sprintf("/proc/%d/exe", pid))
	cwd, _ := os.Readlink(fmt.Sprintf("/proc/%d/cwd", pid))
	if strings.Contains(filepath.Base(exe), "firecracker") && cwd == dir {
		syscall.Kill(pid, syscall.SIGKILL)
	}
}

// DiscardSnapshot drops suspended memory state, turning a warm sprite cold.
func DiscardSnapshot(dir string) {
	os.Remove(filepath.Join(dir, snapState))
	os.Remove(filepath.Join(dir, snapMem))
}

func launch(h Host, cfg Config) (*Machine, error) {
	for _, s := range []string{apiSock, vsockSock} {
		os.Remove(filepath.Join(cfg.Dir, s))
	}
	console, err := os.OpenFile(filepath.Join(cfg.Dir, consoleLog), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, err
	}
	defer console.Close()

	cmd := exec.Command(h.Firecracker, "--api-sock", apiSock, "--level", "Warning")
	cmd.Dir = cfg.Dir
	cmd.Stdout, cmd.Stderr = console, console
	// Own process group so a ^C in spritesd's terminal doesn't hit the VMs directly.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start firecracker: %w", err)
	}
	os.WriteFile(filepath.Join(cfg.Dir, pidFile), []byte(strconv.Itoa(cmd.Process.Pid)), 0o644)
	sock := filepath.Join(cfg.Dir, apiSock)
	m := &Machine{host: h, cfg: cfg, cmd: cmd, exited: make(chan struct{}),
		http: &http.Client{Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", sock)
			},
		}},
	}
	go func() { cmd.Wait(); m.exitOnce.Do(func() { close(m.exited) }) }()

	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(sock); err == nil {
			return m, nil
		}
		select {
		case <-m.exited:
			return nil, fmt.Errorf("firecracker exited during startup (see %s)", filepath.Join(cfg.Dir, consoleLog))
		default:
		}
		if time.Now().After(deadline) {
			m.Kill()
			return nil, errors.New("timed out waiting for firecracker API socket")
		}
		time.Sleep(time.Millisecond)
	}
}

func (m *Machine) api(ctx context.Context, method, path string, body any) error {
	b, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://firecracker"+path, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := m.http.Do(req)
	if err != nil {
		return fmt.Errorf("firecracker %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("firecracker %s %s: %s: %s", method, path, resp.Status, strings.TrimSpace(string(msg)))
	}
	return nil
}

type obj = map[string]any

// Boot cold-starts a VM from the kernel + initramfs with the sprite's disk attached.
func Boot(ctx context.Context, h Host, cfg Config) (*Machine, error) {
	m, err := launch(h, cfg)
	if err != nil {
		return nil, err
	}
	args := []string{"console=ttyS0", "reboot=k", "panic=1", "pci=off", "quiet", "loglevel=3",
		"i8042.noaux", "i8042.nomux", "i8042.dumbkbd",
		"sprite.root=/dev/vda", "sprite.hostname=" + cfg.Hostname}
	if cfg.Tap != "" {
		args = append(args, "sprite.ip="+cfg.IPCIDR, "sprite.gw="+cfg.Gateway, "sprite.dns="+cfg.DNS)
	}
	steps := []struct {
		path string
		body obj
	}{
		{"/boot-source", obj{"kernel_image_path": h.Kernel, "initrd_path": h.Initrd, "boot_args": strings.Join(args, " ")}},
		{"/drives/root", obj{"drive_id": "root", "path_on_host": DiskFile, "is_root_device": false, "is_read_only": false}},
		{"/machine-config", obj{"vcpu_count": cfg.VCPUs, "mem_size_mib": cfg.MemMiB}},
		{"/vsock", obj{"guest_cid": 3, "uds_path": vsockSock}},
		{"/entropy", obj{}},
	}
	if cfg.Tap != "" {
		steps = append(steps, struct {
			path string
			body obj
		}{"/network-interfaces/eth0", obj{"iface_id": "eth0", "host_dev_name": cfg.Tap, "guest_mac": cfg.MAC}})
	}
	for _, s := range steps {
		if err := m.api(ctx, http.MethodPut, s.path, s.body); err != nil {
			m.Kill()
			return nil, err
		}
	}
	if err := m.api(ctx, http.MethodPut, "/actions", obj{"action_type": "InstanceStart"}); err != nil {
		m.Kill()
		return nil, err
	}
	return m, nil
}

// Restore resumes a VM from the snapshot in cfg.Dir. The snapshot files are
// unlinked once loaded: guest memory stays mapped from the open file, and the
// next Suspend must not write over the file backing the VM that is writing it.
func Restore(ctx context.Context, h Host, cfg Config) (*Machine, error) {
	m, err := launch(h, cfg)
	if err != nil {
		return nil, err
	}
	body := obj{
		"snapshot_path": snapState,
		"mem_backend":   obj{"backend_type": "File", "backend_path": snapMem},
		"resume_vm":     true,
	}
	if cfg.Tap != "" {
		body["network_overrides"] = []obj{{"iface_id": "eth0", "host_dev_name": cfg.Tap}}
	}
	if err := m.api(ctx, http.MethodPut, "/snapshot/load", body); err != nil {
		m.Kill()
		return nil, err
	}
	DiscardSnapshot(cfg.Dir)
	return m, nil
}

// Pause and Resume freeze/unfreeze vCPUs (used around disk checkpoints).
func (m *Machine) Pause(ctx context.Context) error {
	return m.api(ctx, http.MethodPatch, "/vm", obj{"state": "Paused"})
}

func (m *Machine) Resume(ctx context.Context) error {
	return m.api(ctx, http.MethodPatch, "/vm", obj{"state": "Resumed"})
}

// Suspend snapshots the VM to disk and exits the VMM. On error the VM is resumed.
func (m *Machine) Suspend(ctx context.Context) error {
	if err := m.Pause(ctx); err != nil {
		return err
	}
	tmpState, tmpMem := snapState+".tmp", snapMem+".tmp"
	err := m.api(ctx, http.MethodPut, "/snapshot/create", obj{
		"snapshot_type": "Full", "snapshot_path": tmpState, "mem_file_path": tmpMem,
	})
	if err != nil {
		os.Remove(filepath.Join(m.cfg.Dir, tmpState))
		os.Remove(filepath.Join(m.cfg.Dir, tmpMem))
		if rerr := m.Resume(ctx); rerr != nil {
			return fmt.Errorf("%w (and resume failed: %v)", err, rerr)
		}
		return err
	}
	m.Kill()
	// Publish only a complete snapshot, so a crash mid-write reads as cold, not corrupt.
	if err := os.Rename(filepath.Join(m.cfg.Dir, tmpMem), filepath.Join(m.cfg.Dir, snapMem)); err != nil {
		return err
	}
	return os.Rename(filepath.Join(m.cfg.Dir, tmpState), filepath.Join(m.cfg.Dir, snapState))
}

// Kill stops the VMM immediately. Guest state that was not synced is lost.
func (m *Machine) Kill() {
	if m.cmd.Process != nil {
		m.cmd.Process.Kill()
	}
	<-m.exited
	os.Remove(filepath.Join(m.cfg.Dir, pidFile))
	os.Remove(filepath.Join(m.cfg.Dir, apiSock))
	os.Remove(filepath.Join(m.cfg.Dir, vsockSock))
}

// ListenGuest accepts streams the guest opens to the host. Firecracker maps a
// guest connect() to CID 2 port N onto the unix socket "<uds_path>_N"; it looks
// the socket up per connection, so the listener only has to exist by the time
// the guest first uses it, and one made before a snapshot restore works the same.
// Closing the listener removes the socket file.
func ListenGuest(dir string, port uint32) (net.Listener, error) {
	path := filepath.Join(dir, fmt.Sprintf("%s_%d", vsockSock, port))
	os.Remove(path) // left behind by a spritesd that died
	return net.Listen("unix", path)
}

// Exited is closed when the VMM process ends (guest reboot/poweroff, crash, or Kill).
func (m *Machine) Exited() <-chan struct{} { return m.exited }

// Dial opens a stream to the guest agent using Firecracker's host-initiated
// vsock handshake: connect to the UDS, send "CONNECT <port>", expect "OK <n>".
func (m *Machine) Dial(ctx context.Context) (net.Conn, error) {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", filepath.Join(m.cfg.Dir, vsockSock))
	if err != nil {
		return nil, err
	}
	if dl, ok := ctx.Deadline(); ok {
		conn.SetDeadline(dl)
	} else {
		conn.SetDeadline(time.Now().Add(5 * time.Second))
	}
	if _, err := fmt.Fprintf(conn, "CONNECT %d\n", m.cfg.AgentPort); err != nil {
		conn.Close()
		return nil, err
	}
	// Read the reply byte-wise: anything after the newline belongs to the caller.
	var line string
	for b := make([]byte, 1); !strings.HasSuffix(line, "\n") && len(line) < 64; {
		if _, err := conn.Read(b); err != nil {
			conn.Close()
			return nil, fmt.Errorf("vsock handshake: %w", err)
		}
		line += string(b)
	}
	if !strings.HasPrefix(line, "OK ") {
		conn.Close()
		return nil, fmt.Errorf("vsock handshake: %q", strings.TrimSpace(line))
	}
	conn.SetDeadline(time.Time{})
	return conn, nil
}
