# mini-sprites

A single-host implementation of the [Sprites](https://sprites.dev) API: persistent,
hardware-isolated Linux environments on Firecracker microVMs that suspend when idle
and wake on the next request. The official `sprite` CLI and Go SDK work against it
unmodified.

```
client (sprite CLI / SDK / curl)
   │  REST + WebSocket, Bearer token              http://<name>.sprites.localhost:7788
   ▼                                                        │
spritesd ── lifecycle engine: wake on request, suspend when idle, go cold after a TTL
   │  vsock (no guest network needed)
   ▼
Firecracker microVM ── sprite-agent as PID 1 (from an initramfs) ── your ext4 disk
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

Guest internet access is the one thing that needs root, once:

```sh
sudo ./scripts/setup-host.sh     # bridge + tap pool + NAT/isolation rules + boot unit
```

Without it sprites run with no NIC; exec, checkpoints, sprite URLs and the TCP proxy
all still work because they travel over vsock.

## Lifecycle

| State | What exists | Wake |
|---|---|---|
| `running` | Firecracker process | – |
| `warm` | memory snapshot on disk, processes frozen | ~15 ms VM restore |
| `cold` | disk only | ~200 ms boot to a responsive agent |

A sprite suspends after `--idle-timeout` (30s) with no API requests in flight, no
attached exec sessions and no session I/O. It goes cold after `--warm-ttl` (1h), which
drops memory state: processes are gone, the filesystem is intact. TCP connections never
survive a suspend. The guest clock is stepped to host time on every resume. On
SIGTERM, spritesd suspends every running sprite so they resume warm after a restart.

## API coverage

Implemented: sprites CRUD + pagination, exec (WebSocket TTY/non-TTY, detach/reattach with
output replay, `max_run_after_disconnect`, signals, HTTP POST variant, session list, kill),
checkpoints (create/list/get/restore, streaming NDJSON), TCP proxy, per-sprite URLs with
`sprite`/`public` auth routed to guest port 8080.

Not yet: services, filesystem API, network/privilege/resource policies, port-watch
notifications, the multiplexed `/control` channel (clients fall back automatically),
the Tasks API, auto checkpoints.

## Deliberate differences from the hosted product

- **Durability is this machine's disk.** There is no object-storage tier.
- Disk is 20 GB sparse by default (`SPRITE_DISK_GB` at image build) rather than 100 GB, and
  Firecracker has no discard, so space freed in a guest is not returned to the host.
- RAM is fixed per sprite (`--mem-mib`, or `config.ram_mb` at create); each warm snapshot
  costs that much disk.
- Checkpoints are full-disk clones: instant on a reflink filesystem (XFS/btrfs), a sparse
  copy (~0.5 s for the base image) on ext4. The VM is paused for the copy.
- Checkpoint IDs start at `v1`. The POST exec response body is unspecified upstream; here
  it is the raw combined output with the exit code in a `Sprite-Exit-Code` trailer.
- Firecracker runs unjailed as your user.

## Development

```sh
make test     # unit tests (the agent's exec protocol runs on the host, no VM needed)
make e2e      # official Sprites Go SDK against a running spritesd
make initrd   # rebuild the agent; sprites pick it up on their next *cold* boot
```
