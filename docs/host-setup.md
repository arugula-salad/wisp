# Host setup

spritesd itself needs no root. Two optional, one-time steps do: guest networking and the
reflink volume. Both install boot units, so they survive a reboot.

## Guest networking

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

## Instant clones

```sh
sudo apt install xfsprogs
sudo ./scripts/setup-storage.sh      # stop spritesd first; SPRITE_VOLUME_GB=40 by default
```

Puts the sprite directory on a loop-mounted XFS volume with reflinks, so creating a sprite,
taking a checkpoint and restoring one are instant and share disk blocks until written
(measured: create 13 ms, checkpoint 2 ms, restore 69 ms; six 20 GB images in 1.7 GB). Warm
snapshots live on the volume too, one per suspended sprite, taking what the guest was using
but briefly more than its RAM size while written, so size it for both; suspend is a little slower through the loop device (~1.6 s vs ~1.2 s). The
script migrates existing sprites, never sizes the volume beyond what the host disk can hold,
and `--remove` moves everything back. spritesd needs no configuration: it probes the
filesystem at startup and logs which mode it is in.

To give sprites more room later, grow the volume, and optionally move its image to a disk
with more space (the image is one file; by default it sits in the data directory):

```sh
systemctl --user stop mini-sprites      # suspends every sprite; they resume warm afterwards
sudo SPRITE_VOLUME_GB=300 SPRITE_VOLUME_IMAGE=/data/mini-sprites/sprites.xfs \
     ./scripts/setup-storage.sh --grow
systemctl --user start mini-sprites
```

Either variable can be left out: `SPRITE_VOLUME_GB` alone grows the image where it is,
`SPRITE_VOLUME_IMAGE` alone moves it. It only ever grows, since XFS cannot shrink, and every
sprite is kept: the filesystem is extended in place, a move copies the image and removes the
old one only once the copy is mounted, and the boot unit is rewritten to wait for the new
disk. The size is refused if the target disk could not hold it plus 5 GB, for the same reason
as at creation: an image that outgrows its disk fails with I/O errors inside guests.
