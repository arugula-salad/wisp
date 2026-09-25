package server

import (
	"bufio"
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/arugula-salad/wisp/internal/store"
	"github.com/arugula-salad/wisp/internal/vmm"
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
	// The upgrade exchange below is plain blocking I/O, and the agent's side of
	// it can take its time (it starts the sprite's HTTP service on demand and
	// waits for it), so a caller's deadline has to be put on the socket or it
	// would not bound this call at all. It is cleared again once the stream is
	// the caller's: from there the proxy, not us, decides how long to wait.
	if dl, ok := ctx.Deadline(); ok {
		conn.SetDeadline(dl)
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
	conn.SetDeadline(time.Time{})
	return &bufferedConn{Conn: conn, r: br}, nil
}

// A sprite URL wakes its sprite and proxies straight into it. On a cold boot the
// VM has resumed long before the user's app has bound its port, so the first
// visitor after an idle period used to get a proxy error on a sprite that was
// about to work perfectly well. The gate below waits, briefly and with a hard
// ceiling, for the guest port to accept a connection.
//
// It is a wait, not a hold: it runs inside the request, under the keep-awake
// hold Acquire already took for it (lifecycle.go), and adds nothing that
// outlives the response. A visitor that gives up cancels the request context and
// the gate stops with it, so nothing keeps polling an otherwise idle sprite.
const (
	defaultURLReadyWait = 10 * time.Second // a cold boot plus a normal app start; --url-ready-wait overrides it, 0 fails on the first refused connection
	maxURLReadyWait     = 60 * time.Second // whatever is configured, the visitor waits no longer
	urlReadyRetryMin    = 100 * time.Millisecond
	urlReadyRetryMax    = 500 * time.Millisecond
	// urlRetryAfter is what the 503 tells the visitor (and any crawler) to do.
	urlRetryAfter = 5
)

// errSpriteNotReady is the gate giving up: the sprite is awake, but nothing
// accepted a connection on its HTTP port in time. It is answered with a 503
// rather than the proxy's 502, because it is honestly temporary.
var errSpriteNotReady = errors.New("sprite is starting: nothing is listening on its HTTP port yet")

// clampReadyWait keeps --url-ready-wait inside what a visitor should ever be
// made to wait. A negative value is an operator slip rather than an intention,
// so it falls back to the default; whatever is configured, the gate gives up by
// maxURLReadyWait, because a browser tab waiting a minute on a blank page is
// worse than an honest 503.
func clampReadyWait(d time.Duration) time.Duration {
	if d < 0 {
		return defaultURLReadyWait
	}
	return min(d, maxURLReadyWait)
}

// dialWhenReady retries dial until it succeeds, the budget runs out or the
// visitor goes away. Each attempt gets the remaining budget as its deadline, so
// the total wait is bounded even when one attempt blocks inside the guest.
func dialWhenReady(ctx context.Context, wait time.Duration, dial func(context.Context) (net.Conn, error)) (net.Conn, error) {
	if wait <= 0 {
		return dial(ctx) // gate off: one attempt, and its error as it came
	}
	deadline := time.Now().Add(min(wait, maxURLReadyWait))
	for backoff := urlReadyRetryMin; ; backoff = min(backoff*2, urlReadyRetryMax) {
		attempt, cancel := context.WithDeadline(ctx, deadline)
		conn, err := dial(attempt)
		cancel() // dial does not keep the context past its return
		if err == nil {
			return conn, nil
		}
		if ctx.Err() != nil {
			return nil, ctx.Err() // the visitor hung up; no answer is owed
		}
		if left := time.Until(deadline); left <= 0 {
			return nil, fmt.Errorf("%w (waited %v; last error: %v)", errSpriteNotReady, min(wait, maxURLReadyWait), err)
		} else if backoff > left {
			backoff = left
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(backoff):
		}
	}
}

// spriteForHost maps "<name>.<url-domain>[:port]", or a custom domain attached
// to a sprite (domains.go), to a sprite name. A sprite answers only under its
// own URL domain: app-1.example.com is not app-1.widgets.test's URL.
func (s *Server) spriteForHost(host string) (string, bool) {
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	if domain, ok := URLDomainUnder(s.urlDomains, host); ok {
		name := strings.TrimSuffix(host, "."+domain)
		if strings.Contains(name, ".") {
			return "", false
		}
		if sp, err := s.store.Get(name); err == nil && s.urlDomainOf(sp) != domain {
			return "", false
		}
		return name, true
	}
	return s.store.DomainOwner(host)
}

// URLDomainUnder is the URL domain whose <name>.<domain> host is: the most
// specific one host is strictly under, since one URL domain may be nested in
// another (games.arugula.io in arugula.io). x.games.arugula.io is sprite x
// under games.arugula.io; games.arugula.io itself is still sprite "games"
// under arugula.io. The listener picks certificates by the same rule.
func URLDomainUnder(domains []string, host string) (string, bool) {
	best := ""
	for _, d := range domains {
		if strings.HasSuffix(host, "."+d) && len(d) > len(best) {
			best = d
		}
	}
	return best, best != ""
}

// urlDomainOf is the domain sp's URL is under. A sprite whose domain is no
// longer served (dropped from --url-domain) falls back to the default.
func (s *Server) urlDomainOf(sp store.Sprite) string {
	if sp.URLDomain != "" && slices.Contains(s.urlDomains, sp.URLDomain) {
		return sp.URLDomain
	}
	return s.urlDomains[0]
}

// underURLDomain reports the URL domain d is, or is under, if any: the most
// specific, when one is nested in another.
func (s *Server) underURLDomain(d string) (string, bool) {
	for _, domain := range s.urlDomains {
		if d == domain {
			return domain, true
		}
	}
	return URLDomainUnder(s.urlDomains, d)
}

// spriteURLProxy is the reverse proxy behind a sprite's URL. dial is how the
// guest's HTTP port is reached: a parameter so the readiness gate and the
// answers below can be exercised without a VM. stripAuth drops the API token
// from requests to a sprite whose URL is ours to guard, not the app's to read.
func spriteURLProxy(dial func(context.Context) (net.Conn, error), wait time.Duration, stripAuth bool) *httputil.ReverseProxy {
	return &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL.Scheme, pr.Out.URL.Host = "http", pr.In.Host
			pr.Out.Host = pr.In.Host
			pr.SetXForwarded()
			if stripAuth {
				pr.Out.Header.Del("Authorization") // the API token is ours, not the app's
			}
		},
		Transport: &http.Transport{DisableKeepAlives: true,
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return dialWhenReady(ctx, wait, dial)
			}},
		FlushInterval: -1,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			if errors.Is(err, errSpriteNotReady) {
				// Honest and temporary: the sprite is up, its app is not there
				// yet. A 502 would tell a visitor (or a crawler) the site is
				// broken; this tells them to come back.
				noteErr(r.Context(), "app not ready")
				w.Header().Set("Retry-After", strconv.Itoa(urlRetryAfter))
				http.Error(w, "this sprite is starting up and its app is not listening yet. Try again in a few seconds.",
					http.StatusServiceUnavailable)
				return
			}
			noteErr(r.Context(), "app unreachable")
			http.Error(w, "sprite is awake but the request failed: "+err.Error(), http.StatusBadGateway)
		},
	}
}

