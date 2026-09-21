package server

import (
	"crypto/tls"
	"net"
	"net/http"
	"sync"
	"time"
)

// PublicHandler is what an internet-facing listener serves: sprite URLs and
// nothing else. The management API is not reachable through it whatever the
// request says, because its routes are simply not here.
func (s *Server) PublicHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name, ok := s.spriteForHost(r.Host)
		if !ok {
			http.NotFound(w, r)
			return
		}
		s.serveSpriteURL(w, r, name, true)
	})
}

// NewPublicServer wraps h for the open internet. There is no overall read or
// write timeout, because sprite apps stream and hold WebSockets; a client that
// dawdles is bounded by the header timeout and by LimitListener instead.
func NewPublicServer(h http.Handler, getCert func(*tls.ClientHelloInfo) (*tls.Certificate, error)) *http.Server {
	return &http.Server{
		Handler:           h,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
		MaxHeaderBytes:    64 << 10,
		TLSConfig:         &tls.Config{GetCertificate: getCert, MinVersion: tls.VersionTLS12},
	}
}

// LimitListener caps open connections, in total and per client, by closing the
// excess at accept. Per client means per IPv4 address or IPv6 /64, since a /64
// is what one subscriber is handed. Zero disables a cap.
func LimitListener(ln net.Listener, total, perClient int) net.Listener {
	return &limitListener{Listener: ln, total: total, perClient: perClient, byClient: map[string]int{}}
}

type limitListener struct {
	net.Listener
	total, perClient int

	mu       sync.Mutex
	n        int
	byClient map[string]int
}

func clientKey(addr net.Addr) string {
	tcp, ok := addr.(*net.TCPAddr)
	if !ok {
		return addr.String()
	}
	if ip4 := tcp.IP.To4(); ip4 != nil {
		return ip4.String()
	}
	return tcp.IP.Mask(net.CIDRMask(64, 128)).String()
}

func (l *limitListener) Accept() (net.Conn, error) {
	for {
		c, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		key := clientKey(c.RemoteAddr())
		l.mu.Lock()
		if (l.total > 0 && l.n >= l.total) || (l.perClient > 0 && l.byClient[key] >= l.perClient) {
			l.mu.Unlock()
			c.Close()
			continue
		}
		l.n++
		l.byClient[key]++
		l.mu.Unlock()
		return &limitConn{Conn: c, release: func() {
			l.mu.Lock()
			l.n--
			if l.byClient[key]--; l.byClient[key] == 0 {
				delete(l.byClient, key)
			}
			l.mu.Unlock()
		}}, nil
	}
}

type limitConn struct {
	net.Conn
	once    sync.Once
	release func()
}

func (c *limitConn) Close() error {
	c.once.Do(c.release)
	return c.Conn.Close()
}
