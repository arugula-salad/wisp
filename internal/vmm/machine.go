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

	"github.com/jhgaylor/mini-sprites/internal/confine"
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
	sparseMem  = "snap.mem.sparse" // being copied from snap.mem.tmp by Suspend
	pidFile    = "fc.pid"
	emptyDrive = "empty.img"
)

// CheckpointSlots is how many checkpoints a sprite can have mounted at once.
// Firecracker cannot hot-plug a drive, but it can swap the file behind one, so
// every VM boots with this many read-only drives backed by a placeholder.
const CheckpointSlots = 4

// SlotDrive is the Firecracker drive ID of a checkpoint slot. Slots are
// configured in order right after the root disk, which is how the guest agent
// knows which block device is which.
func SlotDrive(slot int) string { return fmt.Sprintf("ckpt%d", slot) }

// Host paths shared by all machines.
type Host struct {
	Firecracker string
	Kernel      string
	Initrd      string
	// Confine, when set, sandboxes every VMM: a Landlock domain that sees only
	// its own machine directory, and a cgroup that caps CPU, memory and pids.
	// nil runs Firecracker with just its own seccomp filter. See internal/confine.
	Confine *confine.Confiner
	// NoFreePageReporting boots VMs whose balloon does not report freed pages
	// (see balloon.go). It is part of the device, so it changes at a cold boot.
	NoFreePageReporting bool
}

// localtime is read by Firecracker to stamp its log lines; it is the only file
// outside the data directory that a VMM opens (verified by strace of a cold
// boot, a snapshot and a restore).
const localtime = "/etc/localtime"

// confineSpec is the set of paths this VM's VMM is allowed to touch.
func confineSpec(h Host, cfg Config) confine.Spec {
	spec := confine.Spec{
		Dir:      cfg.Dir,
		ReadOnly: []string{h.Kernel, h.Initrd, localtime},
		Devices:  []string{"/dev/kvm"},
	}
	if cfg.Tap != "" {
		// Firecracker opens /dev/net/tun and attaches by interface name. Landlock
		// cannot narrow that to one tap; see the confinement table in docs/security.md.
		spec.Devices = append(spec.Devices, "/dev/net/tun")
	}
	return spec
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
	// BootArgs are extra sprite.* kernel parameters for the guest agent; they only reach a cold boot.
	BootArgs []string
}

