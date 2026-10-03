package daytona

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/arugula-salad/wisp/internal/vmm"
	"github.com/gorilla/websocket"
)

// The toolbox is wisp-agent's API underneath: its filesystem endpoints over
// HTTP, and its exec sessions over WebSocket (internal/agent), both reached
// over the engine's agent transport. This file is the client side of that.

// Stream bytes of wisp-agent's non-TTY exec framing (internal/agent/session.go).
const (
	streamStdin    byte = 0
	streamStdout   byte = 1
	streamStderr   byte = 2
	streamStdinEOF byte = 4
)

// agentError is wisp-agent answering with an error status.
type agentError struct {
	status int
	code   string // the agent's: not_found, permission_denied, conflict, bad_request, ...
	msg    string
}

func (e *agentError) Error() string { return e.msg }

// httpClient carries requests to mach's wisp-agent, a fresh stream per
// request as the engine's own agent calls are.
func (f *Frontend) httpClient(mach *vmm.Machine) *http.Client {
	return &http.Client{Transport: &http.Transport{DisableKeepAlives: true, DisableCompression: true, DialContext: f.agentDial(mach)}}
}

// agentDo makes one request to the agent and returns its response when it
// succeeded, or an *agentError.
func (f *Frontend) agentDo(ctx context.Context, mach *vmm.Machine, method, path string, q url.Values, body io.Reader) (*http.Response, error) {
	u := "http://agent" + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return nil, err
	}
	resp, err := f.httpClient(mach).Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 300 {
		defer resp.Body.Close()
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		var e struct {
			Error   string `json:"error"`
			Code    string `json:"code"`
			Message string `json:"message"`
		}
		json.Unmarshal(b, &e)
		msg := e.Error
		if e.Message != "" {
			msg = e.Message
		}
		if msg == "" {
			msg = strings.TrimSpace(string(b))
		}
		return nil, &agentError{status: resp.StatusCode, code: e.Code, msg: msg}
	}
	return resp, nil
}

// agentJSON makes a request with an optional JSON body and decodes the answer into out.
func (f *Frontend) agentJSON(ctx context.Context, mach *vmm.Machine, method, path string, q url.Values, body, out any) error {
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	resp, err := f.agentDo(ctx, mach, method, path, q, rd)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

// execSpec is one command for wisp-agent to run.
type execSpec struct {
	Cmd []string
	Env []string // KEY=VALUE, after the agent's own environment (later wins)
	Dir string   // "" is the agent's default, the user's home
	// Stdin keeps the command's stdin open for input (stdin), until closeStdin.
	Stdin bool
	// Detached keeps the command running if this connection goes away (a
	// session command outlives the request that started it); otherwise the
	// agent kills it a few seconds after.
	Detached bool
}

// execConn is a running command: its output arrives through run, and its
// input goes through stdin.
type execConn struct {
	ws *websocket.Conn
	mu sync.Mutex // serializes writes
}

// startExec starts a command in the guest over the agent's exec WebSocket.
func (f *Frontend) startExec(ctx context.Context, mach *vmm.Machine, spec execSpec) (*execConn, error) {
	q := url.Values{"cmd": spec.Cmd, "stdin": {fmt.Sprint(spec.Stdin)}}
	if len(spec.Env) > 0 {
		q["env"] = spec.Env
	}
	if spec.Dir != "" {
		q.Set("dir", spec.Dir)
	}
	if spec.Detached {
		q.Set("max_run_after_disconnect", "0")
	} else {
		q.Set("max_run_after_disconnect", "2s")
	}
	d := websocket.Dialer{NetDialContext: f.agentDial(mach), HandshakeTimeout: 30 * time.Second,
		ReadBufferSize: 64 << 10, WriteBufferSize: 64 << 10}
	ws, resp, err := d.DialContext(ctx, "ws://agent/exec?"+q.Encode(), nil)
	if err != nil {
		if resp != nil {
			b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
			resp.Body.Close()
			var e struct {
				Message string `json:"message"`
			}
			if json.Unmarshal(b, &e) == nil && e.Message != "" {
				return nil, &agentError{status: resp.StatusCode, code: "exec_failed", msg: e.Message}
			}
		}
		return nil, err
	}
	return &execConn{ws: ws}, nil
}

// errExecLost is the connection to a command ending before the command did:
// the VM went away (a stop), or the agent did.
var errExecLost = errors.New("the connection to the command was lost before it exited")

// run reads the command's output, calling out for each chunk of stdout or
// stderr in the order the agent saw it, until the command exits. It returns
// the exit code, or errExecLost.
func (c *execConn) run(out func(stream byte, data []byte)) (int, error) {
	for {
		typ, data, err := c.ws.ReadMessage()
		if err != nil {
			return 0, errExecLost
		}
		switch typ {
		case websocket.BinaryMessage:
			if len(data) > 0 && (data[0] == streamStdout || data[0] == streamStderr) {
				out(data[0], data[1:])
			}
		case websocket.TextMessage:
			var m struct {
				Type     string `json:"type"`
				ExitCode int    `json:"exit_code"`
			}
			if json.Unmarshal(data, &m) == nil && m.Type == "exit" {
				return m.ExitCode, nil
			}
		}
	}
}

func (c *execConn) write(typ int, data []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ws.SetWriteDeadline(time.Now().Add(30 * time.Second))
	return c.ws.WriteMessage(typ, data)
}

// stdin sends input to the command.
func (c *execConn) stdin(p []byte) error {
	return c.write(websocket.BinaryMessage, append([]byte{streamStdin}, p...))
}

// closeStdin ends the command's input.
func (c *execConn) closeStdin() error {
	return c.write(websocket.BinaryMessage, []byte{streamStdinEOF})
}

// kill sends SIGKILL to the command's process group.
func (c *execConn) kill() error {
	return c.write(websocket.TextMessage, []byte(`{"type":"signal","signal":"KILL"}`))
}

func (c *execConn) close() { c.ws.Close() }
