package daytona

import (
	"context"
	"crypto/subtle"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"path"
	"regexp"
	"strconv"
	"strings"
)

// Preview URLs: http://<port>-<sandboxId>.<domain>/..., by Host on this
// listener, proxied to that port in the guest over the engine's port dial
// (no guest network is involved). A private sandbox's preview wants the token
// GET /sandbox/{id}/ports/{port}/preview-url hands out, as the
// x-daytona-preview-token header (or the DAYTONA_SANDBOX_AUTH_KEY query
// parameter), or an API key as a bearer token; a public one wants nothing.

// previewHost is the left-most label of a preview Host: <port>-<UUID>.
var previewHost = regexp.MustCompile(`^([0-9]+)-([0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})$`)

// previewTo is a preview's sandbox and port. port is normalized (a number
// from 1 to 65535, as strconv.Itoa writes it) before anything looks at it;
// badPort is a Host whose port is not one.
type previewTo struct {
	port, id string
	badPort  bool
}

// previewTarget reads a preview Host.
func previewTarget(host string) (previewTo, bool) {
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	label, _, ok := strings.Cut(strings.ToLower(host), ".")
	if !ok {
		return previewTo{}, false
	}
	m := previewHost.FindStringSubmatch(label)
	if m == nil {
		return previewTo{}, false
	}
	n, err := strconv.Atoi(m[1])
	if err != nil || n < 1 || n > 65535 {
		return previewTo{id: m[2], badPort: true}, true
	}
	return previewTo{port: strconv.Itoa(n), id: m[2]}, true
}

// previewAllowed says whether r may reach a port of the sandbox.
func (f *Frontend) previewAllowed(r *http.Request, m meta) bool {
	if m.Public {
		return true
	}
	for _, t := range []string{r.Header.Get("X-Daytona-Preview-Token"), r.URL.Query().Get("DAYTONA_SANDBOX_AUTH_KEY")} {
		if t != "" && subtle.ConstantTimeCompare([]byte(t), []byte(m.PreviewToken)) == 1 {
			return true
		}
	}
	_, ok := f.checkKey(r)
	return ok
}

// cleanPath is p cleaned (no //, /./ or /../), rooted, with a trailing
// slash kept.
func cleanPath(p string) string {
	clean := path.Clean("/" + p)
	if strings.HasSuffix(p, "/") && clean != "/" {
		clean += "/"
	}
	return clean
}

func (f *Frontend) servePreview(w http.ResponseWriter, r *http.Request, t previewTo) {
	if t.badPort {
		writeErr(w, r, http.StatusBadRequest, "BAD_REQUEST", "Invalid preview port: want a number from 1 to 65535")
		return
	}
	// The app sees the path cleaned, as anything checking it here would, with
	// its trailing slash kept: /docs/ and /docs are different pages to many
	// servers, and redirect to each other.
	if clean := cleanPath(r.URL.Path); clean != r.URL.Path {
		r.URL.Path, r.URL.RawPath = clean, ""
	}
	rec, err := f.store.GetRecord(t.id)
	m, ok := metaOf(rec)
	if err != nil || !ok {
		writeErr(w, r, http.StatusNotFound, "NOT_FOUND", "Sandbox "+t.id+" not found")
		return
	}
	if !f.previewAllowed(r, m) {
		writeErr(w, r, http.StatusUnauthorized, "UNAUTHORIZED", "This sandbox is private: send its preview token as x-daytona-preview-token")
		return
	}
	b, release, ok := f.enter(w, r, t.id)
	if !ok {
		return
	}
	defer release()
	mach := b.mach
	// The credentials this front end consumed are its own, not the app's: the
	// app in the guest is untrusted, and must never see a daemon API key or the
	// preview token. An Authorization that is not a daemon key is the app's,
	// and goes through.
	_, daemonKey := f.checkKey(r)
	proxy := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL.Scheme, pr.Out.URL.Host = "http", "sandbox"
			pr.Out.Host = pr.In.Host
			pr.Out.Header.Del("X-Daytona-Preview-Token")
			if daemonKey {
				pr.Out.Header.Del("Authorization")
			}
			if q := pr.Out.URL.Query(); q.Has("DAYTONA_SANDBOX_AUTH_KEY") {
				q.Del("DAYTONA_SANDBOX_AUTH_KEY")
				pr.Out.URL.RawQuery = q.Encode()
			}
			pr.SetXForwarded()
		},
		Transport: &http.Transport{DisableKeepAlives: true, DisableCompression: true,
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) { return f.portDial(ctx, mach, t.port) }},
		FlushInterval: -1,
		ErrorLog:      slog.NewLogLogger(f.log.Handler(), slog.LevelDebug),
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			f.log.Debug("sandbox port unreachable", "id", t.id, "port", t.port, "err", err)
			writeErr(w, r, http.StatusBadGateway, "", "Nothing is listening on port "+t.port+" in sandbox "+t.id)
		},
	}
	proxy.ServeHTTP(w, r)
}
