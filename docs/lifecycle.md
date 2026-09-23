# Lifecycle: running, warm, cold

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
host time on every resume. On SIGTERM, wispd suspends every running sprite so they resume
warm after a restart.

**Tasks** are explicit keep-awake holds, created from inside the sprite as upstream does:

```sh
curl --unix-socket /.sprite/api.sock -X POST http://sprite/v1/tasks -d '{"name":"build","expire":"10m"}'
```

They last at most 1h, are refreshed with PUT, and do not survive a cold boot or a wispd
restart. `/v1/sprites/{name}/tasks` exposes the same thing from outside (our extension).

## Leases: sprites that delete themselves

A task holds a sprite awake. A **lease** is the other kind of expiry: a deadline on the
workspace itself, after which the sprite is deleted, disk and checkpoints and address
included. It is ours, not upstream's, and it is opt-in — a sprite created without one has no
deadline and is never reaped. There is deliberately no flag for a default lease: a persistent
sprite must not acquire an expiry because of how the daemon was started.

```sh
# at creation, as a deadline or a duration
curl -X POST "$SPRITES_API_URL/v1/sprites" -d '{"name":"preview-412","ttl_seconds":7200}'
curl -X POST "$SPRITES_API_URL/v1/sprites" -d '{"name":"demo","expires_at":"2026-10-01T09:00:00Z"}'

# renew, protect, release — the lease endpoint is outside /v1, like the event stream
curl -X POST "$SPRITES_API_URL/wisp/v1/sprites/preview-412/lease" -d '{"ttl_seconds":3600}'
curl -X POST "$SPRITES_API_URL/wisp/v1/sprites/preview-412/lease" -d '{"protected":true}'
curl -X DELETE "$SPRITES_API_URL/wisp/v1/sprites/preview-412/lease"   # no deadline at all
curl "$SPRITES_API_URL/wisp/v1/sprites/preview-412/lease"
```

- `expires_at` (RFC3339) and `ttl_seconds` are two ways to say the same thing; giving both is
  an error, and so is a deadline already in the past. `expires_at: ""` or `ttl_seconds: 0`
  clears the lease, and a request that mentions neither leaves it alone — an SDK that knows
  nothing of leases cannot drop one by accident. The same fields work on
  `PUT /v1/sprites/{name}`, and both appear on the sprite in `GET`, in `wispd status --json`
  and in the web UI's sprite overview.
- `protected: true` holds the deletion off without clearing the deadline, for the sprite
  somebody turns out to still be using. The deadline stays visible and stays in the past;
  clearing the protection hands the sprite straight back to the reaper.
- The reaper runs on the same 30 s janitor tick as the warm-TTL sweep, and **once at
  startup**: a lease that ran out while wispd was down has still run out.
- Expiry is deletion, exactly what `DELETE /v1/sprites/{name}` does and by the same code —
  a running sprite is stopped first, and its address, tap, disks, checkpoints and custom
  domains are released. It is **not** a suspend, and it does **not** take a backup first:
  where a bucket is configured the reaper leaves the same tombstone a manual delete leaves,
  which says the sprite was deleted and never that its last upload was current.
- Renewals and protection changes take the sprite's lifecycle lock, the one every transition
  holds, and a reap that has committed refuses them with `409 expired`. A renewal racing a
  reap therefore either wins outright or is told the sprite is going; there is no state in
  between.
- Two events say what happened: `sprite.expiring` once per deadline, a lead time ahead of it
  (5 minutes by default), and `sprite.expired` just before the `sprite.deleted` that follows.

