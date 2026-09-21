// Package agent implements the in-guest runtime: exec sessions, and the
// HTTP/WebSocket server spritesd proxies to over vsock.
package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/user"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/creack/pty"
	"github.com/gorilla/websocket"
	"golang.org/x/sys/unix"
)

// Stream IDs for the non-TTY binary framing (first byte of each message).
const (
	StreamStdin    byte = 0
	StreamStdout   byte = 1
	StreamStderr   byte = 2
	StreamExit     byte = 3
	StreamStdinEOF byte = 4
)

const (
	replayBufferBytes  = 1 << 20 // output retained per session for late attach
	clientHighWater    = 4 << 20 // pause reading process output while a client is this far behind
	exitedSessionTTL   = 30 * time.Second
	outputDrainTimeout = 500 * time.Millisecond
)

// SessionOpts describes a new exec session.
type SessionOpts struct {
	Cmd        []string
	Path       string
	Env        []string
	Dir        string
	TTY        bool
	Stdin      bool
	Cols, Rows uint16
	// MaxRunAfterDisconnect: 0 means keep running forever.
	MaxRunAfterDisconnect time.Duration
}

type frame struct {
	stream byte
	data   []byte
	end    int64 // cumulative output payload bytes through this frame
}

type wsMsg struct {
	typ   int
	data  []byte
	close bool
}

// client is one attached WebSocket. Its queue never blocks the producer;
// backpressure is applied in the session's output reader instead.
type client struct {
	mu     sync.Mutex
	q      []wsMsg
	qBytes int
	notify chan struct{}
	closed chan struct{}
	once   sync.Once
}

func newClient() *client {
	return &client{notify: make(chan struct{}, 1), closed: make(chan struct{})}
}

func (c *client) enqueue(m wsMsg) {
	c.mu.Lock()
	c.q = append(c.q, m)
	c.qBytes += len(m.data)
	c.mu.Unlock()
	select {
	case c.notify <- struct{}{}:
	default:
	}
}

func (c *client) backlog() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.qBytes
}

func (c *client) shutdown() { c.once.Do(func() { close(c.closed) }) }

// Session is a running (or recently exited) command.
type Session struct {
	ID      string
	Command string
	Workdir string
	Created time.Time
	TTY     bool

	mgr  *Manager
	opts SessionOpts

	mu           sync.Mutex
	cmd          *exec.Cmd
	ptmx         *os.File
	stdin        io.WriteCloser
	cols, rows   uint16
	frames       []frame
	bufBytes     int
	total        int64
	clients      map[*client]struct{}
	exited       bool
	exitCode     int
	lastActivity time.Time
	killTimer    *time.Timer
	rateBytes    int64
	rateStart    time.Time
	done         chan struct{}
}

// Manager owns all sessions in the guest.
type Manager struct {
	mu           sync.Mutex
	sessions     map[string]*Session
	nextID       int
	lastActivity time.Time
}

func NewManager() *Manager {
	return &Manager{sessions: map[string]*Session{}, lastActivity: time.Now()}
}

func (m *Manager) touch() {
	m.mu.Lock()
	m.lastActivity = time.Now()
	m.mu.Unlock()
}

// Activity reports when anything last happened and how many sessions have a client attached.
func (m *Manager) Activity() (last time.Time, attached int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, s := range m.sessions {
		s.mu.Lock()
		if !s.exited && len(s.clients) > 0 {
			attached++
		}
		s.mu.Unlock()
	}
	return m.lastActivity, attached
}

func (m *Manager) Get(id string) *Session {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.sessions[id]
}

// SessionInfo is the List Exec Sessions element shape.
type SessionInfo struct {
	ID             string     `json:"id"`
	Command        string     `json:"command"`
	Workdir        string     `json:"workdir"`
	Created        time.Time  `json:"created"`
	BytesPerSecond float64    `json:"bytes_per_second"`
	IsActive       bool       `json:"is_active"`
	LastActivity   *time.Time `json:"last_activity,omitempty"`
	TTY            bool       `json:"tty"`
}

func (m *Manager) List() []SessionInfo {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := []SessionInfo{}
	for _, s := range m.sessions {
		s.mu.Lock()
		if !s.exited {
			la := s.lastActivity
			var rate float64
			if el := time.Since(s.rateStart).Seconds(); el > 0 {
				rate = float64(s.rateBytes) / el
			}
			out = append(out, SessionInfo{
				ID: s.ID, Command: s.Command, Workdir: s.Workdir, Created: s.Created,
				BytesPerSecond: rate, IsActive: time.Since(la) < 5*time.Second,
				LastActivity: &la, TTY: s.TTY,
			})
		}
		s.mu.Unlock()
	}
	return out
}

