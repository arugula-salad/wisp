# Security: network policy and VMM confinement

How the two host-side boundaries are enforced, and exactly what they do not cover; and what
creating sprites from [container images](#container-images) exposes.

## How network policy is enforced

Unrestricted sprites stay on the kernel NAT path, untouched. A sprite with rules has its
address placed in an nftables set; from then on its DNS and all of its TCP are redirected to
listeners in wispd and everything else it sends (other UDP, ICMP) is dropped, so the policy
is not leaky. The DNS listener answers only for allowed names (REFUSED otherwise, as
upstream), remembers the addresses it handed out, and strips private answers. The proxy
recovers the original destination and connects only to addresses that sprite was given for an
allowed name. Because the proxy dials from the host, it refuses private, CGNAT, link-local,
multicast and the host's own addresses itself, always.

wispd is unprivileged, so the set is edited by a tiny root helper, `wisp-netd`,
that accepts exactly one request: replace the set with these addresses, each validated to be
inside the sprite network. Policy **fails closed**: with the helper unreachable a restrictive
policy is refused with `503 policy_unenforceable`, and a sprite that already has one boots
without a NIC (or, if warm, refuses to wake rather than lose its memory state).

## How Firecracker is confined

Upstream runs Firecracker under its jailer, which must start as root. wispd stays
unprivileged, so each VMM is sandboxed with what an ordinary user is given. wispd re-execs
itself as a small shim that applies a Landlock domain and then `execve`s Firecracker in place
(same pid, same cwd), and the VMM is born inside its own cgroup v2 leaf via
`CLONE_INTO_CGROUP`. The startup log line `vmm confinement` says exactly what is in force.

| | A compromised VMM... | Enforced by |
|---|---|---|
| Filesystem | can read and write only its own machine directory (disk, snapshot, console, sockets); can read the guest kernel, the initrd and `/etc/localtime`; can open `/dev/kvm` and, with a NIC, `/dev/net/tun`. Other sprites' directories, the API token, `$HOME` and the rest of the filesystem are `EACCES`. It cannot `mkdir`, delete or rename anything, and can execute nothing but the Firecracker binary. | Landlock (ABI 1+) |
| Device ioctls | only on the devices above | Landlock ABI 5+ |
| TCP | cannot bind or connect | Landlock ABI 4+ |
| Signals, abstract unix sockets | cannot signal wispd or another VMM, or reach an abstract socket outside its domain | Landlock ABI 6+ |
| CPU | `cpu.max` = vCPUs + 1 core (the extra core is the VMM's own I/O threads) | cgroup v2 |
| Memory | `memory.high` = guest RAM + 192 MiB, `memory.max` 256 MiB above that, so a busy sprite is reclaimed rather than OOM-killed. Sized for the full RAM even under memory autoscale: the balloon is a cooperative guest driver, so the host bound must not depend on it | cgroup v2 |
| Processes | `pids.max` = 32 + 4 per vCPU | cgroup v2 |

The limits come from the VM's shape, so a resources policy that sizes the guest also sizes the
host cgroup. The cgroup subtree is found by walking up from wispd's own cgroup to the first
level with `cpu`, `memory` and `pids` delegated (`user@<uid>.service` on a systemd host).

**What this does not do**, so the claim stays honest:

- Every VMM still runs as your uid. The jailer's per-VM uid and chroot are not reproduced;
  Firecracker's seccomp filter remains what blocks `ptrace` and friends.
- Landlock does not mediate `connect()` on a *pathname* unix socket. A compromised VMM can
  still reach another sprite's Firecracker API and vsock sockets, and `wisp-netd`'s
  socket, because it has the same uid. It cannot read their files, but it can talk to them.
  Closing this needs a mount namespace (so, root or an unrestricted user namespace; Ubuntu's
  AppArmor restricts the latter) or a future Landlock right. `TestLandlockPathnameUnixSockets`
  pins the behaviour down.
- Landlock cannot narrow `/dev/net/tun` to one interface: a VMM with a NIC could attach to
  another *free* tap in the pool (one in use is `EBUSY`).
- Landlock does not cover UDP or raw sockets; that is left to Firecracker's seccomp filter.

`--confine` (or `WISP_CONFINE`) picks the mode: `best-effort` (default) applies what
the kernel and host support and logs the rest, `strict` refuses to start without both Landlock
(with signal scoping) and a delegated cgroup, `off` runs Firecracker as before. Measured on the
development host (kernel 7.0, Landlock ABI 8; median of 15, `scripts/measure-confine-latency.sh`):
cold boot 222 ms off / 226 ms confined, warm wake 23.5 ms off / 26.5 ms confined.

## Container images

Creating a sprite from a container image ([images](images.md)) puts registry content through
the host, so:

- **Only API callers can make the host pull.** The API token is the whole API anyway. From
  inside a sprite (the spawn routes on the guest channel) only images already in the cache are
  accepted; an uncached reference is a 404 and nothing is fetched.
- **References are parsed, not passed through.** They must be registry references and are
  rebuilt in one canonical form before they reach podman, always as an argument vector after
  `--`, never through a shell. Transports that name host files (`oci:`, `dir:`,
  `containers-storage:`...) cannot be expressed. `localhost/` names the daemon user's own podman
  storage, which an API caller can therefore use.
- **The image's files never land on the host filesystem.** `podman export` streams a tar that
  wispd rewrites (names are cleaned, so `..` cannot climb out) and that mke2fs writes into
  the new ext4 image. What does run on the host is the tar parsing, in wispd and in mke2fs,
  as the daemon's user and unconfined: a hostile image is input to those parsers. Nothing from
  the image is executed on the host.
- **The sudo stand-in is a setuid-root binary inside the guest.** It lets only the `sprite`
  user become root, which that user may anyway in any sprite; a setuid copy runs nothing but
  the sudo code, whatever name it is invoked under, and `noNewPrivileges` disables it. It is
  never on the host.
- The disk guard bounds the cache on the sprite volume; podman's own storage, where layers
  sit during a pull, is outside it.
