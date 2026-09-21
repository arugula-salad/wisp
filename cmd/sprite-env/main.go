// sprite-env is the CLI a sprite uses on itself: it talks to the agent's
// management socket at /.sprite/api.sock, so it needs no API token and cannot
// name any other sprite. Output is the API's own JSON / NDJSON, which is what
// the scripts and agents that call it want to parse.
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

const usage = `sprite-env manages this sprite from the inside.

  sprite-env services create <name> --cmd <path> [--args a,b,c] [--env K=v,...] [--dir <path>]
                                    [--needs svc,...] [--http-port <port>] [--duration 5s] [--no-stream]
  sprite-env services list
  sprite-env services get <name>
  sprite-env services start <name>   [--duration 5s]
  sprite-env services restart <name> [--duration 5s]
  sprite-env services stop <name>    [--timeout 10s]
  sprite-env services signal <name> <signal>
  sprite-env services delete <name>

  sprite-env checkpoints create [--comment <text>]
  sprite-env checkpoints list [--include-auto] [--history <version>]
  sprite-env checkpoints get <id>
  sprite-env checkpoints restore <id>     restarts the sprite: this session ends
  sprite-env checkpoints delete <id>
  sprite-env checkpoints mount <id>       read-only, at /.sprite/checkpoints/<id>; copy files out without restoring
  sprite-env checkpoints unmount <id>

  sprite-env curl [curl options] <path>   curl against the management socket, e.g. sprite-env curl /v1/services
`

// socketPath can be overridden for tests.
func socketPath() string {
	if p := os.Getenv("SPRITE_ENV_SOCKET"); p != "" {
		return p
	}
	return "/.sprite/api.sock"
}

func main() {
	if len(os.Args) < 3 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	group, verb, args := os.Args[1], os.Args[2], os.Args[3:]
	var err error
	switch group {
	case "services", "service":
		err = services(verb, args)
	case "checkpoints", "checkpoint":
		err = checkpoints(verb, args)
	case "curl":
		err = curl(os.Args[2:])
	default:
		err = usageErr("unknown command %q", group)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "Error:", err)
		if _, ok := err.(usageError); ok {
			os.Exit(2)
		}
		os.Exit(1)
	}
}

type usageError string

func (e usageError) Error() string { return string(e) }
func usageErr(f string, a ...any) error {
	return usageError(fmt.Sprintf(f, a...) + " (run sprite-env for usage)")
}

// parse accepts flags before, between and after positional arguments, which the
// flag package alone does not: `services create web --cmd x` puts the name first.
func parse(fs *flag.FlagSet, args []string, positionals int) ([]string, error) {
	var pos []string
	for {
		fs.Parse(args)
		if fs.NArg() == 0 {
			break
		}
		pos = append(pos, fs.Arg(0))
		args = fs.Args()[1:]
	}
	if len(pos) != positionals {
		return nil, usageErr("%s takes %d argument(s), got %d", fs.Name(), positionals, len(pos))
	}
	return pos, nil
}

func splitList(v string) []string {
	if v == "" {
		return []string{}
	}
	return strings.Split(v, ",")
}

