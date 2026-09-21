package agent

import (
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
	conn, target, err := dialTarget(init.Host, init.Port)
	if err != nil {
		ws.WriteJSON(map[string]string{"status": "error", "error": err.Error()})
		return
	}
	defer conn.Close()
	if err := ws.WriteJSON(map[string]string{"status": "connected", "target": target}); err != nil {
		return
	}
	s.Sessions.touch()

	go func() {
		defer ws.Close()
		buf := make([]byte, 32*1024)
		for {
			n, err := conn.Read(buf)
			if n > 0 {
				s.Sessions.touch()
				if ws.WriteMessage(websocket.BinaryMessage, buf[:n]) != nil {
					return
				}
			}
			if err != nil {
				return
			}
		}
	}()
	for {
		typ, data, err := ws.ReadMessage()
		if err != nil {
			return
		}
		if typ == websocket.BinaryMessage {
			s.Sessions.touch()
			if _, err := conn.Write(data); err != nil {
				return
			}
		}
	}
}

// handleTCP gives spritesd a raw byte stream to a guest port: after the 101
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
