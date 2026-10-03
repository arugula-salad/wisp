package vercel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/gorilla/websocket"

	"github.com/arugula-salad/wisp/internal/vmm"
)

// The guest side: wisp-agent's exec and filesystem API (internal/agent),
// reached over the engine's agent transport. Vercel's commands and files are
// translated into it here.

// Stream IDs of the agent's non-TTY exec framing (agent.StreamStdout, ...).
const (
	streamStdout byte = 1
	streamStderr byte = 2
	streamExit   byte = 3
)

// execSpec is a process to start in the guest.
type execSpec struct {
	argv []string
	dir  string
	env  []string // K=V, over the agent's base environment (the user's HOME, PATH, ...)
}

// errNotFound is an executable the agent could not find on PATH.
type errNotFound struct{ name string }

func (e errNotFound) Error() string {
	return "[invalid_argument] executable file not found in $PATH: " + e.name
}

// errStart is a process the agent could not start (a cwd that does not exist, say).
type errStart struct{ msg string }

func (e errStart) Error() string { return e.msg }

// guestProc is a process started through the agent's exec WebSocket: its
// output frames as they come, then its exit code.
type guestProc struct {
	conn    *websocket.Conn
	agentID string // the agent's session ID, for kill
}

// startExec starts a process with no stdin and no TTY that keeps running if
// this connection drops (max_run_after_disconnect=0): the connection is held
// for the process's life anyway, by whoever reads it.
func (f *Frontend) startExec(ctx context.Context, m *vmm.Machine, spec execSpec) (*guestProc, error) {
	q := url.Values{}
	for _, a := range spec.argv {
		q.Add("cmd", a)
	}
	for _, e := range spec.env {
		q.Add("env", e)
	}
	q.Set("dir", spec.dir)
	q.Set("stdin", "false")
	q.Set("tty", "false")
	q.Set("max_run_after_disconnect", "0")
	d := websocket.Dialer{HandshakeTimeout: 30 * time.Second, ReadBufferSize: 64 * 1024, WriteBufferSize: 4096,
		NetDialContext: f.dialAgent(m)}
	conn, resp, err := d.DialContext(ctx, "ws://agent/exec?"+q.Encode(), nil)
	if err != nil {
		if resp != nil && resp.StatusCode >= 400 && resp.StatusCode < 500 {
			var body struct {
				Error, Message string
			}
			b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
			json.Unmarshal(b, &body)
			if strings.Contains(body.Message, "not found in PATH") {
				return nil, errNotFound{name: spec.argv[0]}
			}
			msg := body.Message
			if msg == "" {
				msg = strings.TrimSpace(string(b))
			}
			return nil, errStart{msg: msg}
		}
		return nil, fmt.Errorf("reach the guest agent: %w", err)
	}
	p := &guestProc{conn: conn}
	// The agent's first message is the session's info.
	conn.SetReadDeadline(time.Now().Add(30 * time.Second))
	typ, data, err := conn.ReadMessage()
	conn.SetReadDeadline(time.Time{})
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("read the exec session's info: %w", err)
	}
	if typ == websocket.TextMessage {
		var info struct {
			Type      string `json:"type"`
			SessionID string `json:"session_id"`
		}
		if json.Unmarshal(data, &info) == nil && info.Type == "session_info" {
			p.agentID = info.SessionID
		}
	}
	return p, nil
}

// next is the process's next event: output (stream 1 or 2 and its bytes), or
// its exit (exited, code). An error is the connection gone before the exit.
func (p *guestProc) next() (stream byte, data []byte, exited bool, code int, err error) {
	for {
		typ, msg, err := p.conn.ReadMessage()
		if err != nil {
			return 0, nil, false, 0, err
		}
		switch typ {
		case websocket.TextMessage:
			var ev struct {
				Type     string `json:"type"`
				ExitCode int    `json:"exit_code"`
			}
			if json.Unmarshal(msg, &ev) == nil && ev.Type == "exit" {
				return 0, nil, true, ev.ExitCode, nil
			}
		case websocket.BinaryMessage:
			if len(msg) == 0 {
				continue
			}
			switch msg[0] {
			case streamStdout, streamStderr:
				if len(msg) > 1 {
					return msg[0], msg[1:], false, 0, nil
				}
			case streamExit:
				code := 0
				if len(msg) > 1 {
					code = int(msg[1])
				}
				return 0, nil, true, code, nil
			}
		}
	}
}