func services(verb string, args []string) error {
	fs := flag.NewFlagSet("services "+verb, flag.ExitOnError)
	switch verb {
	case "list", "ls":
		if _, err := parse(fs, args, 0); err != nil {
			return err
		}
		return call(http.MethodGet, "/v1/services", nil, nil)
	case "get":
		pos, err := parse(fs, args, 1)
		if err != nil {
			return err
		}
		return call(http.MethodGet, "/v1/services/"+url.PathEscape(pos[0]), nil, nil)
	case "create":
		cmd := fs.String("cmd", "", "the executable to run (binary only, no arguments)")
		argv := fs.String("args", "", "comma-separated arguments")
		env := fs.String("env", "", "comma-separated KEY=value environment variables")
		dir := fs.String("dir", "", "working directory")
		needs := fs.String("needs", "", "comma-separated services that must start first")
		port := fs.Int("http-port", 0, "route the sprite's URL to this port and start the service on demand")
		duration := fs.String("duration", "5s", "how long to stream logs after starting")
		noStream := fs.Bool("no-stream", false, "don't stream logs after creation")
		pos, err := parse(fs, args, 1)
		if err != nil {
			return err
		}
		if *cmd == "" {
			return usageErr("--cmd is required")
		}
		def := map[string]any{"cmd": *cmd, "args": splitList(*argv), "needs": splitList(*needs)}
		if *dir != "" {
			def["dir"] = *dir
		}
		if *port != 0 {
			def["http_port"] = *port
		}
		if *env != "" {
			vars := map[string]string{}
			for _, kv := range splitList(*env) {
				k, v, ok := strings.Cut(kv, "=")
				if !ok || k == "" {
					return usageErr("--env wants KEY=value pairs, got %q", kv)
				}
				vars[k] = v
			}
			def["env"] = vars
		}
		return stream(http.MethodPut, "/v1/services/"+url.PathEscape(pos[0]), *duration, *noStream, def)
	case "start", "restart":
		duration := fs.String("duration", "5s", "how long to stream logs after starting")
		noStream := fs.Bool("no-stream", false, "don't stream logs")
		pos, err := parse(fs, args, 1)
		if err != nil {
			return err
		}
		return stream(http.MethodPost, "/v1/services/"+url.PathEscape(pos[0])+"/"+verb, *duration, *noStream, nil)
	case "stop":
		timeout := fs.String("timeout", "", "how long to wait for a clean exit before SIGKILL (default 10s)")
		pos, err := parse(fs, args, 1)
		if err != nil {
			return err
		}
		q := url.Values{}
		if *timeout != "" {
			q.Set("timeout", *timeout)
		}
		return call(http.MethodPost, "/v1/services/"+url.PathEscape(pos[0])+"/stop", q, nil)
	case "signal":
		pos, err := parse(fs, args, 2)
		if err != nil {
			return err
		}
		return call(http.MethodPost, "/v1/services/signal", nil, map[string]string{"name": pos[0], "signal": pos[1]})
	case "delete", "rm":
		pos, err := parse(fs, args, 1)
		if err != nil {
			return err
		}
		return call(http.MethodDelete, "/v1/services/"+url.PathEscape(pos[0]), nil, nil)
	}
	return usageErr("unknown services command %q", verb)
}

// stream runs a create/start/restart. --no-stream still has to read the (now
// immediate) response to learn whether the action failed; it just prints nothing.
func stream(method, path, duration string, quiet bool, body any) error {
	q := url.Values{"duration": {duration}}
	if quiet {
		q.Set("duration", "0s")
		return request(method, path, q, body, io.Discard)
	}
	return request(method, path, q, body, os.Stdout)
}

func call(method, path string, q url.Values, body any) error {
	return request(method, path, q, body, os.Stdout)
}

func checkpoints(verb string, args []string) error {
	fs := flag.NewFlagSet("checkpoints "+verb, flag.ExitOnError)
	switch verb {
	case "create":
		comment := fs.String("comment", "", "what this checkpoint captures")
		if _, err := parse(fs, args, 0); err != nil {
			return err
		}
		return call(http.MethodPost, "/v1/checkpoint", nil, map[string]string{"comment": *comment})
	case "list", "ls":
		auto := fs.Bool("include-auto", false, "include automatic checkpoints")
		history := fs.String("history", "", "only checkpoints descended from this one")
		if _, err := parse(fs, args, 0); err != nil {
			return err
		}
		q := url.Values{}
		if *auto {
			q.Set("includeAuto", "true")
		}
		if *history != "" {
			q.Set("history", *history)
		}
		return call(http.MethodGet, "/v1/checkpoints", q, nil)
	case "get", "info":
		pos, err := parse(fs, args, 1)
		if err != nil {
			return err
		}
		return call(http.MethodGet, "/v1/checkpoints/"+url.PathEscape(pos[0]), nil, nil)
	case "delete", "rm":
		pos, err := parse(fs, args, 1)
		if err != nil {
			return err
		}
		return call(http.MethodDelete, "/v1/checkpoints/"+url.PathEscape(pos[0]), nil, nil)
	case "restore":
		pos, err := parse(fs, args, 1)
		if err != nil {
			return err
		}
		return restore(pos[0])
	case "mount", "unmount", "umount":
		pos, err := parse(fs, args, 1)
		if err != nil {
			return err
		}
		if verb == "umount" {
			verb = "unmount"
		}
		return call(http.MethodPost, "/v1/checkpoints/"+url.PathEscape(pos[0])+"/"+verb, nil, nil)
	}
	return usageErr("unknown checkpoints command %q", verb)
}

