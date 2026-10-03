package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeSystemDef(t *testing.T, dir, file, body string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, file), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func stopAll(sv *Supervisor) {
	for _, s := range sv.List() {
		sv.Stop(s.Name, time.Second)
	}
}

func TestSystemServicesAbsentLeaveNoTrace(t *testing.T) {
	dir := t.TempDir()
	for _, defs := range []string{dir + "/missing", dir + "/empty"} {
		os.MkdirAll(dir+"/empty", 0o755)
		if sv := NewSystemSupervisor(defs, dir+"/logs", dir+"/run"); sv != nil {
			stopAll(sv)
			t.Fatalf("%s: got a supervisor for no services", defs)
		}
	}
	for _, p := range []string{dir + "/logs", dir + "/run"} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("%s was created for an image with no system services", p)
		}
	}
}

func TestSystemServiceRunsAndRestarts(t *testing.T) {
	dir := t.TempDir()
	defs := dir + "/services.d"
	writeSystemDef(t, defs, "daemon.json", `{"cmd":"sh","args":["-c","echo up $GREETING; pwd; exit 3"],
		"user":"root","env":{"GREETING":"hi"},"dir":"`+dir+`"}`)
	// Skipped, each for its own reason; the good one still starts.
	writeSystemDef(t, defs, "typo.json", `{"cmd":"sh","agrs":["-c","true"]}`)
	writeSystemDef(t, defs, "nocmd.json", `{"args":["x"]}`)
	writeSystemDef(t, defs, "bad name!.json", `{"cmd":"true"}`)
	writeSystemDef(t, defs, "README", `not a definition`)

	sv := NewSystemSupervisor(defs, dir+"/logs", dir+"/run")
	if sv == nil {
		t.Fatal("no supervisor")
	}
	defer stopAll(sv)
	if got := sv.List(); len(got) != 1 || got[0].Name != "daemon" {
		t.Fatalf("services = %+v, want only daemon", got)
	}
	// It exits at once, so it crashes, and is restarted (after a 1s backoff).
	waitFor(t, "a restart", func() bool {
		s, _ := sv.Get("daemon")
		return s.State.RestartCount >= 1 && s.State.Status == "running" || s.State.RestartCount >= 2
	})
	waitFor(t, "two runs in the log", func() bool {
		b, _ := os.ReadFile(filepath.Join(dir, "logs", "daemon.log"))
		return strings.Count(string(b), "[stdout] up hi") >= 2 && strings.Contains(string(b), "[stdout] "+dir)
	})
}

func TestSystemServiceUnknownUserFails(t *testing.T) {
	dir := t.TempDir()
	writeSystemDef(t, dir+"/d", "x.json", `{"cmd":"true","user":"no-such-user-here"}`)
	sv := NewSystemSupervisor(dir+"/d", dir+"/logs", dir+"/run")
	if sv == nil {
		t.Fatal("no supervisor")
	}
	defer stopAll(sv)
	s, _ := sv.Get("x")
	if s.State.Status != "failed" || !strings.Contains(s.State.Error, "no-such-user-here") {
		t.Fatalf("state = %+v, want failed naming the user", s.State)
	}
}
