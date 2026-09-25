package confine

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
)

// The confinement tests run a stand-in for Firecracker — this test binary
// re-execed through the same shim, with the same Spec — and have it try the
// things a compromised VMM would try. Everything the stand-in reports must be
// denied, because the stand-in is standing in for code that has already escaped.

const helperEnv = "CONFINE_TEST_HELPER"

// TestMain doubles as both halves of the launch. It must dispatch the shim
// argument before anything else, exactly as cmd/wispd's main does: the
// stand-in inherits CONFINE_TEST_HELPER through the shim's execve, so checking
// the environment first would run the probes in the unconfined outer process
// and quietly pass every test.
func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == ShimArg {
		RunShim(os.Args[2:])
	}
	if probes := os.Getenv(helperEnv); probes != "" {
		runProbes(probes)
		return
	}
	os.Exit(m.Run())
}

// probe is one thing the stand-in tries to do.
type probe struct {
	Name string `json:"name"`
	Op   string `json:"op"` // read, readdir, openrw, write, mkdir, connect, unixconnect, signal
	Path string `json:"path,omitempty"`
	Pid  int    `json:"pid,omitempty"`
}

func runProbes(blob string) {
	var probes []probe
	if err := json.Unmarshal([]byte(blob), &probes); err != nil {
		fmt.Fprintln(os.Stderr, "helper:", err)
		os.Exit(2)
	}
	for _, p := range probes {
		fmt.Printf("%s=%s\n", p.Name, p.run())
	}
	os.Exit(0)
}

func (p probe) run() string {
	var err error
	switch p.Op {
	case "read":
		var f *os.File
		if f, err = os.Open(p.Path); err == nil {
			buf := make([]byte, 1)
			_, err = f.Read(buf)
			f.Close()
		}
	case "readdir":
		_, err = os.ReadDir(p.Path)
	case "openrw":
		// /dev/kvm is opened O_RDWR and driven by ioctl; it is never read().
		var f *os.File
		if f, err = os.OpenFile(p.Path, os.O_RDWR, 0); err == nil {
			f.Close()
		}
	case "write":
		var f *os.File
		if f, err = os.OpenFile(p.Path, os.O_CREATE|os.O_WRONLY, 0o644); err == nil {
			_, err = f.WriteString("x")
			f.Close()
		}
	case "mkdir":
		err = os.Mkdir(p.Path, 0o755)
	case "connect":
		var c net.Conn
		if c, err = net.Dial("tcp", p.Path); err == nil {
			c.Close()
		}
	case "unixconnect":
		var c net.Conn
		if c, err = net.Dial("unix", p.Path); err == nil {
			c.Close()
		}
	case "signal":
		err = syscall.Kill(p.Pid, syscall.Signal(0))
	default:
		return "bad-op"
	}
	if err == nil {
		return "allowed"
	}
	if errors.Is(err, os.ErrPermission) || errors.Is(err, unix.EACCES) || errors.Is(err, unix.EPERM) {
		return "denied"
	}
	return "error:" + err.Error()
}

// runConfined launches the stand-in under the shim with the given Spec and
// returns each probe's result.
func runConfined(t *testing.T, spec Spec, probes []probe) map[string]string {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	// The stand-in is this test binary, so the domain must let it execute.
	spec.Exec = self
	spec.Strict = true
	specJSON, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	probeJSON, err := json.Marshal(probes)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(self, ShimArg, string(specJSON), self)
	cmd.Dir = spec.Dir
	cmd.Env = append(os.Environ(), helperEnv+"="+string(probeJSON))
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("stand-in failed: %v\n%s", err, out)
	}
	results := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if name, res, ok := strings.Cut(line, "="); ok {
			results[name] = res
		}
	}
	return results
}

func requireLandlock(t *testing.T, min int) int {
	t.Helper()
	abi := abiVersion()
	if abi < min {
		t.Skipf("kernel Landlock ABI is %d, need %d", abi, min)
	}
	return abi
}

