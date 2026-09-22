package agent

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

func TestSupervisorReportsStartsCrashesAndStops(t *testing.T) {
	var mu sync.Mutex
	var got []ServiceReport
	dir := t.TempDir()
	sv := NewReportingSupervisor(dir+"/state", dir+"/run", func(r ServiceReport) {
		mu.Lock()
		got = append(got, r)
		mu.Unlock()
	})
	reports := func() []ServiceReport {
		mu.Lock()
		defer mu.Unlock()
		return append([]ServiceReport(nil), got...)
	}
	if err := sv.Define(ServiceDef{Name: "s", Cmd: "sleep", Args: []string{"60"}}); err != nil {
		t.Fatal(err)
	}
	sv.Start("s")
	t.Cleanup(func() { sv.Stop("s", time.Second) })
	waitFor(t, "start", func() bool { return len(reports()) == 1 })
	sv.Signal("s", syscall.SIGKILL)
	// A crash, then the restart after the backoff.
	waitFor(t, "crash and restart", func() bool { return len(reports()) == 3 })
	sv.Stop("s", time.Second)
	waitFor(t, "stop", func() bool { return len(reports()) == 4 })

	r := reports()
	if r[0].Type != "started" || r[0].PID == 0 || r[0].Service != "s" {
		t.Errorf("start: %+v", r[0])
	}
	if r[1].Type != "crashed" || r[1].ExitCode == nil || *r[1].ExitCode != 128+9 || r[1].RestartCount != 1 || r[1].RestartInMS != 1000 {
		t.Errorf("crash: %+v", r[1])
	}
	if r[2].Type != "started" || r[2].RestartCount != 1 {
		t.Errorf("restart: %+v", r[2])
	}
	if r[3].Type != "stopped" {
		t.Errorf("stop: %+v", r[3])
	}
}

func TestReporterPostsToTheHost(t *testing.T) {
	got := make(chan ServiceReport, 4)
	var calls atomic.Int32
	host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			http.Error(w, "try again", http.StatusBadGateway) // retried
			return
		}
		var rep ServiceReport
		json.NewDecoder(r.Body).Decode(&rep)
		if r.URL.Path != "/internal/service-event" {
			t.Errorf("posted to %s", r.URL.Path)
		}
		got <- rep
	}))
	defer host.Close()
	r := NewReporter(func(ctx context.Context) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "tcp", host.Listener.Addr().String())
	})
	r.retry = time.Millisecond
	r.Report(ServiceReport{Type: "started", Service: "web", PID: 42})
	select {
	case rep := <-got:
		if rep.Service != "web" || rep.PID != 42 {
			t.Fatalf("got %+v", rep)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("nothing reached the host")
	}
}
