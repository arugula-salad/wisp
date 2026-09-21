package agent

import (
	"bufio"
	"encoding/hex"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// Port notifications: a client attached to an exec session is told when a
// process of that session starts or stops listening on a TCP port (the CLI
// forwards such ports to the user's machine), and /ports/watch reports every
// port in the sprite.
//
// There is no kernel event for "a socket entered LISTEN" that works without
// extra privileges or modules, so this polls /proc/net/tcp{,6}. A tick reads
// those two files; the expensive part, finding which process owns a socket
// (a walk over every /proc/<pid>/fd), happens only for a socket not seen on the
// previous tick. Nothing runs while nobody is subscribed, and a notification is
// not activity: a sprite must not stay awake just because it is being watched.
const portPollInterval = 500 * time.Millisecond

// listener is one TCP socket in LISTEN.
type listener struct {
	addr  string // the bound IP, e.g. "0.0.0.0", "127.0.0.1", "::"
	port  int
	inode uint64
}

func (l listener) key() string { return net.JoinHostPort(l.addr, strconv.Itoa(l.port)) }

type openPort struct {
	listener
	pid  int      // 0 if the owner could not be found
	sess *Session // the exec session the owner belongs to, if any
}

func (p openPort) message(typ string) map[string]any {
	// address is the bound IP, which is what the SDK feeds back into the proxy
	// as the host to dial; the API reference calls it a "proxy URL" instead.
	return map[string]any{"type": typ, "port": p.port, "address": p.addr, "pid": p.pid}
}

type portWatcher struct {
	mgr *Manager

	mu   sync.Mutex
	subs map[*client]*Session // a nil session subscribes to every port
	open map[string]openPort  // by listener.key()
	stop chan struct{}
}

// subscribe starts notifications to c: the ports already open, then changes.
// With sess set, only ports owned by that session's processes.
func (w *portWatcher) subscribe(c *client, sess *Session) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.subs) == 0 {
		if w.subs == nil {
			w.subs, w.open = map[*client]*Session{}, map[string]openPort{}
		}
		// Catch up on what changed while nobody was watching. Sockets that are
		// still there keep their owner, so this stays cheap for back-to-back execs.
		w.scanLocked()
		w.stop = make(chan struct{})
		go w.poll(w.stop)
	}
	w.subs[c] = sess

	current := []map[string]any{}
	for _, p := range w.sortedLocked() {
		if sess == nil || p.sess == sess {
			current = append(current, p.message("port_opened"))
		}
	}
	if sess == nil {
		enqueueJSON(c, map[string]any{"type": "port_list", "ports": current})
		return
	}
	for _, m := range current {
		enqueueJSON(c, m)
	}
}

func (w *portWatcher) unsubscribe(c *client) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if _, ok := w.subs[c]; !ok {
		return
	}
	delete(w.subs, c)
	if len(w.subs) == 0 {
		close(w.stop)
	}
}

func (w *portWatcher) poll(stop chan struct{}) {
	tick := time.NewTicker(portPollInterval)
	defer tick.Stop()
	for {
		select {
		case <-stop:
			return
		case <-tick.C:
		}
		w.mu.Lock()
		select {
		case <-stop: // the last subscriber left while we waited for the lock
		default:
			w.scanLocked()
		}
		w.mu.Unlock()
	}
}

func (w *portWatcher) sortedLocked() []openPort {
	out := make([]openPort, 0, len(w.open))
	for _, p := range w.open {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].port < out[j].port || out[i].port == out[j].port && out[i].addr < out[j].addr
	})
	return out
}

// scanLocked diffs the listening sockets against the last scan and notifies subscribers.
func (w *portWatcher) scanLocked() {
	now := readListeners()
	for key, p := range w.open {
		// A different inode on the same address is a new socket: the old one closed.
		if l, ok := now[key]; !ok || l.inode != p.inode {
			delete(w.open, key)
			w.notifyLocked("port_closed", p)
		}
	}
	fresh := map[uint64]bool{}
	for key, l := range now {
		if _, ok := w.open[key]; !ok {
			fresh[l.inode] = true
		}
	}
	if len(fresh) == 0 {
		return
	}
	owners := socketOwners(fresh)
	for key, l := range now {
		if !fresh[l.inode] {
			continue
		}
		p := openPort{listener: l, pid: owners[l.inode]}
		if p.pid != 0 {
			p.sess = w.mgr.sessionOf(p.pid)
		}
		w.open[key] = p
		w.notifyLocked("port_opened", p)
	}
}

func (w *portWatcher) notifyLocked(typ string, p openPort) {
	for c, sess := range w.subs {
		if sess == nil || sess == p.sess {
			enqueueJSON(c, p.message(typ))
		}
	}
}

func enqueueJSON(c *client, v any) {
	b, _ := json.Marshal(v)
	c.enqueue(wsMsg{typ: websocket.TextMessage, data: b})
}

