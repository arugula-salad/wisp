// Package daemon is what runs a wisp daemon: the data directory and its
// operator socket, the engine, the Sprites front end and its listeners, and
// the shutdown. wispd is this with the Sprites API alone; sandboxd adds more
// front ends (Frontend), each on a listener of its own, over the same engine,
// store and API keys.
package daemon

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

	"github.com/arugula-salad/wisp/engine"
	"github.com/arugula-salad/wisp/internal/certs"
	"github.com/arugula-salad/wisp/internal/confine"
	"github.com/arugula-salad/wisp/internal/server"
	"github.com/arugula-salad/wisp/internal/store"
)

// DefaultDataDir is --data's default: $WISP_DATA, else wisp under the XDG
// data directory.
func DefaultDataDir() string {
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
func loadToken(path string) (string, error) { return LoadSecret(path, "wisproot_") }

// LoadSecret reads a secret from path, generating a random one on first run.
func LoadSecret(path, prefix string) (string, error) {
	if b, err := os.ReadFile(path); err == nil {
		return strings.TrimSpace(string(b)), nil
	}
	b := make([]byte, 24)
	rand.Read(b)
	tok := prefix + hex.EncodeToString(b)
	return tok, os.WriteFile(path, []byte(tok+"\n"), 0o600)
}

// A Frontend is another API served from the daemon's engine beside the
// Sprites API, on a listener of its own.
type Frontend struct {
	// Name is what the log calls it.
	Name string
	// Addr is its listen address; empty leaves it off.
	Addr string
	// IDLen is the longest sandbox ID it makes, which the data directory's
	// unix socket paths have to leave room for. 0 is a Sprites ID's.
	IDLen int
	// Setup builds its handler once the engine and the Sprites front end
	// exist, before anything is served or boots, so that hooks it installs on
	// the engine (OnBoot) see every VM.
	Setup func(Env) (http.Handler, error)
}

// Env is what a Frontend is built from.
type Env struct {
	DataDir string
	Options server.Options
	Store   *store.Store
	Engine  *engine.Engine
	Sprites *server.Server
	Log     *slog.Logger
}

// spriteIDLen is store.NewID's.
const spriteIDLen = 12

// Run runs the daemon until SIGINT or SIGTERM: the Sprites API on --listen
// (and --api-listen, --public-listen), and each of extra on its own address.
// It returns only by exiting.
func Run(name string, opts server.Options, f *Flags, extra ...Frontend) {
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	abs, err := filepath.Abs(f.data)
	if err != nil {
		fatal(log, err)
	}
	// Machine dirs hold unix sockets, whose paths are capped at 108 bytes.
	idLen := spriteIDLen
	for _, fe := range extra {
		if fe.Addr != "" {
			idLen = max(idLen, fe.IDLen)
		}
	}
	if n := len(filepath.Join(abs, "vm", strings.Repeat("0", idLen), "fc.sock")); n > 100 {
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
	conf, err := confine.Open(mode, "wisp-"+cgroupTag(abs), f.cgroupCaps, log)
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
		secret, err := LoadSecret(path, "")
		if err != nil {
			fatal(log, fmt.Errorf("webhook secret: %w", err))
		}
		opts.Webhooks = server.WebhookOptions{URLs: f.webhooks, Secret: secret}
		if f.webhookTypes != "" {
			opts.Webhooks.Types = strings.Split(f.webhookTypes, ",")
		}
	}
	life := engine.New(opts.Options, st, log)

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
	var others []*http.Server
	for _, fe := range extra {
		if fe.Addr == "" {
			continue
		}
		h, err := fe.Setup(Env{DataDir: abs, Options: opts, Store: st, Engine: life, Sprites: api, Log: log})
		if err != nil {
			fatal(log, fmt.Errorf("%s: %w", fe.Name, err))
		}
		hs := &http.Server{Addr: fe.Addr, ReadHeaderTimeout: 10 * time.Second, Handler: h}
		// h2c too: an SDK told to speak HTTP/2 to a plain http:// URL does it
		// with prior knowledge.
		hs.Protocols = new(http.Protocols)
		hs.Protocols.SetHTTP1(true)
		hs.Protocols.SetUnencryptedHTTP2(true)
		others = append(others, hs)
		go func() {
			log.Info("serving "+fe.Name, "addr", fe.Addr)
			if err := hs.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
				fatal(log, fmt.Errorf("%s: %w", fe.Name, err))
			}
		}()
	}
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
		log.Info(name+" listening", "addr", f.listen, "data", abs, "token_file", filepath.Join(abs, "token"))
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
	for _, hs := range append([]*http.Server{srv, bearerSrv}, others...) {
		if hs != nil {
			hs.Shutdown(ctx) // in-flight exec sessions are cut off; their sprites still suspend warm
			hs.Close()
		}
	}
	api.SaveHTTPStats()
	life.Shutdown()
}

// listenStatus opens the operator socket. A daemon already answering there
// owns this data directory; two of them would fight over every sprite in it.
func listenStatus(dataDir string) (net.Listener, error) {
	path := filepath.Join(dataDir, server.StatusSocket)
	if c, err := net.DialTimeout("unix", path, time.Second); err == nil {
		c.Close()
		return nil, fmt.Errorf("another wispd is already running on %s (its socket %s answers)", dataDir, path)
	}
	os.Remove(path) // left behind by a daemon that died
	old := syscall.Umask(0o177)
	defer syscall.Umask(old)
	return net.Listen("unix", path)
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
