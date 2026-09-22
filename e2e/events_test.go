//go:build e2e

package e2e

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"slices"
	"strings"
	"testing"
	"time"
)

// event is the stream's JSON (internal/server/events.go).
type event struct {
	ID       uint64         `json:"id"`
	Type     string         `json:"type"`
	Sprite   string         `json:"sprite"`
	ParentID string         `json:"parent_id"`
	Detail   map[string]any `json:"detail"`
}

type frame struct {
	id string // empty for a notice about the stream
	ev event
}

// openEvents opens the operator's event stream; it closes with the test.
func openEvents(t *testing.T, query string, header http.Header) <-chan frame {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, os.Getenv("SPRITES_E2E_URL")+"/mini-sprites/v1/events?"+query, nil)
	req.Header.Set("Authorization", "Bearer "+os.Getenv("SPRITES_E2E_TOKEN"))
	for k, v := range header {
		req.Header[k] = v
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("event stream: %d %s", resp.StatusCode, b)
	}
	out := make(chan frame, 1024)
	go func() {
		defer close(out)
		defer resp.Body.Close()
		sc := bufio.NewScanner(resp.Body)
		var id, data string
		for sc.Scan() {
			switch line := sc.Text(); {
			case strings.HasPrefix(line, "id: "):
				id = strings.TrimPrefix(line, "id: ")
			case strings.HasPrefix(line, "data: "):
				data = strings.TrimPrefix(line, "data: ")
			case line == "":
				if data != "" {
					var f frame
					f.id = id
					json.Unmarshal([]byte(data), &f.ev)
					out <- f
				}
				id, data = "", ""
			}
		}
	}()
	return out
}

// until collects frames until one satisfies stop.
func until(t *testing.T, frames <-chan frame, d time.Duration, stop func(frame) bool) []frame {
	t.Helper()
	var got []frame
	timeout := time.After(d)
	for {
		select {
		case f, ok := <-frames:
			if !ok {
				t.Fatalf("stream ended; had %v", types(got))
			}
			got = append(got, f)
			if stop(f) {
				return got
			}
		case <-timeout:
			t.Fatalf("timed out; had %v", types(got))
		}
	}
}

func types(fs []frame) []string {
	var out []string
	for _, f := range fs {
		s := f.ev.Type
		if m, ok := f.ev.Detail["mode"]; ok {
			s += "(" + fmt.Sprint(m) + ")"
		}
		out = append(out, s)
	}
	return out
}

// uiAction drives the web UI's operator actions (suspend, cool), which the API has no route for.
func uiAction(t *testing.T, name, action string) {
	t.Helper()
	base := os.Getenv("SPRITES_E2E_URL")
	resp, err := http.Post(base+"/ui/login", "application/json", strings.NewReader(`{"token":"`+os.Getenv("SPRITES_E2E_TOKEN")+`"}`))
	if err != nil || resp.StatusCode != http.StatusNoContent {
		t.Fatalf("ui login: %v %v", err, resp)
	}
	req, _ := http.NewRequest(http.MethodPost, base+"/ui/api/sprites/"+name+"/"+action, nil)
	req.Header.Set("X-Mini-Sprites-UI", "1")
	for _, c := range resp.Cookies() {
		req.AddCookie(c)
	}
	r, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	if r.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(r.Body)
		t.Fatalf("%s %s: %d %s", action, name, r.StatusCode, b)
	}
}

// inOrder reports whether want appears in got as a subsequence.
func inOrder(got, want []string) bool {
	i := 0
	for _, g := range got {
		if i < len(want) && g == want[i] {
			i++
		}
	}
	return i == len(want)
}

