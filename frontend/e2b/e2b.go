// Package e2b is an E2B-compatible front end on wisp's engine: the official,
// unmodified E2B SDKs (Python and JS) create, use, pause, resume and kill
// sandboxes against it as they do against hosted E2B. docs/providers/e2b.md is
// the specification it follows; docs/providers/e2b-differences.md lists where
// it differs from hosted E2B.
//
// It serves two planes on one listener (Handler):
//
//   - the control plane, E2B's REST API (/v2/sandboxes, /sandboxes/{id}, ...),
//     authenticated by X-API-Key (control.go);
//   - the envd plane: requests for a sandbox, routed the way E2B's sandbox
//     proxy routes them (by the E2b-Sandbox-Id and E2b-Sandbox-Port headers
//     that E2B_SANDBOX_URL mode sends, by a <port>-<id>.<domain> Host, or by
//     the signature on a signed file URL) and reverse-proxied to that port in
//     the guest over the engine's port dial (envd.go). Port 49983 is envd, E2B's
//     own in-guest daemon, which the E2B guest image runs (docs/images.md); any
//     other port is the user's.
//
// A sandbox is an engine record with API "e2b" and no Sprites name; what E2B
// knows about it (template, metadata, env vars, timeout, envd access token)
// is kept in Record.Ext["e2b"] (meta.go). envd is handed its access token,
// env vars and default user by POST /init after every start of the VM, cold or
// warm, before anything else reaches it: an engine boot hook (initEnvd).
package e2b

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/arugula-salad/wisp/engine"
	"github.com/arugula-salad/wisp/internal/store"
	"github.com/arugula-salad/wisp/internal/vmm"
)

// API is the Record.API of an E2B sandbox, and the Record.Ext key its metadata is under.
const API = "e2b"

// Defaults that hosted E2B has and the SDKs rely on.
const (
	// EnvdPort is where envd listens in the guest (images/e2b/envd.json).
	EnvdPort = "49983"
	// EnvdVersion is the envd the E2B image is built with (scripts/build-envd.sh).
	// The SDKs gate features on the version reported at create; 0.9.0 is past
	// every gate they have.
	EnvdVersion = "0.9.0"
	// defaultTimeout is POST /v2/sandboxes' and connect's (SandboxTimeoutDefaultV2).
	defaultTimeout = 300 * time.Second
	// defaultTimeoutV1 is the deprecated POST /sandboxes'.
	defaultTimeoutV1 = 15 * time.Second
	// clientID is deprecated but required by the spec; hosted always returns this.
	clientID = "6532622b"
	// The base template: what Sandbox.create() asks for by default.
	baseAlias    = "base"
	baseTemplate = "base"
	hostname     = "e2b"
	defaultUser  = "user"
	defaultHome  = "/home/user"
)

// Options configures the front end.
type Options struct {
	// Disk is the E2B guest image every sandbox's disk is cloned from:
	// <data>/images/e2b.ext4, which scripts/build-image.sh e2b builds.
	Disk string
	// Domain is reported as the sandbox's `domain`, so that the SDK's
	// getHost(port) is <port>-<id>.<Domain>: a host[:port] that reaches this
	// front end's listener, e.g. e2b.localhost:7823.
	Domain string
	// CheckKey authenticates an X-API-Key: ok for a key the daemon accepts,
	// admin for one that may change things (a read key may only GET).
	CheckKey func(key string) (admin, ok bool)
	// MaxTimeout is the longest timeout a sandbox may be given (hosted: 1 h
	// on the hobby tier, 24 h on pro). 0 is 24 h.
	MaxTimeout time.Duration
	// CPUs and MemMiB size every E2B sandbox's VM (hosted's base template is
	// 2 vCPUs and 512 MiB); 0 leaves it at the engine's default, which is then
	// what DefaultCPUs and DefaultMemMiB say, for GET /sandboxes/{id} to report.
	CPUs, MemMiB               int
	DefaultCPUs, DefaultMemMiB int
	// MaxSandboxes is how many sandboxes, of every API, may exist on this host
	// (wispd's --max-sprites); creating another is refused with 429, which the
	// SDKs report as a RateLimitError. 0 is no limit.
	MaxSandboxes int
}

// Frontend is the E2B API on an engine.
type Frontend struct {
	opts   Options
	store  *store.Store
	life   *engine.Engine
	log    *slog.Logger
	diskMB int // the template's disk, for diskSizeMB

	// locks serializes the control-plane transitions of one sandbox (pause,
	// connect, timeout, kill) against each other, so that, say, a connect and a
	// pause racing cannot leave the metadata saying the opposite of the VM.
	locks sync.Map // id -> *sync.Mutex

	// acquire starts a sandbox's VM if it is not running and holds it until
	// release (the engine's Acquire), and dialEnvd reaches a VM's ports (the
	// engine's port dial): fields so that tests can stand in for a VM.
	acquire  func(context.Context, store.Record) (*vmm.Machine, func(), error)
	dialEnvd envdDialer
}

// New attaches the E2B front end to life: it registers the boot hook that
// initializes envd, so call it before anything boots (before the daemon
// serves any API).
func New(opts Options, st *store.Store, life *engine.Engine, log *slog.Logger) *Frontend {
	if opts.MaxTimeout <= 0 {
		opts.MaxTimeout = 24 * time.Hour
	}
	f := &Frontend{opts: opts, store: st, life: life, log: log.With("api", API), acquire: life.Acquire, dialEnvd: machineDialer}
	if fi, err := os.Stat(opts.Disk); err == nil {
		f.diskMB = int(fi.Size() >> 20)
	} else {
		f.log.Warn("no E2B guest image: creating an E2B sandbox will fail until it is built (scripts/build-image.sh e2b)", "disk", opts.Disk)
	}
	life.OnBoot(f.initEnvd)
	return f
}

// lock serializes control-plane transitions on one sandbox.
func (f *Frontend) lock(id string) func() {
	v, _ := f.locks.LoadOrStore(id, &sync.Mutex{})
	mu := v.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}

// Handler serves both planes. Which one a request is for is decided the way
// E2B's own proxy decides it (route).
func (f *Frontend) Handler() http.Handler {
	api := f.controlPlane()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if t, ok := f.route(r); ok {
			f.serveSandbox(w, r, t)
			return
		}
		api.ServeHTTP(w, r)
	})
}
