//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestMemoryAutoscale: under resources.memory.autoscale the balloon holds a
// sprite to a starting grant below its limit, gives memory back under pressure
// without an OOM kill, and hands it all back when autoscale is turned off.
func TestMemoryAutoscale(t *testing.T) {
	c := client(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	name := fmt.Sprintf("e2e-autoscale-%d", time.Now().UnixNano()%1e9)
	sp, err := c.CreateSprite(ctx, name, nil)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Cleanup(func() { c.DeleteSprite(context.Background(), name) })
	base := "/v1/sprites/" + name
	// Cold still, so the first boot sizes the VM for the limit.
	want(t, "POST", base+"/policy/resources", `{"memory":{"limit_mb":3072,"autoscale":true}}`, 204)
	sh := func(script string) (string, error) {
		out, err := sp.CommandContext(ctx, "bash", "-c", script).CombinedOutput()
		return strings.TrimSpace(string(out)), err
	}
	meminfo := func() (total, avail int) {
		t.Helper()
		out, err := sh(`awk '/^MemTotal:/{t=$2} /^MemAvailable:/{a=$2} END{print t/1024, a/1024}' /proc/meminfo`)
		f := strings.Fields(out)
		if err != nil || len(f) != 2 {
			t.Fatalf("meminfo: %q %v", out, err)
		}
		total, _ = strconv.Atoi(strings.Split(f[0], ".")[0])
		avail, _ = strconv.Atoi(strings.Split(f[1], ".")[0])
		return total, avail
	}
	eventually := func(what string, ok func(total, avail int) bool) {
		t.Helper()
		var total, avail int
		for deadline := time.Now().Add(15 * time.Second); time.Now().Before(deadline); time.Sleep(250 * time.Millisecond) {
			if total, avail = meminfo(); ok(total, avail) {
				return
			}
		}
		t.Fatalf("%s: MemTotal %d MiB, MemAvailable %d MiB", what, total, avail)
	}

	// The VM has the limit's RAM, but the balloon holds all but ~1 GiB of it.
	eventually("held to the starting grant", func(total, avail int) bool { return total > 3000 && avail < 1300 })

	// Off, live: the balloon empties and the guest has its whole limit again.
	want(t, "POST", base+"/policy/resources", `{"memory":{"limit_mb":3072}}`, 204)
	eventually("balloon emptied", func(total, avail int) bool { return avail > 2500 })
	// And on again, live.
	want(t, "POST", base+"/policy/resources", `{"memory":{"limit_mb":3072,"autoscale":true}}`, 204)
	eventually("held again", func(total, avail int) bool { return avail < 1300 })

	// A burst well past the grant: the guest gets the memory, nothing is killed.
	if out, err := sh(`python3 -c 'b = bytearray(b"\x01") * (2 << 30); print("fits")'`); err != nil || out != "fits" {
		t.Fatalf("2 GiB under autoscale with a 3 GiB limit: %q %v", out, err)
	}
	if out, _ := sh(`sudo -n dmesg | grep -ciE "out of memory|oom-kill" || true`); out != "0" {
		t.Fatalf("the guest OOM-killed under autoscale:\n%s", out)
	}
}
