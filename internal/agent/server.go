package agent

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/gorilla/websocket"
	"golang.org/x/sys/unix"
)

var upgrader = websocket.Upgrader{
	ReadBufferSize:  64 * 1024,
	WriteBufferSize: 64 * 1024,
	CheckOrigin:     func(*http.Request) bool { return true },
}

// Server is the agent's HTTP surface. Public paths mirror the Sprites API with
// the /v1/sprites/{name} prefix stripped; /internal/* is for spritesd only.
type Server struct {
	Sessions *Manager
	// Services is optional; without it the services API answers 404.
	Services *Supervisor
	// Poweroff syncs and stops the guest.
	Poweroff func()
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) })
	mux.HandleFunc("HEAD /exec", func(w http.ResponseWriter, r *http.Request) {})
	mux.HandleFunc("GET /exec", s.handleExec)
	mux.HandleFunc("POST /exec", s.handleExecPost)
	mux.HandleFunc("GET /exec/{id}", s.handleAttach)
	mux.HandleFunc("POST /exec/{id}/kill", s.handleKill)
	if s.Services != nil {
		s.registerServices(mux)
	}
	s.registerFS(mux)
	mux.HandleFunc("GET /proxy", s.handleProxy)
	mux.HandleFunc("GET /internal/tcp", s.handleTCP)
	mux.HandleFunc("GET /internal/activity", s.handleActivity)
	mux.HandleFunc("POST /internal/presuspend", func(w http.ResponseWriter, r *http.Request) {
		unix.Sync()
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /internal/resumed", s.handleResumed)
	mux.HandleFunc("POST /internal/poweroff", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
		if s.Poweroff != nil {
			go s.Poweroff()
		}
	})
	return mux
}

func writeErr(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]string{"error": code, "message": msg})
}

func parseBool(v string) bool { b, _ := strconv.ParseBool(v); return b }

// parseDuration accepts Go durations ("10s") or bare seconds ("10").
func parseDuration(v string, def time.Duration) (time.Duration, error) {
	if v == "" {
		return def, nil
	}
	if n, err := strconv.Atoi(v); err == nil {
		return time.Duration(n) * time.Second, nil
	}
	return time.ParseDuration(v)
}

func optsFromQuery(r *http.Request) (SessionOpts, error) {
	q := r.URL.Query()
	o := SessionOpts{
		Cmd: q["cmd"], Path: q.Get("path"), Env: q["env"], Dir: q.Get("dir"),
		TTY: parseBool(q.Get("tty")),
		// The SDK always sends stdin=true|false; treat absence as enabled for raw WebSocket clients.
		Stdin: q.Get("stdin") == "" || parseBool(q.Get("stdin")),
	}
	if n, err := strconv.Atoi(q.Get("cols")); err == nil {
		o.Cols = uint16(n)
	}
	if n, err := strconv.Atoi(q.Get("rows")); err == nil {
		o.Rows = uint16(n)
	}
	def := 10 * time.Second
	if o.TTY {
		def = 0
	}
	d, err := parseDuration(q.Get("max_run_after_disconnect"), def)
	if err != nil {
		return o, fmt.Errorf("invalid max_run_after_disconnect: %w", err)
	}
	o.MaxRunAfterDisconnect = d
	return o, nil
}

func (s *Server) handleExec(w http.ResponseWriter, r *http.Request) {
	if !websocket.IsWebSocketUpgrade(r) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"sessions": s.Sessions.List()})
		return
	}
	if id := r.URL.Query().Get("id"); id != "" {
		s.attachWS(w, r, id)
		return
	}
	opts, err := optsFromQuery(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	sess, err := s.Sessions.Start(opts)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "exec_failed", err.Error())
		return
	}
	s.serveWS(w, r, sess, 0, true)
}

func (s *Server) handleAttach(w http.ResponseWriter, r *http.Request) {
	if !websocket.IsWebSocketUpgrade(r) {
		writeErr(w, http.StatusBadRequest, "bad_request", "websocket upgrade required")
		return
	}
	s.attachWS(w, r, r.PathValue("id"))
}

func (s *Server) attachWS(w http.ResponseWriter, r *http.Request, id string) {
	sess := s.Sessions.Get(id)
	if sess == nil {
		writeErr(w, http.StatusNotFound, "not_found", "exec session not found")
		return
	}
	offset, _ := strconv.ParseInt(r.URL.Query().Get("output_offset"), 10, 64)
	s.serveWS(w, r, sess, offset, false)
}

func (s *Server) serveWS(w http.ResponseWriter, r *http.Request, sess *Session, offset int64, owner bool) {
	conn, err := upgrader.Upgrade(w, r, http.Header{"X-Sprite-Capabilities": {"signal"}})
	if err != nil {
		if owner {
			sess.Signal(unix.SIGKILL)
		}
		return
	}
	c := newClient()
	sess.attach(c, offset, owner)
	defer sess.detach(c)
	defer c.shutdown()

	// Writer: drains the client queue onto the socket.
	go func() {
		defer conn.Close()
		for {
			select {
			case <-c.notify:
			case <-c.closed:
				return
			}
			for {
				c.mu.Lock()
				if len(c.q) == 0 {
					c.mu.Unlock()
					break
				}
				m := c.q[0]
				c.q = c.q[1:]
				c.qBytes -= len(m.data)
				c.mu.Unlock()
				if m.close {
					deadline := time.Now().Add(time.Second)
					conn.WriteControl(websocket.CloseMessage,
						websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""), deadline)
					// Give the peer a moment to read the close before the TCP teardown.
					time.Sleep(50 * time.Millisecond)
					return
				}
				conn.SetWriteDeadline(time.Now().Add(30 * time.Second))
				if err := conn.WriteMessage(m.typ, m.data); err != nil {
					return
				}
			}
		}
	}()

	for {
		typ, data, err := conn.ReadMessage()
		if err != nil {
			return
		}
		switch typ {
		case websocket.BinaryMessage:
			if sess.TTY {
				sess.WriteStdin(data)
				continue
			}
			if len(data) == 0 {
				continue
			}
			switch data[0] {
			case StreamStdin:
				sess.WriteStdin(data[1:])
			case StreamStdinEOF:
				sess.CloseStdin()
			}
		case websocket.TextMessage:
			var m struct {
				Type       string `json:"type"`
				Cols, Rows uint16
				Signal     string `json:"signal"`
			}
			if json.Unmarshal(data, &m) != nil {
				continue
			}
			switch m.Type {
			case "resize":
				sess.Resize(m.Cols, m.Rows)
			case "signal":
				if sig, err := ParseSignal(m.Signal); err == nil {
					sess.Signal(sig)
				}
			}
		}
	}
}

