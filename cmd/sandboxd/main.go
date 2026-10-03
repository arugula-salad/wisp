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
// With --modal-listen it also serves the Modal API (frontend/modal, a spike):
// the modal client reaches it with MODAL_SERVER_URL=http://127.0.0.1:<port>,
// MODAL_TOKEN_SECRET set to the root token or an API key, and any MODAL_TOKEN_ID.
// wispd's subcommands (status, keys, images, ...) work against a sandboxd's
// data directory as they do against wispd's: they talk to the operator socket.
package main

import (
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

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
	flag.Parse()
	opts, f := finish()

	daemon.Run("sandboxd", opts, f, modalFrontend(*modalListen, *modalImage, *modalRouter),
		vercelFrontend(*vercelListen, *vercelDomain, *vercelImage, *vercelMaxTimeout, *vercelMem), daemon.Frontend{
		Name:  "the E2B API",
		Addr:  *e2bListen,
		IDLen: 21, // "i" and 20 characters, as hosted E2B's
		Setup: func(env daemon.Env) (http.Handler, error) {
			disk := *e2bImage
			if disk == "" {
				disk = filepath.Join(env.DataDir, "images", "e2b.ext4")
			}
			domain := *e2bDomain
			if _, _, err := net.SplitHostPort(domain); err != nil {
				if _, port, err := net.SplitHostPort(*e2bListen); err == nil {
					domain = net.JoinHostPort(domain, port)
				}
			}
			fe := e2b.New(e2b.Options{Disk: disk, Domain: domain, CheckKey: env.Sprites.CheckKey,
				MaxTimeout: *e2bMaxTimeout, CPUs: *e2bCPUs, MemMiB: *e2bMem,
				DefaultCPUs: env.Options.DefaultVCPUs, DefaultMemMiB: env.Options.DefaultMemMiB,
				MaxSandboxes: env.Options.MaxSprites},
				env.Store, env.Engine, env.Log)
			return fe.Handler(), nil
		},
	})
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
func vercelFrontend(listen, domain, image string, maxTimeout time.Duration, memPerVCPU int) daemon.Frontend {
	return daemon.Frontend{
		Name: "the Vercel Sandbox API",
		Addr: listen,
		Setup: func(env daemon.Env) (http.Handler, error) {
			if image == "" {
				image = filepath.Join(env.DataDir, "images", "vercel.ext4")
			}
			host, port, _ := net.SplitHostPort(listen)
			if _, _, err := net.SplitHostPort(domain); err != nil && port != "" {
				domain = net.JoinHostPort(domain, port)
			}
			fe := vercel.New(vercel.Options{Disk: image, CheckKey: env.Sprites.CheckKey, MaxTimeout: maxTimeout,
				MemPerVCPU: memPerVCPU, MaxSandboxes: env.Options.MaxSprites, RouteURL: func(sub string) string { return "http://" + sub + "." + domain }},
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
