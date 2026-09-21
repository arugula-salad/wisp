package server

import (
	"bufio"
	"context"
	"crypto/subtle"
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"
	"strings"

	"github.com/jhgaylor/mini-sprites/internal/vmm"
)

// bufferedConn replays bytes the HTTP response parser read past the 101.
type bufferedConn struct {
	net.Conn
	r *bufio.Reader
}

func (c *bufferedConn) Read(p []byte) (int, error) { return c.r.Read(p) }

// dialGuestTCP opens a raw TCP stream to localhost:port inside the guest, tunnelled over vsock via the agent.
// port is a number, or "http" for the sprite's URL target (its HTTP service, else 8080).
func dialGuestTCP(ctx context.Context, m *vmm.Machine, port string) (net.Conn, error) {
	conn, err := m.Dial(ctx)
	if err != nil {
		return nil, err
	}
	fmt.Fprintf(conn, "GET /internal/tcp?port=%s HTTP/1.1\r\nHost: agent\r\nConnection: Upgrade\r\nUpgrade: tcp\r\n\r\n", port)
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		conn.Close()
		return nil, err
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		resp.Body.Close()
		conn.Close()
		return nil, fmt.Errorf("nothing is listening on the %s port inside the sprite", port)
	}
	return &bufferedConn{Conn: conn, r: br}, nil
}

// spriteForHost maps "<name>.<url-domain>[:port]" to a sprite name.
func (s *Server) spriteForHost(host string) (string, bool) {
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	name, ok := strings.CutSuffix(strings.ToLower(host), "."+s.urlDomain)
	return name, ok && name != "" && !strings.Contains(name, ".")
}

// serveSpriteURL handles requests to a sprite's own URL: wake it, then reverse
// proxy to its HTTP port. The sprite stays pinned awake while requests are in flight.
// public marks a request from the internet-facing listener, which is told less.
func (s *Server) serveSpriteURL(w http.ResponseWriter, r *http.Request, name string, public bool) {
	sp, err := s.store.Get(name)
	if err != nil {
		http.Error(w, "no such sprite", http.StatusNotFound)
		return
	}
	if sp.URLSettings.Auth != "public" {
		got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if subtle.ConstantTimeCompare([]byte(got), []byte(s.token)) != 1 {
			w.Header().Set("WWW-Authenticate", `Bearer realm="sprite"`)
			http.Error(w, "this sprite's URL requires an API token (url_settings.auth is \"sprite\")", http.StatusUnauthorized)
			return
		}
	}
	m, release, err := s.life.Acquire(r.Context(), sp)
	if err != nil {
		s.log.Error("wake failed", "sprite", sp.Name, "err", err)
		msg := "sprite failed to wake"
		if !public {
			msg += ": " + err.Error() // host paths and the guest console
		}
		http.Error(w, msg, http.StatusServiceUnavailable)
		return
	}
	defer release()

	proxy := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL.Scheme, pr.Out.URL.Host = "http", pr.In.Host
			pr.Out.Host = pr.In.Host
			pr.SetXForwarded()
			if sp.URLSettings.Auth != "public" {
				pr.Out.Header.Del("Authorization") // the API token is ours, not the app's
			}
		},
		Transport: &http.Transport{DisableKeepAlives: true,
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return dialGuestTCP(ctx, m, "http")
			}},
		FlushInterval: -1,
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
			http.Error(w, "sprite is awake but the request failed: "+err.Error(), http.StatusBadGateway)
		},
	}
	proxy.ServeHTTP(w, r)
}
