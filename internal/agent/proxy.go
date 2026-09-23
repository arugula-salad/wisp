package agent

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/gorilla/websocket"
)

func dialTarget(host string, port int) (net.Conn, string, error) {
	if host == "" {
		host = "localhost"
	}
	if port < 1 || port > 65535 {
		return nil, "", fmt.Errorf("invalid port %d", port)
	}
	target := net.JoinHostPort(host, strconv.Itoa(port))
	c, err := net.DialTimeout("tcp", target, 5*time.Second)
	return c, target, err
}

// handleProxy is the public TCP proxy: the client sends {"host","port"}, we
// answer {"status":"connected","target"} and then relay binary frames.
func (s *Server) handleProxy(w http.ResponseWriter, r *http.Request) {
	ws, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer ws.Close()
	var init struct {
		Host string `json:"host"`
		Port int    `json:"port"`
	}
	if err := ws.ReadJSON(&init); err != nil {
		return
	}
	in, stop := readFrames(ws)
	defer stop()
	s.serveProxy(init.Host, init.Port, in, ws.WriteMessage)
}

// serveProxy is the TCP proxy protocol after the client has named its target:
// dial, report the outcome, then relay binary frames until the target closes
// (true) or in is closed (false). Like serveSession it leaves the socket to
// the caller, so it serves both /proxy and a proxy op on a control channel.
func (s *Server) serveProxy(host string, port int, in <-chan wsFrame, send func(typ int, data []byte) error) (targetClosed bool, err error) {
	reply := func(v map[string]string) error {
		b, _ := json.Marshal(v)
		return send(websocket.TextMessage, b)
	}
	conn, target, err := dialTarget(host, port)
	if err != nil {
		reply(map[string]string{"status": "error", "error": err.Error()})
		return false, err
	}
	defer conn.Close()
	if reply(map[string]string{"status": "connected", "target": target}) != nil {
		return false, nil
	}
	s.Sessions.touch()

	eof := make(chan struct{})
	go func() {
		defer close(eof)
		buf := make([]byte, 32*1024)
		for {
			n, err := conn.Read(buf)
			if n > 0 {
				s.Sessions.touch()
				if send(websocket.BinaryMessage, buf[:n]) != nil {
					return
				}
			}
			if err != nil {
				return
			}
		}
	}()
	for {
		select {
		case f, ok := <-in:
			if !ok {
				return false, nil
			}
			if f.typ == websocket.BinaryMessage {
				s.Sessions.touch()
				if _, err := conn.Write(f.data); err != nil {
					return true, nil
				}
			}
		case <-eof:
			return true, nil
		}
	}
}

// handleTCP gives wispd a raw byte stream to a guest port: after the 101
// the vsock connection simply becomes the TCP connection. Used for sprite URLs.
func (s *Server) handleTCP(w http.ResponseWriter, r *http.Request) {
	// port=http means "wherever this sprite's URL should go": the service that
	// owns the HTTP port (started on demand), or 8080.
	port, _ := strconv.Atoi(r.URL.Query().Get("port"))
	if r.URL.Query().Get("port") == "http" {
		port = defaultHTTPPort
		if s.Services != nil {
			port = s.Services.HTTPTarget(r.Context())
		}
	}
	conn, _, err := dialTarget(r.URL.Query().Get("host"), port)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "connect_failed", err.Error())
		return
	}
	defer conn.Close()
	hj, ok := w.(http.Hijacker)
	if !ok {
		writeErr(w, http.StatusInternalServerError, "internal", "hijack unsupported")
		return
	}
	up, buf, err := hj.Hijack()
	if err != nil {
		return
	}
	defer up.Close()
	io.WriteString(up, "HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: tcp\r\n\r\n")
	s.Sessions.touch()

	done := make(chan struct{}, 2)
	go func() { io.Copy(conn, buf); closeWrite(conn); done <- struct{}{} }()
	go func() { io.Copy(up, conn); closeWrite(up); done <- struct{}{} }()
	<-done
	// Give the other direction a moment to drain a half-closed exchange, then tear down.
	select {
	case <-done:
	case <-time.After(30 * time.Second):
	}
	s.Sessions.touch()
}

func closeWrite(c net.Conn) {
	if cw, ok := c.(interface{ CloseWrite() error }); ok {
		cw.CloseWrite()
	}
}
