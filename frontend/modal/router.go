package modal

import (
	"context"
	"errors"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/arugula-salad/wisp/frontend/modal/modalpb"
	"github.com/arugula-salad/wisp/internal/store"
)

// router is the TaskCommandRouter service: exec in a sandbox's task.
type router struct {
	modalpb.UnimplementedTaskCommandRouterServer
	f *Frontend
}

// An execution is one command in a sandbox: the entrypoint or an exec. Its
// output is kept whole, per stream, for as long as the sandbox runs, so a
// reader can start (or resume, after a dropped stream) at any byte offset,
// as the client does.
type execution struct {
	sandbox string
	cancel  context.CancelFunc

	mu      sync.Mutex
	out     [2][]byte     // stdout, stderr
	pipe    [2]bool       // whether each is kept at all (PIPE, not DEVNULL)
	changed chan struct{} // closed and replaced on every append, and at the end
	dropped [2]bool       // a stream past maxKept: everything after is discarded

	done chan struct{} // closed at the end, after code and err are set
	code int32
	err  error // set when the command did not run to an exit (the VM went away, ...)
}

type execKey struct{ task, exec string }

// maxKept is how much of one stream of one command is kept; past it, output
// is dropped (and the client sees it cut short).
const maxKept = 64 << 20

const (
	fdStdout = 0
	fdStderr = 1
)

func (x *execution) write(fd int, b []byte) {
	x.mu.Lock()
	defer x.mu.Unlock()
	if !x.pipe[fd] || x.dropped[fd] {
		return
	}
	if len(x.out[fd])+len(b) > maxKept {
		// Truncated from here on: a later, smaller write must not land after
		// the gap as if nothing were missing.
		x.dropped[fd] = true
		return
	}
	x.out[fd] = append(x.out[fd], b...)
	close(x.changed)
	x.changed = make(chan struct{})
}

func (x *execution) finish(code int32, err error) {
	x.mu.Lock()
	defer x.mu.Unlock()
	x.code, x.err = code, err
	close(x.done) // under the lock: a reader that saw it open will see changed close
	close(x.changed)
	x.changed = make(chan struct{})
}

// execSpec is what to run.
type execSpec struct {
	argv    []string
	workdir string
	env     map[string]string
	timeout time.Duration
	// stdout and stderr say whether each is kept; mergeStderr sends stderr
	// into stdout's buffer.
	stdout, stderr, mergeStderr bool
}

// errSandboxEnded is a command started in a sandbox that has already ended.
var errSandboxEnded = errors.New("the sandbox has finished")

// startExec runs a command in a sandbox until it exits, its timeout passes,
// or the sandbox ends. execID "" is the entrypoint, whose output is not kept.
// An exec ID already in use is not run again: the execution that has it is
// returned, with started false. A sandbox that has ended (whose result is
// recorded, which always comes before stopExecs) gets errSandboxEnded.
func (f *Frontend) startExec(rec store.Record, m meta, spec execSpec, execID string) (x *execution, started bool, err error) {
	ctx, cancel := context.WithCancel(context.Background())
	if spec.timeout > 0 {
		ctx, cancel = context.WithTimeout(context.Background(), spec.timeout)
	}
	x = &execution{sandbox: rec.ID, cancel: cancel, changed: make(chan struct{}), done: make(chan struct{}),
		pipe: [2]bool{spec.stdout, spec.stderr && !spec.mergeStderr}}
	env := map[string]string{}
	for k, v := range m.Env {
		env[k] = v
	}
	for k, v := range spec.env {
		env[k] = v
	}
	spec.env = env
	if spec.workdir == "" {
		spec.workdir = m.Workdir
	}
	// Under runMu, against stopExecs: either this sees the sandbox's result
	// and refuses, or stopExecs (which runs after the result is recorded)
	// sees this execution and stops it.
	f.runMu.Lock()
	ended := false
	f.state.view(func(d *stateData) { sb := d.Sandboxes[rec.ID]; ended = sb == nil || sb.Result != nil })
	if ended {
		f.runMu.Unlock()
		cancel()
		return nil, false, errSandboxEnded
	}
	if execID != "" {
		if prev, loaded := f.execs.LoadOrStore(execKey{m.TaskID, execID}, x); loaded {
			f.runMu.Unlock()
			cancel()
			return prev.(*execution), false, nil
		}
	}
	if f.running[rec.ID] == nil {
		f.running[rec.ID] = map[*execution]struct{}{}
	}
	f.running[rec.ID][x] = struct{}{}
	f.runMu.Unlock()
	go func() {
		defer cancel()
		code, err := f.run(ctx, rec, spec, func(fd int, b []byte) {
			if fd == fdStderr && spec.mergeStderr {
				fd = fdStdout
			}
			x.write(fd, b)
		})
		if err != nil && ctx.Err() == context.DeadlineExceeded && spec.timeout > 0 {
			// Modal kills a command at its timeout; the client has given up on it by then.
			code, err = 128+9, nil
		}
		x.finish(code, err)
	}()
	return x, true, nil
}

