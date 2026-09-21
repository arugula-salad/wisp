// sprite-agent is the in-guest runtime. As PID 1 it sets up the system and
// supervises a copy of itself running `serve`, which hosts the agent API on
// vsock. Splitting the two keeps PID 1's wait4(-1) orphan reaping from
// stealing exit statuses that os/exec is waiting on in the server.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/mdlayher/vsock"
	"golang.org/x/sys/unix"

	"github.com/jhgaylor/mini-sprites/internal/agent"
)

// AgentPort is the vsock port spritesd dials.
const AgentPort = 1024

// hostAPIPort is the vsock port spritesd answers on for this sprite (guestAPIPort there).
const hostAPIPort = 1025

func main() {
	log.SetFlags(0)
	log.SetPrefix("sprite-agent: ")
	if os.Getpid() == 1 {
		runInit()
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "serve" {
		serve(os.Args[2:])
		return
	}
	fmt.Fprintln(os.Stderr, "usage: sprite-agent serve [--listen vsock|tcp:ADDR|unix:PATH]")
	os.Exit(2)
}

func serve(args []string) {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	listen := fs.String("listen", "vsock", "vsock, tcp:HOST:PORT or unix:PATH (the latter two are for host-side testing)")
	stateDir := fs.String("state-dir", "/.sprite", "where service definitions and logs live (on the sprite's disk)")
	runDir := fs.String("run-dir", "/run/sprite-services", "pid files; must not survive a reboot")
	fs.Parse(args)

	var ln net.Listener
	var err error
	switch {
	case *listen == "vsock":
		ln, err = vsock.Listen(AgentPort, nil)
	case strings.HasPrefix(*listen, "tcp:"):
		ln, err = net.Listen("tcp", strings.TrimPrefix(*listen, "tcp:"))
	case strings.HasPrefix(*listen, "unix:"):
		ln, err = net.Listen("unix", strings.TrimPrefix(*listen, "unix:"))
	default:
		log.Fatalf("bad --listen %q", *listen)
	}
	if err != nil {
		log.Fatalf("listen: %v", err)
	}

	srv := &agent.Server{
		Sessions: agent.NewManager(),
		Services: agent.NewSupervisor(*stateDir, *runDir),
		Poweroff: func() {
			unix.Sync()
			// With reboot=k on the kernel command line this resets via the
			// keyboard controller, which makes Firecracker exit cleanly.
			if err := unix.Reboot(unix.LINUX_REBOOT_CMD_RESTART); err != nil {
				log.Printf("reboot: %v", err)
			}
		},
	}
	// The in-guest API needs the host channel, which only exists over vsock.
	if *listen == "vsock" {
		sock := filepath.Join(*stateDir, "api.sock")
		if gl, err := agent.ListenGuestAPI(sock); err != nil {
			log.Printf("%s: %v", sock, err)
		} else {
			dialHost := func(context.Context) (net.Conn, error) { return vsock.Dial(vsock.Host, hostAPIPort, nil) }
			gs := &http.Server{Handler: srv.GuestAPI(dialHost), ReadHeaderTimeout: 10 * time.Second}
			go func() { log.Fatal(gs.Serve(gl)) }()
		}
	}
	log.Printf("serving on %s", *listen)
	hs := &http.Server{Handler: srv.Handler(), ReadHeaderTimeout: 10 * time.Second}
	log.Fatal(hs.Serve(ln))
}