// handleExecPost runs a non-TTY command to completion. The upstream docs leave
// the response body unspecified; we stream combined output as text and report
// the exit code in the Sprite-Exit-Code trailer.
func (s *Server) handleExecPost(w http.ResponseWriter, r *http.Request) {
	opts, err := optsFromQuery(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	opts.TTY = false
	opts.Stdin = parseBool(r.URL.Query().Get("stdin"))
	opts.MaxRunAfterDisconnect = time.Second
	sess, err := s.Sessions.Start(opts)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "exec_failed", err.Error())
		return
	}
	if opts.Stdin {
		go func() {
			buf := make([]byte, 32*1024)
			for {
				n, err := r.Body.Read(buf)
				if n > 0 {
					sess.WriteStdin(buf[:n])
				}
				if err != nil {
					sess.CloseStdin()
					return
				}
			}
		}()
	}

	c := newClient()
	sess.attach(c, 0, true)
	defer sess.detach(c)
	defer c.shutdown()
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Trailer", "Sprite-Exit-Code")
	fl, _ := w.(http.Flusher)
	for {
		select {
		case <-c.notify:
		case <-r.Context().Done():
			return
		}
		for {
			c.mu.Lock()
			if len(c.q) == 0 {
				c.mu.Unlock()
				break
			}
			m := c.q[0]
			c.q = c.q[1:]
			c.qBytes -= len(m.data)
			c.mu.Unlock()
			if m.close {
				_, code := sess.Exited()
				w.Header().Set("Sprite-Exit-Code", strconv.Itoa(code))
				return
			}
			if m.typ == websocket.BinaryMessage && len(m.data) > 1 &&
				(m.data[0] == StreamStdout || m.data[0] == StreamStderr) {
				w.Write(m.data[1:])
				if fl != nil {
					fl.Flush()
				}
			}
		}
	}
}

func (s *Server) handleKill(w http.ResponseWriter, r *http.Request) {
	sess := s.Sessions.Get(r.PathValue("id"))
	if sess == nil {
		writeErr(w, http.StatusNotFound, "not_found", "exec session not found")
		return
	}
	if exited, _ := sess.Exited(); exited {
		writeErr(w, http.StatusGone, "session_exited", "exec session already exited")
		return
	}
	sig, err := ParseSignal(r.URL.Query().Get("signal"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	timeout, err := parseDuration(r.URL.Query().Get("timeout"), 10*time.Second)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request", "invalid timeout")
		return
	}

	w.Header().Set("Content-Type", "application/x-ndjson")
	fl, _ := w.(http.Flusher)
	emit := func(ev map[string]any) {
		ev["time"] = time.Now().UTC().Format(time.RFC3339Nano)
		json.NewEncoder(w).Encode(ev)
		if fl != nil {
			fl.Flush()
		}
	}
	name := strings.ToUpper(unix.SignalName(sig))
	if err := sess.Signal(sig); err != nil {
		emit(map[string]any{"type": "error", "message": err.Error()})
		return
	}
	emit(map[string]any{"type": "signal", "signal": name, "message": "sent " + name})
	// timeout=0 means "just deliver the signal" (the SDK's signal fallback).
	if timeout > 0 {
		select {
		case <-sess.Done():
			emit(map[string]any{"type": "exited", "message": "process exited"})
		case <-time.After(timeout):
			emit(map[string]any{"type": "timeout", "message": "process did not exit; sending SIGKILL"})
			sess.Signal(syscall.SIGKILL)
			<-sess.Done()
			emit(map[string]any{"type": "killed", "message": "process killed"})
		}
	}
	ev := map[string]any{"type": "complete"}
	if exited, code := sess.Exited(); exited {
		ev["exit_code"] = code
	}
	emit(ev)
}

func (s *Server) handleActivity(w http.ResponseWriter, r *http.Request) {
	last, attached := s.Sessions.Activity()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"idle_ms":           time.Since(last).Milliseconds(),
		"attached_sessions": attached,
	})
}

// handleResumed is called by spritesd after a snapshot restore: the guest
// clock is frozen at suspend time, so step it to the host's wall clock.
func (s *Server) handleResumed(w http.ResponseWriter, r *http.Request) {
	var body struct {
		UnixNano int64 `json:"unix_nano"`
	}
	b, _ := io.ReadAll(io.LimitReader(r.Body, 1<<16))
	if json.Unmarshal(b, &body) == nil && body.UnixNano > 0 {
		tv := unix.NsecToTimeval(body.UnixNano)
		if err := unix.Settimeofday(&tv); err != nil {
			writeErr(w, http.StatusInternalServerError, "clock", err.Error())
			return
		}
	}
	// Idle time is measured from the guest's monotonic clock, which did not
	// advance while suspended, but count the wake itself as activity.
	s.Sessions.touch()
	w.WriteHeader(http.StatusNoContent)
}