// readListeners returns the TCP sockets in LISTEN, keyed by address.
func readListeners() map[string]listener {
	out := map[string]listener{}
	for _, file := range []string{"/proc/net/tcp", "/proc/net/tcp6"} {
		f, err := os.Open(file)
		if err != nil {
			continue
		}
		sc := bufio.NewScanner(f)
		sc.Scan() // header
		for sc.Scan() {
			// sl local_address rem_address st tx:rx tr:when retrnsmt uid timeout inode ...
			fields := strings.Fields(sc.Text())
			if len(fields) < 10 || fields[3] != "0A" {
				continue
			}
			ip, port, ok := parseProcAddr(fields[1])
			inode, err := strconv.ParseUint(fields[9], 10, 64)
			if !ok || err != nil {
				continue
			}
			l := listener{addr: ip.String(), port: port, inode: inode}
			// With SO_REUSEPORT several sockets share an address; one entry stands for them.
			if _, dup := out[l.key()]; !dup {
				out[l.key()] = l
			}
		}
		f.Close()
	}
	return out
}

// parseProcAddr decodes "0100007F:1F90": the address is hex in host byte
// order, one 32-bit word for IPv4 and four for IPv6.
func parseProcAddr(s string) (net.IP, int, bool) {
	host, portHex, ok := strings.Cut(s, ":")
	raw, err := hex.DecodeString(host)
	port, perr := strconv.ParseUint(portHex, 16, 16)
	if !ok || err != nil || perr != nil || len(raw)%4 != 0 {
		return nil, 0, false
	}
	for i := 0; i < len(raw); i += 4 { // little-endian words, on every arch we run on
		raw[i], raw[i+1], raw[i+2], raw[i+3] = raw[i+3], raw[i+2], raw[i+1], raw[i]
	}
	return net.IP(raw), int(port), true
}

// socketOwners finds, for each wanted socket inode, the lowest pid holding it
// open: the parent, when a server has forked workers that share its listener.
func socketOwners(want map[uint64]bool) map[uint64]int {
	owners := map[uint64]int{}
	entries, _ := os.ReadDir("/proc")
	pids := []int{}
	for _, e := range entries {
		if pid, err := strconv.Atoi(e.Name()); err == nil {
			pids = append(pids, pid)
		}
	}
	sort.Ints(pids)
	for _, pid := range pids {
		if len(owners) == len(want) {
			break
		}
		dir := "/proc/" + strconv.Itoa(pid) + "/fd"
		fds, _ := os.ReadDir(dir)
		for _, fd := range fds {
			link, err := os.Readlink(dir + "/" + fd.Name())
			if err != nil || !strings.HasPrefix(link, "socket:[") {
				continue
			}
			inode, _ := strconv.ParseUint(link[len("socket:["):len(link)-1], 10, 64)
			if _, found := owners[inode]; want[inode] && !found {
				owners[inode] = pid
			}
		}
	}
	return owners
}

// sessionOf finds the exec session a process belongs to: the one whose command
// leads its process group or session, or is an ancestor. A daemon that both
// double-forks and calls setsid escapes this, and is reported only by /ports/watch.
func (m *Manager) sessionOf(pid int) *Session {
	m.mu.Lock()
	roots := map[int]*Session{}
	for _, s := range m.sessions {
		if s.cmd.Process != nil {
			roots[s.cmd.Process.Pid] = s
		}
	}
	m.mu.Unlock()
	for depth := 0; pid > 1 && depth < 64; depth++ {
		ppid, pgrp, sid, ok := procStat(pid)
		if !ok {
			return nil
		}
		for _, id := range []int{pid, pgrp, sid} {
			if s := roots[id]; s != nil {
				if exited, _ := s.Exited(); !exited {
					return s
				}
			}
		}
		pid = ppid
	}
	return nil
}

// procStat reads a process's parent, process group and session from /proc/<pid>/stat.
func procStat(pid int) (ppid, pgrp, sid int, ok bool) {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return 0, 0, 0, false
	}
	// "pid (comm) state ppid pgrp session ...", and comm may itself contain ") ".
	i := strings.LastIndexByte(string(b), ')')
	fields := strings.Fields(string(b[i+1:]))
	if i < 0 || len(fields) < 4 {
		return 0, 0, 0, false
	}
	ppid, _ = strconv.Atoi(fields[1])
	pgrp, _ = strconv.Atoi(fields[2])
	sid, _ = strconv.Atoi(fields[3])
	return ppid, pgrp, sid, true
}

// handlePortsWatch streams a port_list snapshot and then every port_opened / port_closed.
func (s *Server) handlePortsWatch(w http.ResponseWriter, r *http.Request) {
	ws, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer ws.Close()
	c := newClient()
	defer c.shutdown()
	s.Sessions.ports.subscribe(c, nil)
	defer s.Sessions.ports.unsubscribe(c)
	go func() { // the client sends nothing; reading is how we learn it left
		defer c.shutdown()
		for {
			if _, _, err := ws.ReadMessage(); err != nil {
				return
			}
		}
	}()
	c.drain(func(typ int, data []byte) error {
		ws.SetWriteDeadline(time.Now().Add(30 * time.Second))
		return ws.WriteMessage(typ, data)
	})
}
