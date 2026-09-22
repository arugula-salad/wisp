# Sprites from container images

Ours, not upstream's: a sprite can start from any container image instead of the base image.

```sh
curl $SPRITES_API_URL/v1/sprites -H "Authorization: Bearer $SPRITE_TOKEN" \
  -d '{"name": "web", "from": {"image": "docker.io/library/node:22"}}'
```

`from.image` sits beside the existing `from.sprite`/`from.checkpoint` (a clone); give one or
the other. References are written the way docker takes them: `node:22`, `alpine`,
`ghcr.io/owner/app:v1`, `python@sha256:...`. They are normalized (`node:22` is
`docker.io/library/node:22`, no tag is `:latest`) and must be registry references: transports
such as `oci:`, `docker://` or `containers-storage:` are refused, since they would let a
caller name files on the host. `localhost/<name>` is an image in the daemon user's own podman
storage (something you built with `podman build`) and is never pulled.

The new sprite's `source_image` field (ours) records the image, pinned by the digest it was
pulled at. A checkpoint clone of it inherits the field.

The official SDKs have no way to send `from`, so this is a raw HTTP call (or `sprite-env`, below).

## The cache, and when a pull happens

spritesd pulls the image with rootless podman, flattens it into an ext4 disk, and keeps that
disk in a cache under `<data>/vm/.images/`, one per image ID. Every create from it afterwards
clones the cached disk exactly as checkpoints are cloned: instant on a reflink volume, a
sparse copy otherwise (measured on a plain tmpfs, no reflink: 0.19 s for node:22's 1.1 GB, 5 ms
for alpine).

- **A create whose image is not cached pulls it and blocks** until the disk is built
  (node:22: 11 s on a fast line, of which the flatten is a few seconds). The pull is detached
  from the request: a client that gives up does not cancel it, and a retry finds the disk
  ready. Concurrent creates of the same image share one pull.
- **A create whose image is cached never contacts the registry**, even for a tag that has
  since moved. A tag is refreshed only by an explicit `spritesd images pull`; if it now names a
  different image, the reference moves to a new disk and the old disk, if nothing else refers
  to it, is deleted. Sprites made from the old one are unaffected (they are copies).
- **Clients with short timeouts** (the SDKs' default is 30 s) should not trigger a cold pull:
  pull ahead of time with `spritesd images pull`.

```
$ spritesd images pull node:22
pulling docker.io/library/node:22
Trying to pull docker.io/library/node:22...
...
cached docker.io/library/node:22 as image 3112e746c449 in 11s (shell: /bin/bash, own sudo: false)
$ spritesd images list
ID            REFS                                SHELL      SUDO      UID   IMAGE  DISK  BUILT     LAST USED
3112e746c449  docker.io/library/node:22           /bin/bash  stand-in  1001  1.1G   1.1G  2m ago    just now
d01fbf53dae0  localhost/mini-sprites-base:latest  /bin/bash  image's   1000  554M   582M  just now  just now
$ spritesd images rm node:22          # or an ID prefix of 12+ characters
```

`images` talks to the running daemon over the operator socket (`<data>/spritesd.sock`, like
`status`), so it needs no token, and the cache cannot be managed through the API token at all.
`list` also works with no daemon running. `--json` prints the records.

A pull adds the image to podman's own storage (the daemon user's `~/.local/share/containers`)
only for as long as the build takes: a reference the pull introduced is removed again
afterwards, while one that was already there is the operator's and is left alone. Private
registries work if the daemon's user has logged in with `podman login`.

## From inside a sprite