// serveSpriteURL handles requests to a sprite's own URL: wake it, then reverse
// proxy to its HTTP port. The sprite stays pinned awake while requests are in flight.
// public marks a request from the internet-facing listener, which is told less.
func (s *Server) serveSpriteURL(w http.ResponseWriter, r *http.Request, name string, public bool) {
	sp, err := s.store.Get(name)
	if err != nil {
		noteErr(r.Context(), "no such sprite")
		http.Error(w, "no such sprite", http.StatusNotFound)
		return
	}
	noteSprite(r.Context(), sp.Name)
	if sp.URLSettings.Auth != "public" {
		got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if subtle.ConstantTimeCompare([]byte(got), []byte(s.token)) != 1 {
			noteErr(r.Context(), "token required")
			w.Header().Set("WWW-Authenticate", `Bearer realm="sprite"`)
			http.Error(w, "this sprite's URL requires an API token (url_settings.auth is \"sprite\")", http.StatusUnauthorized)
			return
		}
	}
	m, release, err := s.life.Acquire(r.Context(), sp)
	var lim *LimitError
	if errors.As(err, &lim) {
		noteErr(r.Context(), "at a limit")
		writeLimitErr(w, lim)
		return
	}
	if err != nil {
		s.log.Error("wake failed", "sprite", sp.Name, "err", err)
		noteErr(r.Context(), "wake failed")
		msg := "sprite failed to wake"
		if !public {
			msg += ": " + err.Error() // host paths and the guest console
		}
		http.Error(w, msg, http.StatusServiceUnavailable)
		return
	}
	defer release() // the readiness wait below happens inside this hold, and ends with the request

	spriteURLProxy(func(ctx context.Context) (net.Conn, error) {
		return dialGuestTCP(ctx, m, "http")
	}, clampReadyWait(s.opts.URLReadyWait), sp.URLSettings.Auth != "public").ServeHTTP(w, r)
}
