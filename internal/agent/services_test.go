package agent

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

func newServiceServer(t *testing.T) (*httptest.Server, *Supervisor, string) {
	dir := t.TempDir()
	sv := NewSupervisor(dir+"/state", dir+"/run")
	ts := httptest.NewServer((&Server{Sessions: NewManager(), Services: sv}).Handler())
	t.Cleanup(func() {
		ts.Close()
		for _, s := range sv.List() {
			sv.Stop(s.Name, time.Second)
		}
	})
	return ts, sv, dir
}

// put creates a service and returns the streamed events.
func put(t *testing.T, ts *httptest.Server, name, body, query string) (int, []ServiceEvent) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPut, ts.URL+"/services/"+name+"?"+query, strings.NewReader(body))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var evs []ServiceEvent
	if resp.StatusCode == http.StatusOK {
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			var ev ServiceEvent
			if err := json.Unmarshal(sc.Bytes(), &ev); err != nil {
				t.Fatalf("bad NDJSON line %q: %v", sc.Text(), err)
			}
			evs = append(evs, ev)
		}
	}
	return resp.StatusCode, evs
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if cond() {
			return
		}
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestServiceCreateStreamsLogsAndWritesLogFile(t *testing.T) {
	ts, sv, _ := newServiceServer(t)
	code, evs := put(t, ts, "web", `{"cmd":"sh","args":["-c","echo hello; echo oops >&2; sleep 30"],"env":{"A":"b"}}`, "duration=400ms")
	if code != http.StatusOK {
		t.Fatalf("status %d", code)
	}
	var types []string
	for _, e := range evs {
		types = append(types, e.Type+":"+e.Data)
	}
	got := strings.Join(types, " ")
	for _, want := range []string{"started:pid", "stdout:hello", "stderr:oops", "complete:"} {
		if !strings.Contains(got, want) {
			t.Errorf("stream %q missing %q", got, want)
		}
	}
	if last := evs[len(evs)-1]; last.Type != "complete" || last.LogFiles["combined"] == "" {
		t.Errorf("last event = %+v", last)
	}
	svc, _ := sv.Get("web")
	if svc.State.Status != "running" || svc.State.PID == 0 || svc.State.StartedAt == nil {
		t.Fatalf("state = %+v", svc.State)
	}
	log, _ := os.ReadFile(sv.LogPath("web"))
	if !bytes.Contains(log, []byte("[stdout] hello")) || !bytes.Contains(log, []byte("[stderr] oops")) {
		t.Fatalf("log file = %q", log)
	}
}

func TestServiceCrashRestartsAndStopIsSticky(t *testing.T) {
	ts, sv, _ := newServiceServer(t)
	put(t, ts, "flaky", `{"cmd":"sleep","args":["30"]}`, "duration=50ms")
	first, _ := sv.Get("flaky")

	// Killing it behind the supervisor's back counts as a crash.
	resp, _ := http.Post(ts.URL+"/services/signal", "application/json", strings.NewReader(`{"name":"flaky","signal":"KILL"}`))
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("signal status %d", resp.StatusCode)
	}
	waitFor(t, "restart after crash", func() bool {
		s, _ := sv.Get("flaky")
		return s.State.Status == "running" && s.State.PID != 0 && s.State.PID != first.State.PID && s.State.RestartCount == 1
	})

	resp, _ = http.Post(ts.URL+"/services/flaky/stop?timeout=2s", "", nil)
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), `"type":"stopped"`) || !strings.Contains(string(body), `"type":"complete"`) {
		t.Fatalf("stop stream: %s", body)
	}
	time.Sleep(1500 * time.Millisecond) // longer than the restart backoff
	if s, _ := sv.Get("flaky"); s.State.Status != "stopped" || s.State.PID != 0 {
		t.Fatalf("stopped service came back: %+v", s.State)
	}
	resp, _ = http.Post(ts.URL+"/services/signal", "application/json", strings.NewReader(`{"name":"flaky","signal":"TERM"}`))
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("signal to stopped service: status %d, want 409", resp.StatusCode)
	}
}

func TestServiceMissingBinaryIsReportedNotFatal(t *testing.T) {
	ts, sv, _ := newServiceServer(t)
	code, evs := put(t, ts, "ghost", `{"cmd":"no-such-binary-anywhere"}`, "duration=50ms")
	if code != http.StatusOK || evs[0].Type != "error" {
		t.Fatalf("code=%d events=%+v", code, evs)
	}
	if s, _ := sv.Get("ghost"); s.State.Status != "failed" || s.State.NextRestartAt == nil {
		t.Fatalf("state = %+v", s.State)
	}
}

func TestServiceValidation(t *testing.T) {
	ts, _, _ := newServiceServer(t)
	put(t, ts, "a", `{"cmd":"sleep","args":["30"],"http_port":3000}`, "duration=10ms")
	if code, _ := put(t, ts, "b", `{"cmd":"sleep","args":["30"],"http_port":4000}`, ""); code != http.StatusConflict {
		t.Errorf("second http_port service: status %d, want 409", code)
	}
	if code, _ := put(t, ts, "c", `{"cmd":"sleep","needs":["nope"]}`, ""); code != http.StatusBadRequest {
		t.Errorf("unknown dependency: status %d, want 400", code)
	}
	put(t, ts, "dep", `{"cmd":"sleep","args":["30"],"needs":["a"]}`, "duration=10ms")
	req, _ := http.NewRequest(http.MethodDelete, ts.URL+"/services/a", nil)
	if resp, _ := http.DefaultClient.Do(req); resp.StatusCode != http.StatusConflict {
		t.Errorf("deleting a needed service: status %d, want 409", resp.StatusCode)
	}
	if resp, _ := http.Get(ts.URL + "/services/missing"); resp.StatusCode != http.StatusNotFound {
		t.Errorf("get missing: status %d", resp.StatusCode)
	}
}

func TestServicesStartInDependencyOrderOnBoot(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(dir+"/state/services", 0o755)
	order := dir + "/order"
	// Names sort opposite to the dependency order, so alphabetical startup would fail this.
	for name, needs := range map[string]string{"a-app": `["m-cache"]`, "m-cache": `["z-db"]`, "z-db": `[]`} {
		def := fmt.Sprintf(`{"name":%q,"cmd":"sh","args":["-c","echo %s >> %s; sleep 30"],"needs":%s}`, name, name, order, needs)
		os.WriteFile(dir+"/state/services/"+name+".json", []byte(def), 0o644)
	}
	sv := NewSupervisor(dir+"/state", dir+"/run") // what happens at cold boot
	t.Cleanup(func() {
		for _, s := range sv.List() {
			sv.Stop(s.Name, time.Second)
		}
	})
	var got string
	waitFor(t, "all three services to start", func() bool {
		b, _ := os.ReadFile(order)
		got = string(b)
		return strings.Count(got, "\n") == 3
	})
	// Spawn order is what we control; each writes its line immediately, but allow for scheduling jitter
	// only between independent services. Here the chain is strict, so z-db must be spawned first.
	list := sv.List()
	byName := map[string]ServiceWithState{}
	for _, s := range list {
		byName[s.Name] = s
	}
	if !(byName["z-db"].State.StartedAt.Before(*byName["m-cache"].State.StartedAt) &&
		byName["m-cache"].State.StartedAt.Before(*byName["a-app"].State.StartedAt)) {
		t.Fatalf("start order wrong: %s", got)
	}
}
