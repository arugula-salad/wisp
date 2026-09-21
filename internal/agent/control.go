package agent

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

// The control channel is one long-lived WebSocket carrying a sequence of
// operations, so a client pays for the connection (and the wake) once. A client
// opens an op with an envelope, the op's ordinary frames then flow on the
// socket exactly as they would on a dedicated one, the server closes the op
// with an envelope, and the socket is idle again.
//
// The official SDKs disagree on the envelope, so all three spellings are
// accepted and each op is answered in the one it was opened with:
//
//	control:{"type":"op.start","op":"exec","args":{"cmd":["ls"],"tty":"true"}}   exec, every SDK
//	{"type":"op.start","op":"fs.list","args":"path=%2Ftmp"}                      Go SDK filesystem ops
//	{"type":"start","operation":"proxy","params":{"host":"localhost","port":80}} Go SDK proxy
//	{"type":"release"}                                                           Go SDK, when done with the socket
//
// args mirror url.Values: strings, or arrays of strings for repeated keys.
const controlPrefix = "control:"

type envelope struct {
	Type      string          `json:"type"`
	Op        string          `json:"op"`
	Operation string          `json:"operation"`
	Args      json.RawMessage `json:"args"`
	Params    json.RawMessage `json:"params"`

	prefixed bool
}

// parseEnvelope reports whether a text frame is addressed to the channel
// rather than to the running op (whose own JSON messages are resize/signal).
func parseEnvelope(data []byte) (envelope, bool) {
	var e envelope
	rest, prefixed := bytes.CutPrefix(data, []byte(controlPrefix))
	if json.Unmarshal(rest, &e) != nil {
		return e, prefixed
	}
	e.prefixed = prefixed
	switch e.Type {
	case "op.start", "start", "release":
		return e, true
	}
	return e, prefixed
}

// values flattens envelope args into url.Values, whichever way they were sent.
func values(raw json.RawMessage) url.Values {
	out := url.Values{}
	var encoded string
	if json.Unmarshal(raw, &encoded) == nil {
		out, _ = url.ParseQuery(encoded)
		return out
	}
	var m map[string]any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber() // keep 8080 from becoming 8080.000000
	if dec.Decode(&m) != nil {
		return out
	}
	for k, v := range m {
		if list, ok := v.([]any); ok {
			for _, e := range list {
				out.Add(k, fmt.Sprint(e))
			}
		} else if v != nil {
			out.Set(k, fmt.Sprint(v))
		}
	}
	return out
}

// controlOp is the operation currently using a control socket.
type controlOp struct {
	in       chan wsFrame
	done     chan struct{}
	prefixed bool
	closeIn  sync.Once
}

type controlConn struct {
	srv *Server
	ws  *websocket.Conn
	env []string // sprite-level environment, supplied by spritesd on the URL

	wmu sync.Mutex // the op's output and our envelopes share the socket

	mu     sync.Mutex
	op     *controlOp // nil while idle
	closed bool
}

// controlConns tracks the open control sockets so they can be drained before a suspend.
type controlConns struct {
	mu    sync.Mutex
	conns map[*controlConn]struct{}

	opened, ops atomic.Int64 // lifetime counters, for tests and diagnostics
}

func (cs *controlConns) add(cc *controlConn) {
	cs.mu.Lock()
	if cs.conns == nil {
		cs.conns = map[*controlConn]struct{}{}
	}
	cs.conns[cc] = struct{}{}
	cs.mu.Unlock()
	cs.opened.Add(1)
}

func (cs *controlConns) remove(cc *controlConn) {
	cs.mu.Lock()
	delete(cs.conns, cc)
	cs.mu.Unlock()
}

// quiesce closes the idle control sockets ahead of a suspend, so their clients
// find out now rather than by writing into a frozen VM. It reports false if an
// op has started since the host last looked, in which case the suspend is off.
func (cs *controlConns) quiesce() bool {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	for cc := range cs.conns {
		cc.mu.Lock()
		busy := cc.op != nil
		cc.closed = !busy
		cc.mu.Unlock()
		if busy {
			return false
		}
		cc.ws.WriteControl(websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.CloseGoingAway, "sprite suspending"), time.Now().Add(time.Second))
		cc.ws.Close()
	}
	return true
}

func (s *Server) handleControl(w http.ResponseWriter, r *http.Request) {
	ws, err := upgrader.Upgrade(w, r, http.Header{"X-Sprite-Capabilities": {"signal"}})
	if err != nil {
		return
	}
	defer ws.Close()
	q := r.URL.Query()
	cc := &controlConn{srv: s, ws: ws, env: q["env"]}
	s.control.add(cc)
	defer s.control.remove(cc)
	defer cc.release() // the socket is gone: whatever was running loses its peer

	// ?op= starts an operation on connect, its args being the rest of the query.
	if op := q.Get("op"); op != "" {
		q.Del("op")
		q.Del("env")
		cc.start(op, q, true)
	}
	for {
		typ, data, err := ws.ReadMessage()
		if err != nil {
			return
		}
		if typ == websocket.TextMessage {
			if e, ok := parseEnvelope(data); ok {
				cc.handle(e)
				continue
			}
		}
		cc.forward(wsFrame{typ, data})
	}
}

func (cc *controlConn) handle(e envelope) {
	switch e.Type {
	case "op.start":
		cc.start(e.Op, values(e.Args), e.prefixed)
	case "start":
		cc.start(e.Operation, values(e.Params), e.prefixed)
	case "release":
		cc.release()
	}
}

