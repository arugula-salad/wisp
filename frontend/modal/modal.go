// Package modal is a Modal-compatible front end on wisp's engine, from a
// time-boxed spike: the unmodified modal Python client (pinned: 1.6.0), given
// MODAL_SERVER_URL=http://127.0.0.1:<port>, looks up an app, builds
// Image.debian_slim(), creates a sandbox, execs in it (stdout, stderr, exit
// codes, timeouts), reattaches to it with Sandbox.from_id and terminates it.
// docs/providers/modal.md is the specification it follows (section 3 is the
// RPC sequence), docs/providers/modal-differences.md lists where it differs
// from hosted Modal.
//
// Modal's protocol is gRPC: two services, which this serves on one h2c
// listener (Handler) as the client allows when the server is on localhost.
//
//   - modal.client.ModalClient, the control plane (control.go): apps, the
//     environment, images, and the sandbox lifecycle, V2 (the client's
//     default) and V1 (MODAL_SANDBOX_V2=0). Authenticated by the token headers
//     the client sends on every call: the secret is checked against the
//     daemon's keys, the token ID is ignored.
//   - modal.task_command_router.TaskCommandRouter (router.go): exec and its
//     stdio. The control plane hands the client this listener's URL and a JWT
//     for one sandbox's task; the router accepts only that.
//
// Only the RPCs the client makes for the spike's script are implemented
// (modalpb, which is cut down to them); every other one is UNIMPLEMENTED,
// which the client raises as UnimplementedError.
//
// A sandbox is an engine record with API "modal" and no Sprites name; what
// Modal knows about it is in Record.Ext["modal"] (meta.go). Apps, the images
// served, and how finished sandboxes ended are the front end's own, in a small
// file (state.go). Commands run through wisp-agent's exec API, as root through
// sudo (exec.go), with their output kept for the client to read from any
// offset (router.go).
package modal

import (
	"context"
	"crypto/rand"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/arugula-salad/wisp/engine"
	"github.com/arugula-salad/wisp/frontend/modal/modalpb"
	"github.com/arugula-salad/wisp/internal/store"
	"github.com/arugula-salad/wisp/internal/vmm"
)

// API is the Record.API of a Modal sandbox, and the Record.Ext key its metadata is under.
const API = "modal"

const (
	// BuilderVersion is the image builder version the environment reports. The
	// client refuses one it does not know, and builds debian_slim's recipe for
	// this one (images/modal is that recipe, prebuilt).
	BuilderVersion = "2025.06"
	// defaultTimeout is Sandbox.create's, which the client always sends anyway.
	defaultTimeout = 300 * time.Second
	hostname       = "modal"
)

// Options configures the front end.
type Options struct {
	// Disk is the guest image every sandbox's disk is cloned from:
	// <data>/images/modal.ext4, which scripts/build-image.sh modal builds.
	Disk string
	// StateFile keeps apps, images and the results of finished sandboxes.
	StateFile string
	// RouterURL is where the client reaches the task command router: this
	// front end's own listener, http://host:port. The client accepts http://
	// only when MODAL_SERVER_URL is on localhost.
	RouterURL string
	// CheckKey authenticates a token secret: ok for a key the daemon accepts,
	// admin for one that may change things.
	CheckKey func(key string) (admin, ok bool)
	// MaxTimeout is the longest timeout a sandbox may be given (hosted: 24 h).
	MaxTimeout time.Duration
	// CPUs and MemMiB size every sandbox's VM; 0 is the engine's default.
	CPUs, MemMiB int
	// MaxSandboxes is how many sandboxes, of every API, may exist on this
	// host (--max-sprites); 0 is no limit.
	MaxSandboxes int
}

// Frontend is the Modal API on an engine.
type Frontend struct {
	opts  Options
	store *store.Store
	life  *engine.Engine
	log   *slog.Logger
	state *state
	// key signs the router JWTs. It is new in every process: a client holding
	// an old one is refused, and fetches a new one, as it does when one expires.
	key []byte

	execs sync.Map // execKey -> *execution
	// running is each sandbox's entrypoint and execs, which terminate stops.
	runMu   sync.Mutex
	running map[string]map[*execution]struct{}

	// acquire and run stand in for the engine's Acquire and wisp-agent's exec
	// in tests.
	acquire func(context.Context, store.Record) (*vmm.Machine, func(), error)
	run     runner
}

