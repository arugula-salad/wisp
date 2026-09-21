//go:build e2e

package e2e

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"testing"
	"time"

	sprites "github.com/superfly/sprites-go"
)

// TestOperatorOrgAndLimits checks what the official SDK makes of the org block
// and of a refused create or wake. The limit cases need a daemon started with
// the limits and told to the test:
//
//	spritesd --max-sprites 4 --max-running 1 ...
//	SPRITES_E2E_MAX_SPRITES=4 SPRITES_E2E_MAX_RUNNING=1 go test -tags e2e -run Operator ./e2e/
//
// on a daemon with no other sprites, since the limits count all of them.
func TestOperatorOrgAndLimits(t *testing.T) {
	c := client(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	stamp := time.Now().UnixNano() % 1e9
	create := func(i int) (*sprites.Sprite, error) {
		name := fmt.Sprintf("e2e-op-%d-%d", stamp, i)
		sp, err := c.CreateSprite(ctx, name, nil)
		if err == nil {
			t.Cleanup(func() { c.DeleteSprite(context.Background(), name) })
		}
		return sp, err
	}
	first, err := create(0)
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	t.Run("list carries org counts", func(t *testing.T) {
		list, err := c.ListSprites(ctx, &sprites.ListOptions{MaxResults: 1})
		if err != nil {
			t.Fatal(err)
		}
		org := list.Org
		if org == nil || org.Name == "" || org.Running+org.Warm+org.Cold < 1 {
			t.Fatalf("org = %+v", org)
		}
		if want, _ := strconv.Atoi(os.Getenv("SPRITES_E2E_MAX_RUNNING")); org.RunningLimit != want {
			t.Fatalf("running_limit = %d, want %d", org.RunningLimit, want)
		}
	})

	t.Run("max-running refuses a wake", func(t *testing.T) {
		limit, _ := strconv.Atoi(os.Getenv("SPRITES_E2E_MAX_RUNNING"))
		if limit != 1 {
			t.Skip("needs a daemon with --max-running 1 and SPRITES_E2E_MAX_RUNNING=1")
		}
		second, err := create(1)
		if err != nil {
			t.Fatal(err)
		}
		// Holds the only slot while the second sprite asks for one.
		hold := first.CommandContext(ctx, "sleep", "20")
		if err := hold.Start(); err != nil {
			t.Fatalf("start first: %v", err)
		}
		defer hold.Wait()
		if out, err := first.CommandContext(ctx, "echo", "up").Output(); err != nil || string(out) != "up\n" {
			t.Fatalf("first sprite: %q %v", out, err)
		}
		_, err = second.CommandContext(ctx, "echo", "hi").Output()
		var apiErr *sprites.APIError
		if !errors.As(err, &apiErr) {
			t.Fatalf("waking past the limit: want an APIError, got %T: %v", err, err)
		}
		if !apiErr.IsConcurrentLimitExceeded() || !apiErr.IsRateLimitError() || apiErr.Limit != 1 || apiErr.CurrentCount != 1 || apiErr.GetRetryAfterSeconds() < 1 {
			t.Fatalf("limit error = %+v", apiErr)
		}
	})

	t.Run("max-sprites refuses a create", func(t *testing.T) {
		limit, _ := strconv.Atoi(os.Getenv("SPRITES_E2E_MAX_SPRITES"))
		if limit == 0 {
			t.Skip("needs a daemon with --max-sprites N and SPRITES_E2E_MAX_SPRITES=N")
		}
		var err error
		for i := 2; i < limit+3 && err == nil; i++ {
			_, err = create(i)
		}
		var apiErr *sprites.APIError
		if !errors.As(err, &apiErr) {
			t.Fatalf("creating past the limit: want an APIError, got %T: %v", err, err)
		}
		if apiErr.ErrorCode != "sprite_limit_exceeded" || apiErr.Limit != limit || apiErr.CurrentCount != limit {
			t.Fatalf("limit error = %+v", apiErr)
		}
	})
}
