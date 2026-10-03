// Package daytona is a Daytona-compatible front end on wisp's engine: the
// official, unmodified Daytona SDKs (Python `daytona` and TypeScript
// `@daytonaio/sdk`) create, use, stop, start and delete sandboxes against it as
// they do against hosted Daytona, for the core surface docs/providers/daytona.md
// lists. That survey is the specification; docs/providers/daytona-differences.md
// lists where this differs from hosted Daytona.
//
// Daytona's own server, runner, proxy and in-sandbox daemon are AGPL-3.0, and
// none of their source was read for this package: it is written from the
// Apache-2.0 SDKs, their generated API clients, and Daytona's public docs.
//
// It serves three things on one listener (Handler):
//
//   - the control plane, Daytona's REST API under /api (control.go), with the
//     API key as a bearer token;
//   - the toolbox, the API Daytona's SDKs reach inside a sandbox, under
//     /toolbox/{sandboxId}/... (the toolboxProxyUrl each sandbox reports). There
//     is no Daytona daemon in the guest: the toolbox is served here, host-side,
//     on wisp-agent's exec and filesystem API over the engine's agent transport
//     (toolbox.go, files.go, sessions.go);
//   - preview URLs, <port>-<sandboxId>.<domain> by Host, proxied to that port in
//     the guest over the engine's port dial (preview.go).
//
// A sandbox is an engine record with API "daytona" and no Sprites name; what
// Daytona knows about it (name, labels, env, resources, auto-stop interval,
// preview token, whether it was stopped) is kept in Record.Ext["daytona"]
// (meta.go). Its auto-stop interval is the engine's idle rule with action stop;
// stop and start are the engine's Stop and Acquire; delete is its Delete.
package daytona

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"

	"github.com/arugula-salad/wisp/engine"
	"github.com/arugula-salad/wisp/internal/store"
	"github.com/arugula-salad/wisp/internal/vmm"
)

// API is the Record.API of a Daytona sandbox, and the Record.Ext key its metadata is under.
const API = "daytona"

// Defaults that hosted Daytona has and the SDKs rely on or show.
const (
	// DaemonVersion is what GET /version and the DTO's daemonVersion report:
	// this front end's toolbox, not Daytona's daemon.
	DaemonVersion = "wisp-0.1.0"
	// defaultSnapshot is the name the default sandbox image goes by.
	defaultSnapshot = "daytona"
	// defaultUser and defaultHome are the guest image's (images/daytona).
	defaultUser = "daytona"
	defaultHome = "/home/daytona"
	hostname    = "daytona"
	// organizationID is reported for every sandbox: one daemon is one organization.
	organizationID = "wisp"
	defaultTarget  = "local"
	// defaultAutoStop is hosted's auto-stop interval, in minutes, for a create that names none.
	defaultAutoStop = 15
	// Hosted's default sandbox size: 1 vCPU, 1 GiB of memory, 3 GiB of disk.
	defaultCPU    = 1
	defaultMemGiB = 1
)

// Options configures the front end.
type Options struct {
	// Disk is the Daytona guest image every sandbox's disk is cloned from:
	// <data>/images/daytona.ext4, which scripts/build-image.sh daytona builds.
	Disk string
	// Domain is where preview URLs live: <port>-<id>.<Domain>, a host[:port]
	// that reaches this front end's listener, e.g. daytona.localhost:7842.
	Domain string
	// BaseURL is how clients reach this listener (http://127.0.0.1:7842), for
	// the toolboxProxyUrl a sandbox reports. Empty takes it from each request's
	// Host, which is what the SDK connected to.
	BaseURL string
	// CheckKey authenticates a bearer API key: ok for a key the daemon accepts,
	// admin for one that may change things (a read key may only GET).
	CheckKey func(key string) (admin, ok bool)
	// MaxCPU and MaxMemGiB bound what a create may ask for; 0 is no bound.
	MaxCPU, MaxMemGiB int
	// MaxSandboxes is how many sandboxes, of every API, may exist on this host
	// (wispd's --max-sprites); creating another is refused with 429, which the
	// SDKs report as DaytonaRateLimitError. 0 is no limit.
	MaxSandboxes int
}

