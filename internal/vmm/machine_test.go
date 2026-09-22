package vmm

import (
	"bufio"
	"context"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// These tests boot real microVMs. They need /dev/kvm and the artifacts that
// `make deps image initrd` put in the data directory, and skip otherwise.
func testHost(t *testing.T) (Host, string) {
	data := os.Getenv("MINI_SPRITES_DATA")
	if data == "" {
		home, _ := os.UserHomeDir()
		data = filepath.Join(home, ".local", "share", "mini-sprites")
	}
	h := Host{Firecracker: filepath.Join(data, "bin", "firecracker"),
		Kernel: filepath.Join(data, "kernel", "vmlinux"), Initrd: filepath.Join(data, "initrd.cpio")}
	base := filepath.Join(data, "images", "base.ext4")
	for _, p := range []string{"/dev/kvm", h.Firecracker, h.Kernel, h.Initrd, base} {
		if f, err := os.OpenFile(p, os.O_RDONLY, 0); err != nil {
			t.Skipf("skipping VM test: %v", err)
		} else {
			f.Close()
		}
	}
	return h, base
}

func agentGet(ctx context.Context, m *Machine, path string) error {
	conn, err := m.Dial(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(2 * time.Second))
	fmt.Fprintf(conn, "GET %s HTTP/1.1\r\nHost: agent\r\nConnection: close\r\n\r\n", path)
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("status %s", resp.Status)
	}
	return nil
}

func waitAgent(t *testing.T, m *Machine) time.Duration {
	t.Helper()
	start := time.Now()
	for time.Since(start) < 30*time.Second {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		err := agentGet(ctx, m, "/healthz")
		cancel()
		if err == nil {
			return time.Since(start)
		}
		select {
		case <-m.Exited():
			log, _ := os.ReadFile(filepath.Join(m.cfg.Dir, consoleLog))
			t.Fatalf("VM exited while waiting for the agent:\n%s", log)
		case <-time.After(5 * time.Millisecond):
		}
	}
	t.Fatal("agent never answered over vsock")
	return 0
}

func TestBootSuspendRestore(t *testing.T) {
	h, base := testHost(t)
	dir := t.TempDir()
	if out, err := exec.Command("cp", "--reflink=auto", "--sparse=always", base, filepath.Join(dir, DiskFile)).CombinedOutput(); err != nil {
		t.Fatalf("clone disk: %v: %s", err, out)
	}
	cfg := Config{Dir: dir, Hostname: "vmmtest", VCPUs: 2, MemMiB: 512, AgentPort: 1024}
	ctx := context.Background()

	m, err := Boot(ctx, h, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.Kill() })
	t.Logf("cold boot to agent: %s", waitAgent(t, m))
	if HasSnapshot(dir) {
		t.Fatal("a running VM must not look suspended")
	}

	if err := m.Suspend(ctx); err != nil {
		t.Fatalf("suspend: %v", err)
	}
	select {
	case <-m.Exited():
	default:
		t.Fatal("VMM still running after Suspend")
	}
	if !HasSnapshot(dir) {
		t.Fatal("no snapshot after Suspend")
	}
	if _, err := os.Stat(filepath.Join(dir, pidFile)); !os.IsNotExist(err) {
		t.Fatal("pid file left behind after Suspend")
	}

	m2, err := Restore(ctx, h, cfg)
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	t.Cleanup(func() { m2.Kill() })
	t.Logf("restore to agent: %s", waitAgent(t, m2))
	// Restore consumes the snapshot: the next Suspend must never write over the
	// file that backs the running VM's memory.
	if HasSnapshot(dir) {
		t.Fatal("snapshot files still present after Restore")
	}

	// And the cycle repeats cleanly from a restored VM.
	if err := m2.Suspend(ctx); err != nil {
		t.Fatalf("second suspend: %v", err)
	}
	if !HasSnapshot(dir) {
		t.Fatal("no snapshot after second Suspend")
	}
	DiscardSnapshot(dir)
	if HasSnapshot(dir) {
		t.Fatal("DiscardSnapshot left a snapshot")
	}
}

func TestReapOrphanKillsOnlyAFirecrackerInThatDir(t *testing.T) {
	dir := t.TempDir()
	// A pid file pointing at an unrelated live process (this test binary) must be ignored.
	os.WriteFile(filepath.Join(dir, pidFile), []byte(fmt.Sprint(os.Getpid())), 0o644)
	ReapOrphan(dir)
	if _, err := os.Stat(filepath.Join(dir, pidFile)); !os.IsNotExist(err) {
		t.Fatal("stale pid file not cleaned up")
	}
	// Still alive, obviously — reaching this line is the assertion.

	h, _ := testHost(t)
	cmd := exec.Command(h.Firecracker, "--api-sock", "fc.sock")
	cmd.Dir = dir
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { cmd.Wait(); close(done) }()
	os.WriteFile(filepath.Join(dir, pidFile), []byte(fmt.Sprint(cmd.Process.Pid)), 0o644)
	time.Sleep(100 * time.Millisecond)
	ReapOrphan(dir)
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		cmd.Process.Kill()
		t.Fatal("orphaned firecracker was not killed")
	}
}

// A suspend squeezes the guest's free memory into the balloon and the memory
// file keeps only what the guest uses; the resumed guest gets it all back.
func TestBalloonSqueezeMakesSnapshotSparse(t *testing.T) {
	h, base := testHost(t)
	dir := t.TempDir()
	if out, err := exec.Command("cp", "--reflink=auto", "--sparse=always", base, filepath.Join(dir, DiskFile)).CombinedOutput(); err != nil {
		t.Fatalf("clone disk: %v: %s", err, out)
	}
	cfg := Config{Dir: dir, Hostname: "vmmtest", VCPUs: 2, MemMiB: 1024, AgentPort: 1024}
	ctx := context.Background()
	m, err := Boot(ctx, h, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.Kill() })
	waitAgent(t, m)
	st, err := m.Balloon(ctx)
	if err != nil {
		t.Fatalf("balloon statistics: %v", err)
	}
	if st.TotalMemory == 0 {
		time.Sleep(1500 * time.Millisecond) // the first statistics arrive after an interval
	}
	got, err := m.Squeeze(ctx)
	if err != nil {
		t.Fatalf("squeeze: %v", err)
	}
	if got < cfg.MemMiB/2 {
		t.Errorf("an idle guest gave up only %d of %d MiB", got, cfg.MemMiB)
	}
	if err := m.Suspend(ctx); err != nil {
		t.Fatalf("suspend: %v", err)
	}
	var fst syscall.Stat_t
	syscall.Stat(filepath.Join(dir, snapMem), &fst)
	used := fst.Blocks * 512 >> 20
	t.Logf("memory file: %d MiB on disk for %d MiB of RAM (balloon %d MiB)", used, cfg.MemMiB, got)
	if used > int64(cfg.MemMiB-got+64) {
		t.Errorf("memory file holds %d MiB; the guest was using at most %d", used, cfg.MemMiB-got)
	}

	m2, err := Restore(ctx, h, cfg)
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	t.Cleanup(func() { m2.Kill() })
	waitAgent(t, m2)
	if err := m2.SetBalloon(ctx, 0); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(5 * time.Second); ; {
		if st, err = m2.Balloon(ctx); err == nil && st.ActualMiB == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("balloon did not deflate after resume: %+v %v", st, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
