package agent

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
)

// A sprite created from a container image (wispd's images.go) carries the
// image's environment in /.sprite/image.json: PATH additions such as
// /usr/local/go/bin, NODE_VERSION and the like, which the image's own tools
// expect. Every exec session and service gets it on top of the base
// environment, the way a container would. The base image has no such file.

// imageMetaPath is overridden in tests.
var imageMetaPath = "/.sprite/image.json"

const defaultPath = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"

// imageEnv returns the image's environment, less what the agent decides per
// user (HOME, USER and friends would point at the image's build user).
func imageEnv() []string {
	b, err := os.ReadFile(imageMetaPath)
	if err != nil {
		return nil
	}
	var meta struct {
		Env []string `json:"env"`
	}
	if json.Unmarshal(b, &meta) != nil {
		return nil
	}
	var out []string
	for _, kv := range meta.Env {
		k, v, ok := strings.Cut(kv, "=")
		if !ok || k == "" {
			continue
		}
		switch k {
		case "HOME", "USER", "LOGNAME", "SHELL", "HOSTNAME", "TERM":
			continue
		case "PATH":
			// sprite-env and the sudo stand-in live in /usr/local/bin.
			if !strings.Contains(":"+v+":", ":/usr/local/bin:") {
				v += ":/usr/local/bin"
			}
			kv = k + "=" + v
		}
		out = append(out, kv)
	}
	return out
}

// loginShell is the shell commands get by default: bash as in the base image,
// else sh (busybox and alpine images), else none at all (distroless).
func loginShell() string {
	for _, sh := range []string{"/bin/bash", "/usr/bin/bash", "/bin/sh", "/usr/bin/sh"} {
		if fi, err := os.Stat(sh); err == nil && !fi.IsDir() && fi.Mode()&0o111 != 0 {
			return sh
		}
	}
	return ""
}

var errNoShell = errors.New("this sprite has no shell (its image has neither bash nor sh); give a command to run")

// defaultCommand is what an exec with no command runs.
func defaultCommand() ([]string, error) {
	sh := loginShell()
	if sh == "" {
		return nil, errNoShell
	}
	return []string{filepath.Base(sh)}, nil
}

// envValue returns the last value of key in env, as exec does.
func envValue(env []string, key string) string {
	for i := len(env) - 1; i >= 0; i-- {
		if v, ok := strings.CutPrefix(env[i], key+"="); ok {
			return v
		}
	}
	return ""
}

// lookPath finds file on the PATH the command will run with, which for an
// image's sprite is not the agent's own.
func lookPath(file string, env []string) (string, error) {
	path := envValue(env, "PATH")
	if path == "" {
		path = defaultPath
	}
	for _, dir := range filepath.SplitList(path) {
		if dir == "" {
			dir = "."
		}
		p := filepath.Join(dir, file)
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() && fi.Mode()&0o111 != 0 {
			return p, nil
		}
	}
	return "", os.ErrNotExist
}