// restoreWait bounds how long restore waits to be killed along with the VM.
const restoreWait = 60 * time.Second

// restore asks the host to replace the filesystem this very process runs on.
// The host answers by killing the VM mid-response, so a stream that ends
// without an error is success. Returning then would be a trap: in
// `sprite-env checkpoints restore v4 && next-step`, next-step would run on the
// doomed filesystem for a moment. Instead, wait to be killed with everything else.
func restore(id string) error {
	err := request(http.MethodPost, "/v1/checkpoints/"+url.PathEscape(id)+"/restore", nil, nil, os.Stdout)
	if _, rejected := err.(apiError); rejected {
		return err
	}
	// Any other outcome is the response being cut short, which is how success looks from here.
	time.Sleep(restoreWait)
	return fmt.Errorf("restore to %s was accepted, but the sprite has not restarted after %s", id, restoreWait)
}

// apiError is a definitive failure: the API said no, or was never reached. A
// connection that breaks mid-response is not one (see restore).
type apiError string

func (e apiError) Error() string { return string(e) }

// request performs one API call and copies the response to out: NDJSON streams
// line by line as they arrive, JSON documents re-indented for reading.
func request(method, path string, q url.Values, body any, out io.Writer) error {
	sock := socketPath()
	client := &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", sock)
		},
	}}
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	u := "http://sprite" + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	req, err := http.NewRequest(method, u, rd)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := client.Do(req)
	if err != nil {
		return apiError(fmt.Sprintf("management socket %s: %v", sock, err))
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		var e struct {
			Message string `json:"message"`
		}
		if json.Unmarshal(b, &e) == nil && e.Message != "" {
			return apiError(fmt.Sprintf("%s (%d)", e.Message, resp.StatusCode))
		}
		return apiError(fmt.Sprintf("%s: %s", resp.Status, strings.TrimSpace(string(b))))
	}
	if !strings.HasPrefix(resp.Header.Get("Content-Type"), "application/x-ndjson") {
		b, err := io.ReadAll(resp.Body)
		if err != nil {
			return err
		}
		var pretty bytes.Buffer
		if json.Indent(&pretty, bytes.TrimSpace(b), "", "  ") == nil {
			b = append(pretty.Bytes(), '\n')
		}
		_, err = out.Write(b)
		return err
	}

	var failed apiError
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64*1024), 4<<20)
	for sc.Scan() {
		fmt.Fprintln(out, sc.Text())
		// Service streams put the message in "data", checkpoint streams in "error".
		var ev struct{ Type, Data, Error string }
		if json.Unmarshal(sc.Bytes(), &ev) == nil && ev.Type == "error" {
			failed = apiError(ev.Error + ev.Data)
		}
	}
	if failed != "" {
		return failed
	}
	return sc.Err()
}

// curl is `curl --unix-socket /.sprite/api.sock`, with a bare /path argument
// expanded to the socket's virtual host.
func curl(args []string) error {
	bin, err := exec.LookPath("curl")
	if err != nil {
		return err
	}
	argv := []string{"curl", "--unix-socket", socketPath()}
	for _, a := range args {
		if strings.HasPrefix(a, "/v1/") {
			a = "http://sprite" + a
		}
		argv = append(argv, a)
	}
	return syscall.Exec(bin, argv, os.Environ())
}
