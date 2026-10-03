package daemon

import (
	"errors"
	"flag"
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/arugula-salad/wisp/engine"
	"github.com/arugula-salad/wisp/internal/certs"
	"github.com/arugula-salad/wisp/internal/confine"
	"github.com/arugula-salad/wisp/internal/server"
)

// Flags are the flags Run wires up itself: the data directory, the
// listeners, certificates and custom domains, confinement and webhooks.
// Everything else goes straight into server.Options.
type Flags struct {
	data, listen, apiListen, urlDomain, org string

	publicListen                           string
	publicPort                             int
	tlsCert, tlsKey, acmeEmail, acmeDir    string
	publicConns, publicConnsPer            int
	domainsOn                              bool
	domainResolver                         string
	domainsPer, domainsTotal, domainOrders int

	confineMode string
	cgroupCaps  confine.Caps

	webhooks                    []string
	webhookSecret, webhookTypes string

	// proxied is the Sprites API's public URL behind a reverse proxy
	// (BehindProxy); nil without one.
	proxied *url.URL
}

// Bind registers the daemon's flags on fs; a daemon that has flags of its
// own (sandboxd) registers them too, and parses. finish then reads what was
// parsed. The paths under the data directory and the confiner are Run's to
// fill in.
func Bind(fs *flag.FlagSet) (finish func() (server.Options, *Flags)) {
	var o server.Options
	f := &Flags{}
	flag := fs // the definitions below read as they did on the global set
	flag.StringVar(&f.data, "data", DefaultDataDir(), "data directory")
	flag.StringVar(&f.listen, "listen", "127.0.0.1:7788", "API listen address")
	flag.StringVar(&f.apiListen, "api-listen", "", "a second listen address serving the bearer API alone, whatever the Host: no dashboard and no sprite URLs. Point a reverse proxy that publishes the API here rather than at --listen")
	flag.DurationVar(&o.IdleTimeout, "idle-timeout", 30*time.Second, "suspend a sprite after this long with no activity")
	flag.DurationVar(&o.WarmTTL, "warm-ttl", time.Hour, "drop a suspended sprite's memory state (go cold) after this long")
	flag.IntVar(&o.DefaultVCPUs, "vcpus", 8, "default vCPUs per sprite")
	flag.IntVar(&o.DefaultMemMiB, "mem-mib", 2048, "default guest RAM per sprite (MiB); a warm snapshot takes what the guest was using, up to this")
	fpr := flag.Bool("free-page-reporting", true, "guests hand freed memory back to the host while they run (balloon free page reporting); memory a guest frees and then touches again is re-faulted, ~2 s/GiB. Takes effect at each sprite's next cold boot")
	flag.StringVar(&o.DNS, "dns", "1.1.1.1,8.8.8.8", "nameservers handed to guests")
	flag.StringVar(&f.urlDomain, "url-domain", "sprites.localhost", "sprite URLs are <name>.<url-domain>; to serve them beyond this machine, point a wildcard DNS record here and see --public-listen. A comma-separated list serves several: each sprite is under one of them (url_domain when it is created, its parent's when a sprite makes it), and the first is the default")
	apiHosts := flag.String("api-host", "", "comma-separated names a reverse proxy serves the API listener under (e.g. wisp.widgets.wtf). They reach the bearer API alone: never a sprite, even under a --url-domain, and never the dashboard, whose cookie counts for nothing there")
	control := flag.Bool("control", true, "serve the multiplexed /control channel; --control=false makes every SDK fall back to per-operation WebSockets")
	flag.BoolVar(&o.ControlForGoSDK, "control-for-go-sdk", false, "also offer /control to the official Go SDK (by default it is answered 404 there and falls back to per-operation WebSockets, because its ProxyPorts races on a control socket)")
	netOn := flag.Bool("net", true, "attach sprites to the --net-pool tap pool (bridge msbr0 for pool 0); only one wispd per host may own a pool, so run extra dev/test instances with --net=false or on another --net-pool")
	flag.IntVar(&o.NetPool, "net-pool", 0, "host network pool to use: bridge msbrN, taps msNtap*, nft table inet wispN and wisp-netd at /run/wispN/netd.sock (pool 0 is msbr0, mstap*, inet wisp, /run/wisp/netd.sock). A second pool, made with 'sudo WISP_POOL=1 scripts/setup-host.sh', lets a networked wispd (a test stack) run beside another that owns pool 0")
	flag.DurationVar(&o.AutoCheckpointInterval, "auto-checkpoint-interval", time.Hour, "take an automatic checkpoint of a sprite whose disk changed and whose newest checkpoint is older than this (0 = only before restores)")
	flag.IntVar(&o.AutoCheckpointKeep, "auto-checkpoint-keep", 3, "automatic checkpoints kept per sprite; each is a full disk clone (0 = take none)")
	flag.IntVar(&o.GuestCheckpointLimit, "guest-checkpoint-limit", 20, "most checkpoints a sprite may hold when creating one from inside via sprite-env; the API is not limited (0 = no limit)")
	flag.StringVar(&o.NetdSocket, "netd-socket", "", "wisp-netd socket, the root helper that backs restrictive network policies (default /run/wisp/netd.sock, /run/wispN/netd.sock with --net-pool N)")
	flag.StringVar(&f.org, "org", "local", "organization name reported in API responses")
	flag.StringVar(&f.publicListen, "public-listen", "", "serve sprite URLs, and only sprite URLs, over HTTPS on this address; the one to forward a router port to. Needs --url-domain set to a real domain with a wildcard record, and a certificate: --tls-cert/--tls-key, or a Cloudflare token for an automatic one")
	flag.IntVar(&f.publicPort, "public-port", 443, "the port clients reach --public-listen on (the router's side of the forward); used in the URLs the API reports")
	flag.StringVar(&f.tlsCert, "tls-cert", "", "PEM certificate chain for *.<url-domain>; re-read when it changes, so an external ACME client can renew it in place")
	flag.StringVar(&f.tlsKey, "tls-key", "", "PEM private key for --tls-cert")
	flag.StringVar(&f.acmeEmail, "acme-email", "", "contact address for the ACME account (optional)")
	flag.StringVar(&f.acmeDir, "acme-directory", certs.LetsEncrypt, "ACME directory used when no --tls-cert is given. A wildcard certificate is requested over DNS-01 through Cloudflare, with the API token read from $CLOUDFLARE_API_TOKEN or <data>/cloudflare-token (needs Zone:Read and DNS:Edit on the zone)")
	flag.IntVar(&f.publicConns, "public-max-conns", 1024, "open connections allowed on --public-listen (0 = no limit)")
	flag.IntVar(&f.publicConnsPer, "public-max-conns-per-client", 64, "open connections allowed per IPv4 address or IPv6 /64 on --public-listen (0 = no limit; use 0 behind a CDN, where every client shares the CDN's addresses)")
	flag.BoolVar(&f.domainsOn, "custom-domains", true, "with --public-listen, let sprites have custom domains (POST /v1/sprites/<name>/domains), each with its own certificate from --acme-directory over TLS-ALPN-01")
	flag.StringVar(&f.domainResolver, "domain-resolver", "1.1.1.1:53", "recursive DNS server that checks a custom domain points here before a certificate is requested for it")
	flag.IntVar(&f.domainsPer, "max-domains-per-sprite", 5, "custom domains one sprite may have (0 = no limit)")
	flag.IntVar(&f.domainsTotal, "max-domains", 50, "custom domains across all sprites (0 = no limit)")
	flag.IntVar(&f.domainOrders, "acme-orders-per-hour", 10, "certificate orders per hour for custom domains, across all of them; keeps a misconfigured domain from spending the CA's rate limits")
	flag.StringVar(&f.confineMode, "confine", os.Getenv("WISP_CONFINE"), "sandbox each Firecracker with Landlock + a cgroup: \"best-effort\" (default; apply what the kernel supports and log the rest), \"strict\" (refuse to start without both) or \"off\"")
	cgMemMax := flag.String("cgroup-memory-max", "", "hard memory cap (cgroup memory.max) on all of this daemon's VMs together: bytes or a size like 32G, or \"max\" to clear one. Written on its VM cgroup subtree (app.slice/wisp-<tag>), which a systemd MemoryMax= on the unit does not cover. Empty leaves memory.max as it is, including a cap an earlier run wrote. Without a cgroup subtree it warns, or with --confine=strict refuses to start")
	flag.IntVar(&f.cgroupCaps.CPUWeight, "cgroup-cpu-weight", 0, "CPU share (cgroup cpu.weight, 1-10000; the kernel default is 100) of all of this daemon's VMs together against the rest of the host, on the same subtree as --cgroup-memory-max. 0 leaves it as it is")
	backupOpts := BackupFlags(fs)
	flag.IntVar(&o.MaxSprites, "max-sprites", 0, "most sprites that may exist; creating another is refused (0 = no limit)")
	flag.IntVar(&o.MaxRunning, "max-running", 0, "most sprites that may run at once; waking another is refused until one goes idle (0 = no limit)")
	flag.IntVar(&o.MaxRunningMemoryMiB, "max-running-memory-mib", 0, "guest RAM (MiB) all running sprites together may hold; waking another is refused until one goes idle (0 = no budget). Each sprite is counted at its ceiling (its memory limit + 128 MiB, or --mem-mib), because a guest with memory autoscale may deflate its balloon back up to that at any time. Set it below this host's RAM: page cache, Firecracker overhead and everything else on the host are not counted")
	flag.IntVar(&o.MaxConcurrentBoots, "max-concurrent-boots", 0, "cold boots that may be in flight at once; another is refused with a retryable error rather than queued (0 = no limit). Resumes are not capped")
	flag.DurationVar(&o.LeaseWarning, "lease-warning", 5*time.Minute, "how long before a workspace lease expires the sprite.expiring event goes out (0 = the 5 minute default)")
	flag.DurationVar(&o.URLReadyWait, "url-ready-wait", 10*time.Second, "how long a sprite URL waits for the app inside to accept a connection before answering 503; covers a cold boot and the app's own start (0 = fail on the first refused connection, 60s ceiling)")
	diskReserve := flag.Int64("disk-reserve-mib", 2048, "free space (MiB) a create, checkpoint or restore must leave on the sprite volume, or it is refused (0 = never refuse)")
	flag.IntVar(&o.DiskWarnPercent, "disk-warn-percent", 10, "warn in the log while less than this share of the sprite volume is free (0 = never)")
	flag.Func("webhook", "POST every event (see docs/events.md) as JSON to this URL, signed with the webhook secret; repeatable", func(v string) error {
		if !strings.HasPrefix(v, "http://") && !strings.HasPrefix(v, "https://") {
			return errors.New("want an http:// or https:// URL")
		}
		f.webhooks = append(f.webhooks, v)
		return nil
	})
	flag.StringVar(&f.webhookSecret, "webhook-secret-file", "", "the HMAC key for webhook signatures (default <data>/webhook-secret, generated on first use)")
	flag.StringVar(&f.webhookTypes, "webhook-types", "", "comma-separated event type prefixes to send to webhooks, e.g. sprite.,service.crashed (default: every event)")
	return func() (server.Options, *Flags) {
		if o.NetPool < 0 {
			fmt.Fprintln(os.Stderr, "--net-pool must not be negative")
			os.Exit(2)
		}
		var err error
		if f.cgroupCaps.MemoryMax, err = confine.ParseMemoryMax(*cgMemMax); err != nil {
			fmt.Fprintln(os.Stderr, "--cgroup-memory-max:", err)
			os.Exit(2)
		}
		if err := f.cgroupCaps.Validate(); err != nil {
			fmt.Fprintln(os.Stderr, "--cgroup-cpu-weight:", err)
			os.Exit(2)
		}
		o.Host.NoFreePageReporting = !*fpr
		o.NoNetwork, o.NoControl = !*netOn, !*control
		o.Backup = backupOpts()
		o.DiskReserve = *diskReserve << 20
		o.Listen, o.APIHosts = f.listen, parseHosts(*apiHosts)
		return o, f
	}
}

