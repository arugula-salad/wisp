//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"testing"
	"time"

	sprites "github.com/superfly/sprites-go"
)

// TestExecEndsWhenTheSpriteIsRestored: a checkpoint restore kills the VM under
// any running exec, by design. The client must find out promptly. Over the
// control channel the official SDK does not notice a dead socket on its own
// (its reader stops without waking the operation), so spritesd has to say so.
func TestExecEndsWhenTheSpriteIsRestored(t *testing.T) {
	c := client(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	name := fmt.Sprintf("e2e-vanish-%d", time.Now().UnixNano()%1e9)
	sp, err := c.CreateSprite(ctx, name, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.DeleteSprite(context.Background(), name) })

	cs, err := sp.CreateCheckpoint(ctx)
	if err != nil {
		t.Fatal(err)
	}
	cs.ProcessAll(func(*sprites.StreamMessage) error { return nil })
	cps, err := sp.ListCheckpoints(ctx, "")
	if err != nil || len(cps) == 0 {
		t.Fatalf("list checkpoints: %v %v", cps, err)
	}

	for _, tty := range []bool{false, true} {
		t.Run(fmt.Sprintf("tty=%v", tty), func(t *testing.T) {
			cmd := sp.CommandContext(ctx, "sleep", "300")
			cmd.SetTTY(tty)
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			t.Logf("exec running over a %s connection", cmd.ConnectionMode())
			time.Sleep(500 * time.Millisecond)

			rs, err := sp.RestoreCheckpoint(ctx, cps[0].ID)
			if err != nil {
				t.Fatal(err)
			}
			rs.ProcessAll(func(*sprites.StreamMessage) error { return nil })

			done := make(chan error, 1)
			go func() { done <- cmd.Wait() }()
			select {
			case err := <-done:
				if err == nil && cmd.ExitCode() == 0 {
					t.Fatal("a command killed with its VM reported success")
				}
				t.Logf("exec ended: err=%v exit=%d", err, cmd.ExitCode())
			case <-time.After(15 * time.Second):
				t.Fatal("exec still blocked 15s after the sprite was restored out from under it")
			}
		})
	}
}
