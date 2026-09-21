package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/gorilla/websocket"
)

// Filesystem watch: a WebSocket on which the client subscribes to paths and
// receives inotify events as JSON. inotify watches directories one at a time,
// so a recursive subscription is a watch per directory, extended as the tree grows.
//
// A watch is an open client connection, and like any other it keeps the sprite
// awake (suspending would drop it anyway). The events themselves are not
// activity: they only say that something in the guest, which is accounted for
// on its own terms, touched the disk.

// The kernel allows root some 15k inotify watches in a 2 GB guest; leave most
// of that for the sprite's own tools. (A variable so that tests can lower it.)
var maxWatchDirs int64 = 8192

const (
	maxWatchConns = 32
	// watchQueue bounds what a slow reader can make us hold. Past it events are
	// dropped and the client is told how many, as inotify itself would do.
	watchQueue     = 1024
	watchPing      = 30 * time.Second
	watchReadLimit = 1 << 20
)

var watchDirsInUse, watchConnsInUse atomic.Int64

// watchMsg is upstream's WatchMessage, used in both directions.
type watchMsg struct {
	Type       string   `json:"type"` // client: "subscribe"; server: "subscribed", "event", "error"
	Paths      []string `json:"paths,omitempty"`
	Recursive  bool     `json:"recursive,omitempty"`
	WorkingDir string   `json:"workingDir,omitempty"`
	Path       string   `json:"path,omitempty"`
	Event      string   `json:"event,omitempty"` // "create", "write", "remove", "rename" or "chmod"
	Timestamp  string   `json:"timestamp,omitempty"`
	Size       int64    `json:"size,omitempty"`
	IsDir      bool     `json:"isDir,omitempty"`
	Message    string   `json:"message,omitempty"`
}

type fsWatcher struct {
	w   *fsnotify.Watcher
	out chan watchMsg

	mu        sync.Mutex
	dirs      map[string]watchedDir
	files     map[string]bool // files subscribed to by name
	dropped   int
	limitSent bool

	lastRename string // pump goroutine only; see handle
}

type watchedDir struct {
	recursive bool
	// all is false for a directory watched only for the sake of subscribed
	// files inside it: its other children are not reported.
	all bool
}

func (s *Server) fsWatch(w http.ResponseWriter, r *http.Request) {
	if !websocket.IsWebSocketUpgrade(r) {
		writeErr(w, http.StatusBadRequest, "bad_request", "websocket upgrade required")
		return
	}
	if watchConnsInUse.Add(1) > maxWatchConns {
		watchConnsInUse.Add(-1)
		writeErr(w, http.StatusTooManyRequests, "too_many_watchers", fmt.Sprintf("at most %d concurrent filesystem watches", maxWatchConns))
		return
	}
	defer watchConnsInUse.Add(-1)
	nw, err := fsnotify.NewWatcher()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "io_error", err.Error())
		return
	}
	fw := &fsWatcher{w: nw, out: make(chan watchMsg, watchQueue), dirs: map[string]watchedDir{}, files: map[string]bool{}}
	defer fw.close()
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()

	done := make(chan struct{})
	defer close(done)
	go fw.pump(done)
	go fw.write(conn, done)

	// A client that vanished without closing would otherwise pin the sprite
	// awake forever; pings (sent by write) must keep being answered.
	conn.SetReadLimit(watchReadLimit)
	conn.SetReadDeadline(time.Now().Add(3 * watchPing))
	conn.SetPongHandler(func(string) error { return conn.SetReadDeadline(time.Now().Add(3 * watchPing)) })
	for {
		_, data, err := conn.ReadMessage()
		if err != nil {
			return
		}
		conn.SetReadDeadline(time.Now().Add(3 * watchPing))
		var m watchMsg
		if json.Unmarshal(data, &m) != nil || m.Type != "subscribe" {
			fw.send(watchMsg{Type: "error", Message: `expected {"type":"subscribe","paths":[...]}`})
			continue
		}
		fw.subscribe(m)
	}
}