func (p *guestProc) close() { p.conn.Close() }

// agentHTTP is an HTTP client for one VM's guest agent.
func (f *Frontend) agentHTTP(m *vmm.Machine) *http.Client {
	return &http.Client{Transport: &http.Transport{DisableKeepAlives: true, DialContext: f.dialAgent(m)}}
}

// signal delivers sig to a process the agent started, and its group.
func (f *Frontend) signal(ctx context.Context, m *vmm.Machine, agentID string, sig int) error {
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost,
		"http://agent/exec/"+url.PathEscape(agentID)+"/kill?timeout=0&signal="+strconv.Itoa(sig), nil)
	resp, err := f.agentHTTP(m).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	if resp.StatusCode == http.StatusGone || resp.StatusCode == http.StatusNotFound {
		return nil // it has exited already
	}
	if resp.StatusCode >= 300 {
		return fmt.Errorf("agent kill: %s", resp.Status)
	}
	return nil
}

// runShort runs a short command to completion and collects its output.
func (f *Frontend) runShort(ctx context.Context, m *vmm.Machine, spec execSpec) (stdout, stderr string, code int, err error) {
	p, err := f.startExec(ctx, m, spec)
	if err != nil {
		return "", "", 0, err
	}
	defer p.close()
	go func() { <-ctx.Done(); p.close() }()
	var out, errb strings.Builder
	for {
		stream, data, exited, c, err := p.next()
		if err != nil {
			return out.String(), errb.String(), 0, err
		}
		if exited {
			return out.String(), errb.String(), c, nil
		}
		if stream == streamStderr {
			errb.Write(data)
		} else {
			out.Write(data)
		}
	}
}

// agentFSError is a failure the agent's filesystem API answered with.
type agentFSError struct {
	status int
	msg    string
}

func (e agentFSError) Error() string { return e.msg }

func readAgentFSError(resp *http.Response) error {
	var body struct {
		Error string `json:"error"`
		Code  string `json:"code"`
	}
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if json.Unmarshal(b, &body) != nil || body.Error == "" {
		body.Error = strings.TrimSpace(string(b))
	}
	return agentFSError{status: resp.StatusCode, msg: body.Error}
}

// writeFile writes one file through the agent (PUT /fs/write), creating its
// parents. New files and directories belong to the agent's user, which in the
// Vercel image is uid 1000, `ubuntu`.
func (f *Frontend) writeFile(ctx context.Context, m *vmm.Machine, path string, mode int64, size int64, body io.Reader) error {
	q := url.Values{"path": {path}, "mode": {strconv.FormatInt(mode&0o7777, 8)}, "mkdirParents": {"true"}}
	req, _ := http.NewRequestWithContext(ctx, http.MethodPut, "http://agent/fs/write?"+q.Encode(), body)
	req.ContentLength = size
	resp, err := f.agentHTTP(m).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return readAgentFSError(resp)
	}
	io.Copy(io.Discard, resp.Body)
	return nil
}

// openFile reads one file through the agent (GET /fs/read). A missing one is
// an agentFSError with status 404.
func (f *Frontend) openFile(ctx context.Context, m *vmm.Machine, path string) (*http.Response, error) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://agent/fs/read?"+url.Values{"path": {path}}.Encode(), nil)
	resp, err := f.agentHTTP(m).Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 300 {
		defer resp.Body.Close()
		return nil, readAgentFSError(resp)
	}
	return resp, nil
}

// isGone says whether err is the guest going away (a stop under a request).
func isGone(err error) bool {
	var ne net.Error
	return errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || errors.As(err, &ne) ||
		websocket.IsUnexpectedCloseError(err) || websocket.IsCloseError(err, websocket.CloseAbnormalClosure)
}
