package main

import (
	"errors"
	"flag"
	"os"
	"strings"
	"time"

	"github.com/arugula-salad/wisp/internal/certs"
	"github.com/arugula-salad/wisp/internal/server"
)

// daemonFlags are the flags main wires up itself: the data directory, the
// listeners, certificates and custom domains, confinement and webhooks.
// Everything else goes straight into server.Options.
type daemonFlags struct {
	data, listen, apiListen, urlDomain, org string

	publicListen                           string
	publicPort                             int
	tlsCert, tlsKey, acmeEmail, acmeDir    string
	publicConns, publicConnsPer            int
	domainsOn                              bool
	domainResolver                         string
	domainsPer, domainsTotal, domainOrders int

	confineMode string

	webhooks                    []string
	webhookSecret, webhookTypes string
}

// parseFlags reads the daemon's command line. The paths under the data
// directory and the confiner are main's to fill in.
func parseFlags() (server.Options, daemonFlags) {
	var o server.Options
	var f daemonFlags
	flag.StringVar(&f.data, "data", defaultDataDir(), "data directory")
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
	netOn := flag.Bool("net", true, "attach sprites to the msbr0 tap pool; only one wispd per host may own it, so run extra dev/test instances with --net=false")
	flag.DurationVar(&o.AutoCheckpointInterval, "auto-checkpoint-interval", time.Hour, "take an automatic checkpoint of a sprite whose disk changed and whose newest checkpoint is older than this (0 = only before restores)")
	flag.IntVar(&o.AutoCheckpointKeep, "auto-checkpoint-keep", 3, "automatic checkpoints kept per sprite; each is a full disk clone (0 = take none)")
	flag.IntVar(&o.GuestCheckpointLimit, "guest-checkpoint-limit", 20, "most checkpoints a sprite may hold when creating one from inside via sprite-env; the API is not limited (0 = no limit)")
	flag.StringVar(&o.NetdSocket, "netd-socket", "", "wisp-netd socket, the root helper that backs restrictive network policies (default /run/wisp/netd.sock)")
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
	backupOpts := backupFlags(flag.CommandLine)
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
	flag.Parse()

	o.Host.NoFreePageReporting = !*fpr
	o.NoNetwork, o.NoControl = !*netOn, !*control
	o.Backup = backupOpts()
	o.DiskReserve = *diskReserve << 20
	o.Listen, o.APIHosts = f.listen, parseHosts(*apiHosts)
	return o, f
}