// TestLandlockConfinesVMM is the issue's "done means" check: a process under
// the same confinement as a VMM cannot read another sprite's machine directory,
// the API token, or the user's home.
func TestLandlockConfinesVMM(t *testing.T) {
	requireLandlock(t, 1)

	// A stand-in data directory shaped like the real one: two sprites side by
	// side, the token next to them.
	data := t.TempDir()
	mine := filepath.Join(data, "vm", "sprite-mine")
	theirs := filepath.Join(data, "vm", "sprite-theirs")
	for _, d := range []string{mine, theirs} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(d, "disk.ext4"), []byte("disk"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	token := filepath.Join(data, "token")
	if err := os.WriteFile(token, []byte("msprite_secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	kernel := filepath.Join(data, "kernel")
	if err := os.WriteFile(kernel, []byte("vmlinux"), 0o644); err != nil {
		t.Fatal(err)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}

	spec := Spec{Dir: mine, ReadOnly: []string{kernel}, Devices: []string{"/dev/kvm"}}
	probes := []probe{
		// Allowed: its own machine directory, the kernel, /dev/kvm.
		{Name: "own-disk", Op: "read", Path: filepath.Join(mine, "disk.ext4")},
		{Name: "own-write", Op: "write", Path: filepath.Join(mine, "snap.mem")},
		{Name: "kernel", Op: "read", Path: kernel},
		{Name: "kvm", Op: "openrw", Path: "/dev/kvm"},
		// Denied: everything else.
		{Name: "other-sprite-disk", Op: "read", Path: filepath.Join(theirs, "disk.ext4")},
		{Name: "other-sprite-write", Op: "write", Path: filepath.Join(theirs, "pwned")},
		{Name: "api-token", Op: "read", Path: token},
		{Name: "vm-root-listing", Op: "readdir", Path: filepath.Join(data, "vm")},
		{Name: "home-profile", Op: "read", Path: filepath.Join(home, ".bashrc")},
		{Name: "etc-passwd", Op: "read", Path: "/etc/passwd"},
		// A unique name, so that a run where the domain failed to apply leaves
		// an obvious turd rather than making the next run pass on "file exists".
		{Name: "mkdir-in-home", Op: "mkdir", Path: filepath.Join(home, ".confine-test-"+strconv.Itoa(os.Getpid()))},
	}
	got := runConfined(t, spec, probes)

	want := map[string]string{
		"own-disk": "allowed", "own-write": "allowed", "kernel": "allowed", "kvm": "allowed",
		"other-sprite-disk": "denied", "other-sprite-write": "denied", "api-token": "denied",
		"vm-root-listing": "denied", "home-profile": "denied", "etc-passwd": "denied",
		"mkdir-in-home": "denied",
	}
	for name, exp := range want {
		if got[name] != exp {
			t.Errorf("%s: got %q, want %q", name, got[name], exp)
		}
	}
	// A denial must not have leaked the contents either way round.
	if t.Failed() {
		t.Logf("all results: %v", got)
	}
}

// TestLandlockDeniesTCP checks the network half: Firecracker speaks no TCP, so
// the domain grants none. Needs ABI 4.
func TestLandlockDeniesTCP(t *testing.T) {
	requireLandlock(t, abiNet)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()

	dir := t.TempDir()
	got := runConfined(t, Spec{Dir: dir}, []probe{
		{Name: "tcp", Op: "connect", Path: ln.Addr().String()},
	})
	if got["tcp"] != "denied" {
		t.Errorf("tcp connect: got %q, want denied", got["tcp"])
	}
}

// TestLandlockPathnameUnixSockets pins down a limit docs/security.md states: Landlock
// scopes *abstract* unix sockets but does not mediate connect() on a pathname
// one, so a compromised VMM can still reach a socket file whose mode lets it
// in. If a future kernel closes this, the test says so and the doc's
// "not confined" list gets shorter.
func TestLandlockPathnameUnixSockets(t *testing.T) {
	requireLandlock(t, 1)

	data := t.TempDir()
	mine, theirs := filepath.Join(data, "mine"), filepath.Join(data, "theirs")
	for _, d := range []string{mine, theirs} {
		if err := os.Mkdir(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	sock := filepath.Join(theirs, "fc.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()

	got := runConfined(t, Spec{Dir: mine}, []probe{
		{Name: "other-sprite-api-sock", Op: "unixconnect", Path: sock},
	})
	switch res := got["other-sprite-api-sock"]; res {
	case "allowed":
		t.Log("pathname unix sockets are still reachable from inside the domain, as docs/security.md says")
	case "denied":
		t.Log("this kernel denies pathname unix sockets outside the domain: the caveat in docs/security.md can go")
	default:
		t.Errorf("connect to another sprite's pathname socket: %q", res)
	}
}

// TestLandlockScopesSignals checks that a confined VMM cannot signal wispd
// or any other process outside its domain. Needs ABI 6.
func TestLandlockScopesSignals(t *testing.T) {
	requireLandlock(t, abiScope)

	// A process outside the domain to aim at: sleep, reaped at the end.
	victim := exec.Command("/bin/sleep", "30")
	if err := victim.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		victim.Process.Kill()
		victim.Wait()
	}()

	dir := t.TempDir()
	got := runConfined(t, Spec{Dir: dir}, []probe{
		{Name: "signal-outside", Op: "signal", Pid: victim.Process.Pid},
	})
	if got["signal-outside"] != "denied" {
		t.Errorf("signal outside the domain: got %q, want denied", got["signal-outside"])
	}
}

// TestSpecRulesSkipMissingOptionalPaths: /dev/net/tun is absent without the
// host network setup and /etc/localtime is not universal, so an optional path
// that does not exist must not fail the launch.
func TestSpecRulesSkipMissingOptionalPaths(t *testing.T) {
	dir := t.TempDir()
	spec := Spec{
		Dir:      dir,
		Exec:     "/bin/true",
		ReadOnly: []string{filepath.Join(dir, "definitely-absent")},
		Devices:  []string{filepath.Join(dir, "also-absent")},
	}
	rules := spec.rules()
	for path := range rules {
		if strings.HasPrefix(path, dir) && path != dir {
			t.Errorf("absent optional path %s should have been skipped", path)
		}
	}
	if _, ok := rules[dir]; !ok {
		t.Error("the machine directory must always be granted")
	}
	if rules[dir]&unix.LANDLOCK_ACCESS_FS_MAKE_SOCK == 0 {
		t.Error("the machine directory must allow binding unix sockets (fc.sock, v.sock)")
	}
	if rules["/bin/true"]&unix.LANDLOCK_ACCESS_FS_EXECUTE == 0 {
		t.Error("the VMM binary must be executable from inside the domain")
	}
}

// TestFSRightsAreComplete guards the trap that makes a Landlock sandbox useless:
// a right the ruleset does not *handle* is not restricted at all, so the handled
// mask has to name every right the kernel knows, not just the ones we grant.
func TestFSRightsAreComplete(t *testing.T) {
	abi := requireLandlock(t, 1)
	granted := uint64(rightsDir | rightsReadOnly | rightsExec | rightsDevice)
	handled := fsRights(abi)
	if granted&^handled != 0 {
		t.Errorf("granting rights that are not handled (EINVAL from the kernel): %#x", granted&^handled)
	}
	for _, r := range []struct {
		name string
		bit  uint64
	}{
		{"MAKE_DIR", unix.LANDLOCK_ACCESS_FS_MAKE_DIR},
		{"MAKE_SYM", unix.LANDLOCK_ACCESS_FS_MAKE_SYM},
		{"REMOVE_FILE", unix.LANDLOCK_ACCESS_FS_REMOVE_FILE},
		{"REMOVE_DIR", unix.LANDLOCK_ACCESS_FS_REMOVE_DIR},
		{"MAKE_CHAR", unix.LANDLOCK_ACCESS_FS_MAKE_CHAR},
		{"MAKE_BLOCK", unix.LANDLOCK_ACCESS_FS_MAKE_BLOCK},
		{"EXECUTE", unix.LANDLOCK_ACCESS_FS_EXECUTE},
	} {
		if handled&r.bit == 0 {
			t.Errorf("%s is unhandled, so it is unrestricted everywhere", r.name)
		}
	}
}

func TestParseMode(t *testing.T) {
	for in, want := range map[string]Mode{
		"": ModeBestEffort, "best-effort": ModeBestEffort, "BEST-EFFORT": ModeBestEffort,
		"strict": ModeStrict, "off": ModeOff, " off ": ModeOff,
	} {
		got, err := ParseMode(in)
		if err != nil || got != want {
			t.Errorf("ParseMode(%q) = %v, %v; want %v, nil", in, got, err, want)
		}
	}
	if _, err := ParseMode("yes"); err == nil {
		t.Error("ParseMode(\"yes\") should be an error")
	}
}

// TestOffIsNil: ModeOff must leave the launch path exactly as it was.
func TestOffIsNil(t *testing.T) {
	c, err := Open(ModeOff, "wisp-test", nil)
	if err != nil || c != nil {
		t.Fatalf("Open(ModeOff) = %v, %v; want nil, nil", c, err)
	}
	if c.Enforced() {
		t.Error("a nil Confiner must not claim to enforce anything")
	}
	cmd := exec.Command("/bin/true")
	cg, err := c.Start(cmd, Spec{}, "id", Limits{})
	if err != nil || cg != nil {
		t.Fatalf("nil Confiner.Start = %v, %v; want nil, nil", cg, err)
	}
	if cmd.Path != "/bin/true" || len(cmd.Args) != 1 {
		t.Errorf("nil Confiner rewrote the command: %v", cmd.Args)
	}
}
