package agent

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestTasksAPI(t *testing.T) {
	m := NewManager()
	srv := &Server{Sessions: m}
	// Served the way the guest serves it: on the one in-guest socket, next to everything else mounted there.
	sock := filepath.Join(t.TempDir(), "api.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go http.Serve(ln, srv.GuestAPI(nil))
	hc := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", sock)
	}}}
	do := func(method, path, body string, want int) Task {
		t.Helper()
		var resp *http.Response
		var err error
		for i := 0; i < 100; i++ { // the socket appears asynchronously
			req, _ := http.NewRequest(method, "http://sprite"+path, strings.NewReader(body))
			if resp, err = hc.Do(req); err == nil {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != want {
			t.Fatalf("%s %s: %d, want %d", method, path, resp.StatusCode, want)
		}
		var task Task
		json.NewDecoder(resp.Body).Decode(&task)
		return task
	}

	created := do("POST", "/v1/tasks", `{"name":"agent","expire":"30m"}`, http.StatusCreated)
	if d := created.ExpiresAt.Sub(created.StartedAt); created.Name != "agent" || d != 30*time.Minute {
		t.Fatalf("created: %+v", created)
	}
	do("POST", "/v1/tasks", `{"name":"agent","expire":"1h"}`, http.StatusConflict)
	do("POST", "/v1/tasks", `{"name":"long","expire":"2h"}`, http.StatusBadRequest)
	do("POST", "/v1/tasks", `{"name":"neg","expire":-1}`, http.StatusBadRequest)
	do("POST", "/v1/tasks", `{"expire":60}`, http.StatusBadRequest)

	// Both upsert forms refresh the expiry and keep started_at; integer seconds work too.
	refreshed := do("PUT", "/v1/tasks/agent", `{"expire":120}`, http.StatusOK)
	if !refreshed.StartedAt.Equal(created.StartedAt) || refreshed.ExpiresAt.Sub(refreshed.StartedAt) > 3*time.Minute {
		t.Fatalf("refreshed: %+v (created %+v)", refreshed, created)
	}
	do("PUT", "/v1/tasks", `{"name":"other"}`, http.StatusOK)
	do("GET", "/v1/tasks/other", "", http.StatusOK)
	if n := len(srv.tasks.live()); n != 2 {
		t.Fatalf("live tasks: %d", n)
	}

	do("DELETE", "/v1/tasks/other", "", http.StatusNoContent)
	do("DELETE", "/v1/tasks/other", "", http.StatusNotFound)
	do("GET", "/v1/tasks/other", "", http.StatusNotFound)

	// Expiry is what stops a crashed client from holding the sprite forever.
	srv.tasks.put("agent", 50*time.Millisecond, false)
	time.Sleep(60 * time.Millisecond)
	if n := len(srv.tasks.live()); n != 0 {
		t.Fatalf("live tasks after expiry: %d", n)
	}
	do("GET", "/v1/tasks/agent", "", http.StatusNotFound)

	for i := 0; i < maxTasks; i++ {
		do("PUT", "/v1/tasks/t"+strings.Repeat("x", i), `{"expire":60}`, http.StatusOK)
	}
	do("PUT", "/v1/tasks/one-too-many", `{"expire":60}`, http.StatusTooManyRequests)
	do("PUT", "/v1/tasks/t", `{"expire":60}`, http.StatusOK) // refreshing an existing one still works at the limit
}
