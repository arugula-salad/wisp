package modal

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/arugula-salad/wisp/engine"
	"github.com/arugula-salad/wisp/frontend/modal/modalpb"
	"github.com/arugula-salad/wisp/internal/store"
	"github.com/arugula-salad/wisp/internal/vmm"
)

const (
	adminKey = "admin-key"
	readKey  = "read-key"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

// debianSlim is what modal 1.6.0's Image.debian_slim() sends on builder
// 2025.06 from a Python 3.14 (docs/providers/modal.md section 4).
var debianSlim = []string{
	"FROM python:3.14.2-slim-bookworm",
	"RUN apt-get update",
	"RUN apt-get install -y gcc gfortran build-essential",
	"RUN pip install --upgrade pip wheel uv",
	"RUN echo 'debconf debconf/frontend select Noninteractive' | debconf-set-selections",
	`CMD ["sleep", "172800"]`,
}

// fixture is the front end on a real engine and store with no VMs, served
// over h2c as the daemon serves it, and a gRPC client of it. Commands run in
// fakeRun.
type fixture struct {
	t       *testing.T
	f       *Frontend
	life    *engine.Engine
	st      *store.Store
	control modalpb.ModalClientClient
	conn    *grpc.ClientConn
	dir     string

	mu   sync.Mutex
	ran  [][]string
	gate chan struct{} // "wait" commands block on it
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	dir := t.TempDir()
	base := filepath.Join(dir, "base.ext4")
	disk := filepath.Join(dir, "modal.ext4")
	for _, p := range []string{base, disk} {
		if err := os.WriteFile(p, bytes.Repeat([]byte("disk"), 4096), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	life := engine.New(engine.Options{DataDir: dir, BaseImage: base, NoNetwork: true}, st, quiet)
	life.StartReaping()
	t.Cleanup(life.Shutdown)
	fx := &fixture{t: t, life: life, st: st, dir: dir, gate: make(chan struct{})}
	fx.start(disk)
	return fx
}

// start (re)makes the front end, as a daemon restart does.
func (fx *fixture) start(disk string) {
	t := fx.t
	srv := httptest.NewUnstartedServer(nil)
	srv.Config.Protocols = new(http.Protocols)
	srv.Config.Protocols.SetUnencryptedHTTP2(true)
	srv.Start()
	t.Cleanup(srv.Close)
	f, err := New(Options{Disk: disk, StateFile: filepath.Join(fx.dir, "modal", "state.json"), RouterURL: srv.URL,
		CheckKey: func(k string) (bool, bool) {
			switch k {
			case adminKey:
				return true, true
			case readKey:
				return false, true
			}
			return false, false
		}}, fx.st, fx.life, quiet)
	if err != nil {
		t.Fatal(err)
	}
	f.acquire = func(ctx context.Context, rec store.Record) (*vmm.Machine, func(), error) {
		return nil, func() {}, nil
	}
	f.run = fx.fakeRun
	srv.Config.Handler = f.Handler()
	conn, err := grpc.NewClient(strings.TrimPrefix(srv.URL, "http://"), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	fx.f, fx.conn, fx.control = f, conn, modalpb.NewModalClientClient(conn)
}

// fakeRun stands in for a VM: echo, err (stderr, exit 3), exit N, big N
// (N bytes of output), wait (until the gate opens), sleep (until killed).
func (fx *fixture) fakeRun(ctx context.Context, rec store.Record, spec execSpec, out func(int, []byte)) (int32, error) {
	fx.mu.Lock()
	fx.ran = append(fx.ran, command(spec))
	fx.mu.Unlock()
	switch argv := spec.argv; argv[0] {
	case "echo":
		out(fdStdout, []byte(strings.Join(argv[1:], " ")+"\n"))
	case "err":
		out(fdStdout, []byte("out\n"))
		out(fdStderr, []byte("oops\n"))
		return 3, nil
	case "exit":
		n, _ := strconv.Atoi(argv[1])
		return int32(n), nil
	case "big":
		n, _ := strconv.Atoi(argv[1])
		for i := 0; i < n; i += 1000 {
			out(fdStdout, bytes.Repeat([]byte{'a' + byte(i/1000%26)}, min(1000, n-i)))
		}
	case "wait":
		out(fdStdout, []byte("before\n"))
		select {
		case <-fx.gate:
		case <-ctx.Done():
			return 0, ctx.Err()
		}
		out(fdStdout, []byte("after\n"))
	case "sleep":
		<-ctx.Done()
		return 0, ctx.Err()
	default:
		return 127, nil
	}
	return 0, nil
}

// ctxWith is an outgoing context with these headers. Every call in these
// tests is quick: the per-test client's own deadline stands in for a timeout.
func ctxWith(kv ...string) context.Context {
	return metadata.AppendToOutgoingContext(context.Background(), kv...)
}

func admin() context.Context {
	return ctxWith("x-modal-token-id", "ak-anything", "x-modal-token-secret", adminKey)
}

func wantCode(t *testing.T, err error, c codes.Code) {
	t.Helper()
	if status.Code(err) != c {
		t.Fatalf("got %v, want %s", err, c)
	}
}

// sandbox is one created through the API, with a router client for its task.
type sandbox struct {
	id, task, jwt string
	router        modalpb.TaskCommandRouterClient
}

func (fx *fixture) app() string {
	r, err := fx.control.AppGetOrCreate(admin(), &modalpb.AppGetOrCreateRequest{AppName: "wisp-spike",
		ObjectCreationType: modalpb.ObjectCreationType_OBJECT_CREATION_TYPE_CREATE_IF_MISSING})
	if err != nil {
		fx.t.Fatal(err)
	}
	return r.AppId
}

func (fx *fixture) image() string {
	r, err := fx.control.ImageGetOrCreate(admin(), &modalpb.ImageGetOrCreateRequest{Image: &modalpb.Image{DockerfileCommands: debianSlim},
		BuilderVersion: BuilderVersion})
	if err != nil {
		fx.t.Fatal(err)
	}
	return r.ImageId
}

func (fx *fixture) create(def *modalpb.Sandbox) sandbox {
	fx.t.Helper()
	if def.ImageId == "" {
		def.ImageId = fx.image()
	}
	r, err := fx.control.SandboxCreateV2(admin(), &modalpb.SandboxCreateV2Request{AppId: fx.app(), Definition: def})
	if err != nil {
		fx.t.Fatal(err)
	}
	if r.CommandRouterAccess == nil || r.CommandRouterAccess.Url == "" {
		fx.t.Fatalf("no command router access in %v", r)
	}
	return sandbox{id: r.SandboxId, task: r.TaskId, jwt: r.CommandRouterAccess.Jwt, router: modalpb.NewTaskCommandRouterClient(fx.conn)}
}

func (sb sandbox) ctx() context.Context { return ctxWith("authorization", "Bearer "+sb.jwt) }

func (sb sandbox) exec(t *testing.T, id string, args ...string) {
	t.Helper()
	_, err := sb.router.TaskExecStart(sb.ctx(), &modalpb.TaskExecStartRequest{TaskId: sb.task, ExecId: id, CommandArgs: args,
		StdoutConfig: modalpb.TaskExecStdoutConfig_TASK_EXEC_STDOUT_CONFIG_PIPE,
		StderrConfig: modalpb.TaskExecStderrConfig_TASK_EXEC_STDERR_CONFIG_PIPE})
	if err != nil {
		t.Fatal(err)
	}
}

func (sb sandbox) read(t *testing.T, id string, fd modalpb.TaskExecStdioFileDescriptor, off uint64) (string, int) {
	t.Helper()
	s, err := sb.router.TaskExecStdioRead(sb.ctx(), &modalpb.TaskExecStdioReadRequest{TaskId: sb.task, ExecId: id, Offset: off, FileDescriptor: fd})
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	n := 0
	for {
		m, err := s.Recv()
		if err == io.EOF {
			return b.String(), n
		}
		if err != nil {
			t.Fatal(err)
		}
		b.Write(m.Data)
		n++
	}
}

func (sb sandbox) wait(t *testing.T, id string) int32 {
	t.Helper()
	r, err := sb.router.TaskExecWait(sb.ctx(), &modalpb.TaskExecWaitRequest{TaskId: sb.task, ExecId: id})
	if err != nil {
		t.Fatal(err)
	}
	return r.GetCode()
}

const (
	stdout = modalpb.TaskExecStdioFileDescriptor_TASK_EXEC_STDIO_FILE_DESCRIPTOR_STDOUT
	stderr = modalpb.TaskExecStdioFileDescriptor_TASK_EXEC_STDIO_FILE_DESCRIPTOR_STDERR
)

func TestAuth(t *testing.T) {
	fx := newFixture(t)
	req := &modalpb.AppGetOrCreateRequest{AppName: "a", ObjectCreationType: modalpb.ObjectCreationType_OBJECT_CREATION_TYPE_CREATE_IF_MISSING}
	_, err := fx.control.AppGetOrCreate(ctxWith(), req)
	wantCode(t, err, codes.Unauthenticated)
	_, err = fx.control.AppGetOrCreate(ctxWith("x-modal-token-secret", "wrong"), req)
	wantCode(t, err, codes.Unauthenticated)
	_, err = fx.control.AppGetOrCreate(ctxWith("x-modal-token-secret", readKey), req)
	wantCode(t, err, codes.PermissionDenied)
	if _, err := fx.control.EnvironmentGetOrCreate(ctxWith("x-modal-token-secret", readKey), &modalpb.EnvironmentGetOrCreateRequest{}); err != nil {
		t.Fatalf("a read key may look: %v", err)
	}

	sb := fx.create(&modalpb.Sandbox{EntrypointArgs: []string{"sleep", "infinity"}})
	exec := &modalpb.TaskExecStartRequest{TaskId: sb.task, ExecId: "x", CommandArgs: []string{"echo"}}
	_, err = sb.router.TaskExecStart(ctxWith(), exec)
	wantCode(t, err, codes.Unauthenticated)
	_, err = sb.router.TaskExecStart(ctxWith("authorization", "Bearer "+sb.jwt+"x"), exec)
	wantCode(t, err, codes.Unauthenticated)
	// A token is for one task.
	other := fx.create(&modalpb.Sandbox{})
	_, err = sb.router.TaskExecStart(other.ctx(), exec)
	wantCode(t, err, codes.PermissionDenied)
	// A token from before a restart is refused; the client fetches a new one.
	fx.start(fx.f.opts.Disk)
	sb.router = modalpb.NewTaskCommandRouterClient(fx.conn)
	_, err = sb.router.TaskExecStart(sb.ctx(), exec)
	wantCode(t, err, codes.Unauthenticated)
	acc, err := fx.control.SandboxGetCommandRouterAccess(admin(), &modalpb.SandboxGetCommandRouterAccessRequest{
		Target: &modalpb.SandboxGetCommandRouterAccessRequest_SandboxId{SandboxId: sb.id}})
	if err != nil {
		t.Fatal(err)
	}
	sb.jwt = acc.Jwt
	sb.exec(t, "x", "echo")
}

func TestAppsAndEnvironment(t *testing.T) {
	fx := newFixture(t)
	_, err := fx.control.AppGetOrCreate(admin(), &modalpb.AppGetOrCreateRequest{AppName: "wisp-spike"})
	wantCode(t, err, codes.NotFound)
	id := fx.app()
	if !strings.HasPrefix(id, "ap-") || fx.app() != id {
		t.Fatalf("app IDs %q, %q", id, fx.app())
	}
	_, err = fx.control.AppGetOrCreate(admin(), &modalpb.AppGetOrCreateRequest{AppName: "wisp-spike",
		ObjectCreationType: modalpb.ObjectCreationType_OBJECT_CREATION_TYPE_CREATE_FAIL_IF_EXISTS})
	wantCode(t, err, codes.AlreadyExists)
	// Apps are per environment, and survive a restart.
	r, _ := fx.control.AppGetOrCreate(admin(), &modalpb.AppGetOrCreateRequest{AppName: "wisp-spike", EnvironmentName: "dev",
		ObjectCreationType: modalpb.ObjectCreationType_OBJECT_CREATION_TYPE_CREATE_IF_MISSING})
	if r.AppId == id {
		t.Fatal("same app in two environments")
	}
	fx.start(fx.f.opts.Disk)
	if fx.app() != id {
		t.Fatal("the app was forgotten in a restart")
	}

	env, err := fx.control.EnvironmentGetOrCreate(admin(), &modalpb.EnvironmentGetOrCreateRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if env.Metadata.Settings.ImageBuilderVersion != BuilderVersion || env.Metadata.Name != "main" {
		t.Fatalf("environment %v", env)
	}
}

func TestImages(t *testing.T) {
	fx := newFixture(t)
	ctx := admin()
	r, err := fx.control.ImageGetOrCreate(ctx, &modalpb.ImageGetOrCreateRequest{Image: &modalpb.Image{DockerfileCommands: debianSlim}, BuilderVersion: BuilderVersion})
	if err != nil {
		t.Fatal(err)
	}
	if r.Result.Status != modalpb.GenericResult_GENERIC_STATUS_SUCCESS || !strings.HasPrefix(r.ImageId, "im-") {
		t.Fatalf("debian_slim: %v", r)
	}
	// Another local Python asks for another tag, and gets the same guest image.
	other := slices.Clone(debianSlim)
	other[0] = "FROM python:3.12.10-slim-bookworm"
	r2, err := fx.control.ImageGetOrCreate(ctx, &modalpb.ImageGetOrCreateRequest{Image: &modalpb.Image{DockerfileCommands: other}, BuilderVersion: BuilderVersion})
	if err != nil || r2.ImageId == r.ImageId {
		t.Fatalf("python 3.12: %v %v", r2, err)
	}
	s, err := fx.control.ImageJoinStreaming(ctx, &modalpb.ImageJoinStreamingRequest{ImageId: r.ImageId})
	if err != nil {
		t.Fatal(err)
	}
	if m, err := s.Recv(); err != nil || m.Result.Status != modalpb.GenericResult_GENERIC_STATUS_SUCCESS {
		t.Fatalf("join: %v %v", m, err)
	}

	for name, img := range map[string]*modalpb.Image{
		"pip_install":   {DockerfileCommands: []string{"FROM base", "RUN python -m pip install requests"}, BaseImages: []*modalpb.BaseImage{{ImageId: r.ImageId, DockerTag: "base"}}},
		"from_registry": {DockerfileCommands: []string{"FROM alpine:3.20"}},
		"extra step":    {DockerfileCommands: append(slices.Clone(debianSlim), "RUN apt-get install -y git")},
		"empty":         {},
	} {
		_, err := fx.control.ImageGetOrCreate(ctx, &modalpb.ImageGetOrCreateRequest{Image: img, BuilderVersion: BuilderVersion})
		wantCode(t, err, codes.FailedPrecondition)
		if !strings.Contains(status.Convert(err).Message(), "debian_slim() only, prebuilt") {
			t.Errorf("%s: %v", name, err)
		}
	}
	_, err = fx.control.SandboxCreateV2(ctx, &modalpb.SandboxCreateV2Request{AppId: fx.app(), Definition: &modalpb.Sandbox{ImageId: "im-nope"}})
	wantCode(t, err, codes.NotFound)
}

func TestSandboxExec(t *testing.T) {
	fx := newFixture(t)
	sb := fx.create(&modalpb.Sandbox{EntrypointArgs: []string{"sleep", "infinity"}, TimeoutSecs: 300})
	if !isV2(sb.id) || !strings.HasPrefix(sb.id, "sb-") || !strings.HasPrefix(sb.task, "ta-") || !strings.HasSuffix(sb.task, "V") {
		t.Fatalf("V2 IDs: sandbox %q task %q", sb.id, sb.task)
	}
	rec, err := fx.st.GetRecord(sb.id)
	if m, ok := metaOf(rec); err != nil || !ok || rec.API != API || m.TaskID != sb.task {
		t.Fatalf("record %+v %v", rec, err)
	}

	sb.exec(t, "e1", "echo", "hi")
	if got, _ := sb.read(t, "e1", stdout, 0); got != "hi\n" {
		t.Fatalf("stdout %q", got)
	}
	// A reader that reconnects resumes at its offset.
	if got, _ := sb.read(t, "e1", stdout, 1); got != "i\n" {
		t.Fatalf("stdout from 1: %q", got)
	}
	if code := sb.wait(t, "e1"); code != 0 {
		t.Fatalf("exit %d", code)
	}
	// exec_id is an idempotency key.
	sb.exec(t, "e1", "echo", "hi")
	fx.mu.Lock()
	if n := len(fx.ran); n != 2 { // the entrypoint and e1
		t.Fatalf("%d commands ran: %q", n, fx.ran)
	}
	fx.mu.Unlock()

	sb.exec(t, "e2", "err")
	if got, _ := sb.read(t, "e2", stderr, 0); got != "oops\n" {
		t.Fatalf("stderr %q", got)
	}
	if got, _ := sb.read(t, "e2", stdout, 0); got != "out\n" {
		t.Fatalf("stdout %q", got)
	}
	if code := sb.wait(t, "e2"); code != 3 {
		t.Fatalf("exit %d", code)
	}

	// Output streams as it comes, before the exit; poll has no status until then.
	sb.exec(t, "e3", "wait")
	s, err := sb.router.TaskExecStdioRead(sb.ctx(), &modalpb.TaskExecStdioReadRequest{TaskId: sb.task, ExecId: "e3"})
	if err != nil {
		t.Fatal(err)
	}
	if m, err := s.Recv(); err != nil || string(m.Data) != "before\n" {
		t.Fatalf("first chunk %v %v", m, err)
	}
	p, err := sb.router.TaskExecPoll(sb.ctx(), &modalpb.TaskExecPollRequest{TaskId: sb.task, ExecId: "e3"})
	if err != nil || p.ExitStatus != nil {
		t.Fatalf("poll while running: %v %v", p, err)
	}
	close(fx.gate)
	if m, err := s.Recv(); err != nil || string(m.Data) != "after\n" {
		t.Fatalf("second chunk %v %v", m, err)
	}
	if _, err := s.Recv(); err != io.EOF {
		t.Fatalf("want EOF, got %v", err)
	}
	if p, err := sb.router.TaskExecPoll(sb.ctx(), &modalpb.TaskExecPollRequest{TaskId: sb.task, ExecId: "e3"}); err != nil || p.GetCode() != 0 || p.ExitStatus == nil {
		t.Fatalf("poll after: %v %v", p, err)
	}

	// Large output arrives whole, in chunks.
	sb.exec(t, "e4", "big", "300000")
	if got, n := sb.read(t, "e4", stdout, 0); len(got) != 300000 || got[299999] != 'a'+byte(299%26) || n < 2 {
		t.Fatalf("big: %d bytes in %d messages", len(got), n)
	}

	_, err = sb.router.TaskExecWait(sb.ctx(), &modalpb.TaskExecWaitRequest{TaskId: sb.task, ExecId: "nope"})
	wantCode(t, err, codes.NotFound)
	_, err = sb.router.TaskExecStart(sb.ctx(), &modalpb.TaskExecStartRequest{TaskId: sb.task, ExecId: "pty", CommandArgs: []string{"sh"},
		PtyInfo: &modalpb.PTYInfo{Enabled: true}})
	wantCode(t, err, codes.Unimplemented)

	// Terminate: the sandbox's result is TERMINATED, its execs are gone, and
	// it still answers, as from_id and wait need.
	sb.exec(t, "e5", "sleep")
	if _, err := fx.control.SandboxTerminateV2(admin(), &modalpb.SandboxTerminateRequest{SandboxId: sb.id}); err != nil {
		t.Fatal(err)
	}
	w, err := fx.control.SandboxWaitV2(admin(), &modalpb.SandboxWaitRequest{SandboxId: sb.id})
	if err != nil || w.Result.Status != modalpb.GenericResult_GENERIC_STATUS_TERMINATED || w.Metadata.AppId == "" {
		t.Fatalf("wait after terminate: %v %v", w, err)
	}
	if _, err := fx.st.GetRecord(sb.id); err == nil {
		t.Fatal("the record outlived terminate")
	}
	tid, err := fx.control.SandboxGetTaskIdV2(admin(), &modalpb.SandboxGetTaskIdRequest{SandboxId: sb.id})
	if err != nil || tid.TaskId != nil || tid.TaskResult.GetStatus() != modalpb.GenericResult_GENERIC_STATUS_TERMINATED {
		t.Fatalf("task id after terminate: %v %v", tid, err)
	}
	_, err = sb.router.TaskExecWait(sb.ctx(), &modalpb.TaskExecWaitRequest{TaskId: sb.task, ExecId: "e5"})
	wantCode(t, err, codes.NotFound)
	t2, err := fx.control.SandboxTerminateV2(admin(), &modalpb.SandboxTerminateRequest{SandboxId: sb.id})
	if err != nil || t2.ExistingResult.GetStatus() != modalpb.GenericResult_GENERIC_STATUS_TERMINATED {
		t.Fatalf("second terminate: %v %v", t2, err)
	}
	_, err = fx.control.SandboxWaitV2(admin(), &modalpb.SandboxWaitRequest{SandboxId: "sb-nope"})
	wantCode(t, err, codes.NotFound)
}

func TestSandboxEnds(t *testing.T) {
	fx := newFixture(t)
	// The entrypoint's exit ends the sandbox, with its code.
	sb := fx.create(&modalpb.Sandbox{EntrypointArgs: []string{"exit", "7"}})
	w, err := fx.control.SandboxWaitV2(admin(), &modalpb.SandboxWaitRequest{SandboxId: sb.id, Timeout: 5})
	if err != nil || w.Result.Status != modalpb.GenericResult_GENERIC_STATUS_FAILURE || w.Result.Exitcode != 7 {
		t.Fatalf("entrypoint exit: %v %v", w, err)
	}
	sb = fx.create(&modalpb.Sandbox{EntrypointArgs: []string{"echo"}})
	w, _ = fx.control.SandboxWaitV2(admin(), &modalpb.SandboxWaitRequest{SandboxId: sb.id, Timeout: 5})
	if w.Result.Status != modalpb.GenericResult_GENERIC_STATUS_SUCCESS {
		t.Fatalf("entrypoint success: %v", w)
	}

	// The timeout ends it, and a wait answers when it does.
	sb = fx.create(&modalpb.Sandbox{EntrypointArgs: []string{"sleep"}, TimeoutSecs: 1})
	w, err = fx.control.SandboxWaitV2(admin(), &modalpb.SandboxWaitRequest{SandboxId: sb.id, Timeout: 0})
	if err != nil || w.Result.Status != modalpb.GenericResult_GENERIC_STATUS_UNSPECIFIED {
		t.Fatalf("running: %v %v", w, err)
	}
	t0 := time.Now()
	w, err = fx.control.SandboxWaitV2(admin(), &modalpb.SandboxWaitRequest{SandboxId: sb.id, Timeout: 10})
	if err != nil || w.Result.Status != modalpb.GenericResult_GENERIC_STATUS_TIMEOUT || time.Since(t0) > 3*time.Second {
		t.Fatalf("timeout: %v %v after %s", w, err, time.Since(t0))
	}
	if _, err := fx.st.GetRecord(sb.id); err == nil {
		t.Fatal("the record outlived the timeout")
	}
	_, err = fx.control.SandboxCreateV2(admin(), &modalpb.SandboxCreateV2Request{AppId: fx.app(),
		Definition: &modalpb.Sandbox{ImageId: fx.image(), TimeoutSecs: 100 * 3600}})
	wantCode(t, err, codes.InvalidArgument)

	// Results survive a restart.
	fx.start(fx.f.opts.Disk)
	w, err = fx.control.SandboxWaitV2(admin(), &modalpb.SandboxWaitRequest{SandboxId: sb.id})
	if err != nil || w.Result.Status != modalpb.GenericResult_GENERIC_STATUS_TIMEOUT {
		t.Fatalf("after a restart: %v %v", w, err)
	}
}

func TestSandboxV1(t *testing.T) {
	fx := newFixture(t)
	r, err := fx.control.SandboxCreate(admin(), &modalpb.SandboxCreateRequest{AppId: fx.app(), Definition: &modalpb.Sandbox{ImageId: fx.image()}})
	if err != nil {
		t.Fatal(err)
	}
	if isV2(r.SandboxId) || len(r.SandboxId) != 25 {
		t.Fatalf("V1 sandbox ID %q", r.SandboxId)
	}
	tid, err := fx.control.SandboxGetTaskId(admin(), &modalpb.SandboxGetTaskIdRequest{SandboxId: r.SandboxId})
	if err != nil || strings.HasSuffix(tid.GetTaskId(), "V") || sandboxOfTask(tid.GetTaskId()) != r.SandboxId {
		t.Fatalf("V1 task %v %v", tid, err)
	}
	acc, err := fx.control.TaskGetCommandRouterAccess(admin(), &modalpb.TaskGetCommandRouterAccessRequest{TaskId: tid.GetTaskId()})
	if err != nil {
		t.Fatal(err)
	}
	sb := sandbox{id: r.SandboxId, task: tid.GetTaskId(), jwt: acc.Jwt, router: modalpb.NewTaskCommandRouterClient(fx.conn)}
	sb.exec(t, "e", "echo", "v1")
	if got, _ := sb.read(t, "e", stdout, 0); got != "v1\n" {
		t.Fatalf("stdout %q", got)
	}
	if _, err := fx.control.SandboxTerminate(admin(), &modalpb.SandboxTerminateRequest{SandboxId: r.SandboxId}); err != nil {
		t.Fatal(err)
	}
}

func TestCommand(t *testing.T) {
	got := command(execSpec{argv: []string{"echo", "hi"}, env: map[string]string{"FOO": "bar"}})
	want := []string{"sudo", "-n", "-H", "--", "env", "-C", "/", "FOO=bar", "LANG=C.UTF-8", "echo", "hi"}
	if !slices.Equal(got, want) {
		t.Fatalf("got %q, want %q", got, want)
	}
	if got := command(execSpec{argv: []string{"pwd"}, workdir: "/tmp"}); got[6] != "/tmp" {
		t.Fatalf("workdir: %q", got)
	}
}
