//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

// Workspace leases are ours and the Go SDK knows nothing of them, so these go
// over raw HTTP (api/want in platform_test.go) and use the SDK only to see the
// effect. The reaper runs on the daemon's 30 s janitor tick, which is why the
// waits below are in tens of seconds rather than instant.

// janitorPeriod is the lifecycle janitor's tick, plus room for the sweep itself.
const janitorPeriod = 45 * time.Second

func lease(t *testing.T, name string) (expiresAt string, protected bool) {
	t.Helper()
	var l struct {
		ExpiresAt *string `json:"expires_at"`
		Protected bool    `json:"protected"`
	}
	body := want(t, http.MethodGet, "/mini-sprites/v1/sprites/"+name+"/lease", "", http.StatusOK)
	if err := json.Unmarshal([]byte(body), &l); err != nil {
		t.Fatalf("lease %s: %v: %s", name, err, body)
	}
	if l.ExpiresAt == nil {
		return "", l.Protected
	}
	return *l.ExpiresAt, l.Protected
}

// gone waits for the reaper to delete the sprite.
func gone(t *testing.T, name string, within time.Duration) {
	t.Helper()
	for end := time.Now().Add(within); time.Now().Before(end); time.Sleep(2 * time.Second) {
		if code, _ := api(t, http.MethodGet, "/v1/sprites/"+name, ""); code == http.StatusNotFound {
			return
		}
	}
	t.Fatalf("%s outlived its lease by %s", name, within)
}

func TestLeases(t *testing.T) {
	n := time.Now().UnixNano() % 1e9
	keeper, leased := fmt.Sprintf("e2e-keep-%d", n), fmt.Sprintf("e2e-lease-%d", n)
	t.Cleanup(func() {
		for _, name := range []string{keeper, leased} {
			api(t, http.MethodDelete, "/v1/sprites/"+name, "")
		}
	})

	// The default is what it has always been: a sprite nobody leased is nobody's
	// to delete, and the reaper never looks at it again.
	want(t, http.MethodPost, "/v1/sprites", `{"name":"`+keeper+`"}`, http.StatusCreated)
	if sp := want(t, http.MethodGet, "/v1/sprites/"+keeper, "", http.StatusOK); strings.Contains(sp, "expires_at") {
		t.Fatalf("a plain create came back leased: %s", sp)
	}
	if exp, prot := lease(t, keeper); exp != "" || prot {
		t.Fatalf("lease on an unleased sprite: %q %v", exp, prot)
	}

	frames := openEvents(t, "sprite="+leased, nil)

	// 45 s: long enough to protect it and see that hold, short enough that the
	// warning (5 minutes ahead by default) is due at the first sweep.
	created := want(t, http.MethodPost, "/v1/sprites", `{"name":"`+leased+`","ttl_seconds":45}`, http.StatusCreated)
	if !strings.Contains(created, `"expires_at"`) {
		t.Fatalf("create with ttl_seconds: %s", created)
	}
	first, _ := lease(t, leased)
	if first == "" {
		t.Fatal("the lease did not survive the create")
	}

	t.Run("renewal moves the deadline", func(t *testing.T) {
		body := want(t, http.MethodPost, "/mini-sprites/v1/sprites/"+leased+"/lease", `{"ttl_seconds":90}`, http.StatusOK)
		second, _ := lease(t, leased)
		if second <= first {
			t.Fatalf("renewed to %s, which is not after %s (%s)", second, first, body)
		}
		// A deadline in the past is a typo, not an instruction to delete.
		want(t, http.MethodPost, "/mini-sprites/v1/sprites/"+leased+"/lease",
			`{"expires_at":"2020-01-01T00:00:00Z"}`, http.StatusBadRequest)
		want(t, http.MethodPost, "/mini-sprites/v1/sprites/"+leased+"/lease",
			`{"ttl_seconds":30,"expires_at":"2099-01-01T00:00:00Z"}`, http.StatusBadRequest)
	})

	t.Run("protection outlives the deadline", func(t *testing.T) {
		want(t, http.MethodPost, "/mini-sprites/v1/sprites/"+leased+"/lease", `{"protected":true}`, http.StatusOK)
		want(t, http.MethodPost, "/mini-sprites/v1/sprites/"+leased+"/lease", `{"ttl_seconds":5}`, http.StatusOK)
		if _, prot := lease(t, leased); !prot {
			t.Fatal("the renewal cleared the protection")
		}
		time.Sleep(janitorPeriod)
		if code, body := api(t, http.MethodGet, "/v1/sprites/"+leased, ""); code != http.StatusOK {
			t.Fatalf("a protected sprite was reaped: %d %s", code, body)
		}
		// The deadline is still there and still in the past: protection holds the
		// deletion off, it does not pretend the lease is good.
		if exp, _ := lease(t, leased); exp == "" {
			t.Fatal("protecting cleared the deadline")
		}
	})

	t.Run("an expired sprite is deleted whole", func(t *testing.T) {
		want(t, http.MethodPost, "/mini-sprites/v1/sprites/"+leased+"/lease", `{"ttl_seconds":10,"protected":false}`, http.StatusOK)
		gone(t, leased, 2*janitorPeriod)
		// And its name is free again, which it would not be if the record or the
		// address had been left behind.
		want(t, http.MethodPost, "/v1/sprites", `{"name":"`+leased+`"}`, http.StatusCreated)
		want(t, http.MethodDelete, "/v1/sprites/"+leased, "", http.StatusNoContent)
	})

	t.Run("the warning and the expiry are on the event stream", func(t *testing.T) {
		got := until(t, frames, time.Second, func(f frame) bool { return f.ev.Type == "sprite.deleted" })
		if !inOrder(types(got), []string{"sprite.created", "sprite.expiring", "sprite.expired", "sprite.deleted"}) {
			t.Fatalf("events = %v", types(got))
		}
		warnings := 0
		for _, f := range got {
			if f.ev.Type == "sprite.expiring" {
				warnings++
			}
		}
		// Once per deadline, not once per sweep; the renewals above each earn one.
		if warnings > 4 {
			t.Errorf("%d expiring warnings for four deadlines: %v", warnings, types(got))
		}
	})
}

// TestChildLease is the point of the feature: a lobby hands out sprites that
// clean themselves up, so its max_children slots come back without anyone
// remembering to delete anything.
func TestChildLease(t *testing.T) {
	c := client(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	n := time.Now().UnixNano() % 1e9
	lobby, game := fmt.Sprintf("e2e-lobby-%d", n), fmt.Sprintf("e2e-child-%d", n)
	if _, err := c.CreateSprite(ctx, lobby, nil); err != nil {
		t.Fatalf("create %s: %v", lobby, err)
	}
	t.Cleanup(func() {
		for _, name := range []string{game, lobby} {
			c.DeleteSprite(context.Background(), name)
		}
	})
	want(t, http.MethodPost, "/v1/sprites/"+lobby+"/policy/spawn", `{"enabled":true,"child_ttl_seconds":20}`, http.StatusNoContent)

	out, err := c.Sprite(lobby).CommandContext(ctx, "sprite-env", "sprites", "create", game).CombinedOutput()
	if err != nil {
		t.Fatalf("spawn: %v\n%s", err, out)
	}
	if exp, _ := lease(t, game); exp == "" {
		t.Fatal("a child was born without the lease its lobby's policy gives")
	}
	gone(t, game, 2*janitorPeriod)
}