**Leases for a spawner's children** are the reason all this exists. A lobby that hands every
visitor a sprite reaches `spawn_policy.max_children` and stays there, because nothing frees a
slot. `child_ttl_seconds` in the [spawn policy](api.md#sprites-that-create-sprites) gives every
child a lease at birth:

```sh
curl -X POST "$SPRITES_API_URL/v1/sprites/lobby/policy/spawn" \
  -d '{"enabled":true,"max_children":50,"child_ttl_seconds":3600}'
```

0, the default, keeps the old behavior: children never expire and the lobby is expected to
delete them. The policy's TTL is a ceiling rather than a default — a lobby that knows a game
is short-lived may ask for less when it creates one, and a child cannot ask for more, nor
protect itself out of the lease its parent gave it.

## What a sprite costs in memory and disk

Every VM has a virtio-balloon device, and a sprite costs roughly what its guest is using,
not its RAM size:

- **While it runs**, the guest kernel reports memory it frees (in 2 MiB blocks, a few seconds
  after the free) and Firecracker hands it back to the host. A resumed sprite that allocated
  and freed 1 GiB three times sat at 111 MB of host RSS; without reporting it keeps whatever
  it once touched.
- **At suspend**, wispd inflates the balloon over the guest's free memory, so it does not
  wait for reporting to catch up. Firecracker then writes the whole memory file (it has no
  sparse mode), unsynced, and wispd copies it to a new file leaving a hole wherever a page
  is zero, syncs the copy, and deletes the original. A hole reads back as zeros, so the
  snapshot's contents are unchanged. (Punching holes in place was tried first: ext4 and XFS
  write a range's dirty pages to disk before punching it, so every zero still hit the disk.)
  On resume the balloon is set back and the guest deflates it in about 0.15 s; a command
  run in that instant sees little free memory in `free`, and an allocation that cannot wait
  takes pages from the balloon (`deflate_on_oom`).

Measured with a 2 GiB sprite on ext4 (`du` of `snap.mem`, the allocated size, not the
apparent one, which is always the RAM size):

| Guest state at suspend | Before | Now |
|---|---|---|
| idle after boot | 2049 MiB | 115 to 118 MiB |
| allocated and freed 1 GiB just before | 2049 MiB | 125 to 130 MiB (reporting alone: 1151 MiB at once, 650 after 5 s, 138 after 20 s) |
| holding 600 MiB in a tmpfs | 2049 MiB | 711 MiB |
| 4 GiB sprite that read a 2.5 GB file (page cache) | 4225 MiB | 2703 MiB; 790 MiB with autoscale |

On the production XFS reflink volume, under `--confine=strict`, an idle 2 GiB sprite made from
`alpine:3.20` suspended to a 128 MiB snapshot in 581 ms and woke warm in 21 ms.

Latency, six suspend/wake rounds each on the same host with `--confine=strict`: suspend
was 1.2 to 2.5 s before and is 1.16 to 1.21 s now (the first suspend after a cold boot is
~2.5 s in both). Warm wake, restore to a responsive agent, is unchanged: 21 to 25 ms before,
19 to 26 ms now. Disk writes per idle suspend (the partition's write counter across one
suspend): 2049 MiB before, because Firecracker fsynced the whole file; 124 MiB now with
`--confine=off`, and 0.8 to 1.1 GiB with the default cgroup confinement, whose per-cgroup
dirty limits make the kernel write back part of the whole-RAM file while Firecracker is
still writing it, before wispd can delete it. A suspend still needs the full RAM size
free on the volume for a moment, and up to half as much again while the copy exists; the
disk guard reserves both.

The cost of reporting: memory a guest frees and touches again after a few seconds has to be
faulted in from the host again, at about 2 s per GiB here (2 GiB: 4.4 s on first touch in a
fresh VM, 0.37 s when reused at once, 4.4 s again when reused after 30 s with reporting on,
0.36 s with it off). That is the same cost every page already pays on its first touch after a
warm resume. `--free-page-reporting=false` turns it off from each sprite's next cold boot;
the suspend-time squeeze keeps snapshots small either way.

### Memory autoscale

`resources.memory.autoscale` is enforced with the balloon. Upstream describes a sprite that
starts with some memory and grows towards a ceiling under pressure. Here the VM boots with
the ceiling (`limit_mb` + 128 MiB, as without autoscale) and the balloon holds everything
above a grant that starts at 1 GiB. Once a second wispd reads the guest's balloon
statistics: when available memory drops below a fifth of the grant it doubles the grant,
and after 30 s of using less than half it shrinks it back towards what is in use (never
below the start). Page cache does not count as pressure, so a sprite that reads a lot of
files stays at its grant instead of filling its ceiling with cache. Turning autoscale on or
off applies live.

What it is and is not:

- It bounds what the sprite costs the host (the 4 GiB sprite above: 0.96 GB of host RSS with
  autoscale, 2.9 GB without), and so what its snapshot costs.
- A burst faster than the one-second loop is not refused: the balloon runs with
  `deflate_on_oom`, so the guest takes pages back from it instead of OOM-killing, and the
  controller adopts what it took. 3 GiB allocated at once from a 1 GiB grant succeeded
  with no OOM kill in 6.7 to 7.3 s, against 6.5 s for the same first touch without autoscale.
- Inside the guest `MemTotal` stays at the ceiling and ballooned memory shows as used
  (Linux counts it that way when `deflate_on_oom` is on); `MemAvailable` is the honest figure.
- The workload's own limit is still `limit_mb`, and the host cgroup is still sized for the
  ceiling, because the guest can always deflate. The balloon is a cooperative guest driver:
  a hostile guest can ignore it, so it is an economy, not a boundary. VM RAM is the bound.
- A VM resumed from a snapshot taken before VMs had a balloon has none; autoscale (and the
  squeeze) take effect at its next cold boot.

Not used: Firecracker's free page hinting, a developer preview with a documented race that
can discard a page the guest has reused; diff snapshots, whose `mincore` shortcut would skip
guest pages swapped out on the host and would need the previous memory file kept around for
a resumed VM; and `virtio-mem` hotplug, which would give a truer growing `MemTotal` but has
no fallback when the host is slow to grow it and has had several fixes in recent releases.