// defaultUser resolves the unprivileged account commands run as, if the image has one.
func defaultUser() (cred *syscall.Credential, home, name string) {
	u, err := user.Lookup("sprite")
	if err != nil || os.Getuid() != 0 {
		h, _ := os.UserHomeDir()
		if h == "" {
			h = "/"
		}
		return nil, h, os.Getenv("USER")
	}
	uid, _ := strconv.Atoi(u.Uid)
	gid, _ := strconv.Atoi(u.Gid)
	cred = &syscall.Credential{Uid: uint32(uid), Gid: uint32(gid)}
	if gids, err := u.GroupIds(); err == nil {
		for _, g := range gids {
			if n, err := strconv.Atoi(g); err == nil {
				cred.Groups = append(cred.Groups, uint32(n))
			}
		}
	}
	return cred, u.HomeDir, u.Username
}

// Start launches a command and registers the session.
func (m *Manager) Start(o SessionOpts) (*Session, error) {
	if len(o.Cmd) == 0 {
		o.Cmd = []string{"bash"}
	}
	cred, home, uname := defaultUser()
	dir := o.Dir
	if dir == "" {
		dir = home
	}
	env := []string{
		"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
		"HOME=" + home, "USER=" + uname, "LOGNAME=" + uname, "LANG=C.UTF-8", "SHELL=/bin/bash",
	}
	if o.TTY {
		env = append(env, "TERM=xterm-256color")
	}
	env = append(env, o.Env...)

	path := o.Path
	if path == "" {
		path = o.Cmd[0]
	}
	if !strings.Contains(path, "/") {
		lp, err := exec.LookPath(path)
		if err != nil {
			return nil, fmt.Errorf("executable %q not found in PATH", path)
		}
		path = lp
	}
	cmd := &exec.Cmd{Path: path, Args: o.Cmd, Env: env, Dir: dir}
	cmd.SysProcAttr = &syscall.SysProcAttr{Credential: cred}

	now := time.Now()
	s := &Session{
		Command: strings.Join(o.Cmd, " "), Workdir: dir, Created: now, TTY: o.TTY,
		mgr: m, opts: o, cmd: cmd, cols: o.Cols, rows: o.Rows,
		clients: map[*client]struct{}{}, lastActivity: now, rateStart: now,
		done: make(chan struct{}),
	}

	var readers sync.WaitGroup
	if o.TTY {
		if s.cols == 0 {
			s.cols = 80
		}
		if s.rows == 0 {
			s.rows = 24
		}
		ptmx, err := pty.StartWithAttrs(cmd, &pty.Winsize{Cols: s.cols, Rows: s.rows}, &syscall.SysProcAttr{
			Credential: cred, Setsid: true, Setctty: true,
		})
		if err != nil {
			return nil, err
		}
		s.ptmx = ptmx
		readers.Add(1)
		go s.pump(ptmx, StreamStdout, &readers)
	} else {
		cmd.SysProcAttr.Setpgid = true
		outR, outW, err := os.Pipe()
		if err != nil {
			return nil, err
		}
		errR, errW, err := os.Pipe()
		if err != nil {
			return nil, err
		}
		cmd.Stdout, cmd.Stderr = outW, errW
		var inR *os.File
		if o.Stdin {
			var inW *os.File
			if inR, inW, err = os.Pipe(); err != nil {
				return nil, err
			}
			cmd.Stdin = inR
			s.stdin = inW
		}
		err = cmd.Start()
		outW.Close()
		errW.Close()
		if inR != nil {
			inR.Close()
		}
		if err != nil {
			outR.Close()
			errR.Close()
			return nil, err
		}
		readers.Add(2)
		go s.pump(outR, StreamStdout, &readers)
		go s.pump(errR, StreamStderr, &readers)
	}

	m.mu.Lock()
	m.nextID++
	s.ID = strconv.Itoa(m.nextID)
	m.sessions[s.ID] = s
	m.lastActivity = now
	m.mu.Unlock()

	go s.wait(&readers)
	return s, nil
}

// pump copies process output into the replay buffer and to attached clients.
func (s *Session) pump(r *os.File, stream byte, wg *sync.WaitGroup) {
	defer wg.Done()
	defer r.Close()
	buf := make([]byte, 32*1024)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			data := append([]byte(nil), buf[:n]...)
			s.broadcast(stream, data)
			s.throttle()
		}
		if err != nil {
			return
		}
	}
}

func (s *Session) outMsg(stream byte, data []byte) wsMsg {
	if s.TTY {
		return wsMsg{typ: websocket.BinaryMessage, data: data}
	}
	return wsMsg{typ: websocket.BinaryMessage, data: append([]byte{stream}, data...)}
}

func (s *Session) broadcast(stream byte, data []byte) {
	s.mu.Lock()
	s.total += int64(len(data))
	s.frames = append(s.frames, frame{stream: stream, data: data, end: s.total})
	s.bufBytes += len(data)
	for s.bufBytes > replayBufferBytes && len(s.frames) > 1 {
		s.bufBytes -= len(s.frames[0].data)
		s.frames = s.frames[1:]
	}
	s.lastActivity = time.Now()
	s.rateBytes += int64(len(data))
	msg := s.outMsg(stream, data)
	for c := range s.clients {
		c.enqueue(msg)
	}
	s.mu.Unlock()
	s.mgr.touch()
}

