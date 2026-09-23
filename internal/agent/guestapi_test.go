package agent

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestGuestAPISurface(t *testing.T) {
	// Stands in for wispd's per-sprite channel.
	host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "host saw "+r.Method+" "+r.URL.RequestURI())
	}))
	defer host.Close()
	dial := func(ctx context.Context) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "tcp", host.Listener.Addr().String())
	}
	dir := t.TempDir()
	sv := NewSupervisor(dir+"/state", dir+"/run")
	srv := &Server{Sessions: NewManager(), Services: sv}
	ts := httptest.NewServer(srv.GuestAPI(dial))
	defer ts.Close()
	t.Cleanup(func() { sv.Stop("s", time.Second) })

	do := func(method, path, body string) (int, string) {
		t.Helper()
		req, _ := http.NewRequest(method, ts.URL+path, strings.NewReader(body))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}

	if code, body := do(http.MethodPut, "/v1/services/s?duration=0s", `{"cmd":"sleep","args":["60"]}`); code != 200 || !strings.Contains(body, `"started"`) {
		t.Fatalf("create service: %d %s", code, body)
	}
	if code, body := do(http.MethodGet, "/v1/services", ""); code != 200 || !strings.Contains(body, `"name":"s"`) {
		t.Errorf("list services: %d %s", code, body)
	}
	if code, body := do(http.MethodGet, "/v1/services/s", ""); code != 200 || !strings.Contains(body, `"running"`) {
		t.Errorf("get service: %d %s", code, body)
	}
	for path, want := range map[string]string{
		"/v1/checkpoints?includeAuto=true": "host saw GET /v1/checkpoints?includeAuto=true",
		"/v1/checkpoints/v1":               "host saw GET /v1/checkpoints/v1",
	} {
		if code, body := do(http.MethodGet, path, ""); code != 200 || body != want {
			t.Errorf("%s: %d %q", path, code, body)
		}
	}
	if _, body := do(http.MethodPost, "/v1/checkpoints/v1/restore", ""); body != "host saw POST /v1/checkpoints/v1/restore" {
		t.Errorf("restore relayed as %q", body)
	}

	// The sprites this one created are the host's to answer for (403 without a spawn policy).
	if _, body := do(http.MethodDelete, "/v1/sprites/game-1", ""); body != "host saw DELETE /v1/sprites/game-1" {
		t.Errorf("sprite delete relayed as %q", body)
	}

	// So is the event stream about them.
	if _, body := do(http.MethodGet, "/wisp/v1/events?type=sprite.", ""); body != "host saw GET /wisp/v1/events?type=sprite." {
		t.Errorf("events relayed as %q", body)
	}

	// Everything else the agent can do stays on the vsock side, and nothing but
	// checkpoints and sprites reaches the host.
	for _, path := range []string{"/exec", "/v1/exec", "/healthz", "/fs/read?path=/etc/shadow", "/v1/fs/read?path=/etc/shadow",
		"/proxy", "/internal/poweroff", "/v1/internal/poweroff", "/services", "/v1/spritesx", "/sprites", "/v1/checkpointsx",
		"/internal/service-event", "/wisp/v1/webhooks", "/wisp/v1/eventsx"} {
		for _, method := range []string{http.MethodGet, http.MethodPost} {
			if code, body := do(method, path, ""); code != http.StatusNotFound || strings.Contains(body, "host saw") {
				t.Errorf("%s %s: %d %q, want a local 404", method, path, code, body)
			}
		}
	}
}

func TestGuestAPIWithoutHost(t *testing.T) {
	down := func(context.Context) (net.Conn, error) { return nil, io.ErrClosedPipe }
	ts := httptest.NewServer((&Server{Sessions: NewManager()}).GuestAPI(down))
	defer ts.Close()
	resp, err := http.Get(ts.URL + "/v1/checkpoints")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", resp.StatusCode)
	}
}
