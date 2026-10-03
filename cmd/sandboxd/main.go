// sandboxd is wisp's multi-API daemon: one engine, one store, one set of API
// keys and one dashboard, serving the Sprites API (exactly as wispd serves it,
// with every wispd flag), the E2B API (frontend/e2b) and the Vercel Sandbox API
// (frontend/vercel), each on a listener of its own. A sandbox belongs to the
// API that created it: E2B sandboxes are not in the Sprites API's lists, nor
// sprites in E2B's, and so on.
//
//	sandboxd --data ~/ws/22 --listen 127.0.0.1:7822 --e2b-listen 127.0.0.1:7823 --vercel-listen 127.0.0.1:7824 --net=false
//
// The E2B SDKs then reach it with E2B_API_URL=E2B_SANDBOX_URL=http://127.0.0.1:7823
// and E2B_API_KEY set to the root token (<data>/token) or an API key (docs/e2b-sdk.md);
// the Vercel SDKs at http://127.0.0.1:7824 with that token as VERCEL_TOKEN
// (docs/vercel-sdk.md).
// With --daytona-listen 127.0.0.1:7842 it also serves the Daytona API
// (frontend/daytona): the Daytona SDKs reach it with
// DAYTONA_API_URL=http://127.0.0.1:7842/api and DAYTONA_API_KEY set to the
// root token or an API key (docs/daytona-sdk.md).
// With --modal-listen it also serves the Modal API (frontend/modal, a spike):
// the modal client reaches it with MODAL_SERVER_URL=http://127.0.0.1:<port>,
// MODAL_TOKEN_SECRET set to the root token or an API key, and any MODAL_TOKEN_ID.
// With --sprites-public-url, the Sprites API on --listen sits behind a
// reverse proxy, as the other APIs' --*-public-url flags put them: the API at
// that host, sprite URLs at <name>.<that host>.
// wispd's subcommands (status, keys, images, ...) work against a sandboxd's
// data directory as they do against wispd's: they talk to the operator socket.
package main

import (
	"flag"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/arugula-salad/wisp/frontend/daytona"
	"github.com/arugula-salad/wisp/frontend/e2b"
	"github.com/arugula-salad/wisp/frontend/modal"
	"github.com/arugula-salad/wisp/frontend/vercel"
	"github.com/arugula-salad/wisp/internal/confine"
	"github.com/arugula-salad/wisp/internal/daemon"
)

