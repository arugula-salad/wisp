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
func loadToken(path string) (string, error) { return loadSecret(path, "msprite_") }

// loadSecret reads a secret from path, generating a random one on first run.
func loadSecret(path, prefix string) (string, error) {
	if b, err := os.ReadFile(path); err == nil {
		return strings.TrimSpace(string(b)), nil
	}
	b := make([]byte, 24)
	rand.Read(b)
	tok := prefix + hex.EncodeToString(b)
	return tok, os.WriteFile(path, []byte(tok+"\n"), 0o600)
}

func main() {
	// The confinement shim: spritesd re-execs itself to put a Landlock domain
	// on a VMM before exec'ing Firecracker (internal/confine). It never returns.
	if len(os.Args) > 1 && os.Args[1] == confine.ShimArg {
		confine.RunShim(os.Args[2:])
	}

	// A first argument that is not a flag selects a subcommand; none runs the
	// daemon. status asks a running daemon (status.go); restore and backups work
	// offline against the bucket (backups.go).
	if len(os.Args) > 1 && !strings.HasPrefix(os.Args[1], "-") {
		switch cmd := os.Args[1]; cmd {
		case "status":
			os.Exit(runStatus(os.Args[2:]))
		case "restore":
			os.Exit(runRestore(os.Args[2:]))
		case "backups":
			os.Exit(runBackups(os.Args[2:]))
		default:
			fmt.Fprintf(os.Stderr, "unknown command %q (want status, restore or backups; no command runs the daemon)\n", cmd)
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
	urlDomain := flag.String("url-domain", "sprites.localhost", "sprite URLs are <name>.<url-domain>; to serve them beyond this machine, point a wildcard DNS record here and see --public-listen")
	control := flag.Bool("control", true, "serve the multiplexed /control channel; --control=false makes every SDK fall back to per-operation WebSockets")
	controlGo := flag.Bool("control-for-go-sdk", false, "also offer /control to the official Go SDK (by default it is answered 404 there and falls back to per-operation WebSockets, because its ProxyPorts races on a control socket)")
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
	domainsOn := flag.Bool("custom-domains", true, "with --public-listen, let sprites have custom domains (POST /v1/sprites/<name>/domains), each with its own certificate from --acme-directory over TLS-ALPN-01")
	domainResolver := flag.String("domain-resolver", "1.1.1.1:53", "recursive DNS server that checks a custom domain points here before a certificate is requested for it")
	domainsPer := flag.Int("max-domains-per-sprite", 5, "custom domains one sprite may have (0 = no limit)")
	domainsTotal := flag.Int("max-domains", 50, "custom domains across all sprites (0 = no limit)")
	domainOrders := flag.Int("acme-orders-per-hour", 10, "certificate orders per hour for custom domains, across all of them; keeps a misconfigured domain from spending the CA's rate limits")
	confineMode := flag.String("confine", os.Getenv("MINI_SPRITES_CONFINE"), "sandbox each Firecracker with Landlock + a cgroup: \"best-effort\" (default; apply what the kernel supports and log the rest), \"strict\" (refuse to start without both) or \"off\"")
	backupOpts := backupFlags(flag.CommandLine)
	maxSprites := flag.Int("max-sprites", 0, "most sprites that may exist; creating another is refused (0 = no limit)")
	maxRunning := flag.Int("max-running", 0, "most sprites that may run at once; waking another is refused until one goes idle (0 = no limit)")
	diskReserve := flag.Int64("disk-reserve-mib", 2048, "free space (MiB) a create, checkpoint or restore must leave on the sprite volume, or it is refused (0 = never refuse)")
	diskWarn := flag.Int("disk-warn-percent", 10, "warn in the log while less than this share of the sprite volume is free (0 = never)")
	var webhooks []string
	flag.Func("webhook", "POST every event (see docs/events.md) as JSON to this URL, signed with the webhook secret; repeatable", func(v string) error {
		if !strings.HasPrefix(v, "http://") && !strings.HasPrefix(v, "https://") {
			return errors.New("want an http:// or https:// URL")
		}
		webhooks = append(webhooks, v)
		return nil
	})
	webhookSecret := flag.String("webhook-secret-file", "", "the HMAC key for webhook signatures (default <data>/webhook-secret, generated on first use)")
	webhookTypes := flag.String("webhook-types", "", "comma-separated event type prefixes to send to webhooks, e.g. sprite.,service.crashed (default: every event)")
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
		IdleTimeout: *idle, WarmTTL: *warmTTL, DefaultVCPUs: *vcpus, DefaultMemMiB: *mem, DNS: *dns, NoNetwork: !*netOn, NoControl: !*control, ControlForGoSDK: *controlGo,
		AutoCheckpointInterval: *autoEvery, AutoCheckpointKeep: *autoKeep, GuestCheckpointLimit: *guestLimit,
		NetdSocket: *netdSocket, Backup: backupOpts(),
		MaxSprites: *maxSprites, MaxRunning: *maxRunning, DiskReserve: *diskReserve << 20, DiskWarnPercent: *diskWarn,
		Listen: *listen,
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
	if len(webhooks) > 0 {
		path := *webhookSecret
		if path == "" {
			path = filepath.Join(abs, "webhook-secret")
		}
		secret, err := loadSecret(path, "")
		if err != nil {
			fatal(log, fmt.Errorf("webhook secret: %w", err))
		}
		opts.Webhooks = server.WebhookOptions{URLs: webhooks, Secret: secret}
		if *webhookTypes != "" {
			opts.Webhooks.Types = strings.Split(*webhookTypes, ",")
		}
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
	api.StartMetrics()
	srv := &http.Server{Addr: *listen, ReadHeaderTimeout: 10 * time.Second, Handler: api.Handler()}
	srv.RegisterOnShutdown(api.CloseEvents) // event streams never finish on their own

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
		getCert := cs.GetCertificate
		if *domainsOn {
			getCert = api.EnableCustomDomains(context.Background(), server.DomainConfig{
				ACMEDir: filepath.Join(abs, "acme"), DirectoryURL: *acmeDir, Email: *acmeEmail,
				Resolver: *domainResolver, PerSprite: *domainsPer, Total: *domainsTotal, OrdersPerHour: *domainOrders,
			}, getCert)
		}
		public = server.NewPublicServer(api.PublicHandler(), getCert)
		go func() {
			log.Info("serving sprite URLs to the public", "addr", *publicListen, "urls", fmt.Sprintf(urlFmt, "<name>"))
			err := public.ServeTLS(server.LimitListener(ln, *publicConns, *publicConnsPer), "", "")
			if !errors.Is(err, http.ErrServerClosed) {
				fatal(log, err)
			}
		}()
	}
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
	if public != nil {
		public.Close()
	}
	srv.Shutdown(ctx) // in-flight exec sessions are cut off; their sprites still suspend warm
	srv.Close()
	api.SaveHTTPStats()
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