// Frontend is the Daytona API on an engine.
type Frontend struct {
	opts    Options
	store   *store.Store
	life    *engine.Engine
	log     *slog.Logger
	diskGiB int // the image's size, reported as every sandbox's disk

	// locks serializes the control-plane transitions of one sandbox (start,
	// stop, delete, autostop), and names serializes creates against each other
	// so that two cannot take the same name.
	locks sync.Map // id -> *sync.Mutex
	names sync.Mutex
	// gates order a stop against the traffic that would wake the VM: toolbox
	// and preview requests decide and Acquire holding a read lock, and a stop
	// marks the sandbox stopped holding the write lock, so no request that saw
	// it started can boot the VM again after the stop has marked it.
	gates sync.Map // id -> *sync.RWMutex

	sessions *sessions
	// homeDir is the sandbox user's home in the guest, "" for the image's
	// (/home/daytona), and stateDir where sessions keep their state there, ""
	// for sessionStateDir. Tests, whose guest is this host, set both.
	homeDir, stateDir string

	// What reaches the engine and the guest, as fields so that tests can stand
	// in for a VM: acquire starts a VM if it is not running and holds it until
	// release (Acquire); stop is Stop; running asks Peek whether the VM is up
	// (or in a transition); agentDial reaches the guest's wisp-agent and
	// portDial one of its ports.
	acquire   func(context.Context, store.Record) (*vmm.Machine, func(), error)
	stop      func(store.Record) error
	running   func(id string) (up, busy bool)
	agentDial func(*vmm.Machine) func(ctx context.Context, network, addr string) (net.Conn, error)
	portDial  func(ctx context.Context, m *vmm.Machine, port string) (net.Conn, error)
}

// New attaches the Daytona front end to life.
func New(opts Options, st *store.Store, life *engine.Engine, log *slog.Logger) *Frontend {
	f := &Frontend{opts: opts, store: st, life: life, log: log.With("api", API),
		acquire: life.Acquire, stop: life.Stop, agentDial: engine.AgentDial, portDial: engine.DialPort,
		running: func(id string) (bool, bool) { v := life.Peek(id); return v.Running(), v.Busy }}
	f.sessions = newSessions(f)
	if fi, err := os.Stat(opts.Disk); err == nil {
		f.diskGiB = int((fi.Size() + 1<<30 - 1) >> 30)
	} else {
		f.log.Warn("no Daytona guest image: creating a Daytona sandbox will fail until it is built (scripts/build-image.sh daytona)", "disk", opts.Disk)
	}
	// An ephemeral sandbox (auto-delete 0) goes when it stops, however it stops.
	life.Events().AddSink(f.onEvent)
	// A deleted sandbox's sessions go with it, whoever deleted it.
	life.OnDelete(func(sp store.Sprite) {
		if sp.API == API {
			f.sessions.dropSandbox(sp.ID)
		}
	})
	return f
}

// lock serializes control-plane transitions on one sandbox.
func (f *Frontend) lock(id string) func() {
	v, _ := f.locks.LoadOrStore(id, &sync.Mutex{})
	mu := v.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}

// gate is the sandbox's stop/traffic gate (see gates).
func (f *Frontend) gate(id string) *sync.RWMutex {
	v, _ := f.gates.LoadOrStore(id, &sync.RWMutex{})
	return v.(*sync.RWMutex)
}

// forget drops a deleted sandbox's locks.
func (f *Frontend) forget(id string) {
	f.locks.Delete(id)
	f.gates.Delete(id)
}

// Handler serves the three planes: a preview Host goes to the sandbox's
// port, /toolbox/ to the toolbox, and the rest to the control plane.
func (f *Frontend) Handler() http.Handler {
	api := http.StripPrefix("/api", f.controlPlane())
	toolbox := f.toolbox()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if t, ok := previewTarget(r.Host); ok {
			f.servePreview(w, r, t)
			return
		}
		switch {
		case strings.HasPrefix(r.URL.Path, "/toolbox/"):
			toolbox.ServeHTTP(w, r)
		case r.URL.Path == "/api" || strings.HasPrefix(r.URL.Path, "/api/"):
			api.ServeHTTP(w, r)
		default:
			writeErr(w, r, http.StatusNotFound, "", "Not found: the Daytona API is under /api")
		}
	})
}

// toolboxBase is the toolboxProxyUrl reported to a client that sent r.
func (f *Frontend) toolboxBase(r *http.Request) string {
	if f.opts.BaseURL != "" {
		return strings.TrimSuffix(f.opts.BaseURL, "/") + "/toolbox"
	}
	scheme := "http"
	if r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https" {
		scheme = "https"
	}
	return scheme + "://" + r.Host + "/toolbox"
}
