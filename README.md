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
- **Proxy / URLs**: the TCP proxy, and per-sprite URLs with `sprite`/`public` auth.

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
- Firecracker runs unjailed as your user, under its own seccomp filter only ([#1](https://github.com/jhgaylor/mini-sprites/issues/1)).

### Issues in the official Go SDK that this server works around or documents

- **`ProxyPorts` hangs once a server offers `/control`** (two readers on one socket). Use the
  SDK's `WithDisableControl()`, or run spritesd with `--control=false`. The `sprite` CLI never
  uses control and is unaffected.
- **A control connection that dies mid-operation is never reported to the operation.**
  spritesd terminates the `/control` WebSocket itself, so that when a VM goes away under a
  running exec (a checkpoint restore does this by design) the client is told instead of
  hanging until its context expires.
- Over control the SDK does not send an initial TTY size; resize after start.

## Development

```sh
make test     # unit tests, race detector. The vmm tests boot a real microVM; they skip without /dev/kvm.
make e2e      # official Sprites Go SDK against a running spritesd
make initrd   # rebuild the agent and sprite-env; sprites pick them up on their next *cold* boot
make netd     # build the network-policy helper that setup-host.sh installs

./scripts/test-netpolicy-netns.sh     # the real nft ruleset + helper, in a rootless podman netns
./scripts/verify-network-policy.sh    # network policy on the real host, from inside real guests
```

e2e knobs: `SPRITES_E2E_IDLE_TIMEOUT=<the daemon's --idle-timeout>` enables the lifecycle
subtests (tasks, watch); `SPRITES_SDK_DEBUG=1` shows which connection mode the SDK used.

Only one spritesd per host may own the tap pool. For extra dev/test stacks:

```sh
./scripts/dev-data.sh /tmp/ms-x            # keep it short: the dir holds unix sockets (108-byte limit)
MINI_SPRITES_DATA=/tmp/ms-x ./scripts/build-initrd.sh
./bin/spritesd --data /tmp/ms-x --listen 127.0.0.1:7801 --net=false
```
