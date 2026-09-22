package agent

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// newQuietSupervisor is a supervisor with no services running: the rotation
// tests drive the log path directly, which is deterministic where a real
// process's output is not.
func newQuietSupervisor(t *testing.T, rot LogRotation) (*Supervisor, string) {
	t.Helper()
	dir := t.TempDir()
	sv := NewSupervisor(dir+"/state", dir+"/run")
	sv.SetLogRotation(rot)
	return sv, dir + "/state"
}

// define registers a service without starting it (Define leaves the start to
// the caller), so nothing but the test writes to its log.
func define(t *testing.T, sv *Supervisor, name string) *service {
	t.Helper()
	if err := sv.Define(ServiceDef{Name: name, Cmd: "sleep", Args: []string{"30"}}); err != nil {
		t.Fatal(err)
	}
	sv.mu.Lock()
	defer sv.mu.Unlock()
	return sv.services[name]
}

// say writes one log line the way a service's output would.
func say(t *testing.T, sv *Supervisor, s *service, text string) {
	t.Helper()
	sv.mu.Lock()
	defer sv.mu.Unlock()
	if s.logFile == nil {
		if err := sv.openLogLocked(s); err != nil {
			t.Fatal(err)
		}
	}
	sv.emitLocked(s, ServiceEvent{Type: "stdout", Data: text})
}

func logDirFiles(t *testing.T, sv *Supervisor) []string {
	t.Helper()
	entries, err := os.ReadDir(sv.logsDir())
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	return names
}

func dirBytes(t *testing.T, sv *Supervisor) int64 {
	t.Helper()
	var total int64
	for _, n := range logDirFiles(t, sv) {
		st, err := os.Stat(filepath.Join(sv.logsDir(), n))
		if err != nil {
			t.Fatal(err)
		}
		total += st.Size()
	}
	return total
}

// The point of the feature: however much a service prints, its logs stop
// growing, and the newest output is still the output you get.
func TestServiceLogRotationBoundsTheDisk(t *testing.T) {
	rot := LogRotation{MaxBytes: 2000, Keep: 2}
	sv, _ := newQuietSupervisor(t, rot)
	s := define(t, sv, "chatty")

	for i := 0; i < 400; i++ {
		say(t, sv, s, fmt.Sprintf("line %03d %s", i, strings.Repeat("x", 60)))
	}

	want := []string{"chatty.log", "chatty.log.1", "chatty.log.2"}
	if got := logDirFiles(t, sv); !equal(got, want) {
		t.Fatalf("log files = %v, want %v (a third rotation must fall off the end)", got, want)
	}
	// A line may cross the limit before it is noticed, hence the one-line slack.
	if limit := rot.MaxBytes*int64(rot.Keep+1) + 200; dirBytes(t, sv) > limit {
		t.Fatalf("logs hold %d bytes, want at most %d", dirBytes(t, sv), limit)
	}
	live, err := os.ReadFile(sv.LogPath("chatty"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(live), "line 399") {
		t.Fatalf("the newest line is not in the live log: %q", tailOf(string(live)))
	}
}

// The handle the service writes through is swapped as part of the rotation, so
// output after it lands in the new file rather than in a renamed or closed one.
func TestServiceLogRotationHandsOverTheOpenFile(t *testing.T) {
	sv, _ := newQuietSupervisor(t, LogRotation{MaxBytes: 300, Keep: 1})
	s := define(t, sv, "web")
	say(t, sv, s, "first")
	sv.mu.Lock()
	before := s.logFile
	sv.mu.Unlock()

	for i := 0; i < 20; i++ {
		say(t, sv, s, fmt.Sprintf("noise %d %s", i, strings.Repeat("y", 40)))
	}
	say(t, sv, s, "after the rotation")

	sv.mu.Lock()
	after, bytesHeld := s.logFile, s.logBytes
	sv.mu.Unlock()
	if after == before {
		t.Fatal("the service is still writing through the pre-rotation handle")
	}
	if _, err := before.Write([]byte("x")); err == nil {
		t.Fatal("the pre-rotation handle was left open")
	}
	live, _ := os.ReadFile(sv.LogPath("web"))
	if !strings.Contains(string(live), "after the rotation") {
		t.Fatalf("post-rotation output missed the live log: %q", live)
	}
	if int64(len(live)) != bytesHeld {
		t.Fatalf("the tracked size %d does not match the file's %d", bytesHeld, len(live))
	}
	if first, err := os.ReadFile(sv.LogPath("web") + ".1"); err != nil || !strings.Contains(string(first), "noise") {
		t.Fatalf("rotated file = %q (%v)", first, err)
	}
}

// A reader streaming the log keeps reading a whole file: rotation renames, it
// never truncates under an open descriptor.
func TestServiceLogRotationLeavesAReaderWhole(t *testing.T) {
	sv, _ := newQuietSupervisor(t, LogRotation{MaxBytes: 400, Keep: 1})
	s := define(t, sv, "web")
	for i := 0; i < 3; i++ {
		say(t, sv, s, fmt.Sprintf("early %d", i))
	}
	reader, err := os.Open(sv.LogPath("web"))
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	head := make([]byte, 20)
	if _, err := io.ReadFull(reader, head); err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 30; i++ { // several rotations under the reader's feet
		say(t, sv, s, fmt.Sprintf("later %d %s", i, strings.Repeat("z", 40)))
	}
	rest, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("reading across a rotation: %v", err)
	}
	whole := string(head) + string(rest)
	for i := 0; i < 3; i++ {
		if !strings.Contains(whole, fmt.Sprintf("early %d", i)) {
			t.Fatalf("the reader lost line %d of the file it opened: %q", i, whole)
		}
	}
}

