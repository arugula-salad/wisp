//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

// TestCreateFromImage starts a sprite from a container image. It pulls from a
// registry, so it runs only when asked: SPRITES_E2E_IMAGES=1. The image is
// alpine, the odd userland (busybox sh, no bash, no sudo).
func TestCreateFromImage(t *testing.T) {
	if os.Getenv("SPRITES_E2E_IMAGES") == "" {
		t.Skip("set SPRITES_E2E_IMAGES=1 to pull an image from Docker Hub")
	}
	c := client(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	n := time.Now().UnixNano() % 1e9
	name, child := fmt.Sprintf("e2e-img-%d", n), fmt.Sprintf("e2e-imgkid-%d", n)
	t.Cleanup(func() {
		c.DeleteSprite(context.Background(), child)
		c.DeleteSprite(context.Background(), name)
	})

	// Blocks for the pull when the image is not cached yet.
	start := time.Now()
	body := want(t, http.MethodPost, "/v1/sprites", `{"name":"`+name+`","from":{"image":"alpine:3.20"},"url_settings":{"auth":"public"}}`, http.StatusCreated)
	if !strings.Contains(body, `"source_image":"docker.io/library/alpine@sha256:`) {
		t.Fatalf("create: %s", body)
	}
	t.Logf("create (pull on a miss): %v", time.Since(start).Round(time.Millisecond))

	sh := func(t *testing.T, sprite, script string) string {
		t.Helper()
		out, err := c.Sprite(sprite).CommandContext(ctx, "sh", "-c", script).CombinedOutput()
		if err != nil {
			t.Fatalf("%s: %v\n%s", script, err, out)
		}
		return string(out)
	}
	if out := sh(t, name, `id -un; echo $HOME; grep ^ID= /etc/os-release; sudo id -u`); out != "sprite\n/home/sprite\nID=alpine\n0\n" {
		t.Fatalf("userland: %q", out)
	}
	// alpine's busybox has no httpd applet; nc, kept listening, answers well enough.
	sh(t, name, `printf '#!/bin/sh\nprintf "HTTP/1.0 200 OK\\r\\nContent-Length: 12\\r\\n\\r\\nfrom-alpine\\n"\n' > ~/resp.sh && chmod +x ~/resp.sh &&
		sprite-env services create web --cmd nc --args -lk,-p,8080,-e,/home/sprite/resp.sh --http-port 8080 --duration 1s`)
	if got := fetchURL(t, name); got != "from-alpine\n" {
		t.Fatalf("sprite URL served %q", got)
	}

	// A cached image clones without a pull; from inside only cached images work.
	want(t, http.MethodPost, "/v1/sprites/"+name+"/policy/spawn", `{"enabled":true}`, http.StatusNoContent)
	if out := sh(t, name, `sprite-env sprites create `+child+` --image alpine:3.20 >/dev/null && echo ok`); out != "ok\n" {
		t.Fatalf("spawn from a cached image: %s", out)
	}
	out, err := c.Sprite(name).CommandContext(ctx, "sprite-env", "sprites", "create", child+"-x", "--image", "docker.io/library/alpine:edge-not-cached").CombinedOutput()
	if err == nil || !strings.Contains(string(out), "image_not_cached") && !strings.Contains(string(out), "not in this host's image cache") {
		t.Fatalf("a guest got an uncached image: %v %s", err, out)
	}
}