func (fw *fsWatcher) close() {
	fw.w.Close()
	fw.mu.Lock()
	watchDirsInUse.Add(-int64(len(fw.dirs)))
	fw.dirs = map[string]watchedDir{}
	fw.mu.Unlock()
}

// send never blocks: the inotify reader must keep draining whatever the client does.
func (fw *fsWatcher) send(m watchMsg) {
	select {
	case fw.out <- m:
	default:
		fw.mu.Lock()
		fw.dropped++
		fw.mu.Unlock()
	}
}

// overflow is the notice owed to a client that fell behind, once it has caught up.
func (fw *fsWatcher) overflow() (watchMsg, bool) {
	fw.mu.Lock()
	defer fw.mu.Unlock()
	if fw.dropped == 0 || len(fw.out) > 0 {
		return watchMsg{}, false
	}
	m := watchMsg{Type: "error", Message: fmt.Sprintf("client too slow: %d events dropped", fw.dropped)}
	fw.dropped = 0
	return m, true
}

func (fw *fsWatcher) write(conn *websocket.Conn, done <-chan struct{}) {
	ping := time.NewTicker(watchPing)
	defer ping.Stop()
	defer conn.Close() // unblocks the reader, which owns the cleanup
	for {
		var err error
		select {
		case <-done:
			return
		case m := <-fw.out:
			conn.SetWriteDeadline(time.Now().Add(watchPing))
			err = conn.WriteJSON(m)
			if m, ok := fw.overflow(); ok && err == nil {
				err = conn.WriteJSON(m)
			}
		case <-ping.C:
			err = conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(watchPing))
		}
		if err != nil {
			return
		}
	}
}

func (fw *fsWatcher) subscribe(m watchMsg) {
	if len(m.Paths) == 0 {
		fw.send(watchMsg{Type: "error", Message: "paths is required"})
		return
	}
	var ok []string
	for _, p := range m.Paths {
		path, err := resolvePath(p, m.WorkingDir)
		var info fs.FileInfo
		if err == nil {
			// Walking does not follow symlinks, so start from what the path really is.
			var real string
			if real, err = filepath.EvalSymlinks(path); err == nil {
				path = real
				info, err = os.Stat(path)
			}
		}
		switch {
		case err != nil:
		case info.IsDir():
			err = fw.addTree(path, m.Recursive, false)
		default:
			// Watch the parent, not the inode: editors replace files by rename,
			// which would silently end a watch on the file itself.
			fw.mu.Lock()
			fw.files[path] = true
			fw.mu.Unlock()
			_, err = fw.addDir(filepath.Dir(path), watchedDir{})
		}
		if err != nil {
			fw.send(watchMsg{Type: "error", Path: path, Message: watchErrText(err)})
			continue
		}
		ok = append(ok, path)
	}
	fw.send(watchMsg{Type: "subscribed", Paths: ok})
}

func watchErrText(err error) string {
	var pe *fs.PathError
	if errors.As(err, &pe) {
		return pe.Err.Error()
	}
	return err.Error()
}

var errWatchLimit = errors.New("watch limit reached (directories, across all watchers); not watching")

// addDir watches one directory, or widens what an existing watch reports.
func (fw *fsWatcher) addDir(dir string, want watchedDir) (added bool, err error) {
	fw.mu.Lock()
	defer fw.mu.Unlock()
	if have, known := fw.dirs[dir]; known {
		fw.dirs[dir] = watchedDir{recursive: have.recursive || want.recursive, all: have.all || want.all}
		return false, nil
	}
	if watchDirsInUse.Add(1) > maxWatchDirs {
		watchDirsInUse.Add(-1)
		return false, errWatchLimit
	}
	if err := fw.w.Add(dir); err != nil {
		watchDirsInUse.Add(-1)
		return false, err
	}
	fw.dirs[dir] = want
	return true, nil
}

