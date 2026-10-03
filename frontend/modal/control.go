package modal

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/arugula-salad/wisp/engine"
	"github.com/arugula-salad/wisp/frontend/modal/modalpb"
	"github.com/arugula-salad/wisp/internal/store"
)

// control is the ModalClient service: the RPCs of docs/providers/modal.md
// section 3, V2 and V1. Every other is UNIMPLEMENTED.
type control struct {
	modalpb.UnimplementedModalClientServer
	f *Frontend
}

const defaultEnvironment = "main"

func envName(n string) string {
	if n == "" {
		return defaultEnvironment
	}
	return n
}

// Apps: names in an environment, with IDs. Nothing runs in them; a sandbox
// belongs to one.

func (c *control) AppGetOrCreate(ctx context.Context, req *modalpb.AppGetOrCreateRequest) (*modalpb.AppGetOrCreateResponse, error) {
	if req.AppName == "" {
		return nil, status.Error(codes.InvalidArgument, "app name is required")
	}
	env := envName(req.EnvironmentName)
	var id string
	err := c.f.state.update(func(d *stateData) error {
		apps := d.Apps[env]
		existing, ok := apps[req.AppName]
		switch req.ObjectCreationType {
		case modalpb.ObjectCreationType_OBJECT_CREATION_TYPE_UNSPECIFIED:
			if !ok {
				return status.Errorf(codes.NotFound, "App '%s' not found in environment '%s'", req.AppName, env)
			}
		case modalpb.ObjectCreationType_OBJECT_CREATION_TYPE_CREATE_IF_MISSING:
		case modalpb.ObjectCreationType_OBJECT_CREATION_TYPE_CREATE_FAIL_IF_EXISTS:
			if ok {
				return status.Errorf(codes.AlreadyExists, "App '%s' already exists in environment '%s'", req.AppName, env)
			}
		default:
			return status.Errorf(codes.Unimplemented, "this server does not implement object creation type %s", req.ObjectCreationType)
		}
		if ok {
			id = existing
			return nil
		}
		id = randID("ap-", 22)
		if apps == nil {
			apps = map[string]string{}
			d.Apps[env] = apps
		}
		apps[req.AppName] = id
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &modalpb.AppGetOrCreateResponse{AppId: id,
		HandleMetadata: &modalpb.AppHandleMetadata{AppId: id, EnvironmentName: env, Description: req.AppName}}, nil
}

// appExists is whether id is an app.
func (f *Frontend) appExists(id string) bool {
	found := false
	f.state.view(func(d *stateData) {
		for _, apps := range d.Apps {
			for _, a := range apps {
				found = found || a == id
			}
		}
	})
	return found
}

// EnvironmentGetOrCreate is how the client learns the image builder version.
// There is one environment, whatever it is called.
func (c *control) EnvironmentGetOrCreate(ctx context.Context, req *modalpb.EnvironmentGetOrCreateRequest) (*modalpb.EnvironmentGetOrCreateResponse, error) {
	return &modalpb.EnvironmentGetOrCreateResponse{EnvironmentId: "en-wisp",
		Metadata: &modalpb.EnvironmentMetadata{Name: envName(req.DeploymentName),
			Settings: &modalpb.EnvironmentSettings{ImageBuilderVersion: BuilderVersion}}}, nil
}

func (c *control) AuthTokenGet(ctx context.Context, req *modalpb.AuthTokenGetRequest) (*modalpb.AuthTokenGetResponse, error) {
	return &modalpb.AuthTokenGetResponse{Token: c.f.signJWT(claims{Admin: callerOf(ctx).admin})}, nil
}

// Images. Nothing is built: a recipe this server recognizes is answered
// "built" at once, standing for a guest image built ahead of time.

// diskModal is the one guest image there is, images/modal.
const diskModal = "modal"

var (
	debianSlimFrom = regexp.MustCompile(`^FROM python:3\.\d+\.\d+-slim-(bookworm|bullseye)$`)
	// debianSlimSteps are the rest of what Image.debian_slim() sends, on the
	// builders the client supports (_image.py, debian_slim); any order, each optional.
	debianSlimSteps = map[string]bool{
		"RUN apt-get update": true,
		"RUN apt-get install -y gcc gfortran build-essential":                                true,
		"RUN pip install --upgrade pip wheel uv":                                             true,
		"RUN pip install --upgrade pip":                                                      true,
		"RUN echo 'debconf debconf/frontend select Noninteractive' | debconf-set-selections": true,
		`CMD ["sleep", "172800"]`:                                                            true,
	}
)

// recipe names the guest image an image definition stands for, or says why
// there is none.
func recipe(img *modalpb.Image) (string, error) {
	if img == nil || len(img.DockerfileCommands) == 0 {
		return "", errors.New("an image with no Dockerfile commands")
	}
	switch {
	case len(img.BaseImages) > 0:
		return "", errors.New("an image layered on another (.pip_install, .run_commands, ...)")
	case len(img.ContextFiles) > 0 || img.ContextMountId != "":
		return "", errors.New("an image with build context files (.add_local_*, .copy_*)")
	case img.ImageRegistryConfig != nil && img.ImageRegistryConfig.RegistryAuthType != 0:
		return "", errors.New("an image from a private registry")
	case len(img.SecretIds) > 0:
		return "", errors.New("an image built with secrets")
	case img.BuildFunction != nil || img.BuildFunctionDef != "":
		return "", errors.New("an image built by a function (.run_function)")
	}
	cmds := img.DockerfileCommands
	if !debianSlimFrom.MatchString(cmds[0]) {
		return "", fmt.Errorf("an image %q", cmds[0])
	}
	for _, c := range cmds[1:] {
		if !debianSlimSteps[strings.TrimSpace(c)] {
			return "", fmt.Errorf("the build step %q", c)
		}
	}
	return diskModal, nil
}

func imageID(img *modalpb.Image, builder string) string {
	h := sha256.New()
	fmt.Fprintf(h, "%s\n", builder)
	for _, c := range img.DockerfileCommands {
		fmt.Fprintf(h, "%s\n", c)
	}
	return "im-" + hex.EncodeToString(h.Sum(nil))[:22]
}

var imageBuilt = &modalpb.GenericResult{Status: modalpb.GenericResult_GENERIC_STATUS_SUCCESS}

func (c *control) ImageGetOrCreate(ctx context.Context, req *modalpb.ImageGetOrCreateRequest) (*modalpb.ImageGetOrCreateResponse, error) {
	disk, err := recipe(req.Image)
	if err != nil {
		return nil, status.Errorf(codes.FailedPrecondition, "this Modal-compatible server cannot build %v: it serves Image.debian_slim() only, prebuilt (docs/providers/modal-differences.md)", err)
	}
	id := imageID(req.Image, req.BuilderVersion)
	if err := c.f.state.update(func(d *stateData) error { d.Images[id] = disk; return nil }); err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &modalpb.ImageGetOrCreateResponse{ImageId: id, Result: imageBuilt,
		Metadata: &modalpb.ImageMetadata{ImageBuilderVersion: ptr(BuilderVersion)}}, nil
}

// ImageJoinStreaming is the build log of an image ImageGetOrCreate answered
// built, so the client never asks; it is here for one that does.
func (c *control) ImageJoinStreaming(req *modalpb.ImageJoinStreamingRequest, s grpc.ServerStreamingServer[modalpb.ImageJoinStreamingResponse]) error {
	ok := false
	c.f.state.view(func(d *stateData) { _, ok = d.Images[req.ImageId] })
	if !ok {
		return status.Errorf(codes.NotFound, "Image %s not found", req.ImageId)
	}
	return s.Send(&modalpb.ImageJoinStreamingResponse{Result: imageBuilt, Eof: true,
		Metadata: &modalpb.ImageMetadata{ImageBuilderVersion: ptr(BuilderVersion)}})
}

func ptr[T any](v T) *T { return &v }

// Sandboxes.

func (c *control) SandboxCreateV2(ctx context.Context, req *modalpb.SandboxCreateV2Request) (*modalpb.SandboxCreateV2Response, error) {
	var env map[string]string
	if req.EphemeralSecrets != nil {
		env = req.EphemeralSecrets.Contents
	}
	if len(req.CloudBucketMountCredentials) > 0 {
		return nil, unsupported("cloud bucket mounts")
	}
	rec, m, err := c.f.create(ctx, req.AppId, req.Definition, env, req.Tags, true)
	if err != nil {
		return nil, err
	}
	return &modalpb.SandboxCreateV2Response{SandboxId: rec.ID, TaskId: m.TaskID,
		Metadata:            &modalpb.SandboxHandleMetadata{AppId: m.AppID},
		CommandRouterAccess: &modalpb.CommandRouterAccess{Url: c.f.opts.RouterURL, Jwt: c.f.routerJWT(ctx, m.TaskID)}}, nil
}

func (c *control) SandboxCreate(ctx context.Context, req *modalpb.SandboxCreateRequest) (*modalpb.SandboxCreateResponse, error) {
	rec, m, err := c.f.create(ctx, req.AppId, req.Definition, nil, req.Tags, false)
	if err != nil {
		return nil, err
	}
	return &modalpb.SandboxCreateResponse{SandboxId: rec.ID, Metadata: &modalpb.SandboxHandleMetadata{AppId: m.AppID}}, nil
}

func unsupported(what string) error {
	return status.Errorf(codes.Unimplemented, "this Modal-compatible server does not support %s (docs/providers/modal-differences.md)", what)
}

// create makes and boots a sandbox, and starts its entrypoint.
func (f *Frontend) create(ctx context.Context, appID string, def *modalpb.Sandbox, env map[string]string, tags []*modalpb.SandboxTag, v2 bool) (store.Record, meta, error) {
	if def == nil {
		return store.Record{}, meta{}, status.Error(codes.InvalidArgument, "a sandbox definition is required")
	}
	if !f.appExists(appID) {
		return store.Record{}, meta{}, status.Errorf(codes.NotFound, "App %s not found", appID)
	}
	switch {
	case def.PtyInfo != nil && def.PtyInfo.Enabled:
		return store.Record{}, meta{}, unsupported("PTY sandboxes")
	case def.Resources != nil && def.Resources.GpuConfig != nil && def.Resources.GpuConfig.Count > 0:
		return store.Record{}, meta{}, unsupported("GPUs")
	case len(def.MountIds) > 0:
		return store.Record{}, meta{}, unsupported("mounts")
	case len(def.SecretIds) > 0:
		return store.Record{}, meta{}, unsupported("named secrets (pass env= instead)")
	case len(def.VolumeMounts) > 0 || len(def.NfsMounts) > 0 || len(def.S3Mounts) > 0 || len(def.CloudBucketMounts) > 0:
		return store.Record{}, meta{}, unsupported("volumes and bucket mounts")
	case def.GetOpenPorts() != nil && len(def.GetOpenPorts().Ports) > 0:
		return store.Record{}, meta{}, unsupported("open ports and tunnels")
	case def.Name != nil && *def.Name != "":
		return store.Record{}, meta{}, unsupported("named sandboxes")
	case len(tags) > 0:
		return store.Record{}, meta{}, unsupported("sandbox tags")
	}
	var disk string
	f.state.view(func(d *stateData) { disk = d.Images[def.ImageId] })
	if disk != diskModal {
		return store.Record{}, meta{}, status.Errorf(codes.NotFound, "Image %s not found", def.ImageId)
	}
	timeout := defaultTimeout
	if def.TimeoutSecs > 0 {
		timeout = time.Duration(def.TimeoutSecs) * time.Second
	}
	if timeout > f.opts.MaxTimeout {
		return store.Record{}, meta{}, status.Errorf(codes.InvalidArgument, "timeout must be at most %s", f.opts.MaxTimeout)
	}
	if limit, n := f.opts.MaxSandboxes, f.store.Count(); limit > 0 && n >= limit {
		return store.Record{}, meta{}, status.Errorf(codes.ResourceExhausted, "this server allows at most %d sandboxes", limit)
	}
	cfg := store.Config{CPUs: f.opts.CPUs, RamMB: f.opts.MemMiB}
	if r := def.Resources; r != nil {
		if r.MilliCpu > 0 {
			cfg.CPUs = int(r.MilliCpu+999) / 1000
		}
		if r.MemoryMb > 0 {
			cfg.RamMB = int(r.MemoryMb)
		}
	}
	now := time.Now().UTC()
	id := newSandboxID(v2)
	m := meta{AppID: appID, TaskID: taskOf(id), ImageID: def.ImageId, Entrypoint: def.EntrypointArgs,
		Env: env, Deadline: now.Add(timeout)}
	if def.Workdir != nil {
		m.Workdir = *def.Workdir
	}
	end := m.Deadline
	sp := store.Sprite{Record: store.Record{ID: id, API: API, Hostname: hostname, CreatedAt: now, UpdatedAt: now, Config: cfg,
		// A Modal sandbox runs until its entrypoint exits or its timeout, and
		// is never suspended for being idle.
		Lifecycle: &store.LifecyclePolicy{IdleAction: store.IdleNone, DeadlineAction: store.DeadlineDelete},
		ExpiresAt: &end}}
	setMeta(&sp.Record, m)
	if err := f.state.update(func(d *stateData) error {
		d.Sandboxes[id] = &sandboxState{AppID: appID, Deadline: m.Deadline}
		return nil
	}); err != nil {
		return store.Record{}, meta{}, status.Error(codes.Internal, err.Error())
	}
	fail := func(err error) (store.Record, meta, error) {
		f.state.update(func(d *stateData) error { delete(d.Sandboxes, id); return nil })
		return store.Record{}, meta{}, err
	}
	created, err := f.life.Create(ctx, engine.CreateSpec{Sprite: sp, ImageDisk: f.opts.Disk})
	if errors.Is(err, engine.ErrNoRoom) {
		return fail(status.Error(codes.ResourceExhausted, err.Error()))
	}
	if err != nil {
		f.log.Error("create failed", "err", err)
		return fail(status.Error(codes.Internal, "failed to create sandbox: "+err.Error()))
	}
	// Hosted create returns with the sandbox scheduled: boot it now.
	_, release, err := f.acquire(ctx, created.Record)
	if err != nil {
		f.life.Delete(created.Record)
		var lim *engine.LimitError
		if errors.As(err, &lim) {
			return fail(status.Error(codes.ResourceExhausted, lim.Message))
		}
		return fail(status.Error(codes.Internal, "failed to start sandbox: "+err.Error()))
	}
	release()
	if len(m.Entrypoint) > 0 {
		f.startEntrypoint(created.Record, m)
	}
	f.log.Info("sandbox created", "id", id, "app", appID, "timeout", timeout, "entrypoint", m.Entrypoint)
	return created.Record, m, nil
}

// startEntrypoint runs the sandbox's command; the sandbox ends with it.
func (f *Frontend) startEntrypoint(rec store.Record, m meta) {
	x := f.startExec(rec, m, execSpec{argv: m.Entrypoint, workdir: m.Workdir}, "")
	go func() {
		<-x.done
		if x.err != nil {
			return // the sandbox went first (terminate, timeout), and says how
		}
		r := result{Status: modalpb.GenericResult_GENERIC_STATUS_SUCCESS, ExitCode: x.code}
		if x.code != 0 {
			r.Status = modalpb.GenericResult_GENERIC_STATUS_FAILURE
		}
		if f.state.finish(rec.ID, r) {
			f.log.Info("sandbox entrypoint exited", "id", rec.ID, "code", x.code)
			f.end(rec.ID)
		}
	}()
}

// sandboxView is a sandbox as Modal sees it.
type sandboxView struct {
	id     string
	appID  string
	result *result // nil while it runs
	rec    store.Record
	m      meta
}

// lookup finds a sandbox, running or finished, and settles a running one
// whose timeout has passed or whose VM is gone.
func (f *Frontend) lookup(id string) (sandboxView, error) {
	var sb sandboxState
	ok := false
	f.state.view(func(d *stateData) {
		if s := d.Sandboxes[id]; s != nil {
			sb, ok = *s, true
		}
	})
	if !ok {
		return sandboxView{}, status.Errorf(codes.NotFound, "Sandbox %s not found", id)
	}
	v := sandboxView{id: id, appID: sb.AppID, result: sb.Result}
	if v.result != nil {
		return v, nil
	}
	rec, err := f.store.GetRecord(id)
	m, isModal := metaOf(rec)
	switch now := time.Now(); {
	case !now.Before(sb.Deadline):
		f.state.finish(id, result{Status: modalpb.GenericResult_GENERIC_STATUS_TIMEOUT, Exception: "Sandbox timed out"})
		f.end(id)
	case err != nil || !isModal:
		f.state.finish(id, result{Status: modalpb.GenericResult_GENERIC_STATUS_TERMINATED, Exception: "Sandbox was deleted"})
		f.end(id)
	default:
		v.rec, v.m = rec, m
		return v, nil
	}
	f.state.view(func(d *stateData) { v.result = d.Sandboxes[id].Result })
	return v, nil
}

// running finds a sandbox that is still running.
func (f *Frontend) runningSandbox(id string) (sandboxView, error) {
	v, err := f.lookup(id)
	if err != nil {
		return v, err
	}
	if v.result != nil {
		return v, status.Errorf(codes.FailedPrecondition, "Sandbox %s has already finished", id)
	}
	return v, nil
}

// end stops a finished sandbox's processes and deletes its VM.
func (f *Frontend) end(id string) {
	f.stopExecs(id)
	if rec, err := f.store.GetRecord(id); err == nil {
		if err := f.life.Delete(rec); err != nil && !errors.Is(err, store.ErrNotFound) {
			f.log.Error("deleting a finished sandbox", "id", id, "err", err)
		}
	}
}

func (c *control) SandboxGetTaskIdV2(ctx context.Context, req *modalpb.SandboxGetTaskIdRequest) (*modalpb.SandboxGetTaskIdResponse, error) {
	return c.SandboxGetTaskId(ctx, req)
}

func (c *control) SandboxGetTaskId(ctx context.Context, req *modalpb.SandboxGetTaskIdRequest) (*modalpb.SandboxGetTaskIdResponse, error) {
	v, err := c.f.lookup(req.SandboxId)
	if err != nil {
		return nil, err
	}
	if v.result != nil {
		return &modalpb.SandboxGetTaskIdResponse{TaskResult: v.result.proto()}, nil
	}
	return &modalpb.SandboxGetTaskIdResponse{TaskId: ptr(v.m.TaskID)}, nil
}

// routerJWT is the command router's token for one task.
func (f *Frontend) routerJWT(ctx context.Context, task string) string {
	return f.signJWT(claims{Task: task, Admin: callerOf(ctx).admin})
}

func (c *control) SandboxGetCommandRouterAccess(ctx context.Context, req *modalpb.SandboxGetCommandRouterAccessRequest) (*modalpb.SandboxGetCommandRouterAccessResponse, error) {
	id := req.GetSandboxId()
	if t := req.GetTaskId(); t != "" {
		id = sandboxOfTask(t)
	}
	v, err := c.f.runningSandbox(id)
	if err != nil {
		return nil, err
	}
	return &modalpb.SandboxGetCommandRouterAccessResponse{Url: c.f.opts.RouterURL, Jwt: c.f.routerJWT(ctx, v.m.TaskID)}, nil
}

func (c *control) TaskGetCommandRouterAccess(ctx context.Context, req *modalpb.TaskGetCommandRouterAccessRequest) (*modalpb.TaskGetCommandRouterAccessResponse, error) {
	v, err := c.f.runningSandbox(sandboxOfTask(req.TaskId))
	if err != nil {
		return nil, err
	}
	return &modalpb.TaskGetCommandRouterAccessResponse{Url: c.f.opts.RouterURL, Jwt: c.f.routerJWT(ctx, v.m.TaskID)}, nil
}

func (c *control) SandboxWaitV2(ctx context.Context, req *modalpb.SandboxWaitRequest) (*modalpb.SandboxWaitResponse, error) {
	return c.SandboxWait(ctx, req)
}

// maxWait caps one SandboxWait; the client calls again.
const maxWait = 50 * time.Second

// SandboxWait answers as soon as the sandbox has ended, or after timeout
// seconds (at most maxWait) with an empty result.
func (c *control) SandboxWait(ctx context.Context, req *modalpb.SandboxWaitRequest) (*modalpb.SandboxWaitResponse, error) {
	wait := min(time.Duration(float64(req.Timeout)*float64(time.Second)), maxWait)
	deadline := time.Now().Add(wait)
	for {
		v, err := c.f.lookup(req.SandboxId)
		if err != nil {
			return nil, err
		}
		if v.result != nil || !time.Now().Before(deadline) {
			r := v.result.proto()
			return &modalpb.SandboxWaitResponse{Result: r, Metadata: &modalpb.SandboxHandleMetadata{AppId: v.appID, Result: r}}, nil
		}
		select {
		case <-ctx.Done():
			return nil, status.FromContextError(ctx.Err()).Err()
		case <-time.After(min(200*time.Millisecond, time.Until(deadline))):
		}
	}
}

func (c *control) SandboxTerminateV2(ctx context.Context, req *modalpb.SandboxTerminateRequest) (*modalpb.SandboxTerminateResponse, error) {
	return c.SandboxTerminate(ctx, req)
}

// SandboxTerminate ends a sandbox; one that has already ended keeps its
// result, which is returned.
func (c *control) SandboxTerminate(ctx context.Context, req *modalpb.SandboxTerminateRequest) (*modalpb.SandboxTerminateResponse, error) {
	v, err := c.f.lookup(req.SandboxId)
	if err != nil {
		return nil, err
	}
	if v.result != nil {
		return &modalpb.SandboxTerminateResponse{ExistingResult: v.result.proto()}, nil
	}
	if c.f.state.finish(req.SandboxId, result{Status: modalpb.GenericResult_GENERIC_STATUS_TERMINATED}) {
		c.f.end(req.SandboxId)
		c.f.log.Info("sandbox terminated", "id", req.SandboxId)
		return &modalpb.SandboxTerminateResponse{}, nil
	}
	v, _ = c.f.lookup(req.SandboxId)
	return &modalpb.SandboxTerminateResponse{ExistingResult: v.result.proto()}, nil
}
