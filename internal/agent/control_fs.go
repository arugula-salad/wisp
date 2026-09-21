package agent

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"

	"github.com/gorilla/websocket"
)

// Filesystem ops on a control channel are the HTTP filesystem API replayed
// through its own handlers, with the answers reshaped into what the Go SDK's
// Fs*Control calls decode: a JSON text frame (then one binary frame of content
// for fs.read) followed by op.complete. A failure is the HTTP error body alone:
// the SDK stops reading there, so an envelope after it would be taken for the
// answer to the next op.

// memResponse is the ResponseWriter those handlers write into.
type memResponse struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func (m *memResponse) Header() http.Header         { return m.header }
func (m *memResponse) WriteHeader(status int)      { m.status = status }
func (m *memResponse) Write(p []byte) (int, error) { return m.body.Write(p) }

func (cc *controlConn) fsOp(o *controlOp, name string, args url.Values) {
	frames, ok := cc.srv.fsReply(o, name, args)
	cc.end(o)
	for _, f := range frames {
		cc.send(f.typ, f.data)
	}
	if ok {
		cc.finish(o, "", nil)
	}
}

// fsReply runs one filesystem op and returns the frames that answer it.
func (s *Server) fsReply(o *controlOp, name string, args url.Values) (frames []wsFrame, ok bool) {
	text := func(v any) []wsFrame {
		b, _ := json.Marshal(v)
		return []wsFrame{{websocket.TextMessage, b}}
	}
	fail := func(msg string) ([]wsFrame, bool) {
		return text(map[string]string{"error": msg, "code": "bad_request", "path": args.Get("path")}), false
	}
	jsonBody := func(fields map[string]any) *bytes.Reader {
		fields["workingDir"] = args.Get("workingDir")
		fields["recursive"] = parseBool(args.Get("recursive"))
		b, _ := json.Marshal(fields)
		return bytes.NewReader(b)
	}

	var handler http.HandlerFunc
	body := bytes.NewReader(nil)
	var reshape func(map[string]any) map[string]any // nil: the HTTP answer already fits
	switch name {
	case "fs.read":
		if args.Has("start") || args.Has("end") {
			return fail("range reads are not supported")
		}
		handler = s.fsRead
	case "fs.write":
		// The content is the client's next frame.
		f, open := <-o.in
		if !open {
			return nil, false
		}
		if f.typ != websocket.BinaryMessage {
			return fail("fs.write expects one binary frame of content")
		}
		// The SDK names mkdirParents only to turn it off.
		if !args.Has("mkdirParents") {
			args.Set("mkdirParents", "true")
		}
		handler, body = s.fsWrite, bytes.NewReader(f.data)
	case "fs.list":
		handler = s.fsList
	case "fs.delete":
		handler = s.fsDelete
		reshape = func(m map[string]any) map[string]any {
			return map[string]any{"deleted": []any{m["path"]}, "count": 1}
		}
	case "fs.rename":
		handler, body = s.fsRename, jsonBody(map[string]any{"source": args.Get("source"), "dest": args.Get("dest")})
	case "fs.copy":
		handler, body = s.fsCopy, jsonBody(map[string]any{"source": args.Get("source"), "dest": args.Get("dest")})
		reshape = func(m map[string]any) map[string]any {
			return map[string]any{"copied": []any{m}, "count": 1}
		}
	case "fs.chmod":
		handler, body = s.fsChmod, jsonBody(map[string]any{"path": args.Get("path"), "mode": args.Get("mode")})
		reshape = func(m map[string]any) map[string]any {
			return map[string]any{"affected": []any{m}, "count": 1}
		}
	case "fs.chown":
		fields := map[string]any{"path": args.Get("path")}
		for _, k := range []string{"uid", "gid"} {
			if n, err := strconv.Atoi(args.Get(k)); err == nil {
				fields[k] = n
			}
		}
		handler, body = s.fsChown, jsonBody(fields)
		reshape = func(m map[string]any) map[string]any {
			return map[string]any{"affected": []any{m}, "count": 1}
		}
	default:
		return fail("unknown operation " + strconv.Quote(name))
	}

	req, _ := http.NewRequest(http.MethodPost, "/?"+args.Encode(), body)
	resp := &memResponse{header: http.Header{}, status: http.StatusOK}
	handler(resp, req)

	answer := []wsFrame{{websocket.TextMessage, bytes.TrimSpace(resp.body.Bytes())}}
	switch {
	case resp.status != http.StatusOK:
		return answer, false
	case name == "fs.read":
		path, _ := resolvePath(args.Get("path"), args.Get("workingDir"))
		return append(text(map[string]any{"path": path, "size": resp.body.Len()}),
			wsFrame{websocket.BinaryMessage, resp.body.Bytes()}), true
	case reshape != nil:
		var m map[string]any
		json.Unmarshal(resp.body.Bytes(), &m)
		return text(reshape(m)), true
	}
	return answer, true
}
