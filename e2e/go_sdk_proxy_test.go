//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// TestGoSDKProxyPortsWithDefaultOptions: the official Go SDK, configured the way
// its README shows, must be able to forward a port. It could not while the
// server offered it /control: over control the SDK's pool reader and its proxy
// handshake both read the one socket, and the forward hung whenever the pool
// reader won. wispd therefore answers that SDK's /control probe with 404.
func TestGoSDKProxyPortsWithDefaultOptions(t *testing.T) {
	if os.Getenv("SPRITES_E2E_GO_CONTROL") != "" {
		t.Skip("the daemon offers /control to the Go SDK, where this is known to race")
	}
	c := client(t) // default options: control NOT disabled
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	name := fmt.Sprintf("e2e-gopx-%d", time.Now().UnixNano()%1e9)
	sp, err := c.CreateSprite(ctx, name, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.DeleteSprite(context.Background(), name) })

	cmd := sp.CommandContext(ctx, "sh", "-c", `mkdir -p ~/w && echo forwarded > ~/w/index.html && cd ~/w && (setsid python3 -m http.server 8080 >/dev/null 2>&1 &); sleep 0.6`)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("start server: %v %s", err, out)
	}
	if mode := cmd.ConnectionMode(); mode != "direct" {
		t.Fatalf("the Go SDK ran an exec over a %q connection; it should have been told there is no control channel", mode)
	}

	sess, err := sp.ProxyPort(ctx, 0, 8080)
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	// One TCP connection per request: every one repeats the SDK's control probe and proxy
	// handshake, which is where the race was. It lost about two times in three.
	hc := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{DisableKeepAlives: true}}
	for i := 0; i < 15; i++ {
		resp, err := hc.Get("http://" + sess.LocalAddr().String() + "/")
		if err != nil {
			t.Fatalf("request %d through the forwarded port: %v", i, err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if strings.TrimSpace(string(body)) != "forwarded" {
			t.Fatalf("request %d: body %q", i, body)
		}
	}
}

// TestControlIsWithheldOnlyFromTheGoSDK pins down who gets the channel.
func TestControlIsWithheldOnlyFromTheGoSDK(t *testing.T) {
	if os.Getenv("SPRITES_E2E_GO_CONTROL") != "" {
		t.Skip("the daemon offers /control to everyone")
	}
	c := client(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	name := fmt.Sprintf("e2e-ua-%d", time.Now().UnixNano()%1e9)
	if _, err := c.CreateSprite(ctx, name, nil); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.DeleteSprite(context.Background(), name) })

	u := e2eWS("/v1/sprites/" + name + "/control")
	for ua, wantStatus := range map[string]int{
		"sprites-go-sdk/1.0":          http.StatusNotFound,           // falls back to a socket per operation
		"":                            http.StatusSwitchingProtocols, // JS (Node sends none) and raw clients
		"Python/3.14 websockets/15.0": http.StatusSwitchingProtocols,
	} {
		h := e2eAuth()
		if ua != "" {
			h.Set("User-Agent", ua)
		}
		conn, resp, err := websocket.DefaultDialer.Dial(u, h)
		if conn != nil {
			conn.Close()
		}
		if resp == nil || resp.StatusCode != wantStatus {
			t.Errorf("User-Agent %q: got %v (err %v), want HTTP %d", ua, resp, err, wantStatus)
		}
	}
}
