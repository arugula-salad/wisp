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
	"hash/fnv"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/jhgaylor/mini-sprites/internal/certs"
	"github.com/jhgaylor/mini-sprites/internal/confine"
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
	// The confinement shim: spritesd re-execs itself to put a Landlock domain
	// on a VMM before exec'ing Firecracker (internal/confine). It never returns.
	if len(os.Args) > 1 && os.Args[1] == confine.ShimArg {
		confine.RunShim(os.Args[2:])
	}

	data := flag.String("data", defaultDataDir(), "data directory")
	listen := flag.String("listen", "127.0.0.1:7788", "API listen address")
	idle := flag.Duration("idle-timeout", 30*time.Second, "suspend a sprite after this long with no activity")
	warmTTL := flag.Duration("warm-ttl", time.Hour, "drop a suspended sprite's memory state (go cold) after this long")
	vcpus := flag.Int("vcpus", 8, "default vCPUs per sprite")
	mem := flag.Int("mem-mib", 2048, "default guest RAM per sprite (MiB); also the size of each warm snapshot on disk")
	dns := flag.String("dns", "1.1.1.1,8.8.8.8", "nameservers handed to guests")
	urlDomain := flag.String("url-domain", "sprites.localhost", "sprite URLs are <name>.<url-domain>; to serve them beyond this machine, point a wildcard DNS record here and see --public-listen")
	control := flag.Bool("control", true, "serve the multiplexed /control channel. The official Go SDK's ProxyPorts hangs once a server offers it (an SDK bug: two readers on one socket); --control=false makes SDKs fall back to per-operation WebSockets")
	netOn := flag.Bool("net", true, "attach sprites to the msbr0 tap pool; only one spritesd per host may own it, so run extra dev/test instances with --net=false")
	autoEvery := flag.Duration("auto-checkpoint-interval", time.Hour, "take an automatic checkpoint of a sprite whose disk changed and whose newest checkpoint is older than this (0 = only before restores)")
	autoKeep := flag.Int("auto-checkpoint-keep", 3, "automatic checkpoints kept per sprite; each is a full disk clone (0 = take none)")
	guestLimit := flag.Int("guest-checkpoint-limit", 20, "most checkpoints a sprite may hold when creating one from inside via sprite-env; the API is not limited (0 = no limit)")
	netdSocket := flag.String("netd-socket", "", "mini-sprites-netd socket, the root helper that backs restrictive network policies (default /run/mini-sprites/netd.sock)")
	org := flag.String("org", "local", "organization name reported in API responses")
	publicListen := flag.String("public-listen", "", "serve sprite URLs, and only sprite URLs, over HTTPS on this address; the one to forward a router port to. Needs --url-domain set to a real domain with a wildcard record, and a certificate: --tls-cert/--tls-key, or a Cloudflare token for an automatic one")
	publicPort := flag.Int("public-port", 443, "the port clients reach --public-listen on (the router's side of the forward); used in the URLs the API reports")
	tlsCert := flag.String("tls-cert", "", "PEM certificate chain for *.<url-domain>; re-read when it changes, so an external ACME client can renew it in place")
	tlsKey := flag.String("tls-key", "", "PEM private key for --tls-cert")
	acmeEmail := flag.String("acme-email", "", "contact address for the ACME account (optional)")
	acmeDir := flag.String("acme-directory", certs.LetsEncrypt, "ACME directory used when no --tls-cert is given. A wildcard certificate is requested over DNS-01 through Cloudflare, with the API token read from $CLOUDFLARE_API_TOKEN or <data>/cloudflare-token (needs Zone:Read and DNS:Edit on the zone)")
	publicConns := flag.Int("public-max-conns", 1024, "open connections allowed on --public-listen (0 = no limit)")
	publicConnsPer := flag.Int("public-max-conns-per-client", 64, "open connections allowed per IPv4 address or IPv6 /64 on --public-listen (0 = no limit; use 0 behind a CDN, where every client shares the CDN's addresses)")
	confineMode := flag.String("confine", os.Getenv("MINI_SPRITES_CONFINE"), "sandbox each Firecracker with Landlock + a cgroup: \"best-effort\" (default; apply what the kernel supports and log the rest), \"strict\" (refuse to start without both) or \"off\"")
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

	mode, err := confine.ParseMode(*confineMode)
	if err != nil {
		fatal(log, err)
	}
	// One cgroup subtree per data directory, so a second spritesd (dev, tests)
	// on the same host does not sweep away the first one's VM cgroups.
	conf, err := confine.Open(mode, "mini-sprites-"+cgroupTag(abs))
	if err != nil {
		fatal(log, err)
	}
	opts.Host.Confine = conf
	log.Info("vmm confinement", "detail", conf.Describe())

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
	urlFmt := "http://%s." + *urlDomain + ":" + port
	if *publicListen != "" {
		urlFmt = "https://%s." + *urlDomain
		if *publicPort != 443 {
			urlFmt += fmt.Sprintf(":%d", *publicPort)
		}
	}
	api := server.New(opts, st, life, log, token, *org, *urlDomain, urlFmt)
	srv := &http.Server{Addr: *listen, ReadHeaderTimeout: 10 * time.Second, Handler: api.Handler()}

	var public *http.Server
	if *publicListen != "" {
		cs, err := publicCerts(abs, *urlDomain, *tlsCert, *tlsKey, *acmeEmail, *acmeDir, log)
		if err != nil {
			fatal(log, err)
		}
		ln, err := net.Listen("tcp", *publicListen)
		if err != nil {
			fatal(log, err)
		}
		public = server.NewPublicServer(api.PublicHandler(), cs.GetCertificate)
		go func() {
			log.Info("serving sprite URLs to the public", "addr", *publicListen, "urls", fmt.Sprintf(urlFmt, "<name>"))
			err := public.ServeTLS(server.LimitListener(ln, *publicConns, *publicConnsPer), "", "")
			if !errors.Is(err, http.ErrServerClosed) {
				fatal(log, err)
			}
		}()
	}

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
	if public != nil {
		public.Close()
	}
	srv.Shutdown(ctx) // in-flight exec sessions are cut off; their sprites still suspend warm
	srv.Close()
	life.Shutdown()
}