func (cc *controlConn) send(typ int, data []byte) error {
	cc.wmu.Lock()
	defer cc.wmu.Unlock()
	cc.ws.SetWriteDeadline(time.Now().Add(30 * time.Second))
	err := cc.ws.WriteMessage(typ, data)
	if err != nil {
		// A dedicated socket dies with its one op; this one would otherwise sit
		// in the client's pool with a frame missing from the middle of a stream.
		cc.ws.Close()
	}
	return err
}

func (cc *controlConn) sendJSON(v any) error {
	b, _ := json.Marshal(v)
	return cc.send(websocket.TextMessage, b)
}

// finish closes an op: op.complete, or op.error when msg is set.
func (cc *controlConn) finish(o *controlOp, msg string, args map[string]any) {
	e := map[string]any{"type": "op.complete", "args": args}
	if msg != "" {
		e = map[string]any{"type": "op.error", "args": map[string]any{"error": msg}}
	}
	b, _ := json.Marshal(e)
	if o.prefixed {
		b = append([]byte(controlPrefix), b...)
	}
	cc.send(websocket.TextMessage, b)
}

func (cc *controlConn) start(name string, args url.Values, prefixed bool) {
	o := &controlOp{in: make(chan wsFrame), done: make(chan struct{}), prefixed: prefixed}
	var run func(*controlOp, url.Values)
	switch {
	case name == "exec":
		run = cc.execOp
	case name == "proxy":
		run = cc.proxyOp
	case strings.HasPrefix(name, "fs."):
		run = func(o *controlOp, args url.Values) { cc.fsOp(o, name, args) }
	default:
		cc.finish(o, fmt.Sprintf("unknown operation %q", name), nil)
		return
	}

	cc.mu.Lock()
	if cc.op != nil || cc.closed {
		cc.mu.Unlock()
		cc.finish(o, "operation already in progress", nil)
		return
	}
	cc.op = o
	cc.mu.Unlock()
	cc.srv.control.ops.Add(1)
	cc.srv.Sessions.touch()

	// An op holds the sprite awake the way an attached exec session does.
	unpin := cc.srv.Sessions.pin()
	go func() {
		run(o, args)
		unpin()
	}()
}

// end marks the socket idle. It comes before the closing envelope, because a
// client may answer that envelope with its next op.start straight away.
func (cc *controlConn) end(o *controlOp) {
	cc.mu.Lock()
	if cc.op == o {
		cc.op = nil
	}
	cc.mu.Unlock()
	close(o.done)
}

// forward hands a frame to the running op; with none it is a straggler from
// an op that already ended, and is dropped.
func (cc *controlConn) forward(f wsFrame) {
	cc.mu.Lock()
	o := cc.op
	cc.mu.Unlock()
	if o == nil {
		return
	}
	select {
	case o.in <- f:
	case <-o.done:
	}
}

// release detaches the running op, if any, and waits until the socket is idle.
// Like forward it runs only on the read loop, which is what makes closing in safe.
func (cc *controlConn) release() {
	cc.mu.Lock()
	o := cc.op
	cc.mu.Unlock()
	if o == nil {
		return
	}
	o.closeIn.Do(func() { close(o.in) })
	<-o.done
}

func (cc *controlConn) execOp(o *controlOp, args url.Values) {
	// A failure is announced in the exec protocol as well: the Go SDK ignores
	// envelopes while an exec is running, and its TTY loop ends only on an exit.
	fail := func(msg string) {
		cc.end(o)
		cc.sendJSON(map[string]any{"type": "error", "error": msg})
		cc.sendJSON(map[string]any{"type": "exit", "exit_code": 127})
		cc.finish(o, msg, nil)
	}

	var sess *Session
	var offset int64
	if id := args.Get("id"); id != "" {
		if sess = cc.srv.Sessions.Get(id); sess == nil {
			fail("exec session not found")
			return
		}
		offset, _ = strconv.ParseInt(args.Get("output_offset"), 10, 64)
	} else {
		// Unlike a dedicated socket, a control client that has no stdin says nothing at all.
		if args.Get("stdin") == "" {
			args.Set("stdin", "false")
		}
		opts, err := optsFromValues(args)
		if err == nil {
			opts.Env = append(append([]string{}, cc.env...), opts.Env...)
			sess, err = cc.srv.Sessions.Start(opts)
		}
		if err != nil {
			fail(err.Error())
			return
		}
	}

	ended := serveSession(sess, offset, args.Get("id") == "", o.in, cc.send)
	cc.end(o)
	if ended {
		_, code := sess.Exited()
		cc.finish(o, "", map[string]any{"exitCode": code})
	}
}

func (cc *controlConn) proxyOp(o *controlOp, args url.Values) {
	port, _ := strconv.Atoi(args.Get("port"))
	targetClosed, err := cc.srv.serveProxy(args.Get("host"), port, o.in, cc.send)
	cc.end(o)
	switch {
	case err != nil:
		cc.finish(o, err.Error(), nil)
	case targetClosed:
		cc.finish(o, "", nil)
		// The Go SDK's proxy connection reads EOF only from a WebSocket close,
		// so a target that hung up costs the socket its reuse.
		cc.ws.WriteControl(websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""), time.Now().Add(time.Second))
		time.Sleep(50 * time.Millisecond)
		cc.ws.Close()
	}
}