func main() {
	// The confinement shim: the daemon re-execs itself to put a Landlock
	// domain on a VMM before exec'ing Firecracker (internal/confine).
	if len(os.Args) > 1 && os.Args[1] == confine.ShimArg {
		confine.RunShim(os.Args[2:])
	}
	if len(os.Args) > 1 && !strings.HasPrefix(os.Args[1], "-") {
		fmt.Fprintf(os.Stderr, "sandboxd has no subcommands; use wispd %s --data <dir>, which talks to a running sandboxd too\n", os.Args[1])
		os.Exit(2)
	}

	finish := daemon.Bind(flag.CommandLine)
	e2bListen := flag.String("e2b-listen", "127.0.0.1:7820", "serve the E2B API (control plane and sandbox traffic) on this address; empty turns it off")
	e2bDomain := flag.String("e2b-domain", "e2b.localhost", "E2B sandboxes report this as their domain, with --e2b-listen's port appended unless it has one, so the SDK's getHost(port) is <port>-<id>.<domain>, which this listener serves")
	e2bImage := flag.String("e2b-image", "", "the E2B guest disk every E2B sandbox starts from (default <data>/images/e2b.ext4, built by scripts/build-image.sh e2b)")
	e2bMaxTimeout := flag.Duration("e2b-max-timeout", 24*time.Hour, "the longest timeout an E2B sandbox may be given")
	e2bCPUs := flag.Int("e2b-vcpus", 2, "vCPUs per E2B sandbox, as hosted E2B's base template has (0 = --vcpus)")
	e2bMem := flag.Int("e2b-mem-mib", 512, "guest RAM (MiB) per E2B sandbox, as hosted E2B's base template has (0 = --mem-mib)")
	modalListen := flag.String("modal-listen", "", "serve the Modal API (a spike: frontend/modal) on this address, e.g. 127.0.0.1:7852; empty (the default) turns it off")
	modalImage := flag.String("modal-image", "", "the guest disk every Modal sandbox starts from (default <data>/images/modal.ext4, built by scripts/build-image.sh modal)")
	modalRouter := flag.String("modal-router-url", "", "the URL the Modal client is told to reach the task command router at (default http://<--modal-listen>; the client takes http:// only when MODAL_SERVER_URL is on localhost)")
	vercelListen := flag.String("vercel-listen", "", "serve the Vercel Sandbox API (and its sandboxes' routes) on this address, e.g. 127.0.0.1:7824; empty leaves it off")
	vercelDomain := flag.String("vercel-domain", "vercel.localhost", "Vercel routes are http://<subdomain>.<domain>, with --vercel-listen's port appended unless it has one, which that listener serves")
	vercelImage := flag.String("vercel-image", "", "the guest disk every Vercel sandbox starts from (default <data>/images/vercel.ext4, built by scripts/build-image.sh vercel)")
	vercelMaxTimeout := flag.Duration("vercel-max-timeout", 24*time.Hour, "the longest a Vercel sandbox session may run")
	vercelMem := flag.Int("vercel-mem-per-vcpu-mib", 2048, "guest RAM (MiB) per vCPU of a Vercel sandbox, as hosted Vercel gives")
	daytonaListen := flag.String("daytona-listen", "", "serve the Daytona API (control plane under /api, toolbox, preview URLs) on this address, e.g. 127.0.0.1:7842; empty leaves it off")
	daytonaDomain := flag.String("daytona-domain", "daytona.localhost", "Daytona preview URLs are <port>-<id>.<domain>, with --daytona-listen's port appended unless it has one; the domain must resolve to this listener")
	daytonaImage := flag.String("daytona-image", "", "the Daytona guest disk every Daytona sandbox starts from (default <data>/images/daytona.ext4, built by scripts/build-image.sh daytona)")
	daytonaURL := flag.String("daytona-url", "", "how Daytona clients reach --daytona-listen (e.g. https://daytona.example.com), for the toolbox URL sandboxes report; default: --daytona-public-url, else the Host each request came to")
	e2bPublic := flag.String("e2b-public-url", "", "the public URL of --e2b-listen behind a proxy (e.g. https://e2b.example.com): sandboxes report its host as their domain, port and all, instead of --e2b-domain with the listen port")
	vercelPublic := flag.String("vercel-public-url", "", "the public URL of --vercel-listen behind a proxy (e.g. https://vercel.example.com): routes are <scheme>://<subdomain>.<its host>, instead of http:// under --vercel-domain with the listen port")
	spritesPublic := flag.String("sprites-public-url", "", "the public URL of --listen behind a proxy that forwards that host and every name under it (e.g. https://sprites.example.com): the Sprites API is served there as with --api-host, and sprite URLs are <scheme>://<name>.<its host>. Replaces --url-domain and --public-listen")
	daytonaPublic := flag.String("daytona-public-url", "", "the public URL of --daytona-listen behind a proxy (e.g. https://daytona.example.com): previews are <scheme>://<port>-<id>.<its host> and the toolbox is under it, instead of --daytona-domain with the listen port")
	flag.Parse()
	opts, f := finish()
	var pub [3]*url.URL
	for i, p := range []struct{ name, raw string }{{"e2b-public-url", *e2bPublic}, {"vercel-public-url", *vercelPublic}, {"daytona-public-url", *daytonaPublic}} {
		u, err := publicURL(p.raw)
		if err != nil {
			fmt.Fprintf(os.Stderr, "--%s: %v\n", p.name, err)
			os.Exit(2)
		}
		pub[i] = u
	}
	if u, err := publicURL(*spritesPublic); err != nil {
		fmt.Fprintf(os.Stderr, "--sprites-public-url: %v\n", err)
		os.Exit(2)
	} else if u != nil {
		if bad := setFlags("url-domain", "public-listen"); bad != "" {
			fmt.Fprintf(os.Stderr, "--sprites-public-url replaces --%s; drop one\n", bad)
			os.Exit(2)
		}
		f.BehindProxy(&opts, u)
	}
	*e2bDomain = reportedDomain(*e2bDomain, *e2bListen, pub[0])
	*vercelDomain = reportedDomain(*vercelDomain, *vercelListen, pub[1])
	*daytonaDomain = reportedDomain(*daytonaDomain, *daytonaListen, pub[2])
	vercelScheme := "http"
	if pub[1] != nil {
		vercelScheme = pub[1].Scheme
	}
	if *daytonaURL == "" && pub[2] != nil {
		*daytonaURL = pub[2].String()
	}

	daemon.Run("sandboxd", opts, f, modalFrontend(*modalListen, *modalImage, *modalRouter),
		vercelFrontend(*vercelListen, *vercelDomain, vercelScheme, *vercelImage, *vercelMaxTimeout, *vercelMem),
		daytonaFrontend(*daytonaListen, *daytonaDomain, *daytonaImage, *daytonaURL), daemon.Frontend{
			Name:  "the E2B API",
			Addr:  *e2bListen,
			IDLen: 21, // "i" and 20 characters, as hosted E2B's
			Setup: func(env daemon.Env) (http.Handler, error) {
				disk := *e2bImage
				if disk == "" {
					disk = filepath.Join(env.DataDir, "images", "e2b.ext4")
				}
				fe := e2b.New(e2b.Options{Disk: disk, Domain: *e2bDomain, CheckKey: env.Sprites.CheckKey,
					MaxTimeout: *e2bMaxTimeout, CPUs: *e2bCPUs, MemMiB: *e2bMem,
					DefaultCPUs: env.Options.DefaultVCPUs, DefaultMemMiB: env.Options.DefaultMemMiB,
					MaxSandboxes: env.Options.MaxSprites},
					env.Store, env.Engine, env.Log)
				return fe.Handler(), nil
			},
		})
}

