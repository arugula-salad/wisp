// wispd is a single-host implementation of the Sprites API: persistent,
// hardware-isolated Linux environments backed by Firecracker microVMs that
// suspend when idle and wake on demand.
package main

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"hash/fnv"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/arugula-salad/wisp/internal/certs"
	"github.com/arugula-salad/wisp/internal/confine"
	"github.com/arugula-salad/wisp/internal/server"
	"github.com/arugula-salad/wisp/internal/store"
)

func defaultDataDir() string {
	if d := os.Getenv("WISP_DATA"); d != "" {
		return d
	}
	if d := os.Getenv("XDG_DATA_HOME"); d != "" {
		return filepath.Join(d, "wisp")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "share", "wisp")
}

// loadToken reads the API token, generating one on first run.
func loadToken(path string) (string, error) { return loadSecret(path, "wisproot_") }

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

	opts, f := parseFlags()

	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	abs, err := filepath.Abs(f.data)
	if err != nil {
		fatal(log, err)
	}
	// Machine dirs hold unix sockets, whose paths are capped at 108 bytes.
	if n := len(filepath.Join(abs, "vm", "0123456789ab", "fc.sock")); n > 100 {
		fatal(log, fmt.Errorf("data directory path is too long for unix sockets (%d bytes): %s", n, abs))
	}
	opts.DataDir, opts.BaseImage = abs, filepath.Join(abs, "images", "base.ext4")
	opts.Host.Firecracker = filepath.Join(abs, "bin", "firecracker")
	opts.Host.Kernel = filepath.Join(abs, "kernel", "vmlinux")
	opts.Host.Initrd = filepath.Join(abs, "initrd.cpio")
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

	mode, err := confine.ParseMode(f.confineMode)
	if err != nil {
		fatal(log, err)
	}
	// One cgroup subtree per data directory, so a second wispd (dev, tests)
	// on the same host does not sweep away the first one's VM cgroups.
	conf, err := confine.Open(mode, "wisp-"+cgroupTag(abs), log)
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
	if len(f.webhooks) > 0 {
		path := f.webhookSecret
		if path == "" {
			path = filepath.Join(abs, "webhook-secret")
		}
		secret, err := loadSecret(path, "")
		if err != nil {
			fatal(log, fmt.Errorf("webhook secret: %w", err))
		}
		opts.Webhooks = server.WebhookOptions{URLs: f.webhooks, Secret: secret}
		if f.webhookTypes != "" {
			opts.Webhooks.Types = strings.Split(f.webhookTypes, ",")
		}
	}
	life := server.NewLifecycle(opts, st, log)

	urlDomains, err := parseURLDomains(f.urlDomain)
	if err != nil {
		fatal(log, err)
	}
	_, port, _ := net.SplitHostPort(f.listen)
	urlFmt := "http://%s.%s:" + port
	if f.publicListen != "" {
		urlFmt = "https://%s.%s"
		if f.publicPort != 443 {
			urlFmt += fmt.Sprintf(":%d", f.publicPort)
		}
	}
	opts.Org, opts.URLDomains, opts.URLFormat = f.org, urlDomains, urlFmt
	api := server.New(opts, st, life, log, token)
	api.StartMetrics()
	srv := &http.Server{Addr: f.listen, ReadHeaderTimeout: 10 * time.Second, Handler: api.Handler()}
	srv.RegisterOnShutdown(api.CloseEvents) // event streams never finish on their own
	var bearerSrv *http.Server
	if f.apiListen != "" {
		bearerSrv = &http.Server{Addr: f.apiListen, ReadHeaderTimeout: 10 * time.Second, Handler: api.BearerHandler()}
		bearerSrv.RegisterOnShutdown(api.CloseEvents)
	}

	var public *http.Server
	if f.publicListen != "" {
		getCert, err := publicCerts(abs, urlDomains, f.tlsCert, f.tlsKey, f.acmeEmail, f.acmeDir, log)
		if err != nil {
			fatal(log, err)
		}
		ln, err := net.Listen("tcp", f.publicListen)
		if err != nil {
			fatal(log, err)
		}
		if f.domainsOn {
			getCert = api.EnableCustomDomains(context.Background(), server.DomainConfig{
				ACMEDir: filepath.Join(abs, "acme"), DirectoryURL: f.acmeDir, Email: f.acmeEmail,
				Resolver: f.domainResolver, PerSprite: f.domainsPer, Total: f.domainsTotal, OrdersPerHour: f.domainOrders,
			}, getCert)
		}
		public = server.NewPublicServer(api.PublicHandler(), getCert)
		go func() {
			log.Info("serving sprite URLs to the public", "addr", f.publicListen, "urls", fmt.Sprintf(urlFmt, "<name>", strings.Join(urlDomains, "|")))
			err := public.ServeTLS(server.LimitListener(ln, f.publicConns, f.publicConnsPer), "", "")
			if !errors.Is(err, http.ErrServerClosed) {
				fatal(log, err)
			}
		}()
	}
	go http.Serve(statusLn, api.StatusHandler(f.listen))

	go func() {
		log.Info("wispd listening", "addr", f.listen, "data", abs, "token_file", filepath.Join(abs, "token"))
		if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
			fatal(log, err)
		}
	}()
	if bearerSrv != nil {
		go func() {
			log.Info("serving the bearer API alone", "addr", f.apiListen)
			if err := bearerSrv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
				fatal(log, err)
			}
		}()
	}

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	log.Info("shutting down: suspending running sprites")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if public != nil {
		public.Close()
	}
	for _, hs := range []*http.Server{srv, bearerSrv} {
		if hs != nil {
			hs.Shutdown(ctx) // in-flight exec sessions are cut off; their sprites still suspend warm
			hs.Close()
		}
	}
	api.SaveHTTPStats()
	life.Shutdown()
}

