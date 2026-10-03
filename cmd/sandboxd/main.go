// sandboxd is wisp's multi-API daemon: one engine, one store, one set of API
// keys and one dashboard, serving the Sprites API (exactly as wispd serves it,
// with every wispd flag) and the E2B API (frontend/e2b), each on a listener of
// its own. A sandbox belongs to the API that created it: E2B sandboxes are not
// in the Sprites API's lists, nor sprites in E2B's.
//
//	sandboxd --data ~/ws/22 --listen 127.0.0.1:7822 --e2b-listen 127.0.0.1:7823 --net=false
//
// The E2B SDKs then reach it with E2B_API_URL=E2B_SANDBOX_URL=http://127.0.0.1:7823
// and E2B_API_KEY set to the root token (<data>/token) or an API key (docs/e2b-sdk.md).
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
	flag.Parse()
	opts, f := finish()

	daemon.Run("sandboxd", opts, f, daemon.Frontend{
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