// TestEventStream drives a sprite through its lifecycle and watches it happen.
func TestEventStream(t *testing.T) {
	c := client(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	name := fmt.Sprintf("e2e-ev-%d", time.Now().UnixNano()%1e9)
	frames := openEvents(t, "sprite="+name, nil)
	t.Cleanup(func() { c.DeleteSprite(context.Background(), name) })

	if _, err := c.CreateSprite(ctx, name, nil); err != nil {
		t.Fatal(err)
	}
	sh := func(script string) string {
		t.Helper()
		out, err := c.Sprite(name).CommandContext(ctx, "bash", "-c", script).CombinedOutput()
		if err != nil {
			t.Fatalf("%s: %v\n%s", script, err, out)
		}
		return string(out)
	}
	// A service that is killed: the agent reports the crash and the restart.
	sh(`sprite-env services create worker --cmd sleep --args 600 --no-stream && sprite-env services signal worker KILL`)
	svc := until(t, frames, 30*time.Second, func(f frame) bool {
		return f.ev.Type == "service.started" && f.ev.Detail["restart_count"] != nil
	})
	want(t, http.MethodPost, "/v1/sprites/"+name+"/checkpoint", "", http.StatusOK)
	uiAction(t, name, "suspend")
	sh(`true`) // wakes it warm
	uiAction(t, name, "suspend")
	uiAction(t, name, "cool")
	want(t, http.MethodDelete, "/v1/sprites/"+name+"/checkpoints/v1", "", http.StatusNoContent)
	want(t, http.MethodPost, "/v1/sprites/"+name+"/policy/privileges", `{"profile":"standard"}`, http.StatusNoContent)
	if err := c.DeleteSprite(ctx, name); err != nil {
		t.Fatal(err)
	}
	rest := until(t, frames, 30*time.Second, func(f frame) bool { return f.ev.Type == "sprite.deleted" })
	all := append(svc, rest...)
	got := types(all)
	t.Logf("events: %v", got)

	if !inOrder(got, []string{"sprite.created", "sprite.woke(cold)", "checkpoint.created", "sprite.suspended",
		"sprite.woke(warm)", "sprite.suspended", "sprite.cold", "checkpoint.deleted", "policy.changed", "sprite.deleted"}) {
		t.Errorf("lifecycle out of order or missing: %v", got)
	}
	if !inOrder(got, []string{"service.started", "service.crashed", "service.started"}) {
		t.Errorf("service crash and restart missing: %v", got)
	}
	var ids []uint64
	for _, f := range all {
		if f.ev.Sprite != name {
			t.Errorf("the sprite filter let through %+v", f.ev)
		}
		if f.ev.Type == "service.crashed" && (f.ev.Detail["service"] != "worker" || f.ev.Detail["exit_code"] != float64(137)) {
			t.Errorf("crash detail: %v", f.ev.Detail)
		}
		if f.ev.Type == "sprite.woke" {
			if ms, ok := f.ev.Detail["ms"].(float64); !ok || ms <= 0 {
				t.Errorf("wake without a latency: %v", f.ev.Detail)
			}
		}
		ids = append(ids, f.ev.ID)
	}
	if !slices.IsSorted(ids) {
		t.Errorf("ids not increasing: %v", ids)
	}

	t.Run("resume with Last-Event-ID", func(t *testing.T) {
		mid := len(all) / 2
		resumed := openEvents(t, "sprite="+name, http.Header{"Last-Event-Id": {all[mid].id}})
		replay := until(t, resumed, 10*time.Second, func(f frame) bool { return f.ev.Type == "sprite.deleted" })
		if len(replay) != len(all)-mid-1 || replay[0].id != all[mid+1].id {
			t.Fatalf("resumed after %s with %v, want %v", all[mid].id, types(replay), got[mid+1:])
		}
	})
	t.Run("an ID that has fallen out is a gap", func(t *testing.T) {
		old := openEvents(t, "sprite="+name+"&last_event_id=1", nil)
		if f := until(t, old, 10*time.Second, func(frame) bool { return true })[0]; f.id != "" || f.ev.Type != "stream.gap" {
			t.Fatalf("want a gap notice first, got %+v", f)
		}
	})
	t.Run("type filter", func(t *testing.T) {
		only := openEvents(t, "sprite="+name+"&type=checkpoint.&last_event_id=0", nil)
		fs := until(t, only, 10*time.Second, func(f frame) bool { return f.ev.Type == "checkpoint.deleted" })
		if strings.Join(types(fs), " ") != "checkpoint.created checkpoint.deleted" {
			t.Fatalf("got %v", types(fs))
		}
	})
}

// TestEventStreamFromInside is the lobby: a spawner follows its own children
// with sprite-env, through its own suspend, and sees nothing of anyone else.
func TestEventStreamFromInside(t *testing.T) {
	c := client(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	n := time.Now().UnixNano() % 1e9
	lobby, bystander := fmt.Sprintf("e2e-evlobby-%d", n), fmt.Sprintf("e2e-evby-%d", n)
	kids := []string{fmt.Sprintf("e2e-evkid1-%d", n), fmt.Sprintf("e2e-evkid2-%d", n), fmt.Sprintf("e2e-evkid3-%d", n)}
	for _, name := range []string{lobby, bystander} {
		if _, err := c.CreateSprite(ctx, name, nil); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		for _, name := range append(kids, lobby, bystander) {
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

	if out, err := sh(t, lobby, `sprite-env sprites events`); err == nil || !strings.Contains(out, "may not manage sprites") {
		t.Fatalf("no spawn policy: %v %s", err, out)
	}
	want(t, http.MethodPost, "/v1/sprites/"+lobby+"/policy/spawn", `{"enabled":true}`, http.StatusNoContent)

	// A follower in the background, as a lobby's status page would run one. It
	// waits for the second and third child, so nothing it hears of comes before
	// the lobby's suspend cuts its stream: it has to resume from the position
	// the server gave it, or it could miss the child created as the lobby wakes.
	must(t, lobby, `setsid nohup sprite-env sprites events --type sprite.created --sprite `+kids[1]+`,`+kids[2]+` --count 2 > /tmp/follow.jsonl 2>/tmp/follow.err < /dev/null &
		sleep 1; pgrep -f "sprite-env sprites events" >/dev/null`)
	must(t, lobby, `sprite-env sprites create `+kids[0])
	// The lobby's suspend cuts its stream; the follower has to come back on its own.
	uiAction(t, lobby, "suspend")
	must(t, bystander, `true`) // someone else's events, meanwhile
	must(t, lobby, `sprite-env sprites create `+kids[1])
	must(t, lobby, `sprite-env sprites create `+kids[2]+` && sprite-env sprites delete `+kids[2])

	var follow string
	for deadline := time.Now().Add(20 * time.Second); time.Now().Before(deadline); time.Sleep(500 * time.Millisecond) {
		if follow = must(t, lobby, `cat /tmp/follow.jsonl`); strings.Count(follow, "\n") >= 2 {
			break
		}
	}
	for i, line := range strings.Split(strings.TrimSpace(follow), "\n") {
		var e event
		if err := json.Unmarshal([]byte(line), &e); err != nil || e.Type != "sprite.created" || e.Sprite != kids[i+1] {
			t.Fatalf("follower line %d: %q (%v); stderr: %s", i, line, err, must(t, lobby, `cat /tmp/follow.err`))
		}
	}

	// Everything the lobby can see, from the buffer: its children, and only them.
	out, _ := sh(t, lobby, `timeout 3 sprite-env sprites events --all`)
	var seen []string
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		var e event
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatalf("not an event: %q", line)
		}
		if !slices.Contains(kids, e.Sprite) {
			t.Errorf("the lobby saw another sprite's event: %s", line)
		}
		seen = append(seen, e.Type+":"+e.Sprite)
	}
	if !inOrder(seen, []string{"sprite.created:" + kids[0], "sprite.created:" + kids[1], "sprite.created:" + kids[2], "sprite.deleted:" + kids[2]}) {
		t.Errorf("lobby saw %v", seen)
	}
	// A child is no spawner, so it sees nothing at all.
	if out, err := sh(t, kids[0], `sprite-env sprites events`); err == nil || !strings.Contains(out, "may not manage sprites") {
		t.Fatalf("a child followed events: %v %s", err, out)
	}
}
