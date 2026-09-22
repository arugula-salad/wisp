# Web UI

spritesd serves a dashboard from the API listener, at `/ui/` (the bare `/` redirects there).
With the defaults that is <http://127.0.0.1:7788/>. It is plain HTML, CSS and JavaScript
embedded in the binary (`internal/webui`), with a vendored xterm.js for the terminal: no build
step, and nothing loaded from the internet.

## Signing in

Paste the API token, from `<data>/token` (by default `~/.local/share/mini-sprites/token`).
The page trades it for an HttpOnly, SameSite=Strict cookie derived from the token, so rotating
the token signs every browser out. The cookie alone authorizes nothing: every request must also
carry an `X-Mini-Sprites-UI` header, which a page on another origin (a sprite's own URL
included) cannot add without a CORS preflight spritesd never answers, or be a WebSocket whose
`Origin` is the dashboard's own host. The public listener (`--public-listen`) serves sprite URLs
only, never the dashboard.

## What it shows

- **Overview**: sprite counts by state, CPU in use across all VMs, VM memory, the sprite volume
  and networking; charts of states, CPU and memory over the last hour; a lane per sprite showing
  when it was running, warm or cold; memory and disk by sprite (disk split into what only that
  sprite holds and what it shares with clones); and a feed of state changes.
- **Sprites**: a filterable table with a 10-minute CPU sparkline per sprite, wake and suspend.
- **Sprite**: the same figures for one sprite, and tabs for a **terminal** (a login shell over
  the exec WebSocket), **files** (browse, view, download), **checkpoints** (create, restore,
  delete, clone into a new sprite), **services** (start, stop, restart), **policies** (network,
  privileges, resources, spawn, as JSON) and the raw API and operator records.
- **Host**: host memory, load and volume over time, the daemon, and stray Firecracker or
  spritesd processes (as `spritesd status` reports them).

Tabs that talk to the guest (terminal, files, services) wake the sprite; the files and services
tabs ask first. The dashboard itself never keeps a sprite awake.

Besides `/v1`, it uses three endpoints of its own under `/ui/api/`: `status` (the same JSON as
`spritesd status --json`), `metrics` (the history below) and `sprites/{name}/wake|suspend|cool`.
Suspend keeps memory (warm); cool drops a warm sprite's snapshot, as `--warm-ttl` would. Nothing
in the dashboard kills a running VM.

## Metrics history

spritesd samples every 5 seconds and keeps one hour in memory: counts by state, CPU and resident
memory of each sprite's Firecracker process, host memory and load, and volume usage. Disk figures
per sprite (which read extent maps) are refreshed every minute and whenever a sprite appears or
goes. The history is not persisted: a restart starts it over, and the charts show the gap.

## From another machine

The API listens on 127.0.0.1 by default. Rather than exposing it, forward the port:

```sh
ssh -L 7788:127.0.0.1:7788 <host>
```

and open <http://127.0.0.1:7788/> locally. Sprite links in the dashboard point at the sprite URLs
(`<name>.sprites.localhost:7788` by default, or your `--url-domain`).
