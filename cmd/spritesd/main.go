// spritesd is a single-host implementation of the Sprites API: persistent,
// hardware-isolated Linux environments backed by Firecracker microVMs that
// suspend when idle and wake on demand.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/jhgaylor/mini-sprites/internal/server"
	"github.com/jhgaylor/mini-sprites/internal/store"
	"github.com/jhgaylor/mini-sprites/internal/vmm"
)

func defaultDataDir() string {
	if d := os.Getenv("MINI_SPRITES_DATA"); d != "" {
		return d
	}
	if d := os.Getenv("XDG_DATA_HOME"); d != "" {
		return filepath.Join(d, "mini-sprites")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "share", "mini-sprites")
}

// loadToken reads the API token, generating one on first run.
func loadToken(path string) (string, error) {
	if b, err := os.ReadFile(path); err == nil {
		return strings.TrimSpace(string(b)), nil
	}
	b := make([]byte, 24)
	rand.Read(b)
	tok := "msprite_" + hex.EncodeToString(b)
	return tok, os.WriteFile(path, []byte(tok+"\n"), 0o600)
}

func main() {
	// A first argument that is not a flag selects a subcommand; none runs the daemon.
	if len(os.Args) > 1 && !strings.HasPrefix(os.Args[1], "-") {
		switch cmd := os.Args[1]; cmd {
		case "status":
			os.Exit(runStatus(os.Args[2:]))
		default:
			fmt.Fprintf(os.Stderr, "unknown command %q (want status; no command runs the daemon)\n", cmd)
			os.Exit(2)
		}
	}

	data := flag.String("data", defaultDataDir(), "data directory")
	listen := flag.String("listen", "127.0.0.1:7788", "API listen address")
	idle := flag.Duration("idle-timeout", 30*time.Second, "suspend a sprite after this long with no activity")
	warmTTL := flag.Duration("warm-ttl", time.Hour, "drop a suspended sprite's memory state (go cold) after this long")
	vcpus := flag.Int("vcpus", 8, "default vCPUs per sprite")
	mem := flag.Int("mem-mib", 2048, "default guest RAM per sprite (MiB); also the size of each warm snapshot on disk")
	dns := flag.String("dns", "1.1.1.1,8.8.8.8", "nameservers handed to guests")
	urlDomain := flag.String("url-domain", "sprites.localhost", "sprite URLs are http://<name>.<url-domain>:<port>; point a wildcard DNS record here to serve them beyond this machine")
	control := flag.Bool("control", true, "serve the multiplexed /control channel. The official Go SDK's ProxyPorts hangs once a server offers it (an SDK bug: two readers on one socket); --control=false makes SDKs fall back to per-operation WebSockets")
	netOn := flag.Bool("net", true, "attach sprites to the msbr0 tap pool; only one spritesd per host may own it, so run extra dev/test instances with --net=false")
	autoEvery := flag.Duration("auto-checkpoint-interval", time.Hour, "take an automatic checkpoint of a sprite whose disk changed and whose newest checkpoint is older than this (0 = only before restores)")
	autoKeep := flag.Int("auto-checkpoint-keep", 3, "automatic checkpoints kept per sprite; each is a full disk clone (0 = take none)")
	guestLimit := flag.Int("guest-checkpoint-limit", 20, "most checkpoints a sprite may hold when creating one from inside via sprite-env; the API is not limited (0 = no limit)")
	netdSocket := flag.String("netd-socket", "", "mini-sprites-netd socket, the root helper that backs restrictive network policies (default /run/mini-sprites/netd.sock)")
	org := flag.String("org", "local", "organization name reported in API responses")
	maxSprites := flag.Int("max-sprites", 0, "most sprites that may exist; creating another is refused (0 = no limit)")
	maxRunning := flag.Int("max-running", 0, "most sprites that may run at once; waking another is refused until one goes idle (0 = no limit)")
	diskReserve := flag.Int64("disk-reserve-mib", 2048, "free space (MiB) a create, checkpoint or restore must leave on the sprite volume, or it is refused (0 = never refuse)")
	diskWarn := flag.Int("disk-warn-percent", 10, "warn in the log while less than this share of the sprite volume is free (0 = never)")
	flag.Parse()

	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	abs, err := filepath.Abs(*data)
	if err != nil {
		fatal(log, err)
	}
	// Machine dirs hold unix sockets, whose paths are capped at 108 bytes.
	if n := len(filepath.Join(abs, "vm", "0123456789ab", "fc.sock")); n > 100 {
		fatal(log, fmt.Errorf("data directory path is too long for unix sockets (%d bytes): %s", n, abs))
	}
	opts := server.Options{
		DataDir: abs, BaseImage: filepath.Join(abs, "images", "base.ext4"),
		Host: vmm.Host{
			Firecracker: filepath.Join(abs, "bin", "firecracker"),
			Kernel:      filepath.Join(abs, "kernel", "vmlinux"),
			Initrd:      filepath.Join(abs, "initrd.cpio"),
		},
		IdleTimeout: *idle, WarmTTL: *warmTTL, DefaultVCPUs: *vcpus, DefaultMemMiB: *mem, DNS: *dns, NoNetwork: !*netOn, NoControl: !*control,
		AutoCheckpointInterval: *autoEvery, AutoCheckpointKeep: *autoKeep, GuestCheckpointLimit: *guestLimit,
		NetdSocket: *netdSocket,
		MaxSprites: *maxSprites, MaxRunning: *maxRunning, DiskReserve: *diskReserve << 20, DiskWarnPercent: *diskWarn,
	}
	for what, p := range map[string]string{"firecracker (scripts/fetch-deps.sh)": opts.Host.Firecracker,
		"guest kernel (scripts/fetch-deps.sh)": opts.Host.Kernel, "initrd (scripts/build-initrd.sh)": opts.Host.Initrd,
		"base image (scripts/build-image.sh)": opts.BaseImage} {
		if _, err := os.Stat(p); err != nil {
			fatal(log, fmt.Errorf("missing %s: %s", what, p))
		}
	}
	if f, err := os.OpenFile("/dev/kvm", os.O_RDWR, 0); err != nil {
		fatal(log, fmt.Errorf("need read/write access to /dev/kvm: %w", err))
	} else {
		f.Close()
	}

	// First, because it doubles as the lock on the data directory.
	statusLn, err := listenStatus(abs)
	if err != nil {
		fatal(log, err)
	}
	defer statusLn.Close() // which also removes the socket

	token, err := loadToken(filepath.Join(abs, "token"))
	if err != nil {
		fatal(log, err)
	}
	st, err := store.Open(abs)
	if err != nil {
		fatal(log, err)
	}
	life := server.NewLifecycle(opts, st, log)

	_, port, _ := net.SplitHostPort(*listen)
	api := server.New(opts, st, life, log, token, *org, *urlDomain, port)
	srv := &http.Server{Addr: *listen, ReadHeaderTimeout: 10 * time.Second, Handler: api.Handler()}
	go http.Serve(statusLn, api.StatusHandler(*listen))

	go func() {
		log.Info("spritesd listening", "addr", *listen, "data", abs, "token_file", filepath.Join(abs, "token"))
		if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
			fatal(log, err)
		}
	}()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	log.Info("shutting down: suspending running sprites")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	srv.Shutdown(ctx) // in-flight exec sessions are cut off; their sprites still suspend warm
	srv.Close()
	life.Shutdown()
}

func fatal(log *slog.Logger, err error) {
	log.Error(err.Error())
	os.Exit(1)
}
