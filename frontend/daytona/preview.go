package daytona

import (
	"context"
	"crypto/subtle"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"regexp"
	"strings"
)

// Preview URLs: http://<port>-<sandboxId>.<domain>/..., by Host on this
// listener, proxied to that port in the guest over the engine's port dial
// (no guest network is involved). A private sandbox's preview wants the token
// GET /sandbox/{id}/ports/{port}/preview-url hands out, as the
// x-daytona-preview-token header (or the DAYTONA_SANDBOX_AUTH_KEY query
// parameter), or an API key as a bearer token; a public one wants nothing.

// previewHost is the left-most label of a preview Host: <port>-<UUID>.
var previewHost = regexp.MustCompile(`^([0-9]{1,5})-([0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})$`)

type previewTo struct{ port, id string }

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
	return previewTo{port: m[1], id: m[2]}, true
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

func (f *Frontend) servePreview(w http.ResponseWriter, r *http.Request, t previewTo) {
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
	if st := f.state(rec, m); st != "started" {
		writeErr(w, r, http.StatusConflict, "CONFLICT", "Sandbox "+t.id+" is "+st+": start it first")
		return
	}
	mach, release, err := f.acquire(r.Context(), rec)
	if err != nil {
		f.bootFailed(w, r, err)
		return
	}
	defer release()
	proxy := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL.Scheme, pr.Out.URL.Host = "http", "sandbox"
			pr.Out.Host = pr.In.Host
			pr.Out.Header.Del("X-Daytona-Preview-Token")
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