// parseURLDomains reads --url-domain: one domain, or several separated by commas.
func parseURLDomains(flagValue string) ([]string, error) {
	var domains []string
	for _, d := range strings.Split(flagValue, ",") {
		d = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(d)), ".")
		if d == "" {
			continue
		}
		// One may be nested in another (arugula.io, games.arugula.io): a host
		// belongs to the most specific (server.URLDomainUnder).
		if slices.Contains(domains, d) {
			return nil, fmt.Errorf("--url-domain: %s is listed twice", d)
		}
		domains = append(domains, d)
	}
	if len(domains) == 0 {
		return nil, errors.New("--url-domain is empty")
	}
	return domains, nil
}

func parseHosts(flagValue string) []string {
	var hosts []string
	for _, h := range strings.Split(flagValue, ",") {
		if h = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(h)), "."); h != "" {
			hosts = append(hosts, h)
		}
	}
	return hosts
}

// publicCerts is the certificate source for the public listener: the operator's
// own PEM pair (one domain only), or else a wildcard per URL domain that we keep
// current ourselves over ACME, chosen by the name the client asks for. The first
// domain's pair stays where it always was.
func publicCerts(data string, domains []string, certFile, keyFile, email, directory string, log *slog.Logger) (func(*tls.ClientHelloInfo) (*tls.Certificate, error), error) {
	if (certFile == "") != (keyFile == "") {
		return nil, errors.New("--tls-cert and --tls-key go together")
	}
	if certFile != "" {
		if len(domains) > 1 {
			return nil, errors.New("--tls-cert covers one --url-domain; leave it out to have a certificate kept for each")
		}
		cs := certs.NewStore(certFile, keyFile, log)
		return cs.GetCertificate, cs.Load()
	}
	for _, domain := range domains {
		if domain == "localhost" || strings.HasSuffix(domain, ".localhost") {
			return nil, fmt.Errorf("--public-listen needs --url-domain set to domains you control, not %s", domain)
		}
	}
	cfToken := os.Getenv("CLOUDFLARE_API_TOKEN")
	if b, err := os.ReadFile(filepath.Join(data, "cloudflare-token")); cfToken == "" && err == nil {
		cfToken = strings.TrimSpace(string(b))
	}
	if cfToken == "" {
		return nil, fmt.Errorf("--public-listen needs a certificate: pass --tls-cert and --tls-key, or put a Cloudflare API token in $CLOUDFLARE_API_TOKEN or %s", filepath.Join(data, "cloudflare-token"))
	}
	stores := make([]*certs.Store, len(domains))
	for i, domain := range domains {
		mgr := &certs.Manager{Domain: domain, Email: email, Dir: filepath.Join(data, "acme"), DirectoryURL: directory,
			DNS: &certs.Cloudflare{Token: cfToken}, Log: log}
		if i > 0 {
			mgr.Subdir = filepath.Join("url-domains", domain)
		}
		stores[i] = certs.NewStore(mgr.CertFile(), mgr.KeyFile(), log)
		go mgr.Run(context.Background(), stores[i])
	}
	return func(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
		name := strings.TrimSuffix(strings.ToLower(hello.ServerName), ".")
		// The most specific domain: x.games.arugula.io needs *.games.arugula.io, not *.arugula.io.
		if domain, ok := server.URLDomainUnder(domains, name); ok {
			return stores[slices.Index(domains, domain)].GetCertificate(hello)
		}
		// A domain's own apex (not nested in another, or it was matched above): its store, as before.
		if i := slices.Index(domains, name); i >= 0 {
			return stores[i].GetCertificate(hello)
		}
		return stores[0].GetCertificate(hello) // no SNI, or a name we do not serve: as before
	}, nil
}

// cgroupTag derives a stable, filesystem-safe suffix from the data directory,
// so two wispd instances on one host get separate cgroup subtrees.
func cgroupTag(dataDir string) string {
	h := fnv.New32a()
	h.Write([]byte(dataDir))
	return hex.EncodeToString(h.Sum(nil))
}

func fatal(log *slog.Logger, err error) {
	log.Error(err.Error())
	os.Exit(1)
}