// stopExecs ends every command in a sandbox that has ended, and forgets them.
func (f *Frontend) stopExecs(sandbox string) {
	task := taskOf(sandbox)
	f.runMu.Lock()
	xs := f.running[sandbox]
	delete(f.running, sandbox)
	f.execs.Range(func(k, _ any) bool {
		if k.(execKey).task == task {
			f.execs.Delete(k)
		}
		return true
	})
	f.runMu.Unlock()
	for x := range xs {
		x.cancel()
	}
}

// forTask checks that the caller's token is for the task asked about, and
// that its sandbox still runs.
func (f *Frontend) forTask(ctx context.Context, task string) (sandboxView, error) {
	if c := callerOf(ctx); c.task != task {
		return sandboxView{}, status.Error(codes.PermissionDenied, "this token is for another task")
	}
	v, err := f.lookup(sandboxOfTask(task))
	if err != nil {
		return v, err
	}
	if v.result != nil {
		return v, status.Errorf(codes.NotFound, "task %s has finished", task)
	}
	return v, nil
}

func (f *Frontend) execution(ctx context.Context, task, execID string) (*execution, error) {
	if _, err := f.forTask(ctx, task); err != nil {
		return nil, err
	}
	x, ok := f.execs.Load(execKey{task, execID})
	if !ok {
		return nil, status.Errorf(codes.NotFound, "exec %s not found", execID)
	}
	return x.(*execution), nil
}

// TaskExecStart starts a command. exec_id is the client's idempotency key:
// a second start with the same one is a no-op.
func (r *router) TaskExecStart(ctx context.Context, req *modalpb.TaskExecStartRequest) (*modalpb.TaskExecStartResponse, error) {
	v, err := r.f.forTask(ctx, req.TaskId)
	if err != nil {
		return nil, err
	}
	if !callerOf(ctx).admin {
		return nil, status.Error(codes.PermissionDenied, "this API key is read-only")
	}
	switch {
	case req.ExecId == "":
		return nil, status.Error(codes.InvalidArgument, "exec_id is required")
	case len(req.CommandArgs) == 0:
		return nil, status.Error(codes.InvalidArgument, "a command is required")
	case req.PtyInfo != nil && req.PtyInfo.Enabled:
		return nil, unsupported("PTY execs")
	case len(req.SecretIds) > 0:
		return nil, unsupported("named secrets (pass env= instead)")
	case req.ContainerId != "":
		return nil, unsupported("sandbox containers")
	}
	spec := execSpec{argv: req.CommandArgs, env: req.Env, workdir: req.GetWorkdir(),
		stdout:      req.StdoutConfig == modalpb.TaskExecStdoutConfig_TASK_EXEC_STDOUT_CONFIG_PIPE,
		stderr:      req.StderrConfig != modalpb.TaskExecStderrConfig_TASK_EXEC_STDERR_CONFIG_DEVNULL,
		mergeStderr: req.StderrConfig == modalpb.TaskExecStderrConfig_TASK_EXEC_STDERR_CONFIG_STDOUT}
	if req.TimeoutSecs != nil && *req.TimeoutSecs > 0 {
		spec.timeout = time.Duration(*req.TimeoutSecs) * time.Second
	}
	if _, _, err := r.f.startExec(v.rec, v.m, spec, req.ExecId); err != nil {
		return nil, status.Errorf(codes.NotFound, "task %s has finished", req.TaskId)
	}
	return &modalpb.TaskExecStartResponse{}, nil
}