// addTree watches dir and, if recursive, everything under it. With announce it
// also reports what it finds as created: that is for a directory that appeared
// in a watched tree, whose first contents can beat the watch being placed.
//
// It is all or nothing: a tree too big for the watch budget is not left half
// watched, holding the budget and reporting an arbitrary part of itself.
func (fw *fsWatcher) addTree(dir string, recursive, announce bool) error {
	if !recursive {
		_, err := fw.addDir(dir, watchedDir{all: true})
		return err
	}
	var added []string
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if p == dir {
				return err
			}
			return nil // vanished or unreadable while walking: nothing to watch
		}
		if announce && p != dir {
			fw.event(p, "create")
		}
		if !d.IsDir() {
			return nil
		}
		ok, err := fw.addDir(p, watchedDir{recursive: true, all: true})
		if ok {
			added = append(added, p)
		}
		return err
	})
	if err != nil {
		fw.mu.Lock()
		for _, d := range added {
			fw.w.Remove(d)
			delete(fw.dirs, d)
		}
		watchDirsInUse.Add(-int64(len(added)))
		fw.mu.Unlock()
	}
	return err
}

// forget drops bookkeeping for a directory that was removed or renamed away,
// and everything under it. inotify watches follow the inode, so after a rename
// the descendants' watches would report paths that no longer exist.
func (fw *fsWatcher) forget(dir string) {
	fw.mu.Lock()
	defer fw.mu.Unlock()
	for d := range fw.dirs {
		if d == dir || strings.HasPrefix(d, dir+"/") {
			fw.w.Remove(d) // already gone if the directory was deleted
			delete(fw.dirs, d)
			watchDirsInUse.Add(-1)
		}
	}
}

func (fw *fsWatcher) pump(done <-chan struct{}) {
	for {
		select {
		case <-done:
			return
		case err, ok := <-fw.w.Errors:
			if !ok {
				return
			}
			fw.send(watchMsg{Type: "error", Message: err.Error()})
		case ev, ok := <-fw.w.Events:
			if !ok {
				return
			}
			fw.handle(ev)
		}
	}
}

func (fw *fsWatcher) handle(ev fsnotify.Event) {
	fw.mu.Lock()
	self, wasDir := fw.dirs[ev.Name]
	parent := fw.dirs[filepath.Dir(ev.Name)]
	wanted := parent.all || self.all || fw.files[ev.Name]
	fw.mu.Unlock()

	if wasDir && (ev.Has(fsnotify.Remove) || ev.Has(fsnotify.Rename)) {
		fw.forget(ev.Name)
	}
	if !wanted {
		return
	}
	// A watched directory that moves is reported twice, by its parent's watch and
	// by its own. Nothing can rename the same path twice in a row without a
	// create in between, so an immediate repeat is that echo.
	if ev.Op == fsnotify.Rename && ev.Name == fw.lastRename {
		fw.lastRename = ""
		return
	}
	fw.lastRename = ""
	if ev.Has(fsnotify.Rename) {
		fw.lastRename = ev.Name
	}
	for _, op := range []fsnotify.Op{fsnotify.Create, fsnotify.Write, fsnotify.Remove, fsnotify.Rename, fsnotify.Chmod} {
		if ev.Has(op) {
			fw.event(ev.Name, strings.ToLower(op.String()))
		}
	}
	if parent.recursive && ev.Has(fsnotify.Create) {
		if info, err := os.Lstat(ev.Name); err == nil && info.IsDir() {
			if err := fw.addTree(ev.Name, true, true); err != nil {
				fw.limitOnce(ev.Name, err)
			}
		}
	}
}

// limitOnce reports a failure to extend a recursive watch, but says "limit
// reached" once rather than for every directory of an unpacking tarball.
func (fw *fsWatcher) limitOnce(path string, err error) {
	if errors.Is(err, errWatchLimit) {
		fw.mu.Lock()
		sent := fw.limitSent
		fw.limitSent = true
		fw.mu.Unlock()
		if sent {
			return
		}
	}
	fw.send(watchMsg{Type: "error", Path: path, Message: watchErrText(err)})
}

func (fw *fsWatcher) event(path, op string) {
	m := watchMsg{Type: "event", Path: path, Event: op, Timestamp: time.Now().UTC().Format(time.RFC3339Nano)}
	if info, err := os.Lstat(path); err == nil {
		m.Size, m.IsDir = info.Size(), info.IsDir()
		if m.IsDir {
			m.Size = 0
		}
	}
	fw.send(m)
}
