package server

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

var controlUpgrader = websocket.Upgrader{
	ReadBufferSize:  64 * 1024,
	WriteBufferSize: 64 * 1024,
	CheckOrigin:     func(*http.Request) bool { return true },
}

// goSDKUserAgent is what the official Go SDK sends on every WebSocket it dials.
const goSDKUserAgent = "sprites-go-sdk/"

// offersControl decides whether this client gets the control channel. Everyone
// does except, by default, the official Go SDK: it is the only client that
// switches to control on its own (the JS and Python SDKs are opt-in, and the
// sprite CLI disables it), and its port proxy is broken there. Over control
// its pool reader and its proxy handshake both read the one socket, so
// ProxyPorts hangs whenever the pool reader wins the race, about two times in
// three, with no fallback. Nothing the server sends can avoid a race between
// two readers in the client. A 404 can: the SDK takes it to mean "no control
// channel here" and uses a socket per operation for everything, which costs it
// nothing, since it never reuses a control socket anyway.
//
// Remove this once the SDK routes proxy reads through its pool reader.
func (s *Server) offersControl(r *http.Request) bool {
	return s.opts.ControlForGoSDK || !strings.HasPrefix(r.UserAgent(), goSDKUserAgent)
}

// controlRelay serves /control by terminating the WebSocket here and relaying
// messages to the agent's /control, instead of proxying bytes.
//
// It exists for one failure: the VM going away in the middle of an operation,
// which a checkpoint restore does on purpose. The official Go SDK's control
// reader simply stops when its socket dies and never wakes the operation that
// was using it, so the caller hangs until its context expires. Only a hop that
// owns the client's WebSocket can still speak after the backend is gone, so
// this one ends the operation in words the SDK does act on.
func (s *Server) controlRelay(w http.ResponseWriter, r *http.Request) {
	sp, ok := s.lookup(w, r)
	if !ok {
		return
	}
	m, release, err := s.life.Acquire(r.Context(), sp)
	if err != nil {
		s.log.Error("wake failed", "sprite", sp.Name, "err", err)
		writeErr(w, http.StatusServiceUnavailable, "wake_failed", err.Error())
		return
	}
	defer release()

	dialer := websocket.Dialer{HandshakeTimeout: 10 * time.Second, ReadBufferSize: 64 * 1024, WriteBufferSize: 64 * 1024,
		NetDialContext: func(ctx context.Context, _, _ string) (net.Conn, error) { return m.Dial(ctx) }}
	backend, resp, err := dialer.DialContext(r.Context(), "ws://agent/control?"+withSpriteEnv(r.URL.Query(), sp).Encode(), nil)
	if err != nil {
		if resp != nil { // the agent refused the upgrade; its answer is the client's answer
			w.Header().Set("Content-Type", resp.Header.Get("Content-Type"))
			w.WriteHeader(resp.StatusCode)
			io.Copy(w, resp.Body)
			return
		}
		writeErr(w, http.StatusBadGateway, "agent_unreachable", err.Error())
		return
	}
	defer backend.Close()
	hdr := http.Header{}
	if caps := resp.Header.Get("X-Sprite-Capabilities"); caps != "" {
		hdr.Set("X-Sprite-Capabilities", caps)
	}
	client, err := controlUpgrader.Upgrade(w, r, hdr)
	if err != nil {
		return
	}
	defer client.Close()
	// A pooled control socket with nothing running on it must not keep the sprite
	// awake; the agent pins the sprite itself for as long as an operation runs.
	release()

	var busy atomic.Bool // an operation has started and not yet completed
	go func() {
		defer backend.Close() // unblocks the loop below when the client leaves
		for {
			typ, data, err := client.ReadMessage()
			if err != nil {
				return
			}
			if typ == websocket.TextMessage && startsOp(data) {
				busy.Store(true)
			}
			if backend.WriteMessage(typ, data) != nil {
				return
			}
		}
	}()

	for {
		typ, data, err := backend.ReadMessage()
		if err != nil {
			deadline := time.Now().Add(time.Second)
			// A close frame means the agent ended the socket on purpose (say, a proxied
			// target hung up): that is the agent's message to the client, so pass it on.
			// Anything else is the backend vanishing under us.
			var ce *websocket.CloseError
			if errors.As(err, &ce) && ce.Code != websocket.CloseAbnormalClosure {
				client.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(ce.Code, ce.Text), deadline)
				return
			}
			if busy.Load() {
				// The SDK acts on "error" in pipe mode and only on "exit" in TTY mode, so
				// send both, then the envelope that ends the operation.
				for _, msg := range []string{
					`{"type":"error","error":"the sprite stopped while the operation was running"}`,
					`{"type":"exit","exit_code":255}`,
					`control:{"type":"op.error","args":{"error":"the sprite stopped while the operation was running"}}`,
				} {
					client.WriteMessage(websocket.TextMessage, []byte(msg))
				}
			}
			client.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseGoingAway, "sprite stopped"), deadline)
			return
		}
		if typ == websocket.TextMessage && endsOp(data) {
			busy.Store(false)
		}
		if client.WriteMessage(typ, data) != nil {
			return
		}
	}
}

// The control envelopes come in a prefixed dialect (control:{...}) and a bare
// one, and "start" is spelled two ways; see internal/agent/control.go. Matching
// on the type is enough here: a false positive only means a redundant notice on
// a socket that is being torn down anyway.
func startsOp(msg []byte) bool {
	msg = bytes.TrimPrefix(msg, []byte("control:"))
	return bytes.Contains(msg, []byte(`"type":"op.start"`)) || bytes.Contains(msg, []byte(`"type":"start"`))
}

func endsOp(msg []byte) bool {
	msg = bytes.TrimPrefix(msg, []byte("control:"))
	return bytes.Contains(msg, []byte(`"type":"op.complete"`)) || bytes.Contains(msg, []byte(`"type":"op.error"`))
}