// setFlags is the first of names given on the command line, or "".
func setFlags(names ...string) string {
	var set string
	flag.Visit(func(fl *flag.Flag) {
		if set == "" && slices.Contains(names, fl.Name) {
			set = fl.Name
		}
	})
	return set
}

// publicURL parses a --*-public-url: http or https, a host, and nothing
// else, since everything reported under it is built from its host. Empty is
// nil: no proxy in front.
func publicURL(raw string) (*url.URL, error) {
	if raw == "" {
		return nil, nil
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, err
	}
	if u.Scheme != "http" && u.Scheme != "https" || u.Host == "" {
		return nil, fmt.Errorf("want http(s)://host[:port], got %q", raw)
	}
	if u.User != nil || strings.Trim(u.Path, "/") != "" || u.RawQuery != "" || u.Fragment != "" {
		return nil, fmt.Errorf("want only a scheme and a host[:port], got %q", raw)
	}
	u.Path = ""
	return u, nil
}

// reportedDomain is the domain an API puts in the URLs it hands its SDK: the
// public URL's host when there is one, port and all (none behind 443);
// otherwise domain, with listen's port appended unless it has one.
func reportedDomain(domain, listen string, public *url.URL) string {
	if public != nil {
		return public.Host
	}
	if _, _, err := net.SplitHostPort(domain); err != nil {
		if _, port, err := net.SplitHostPort(listen); err == nil {
			return net.JoinHostPort(domain, port)
		}
	}
	return domain
}

// modalFrontend is the Modal API (frontend/modal) on addr.
func modalFrontend(addr, image, routerURL string) daemon.Frontend {
	return daemon.Frontend{
		Name:  "the Modal API",
		Addr:  addr,
		IDLen: modal.IDLen,
		Setup: func(env daemon.Env) (http.Handler, error) {
			if image == "" {
				image = filepath.Join(env.DataDir, "images", "modal.ext4")
			}
			if routerURL == "" {
				host, port, err := net.SplitHostPort(addr)
				if err != nil {
					return nil, err
				}
				if ip := net.ParseIP(host); host == "" || ip != nil && ip.IsUnspecified() {
					host = "127.0.0.1"
				}
				routerURL = "http://" + net.JoinHostPort(host, port)
			}
			fe, err := modal.New(modal.Options{Disk: image, StateFile: filepath.Join(env.DataDir, "modal", "state.json"),
				RouterURL: routerURL, CheckKey: env.Sprites.CheckKey, MaxSandboxes: env.Options.MaxSprites},
				env.Store, env.Engine, env.Log)
			if err != nil {
				return nil, err
			}
			return fe.Handler(), nil
		},
	}
}

// vercelFrontend is the Vercel Sandbox API's listener.
func vercelFrontend(listen, domain, scheme, image string, maxTimeout time.Duration, memPerVCPU int) daemon.Frontend {
	return daemon.Frontend{
		Name: "the Vercel Sandbox API",
		Addr: listen,
		Setup: func(env daemon.Env) (http.Handler, error) {
			if image == "" {
				image = filepath.Join(env.DataDir, "images", "vercel.ext4")
			}
			host, port, _ := net.SplitHostPort(listen)
			fe := vercel.New(vercel.Options{Disk: image, CheckKey: env.Sprites.CheckKey, MaxTimeout: maxTimeout,
				MemPerVCPU: memPerVCPU, MaxSandboxes: env.Options.MaxSprites, RouteURL: func(sub string) string { return scheme + "://" + sub + "." + domain }},
				env.Store, env.Engine, env.Log)
			h := fe.Handler()
			// *.localhost resolves to ::1 where systemd-resolved answers it, so a
			// route on an IPv4 loopback listener is served on the IPv6 one too.
			if host == "127.0.0.1" && strings.HasSuffix(strings.Split(domain, ":")[0], "localhost") {
				if ln, err := net.Listen("tcp", net.JoinHostPort("::1", port)); err == nil {
					go (&http.Server{ReadHeaderTimeout: 10 * time.Second, Handler: h}).Serve(ln)
				} else {
					env.Log.Warn("Vercel routes on *.localhost may not resolve to this listener", "err", err)
				}
			}
			return h, nil
		},
	}
}

// daytonaFrontend is the Daytona API's listener.
func daytonaFrontend(listen, domain, image, baseURL string) daemon.Frontend {
	return daemon.Frontend{
		Name:  "the Daytona API",
		Addr:  listen,
		IDLen: 36, // a UUID, as hosted Daytona's
		Setup: func(env daemon.Env) (http.Handler, error) {
			if image == "" {
				image = filepath.Join(env.DataDir, "images", "daytona.ext4")
			}
			fe := daytona.New(daytona.Options{Disk: image, Domain: domain, BaseURL: baseURL, CheckKey: env.Sprites.CheckKey,
				MaxSandboxes: env.Options.MaxSprites}, env.Store, env.Engine, env.Log)
			return fe.Handler(), nil
		},
	}
}