// Machine is a running Firecracker process.
type Machine struct {
	host Host
	cfg  Config
	cmd  *exec.Cmd
	cg   *confine.Cgroup // nil when unconfined; removed when the VMM exits
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

// SnapshotBytes is the disk space dir's suspend snapshot occupies.
func SnapshotBytes(dir string) int64 {
	var n int64
	for _, f := range []string{snapState, snapMem} {
		var st syscall.Stat_t
		if syscall.Stat(filepath.Join(dir, f), &st) == nil {
			n += st.Blocks * 512
		}
	}
	return n
}

// PidOf returns the VMM recorded in dir's pid file, if that process is still a
// Firecracker running there (pids get reused).
func PidOf(dir string) (int, bool) {
	b, err := os.ReadFile(filepath.Join(dir, pidFile))
	if err != nil {
		return 0, false
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		return 0, false
	}
	exe, _ := os.Readlink(fmt.Sprintf("/proc/%d/exe", pid))
	cwd, _ := os.Readlink(fmt.Sprintf("/proc/%d/cwd", pid))
	return pid, strings.Contains(filepath.Base(exe), "firecracker") && cwd == dir
}

// ReapOrphan kills a Firecracker left behind in dir by a previous spritesd that
// died without cleaning up. Its memory state is unrecoverable, so the sprite goes cold.
func ReapOrphan(dir string) {
	// Guard against pid reuse: only signal it if it really is a firecracker in this dir.
	if pid, ok := PidOf(dir); ok {
		syscall.Kill(pid, syscall.SIGKILL)
	}
	os.Remove(filepath.Join(dir, pidFile))
}

// DiscardSnapshot drops suspended memory state, turning a warm sprite cold.
func DiscardSnapshot(dir string) {
	os.Remove(filepath.Join(dir, snapState))
	os.Remove(filepath.Join(dir, snapMem))
	os.Remove(filepath.Join(dir, sparseMem)) // left by a spritesd that died mid-suspend
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
	// Sandbox the VMM. The shim execs Firecracker, so the pid below stays the
	// Firecracker pid and ReapOrphan keeps recognising it.
	cg, err := h.Confine.Start(cmd, confineSpec(h, cfg), filepath.Base(cfg.Dir),
		confine.Limits{VCPUs: cfg.VCPUs, MemMiB: cfg.MemMiB})
	if err != nil {
		return nil, fmt.Errorf("confine firecracker: %w", err)
	}
	if err := cmd.Start(); err != nil {
		cg.Remove()
		return nil, fmt.Errorf("start firecracker: %w", err)
	}
	// The cgroup itself outlives this fd; the VM is in it now.
	cg.Close()
	os.WriteFile(filepath.Join(cfg.Dir, pidFile), []byte(strconv.Itoa(cmd.Process.Pid)), 0o644)
	sock := filepath.Join(cfg.Dir, apiSock)
	m := &Machine{host: h, cfg: cfg, cmd: cmd, cg: cg, exited: make(chan struct{}),
		http: &http.Client{Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", sock)
			},
		}},
	}
	go func() {
		cmd.Wait()
		cg.Remove()
		m.exitOnce.Do(func() { close(m.exited) })
	}()

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
	args = append(args, cfg.BootArgs...)
	steps := []struct {
		path string
		body obj
	}{
		{"/boot-source", obj{"kernel_image_path": h.Kernel, "initrd_path": h.Initrd, "boot_args": strings.Join(args, " ")}},
		{"/drives/root", obj{"drive_id": "root", "path_on_host": DiskFile, "is_root_device": false, "is_read_only": false}},
		{"/machine-config", obj{"vcpu_count": cfg.VCPUs, "mem_size_mib": cfg.MemMiB}},
		{"/vsock", obj{"guest_cid": 3, "uds_path": vsockSock}},
		{"/entropy", obj{}},
		{"/balloon", balloonConfig(!h.NoFreePageReporting)},
	}
	// A drive needs a backing file even when it holds nothing yet.
	if f, err := os.OpenFile(filepath.Join(cfg.Dir, emptyDrive), os.O_CREATE|os.O_RDWR, 0o644); err == nil {
		f.Truncate(1 << 20)
		f.Close()
	}
	for i := 0; i < CheckpointSlots; i++ {
		id := SlotDrive(i)
		steps = append(steps, struct {
			path string
			body obj
		}{"/drives/" + id, obj{"drive_id": id, "path_on_host": emptyDrive, "is_root_device": false, "is_read_only": true}})
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

// SwapDrive points a running VM's drive at another file (relative to the machine
// directory, like every path Firecracker is given). The guest is notified and
// sees the new size. An empty path puts the placeholder back.
func (m *Machine) SwapDrive(ctx context.Context, driveID, path string) error {
	if path == "" {
		path = emptyDrive
	}
	return m.api(ctx, http.MethodPatch, "/drives/"+driveID, obj{"drive_id": driveID, "path_on_host": path})
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
	// Firecracker would fsync the whole-RAM memory file, zeros and all; the
	// files are synced below instead, once the memory file is sparse.
	err := m.api(ctx, http.MethodPut, "/snapshot/create", obj{
		"snapshot_type": "Full", "snapshot_path": tmpState, "mem_file_path": tmpMem,
		"sync_snapshot_files": false,
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
	// Keep what the guest was using, not its RAM (see sparseCopy). The copy
	// reads the same as the original, so when it cannot be made the original
	// is published instead and only disk space is lost. Deleting the original
	// unsynced drops its zeros before they reach the disk.
	mem := filepath.Join(m.cfg.Dir, tmpMem)
	if _, err := sparseCopy(mem, filepath.Join(m.cfg.Dir, sparseMem)); err == nil {
		os.Remove(mem)
		mem = filepath.Join(m.cfg.Dir, sparseMem)
	} else if err := syncFile(mem); err != nil {
		return err
	}
	if err := syncFile(filepath.Join(m.cfg.Dir, tmpState)); err != nil {
		return err
	}
	// Publish only a complete snapshot, so a crash mid-write reads as cold, not corrupt.
	if err := os.Rename(mem, filepath.Join(m.cfg.Dir, snapMem)); err != nil {
		return err
	}
	return os.Rename(filepath.Join(m.cfg.Dir, tmpState), filepath.Join(m.cfg.Dir, snapState))
}

func syncFile(path string) error {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
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

// Pid is the VMM process.
func (m *Machine) Pid() int { return m.cmd.Process.Pid }

// MemMiB is the guest RAM: the most a suspend snapshot keeps, and what it writes
// before it is made sparse.
func (m *Machine) MemMiB() int { return m.cfg.MemMiB }

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