// BehindProxy serves the Sprites API to the public through a reverse proxy
// that terminates TLS for u's host and every name under it, and forwards them
// to --listen with the Host intact (sandboxd's --sprites-public-url). u's host
// becomes an --api-host, the bearer API alone, and the one URL domain: sprite
// URLs are <scheme>://<name>.<u's host>, served on --listen and told no more
// than --public-listen would tell. It replaces --url-domain and --public-listen,
// which the caller must refuse beside it. u is http(s)://host[:port] and nothing
// else.
func (f *Flags) BehindProxy(opts *server.Options, u *url.URL) {
	f.proxied = u
	f.urlDomain = u.Hostname()
	opts.APIHosts = append(opts.APIHosts, parseHosts(u.Hostname())...)
	opts.URLsProxied = true
}

// Listen is the Sprites API's address, --listen.
func (f *Flags) Listen() string { return f.listen }

// BackupFlags registers the bucket flags on fs, shared by the daemon and both
// subcommands so that the same arguments work everywhere.
func BackupFlags(fs *flag.FlagSet) func() engine.BackupOptions {
	endpoint := fs.String("backup-endpoint", "", "S3 endpoint for the backup tier, e.g. http://garage-s3:3900")
	bucket := fs.String("backup-bucket", "", "S3 bucket for sprite backups (empty disables backups entirely)")
	region := fs.String("backup-region", "us-east-1", "S3 region the bucket reports, e.g. home-cloud for Garage")
	creds := fs.String("backup-credentials-file", "", "file of AWS_ACCESS_KEY_ID / AWS_SECRET_ACCESS_KEY lines (default: the environment)")
	key := fs.String("backup-key-file", "", "32-byte key (64 hex chars) enabling client-side encryption; a key kept only on this machine is not a backup unless you copy it elsewhere")
	parallel := fs.Int("backup-parallel", 4, "concurrent chunk transfers; each holds 4 MiB")
	rate := fs.Int64("backup-rate-limit", 0, "cap backup traffic in bytes/second (0 = unlimited)")
	interval := fs.Duration("backup-interval", 6*time.Hour, "re-upload a sprite whose disk changed this long after its last backup; also the retry for a failed one (0 = only on suspend)")
	retention := fs.Duration("backup-retention", 30*24*time.Hour, "how long a deleted sprite's backups are kept by `wispd backups prune`")
	keep := fs.Int("backup-keep", 0, "manifests to keep per sprite in `wispd backups prune` (0 = all)")
	return func() engine.BackupOptions {
		return engine.BackupOptions{Endpoint: *endpoint, Bucket: *bucket, Region: *region,
			CredentialsFile: *creds, KeyFile: *key, Parallel: *parallel, RateLimit: *rate,
			Interval: *interval, Retention: *retention, Keep: *keep}
	}
}
