package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// The sudo stand-in. In the base image the sprite user has passwordless sudo;
// most container images (node, python, alpine) ship no sudo at all, which would
// leave a sprite made from one with no way to install a package. On such a
// disk the agent installs a copy of this binary at /.sprite/bin/sudo, setuid
// root, linked from /usr/local/bin/sudo, and it acts as a passwordless sudo
// for the sprite user only. It understands the options scripts commonly use
// (-u, -E, -H, -i, -s, -n, -k, -v, -l); anything else is refused, not ignored.
//
// A setuid copy only ever runs this code, whatever it is called: a guest user
// must not get the rest of sprite-env with root's identity. Under the
// privileges policy's noNewPrivileges the kernel ignores the setuid bit, and
// so, like the real sudo, it stops working.

const secureDefaultPath = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"

// sudoMode says this process should act as sudo.
func sudoMode() bool {
	return os.Geteuid() != os.Getuid() || filepath.Base(os.Args[0]) == "sudo"
}

type sudoOpts struct {
	user      string
	keepEnv   bool
	login     bool // -i
	shell     bool // -s
	justCheck bool // -v, -k/-K with no command, -l
	list      bool
	cmd       []string
}

func parseSudo(args []string) (sudoOpts, error) {
	o := sudoOpts{user: "root"}
	for len(args) > 0 {
		a := args[0]
		if a == "--" {
			args = args[1:]
			break
		}
		if !strings.HasPrefix(a, "-") || a == "-" {
			break
		}
		args = args[1:]
		if long, ok := strings.CutPrefix(a, "--"); ok {
			name, val, hasVal := strings.Cut(long, "=")
			switch name {
			case "user":
				if !hasVal {
					if len(args) == 0 {
						return o, errors.New("option --user needs a value")
					}
					val, args = args[0], args[1:]
				}
				o.user = val
			case "preserve-env":
				o.keepEnv = true // a list of names is taken as "all of them"
			case "login":
				o.login = true
			case "shell":
				o.shell = true
			case "non-interactive", "set-home", "reset-timestamp", "remove-timestamp":
			case "validate":
				o.justCheck = true
			case "list":
				o.list, o.justCheck = true, true
			default:
				return o, fmt.Errorf("option --%s is not supported by this sprite's sudo stand-in", name)
			}
			continue
		}
		flags := a[1:]
		for i := 0; i < len(flags); i++ {
			switch flags[i] {
			case 'u':
				val := flags[i+1:]
				if val == "" {
					if len(args) == 0 {
						return o, errors.New("option -u needs a user")
					}
					val, args = args[0], args[1:]
				}
				o.user = val
				i = len(flags)
			case 'E':
				o.keepEnv = true
			case 'i':
				o.login = true
			case 's':
				o.shell = true
			case 'n', 'H', 'k', 'K', 'S', 'A':
				// -S and -A read a password; none is ever asked for.
			case 'v':
				o.justCheck = true
			case 'l':
				o.list, o.justCheck = true, true
			default:
				return o, fmt.Errorf("option -%c is not supported by this sprite's sudo stand-in", flags[i])
			}
		}
	}
	if len(args) > 0 {
		o.cmd = args
	}
	if len(o.cmd) == 0 && !o.login && !o.shell {
		o.justCheck = true
	}
	return o, nil
}

type account struct {
	name, home, shell string
	uid, gid          int
}

// lookupAccount reads /etc/passwd itself, since os/user leaves out the shell.
// A name of the form #<uid> is a numeric id, as in sudo.
func lookupAccount(name string) (account, error) {
	f, err := os.Open("/etc/passwd")
	if err != nil {
		return account{}, err
	}
	defer f.Close()
	byID := strings.HasPrefix(name, "#")
	for sc := bufio.NewScanner(f); sc.Scan(); {
		p := strings.Split(sc.Text(), ":")
		if len(p) < 7 {
			continue
		}
		if (byID && p[2] == name[1:]) || (!byID && p[0] == name) {
			a := account{name: p[0], home: p[5], shell: p[6]}
			a.uid, _ = strconv.Atoi(p[2])
			a.gid, _ = strconv.Atoi(p[3])
			if a.shell == "" {
				a.shell = "/bin/sh"
			}
			return a, nil
		}
	}
	return account{}, fmt.Errorf("unknown user %s", name)
}

// shellQuote joins a command for sh -c, the way sudo -s and -i pass one on.
func shellQuote(args []string) string {
	q := make([]string, len(args))
	for i, a := range args {
		if a != "" && strings.Trim(a, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-_./=:,+@%") == "" {
			q[i] = a
		} else {
			q[i] = "'" + strings.ReplaceAll(a, "'", `'\''`) + "'"
		}
	}
	return strings.Join(q, " ")
}