// A deleted service takes its logs with it: nothing can read them back through
// the API afterwards, so keeping them would only be the leak this replaces.
func TestDeletingAServiceRemovesItsLogs(t *testing.T) {
	sv, stateDir := newQuietSupervisor(t, LogRotation{MaxBytes: 300, Keep: 2})
	s := define(t, sv, "web")
	define(t, sv, "keeper")
	for i := 0; i < 20; i++ {
		say(t, sv, s, fmt.Sprintf("line %d %s", i, strings.Repeat("q", 40)))
	}
	say(t, sv, sv.services["keeper"], "kept")
	if files := logDirFiles(t, sv); len(files) < 3 {
		t.Fatalf("expected rotated files to delete, got %v", files)
	}

	if err := sv.Delete("web"); err != nil {
		t.Fatal(err)
	}
	if got := logDirFiles(t, sv); !equal(got, []string{"keeper.log"}) {
		t.Fatalf("after deleting web the log dir holds %v", got)
	}
	if _, err := os.Stat(filepath.Join(stateDir, "services", "web.json")); !os.IsNotExist(err) {
		t.Fatalf("definition survived the delete: %v", err)
	}
}

// Logs of services that no longer exist (deleted by an older agent, which kept
// them for ever) go at the next cold boot.
func TestOrphanLogsAreSweptAtStart(t *testing.T) {
	dir := t.TempDir()
	sv := NewSupervisor(dir+"/state", dir+"/run")
	define(t, sv, "web")
	for _, name := range []string{"web.log", "ghost.log", "ghost.log.1", "ghost.log.7", "notalog.txt"} {
		if err := os.WriteFile(filepath.Join(sv.logsDir(), name), []byte("x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, s := range sv.List() {
		sv.Stop(s.Name, 0)
	}

	next := NewSupervisor(dir+"/state", dir+"/run") // the sprite cold-boots
	t.Cleanup(func() {
		for _, s := range next.List() {
			next.Stop(s.Name, 0)
		}
	})
	if got := logDirFiles(t, next); !equal(got, []string{"notalog.txt", "web.log"}) {
		t.Fatalf("after the sweep the log dir holds %v, want web's log and the unrelated file", got)
	}
}

// The limits are the sprite's to set, on its own disk, and nonsense in the file
// leaves the defaults standing rather than the log unbounded.
func TestLogRotationConfigFile(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		want       LogRotation
	}{
		{"absent", "", LogRotation{defaultLogMaxBytes, defaultLogKeep}},
		{"set", `{"max_bytes": 1048576, "keep": 4}`, LogRotation{1 << 20, 4}},
		{"off", `{"max_bytes": 0}`, LogRotation{0, defaultLogKeep}},
		{"no history", `{"keep": 0}`, LogRotation{defaultLogMaxBytes, 0}},
		{"absurd depth", `{"keep": 5000}`, LogRotation{defaultLogMaxBytes, maxLogKeep}},
		{"negative", `{"max_bytes": -1, "keep": -3}`, LogRotation{defaultLogMaxBytes, defaultLogKeep}},
		{"truncated", `{"max_bytes":`, LogRotation{defaultLogMaxBytes, defaultLogKeep}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if tc.body != "" {
				os.MkdirAll(dir+"/state", 0o755)
				if err := os.WriteFile(dir+"/state/logrotate.json", []byte(tc.body), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			if got := NewSupervisor(dir+"/state", dir+"/run").LogRotationSettings(); got != tc.want {
				t.Fatalf("rotation = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// Rotation off is a real opt-out: the log grows as it always did.
func TestLogRotationCanBeTurnedOff(t *testing.T) {
	sv, _ := newQuietSupervisor(t, LogRotation{MaxBytes: 0, Keep: 2})
	s := define(t, sv, "web")
	for i := 0; i < 100; i++ {
		say(t, sv, s, strings.Repeat("u", 100))
	}
	if got := logDirFiles(t, sv); !equal(got, []string{"web.log"}) {
		t.Fatalf("log files = %v, want only the live one", got)
	}
	if n := dirBytes(t, sv); n < 10000 {
		t.Fatalf("live log holds %d bytes: it was rotated after all", n)
	}
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func tailOf(s string) string {
	if len(s) > 200 {
		return s[len(s)-200:]
	}
	return s
}
