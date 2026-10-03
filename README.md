# wisp

A single-host sandbox server: persistent, hardware-isolated Linux environments on Firecracker
microVMs that suspend when idle and wake on the next request. It speaks the
[Sprites](https://sprites.dev) API, so the official `sprite` CLI and SDKs work against it
unmodified, and, from the same engine, the [E2B](https://e2b.dev),
[Vercel Sandbox](https://vercel.com/docs/sandbox) and [Daytona](https://daytona.io) APIs, so their
official SDKs do too. A subset of [Modal](https://modal.com)'s API, enough for its sandboxes,
is there as a spike.

This is an independent, unofficial project. It is not affiliated with or endorsed by Fly.io,
E2B, Vercel, Daytona or Modal. Their products are theirs; this only implements compatible APIs
for running on your own machine.

```
client (sprite CLI / SDK / curl)         E2B · Vercel · Daytona · Modal SDKs
   │  Sprites API, :7788                     │  one listener per API (sandboxd only)
   ▼                                         ▼
wispd / sandboxd ── front ends over one engine: wake on request, suspend when idle,
   │                go cold after a TTL; one store, one set of API keys, one dashboard
   │  vsock, both directions (no guest network needed)
   ▼
Firecracker microVM ── wisp-agent as PID 1 (from an initramfs) ── your ext4 disk
                       └─ /.sprite/api.sock + sprite-env, for use from inside
```

`wispd` serves the Sprites API alone. `sandboxd` is `wispd` with the other APIs added, and takes
every `wispd` flag. Run one or the other on a data directory, not both.

## Install

Needs Linux with read/write access to `/dev/kvm`, Go, and rootless podman. wispd runs as
you; nothing below needs root until the optional step at the end.

```sh
git clone https://github.com/arugula-salad/wisp && cd wisp
make deps image          # Firecracker + a guest kernel, then the base disk image
make install-service     # build, install as a systemd user service, start on 127.0.0.1:7788
```

`make run` instead of `make install-service` runs it in the foreground, for trying it out.
Either way the API token is written to `~/.local/share/wisp/token` on first start.

Give sprites a network (once, needs root). Without it they have no NIC; exec, checkpoints,
sprite URLs and the TCP proxy still work, because those travel over vsock.

```sh
make netd && sudo ./scripts/setup-host.sh
```

More in [host setup](docs/host-setup.md) (networking, and instant copy-on-write clones) and
[operating it](docs/operations.md) (the service, surviving reboots, `wispd status`, limits).

## Use it

Everything that speaks the Sprites API needs two things: where the server is, and the token.

```sh
export SPRITES_API_URL=http://127.0.0.1:7788
export SPRITE_TOKEN=$(cat ~/.local/share/wisp/token)
```

### With an official SDK

The only change from upstream's docs is the base URL. Python (`pip install sprites-py`):

```python
import os
from sprites import SpritesClient

client = SpritesClient(os.environ["SPRITE_TOKEN"], base_url=os.environ["SPRITES_API_URL"])

sprite = client.create_sprite("dev")                 # a fresh VM; cold until first used
print(sprite.command("uname", "-a").output().decode())   # boots it (~200 ms), runs, returns stdout

sprite.command("sh", "-c", "echo hello > ~/note").run()
print(sprite.command("cat", "/home/sprite/note").output().decode())   # the disk persists across suspends

client.delete_sprite("dev")
```

Leave a sprite alone for 30 seconds and it suspends to disk; the next call wakes it in about
20 ms with its processes and memory intact ([lifecycle](docs/lifecycle.md)).

JavaScript (`npm install @fly/sprites`):

```js
import { SpritesClient } from '@fly/sprites';

const client = new SpritesClient(process.env.SPRITE_TOKEN, { baseURL: process.env.SPRITES_API_URL });

const sprite = await client.createSprite('dev');
const { stdout } = await sprite.exec('uname -a');
console.log(stdout);

await client.deleteSprite('dev');
```

Go (`go get github.com/superfly/sprites-go`):

```go
client := sprites.New(os.Getenv("SPRITE_TOKEN"), sprites.WithBaseURL(os.Getenv("SPRITES_API_URL")))

sprite, err := client.CreateSprite(ctx, "dev", nil)
if err != nil {
	log.Fatal(err)
}
defer client.DeleteSprite(ctx, "dev")

out, err := sprite.CommandContext(ctx, "uname", "-a").Output()
fmt.Print(string(out))
```

### In a browser

wispd serves a dashboard on the API address: open <http://127.0.0.1:7788/> and paste the
token. It shows every sprite and the host at a glance, with an hour of CPU, memory, disk and
state history, request traffic and latency, and lets you open a terminal in any sprite, browse its files, take and restore
checkpoints, and edit its policies ([web UI](docs/web-ui.md)).

<picture>
  <source media="(prefers-color-scheme: dark)" srcset="docs/images/dashboard-dark.png">
  <img alt="The dashboard's overview: sprites by state, CPU and memory in use, the sprite volume, charts of state, CPU and memory over time, and a lane per sprite showing when it was running, warm or cold" src="docs/images/dashboard-light.png">
</picture>

<table><tr>
<td width="50%"><picture>
  <source media="(prefers-color-scheme: dark)" srcset="docs/images/sprite-dark.png">
  <img alt="One sprite's page: current CPU, memory, disk and checkpoints, with its CPU and memory over time" src="docs/images/sprite-light.png">
</picture></td>
<td width="50%"><img alt="A terminal in the browser, running a shell inside a sprite" src="docs/images/terminal.png"></td>
</tr></table>

### With the `sprite` CLI

It reads the two variables above:

```sh
sprite create dev
sprite exec -s dev -- uname -a
```

### With the E2B, Vercel, Daytona or Modal SDKs

Run `sandboxd` in place of `wispd`. It serves the Sprites API on `--listen` exactly as `wispd`
does, and each other API on a listener of its own. They share the data directory, the store and
the API keys, and the dashboard shows every sandbox, but a sandbox belongs to the API that made
it: E2B sandboxes are not in the Sprites API's lists, nor sprites in E2B's. `wispd status`,
`wispd keys` and the other subcommands work against a `sandboxd`'s data directory too.

```sh
systemctl --user stop wisp                # if wispd is installed as a service: one daemon per data directory
make build                                # bin/wispd and bin/sandboxd
./scripts/build-image.sh e2b              # each API's guest disk, as <data>/images/<api>.ext4
./scripts/build-image.sh vercel
./scripts/build-image.sh daytona
./bin/sandboxd --vercel-listen 127.0.0.1:7824 --daytona-listen 127.0.0.1:7842
```

| API | Listener | Point the SDK at it | Guide |
|---|---|---|---|
| [E2B](https://e2b.dev) | `--e2b-listen`, on by default at `127.0.0.1:7820` | `E2B_API_URL` and `E2B_SANDBOX_URL` = `http://127.0.0.1:7820`, `E2B_API_KEY` = the token | [Using the E2B SDKs](docs/e2b-sdk.md) |
| [Vercel Sandbox](https://vercel.com/docs/sandbox) | `--vercel-listen`, off by default | `VERCEL_TOKEN` = the token, any `VERCEL_TEAM_ID` and `VERCEL_PROJECT_ID`; the JS SDK through a `fetch` preload | [Using the Vercel Sandbox SDKs](docs/vercel-sdk.md) |
| [Daytona](https://daytona.io) | `--daytona-listen`, off by default | `DAYTONA_API_URL` = `http://127.0.0.1:7842/api`, `DAYTONA_API_KEY` = the token | [Using the Daytona SDKs](docs/daytona-sdk.md) |
| [Modal](https://modal.com) (spike: sandboxes and exec only) | `--modal-listen`, off by default | `MODAL_SERVER_URL` = `http://127.0.0.1:7852`, `MODAL_TOKEN_SECRET` = the token, any `MODAL_TOKEN_ID` | [Using the Modal client](docs/modal-client.md) |

"The token" is `~/.local/share/wisp/token` or a key from `wispd keys create`. Then the official
SDKs work as they are:

```sh
export E2B_API_URL=http://127.0.0.1:7820 E2B_SANDBOX_URL=http://127.0.0.1:7820
export E2B_API_KEY=$(cat ~/.local/share/wisp/token)
python3 -c 'from e2b import Sandbox; print(Sandbox.create().commands.run("uname -a").stdout)'

export DAYTONA_API_URL=http://127.0.0.1:7842/api DAYTONA_API_KEY=$(cat ~/.local/share/wisp/token)
python3 -c 'from daytona import Daytona; print(Daytona().create().process.exec("uname -a").result)'

export VERCEL_TOKEN=$(cat ~/.local/share/wisp/token) VERCEL_TEAM_ID=team_local VERCEL_PROJECT_ID=prj_local
VERCEL_SANDBOX_URL=http://127.0.0.1:7824 node --import ./e2e/providers/vercel/target.mjs app.mjs
```

Each guide covers its flags and what works, and links to how it differs from the hosted product.

### Sprite URLs

Every sprite has a URL that wakes it and proxies to port 8080 inside it (or to the service you
mark with `http_port`):

```sh
sprite exec -s dev -- sh -c 'mkdir -p ~/site && echo hi > ~/site/index.html'
sprite exec -s dev -- sprite-env services create web --cmd python3 \
  --args "-m,http.server,8080,--directory,/home/sprite/site" --http-port 8080
curl -H "Authorization: Bearer $SPRITE_TOKEN" http://dev.sprites.localhost:7788/
```

To serve them on the internet under your own domain, over HTTPS, without exposing the API:
[public sprite URLs](docs/public-urls.md). A sprite can also answer at custom domains of its
own (`game.example.com`), each with its own certificate: [custom domains](docs/public-urls.md#custom-domains).

## Documentation

[Project website](https://arugula-salad.github.io/wisp/) · [Website preview and publishing](docs/site.md)

| | |
|---|---|
| [Host setup](docs/host-setup.md) | Guest networking and the reflink volume: the two optional steps that need root once |
| [Operating it](docs/operations.md) | Running as a service, reboots, `wispd status`, limits, disk pressure, what is not built |
| [Lifecycle](docs/lifecycle.md) | `running` / `warm` / `cold`, what keeps a sprite awake, tasks, what a sprite costs in memory, autoscale |
| [API keys](docs/api-keys.md) | Named, revocable admin and read-only keys (`wispd keys`), and serving the API in public with `--api-host` |
| [Web UI](docs/web-ui.md) | The browser dashboard: what it shows, how it signs in, reaching it from another machine |
| [API coverage](docs/api.md) | What is implemented, and `sprite-env` for use from inside a sprite |
| [Container images](docs/images.md) | Creating a sprite from any image (`"from": {"image": "node:22"}`), the image cache, odd userlands |
| [Events and webhooks](docs/events.md) | Our own addition: a live event stream (SSE) of everything that happens to sprites, and signed webhooks |
| [Public sprite URLs](docs/public-urls.md) | A wildcard domain, automatic certificates, the listener that serves only sprite URLs, and custom domains |
| [Backups](docs/backups.md) | Incremental, deduplicated backups to any S3-compatible bucket, and restoring onto a new host |
| [Security](docs/security.md) | How network policy is enforced, how each Firecracker is confined, and what neither covers |
| [Differences from the hosted product](docs/differences.md) | Deliberate ones, and the official Go SDK issues this server works around |
| [Using the E2B SDKs](docs/e2b-sdk.md) | `sandboxd --e2b-listen`: the E2B API for the official E2B SDKs, with E2B's own envd in the guest, and [how it differs from hosted E2B](docs/providers/e2b-differences.md) |
| [Using the Modal client](docs/modal-client.md) | A spike: `sandboxd --modal-listen` runs the unmodified `modal` client's sandboxes and exec, and [how it differs from hosted Modal](docs/providers/modal-differences.md) |
| [Using the Vercel Sandbox SDKs](docs/vercel-sdk.md) | `sandboxd --vercel-listen`: the Vercel Sandbox API for the official Vercel SDKs, and [how it differs from hosted Vercel](docs/providers/vercel-differences.md) |
| [Using the Daytona SDKs](docs/daytona-sdk.md) | `sandboxd --daytona-listen`: the Daytona API, for the official Daytona SDKs, and [how it differs from hosted Daytona](docs/providers/daytona-differences.md) |
| [Development](docs/development.md) | The code's layout (`engine/`, `frontend/`, the two daemons), tests, the e2e suites against the official SDKs, extra dev stacks |
