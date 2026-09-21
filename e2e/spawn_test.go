//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

// TestSpawn is the lobby pattern: one sprite hands out sprites of its own,
// each a clone of a template that was set up once, and sends visitors to them.
func TestSpawn(t *testing.T) {
	c := client(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	n := time.Now().UnixNano() % 1e9
	template, lobby, game, other := fmt.Sprintf("e2e-tmpl-%d", n), fmt.Sprintf("e2e-lobby-%d", n), fmt.Sprintf("e2e-game-%d", n), fmt.Sprintf("e2e-other-%d", n)
	for _, name := range []string{template, lobby, other} {
		if _, err := c.CreateSprite(ctx, name, nil); err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
	}
	t.Cleanup(func() {
		for _, name := range []string{game, template, lobby, other} {
			c.DeleteSprite(context.Background(), name)
		}
	})
	sh := func(t *testing.T, sprite, script string) (string, error) {
		t.Helper()
		out, err := c.Sprite(sprite).CommandContext(ctx, "bash", "-c", script).CombinedOutput()
		return string(out), err
	}
	must := func(t *testing.T, sprite, script string) string {
		t.Helper()
		out, err := sh(t, sprite, script)
		if err != nil {
			t.Fatalf("%s: %v\n%s", script, err, out)
		}
		return out
	}

	// The slow part happens once, in the template: install the game, define its service, checkpoint.
	must(t, template, `mkdir -p ~/game && echo "the game" > ~/game/index.html &&
		sprite-env services create web --cmd python3 --args "-m,http.server,3000" --dir /home/sprite/game --http-port 3000 --duration 1s &&
		sprite-env checkpoints create --comment ready`)

	t.Run("refused without a spawn policy", func(t *testing.T) {
		if out, err := sh(t, lobby, `sprite-env sprites list`); err == nil || !strings.Contains(out, "may not manage sprites") {
			t.Fatalf("got %v: %s", err, out)
		}
	})
	want(t, http.MethodPost, "/v1/sprites/"+lobby+"/policy/spawn", `{"enabled":true,"sources":["`+template+`"]}`, http.StatusNoContent)

	t.Run("a clone serves the template's game at its own URL", func(t *testing.T) {
		start := time.Now()
		out := must(t, lobby, `sprite-env sprites create `+game+` --from `+template+` --public`)
		if !strings.Contains(out, `"name": "`+game+`"`) || !strings.Contains(out, `"url": "`) {
			t.Fatalf("create: %s", out)
		}
		// Nobody ran anything in the new sprite: its URL cold-boots it and starts the cloned service.
		if got := fetchURL(t, game); got != "the game\n" {
			t.Fatalf("game URL served %q", got)
		}
		t.Logf("create to first response: %v", time.Since(start).Round(time.Millisecond))
		if got := must(t, game, `hostname; cat /.sprite/policy/network.json 2>/dev/null | head -c 200`); strings.Contains(got, template) {
			t.Errorf("the clone still thinks it is the template: %s", got)
		}
	})

	t.Run("sees and deletes only its own", func(t *testing.T) {
		if out := must(t, lobby, `sprite-env sprites list`); !strings.Contains(out, game) || strings.Contains(out, other) || strings.Contains(out, template) {
			t.Fatalf("list from inside: %s", out)
		}
		if out, err := sh(t, lobby, `sprite-env sprites delete `+other); err == nil {
			t.Fatalf("deleted a sprite it did not create: %s", out)
		}
		if _, err := c.GetSprite(ctx, other); err != nil {
			t.Fatalf("bystander: %v", err)
		}
		if out, err := sh(t, game, `sprite-env sprites list`); err == nil || !strings.Contains(out, "may not manage sprites") {
			t.Fatalf("a child can spawn: %v %s", err, out)
		}
		must(t, lobby, `sprite-env sprites delete `+game)
		if _, err := c.GetSprite(ctx, game); err == nil {
			t.Fatal("the game survived its delete")
		}
	})
}
