// Package vercel is a Vercel Sandbox-compatible front end on wisp's engine:
// the official, unmodified Vercel Sandbox SDKs (JS @vercel/sandbox 3.x and
// Python vercel-sandbox) create, use, stop, resume, snapshot and delete
// sandboxes against it as they do against hosted Vercel.
// docs/providers/vercel.md is the specification it follows;
// docs/providers/vercel-differences.md lists where it differs from hosted Vercel.
//
// One listener (Handler) serves two things:
//
//   - the API, Vercel's /v2 and /v3 sandbox endpoints, with or without the
//     /api prefix of https://vercel.com/api, authenticated by a bearer token
//     (api.go, sessions.go, commands.go, files.go, snapshots.go);
//   - port traffic: a request whose Host's first label, or whose first path
//     segment, is one of a sandbox's route subdomains (sb-…) is proxied to
//     that port in the guest over the engine's port dial (routes.go).
//
// Vercel's sandbox protocol has no daemon in the guest: commands and files are
// translated here into wisp-agent's exec and filesystem API, reached over the
// engine's agent transport. The guest image is images/vercel (docs/images.md),
// where wisp-agent's `sprite` account is a second name for Vercel's `ubuntu`.
//
// A sandbox is an engine record with API "vercel" and its Vercel name as its
// name (names are unique within the vercel namespace). What Vercel knows about
// it (the current and past sessions, ports and routes, timeout, persistence,
// tags, snapshots) is kept in Record.Ext["vercel"] (meta.go). A session is one
// run of the VM: its timeout is the engine's deadline, whose action is stop;
// a stop of a persistent sandbox checkpoints the disk (the snapshot), and a
// resume starts a new session on that state.
package vercel

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/arugula-salad/wisp/engine"
	"github.com/arugula-salad/wisp/internal/store"
	"github.com/arugula-salad/wisp/internal/vmm"
)

// API is the Record.API of a Vercel sandbox, and the Record.Ext key its metadata is under.
const API = "vercel"

// Defaults that hosted Vercel has and the SDKs rely on (docs/providers/vercel.md).
const (
	defaultTimeout = 5 * time.Minute
	defaultVCPUs   = 2
	// memPerVCPU is hosted's: 2048 MB per vCPU.
	memPerVCPU = 2048
	// defaultHome is the default user's home and every command's default cwd.
	defaultHome = "/vercel"
	defaultUser = "ubuntu"
	hostname    = "vercel-sandbox"
	// What hosted reports for the default image (observed).
	runtimeName  = "node22"
	imageName    = "vercel/sandbox/universal"
	architecture = "amd64"
	// interactivePort is hosted's; there is no interactive PTY here.
	interactivePort = 26661
	maxPorts        = 15
)

// Options configures the front end.
type Options struct {
	// Disk is the guest image every sandbox's disk is cloned from:
	// <data>/images/vercel.ext4, which scripts/build-image.sh vercel builds.
	Disk string
	// CheckKey authenticates a bearer token: ok for one the daemon accepts,
	// admin for one that may change things (a read key may only GET).
	CheckKey func(key string) (admin, ok bool)
	// RouteURL is the url a route reports for a subdomain: something that
	// reaches this listener with that subdomain as the Host's first label or
	// as the path's first segment. Nil is http://<subdomain>.vercel.localhost.
	RouteURL func(subdomain string) string
	// MaxTimeout is the longest session timeout (hosted: 45 min on Hobby, 24 h
	// on Pro). 0 is 24 h.
	MaxTimeout time.Duration
	// MemPerVCPU is each sandbox's RAM per vCPU in MiB; 0 is hosted's 2048.
	MemPerVCPU int
	// MaxVCPUs caps resources.vcpus; 0 is 8.
	MaxVCPUs int
	// Region is reported as every sandbox's and session's region.
	Region string
	// Home is the default user's home, and every command's default cwd. Empty
	// is /vercel, as in the guest image; tests point it elsewhere.
	Home string
	// MaxSandboxes is how many sandboxes the daemon may hold, of every API
	// (wispd's --max-sprites); 0 is no limit.
	MaxSandboxes int
	// Sudo is the program a command with sudo: true runs under: empty is
	// "sudo", as in the guest image; tests use none ("-").
	Sudo string
}

