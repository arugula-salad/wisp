package agent

import (
	"bytes"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
)

// noNewPrivs runs a child through launch and reports the flag it was born with.
func noNewPrivs(t *testing.T) (string, error) {
	t.Helper()
	var out bytes.Buffer
	cmd := exec.Command("grep", "NoNewPrivs", "/proc/self/status")
	cmd.Stdout = &out
	cmd.SysProcAttr = &syscall.SysProcAttr{}
	if err := launch(cmd.SysProcAttr, cmd.Start); err != nil {
		return "", err
	}
	cmd.Wait()
	return strings.Join(strings.Fields(out.String()), " "), nil
}

func TestLaunchPolicy(t *testing.T) {
	t.Cleanup(func() { SetPolicy(Policy{}) })
	if err := SetPolicy(Policy{Profile: "root-ish"}); err == nil {
		t.Fatal("unknown profile accepted")
	}

	if got, _ := noNewPrivs(t); got != "NoNewPrivs: 0" {
		t.Skipf("the test process itself runs with %q", got)
	}
	SetPolicy(Policy{NoNewPrivs: true})
	for i := 0; i < 20; i++ {
		if got, err := noNewPrivs(t); err != nil || got != "NoNewPrivs: 1" {
			t.Fatalf("with the policy: %q %v", got, err)
		}
	}
	// The flag is set on a thread of ours and cannot be cleared; that thread
	// must never be reused for a launch made under a laxer policy.
	SetPolicy(Policy{})
	for i := 0; i < 200; i++ {
		if got, err := noNewPrivs(t); err != nil || got != "NoNewPrivs: 0" {
			t.Fatalf("after lifting the policy (launch %d): %q %v", i, got, err)
		}
	}

	if os.Getuid() != 0 {
		// Dropping from the bounding set needs CAP_SETPCAP. Not having it must
		// fail the launch, not run the command with every capability.
		SetPolicy(Policy{Profile: "standard"})
		if got, err := noNewPrivs(t); err == nil {
			t.Fatalf("launched although the profile could not be applied: %q", got)
		}
	}
}
