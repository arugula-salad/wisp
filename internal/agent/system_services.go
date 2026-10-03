package agent

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// System services are daemons the disk image itself declares, one JSON file per
// service in a drop-in directory (/etc/wisp/services.d/<name>.json):
//
//	{"cmd": "/usr/bin/envd", "args": ["-port", "49983"], "user": "root",
//	 "env": {"GOTRACEBACK": "all"}, "dir": "/"}
//
// They are part of the image, not of the sprite's user: the agent starts them at
// every boot and restarts them when they exit (with the same backoff as services),
// but they are not in the services API, cannot be defined, stopped or deleted
// through it, and their starts and crashes are not reported as service events. An
// image that declares none, which is every image but the E2B one today, gets no
// supervisor and nothing on its disk changes.
//
// They run under the sprite's policy like everything else the agent launches, so
// whatever they start in turn is confined the same way. What they print goes to
// <logsDir>/<name>.log, rotated as services' logs are.

// SystemServiceDef is one file in the drop-in directory. The service is named
// after the file.
type SystemServiceDef struct {
	Cmd  string            `json:"cmd"`
	Args []string          `json:"args,omitempty"`
	User string            `json:"user,omitempty"` // an account on the disk; root when empty
	Env  map[string]string `json:"env,omitempty"`
	Dir  string            `json:"dir,omitempty"` // the user's home when empty
}

// NewSystemSupervisor starts the system services declared in defsDir. It returns
// nil, having touched nothing, when the directory holds none.
func NewSystemSupervisor(defsDir, logsDir, runDir string) *Supervisor {
	entries, err := os.ReadDir(defsDir)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Printf("system services: %v", err)
		}
		return nil
	}
	services := map[string]*service{}
	skipped := false
	for _, e := range entries {
		name, ok := strings.CutSuffix(e.Name(), ".json")
		if !ok || e.IsDir() {
			continue
		}
		def, err := loadSystemServiceDef(filepath.Join(defsDir, e.Name()))
		if err == nil && !serviceNameRE.MatchString(name) {
			err = fmt.Errorf("invalid service name %q", name)
		}
		if err != nil {
			// The rest still start: one bad file must not take the others down with it.
			log.Printf("system service %s: %v", e.Name(), err)
			skipped = true
			continue
		}
		s := newService(ServiceDef{Name: name, Cmd: def.Cmd, Args: def.Args, Needs: []string{}, Env: def.Env, Dir: def.Dir})
		s.user = def.User
		if s.user == "" {
			s.user = "root"
		}
		services[name] = s
	}
	if len(services) == 0 {
		return nil
	}
	sv := &Supervisor{defsDir: defsDir, logsDir: logsDir, runDir: runDir, services: services,
		logRot: LogRotation{MaxBytes: defaultLogMaxBytes, Keep: defaultLogKeep}}
	os.MkdirAll(logsDir, 0o755)
	os.MkdirAll(runDir, 0o755)
	// The sweep of logs and pids no definition owns is only safe when every
	// definition was read: a file that failed to parse still owns its old logs,
	// which are what anyone debugging it needs.
	sv.startAll(!skipped)
	for _, n := range sv.sortedNames() {
		log.Printf("system service %s: %s", n, sv.services[n].state.Status)
	}
	return sv
}

func loadSystemServiceDef(path string) (SystemServiceDef, error) {
	var def SystemServiceDef
	b, err := os.ReadFile(path)
	if err != nil {
		return def, err
	}
	// Strict: a misspelt field in an image would otherwise be silently ignored.
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&def); err != nil {
		return def, err
	}
	if def.Cmd == "" {
		return def, fmt.Errorf("cmd is required")
	}
	return def, nil
}

// lookupUser resolves an account on the sprite's disk the way defaultUser does
// the sprite user. An agent that is not root (tests on the host) cannot switch
// users and runs everything as itself.
func lookupUser(name string) (cred *syscall.Credential, home, uname string, err error) {
	u, err := user.Lookup(name)
	if err != nil {
		return nil, "", "", fmt.Errorf("user %q: %w", name, err)
	}
	if os.Getuid() != 0 {
		h, _ := os.UserHomeDir()
		if h == "" {
			h = "/"
		}
		return nil, h, os.Getenv("USER"), nil
	}
	return credential(u), u.HomeDir, u.Username, nil
}

func credential(u *user.User) *syscall.Credential {
	uid, _ := strconv.Atoi(u.Uid)
	gid, _ := strconv.Atoi(u.Gid)
	cred := &syscall.Credential{Uid: uint32(uid), Gid: uint32(gid)}
	if gids, err := u.GroupIds(); err == nil {
		for _, g := range gids {
			if n, err := strconv.Atoi(g); err == nil {
				cred.Groups = append(cred.Groups, uint32(n))
			}
		}
	}
	return cred
}