A sprite with a [spawn policy](api.md#sprites-that-create-sprites) may create sprites from
images, but **only from images already in the cache**. A guest can never make the host
pull: that would let it fetch arbitrary content and fill the disk. An uncached image is a
`404 image_not_cached` that names the reference.

```sh
sprite-env sprites create game-7 --image node:22 --public
```

## What a disk built from an image contains

Everything in the image, plus what the base image adds on top of ubuntu and the agent relies on:

- a `sprite` account that exec sessions and services run as: uid and gid 1000 as in the base
  image, or the next free ids when the image already uses 1000 (node's images have a `node`
  user there). An image that already has a `sprite` account keeps its own.
- its home, `/home/sprite`, seeded from the image's `/etc/skel`;
- a locked entry in `/etc/shadow` and `/etc/gshadow` where the image has those files, and a
  NOPASSWD sudoers entry where the image has sudo;
- `/.sprite/image.json`: the reference, digest and the image's environment (`ENV`).

The image's environment is applied to every exec session and service, over the agent's base
environment, the way a container would get it: `PATH` additions such as `/usr/local/go/bin`,
`NODE_VERSION`, `PYTHON_VERSION`. `HOME`, `USER`, `LOGNAME` and `SHELL` stay the sprite
user's, and `/usr/local/bin` is kept on `PATH` for `sprite-env`. `ENTRYPOINT`, `CMD`,
`USER`, `WORKDIR`, `EXPOSE` and volumes are ignored: a sprite is a machine, not a container;
run the image's program as a service if you want it running.

Hostname, `/etc/hosts`, `resolv.conf`, `/proc` and the rest are set up by the agent at boot, as
for any sprite. Ownership, modes, hard links, setuid bits and file capabilities (xattrs) are
carried over: the flatten is `podman export` into a tar that mke2fs reads directly, so no user
namespace is involved (this needs e2fsprogs 1.47.1 or newer, whose `mkfs.ext4 -d` takes a tar).

The disk has the base image's size (20 GB apparent, sparse; `SPRITE_DISK_GB` at `make image`).

### Sudo

Most images ship no sudo (node, python, alpine, busybox, distroless), which would leave the
sprite user no way to install a package. On such a disk the agent installs a **stand-in** at
every boot: a setuid-root copy of `sprite-env` at `/.sprite/bin/sudo`, linked from
`/usr/local/bin/sudo`. It gives the sprite user (and only that user) root without a password,
like the base image's sudoers entry, and understands the options scripts commonly use (`-u`,
`-E`, `-H`, `-i`, `-s`, `-n`, `-k`, `-v`, `-l`); anything else is refused with an error rather
than ignored. The environment is reset as sudo's `env_reset` does, unless `-E`. As soon as the
disk has a real sudo (`apt-get install sudo`), the next boot removes the stand-in. Under the
privileges policy's `noNewPrivileges` it stops working, as the real sudo does.

### Odd userlands

| Image | Shell | What works |
|---|---|---|
| Debian/Ubuntu based (`node:22`, `python:3.12-slim`) | bash | Everything, as with the base image. |
| Alpine, busybox | busybox `sh`; no bash | Everything; an exec with no command starts `sh`, and `SHELL=/bin/sh`. Use `sh -c`, not `bash -c`, in commands. alpine's busybox has no `httpd` applet. |
| Distroless (`gcr.io/distroless/*`), scratch | none | Exec of a program by path (`/nodejs/bin/node app.js`), services, sprite URLs, the filesystem API, checkpoints. An exec with no command fails with "this sprite has no shell", one of `sh` with `executable "sh" not found in PATH`; `sprite-env` is there, but anything that needs a shell does not work. |

What was verified on a real daemon (no guest network, no reflink volume):

- node:22: exec as `sprite` (uid 1001, beside the image's `node`), the image's environment,
  sudo, `sudo -u node`, a service behind the sprite URL, and a file and the service surviving
  a warm suspend and a cold boot; spawning from inside, cached and uncached.
- alpine:3.20: exec, home, sudo, `sprite-env`, a service behind the sprite URL (the opt-in
  e2e test), spawning from inside.
- python:3.12-slim and busybox:1.37: exec, the image's environment, sudo.
- gcr.io/distroless/nodejs22-debian12: exec of `/nodejs/bin/node` as `sprite`, a file written
  through the filesystem API run as a service behind the sprite URL, a checkpoint, and the
  clear failures of `sh` and of an exec with no command.
- A `localhost/` image (the base image itself): its own `sprite` account and real sudo are kept,
  no stand-in is installed.

## Disk accounting

Cached disks live on the sprite volume, so the [disk guard](operations.md#disk-pressure)
sees them: a build is refused (`507`) unless about twice the image's size (the temporary tar
and the disk's blocks) fits above `--disk-reserve-mib`. `spritesd status` has an `images`
line, and its DISK/OWN columns count the blocks a sprite shares with the cached disk it came
from once, as they do for the base image.

What the guard does not cover is podman's own storage, on the filesystem of the daemon user's
home, which holds the compressed and unpacked layers during a pull. There is no automatic
eviction: remove what you no longer need with `spritesd images rm`.

## Not covered

- The image is pulled for the host's architecture only; there is no `--platform`.
- No pull progress through the API: a create simply blocks. `spritesd images pull` streams it.
- Image `HEALTHCHECK`, `STOPSIGNAL`, labels and the like are ignored.
