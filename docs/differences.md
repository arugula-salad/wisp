# Deliberate differences from the hosted product

- **Durability is a backup tier, not continuous sync.** Without `--backup-bucket` a sprite
  lives and dies with this machine's disk. With one, the recovery point is the last completed
  upload — the sprite's last suspend, or `--backup-interval` for a long-running one. Upstream
  keeps the local disk as a cache and syncs chunks continuously, so it can wake a sprite on a
  different host without a full download; that tier is [#2](https://github.com/arugula-salad/wisp/issues/2)'s second half and is not built.
- Disk is 20 GB sparse by default (`SPRITE_DISK_GB` at image build) rather than 100 GB, and
  Firecracker has no discard, so space freed in a guest is not returned to the host.
- RAM is set per sprite (`--mem-mib`, `config.ram_mb`, or a resources policy), but a running
  sprite and a warm snapshot cost roughly what the guest uses (an idle 2 GiB sprite's
  snapshot is ~120 MiB), through a balloon with free page reporting. `resources.memory.autoscale`
  is enforced with that balloon: the guest boots with the limit as its RAM and is held to a
  1 GiB grant that grows under pressure. Unlike a grown VM, the guest's `MemTotal` shows the
  limit throughout. See [what a sprite costs](lifecycle.md#what-a-sprite-costs-in-memory-and-disk).
  `privileges.devices` is stored and returned but **not enforced**.
- The memory cgroup is a guard rail, not a boundary: guest root can leave it unless
  `noNewPrivileges` closes the sudo route. VM RAM is the hard bound. Privileges do not bind
  the filesystem API, which acts as root.
- Checkpoints are full-disk clones: a sparse copy (~0.5 s for the base image, VM paused
  for it) on a plain directory, or an instant copy-on-write clone after
  `sudo ./scripts/setup-storage.sh` (see [host setup](host-setup.md#instant-clones)). IDs start at `v1`.
  An automatic checkpoint is taken before every restore so that a restore can be undone, which
  upstream does not do. Autos are kept to `--auto-checkpoint-keep` (3). Creating checkpoints
  from inside is capped (`--guest-checkpoint-limit`, 20) so one guest cannot fill the host disk.
- Ours, not upstream's: `"from": {"image": ...}` creates a sprite from a container image
  (pulled with rootless podman, cached as a disk; [images](images.md)), and `from.sprite` clones
  a checkpoint. Neither exists upstream, and the official SDKs cannot send `from`.
- Restricted sprites are TCP-only (no QUIC, NTP or ping), and a denied destination connects
  and is then reset, because a transparent proxy must accept before it can decide. The
  `defaults` allowlist is our own.
- Setting a policy returns 204 where upstream's docs say 200: the official SDKs require 204.
- Firecracker runs as your user, not under upstream's jailer: there is no chroot and no
  per-VM uid. Each VMM is instead confined with Landlock and a cgroup, without root; see
  [How Firecracker is confined](security.md#how-firecracker-is-confined) for exactly what that covers.

## Issues in the official Go SDK that this server works around or documents

- **`ProxyPorts` races on a control socket.** Over `/control` the SDK's pool reader and its
  proxy handshake both read the one WebSocket, and the forward hangs whenever the pool reader
  wins (about two times in three, measured), with no fallback. Nothing a server sends can
  settle a race between two readers in the client, so wispd answers *that SDK's* `/control`
  probe (`User-Agent: sprites-go-sdk/`) with 404. It takes that to mean "no control channel"
  and uses a socket per operation, which costs it nothing: it never reuses a control socket
  anyway. The JS and Python SDKs, where control is opt-in and does multiplex, still get it.
  `--control-for-go-sdk` offers it to the Go SDK too; `--control=false` turns it off for all.
- **A control connection that dies mid-operation is never reported to the operation.**
  wispd terminates the `/control` WebSocket itself, so that when a VM goes away under a
  running exec (a checkpoint restore does this by design) the client is told instead of
  hanging until its context expires.
- Over control the SDK does not send an initial TTY size; resize after start.
