package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Services are runtime-owned processes. Their definitions live on the sprite's
// disk, so they survive cold boots and travel with checkpoints; the supervisor
// starts them all, in dependency order, whenever the agent comes up.

var (
	ErrServiceNotFound = errors.New("service not found")
	ErrServiceConflict = errors.New("service conflict")
	serviceNameRE      = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,62}$`)
)

const (
	defaultHTTPPort    = 8080
	defaultStopTimeout = 10 * time.Second
	maxRestartBackoff  = 30 * time.Second
	stableRunTime      = 10 * time.Second // a run this long resets the crash backoff
)

// Service output is appended to <stateDir>/logs/services/<name>.log, which lives
// on the sprite's disk and therefore on the volume every sprite on the host
// shares. Nothing used to bound it: one service printing a line per request
// filled the disk, and deleting the service kept its log for ever. Rotation by
// size, with a bounded number of rotations, caps a service's output at
// MaxBytes*(Keep+1) bytes however chatty it is, without asking it to cooperate.
//
// The default is generous per file and shallow in history, because what people
// actually read is the last few minutes of a service that just misbehaved, and
// they read it through the logs API, which tails the live file.
const (
	defaultLogMaxBytes = 8 << 20 // 8 MiB live, 24 MiB per service in total
	defaultLogKeep     = 2
	maxLogKeep         = 20 // a ceiling on the configured depth: the point is to bound the disk
)

// LogRotation bounds one service's log. It is read once, when the agent starts,
// from <stateDir>/logrotate.json ({"max_bytes":8388608,"keep":2}), so a sprite
// that wants a longer window says so on its own disk and the setting travels
// with its checkpoints like the service definitions do. There is no daemon-side
// flag, because the disk the limit protects is the sprite's own.
type LogRotation struct {
	// MaxBytes rotates the live log once it has grown past this. 0 disables
	// rotation entirely, which is the explicit opt-out: the log grows unbounded again.
	MaxBytes int64 `json:"max_bytes"`
	// Keep is how many rotated files (<name>.log.1 .. .log.N, 1 newest) are kept
	// beside the live one. 0 throws the old log away at each rotation.
	Keep int `json:"keep"`
}

// logFileRE matches a service's live log and its rotations: "<name>.log[.<n>]".
var logFileRE = regexp.MustCompile(`^(.+)\.log(?:\.[0-9]+)?$`)

// loadLogRotation prefers the defaults to anything it cannot make sense of: a
// truncated or hand-edited file must not be a way to end up unbounded again.
func loadLogRotation(stateDir string) LogRotation {
	r := LogRotation{MaxBytes: defaultLogMaxBytes, Keep: defaultLogKeep}
	b, err := os.ReadFile(filepath.Join(stateDir, "logrotate.json"))
	if err != nil {
		return r
	}
	var got struct {
		MaxBytes *int64 `json:"max_bytes"`
		Keep     *int   `json:"keep"`
	}
	if json.Unmarshal(b, &got) != nil {
		return r
	}
	if got.MaxBytes != nil && *got.MaxBytes >= 0 {
		r.MaxBytes = *got.MaxBytes
	}
	if got.Keep != nil && *got.Keep >= 0 {
		r.Keep = min(*got.Keep, maxLogKeep)
	}
	return r
}

type ServiceDef struct {
	Name     string            `json:"name"`
	Cmd      string            `json:"cmd"`
	Args     []string          `json:"args"`
	Needs    []string          `json:"needs"`
	HTTPPort *int              `json:"http_port,omitempty"`
	Env      map[string]string `json:"env,omitempty"`
	Dir      string            `json:"dir,omitempty"`
}

type ServiceState struct {
	Name          string     `json:"name"`
	Status        string     `json:"status"` // stopped, starting, running, stopping, failed
	PID           int        `json:"pid,omitempty"`
	StartedAt     *time.Time `json:"started_at,omitempty"`
	Error         string     `json:"error,omitempty"`
	RestartCount  int        `json:"restart_count,omitempty"`
	NextRestartAt *time.Time `json:"next_restart_at,omitempty"`
}

type ServiceWithState struct {
	ServiceDef
	State ServiceState `json:"state"`
}

// ServiceEvent is one NDJSON line of a create/start/stop/restart/logs stream.
type ServiceEvent struct {
	Type      string            `json:"type"` // started, stdout, stderr, exit, error, stopping, stopped, complete
	Data      string            `json:"data,omitempty"`
	ExitCode  *int              `json:"exit_code,omitempty"`
	Timestamp int64             `json:"timestamp"` // unix milliseconds
	LogFiles  map[string]string `json:"log_files,omitempty"`
}

// ServiceReport is a service lifecycle change the supervisor tells spritesd
// about (see Reporter): started, crashed (exited on its own; a restart is
// scheduled), stopped (on purpose) or failed (could not be launched).
type ServiceReport struct {
	Type         string `json:"type"`
	Service      string `json:"service"`
	PID          int    `json:"pid,omitempty"`
	ExitCode     *int   `json:"exit_code,omitempty"`
	RestartCount int    `json:"restart_count,omitempty"`
	RestartInMS  int64  `json:"restart_in_ms,omitempty"`
	Error        string `json:"error,omitempty"`
}

type service struct {
	def         ServiceDef
	state       ServiceState
	cmd         *exec.Cmd
	wantRunning bool
	gen         int // bumped on every start/stop so stale restart timers and exit handlers no-op
	quickFails  int
	exited      chan struct{} // closed when the current process has been reaped
	subs        map[chan ServiceEvent]struct{}
	logFile     *os.File
	logBytes    int64 // what logFile holds, so rotation needs no stat per line
}

type Supervisor struct {
	stateDir string // definitions + logs; on the sprite's disk
	runDir   string // pid files; tmpfs, so it empties on cold boot

	// report hears of starts, crashes and stops; nil for none. It is called with
	// mu held and must not block.
	report func(ServiceReport)

	mu       sync.Mutex
	services map[string]*service
	logRot   LogRotation
}

// NewSupervisor loads definitions and starts every service in dependency order.
func NewSupervisor(stateDir, runDir string) *Supervisor {
	return NewReportingSupervisor(stateDir, runDir, nil)
}

// NewReportingSupervisor is NewSupervisor with report set before the first
// service starts, so the starts at boot are reported too.
func NewReportingSupervisor(stateDir, runDir string, report func(ServiceReport)) *Supervisor {
	sv := &Supervisor{stateDir: stateDir, runDir: runDir, services: map[string]*service{}, report: report,
		logRot: loadLogRotation(stateDir)}
	os.MkdirAll(sv.defsDir(), 0o755)
	os.MkdirAll(sv.logsDir(), 0o755)
	os.MkdirAll(runDir, 0o755)
	entries, defsErr := os.ReadDir(sv.defsDir())
	for _, e := range entries {
		b, err := os.ReadFile(filepath.Join(sv.defsDir(), e.Name()))
		if err != nil || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		var def ServiceDef
		if json.Unmarshal(b, &def) != nil || def.Name == "" {
			continue
		}
		sv.services[def.Name] = newService(def)
	}
	sv.mu.Lock()
	defer sv.mu.Unlock()
	if defsErr == nil {
		sv.sweepOrphanLogsLocked() // logs left by an older agent, which kept a deleted service's for ever
	}
	for _, name := range sv.sortedNames() {
		sv.killStale(name)
		sv.startLocked(name, map[string]bool{})
	}
	return sv
}

func newService(def ServiceDef) *service {
	return &service{def: def, state: ServiceState{Name: def.Name, Status: "stopped"}, subs: map[chan ServiceEvent]struct{}{}}
}

func (sv *Supervisor) defsDir() string         { return filepath.Join(sv.stateDir, "services") }
func (sv *Supervisor) logsDir() string         { return filepath.Join(sv.stateDir, "logs", "services") }
func (sv *Supervisor) LogPath(n string) string { return filepath.Join(sv.logsDir(), n+".log") }
func (sv *Supervisor) pidPath(n string) string { return filepath.Join(sv.runDir, n+".pid") }
func (sv *Supervisor) defPath(n string) string { return filepath.Join(sv.defsDir(), n+".json") }

// SetLogRotation replaces the limits while the agent runs. A lowered MaxBytes
// applies from the next line written, so it does not wait for a restart.
func (sv *Supervisor) SetLogRotation(r LogRotation) {
	sv.mu.Lock()
	defer sv.mu.Unlock()
	if r.Keep > maxLogKeep {
		r.Keep = maxLogKeep
	}
	sv.logRot = r
}

// LogRotationSettings reports the limits in force.
func (sv *Supervisor) LogRotationSettings() LogRotation {
	sv.mu.Lock()
	defer sv.mu.Unlock()
	return sv.logRot
}

// rotationPath is where the nth-newest rotated log lives: <name>.log.1 is the
// one just retired, .log.2 the one before it, and so on up to Keep.
func (sv *Supervisor) rotationPath(name string, n int) string {
	return sv.LogPath(name) + "." + strconv.Itoa(n)
}

// openLogLocked attaches a service to its log file, picking up a log that
// earlier runs left behind (the file is appended to across restarts, so its
// size carries over) and rotating straight away if that is already over the limit.
func (sv *Supervisor) openLogLocked(s *service) error {
	f, err := os.OpenFile(sv.LogPath(s.def.Name), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	s.logFile, s.logBytes = f, 0
	if st, err := f.Stat(); err == nil {
		s.logBytes = st.Size()
	}
	sv.rotateIfFullLocked(s)
	return nil
}

func (sv *Supervisor) rotateIfFullLocked(s *service) {
	if sv.logRot.MaxBytes > 0 && s.logBytes >= sv.logRot.MaxBytes {
		sv.rotateLocked(s)
	}
}

// rotateLocked retires the live log and starts a new one. It renames rather
// than truncates on purpose: a reader that already has the file open (the logs
// API tailing it, a shell inside the sprite) keeps reading the inode it opened,
// sees a complete file and never a half-erased one. The service's own handle is
// swapped here under sv.mu, which is the lock every writer in emitLocked holds,
// so no line is written to a file that has just been rotated away.
func (sv *Supervisor) rotateLocked(s *service) {
	name := s.def.Name
	if s.logFile != nil {
		s.logFile.Close()
		s.logFile = nil
	}
	if keep := sv.logRot.Keep; keep <= 0 {
		os.Remove(sv.LogPath(name)) // no history wanted: the old log goes
	} else {
		os.Remove(sv.rotationPath(name, keep)) // the oldest falls off the end
		for i := keep - 1; i >= 1; i-- {
			os.Rename(sv.rotationPath(name, i), sv.rotationPath(name, i+1))
		}
		os.Rename(sv.LogPath(name), sv.rotationPath(name, 1))
	}
	f, err := os.OpenFile(sv.LogPath(name), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return // logging stops until the next start; the service itself is untouched
	}
	s.logFile, s.logBytes = f, 0
}

// removeLogsLocked deletes a service's live log and every rotation of it. A
// deleted service's logs go with it: the logs API asks the supervisor for the
// service before it opens the file, so nothing can read them back afterwards,
// and keeping them is the unbounded case this rotation exists to remove. A
// reader that already has one open still finishes what it was reading.
func (sv *Supervisor) removeLogsLocked(name string) {
	for _, f := range sv.logFilesFor(name) {
		os.Remove(filepath.Join(sv.logsDir(), f))
	}
}

// logFilesFor lists the log files on disk belonging to name, whatever depth of
// rotation an earlier setting may have left behind.
func (sv *Supervisor) logFilesFor(name string) []string {
	var out []string
	entries, _ := os.ReadDir(sv.logsDir())
	for _, e := range entries {
		if m := logFileRE.FindStringSubmatch(e.Name()); m != nil && m[1] == name {
			out = append(out, e.Name())
		}
	}
	return out
}

// sweepOrphanLogsLocked drops logs that belong to no definition: a service
// deleted by an older agent, which kept its log for ever, or one whose
// definition was removed by hand. They are unreachable through the API and pure
// occupancy on the sprite's disk. It runs at start, where the caller has just
// loaded every definition and knows the read succeeded.
func (sv *Supervisor) sweepOrphanLogsLocked() {
	entries, err := os.ReadDir(sv.logsDir())
	if err != nil {
		return
	}
	for _, e := range entries {
		m := logFileRE.FindStringSubmatch(e.Name())
		if m == nil || e.IsDir() {
			continue
		}
		if _, live := sv.services[m[1]]; !live {
			os.Remove(filepath.Join(sv.logsDir(), e.Name()))
		}
	}
}

func (sv *Supervisor) sortedNames() []string {
	names := make([]string, 0, len(sv.services))
	for n := range sv.services {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// killStale cleans up a process group left by a previous agent instance (the
// agent restarted without the guest rebooting), which would otherwise hold the service's ports.
func (sv *Supervisor) killStale(name string) {
	b, err := os.ReadFile(sv.pidPath(name))
	if err != nil {
		return
	}
	os.Remove(sv.pidPath(name))
	if pid, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil && pid > 1 {
		syscall.Kill(-pid, syscall.SIGKILL)
	}
}

func (sv *Supervisor) snapshot(s *service) ServiceWithState {
	return ServiceWithState{ServiceDef: s.def, State: s.state}
}

func (sv *Supervisor) List() []ServiceWithState {
	sv.mu.Lock()
	defer sv.mu.Unlock()
	out := []ServiceWithState{}
	for _, n := range sv.sortedNames() {
		out = append(out, sv.snapshot(sv.services[n]))
	}
	return out
}

func (sv *Supervisor) Get(name string) (ServiceWithState, error) {
	sv.mu.Lock()
	defer sv.mu.Unlock()
	s, ok := sv.services[name]
	if !ok {
		return ServiceWithState{}, ErrServiceNotFound
	}
	return sv.snapshot(s), nil
}

// Define creates or replaces a definition. A replaced service is stopped; the
// caller starts it (separately, so it can subscribe to events in between).
func (sv *Supervisor) Define(def ServiceDef) error {
	if !serviceNameRE.MatchString(def.Name) {
		return fmt.Errorf("invalid service name %q", def.Name)
	}
	if def.Cmd == "" {
		return errors.New("cmd is required")
	}
	if def.Args == nil {
		def.Args = []string{}
	}
	if def.Needs == nil {
		def.Needs = []string{}
	}
	sv.mu.Lock()
	defer sv.mu.Unlock()
	for _, need := range def.Needs {
		if _, ok := sv.services[need]; !ok || need == def.Name {
			return fmt.Errorf("needs unknown service %q", need)
		}
	}
	if def.HTTPPort != nil {
		for n, o := range sv.services {
			if n != def.Name && o.def.HTTPPort != nil {
				return fmt.Errorf("%w: another service already has an HTTP port configured", ErrServiceConflict)
			}
		}
	}
	old, existed := sv.services[def.Name]
	if existed {
		prev := old.def
		old.def = def
		if sv.hasCycle(def.Name, map[string]bool{}) {
			old.def = prev
			return errors.New("needs would form a dependency cycle")
		}
		old.def = prev
	}
	b, _ := json.MarshalIndent(def, "", "  ")
	if err := os.WriteFile(sv.defPath(def.Name), b, 0o644); err != nil {
		return err
	}
	if existed {
		sv.stopLocked(old, defaultStopTimeout)
		old.def = def
		old.state.RestartCount = 0
	} else {
		sv.services[def.Name] = newService(def)
	}
	return nil
}

func (sv *Supervisor) hasCycle(name string, path map[string]bool) bool {
	if path[name] {
		return true
	}
	path[name] = true
	defer delete(path, name)
	if s, ok := sv.services[name]; ok {
		for _, need := range s.def.Needs {
			if sv.hasCycle(need, path) {
				return true
			}
		}
	}
	return false
}

func (sv *Supervisor) Delete(name string) error {
	sv.mu.Lock()
	defer sv.mu.Unlock()
	s, ok := sv.services[name]
	if !ok {
		return ErrServiceNotFound
	}
	for n, o := range sv.services {
		for _, need := range o.def.Needs {
			if need == name {
				return fmt.Errorf("%w: service %q needs it", ErrServiceConflict, n)
			}
		}
	}
	sv.stopLocked(s, defaultStopTimeout)
	if s.logFile != nil {
		s.logFile.Close()
		s.logFile, s.logBytes = nil, 0
	}
	delete(sv.services, name)
	sv.removeLogsLocked(name) // the logs go with the service; see removeLogsLocked
	return os.Remove(sv.defPath(name))
}

func (sv *Supervisor) Start(name string) error {
	sv.mu.Lock()
	defer sv.mu.Unlock()
	if _, ok := sv.services[name]; !ok {
		return ErrServiceNotFound
	}
	sv.startLocked(name, map[string]bool{})
	return nil
}

// startLocked starts name after everything it needs. It is a no-op for a live
// service. A process that fails to launch is not an error here: it shows up in
// the service's state and event stream, and is retried with backoff.
func (sv *Supervisor) startLocked(name string, visiting map[string]bool) {
	s := sv.services[name]
	if s == nil || visiting[name] {
		return
	}
	visiting[name] = true
	for _, need := range s.def.Needs {
		sv.startLocked(need, visiting)
	}
	if s.cmd != nil {
		return
	}
	s.wantRunning = true
	s.gen++
	sv.spawnLocked(s)
}

func (sv *Supervisor) spawnLocked(s *service) {
	cred, home, uname := defaultUser()
	dir := s.def.Dir
	if dir == "" {
		dir = home
	}
	env := baseEnv(home, uname)
	for k, v := range s.def.Env {
		env = append(env, k+"="+v)
	}
	path := s.def.Cmd
	var err error
	if !strings.Contains(path, "/") {
		if path, err = lookPath(path, env); err != nil {
			err = fmt.Errorf("executable %q not found in PATH", s.def.Cmd)
		}
	}
	var outR, outW, errR, errW *os.File
	if err == nil {
		outR, outW, err = os.Pipe()
	}
	if err == nil {
		errR, errW, err = os.Pipe()
	}
	if err == nil && s.logFile == nil {
		err = sv.openLogLocked(s)
	}
	var cmd *exec.Cmd
	if err == nil {
		cmd = &exec.Cmd{Path: path, Args: append([]string{s.def.Cmd}, s.def.Args...), Env: env, Dir: dir,
			Stdout: outW, Stderr: errW,
			SysProcAttr: &syscall.SysProcAttr{Credential: cred, Setpgid: true}}
		err = launch(cmd.SysProcAttr, cmd.Start)
	}
	for _, f := range []*os.File{outW, errW} {
		if f != nil {
			f.Close()
		}
	}
	s.state.NextRestartAt = nil
	if err != nil {
		for _, f := range []*os.File{outR, errR} {
			if f != nil {
				f.Close()
			}
		}
		s.state.Status, s.state.Error, s.state.PID = "failed", err.Error(), 0
		s.quickFails++
		sv.emitLocked(s, ServiceEvent{Type: "error", Data: err.Error()})
		delay := sv.scheduleRestartLocked(s)
		sv.reportLocked(ServiceReport{Type: "failed", Service: s.def.Name, Error: err.Error(),
			RestartCount: s.state.RestartCount, RestartInMS: delay.Milliseconds()})
		return
	}

	now := time.Now().UTC()
	s.cmd, s.exited = cmd, make(chan struct{})
	s.state.Status, s.state.PID, s.state.StartedAt, s.state.Error = "running", cmd.Process.Pid, &now, ""
	os.WriteFile(sv.pidPath(s.def.Name), []byte(strconv.Itoa(cmd.Process.Pid)), 0o644)
	sv.emitLocked(s, ServiceEvent{Type: "started", Data: fmt.Sprintf("pid %d", cmd.Process.Pid)})
	sv.reportLocked(ServiceReport{Type: "started", Service: s.def.Name, PID: cmd.Process.Pid, RestartCount: s.state.RestartCount})

	var readers sync.WaitGroup
	readers.Add(2)
	go sv.pumpLines(s, outR, "stdout", &readers)
	go sv.pumpLines(s, errR, "stderr", &readers)
	go sv.reap(s, cmd, s.gen, s.exited, now, &readers)
}

func (sv *Supervisor) pumpLines(s *service, r *os.File, stream string, wg *sync.WaitGroup) {
	defer wg.Done()
	defer r.Close()
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 1<<20)
	for sc.Scan() {
		sv.mu.Lock()
		sv.emitLocked(s, ServiceEvent{Type: stream, Data: sc.Text()})
		sv.mu.Unlock()
	}
}

// emitLocked fans an event out to stream subscribers and appends output to the log file.
// Service output deliberately does not count as sprite activity: services must not keep a sprite awake.
func (sv *Supervisor) emitLocked(s *service, ev ServiceEvent) {
	now := time.Now().UTC()
	ev.Timestamp = now.UnixMilli()
	if s.logFile != nil && (ev.Type == "stdout" || ev.Type == "stderr") {
		line := fmt.Sprintf("%s [%s] %s\n", now.Format("2006-01-02T15:04:05.000Z"), ev.Type, ev.Data)
		// Rotate before the line that would cross the limit, never in the middle
		// of one, and never after it: the live log is what the logs API tails, so
		// it should always hold the newest output rather than start out empty.
		if sv.logRot.MaxBytes > 0 && s.logBytes > 0 && s.logBytes+int64(len(line)) > sv.logRot.MaxBytes {
			sv.rotateLocked(s)
		}
		if s.logFile != nil { // a rotation that could not reopen leaves nothing to write to
			n, _ := fmt.Fprint(s.logFile, line)
			s.logBytes += int64(n)
		}
	}
	for ch := range s.subs {
		select {
		case ch <- ev:
		default: // a stalled stream reader loses events rather than stalling the service
		}
	}
}

func (sv *Supervisor) reap(s *service, cmd *exec.Cmd, gen int, exited chan struct{}, started time.Time, readers *sync.WaitGroup) {
	err := cmd.Wait()
	drained := make(chan struct{})
	go func() { readers.Wait(); close(drained) }()
	select {
	case <-drained:
	case <-time.After(outputDrainTimeout):
	}
	code := 0
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			code = 128 + int(ws.Signal())
		} else {
			code = ee.ExitCode()
		}
	}

	sv.mu.Lock()
	defer sv.mu.Unlock()
	defer close(exited)
	os.Remove(sv.pidPath(s.def.Name))
	sv.emitLocked(s, ServiceEvent{Type: "exit", ExitCode: &code})
	if s.cmd == cmd {
		s.cmd = nil
		s.state.PID = 0
	}
	if gen != s.gen || !s.wantRunning {
		sv.reportLocked(ServiceReport{Type: "stopped", Service: s.def.Name, ExitCode: &code})
		return // stopped or replaced on purpose
	}
	// It exited on its own: that is a crash, however clean the exit code.
	if time.Since(started) >= stableRunTime {
		s.quickFails = 0
	} else {
		s.quickFails++
	}
	s.state.Status = "failed"
	s.state.Error = fmt.Sprintf("exited with code %d", code)
	s.state.RestartCount++
	delay := sv.scheduleRestartLocked(s)
	sv.reportLocked(ServiceReport{Type: "crashed", Service: s.def.Name, ExitCode: &code,
		RestartCount: s.state.RestartCount, RestartInMS: delay.Milliseconds()})
}

func (sv *Supervisor) reportLocked(r ServiceReport) {
	if sv.report != nil {
		sv.report(r)
	}
}

// scheduleRestartLocked returns how long until the restart.
func (sv *Supervisor) scheduleRestartLocked(s *service) time.Duration {
	// A service that had been up for a while restarts immediately; one that keeps dying backs off 1s, 2s, 4s...
	var delay time.Duration
	if s.quickFails > 0 {
		delay = min(time.Second<<min(s.quickFails-1, 5), maxRestartBackoff)
	}
	at := time.Now().UTC().Add(delay)
	s.state.NextRestartAt = &at
	gen := s.gen
	time.AfterFunc(delay, func() {
		sv.mu.Lock()
		defer sv.mu.Unlock()
		if s.gen == gen && s.wantRunning && s.cmd == nil && sv.services[s.def.Name] == s {
			sv.spawnLocked(s)
		}
	})
	return delay
}

func (sv *Supervisor) Stop(name string, timeout time.Duration) error {
	sv.mu.Lock()
	defer sv.mu.Unlock()
	s, ok := sv.services[name]
	if !ok {
		return ErrServiceNotFound
	}
	sv.stopLocked(s, timeout)
	return nil
}

// stopLocked is sticky: the service stays down until started again. It
// releases the lock while waiting for the process to die.
func (sv *Supervisor) stopLocked(s *service, timeout time.Duration) {
	s.wantRunning = false
	s.gen++
	s.state.NextRestartAt = nil
	if s.cmd == nil {
		s.state.Status = "stopped"
		return
	}
	if timeout <= 0 {
		timeout = defaultStopTimeout
	}
	pid, exited := s.cmd.Process.Pid, s.exited
	s.state.Status = "stopping"
	sv.emitLocked(s, ServiceEvent{Type: "stopping", Data: "sending SIGTERM"})
	syscall.Kill(-pid, syscall.SIGTERM)
	sv.mu.Unlock()
	select {
	case <-exited:
	case <-time.After(timeout):
		syscall.Kill(-pid, syscall.SIGKILL)
		<-exited
	}
	sv.mu.Lock()
	s.state.Status, s.state.Error = "stopped", ""
	sv.emitLocked(s, ServiceEvent{Type: "stopped"})
}

func (sv *Supervisor) Restart(name string, timeout time.Duration) error {
	sv.mu.Lock()
	defer sv.mu.Unlock()
	s, ok := sv.services[name]
	if !ok {
		return ErrServiceNotFound
	}
	sv.stopLocked(s, timeout)
	sv.startLocked(name, map[string]bool{})
	return nil
}

// Signal delivers sig to a running service's process group. If that kills it, the supervisor treats it as a crash.
func (sv *Supervisor) Signal(name string, sig syscall.Signal) error {
	sv.mu.Lock()
	defer sv.mu.Unlock()
	s, ok := sv.services[name]
	if !ok {
		return ErrServiceNotFound
	}
	if s.cmd == nil {
		return fmt.Errorf("%w: service is not running", ErrServiceConflict)
	}
	return syscall.Kill(-s.cmd.Process.Pid, sig)
}

// Subscribe returns a channel of the service's events until cancel is called.
func (sv *Supervisor) Subscribe(name string) (<-chan ServiceEvent, func(), error) {
	sv.mu.Lock()
	defer sv.mu.Unlock()
	s, ok := sv.services[name]
	if !ok {
		return nil, nil, ErrServiceNotFound
	}
	ch := make(chan ServiceEvent, 1024)
	s.subs[ch] = struct{}{}
	return ch, func() {
		sv.mu.Lock()
		delete(s.subs, ch)
		sv.mu.Unlock()
	}, nil
}

// HTTPTarget is the guest port a sprite's URL routes to. If a service owns the
// HTTP port it is started on demand and given a moment to begin listening.
func (sv *Supervisor) HTTPTarget(ctx context.Context) int {
	sv.mu.Lock()
	var owner string
	port := defaultHTTPPort
	for n, s := range sv.services {
		if s.def.HTTPPort != nil {
			owner, port = n, *s.def.HTTPPort
		}
	}
	if owner != "" {
		sv.startLocked(owner, map[string]bool{})
	}
	sv.mu.Unlock()
	if owner == "" {
		return port
	}
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) && ctx.Err() == nil {
		if c, err := net.DialTimeout("tcp", net.JoinHostPort("localhost", strconv.Itoa(port)), time.Second); err == nil {
			c.Close()
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	return port
}
