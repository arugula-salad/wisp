// wispd is a single-host implementation of the Sprites API: persistent,
// hardware-isolated Linux environments backed by Firecracker microVMs that
// suspend when idle and wake on demand.
package main

import (
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/arugula-salad/wisp/internal/confine"
	"github.com/arugula-salad/wisp/internal/daemon"
)

func main() {
	// The confinement shim: wispd re-execs itself to put a Landlock domain
	// on a VMM before exec'ing Firecracker (internal/confine). It never returns.
	if len(os.Args) > 1 && os.Args[1] == confine.ShimArg {
		confine.RunShim(os.Args[2:])
	}

	// A first argument that is not a flag selects a subcommand; none runs the
	// daemon. status asks a running daemon (status.go); restore and backups work
	// offline against the bucket (backups.go); images manages the container
	// image cache through a running daemon (images.go); keys manages API keys
	// the same way (keys.go).
	if len(os.Args) > 1 && !strings.HasPrefix(os.Args[1], "-") {
		switch cmd := os.Args[1]; cmd {
		case "status":
			os.Exit(runStatus(os.Args[2:]))
		case "restore":
			os.Exit(runRestore(os.Args[2:]))
		case "backups":
			os.Exit(runBackups(os.Args[2:]))
		case "images":
			os.Exit(runImages(os.Args[2:]))
		case "keys":
			os.Exit(runKeys(os.Args[2:]))
		default:
			fmt.Fprintf(os.Stderr, "unknown command %q (want status, restore, backups, images or keys; no command runs the daemon)\n", cmd)
			os.Exit(2)
		}
	}

	finish := daemon.Bind(flag.CommandLine)
	flag.Parse()
	opts, f := finish()
	daemon.Run("wispd", opts, f)
}
