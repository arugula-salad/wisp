//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"
)

// fetchSpriteURL GETs the sprite's own URL and reports what came back, however
// it came back: the readiness gate's answers are the point of these tests, so
// unlike fetchURL nothing here treats a non-200 as a fatal error.
func fetchSpriteURL(t *testing.T, sprite string) (int, http.Header, string) {
	t.Helper()
	base, _ := url.Parse(os.Getenv("SPRITES_E2E_URL"))
	req, _ := http.NewRequest(http.MethodGet, base.String()+"/", nil)
	req.Host = sprite + ".sprites.localhost:" + base.Port()
	req.Header.Set("Authorization", "Bearer "+os.Getenv("SPRITES_E2E_TOKEN"))
	resp, err := (&http.Client{Timeout: 2 * time.Minute}).Do(req)
	if err != nil {
		t.Fatalf("fetch sprite URL: %v", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Header, string(b)
}

// TestSpriteURLReadinessGate covers what a visitor sees when a sprite's app is
// not listening yet, which is the normal state of a sprite that has just cold
// booted: a bounded wait rather than a proxy error, and an honest 503 when the
// wait runs out. The app here is a service without an http_port, so the guest
// agent does not wait for it either (that path has its own wait); what is being
// measured is the daemon's gate in internal/server/urlproxy.go.
func TestSpriteURLReadinessGate(t *testing.T) {
	c := client(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	name := fmt.Sprintf("e2e-ready-%d", time.Now().UnixNano()%1e9)
	if _, err := c.CreateSprite(ctx, name, nil); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.DeleteSprite(context.Background(), name) })

	t.Run("nothing listening: 503 with Retry-After, not a proxy error", func(t *testing.T) {
		start := time.Now()
		code, hdr, body := fetchSpriteURL(t, name)
		elapsed := time.Since(start)
		if code != http.StatusServiceUnavailable {
			t.Fatalf("got %d %q, want 503 from the readiness gate", code, body)
		}
		if hdr.Get("Retry-After") == "" {
			t.Errorf("no Retry-After on the 503")
		}
		if !strings.Contains(body, "starting up") {
			t.Errorf("body %q does not tell the visitor the sprite is starting", body)
		}
		// Bounded: the wait has a hard ceiling of a minute, plus the wake itself.
		if elapsed > 2*time.Minute {
			t.Errorf("the gate held the request for %v", elapsed)
		}
		t.Logf("gave up after %v", elapsed.Round(time.Millisecond))
	})

	// An app that binds its port a few seconds after the request arrives, the
	// way a real one does when its sprite has just been woken.
	out, err := c.Sprite(name).CommandContext(ctx, "bash", "-c",
		`mkdir -p ~/site && echo "up at last" > ~/site/index.html &&
		 sprite-env services create slow --cmd bash --dir /home/sprite/site \
		   --args "-c,sleep 4; exec python3 -m http.server 8080" --duration 0s`).CombinedOutput()
	if err != nil {
		t.Fatalf("create the slow service: %v\n%s", err, out)
	}

	t.Run("a slow app is waited for rather than refused", func(t *testing.T) {
		start := time.Now()
		code, _, body := fetchSpriteURL(t, name)
		elapsed := time.Since(start)
		if code != http.StatusOK || !strings.Contains(body, "up at last") {
			t.Fatalf("got %d %q after %v, want the app's page once it was listening", code, body, elapsed)
		}
		t.Logf("served after waiting %v", elapsed.Round(time.Millisecond))
	})
}

// TestServiceLogsAreRotated checks the other half from inside a sprite: a
// service that prints without end no longer grows its log without end, and a
// deleted service's logs do not stay behind. The rotation lives in the guest
// agent, which ships in the initramfs, so this test only passes against a
// daemon whose initrd was rebuilt (make initrd) and on a sprite that has cold
// booted since.
func TestServiceLogsAreRotated(t *testing.T) {
	c := client(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	name := fmt.Sprintf("e2e-logs-%d", time.Now().UnixNano()%1e9)
	if _, err := c.CreateSprite(ctx, name, nil); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.DeleteSprite(context.Background(), name) })

	// The defaults are what ships (8 MiB live, 2 rotations kept); the sprite can
	// lower them in /.sprite/logrotate.json, but that file is read when the agent
	// starts, so changing it here would need a reboot to take effect. Printing
	// past the default is quick enough not to bother.
	script := `set -e
	sprite-env services create chatty --cmd bash \
	  --args "-c,for i in \$(seq 1 500); do head -c 65000 /dev/zero | tr '\0' 'x'; echo; done; sleep 600" --duration 0s
	sleep 20
	ls /.sprite/logs/services/
	wc -c /.sprite/logs/services/chatty.log*`
	out, err := c.Sprite(name).CommandContext(ctx, "bash", "-c", script).CombinedOutput()
	if err != nil {
		t.Fatalf("run the chatty service: %v\n%s", err, out)
	}
	t.Logf("%s", out)
	if !strings.Contains(string(out), "chatty.log.1") {
		t.Errorf("32 MiB of output produced no rotated log:\n%s", out)
	}
	if strings.Contains(string(out), "chatty.log.3") {
		t.Errorf("rotations past keep=2 were retained:\n%s", out)
	}

	// The whole directory has to stay inside max_bytes*(keep+1) plus a line.
	sizes, err := c.Sprite(name).CommandContext(ctx, "bash", "-c",
		`sprite-env services delete chatty >/dev/null 2>&1 || true; sleep 1; ls /.sprite/logs/services/ | wc -l`).CombinedOutput()
	if err != nil {
		t.Fatalf("delete the service: %v\n%s", err, sizes)
	}
	if strings.TrimSpace(string(sizes)) != "0" {
		t.Errorf("a deleted service left logs behind: %q", sizes)
	}
}
