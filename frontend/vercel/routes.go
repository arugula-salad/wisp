package vercel

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"regexp"
	"strconv"
	"strings"
)

// Port traffic: each declared port has a route subdomain (sb-…). Hosted serves
// it at https://<subdomain>.vercel.run; here it is this listener, with the
// subdomain as the Host's first label (http://<subdomain>.<domain>) or as the
// first path segment (http://<listener>/<subdomain>/…, for clients that cannot
// resolve a wildcard host), proxied to the port in the guest over the engine's
// port dial. No guest network is involved.

var subdomainRE = regexp.MustCompile(`^sb-[a-z0-9]+$`)

// portTarget is a sandbox port a request is for.
type portTarget struct {
	recordID string
	port     int
	// strip is the path prefix to take off (path-based routing).
	strip string
}

// route decides whether r is port traffic.
func (f *Frontend) route(r *http.Request) (portTarget, bool) {
	host := r.Host
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	if label, _, ok := strings.Cut(strings.ToLower(host), "."); ok && subdomainRE.MatchString(label) {
		if t, ok := f.bySubdomain(label); ok {
			return t, true
		}
	}
	seg, _, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/")
	if subdomainRE.MatchString(seg) {
		if t, ok := f.bySubdomain(seg); ok {
			t.strip = "/" + seg
			return t, true
		}
	}
	return portTarget{}, false
}

func (f *Frontend) bySubdomain(sub string) (portTarget, bool) {
	for _, sp := range f.sandboxes() {
		m, ok := metaOf(sp.Record)
		if !ok {
			continue
		}
		for _, rt := range m.Routes {
			if rt.Subdomain == sub {
				return portTarget{recordID: sp.ID, port: rt.Port}, true
			}
		}
	}
	return portTarget{}, false
}

func portErr(w http.ResponseWriter, status int, code, msg string) { writeErr(w, status, code, msg) }

// servePort proxies r to its port in a running sandbox. A stopped sandbox's
// routes answer 410 and do not wake it.
func (f *Frontend) servePort(w http.ResponseWriter, r *http.Request, t portTarget) {
	rec, err := f.store.GetRecord(t.recordID)
	m, ok := metaOf(rec)
	if err != nil || !ok {
		portErr(w, http.StatusNotFound, "not_found", "No sandbox serves this route.")
		return
	}
	rec, m = f.settle(rec, m)
	if m.current().Status != "running" {
		portErr(w, http.StatusGone, "sandbox_stopped", "Sandbox has stopped execution and is no longer available")
		return
	}
	mach, release, err := f.acquire(r.Context(), rec)
	if err != nil {
		portErr(w, http.StatusBadGateway, "sandbox_unavailable", "The sandbox could not be reached.")
		return
	}
	defer release()
	port := strconv.Itoa(t.port)
	proxy := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL.Scheme, pr.Out.URL.Host = "http", "sandbox"
			if t.strip != "" {
				p := strings.TrimPrefix(pr.In.URL.Path, t.strip)
				if !strings.HasPrefix(p, "/") {
					p = "/" + p
				}
				pr.Out.URL.Path, pr.Out.URL.RawPath = p, ""
			}
			pr.Out.Host = pr.In.Host
			pr.SetXForwarded()
		},
		Transport: &http.Transport{DisableKeepAlives: true, DisableCompression: true,
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) { return f.dialPort(ctx, mach, port) }},
		FlushInterval: -1,
		ErrorLog:      slog.NewLogLogger(f.log.Handler(), slog.LevelDebug),
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			f.log.Debug("sandbox port unreachable", "name", m.Name, "port", port, "err", err)
			portErr(w, http.StatusBadGateway, "port_not_open", "The sandbox is running but nothing is listening on port "+port+".")
		},
	}
	proxy.ServeHTTP(w, r)
}