// publicCerts is the certificate source for the public listener: the operator's
// own PEM pair, or else one we keep current ourselves over ACME.
func publicCerts(data, domain, certFile, keyFile, email, directory string, log *slog.Logger) (*certs.Store, error) {
	if (certFile == "") != (keyFile == "") {
		return nil, errors.New("--tls-cert and --tls-key go together")
	}
	if certFile != "" {
		cs := certs.NewStore(certFile, keyFile, log)
		return cs, cs.Load()
	}
	if domain == "localhost" || strings.HasSuffix(domain, ".localhost") {
		return nil, fmt.Errorf("--public-listen needs --url-domain set to a domain you control, not %s", domain)
	}
	cfToken := os.Getenv("CLOUDFLARE_API_TOKEN")
	if b, err := os.ReadFile(filepath.Join(data, "cloudflare-token")); cfToken == "" && err == nil {
		cfToken = strings.TrimSpace(string(b))
	}
	if cfToken == "" {
		return nil, fmt.Errorf("--public-listen needs a certificate: pass --tls-cert and --tls-key, or put a Cloudflare API token in $CLOUDFLARE_API_TOKEN or %s", filepath.Join(data, "cloudflare-token"))
	}
	mgr := &certs.Manager{Domain: domain, Email: email, Dir: filepath.Join(data, "acme"), DirectoryURL: directory,
		DNS: &certs.Cloudflare{Token: cfToken}, Log: log}
	cs := certs.NewStore(mgr.CertFile(), mgr.KeyFile(), log)
	go mgr.Run(context.Background(), cs)
	return cs, nil
}

// cgroupTag derives a stable, filesystem-safe suffix from the data directory,
// so two spritesd instances on one host get separate cgroup subtrees.
func cgroupTag(dataDir string) string {
	h := fnv.New32a()
	h.Write([]byte(dataDir))
	return hex.EncodeToString(h.Sum(nil))
}

func fatal(log *slog.Logger, err error) {
	log.Error(err.Error())
	os.Exit(1)
}
