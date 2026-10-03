package modal

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"

	"github.com/arugula-salad/wisp/engine"
	"github.com/arugula-salad/wisp/internal/store"
)

// A runner runs one command in a sandbox's VM to its exit, handing its output
// to out as it comes (fd 0 stdout, 1 stderr), and returns its exit code; err
// is for a command that did not run to an exit. Cancelling ctx kills it.
type runner func(ctx context.Context, rec store.Record, spec execSpec, out func(fd int, b []byte)) (code int32, err error)

// baseEnv is set for every command, under the sandbox's and the exec's own:
// what the debian_slim image's ENV has that sudo's env_reset drops.
var baseEnv = map[string]string{"LANG": "C.UTF-8"}

// command is the argv wisp-agent runs. Its exec sessions run as the guest's
// `sprite` user; Modal's run as root, in the container's environment, so the
// command goes through sudo (which resets the environment to root's: HOME,
// PATH with /usr/local/bin, ...) and env, which sets the variables and the
// working directory (the agent's own dir would be entered as `sprite`, who
// cannot enter /root).
func command(spec execSpec) []string {
	dir := spec.workdir
	if dir == "" {
		dir = "/"
	}
	argv := []string{"sudo", "-n", "-H", "--", "env", "-C", dir}
	env := map[string]string{}
	for k, v := range baseEnv {
		env[k] = v
	}
	for k, v := range spec.env {
		env[k] = v
	}
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		argv = append(argv, k+"="+env[k])
	}
	return append(argv, spec.argv...)
}

// agentExec runs a command through wisp-agent's HTTP exec (POST /exec, the
// one the Sprites API's exec uses), holding the VM up while it runs.
func (f *Frontend) agentExec(ctx context.Context, rec store.Record, spec execSpec, out func(int, []byte)) (int32, error) {
	m, release, err := f.acquire(ctx, rec)
	if err != nil {
		return 0, err
	}
	defer release()
	q := url.Values{"cmd": command(spec), "dir": {"/"}, "stdin": {"false"}, "framing": {"length"}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://agent/exec?"+q.Encode(), http.NoBody)
	if err != nil {
		return 0, err
	}
	resp, err := engine.AgentTransport(m).RoundTrip(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return 0, fmt.Errorf("wisp-agent refused the command: %s: %s", resp.Status, b)
	}
	// Frames: stream ID, big-endian uint32 length, payload. Stdout is 1,
	// stderr 2, and 3 is the exit, whose payload is the code (internal/agent).
	br := bufio.NewReader(resp.Body)
	head := make([]byte, 5)
	for {
		if _, err := io.ReadFull(br, head); err != nil {
			return 0, errors.New("the command's output ended before its exit (the VM went away?)")
		}
		payload := make([]byte, binary.BigEndian.Uint32(head[1:]))
		if _, err := io.ReadFull(br, payload); err != nil {
			return 0, errors.New("the command's output ended before its exit (the VM went away?)")
		}
		switch head[0] {
		case 1:
			out(fdStdout, payload)
		case 2:
			out(fdStderr, payload)
		case 3:
			if len(payload) == 0 {
				return 0, errors.New("an exit frame with no code")
			}
			return int32(payload[0]), nil
		}
	}
}
