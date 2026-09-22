# Operating it

## Run it as a service

`make run` is for trying things out: the API is gone when the terminal is. To have it come
back after a reboot, like the network and the volume already do:

```sh
make install-service                       # builds, installs a systemd *user* unit, (re)starts it
./scripts/install-service.sh -- --max-running 8 --url-domain sprites.example.com   # with spritesd flags
./scripts/install-service.sh --uninstall   # suspends the sprites and removes the unit; data stays
```

It runs as you, with no root, exactly like `make run`. The binary is copied to
`~/.local/lib/mini-sprites/`, so the unit does not depend on this checkout; the flags live in
`~/.config/mini-sprites/mini-sprites.env` and survive a re-install. Stop the `make run` daemon
first (`^C` suspends its sprites, and the service resumes them).

```sh
journalctl --user -u mini-sprites -f            # the log: wakes with latency, suspends, egress denied, ...
journalctl --user -u mini-sprites -b -p warning # this boot, trouble only
systemctl --user restart mini-sprites           # suspends every running sprite, starts, resumes on demand
```

A stop sends SIGTERM to spritesd alone (`KillMode=mixed`), which writes every running sprite's
RAM to disk before exiting: about 0.6 s for three 2 GiB guests at once here. Measured on a
restart with three sprites running: all three came back with the same kernel `boot_id`.

Two things need root, once, and the installer tells you when they are missing rather than
doing them:

- `sudo loginctl enable-linger $USER`: without lingering, your user manager (and spritesd in
  it) starts at your first login and stops at your last logout.
- `sudo ./scripts/install-service.sh --system-dropin`: a drop-in for `user@<uid>.service` that
  orders your user manager after `mini-sprites-net`, `-netd` and `-storage` (and so stops it
  before them), and raises its stop timeout. **Ubuntu ships that timeout at 5 seconds**
  (`user@.service.d/timeout.conf`): at reboot every user service is killed 5 s after being
  asked to stop, whatever its own `TimeoutStopSec` says. A sprite cut off mid-snapshot is
  intact (an incomplete snapshot is never published) but comes back cold.

Until the drop-in is there the unit still waits, for up to 90 s, for the volume to be mounted
and the bridge to exist: started early, spritesd would see an empty sprite directory, or boot
every sprite without a NIC.

## See what is running

```
$ spritesd status
spritesd   pid 1140375, up 3h12m, API on 127.0.0.1:7788
data       /home/you/.local/share/mini-sprites
volume     5.9G used, 33G free of 39G, reflink clones; 2.0G kept in reserve
image      /home/you/.local/share/mini-sprites/sprites.xfs; 66G free on its filesystem
sprites    1 running (limit 8), 1 warm, 0 cold; 2 in all (no limit)
network    1 of 32 taps in use
policy     helper reachable

NAME   ID            STATE    PID      RSS   DISK  OWN   SNAP  CKPTS             HOLDS  IP          POLICY
build  09dc6ed0dfbd  running  1140792  412M  1.9G  1.3G  0     3 (mounted 0=v2)  1      10.209.0.2  restricted
hello  19610039ea00  warm     -        -     589M  7M    2.0G  0                 -      10.209.0.3  open

ORPHANED VMs: 1 firecracker process(es) of yours that no running spritesd started. Not touched; ...
  PID      RSS   PARENT          CWD
  1083557  915M  systemd (6354)  /tmp/ms-mem/vm/e7a5da5ef31f
```

It needs no token: the running daemon answers on `<data>/spritesd.sock` (mode 0600, so the
filesystem permission is the authentication), and with no daemon up the same command answers
from the files. That socket is also the lock on the data directory: a second spritesd on the
same one refuses to start. `--json` prints everything, under field names that are meant to be
scripted against (`internal/server/status.go`).

- **DISK / OWN**: a 20 GB apparent size says nothing, and on a reflink volume neither does
  `du`, which counts a shared block once per clone. These come from the files' extent maps:
  DISK is what the sprite's disk and checkpoints occupy with every shared block counted once,
  OWN the part nothing else shares, i.e. what deleting the sprite gives back.
- **HOLDS** is the number of live tasks keeping a sprite awake: the answer to "why is this
  VM still running".
- **Orphans** are Firecracker processes of your user whose parent is not a running spritesd:
  a VM started by hand, or left by a daemon that was killed. They are reported, never killed,
  since another data directory or someone's experiment may own them. (A spritesd does reap
  stale VMs in *its own* data directory when it starts.) Other spritesd instances and their VM
  counts are listed separately.

## Limits

`--max-sprites` and `--max-running` (both 0 = none) are enforced at create and at wake. The
errors have upstream's shape, which the SDKs parse into their `APIError`
(`limit`, `current_count`, `retry_after_seconds`): a wake past `--max-running` is a `429
concurrent_sprite_limit_exceeded` with `Retry-After` set to the idle timeout, which is when a
slot can free up; a create past `--max-sprites` is a `403 sprite_limit_exceeded`, since
waiting does not help. `GET /v1/sprites` carries upstream's `org` block
(`running`/`warm`/`cold` over all sprites, `running_limit`; `warm_limit` is always 0, because
what bounds warm sprites here is the disk, below).

## Disk pressure

The volume holds sprite disks, checkpoints and one guest-RAM-sized snapshot per warm sprite.
When it is the loop-mounted image from `setup-storage.sh`, filling it, or the host filesystem
under the sparse image, does not produce a clean ENOSPC but I/O errors inside guests. So:

- A create, checkpoint or restore that would leave less than `--disk-reserve-mib` (2048) free
  is refused with `507 insufficient_storage`. On a reflink volume a clone costs nothing up
  front, so this is the reserve being defended; elsewhere the full copy is counted.
- A suspend must not fail, or the VM would run forever. If its snapshot does not fit, the
  longest-suspended warm sprites are turned cold first (they lose only memory state), and if
  even that cannot make room the sprite is synced and stopped cold instead. Concurrent
  suspends (a shutdown) are not promised the same free bytes twice.
- Free space is the smaller of the volume's and, for a sparse image, its host filesystem's;
  `spritesd status` shows both. The log warns while either is below `--disk-warn-percent` (10).

What the guard cannot do is stop running guests from growing their own disks, which are
sparse too; the warning and the reserve are the margin for that.
`scripts/verify-disk-guard.sh` fills a real 3 GB filesystem under real VMs and checks all of
the above (22 checks), including that nothing is corrupted afterwards; it needs no root where
`udisksctl` can set up a loop device.

## Not built yet

- **Metrics**: no Prometheus endpoint. The same events the log has (wake mode and latency,
  suspends, going cold, policy denials, limit refusals, disk warnings) are available as a
  stream and as webhooks ([events](events.md)), and `spritesd status --json` has the gauges.
- **Tokens**: one bearer token in `<data>/token`; to rotate it, replace the file and restart.
  No named tokens, scopes or revocation.
- **Listening beyond localhost**: the API binds `127.0.0.1` in plaintext, and the supported
  way to expose it is a TLS-terminating reverse proxy in front (it must pass WebSocket
  upgrades and the `Host` header, which routes sprite URLs). There is no built-in TLS for the
  API. Sprite URLs are separate: `--public-listen` serves them, and only them, over HTTPS; see
  [public sprite URLs](public-urls.md).