// readChunk is the most one stdio message carries.
const readChunk = 64 << 10

// TaskExecStdioRead streams one of a command's outputs from a byte offset
// until the command has exited and all of it is sent: the clean end of the
// stream is the client's EOF. A stream that breaks is resumed by the client
// at the offset it got to.
func (r *router) TaskExecStdioRead(req *modalpb.TaskExecStdioReadRequest, s grpc.ServerStreamingServer[modalpb.TaskExecStdioReadResponse]) error {
	x, err := r.f.execution(s.Context(), req.TaskId, req.ExecId)
	if err != nil {
		return err
	}
	fd := fdStdout
	if req.FileDescriptor == modalpb.TaskExecStdioFileDescriptor_TASK_EXEC_STDIO_FILE_DESCRIPTOR_STDERR {
		fd = fdStderr
	}
	off := req.Offset
	for {
		x.mu.Lock()
		buf, changed := x.out[fd], x.changed
		ended := isClosed(x.done)
		x.mu.Unlock()
		if off < uint64(len(buf)) {
			chunk := buf[off:min(uint64(len(buf)), off+readChunk)]
			if err := s.Send(&modalpb.TaskExecStdioReadResponse{Data: chunk}); err != nil {
				return err
			}
			off += uint64(len(chunk))
			continue
		}
		if ended {
			return nil
		}
		select {
		case <-changed:
		case <-s.Context().Done():
			return status.FromContextError(s.Context().Err()).Err()
		}
	}
}

func isClosed(c chan struct{}) bool {
	select {
	case <-c:
		return true
	default:
		return false
	}
}

// exitStatus is a finished command's code, or the error a client should get
// for one that never reached an exit.
func exitStatus(x *execution) (int32, error) {
	if x.err != nil {
		return 0, status.Errorf(codes.FailedPrecondition, "the command did not run to an exit: %v", x.err)
	}
	return x.code, nil
}

// TaskExecWait answers when the command has exited. The client calls with a
// 60 s deadline and calls again when it passes.
func (r *router) TaskExecWait(ctx context.Context, req *modalpb.TaskExecWaitRequest) (*modalpb.TaskExecWaitResponse, error) {
	x, err := r.f.execution(ctx, req.TaskId, req.ExecId)
	if err != nil {
		return nil, err
	}
	select {
	case <-x.done:
	case <-ctx.Done():
		return nil, status.FromContextError(ctx.Err()).Err()
	}
	code, err := exitStatus(x)
	if err != nil {
		return nil, err
	}
	return &modalpb.TaskExecWaitResponse{ExitStatus: &modalpb.TaskExecWaitResponse_Code{Code: code}}, nil
}

// TaskExecPoll is TaskExecWait without the wait: no exit status while it runs.
func (r *router) TaskExecPoll(ctx context.Context, req *modalpb.TaskExecPollRequest) (*modalpb.TaskExecPollResponse, error) {
	x, err := r.f.execution(ctx, req.TaskId, req.ExecId)
	if err != nil {
		return nil, err
	}
	if !isClosed(x.done) {
		return &modalpb.TaskExecPollResponse{}, nil
	}
	code, err := exitStatus(x)
	if err != nil {
		return nil, err
	}
	return &modalpb.TaskExecPollResponse{ExitStatus: &modalpb.TaskExecPollResponse_Code{Code: code}}, nil
}