// Frontend is the Vercel Sandbox API on an engine.
type Frontend struct {
	opts  Options
	store *store.Store
	life  *engine.Engine
	log   *slog.Logger

	// locks serializes the lifecycle transitions of one sandbox (create's
	// boot, stop, resume, snapshot, extend, delete) against each other, so the
	// metadata never says the opposite of the VM.
	locks sync.Map // record ID -> *sync.Mutex

	// sessions remembers which record a session ID belongs to.
	sessions sync.Map // session ID -> record ID

	cmds commandTable

	// acquire starts a sandbox's VM if it is not running and holds it until
	// release (the engine's Acquire); dialAgent reaches a VM's guest agent and
	// dialPort a port in it (the engine's agent transport and port dial).
	// Fields so that tests can stand in for a VM.
	acquire   func(context.Context, store.Record) (*vmm.Machine, func(), error)
	dialAgent func(*vmm.Machine) func(ctx context.Context, network, addr string) (net.Conn, error)
	dialPort  func(ctx context.Context, m *vmm.Machine, port string) (net.Conn, error)
	now       func() time.Time
	// checkpoint checkpoints a stopped sandbox's disk (the engine's
	// CreateCheckpoint): a field so that tests can make it fail.
	checkpoint func(rec store.Record, comment string) (store.Checkpoint, error)
}

// New attaches the Vercel front end to life.
func New(opts Options, st *store.Store, life *engine.Engine, log *slog.Logger) *Frontend {
	if opts.MaxTimeout <= 0 {
		opts.MaxTimeout = 24 * time.Hour
	}
	if opts.MemPerVCPU <= 0 {
		opts.MemPerVCPU = memPerVCPU
	}
	if opts.MaxVCPUs <= 0 {
		opts.MaxVCPUs = 8
	}
	if opts.Region == "" {
		opts.Region = "local"
	}
	if opts.Home == "" {
		opts.Home = defaultHome
	}
	if opts.Sudo == "" {
		opts.Sudo = "sudo"
	}
	if opts.RouteURL == nil {
		opts.RouteURL = func(sub string) string { return "http://" + sub + ".vercel.localhost" }
	}
	f := &Frontend{opts: opts, store: st, life: life, log: log.With("api", API),
		acquire: life.Acquire, dialAgent: engine.AgentDial, dialPort: engine.DialPort, now: time.Now,
		checkpoint: func(rec store.Record, comment string) (store.Checkpoint, error) {
			return life.CreateCheckpoint(rec, nil, comment, func(string, ...any) {})
		}}
	f.cmds.init()
	// A deleted sandbox's commands go with it, whoever deleted it.
	life.OnDelete(func(sp store.Sprite) {
		if sp.API == API {
			f.cmds.dropSandbox(sp.ID)
			f.locks.Delete(sp.ID)
		}
	})
	return f
}

// lock serializes lifecycle transitions on one sandbox.
func (f *Frontend) lock(id string) func() {
	v, _ := f.locks.LoadOrStore(id, &sync.Mutex{})
	mu := v.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}

// Handler serves the API and port traffic on one listener.
func (f *Frontend) Handler() http.Handler {
	api := f.apiMux()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if t, ok := f.route(r); ok {
			f.servePort(w, r, t)
			return
		}
		// The SDKs' base URL is https://vercel.com/api; a server pointed at as
		// http://host:port or http://host:port/api serves the same paths.
		if p, ok := strings.CutPrefix(r.URL.Path, "/api/"); ok {
			r2 := r.Clone(r.Context())
			r2.URL.Path = "/" + p
			r2.URL.RawPath = ""
			r = r2
		}
		api.ServeHTTP(w, r)
	})
}
