//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	sprites "github.com/superfly/sprites-go"
)

// TestCheckpointMountsAndPolicyFile covers the two things a sprite can read
// about itself from inside: old checkpoints, mounted read-only without a
// restore, and its network policy.
func TestCheckpointMountsAndPolicyFile(t *testing.T) {
	c := client(t)
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	name := fmt.Sprintf("e2e-mnt-%d", time.Now().UnixNano()%1e9)
	sp, err := c.CreateSprite(ctx, name, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.DeleteSprite(context.Background(), name) })

	sh := func(script string) (string, error) {
		out, err := sp.CommandContext(ctx, "sh", "-c", script).CombinedOutput()
		return strings.TrimSpace(string(out)), err
	}
	must := func(script string) string {
		t.Helper()
		out, err := sh(script)
		if err != nil {
			t.Fatalf("%s: %v\n%s", script, err, out)
		}
		return out
	}
	checkpoint := func() {
		t.Helper()
		cs, err := sp.CreateCheckpoint(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if err := cs.ProcessAll(func(*sprites.StreamMessage) error { return nil }); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("read the past without restoring", func(t *testing.T) {
		must(`echo original > ~/notes; mkdir ~/proj; echo keep > ~/proj/a`)
		checkpoint() // v1
		must(`echo overwritten > ~/notes; rm -rf ~/proj`)
		must(`sprite-env checkpoints mount v1`)
		if got := must(`cat ~/notes; cat /.sprite/checkpoints/v1/home/sprite/notes; cat /.sprite/checkpoints/v1/home/sprite/proj/a`); got != "overwritten\noriginal\nkeep" {
			t.Fatalf("live vs checkpoint view: %q", got)
		}
		if out, err := sh(`touch /.sprite/checkpoints/v1/home/sprite/x`); err == nil {
			t.Fatalf("a checkpoint mount must be read-only, but a write succeeded: %s", out)
		}
		must(`sprite-env checkpoints mount v1`) // idempotent
		if n := must(`grep -c ' /.sprite/checkpoints/v1 ' /proc/mounts`); n != "1" {
			t.Fatalf("mounted %s times", n)
		}
	})

	t.Run("a mounted checkpoint cannot be deleted from under the guest", func(t *testing.T) {
		if out, err := sh(`sprite-env checkpoints delete v1`); err == nil || !strings.Contains(out, "mounted") {
			t.Fatalf("delete of a mounted checkpoint: err=%v out=%q", err, out)
		}
		if out, err := sh(`cd /.sprite/checkpoints/v1 && sprite-env checkpoints unmount v1`); err == nil {
			t.Fatalf("unmount while in use should be refused, not forced: %s", out)
		}
		must(`sprite-env checkpoints unmount v1 && sprite-env checkpoints delete v1`)
		if n := must(`cat /sys/block/vdb/size`); n != "2048" {
			t.Fatalf("slot not returned to its placeholder: %s sectors", n)
		}
	})

	t.Run("slots are finite and say so", func(t *testing.T) {
		for i := 0; i < 5; i++ {
			checkpoint() // v2..v6
		}
		must(`for v in v2 v3 v4 v5; do sprite-env checkpoints mount $v >/dev/null || exit 1; done`)
		out, err := sh(`sprite-env checkpoints mount v6`)
		if err == nil || !strings.Contains(out, "slots") {
			t.Fatalf("fifth mount: err=%v out=%q", err, out)
		}
		must(`sprite-env checkpoints unmount v3 && sprite-env checkpoints mount v6 >/dev/null && test -d /.sprite/checkpoints/v6/home`)
	})

	t.Run("a restore resets the slots", func(t *testing.T) {
		rs, err := sp.RestoreCheckpoint(ctx, "v2")
		if err != nil {
			t.Fatal(err)
		}
		rs.ProcessAll(func(*sprites.StreamMessage) error { return nil })
		// A new VM: nothing mounted, every slot free again, and v2 (mounted before) is deletable.
		if n := must(`grep -c /.sprite/checkpoints/ /proc/mounts || true`); n != "0" {
			t.Fatalf("%s checkpoint mounts survived a restore", n)
		}
		must(`sprite-env checkpoints mount v4 >/dev/null && cat /.sprite/checkpoints/v4/home/sprite/notes`)
		must(`sprite-env checkpoints delete v2`)
	})

	t.Run("network policy file", func(t *testing.T) {
		if got := must(`cat /.sprite/policy/network.json`); !strings.Contains(got, `"rules":[]`) {
			t.Fatalf("with no policy: %q", got)
		}
		if out, err := sh(`echo '{}' > /.sprite/policy/network.json`); err == nil {
			t.Fatalf("the sprite user could overwrite its policy file: %s", out)
		}
		// Allow-everything is accepted on any daemon (it restricts nothing), so this runs without the root helper too.
		allowAll := &sprites.NetworkPolicy{Rules: []sprites.NetworkPolicyRule{{Domain: "*", Action: "allow"}}}
		if err := sp.UpdateNetworkPolicy(ctx, allowAll); err != nil {
			t.Fatal(err)
		}
		deadline := time.Now().Add(5 * time.Second)
		for {
			got := must(`cat /.sprite/policy/network.json`)
			if strings.Contains(got, `"domain":"*"`) {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("policy file never reflected the change: %q", got)
			}
			time.Sleep(200 * time.Millisecond)
		}
	})
}
