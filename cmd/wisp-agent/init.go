package main

import (
	"log"
	"net"
	"os"
	"strings"
	"time"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

// cmdline returns the sprite.* parameters wispd put on the kernel command line.
func cmdline() map[string]string {
	out := map[string]string{}
	b, _ := os.ReadFile("/proc/cmdline")
	for _, f := range strings.Fields(string(b)) {
		if k, v, ok := strings.Cut(f, "="); ok && strings.HasPrefix(k, "sprite.") {
			out[strings.TrimPrefix(k, "sprite.")] = v
		}
	}
	return out
}

func mount(src, dst, fstype string, flags uintptr, data string) {
	os.MkdirAll(dst, 0o755)
	if err := unix.Mount(src, dst, fstype, flags, data); err != nil && err != unix.EBUSY {
		log.Printf("mount %s: %v", dst, err)
	}
}

// attachConsole points stdio at the serial console. The initramfs has no
// /dev/console node, so the kernel starts /init with nothing open.
func attachConsole() {
	f, err := os.OpenFile("/dev/console", os.O_RDWR, 0)
	if err != nil {
		return
	}
	for fd := 0; fd < 3; fd++ {
		unix.Dup2(int(f.Fd()), fd)
	}
}

// pivotToDisk runs when booted from the initramfs: it mounts the sprite's disk
// and makes it the root, the same dance as switch_root. PID 1 keeps running
// from the initramfs copy of this binary, so the disk never needs to contain it.
func pivotToDisk() map[string]string {
	mount("proc", "/proc", "proc", 0, "")
	p := cmdline()
	unix.Unmount("/proc", 0)
	dev := p["root"]
	if dev == "" {
		return p
	}
	mount("devtmpfs", "/dev", "devtmpfs", unix.MS_NOSUID, "mode=0755")
	attachConsole()
	os.MkdirAll("/newroot", 0o755)
	var err error
	for i := 0; i < 200; i++ {
		if err = unix.Mount(dev, "/newroot", "ext4", 0, ""); err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err != nil {
		log.Fatalf("mount %s: %v", dev, err)
	}
	installTools("/newroot")
	if err := unix.Mount("/dev", "/newroot/dev", "", unix.MS_MOVE, ""); err != nil {
		log.Printf("move /dev: %v", err)
	}
	must := func(what string, err error) {
		if err != nil {
			log.Fatalf("%s: %v", what, err)
		}
	}
	must("chdir", unix.Chdir("/newroot"))
	must("move root", unix.Mount(".", "/", "", unix.MS_MOVE, ""))
	must("chroot", unix.Chroot("."))
	must("chdir /", unix.Chdir("/"))
	return p
}

// installTools copies the in-guest CLI from the initramfs onto the sprite's
// disk. It has to happen before the pivot, after which the initramfs is out of
// reach, and on every boot, so the CLI always matches the running agent even on
// a disk restored from an old checkpoint. /usr/local/bin is on every PATH the
// agent hands out (and sudo's secure_path); a user's own file there is left alone.
func installTools(root string) {
	const bin = "/.sprite/bin/sprite-env"
	b, err := os.ReadFile("/sprite-env")
	if err != nil {
		log.Printf("sprite-env not in initramfs: %v", err)
		return
	}
	dst := root + bin
	os.MkdirAll(root+"/.sprite/bin", 0o755)
	// Rename into place: a VM killed mid-copy must not leave half a binary behind.
	if err := os.WriteFile(dst+".tmp", b, 0o755); err != nil {
		log.Printf("install sprite-env: %v", err)
		return
	}
	if err := os.Rename(dst+".tmp", dst); err != nil {
		log.Printf("install sprite-env: %v", err)
		return
	}
	os.MkdirAll(root+"/usr/local/bin", 0o755)
	if err := os.Symlink(bin, root+"/usr/local/bin/sprite-env"); err != nil && !os.IsExist(err) {
		log.Printf("link sprite-env: %v", err)
	}
	installSudo(root, b)
}

// installSudo gives a disk with no sudo of its own (most container images: see
// wispd's images.go) a stand-in: sprite-env, setuid root, which acts as a
// passwordless sudo for the sprite user (cmd/sprite-env/sudo.go). Once the disk
// has a real sudo (apt install sudo), the stand-in is taken away again.
func installSudo(root string, b []byte) {
	const shim, link = "/.sprite/bin/sudo", "/usr/local/bin/sudo"
	ours := func() bool { t, err := os.Readlink(root + link); return err == nil && t == shim }
	hasSudo := false
	for _, p := range []string{"/usr/bin/sudo", "/bin/sudo", "/usr/sbin/sudo", "/sbin/sudo", link} {
		// Lstat: an absolute symlink would resolve inside the initramfs, not the disk.
		if _, err := os.Lstat(root + p); err == nil && !(p == link && ours()) {
			hasSudo = true
		}
	}
	if hasSudo {
		if ours() {
			os.Remove(root + link)
		}
		os.Remove(root + shim)
		return
	}
	dst := root + shim
	if err := os.WriteFile(dst+".tmp", b, 0o755); err != nil {
		log.Printf("install sudo stand-in: %v", err)
		return
	}
	// Chmod after the write: the setuid bit must be set on a root-owned, complete file.
	if err := os.Chown(dst+".tmp", 0, 0); err != nil {
		log.Printf("install sudo stand-in: %v", err)
		return
	}
	if err := os.Chmod(dst+".tmp", 0o755|os.ModeSetuid); err != nil {
		log.Printf("install sudo stand-in: %v", err)
		return
	}
	if err := os.Rename(dst+".tmp", dst); err != nil {
		log.Printf("install sudo stand-in: %v", err)
		return
	}
	if err := os.Symlink(shim, root+link); err != nil && !os.IsExist(err) {
		log.Printf("link sudo stand-in: %v", err)
	}
}

func runInit() {
	const nsd = unix.MS_NOSUID | unix.MS_NODEV
	p := pivotToDisk()
	mount("proc", "/proc", "proc", nsd|unix.MS_NOEXEC, "")
	mount("sysfs", "/sys", "sysfs", nsd|unix.MS_NOEXEC, "")
	mount("devtmpfs", "/dev", "devtmpfs", unix.MS_NOSUID, "mode=0755")
	mount("devpts", "/dev/pts", "devpts", unix.MS_NOSUID|unix.MS_NOEXEC, "gid=5,mode=0620,ptmxmode=0666")
	mount("tmpfs", "/dev/shm", "tmpfs", nsd, "mode=1777")
	mount("tmpfs", "/run", "tmpfs", nsd, "mode=0755")
	mount("tmpfs", "/tmp", "tmpfs", nsd, "mode=1777")
	mount("cgroup2", "/sys/fs/cgroup", "cgroup2", nsd|unix.MS_NOEXEC, "")
	for _, l := range [][2]string{{"/proc/self/fd", "/dev/fd"}, {"/proc/self/fd/0", "/dev/stdin"},
		{"/proc/self/fd/1", "/dev/stdout"}, {"/proc/self/fd/2", "/dev/stderr"}} {
		os.Symlink(l[0], l[1])
	}

	if h := p["hostname"]; h != "" {
		unix.Sethostname([]byte(h))
		// sudo and friends expect the hostname to resolve.
		os.WriteFile("/etc/hostname", []byte(h+"\n"), 0o644)
		os.WriteFile("/etc/hosts", []byte("127.0.0.1\tlocalhost\n127.0.1.1\t"+h+"\n"+
			"::1\tlocalhost ip6-localhost ip6-loopback\n"), 0o644)
	}
	setupNetwork(p)

	for {
		proc, err := os.StartProcess("/proc/self/exe", []string{"wisp-agent", "serve"}, &os.ProcAttr{
			Files: []*os.File{os.Stdin, os.Stdout, os.Stderr},
			Env:   []string{"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin", "HOME=/root"},
		})
		if err != nil {
			log.Printf("start server: %v", err)
			time.Sleep(time.Second)
			continue
		}
		// Reap everything; orphans are reparented to us. Restart the server if it is the one that died.
		for {
			var ws unix.WaitStatus
			pid, err := unix.Wait4(-1, &ws, 0, nil)
			if err == unix.EINTR {
				continue
			}
			if err != nil || pid == proc.Pid {
				break
			}
		}
		log.Printf("server exited; restarting")
		time.Sleep(200 * time.Millisecond)
	}
}

func setupNetwork(p map[string]string) {
	if lo, err := netlink.LinkByName("lo"); err == nil {
		netlink.LinkSetUp(lo)
	}
	if p["ip"] == "" {
		return
	}
	eth, err := netlink.LinkByName("eth0")
	if err != nil {
		log.Printf("eth0: %v", err)
		return
	}
	addr, err := netlink.ParseAddr(p["ip"])
	if err != nil {
		log.Printf("sprite.ip: %v", err)
		return
	}
	if err := netlink.AddrReplace(eth, addr); err != nil {
		log.Printf("addr: %v", err)
	}
	if err := netlink.LinkSetUp(eth); err != nil {
		log.Printf("eth0 up: %v", err)
	}
	if gw := net.ParseIP(p["gw"]); gw != nil {
		if err := netlink.RouteReplace(&netlink.Route{LinkIndex: eth.Attrs().Index, Gw: gw}); err != nil {
			log.Printf("default route: %v", err)
		}
	}
	if dns := p["dns"]; dns != "" {
		var b strings.Builder
		for _, s := range strings.Split(dns, ",") {
			b.WriteString("nameserver " + s + "\n")
		}
		os.Remove("/etc/resolv.conf") // often a dangling systemd-resolved symlink
		os.WriteFile("/etc/resolv.conf", []byte(b.String()), 0o644)
	}
}