// imagePath is the PATH a container image set, if this sprite came from one.
func imagePath() string {
	b, err := os.ReadFile("/.sprite/image.json")
	if err != nil {
		return ""
	}
	var meta struct {
		Env []string `json:"env"`
	}
	json.Unmarshal(b, &meta)
	for _, kv := range meta.Env {
		if v, ok := strings.CutPrefix(kv, "PATH="); ok {
			if !strings.Contains(":"+v+":", ":/usr/local/bin:") {
				v += ":/usr/local/bin"
			}
			return v
		}
	}
	return ""
}

// sudoEnv is the environment the command runs with: reset, as sudo's
// env_reset does, keeping the terminal and locale, unless -E.
func sudoEnv(caller []string, keep bool, target account, invoker int) []string {
	path := imagePath()
	if path == "" {
		path = secureDefaultPath
	}
	var env []string
	for _, kv := range caller {
		k, _, _ := strings.Cut(kv, "=")
		switch {
		case keep && k != "HOME" && k != "USER" && k != "LOGNAME" && k != "SHELL" && !strings.HasPrefix(k, "SUDO_"):
			env = append(env, kv)
		case k == "TERM" || k == "COLORTERM" || k == "LANG" || k == "LANGUAGE" || k == "TZ" || k == "DISPLAY" || strings.HasPrefix(k, "LC_"):
			env = append(env, kv)
		}
	}
	if !keep {
		env = append(env, "PATH="+path)
	}
	env = append(env, "HOME="+target.home, "USER="+target.name, "LOGNAME="+target.name, "SHELL="+target.shell,
		"SUDO_UID="+strconv.Itoa(invoker))
	if u, err := user.LookupId(strconv.Itoa(invoker)); err == nil {
		env = append(env, "SUDO_USER="+u.Username, "SUDO_GID="+u.Gid)
	}
	return env
}

func findIn(file, path string) (string, error) {
	if strings.Contains(file, "/") {
		return file, nil
	}
	for _, dir := range filepath.SplitList(path) {
		p := filepath.Join(dir, file)
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() && fi.Mode()&0o111 != 0 {
			return p, nil
		}
	}
	return "", fmt.Errorf("%s: command not found", file)
}

// runSudo does not return on success: it execs the command.
func runSudo(args []string) int {
	fail := func(format string, a ...any) int {
		fmt.Fprintf(os.Stderr, "sudo: "+format+"\n", a...)
		return 1
	}
	invoker := os.Getuid()
	if os.Geteuid() != 0 {
		return fail("cannot become root: this sprite's sudo is a setuid stand-in, and setuid programs are disabled here (no_new_privs, see the privileges policy)")
	}
	if invoker != 0 {
		sp, err := lookupAccount("sprite")
		if err != nil || sp.uid != invoker {
			return fail("only the sprite user may use sudo in this sprite")
		}
	}
	o, err := parseSudo(args)
	if err != nil {
		return fail("%v", err)
	}
	if o.list {
		fmt.Println("User may run the following commands on this sprite:\n    (ALL : ALL) NOPASSWD: ALL")
	}
	if o.justCheck {
		return 0
	}
	target, err := lookupAccount(o.user)
	if err != nil {
		return fail("%v", err)
	}
	env := sudoEnv(os.Environ(), o.keepEnv, target, invoker)
	argv := o.cmd
	switch {
	case o.login:
		argv = []string{target.shell, "-l"}
		if len(o.cmd) > 0 {
			argv = append(argv, "-c", shellQuote(o.cmd))
		}
		if err := os.Chdir(target.home); err != nil {
			os.Chdir("/")
		}
	case o.shell:
		sh := os.Getenv("SHELL")
		if sh == "" {
			sh = target.shell
		}
		argv = []string{sh}
		if len(o.cmd) > 0 {
			argv = append(argv, "-c", shellQuote(o.cmd))
		}
	}
	path := secureDefaultPath
	for _, kv := range env {
		if v, ok := strings.CutPrefix(kv, "PATH="); ok {
			path = v
		}
	}
	bin, err := findIn(argv[0], path)
	if err != nil {
		return fail("%v", err)
	}
	var groups []int
	if u, err := user.LookupId(strconv.Itoa(target.uid)); err == nil {
		if ids, err := u.GroupIds(); err == nil {
			for _, g := range ids {
				if n, err := strconv.Atoi(g); err == nil {
					groups = append(groups, n)
				}
			}
		}
	}
	if err := syscall.Setgroups(groups); err != nil {
		return fail("setgroups: %v", err)
	}
	if err := syscall.Setgid(target.gid); err != nil {
		return fail("setgid: %v", err)
	}
	if err := syscall.Setuid(target.uid); err != nil {
		return fail("setuid: %v", err)
	}
	err = syscall.Exec(bin, argv, env)
	return fail("%s: %v", argv[0], err)
}
