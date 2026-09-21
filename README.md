# mini-sprites

A single-host implementation of the [Sprites](https://sprites.dev) API: persistent,
hardware-isolated Linux environments on Firecracker microVMs that suspend when idle
and wake on the next request. The official `sprite` CLI and SDKs work against it
unmodified.

This is an independent, unofficial project. It is not affiliated with or endorsed by Fly.io;
"Sprites" is their product, and this only implements a compatible API for running on your
own machine.

```
client (sprite CLI / SDK / curl)
   │  REST + WebSocket, Bearer token              http://<name>.sprites.localhost:7788
   ▼                                                        │
spritesd ── lifecycle engine: wake on request, suspend when idle, go cold after a TTL
   │  vsock, both directions (no guest network needed)
   ▼
Firecracker microVM ── sprite-agent as PID 1 (from an initramfs) ── your ext4 disk
                       └─ /.sprite/api.sock + sprite-env, for use from inside
```

## Quick start

Needs Linux with read/write access to `/dev/kvm`, Go, and rootless podman. No root.

```sh
make deps image        # firecracker + guest kernel, then the base disk image
make run               # builds spritesd + the agent initrd and starts on 127.0.0.1:7788

export SPRITES_API_URL=http://127.0.0.1:7788
export SPRITE_TOKEN=$(cat ~/.local/share/mini-sprites/token)
sprite create dev && sprite exec -s dev -- uname -a
```

### Instant clones (optional, needs root once)

```sh
sudo apt install xfsprogs
sudo ./scripts/setup-storage.sh      # stop spritesd first; SPRITE_VOLUME_GB=40 by default
```

Puts the sprite directory on a loop-mounted XFS volume with reflinks, so creating a sprite,
taking a checkpoint and restoring one are instant and share disk blocks until written
(measured: create 13 ms, checkpoint 2 ms, restore 69 ms; six 20 GB images in 1.7 GB). Warm
snapshots live on the volume too, at one guest-RAM-sized file per suspended sprite, so size
it for both; suspend is a little slower through the loop device (~1.6 s vs ~1.2 s). The
script migrates existing sprites, never sizes the volume beyond what the host disk can hold,
and `--remove` moves everything back. spritesd needs no configuration: it probes the
filesystem at startup and logs which mode it is in.

### Guest networking (the one thing that needs root, once)

```sh
make netd                        # optional: the helper that makes network policies enforceable
sudo ./scripts/setup-host.sh     # bridge (10.209.0.0/16) + tap pool + NAT/isolation rules + boot units
```

Sprites get outbound internet but cannot reach each other, the host, or private/LAN/tailnet
ranges. Without the setup sprites simply have no NIC; exec, checkpoints, sprite URLs and the
TCP proxy still work because they travel over vsock.

What the script knows about because it bit us:
- It refuses a subnet that overlaps an existing route (`MINI_SPRITES_NET_PREFIX` picks another
  /16). podman owns 10.88/16 by default, and an overlap silently steals all return traffic.
  spritesd reads the network back off the bridge, so the script is the only place it is set.
- If ufw is active it adds `ufw route allow in on msbr0` and input allowances for the two
  policy ports: ufw's policies are DROP, and a drop in any netfilter table is final.
- `--print-rules` shows the nftables ruleset without root; `--remove` undoes everything.

## Lifecycle

| State | What exists | Wake |
|---|---|---|
| `running` | Firecracker process | – |
| `warm` | memory snapshot on disk, processes frozen | ~15 ms VM restore |
| `cold` | disk only | ~200 ms boot to a responsive agent |

A sprite suspends after `--idle-timeout` (30s) with nothing keeping it awake.

- **Keeps it awake:** an API or sprite-URL request in flight, an attached exec session or a
  running control-channel operation, session I/O, an open filesystem watch, a live task, and
  checkpoint calls made from inside.
- **Does not:** services and their output, port notifications, filesystem events, and idle
  pooled `/control` or `ports/watch` sockets (these are closed at suspend).

It goes cold after `--warm-ttl` (1h), which drops memory state: processes are gone, the
filesystem is intact. TCP connections never survive a suspend. The guest clock is stepped to
host time on every resume. On SIGTERM, spritesd suspends every running sprite so they resume
warm after a restart.

**Tasks** are explicit keep-awake holds, created from inside the sprite as upstream does:

```sh
curl --unix-socket /.sprite/api.sock -X POST http://sprite/v1/tasks -d '{"name":"build","expire":"10m"}'
```

They last at most 1h, are refreshed with PUT, and do not survive a cold boot or a spritesd
restart. `/v1/sprites/{name}/tasks` exposes the same thing from outside (our extension).

## API coverage

- **Sprites**: CRUD, pagination, labels, URL settings.
- **Exec**: WebSocket TTY/non-TTY, detach/reattach with output replay,
  `max_run_after_disconnect`, signals, session list, kill, and HTTP POST exec in upstream's
  frame format.
- **Control**: the multiplexed `/control` channel (exec, proxy and `fs.*` operations).
- **Ports**: `port_opened`/`port_closed` notifications on exec sessions (`address` is the bind
  IP, which is what the CLI dials) and `ports/watch`.
- **Checkpoints**: create/list/get/delete/restore with streaming NDJSON, `history`, and
  automatic checkpoints (`auto-<n>`, hidden unless `includeAuto`).
- **Services**: create/get/list/delete, start/stop/restart with streamed logs, signal, `needs`
  ordering, crash restart with backoff, and one `http_port` service that the sprite URL routes
  to and starts on demand (otherwise the URL goes to port 8080). Definitions live on the
  sprite's disk in `/.sprite/services/`, logs in `/.sprite/logs/services/<name>.log`, so both
  travel with checkpoints. Every service starts on a cold boot.
- **Filesystem**: read, write (atomic), list, delete, rename, copy, chmod, chown, and `watch`
  (recursive, including directories created later; bounded, and a slow reader is told how many
  events it missed rather than stalling the agent). New files belong to the `sprite` user.
- **Policies**:
  - `policy/network`: a domain allowlist with `{"include":"defaults"}` and `*.` wildcards.
    Empty rules mean unrestricted. Changes apply live. See below for how it is enforced.
  - `policy/privileges`: capability profiles (`minimal`, `standard`, `privileged`) and
    `noNewPrivileges`, applied to processes started after the change.
  - `policy/resources`: a memory limit, as a guest cgroup immediately and as VM RAM of
    `limit_mb + 128` from the next cold boot.
- **Proxy / URLs**: the TCP proxy, and per-sprite URLs with `sprite`/`public` auth (see
  [Public sprite URLs](#public-sprite-urls) for serving them to the internet).

### From inside a sprite

`sprite-env` (at `/.sprite/bin`, on `$PATH`, reinstalled from the initramfs on every cold
boot) talks to `/.sprite/api.sock`, owned by the `sprite` user. It manages services and
checkpoints with no API token:

```sh
sprite-env services create web --cmd python3 --args "-m,http.server,3000" --http-port 3000
sprite-env checkpoints create && sprite-env checkpoints list
```

An old checkpoint can be browsed without restoring it:

```sh
sprite-env checkpoints mount v3          # read-only at /.sprite/checkpoints/v3
cp /.sprite/checkpoints/v3/home/sprite/app/config.yml ~/app/   # pull one file back from the past
sprite-env checkpoints unmount v3
```

Firecracker cannot hot-plug a drive but can swap the file behind one, so every VM boots with
four placeholder drives and a mount points one at the checkpoint's image: no copy, no reboot.
Mounts survive a warm suspend, are reset by a cold boot or a restore, and a mounted checkpoint
cannot be deleted. The sprite's network policy is readable at `/.sprite/policy/network.json`
(information only; enforcement is on the host).

Checkpoint calls ride a guest-initiated vsock channel to a per-VM listener bound to that one
sprite: the channel is the identity, and a guest cannot address anything but itself.
Restoring from inside ends the session, since the VM is replaced.

### How network policy is enforced

Unrestricted sprites stay on the kernel NAT path, untouched. A sprite with rules has its
address placed in an nftables set; from then on its DNS and all of its TCP are redirected to
listeners in spritesd and everything else it sends (other UDP, ICMP) is dropped, so the policy
is not leaky. The DNS listener answers only for allowed names (REFUSED otherwise, as
upstream), remembers the addresses it handed out, and strips private answers. The proxy
recovers the original destination and connects only to addresses that sprite was given for an
allowed name. Because the proxy dials from the host, it refuses private, CGNAT, link-local,
multicast and the host's own addresses itself, always.

spritesd is unprivileged, so the set is edited by a tiny root helper, `mini-sprites-netd`,
that accepts exactly one request: replace the set with these addresses, each validated to be
inside the sprite network. Policy **fails closed**: with the helper unreachable a restrictive
policy is refused with `503 policy_unenforceable`, and a sprite that already has one boots
without a NIC (or, if warm, refuses to wake rather than lose its memory state).

### How Firecracker is confined

Upstream runs Firecracker under its jailer, which must start as root. spritesd stays
unprivileged, so each VMM is sandboxed with what an ordinary user is given. spritesd re-execs
itself as a small shim that applies a Landlock domain and then `execve`s Firecracker in place
(same pid, same cwd), and the VMM is born inside its own cgroup v2 leaf via
`CLONE_INTO_CGROUP`. The startup log line `vmm confinement` says exactly what is in force.

| | A compromised VMM... | Enforced by |
|---|---|---|
| Filesystem | can read and write only its own machine directory (disk, snapshot, console, sockets); can read the guest kernel, the initrd and `/etc/localtime`; can open `/dev/kvm` and, with a NIC, `/dev/net/tun`. Other sprites' directories, the API token, `$HOME` and the rest of the filesystem are `EACCES`. It cannot `mkdir`, delete or rename anything, and can execute nothing but the Firecracker binary. | Landlock (ABI 1+) |
| Device ioctls | only on the devices above | Landlock ABI 5+ |
| TCP | cannot bind or connect | Landlock ABI 4+ |
| Signals, abstract unix sockets | cannot signal spritesd or another VMM, or reach an abstract socket outside its domain | Landlock ABI 6+ |
| CPU | `cpu.max` = vCPUs + 1 core (the extra core is the VMM's own I/O threads) | cgroup v2 |
| Memory | `memory.high` = guest RAM + 192 MiB, `memory.max` 256 MiB above that, so a busy sprite is reclaimed rather than OOM-killed | cgroup v2 |
| Processes | `pids.max` = 32 + 4 per vCPU | cgroup v2 |

The limits come from the VM's shape, so a resources policy that sizes the guest also sizes the
host cgroup. The cgroup subtree is found by walking up from spritesd's own cgroup to the first
level with `cpu`, `memory` and `pids` delegated (`user@<uid>.service` on a systemd host).

**What this does not do**, so the claim stays honest:

- Every VMM still runs as your uid. The jailer's per-VM uid and chroot are not reproduced;
  Firecracker's seccomp filter remains what blocks `ptrace` and friends.
- Landlock does not mediate `connect()` on a *pathname* unix socket. A compromised VMM can
  still reach another sprite's Firecracker API and vsock sockets, and `mini-sprites-netd`'s
  socket, because it has the same uid. It cannot read their files, but it can talk to them.
  Closing this needs a mount namespace (so, root or an unrestricted user namespace; Ubuntu's
  AppArmor restricts the latter) or a future Landlock right. `TestLandlockPathnameUnixSockets`
  pins the behaviour down.
- Landlock cannot narrow `/dev/net/tun` to one interface: a VMM with a NIC could attach to
  another *free* tap in the pool (one in use is `EBUSY`).
- Landlock does not cover UDP or raw sockets; that is left to Firecracker's seccomp filter.

`--confine` (or `MINI_SPRITES_CONFINE`) picks the mode: `best-effort` (default) applies what
the kernel and host support and logs the rest, `strict` refuses to start without both Landlock
(with signal scoping) and a delegated cgroup, `off` runs Firecracker as before. Measured on the
development host (kernel 7.0, Landlock ABI 8; median of 15, `scripts/measure-confine-latency.sh`):
cold boot 222 ms off / 226 ms confined, warm wake 23.5 ms off / 26.5 ms confined.

## Public sprite URLs

Every sprite has a URL, `<name>.<url-domain>`, that wakes it and proxies to its `http_port`
service (else port 8080). Out of the box that is `http://<name>.sprites.localhost:7788`, on
the same listener as the API. To put the URLs on the internet without putting the API there:

```sh
# once, in Cloudflare: an A record  *.widgets.wtf -> your public IP  (DNS only, grey cloud),
# and an API token with Zone:Read + DNS:Edit on that zone
(umask 077; echo "$TOKEN" > ~/.local/share/mini-sprites/cloudflare-token)
# once, on the router: forward external 443 -> this machine's 8443

./bin/spritesd --url-domain widgets.wtf --public-listen :8443
```

- `--public-listen` serves sprite URLs over HTTPS and **nothing else**: the management API has
  no routes there, so a leaked forward cannot expose it. Keep `--listen` on loopback or a tailnet.
- The certificate is one wildcard, `*.widgets.wtf`, from Let's Encrypt over DNS-01, renewed at
  two thirds of its life and kept in `<data>/acme/`. A wildcard because sprite names then never
  appear in certificate-transparency logs, a new sprite's URL works at once, and DNS-01 needs no
  inbound port. Try `--acme-directory https://acme-staging-v02.api.letsencrypt.org/directory`
  first: production rate-limits failed attempts. No Cloudflare? Bring any certificate with
  `--tls-cert/--tls-key`; the files are re-read when they change.
- A sprite's URL needs the API token as a bearer unless its `url_settings.auth` is `public`,
  which is upstream's model and the default is the closed one. Anyone can wake a `public`
  sprite, and it holds its RAM until it idles out again.
- The API reports `https://<name>.widgets.wtf`; add `--public-port` if the router's outside
  port is not 443. Connections are capped in total and per client (`--public-max-conns*`).
- Proxying through Cloudflare (orange cloud) also works and hides your address: use an outside
  port Cloudflare connects to (443, 8443, 2053...), SSL mode "Full (strict)", and
  `--public-max-conns-per-client 0`, since every visitor then arrives from Cloudflare's addresses.
- Outside 443 already taken by another reverse proxy? Have it pass the TLS through by SNI rather
  than terminate it, so the certificate and the per-sprite auth stay here. In Traefik that is an
  `IngressRouteTCP` on the HTTPS entrypoint matching ``HostSNIRegexp(`^[a-z0-9-]+\.widgets\.wtf$`)``
  with `tls.passthrough: true`, pointing at this machine's 8443. It also needs
  `--public-max-conns-per-client 0`: every visitor arrives from the proxy's address.
- Not handled: updating the A record when a dynamic IP changes, and a port-80 redirect (a
  reverse proxy in front can do the redirect).

## Deliberate differences from the hosted product

- **Durability is this machine's disk.** There is no object-storage tier ([#2](https://github.com/jhgaylor/mini-sprites/issues/2)).
- Disk is 20 GB sparse by default (`SPRITE_DISK_GB` at image build) rather than 100 GB, and
  Firecracker has no discard, so space freed in a guest is not returned to the host.
- RAM is fixed per sprite (`--mem-mib`, `config.ram_mb`, or a resources policy); each warm
  snapshot costs that much disk. There is no memory autoscale: `resources.memory.autoscale`
  and `privileges.devices` are stored and returned but **not enforced**.
- The memory cgroup is a guard rail, not a boundary: guest root can leave it unless
  `noNewPrivileges` closes the sudo route. VM RAM is the hard bound. Privileges do not bind
  the filesystem API, which acts as root.
- Checkpoints are full-disk clones: a sparse copy (~0.5 s for the base image, VM paused
  for it) on a plain directory, or an instant copy-on-write clone after
  `sudo ./scripts/setup-storage.sh` (see below). IDs start at `v1`.
  An automatic checkpoint is taken before every restore so that a restore can be undone, which
  upstream does not do. Autos are kept to `--auto-checkpoint-keep` (3). Creating checkpoints
  from inside is capped (`--guest-checkpoint-limit`, 20) so one guest cannot fill the host disk.
- Restricted sprites are TCP-only (no QUIC, NTP or ping), and a denied destination connects
  and is then reset, because a transparent proxy must accept before it can decide. The
  `defaults` allowlist is our own.
- Setting a policy returns 204 where upstream's docs say 200: the official SDKs require 204.
- Firecracker runs as your user, not under upstream's jailer: there is no chroot and no
  per-VM uid. Each VMM is instead confined with Landlock and a cgroup, without root; see
  [How Firecracker is confined](#how-firecracker-is-confined) for exactly what that covers.

### Issues in the official Go SDK that this server works around or documents

- **`ProxyPorts` races on a control socket.** Over `/control` the SDK's pool reader and its
  proxy handshake both read the one WebSocket, and the forward hangs whenever the pool reader
  wins (about two times in three, measured), with no fallback. Nothing a server sends can
  settle a race between two readers in the client, so spritesd answers *that SDK's* `/control`
  probe (`User-Agent: sprites-go-sdk/`) with 404. It takes that to mean "no control channel"
  and uses a socket per operation, which costs it nothing: it never reuses a control socket
  anyway. The JS and Python SDKs, where control is opt-in and does multiplex, still get it.
  `--control-for-go-sdk` offers it to the Go SDK too; `--control=false` turns it off for all.
- **A control connection that dies mid-operation is never reported to the operation.**
  spritesd terminates the `/control` WebSocket itself, so that when a VM goes away under a
  running exec (a checkpoint restore does this by design) the client is told instead of
  hanging until its context expires.
- Over control the SDK does not send an initial TTY size; resize after start.

## Development

```sh
make test     # unit tests, race detector. The vmm tests boot a real microVM; they skip without /dev/kvm.
              # The ACME test runs against pebble if it is on PATH (go install github.com/letsencrypt/pebble/v2/cmd/pebble@latest).
make e2e      # official Sprites Go SDK against a running spritesd
make initrd   # rebuild the agent and sprite-env; sprites pick them up on their next *cold* boot
make netd     # build the network-policy helper that setup-host.sh installs

./scripts/test-netpolicy-netns.sh     # the real nft ruleset + helper, in a rootless podman netns
./scripts/verify-network-policy.sh    # network policy on the real host, from inside real guests
```

e2e knobs: `SPRITES_E2E_IDLE_TIMEOUT=<the daemon's --idle-timeout>` enables the lifecycle
subtests (tasks, watch); `SPRITES_SDK_DEBUG=1` shows which connection mode the SDK used.
`SPRITES_E2E_GO_CONTROL=1` (with a daemon started `--control-for-go-sdk`) runs the tests that
drive `/control` with the Go SDK. `./scripts/probe-sdks.sh` runs the official JS and Python
SDKs against a running daemon: exec with control mode off and on, port proxying, and an exec
whose VM is restored away.

Only one spritesd per host may own the tap pool. For extra dev/test stacks:

```sh
./scripts/dev-data.sh /tmp/ms-x            # keep it short: the dir holds unix sockets (108-byte limit)
MINI_SPRITES_DATA=/tmp/ms-x ./scripts/build-initrd.sh
./bin/spritesd --data /tmp/ms-x --listen 127.0.0.1:7801 --net=false
```