// throttle blocks the output reader while any attached client is far behind.
func (s *Session) throttle() {
	for {
		slow := false
		s.mu.Lock()
		for c := range s.clients {
			if c.backlog() > clientHighWater {
				slow = true
			}
		}
		s.mu.Unlock()
		if !slow {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func (s *Session) wait(readers *sync.WaitGroup) {
	err := s.cmd.Wait()
	code := 0
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			code = 128 + int(ws.Signal())
		} else {
			code = ee.ExitCode()
		}
	} else if err != nil {
		code = 1
	}

	// Let output drain, but don't hang on a grandchild that kept the pipe open.
	drained := make(chan struct{})
	go func() { readers.Wait(); close(drained) }()
	select {
	case <-drained:
	case <-time.After(outputDrainTimeout):
	}
	if s.ptmx != nil {
		s.ptmx.Close()
	}

	s.mu.Lock()
	s.exited, s.exitCode = true, code
	if s.killTimer != nil {
		s.killTimer.Stop()
	}
	for c := range s.clients {
		for _, m := range s.exitMsgs() {
			c.enqueue(m)
		}
	}
	s.mu.Unlock()
	close(s.done)
	s.mgr.touch()

	time.AfterFunc(exitedSessionTTL, func() {
		s.mgr.mu.Lock()
		delete(s.mgr.sessions, s.ID)
		s.mgr.mu.Unlock()
	})
}

// exitMsgs announces process exit. The JSON message is what the official CLI
// keys "clean exit" off in both modes; non-TTY streams also get the binary
// exit frame, after it, for clients that only speak the stream framing.
func (s *Session) exitMsgs() []wsMsg {
	b, _ := json.Marshal(map[string]any{"type": "exit", "exit_code": s.exitCode})
	msgs := []wsMsg{{typ: websocket.TextMessage, data: b}}
	if !s.TTY {
		msgs = append(msgs, wsMsg{typ: websocket.BinaryMessage, data: []byte{StreamExit, byte(s.exitCode)}})
	}
	return append(msgs, wsMsg{close: true})
}

// attach registers a client, replaying buffered output past offset first so
// there is no gap or duplicate between replay and live output.
func (s *Session) attach(c *client, offset int64, isOwner bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	info, _ := json.Marshal(map[string]any{
		"type": "session_info", "session_id": s.ID, "command": s.Command,
		"created": s.Created.Unix(), "cols": s.cols, "rows": s.rows,
		"is_owner": isOwner, "tty": s.TTY,
	})
	c.enqueue(wsMsg{typ: websocket.TextMessage, data: info})
	for _, f := range s.frames {
		if f.end <= offset {
			continue
		}
		data := f.data
		if start := f.end - int64(len(f.data)); offset > start {
			data = data[offset-start:]
		}
		c.enqueue(s.outMsg(f.stream, data))
	}
	if s.exited {
		for _, m := range s.exitMsgs() {
			c.enqueue(m)
		}
		return
	}
	s.clients[c] = struct{}{}
	if s.killTimer != nil {
		s.killTimer.Stop()
		s.killTimer = nil
	}
}

func (s *Session) detach(c *client) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.clients, c)
	if len(s.clients) == 0 && !s.exited && s.opts.MaxRunAfterDisconnect > 0 {
		s.killTimer = time.AfterFunc(s.opts.MaxRunAfterDisconnect, func() { s.Signal(unix.SIGKILL) })
	}
}

func (s *Session) WriteStdin(p []byte) {
	s.mu.Lock()
	var w io.Writer
	if s.TTY {
		w = s.ptmx
	} else if s.stdin != nil {
		w = s.stdin
	}
	s.lastActivity = time.Now()
	s.mu.Unlock()
	s.mgr.touch()
	if w != nil {
		w.Write(p)
	}
}

func (s *Session) CloseStdin() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stdin != nil {
		s.stdin.Close()
		s.stdin = nil
	}
}

func (s *Session) Resize(cols, rows uint16) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ptmx == nil || cols == 0 || rows == 0 {
		return
	}
	s.cols, s.rows = cols, rows
	pty.Setsize(s.ptmx, &pty.Winsize{Cols: cols, Rows: rows})
}

// Signal delivers sig to the session's whole process group.
func (s *Session) Signal(sig syscall.Signal) error {
	if s.cmd.Process == nil {
		return errors.New("not started")
	}
	// Both the pty (setsid) and pipe (setpgid) paths make the child a group leader.
	return syscall.Kill(-s.cmd.Process.Pid, sig)
}

func (s *Session) Exited() (bool, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.exited, s.exitCode
}

func (s *Session) Done() <-chan struct{} { return s.done }

// ParseSignal accepts "TERM", "SIGTERM" or a number.
func ParseSignal(name string) (syscall.Signal, error) {
	if name == "" {
		return unix.SIGTERM, nil
	}
	if n, err := strconv.Atoi(name); err == nil {
		return syscall.Signal(n), nil
	}
	name = strings.ToUpper(name)
	if !strings.HasPrefix(name, "SIG") {
		name = "SIG" + name
	}
	if sig := unix.SignalNum(name); sig != 0 {
		return sig, nil
	}
	return 0, fmt.Errorf("unknown signal %q", name)
}