// New attaches the Modal front end to life.
func New(opts Options, st *store.Store, life *engine.Engine, log *slog.Logger) (*Frontend, error) {
	if opts.MaxTimeout <= 0 {
		opts.MaxTimeout = 24 * time.Hour
	}
	s, err := openState(opts.StateFile)
	if err != nil {
		return nil, err
	}
	key := make([]byte, 32)
	rand.Read(key)
	f := &Frontend{opts: opts, store: st, life: life, log: log.With("api", API), state: s, key: key,
		running: map[string]map[*execution]struct{}{}, acquire: life.Acquire}
	f.run = f.agentExec
	if _, err := os.Stat(opts.Disk); err != nil {
		f.log.Warn("no Modal guest image: creating a Modal sandbox will fail until it is built (scripts/build-image.sh modal)", "disk", opts.Disk)
	}
	return f, nil
}

// Handler serves both gRPC services. It needs HTTP/2, which the daemon's
// listeners speak in cleartext (h2c) to a client that assumes it, as grpclib does.
func (f *Frontend) Handler() http.Handler {
	gs := grpc.NewServer(grpc.UnaryInterceptor(f.unaryAuth), grpc.StreamInterceptor(f.streamAuth))
	modalpb.RegisterModalClientServer(gs, &control{f: f})
	modalpb.RegisterTaskCommandRouterServer(gs, &router{f: f})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ProtoMajor != 2 || !strings.HasPrefix(r.Header.Get("Content-Type"), "application/grpc") {
			http.Error(w, "this is a Modal-compatible gRPC endpoint: point the modal client's MODAL_SERVER_URL here", http.StatusUnsupportedMediaType)
			return
		}
		gs.ServeHTTP(w, r)
	})
}

// Who is calling, as the auth interceptors found it.
type caller struct {
	admin bool
	task  string // the router: the task the JWT is for
}

type callerKey struct{}

func callerOf(ctx context.Context) caller {
	c, _ := ctx.Value(callerKey{}).(caller)
	return c
}

const (
	controlService = "/modal.client.ModalClient/"
	routerService  = "/modal.task_command_router.TaskCommandRouter/"
)

// authenticate checks a call's credentials: the token secret on the control
// plane, the bearer JWT on the router.
func (f *Frontend) authenticate(ctx context.Context, method string) (context.Context, error) {
	md, _ := metadata.FromIncomingContext(ctx)
	get := func(k string) string {
		if v := md.Get(k); len(v) > 0 {
			return v[0]
		}
		return ""
	}
	switch {
	case strings.HasPrefix(method, routerService):
		tok, ok := strings.CutPrefix(get("authorization"), "Bearer ")
		if !ok {
			return nil, status.Error(codes.Unauthenticated, "missing the command router's bearer token")
		}
		c, err := f.verifyJWT(tok)
		if err != nil {
			return nil, status.Error(codes.Unauthenticated, err.Error())
		}
		return context.WithValue(ctx, callerKey{}, c), nil
	case strings.HasPrefix(method, controlService):
		secret := get("x-modal-token-secret")
		admin, ok := false, false
		if secret != "" && f.opts.CheckKey != nil {
			admin, ok = f.opts.CheckKey(secret)
		}
		if !ok {
			return nil, status.Error(codes.Unauthenticated, "invalid token: set MODAL_TOKEN_SECRET to this daemon's root token or one of its API keys (wispd keys), and MODAL_TOKEN_ID to anything")
		}
		if !admin && !readOnly[strings.TrimPrefix(method, controlService)] {
			return nil, status.Error(codes.PermissionDenied, "this API key is read-only")
		}
		return context.WithValue(ctx, callerKey{}, caller{admin: admin}), nil
	}
	return nil, status.Error(codes.Unimplemented, "unknown service")
}

// readOnly are the control-plane calls a read key may make.
var readOnly = map[string]bool{
	"EnvironmentGetOrCreate": true, "AuthTokenGet": true,
	"SandboxWait": true, "SandboxWaitV2": true, "SandboxGetTaskId": true, "SandboxGetTaskIdV2": true,
	"SandboxGetCommandRouterAccess": true, "TaskGetCommandRouterAccess": true, "ImageJoinStreaming": true,
}

func (f *Frontend) unaryAuth(ctx context.Context, req any, info *grpc.UnaryServerInfo, h grpc.UnaryHandler) (any, error) {
	ctx, err := f.authenticate(ctx, info.FullMethod)
	if err != nil {
		return nil, err
	}
	return h(ctx, req)
}

type authedStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (s authedStream) Context() context.Context { return s.ctx }

func (f *Frontend) streamAuth(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, h grpc.StreamHandler) error {
	ctx, err := f.authenticate(ss.Context(), info.FullMethod)
	if err != nil {
		return err
	}
	return h(srv, authedStream{ss, ctx})
}
