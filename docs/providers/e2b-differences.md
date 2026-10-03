# Differences from hosted E2B

What wisp's E2B front end (`frontend/e2b`, served by `sandboxd --e2b-listen`) does differently
from hosted E2B, on purpose or because it is not built. The specification it follows is
[e2b.md](e2b.md); how to point the SDKs at it is [Using the E2B SDKs](../e2b-sdk.md).

The yardstick is the probe suite in `e2e/providers/e2b/`: the official Python and JS SDKs
(2.52.0) pass all 26 steps against `sandboxd`, and a `--record` run differs from hosted E2B's
golden traces only in IDs, timestamps, hosts, and what is listed here.

## Seen in the traces

| | Hosted E2B | wisp |
| --- | --- | --- |
| `templateID` | the resolved template ID (`rki5dems9wqfm4r03t7g` for `base`) | `base`: the template has no other ID. `alias` is `base` on both |
| `envdVersion` | `0.6.10` (the `base` template's envd) | `0.9.0`: the envd the E2B image is built with (`scripts/build-envd.sh`), past every SDK feature gate |
| `domain` in create, connect, get and list | absent (the SDK falls back to `E2B_DOMAIN`) | `--e2b-domain` with the listener's port, `e2b.localhost:7820` by default, so that `getHost(port)` is a host:port this server answers |
| `cpuCount`, `memoryMB` | 2, 512 | 2, 512 by default (`--e2b-vcpus`, `--e2b-mem-mib`); `memTotal` in metrics is the guest's, a little under that |
| `diskSizeMB` | 23301 | the E2B image's size, 20480 (`SPRITE_DISK_GB` when it is built) |
| `GET /sandboxes/{id}/metrics` | a sample every few seconds, with history over `start`/`end`; `[]` right after boot | one sample, read from envd's own `/metrics` when the call is made, for a sandbox whose VM is up; `[]` for one that is paused or whose VM is down. `start` and `end` are ignored |
| `X-RateLimit-Limit` / `-Remaining` / `-Reset` | on every response | absent. Nothing is rate limited except by the daemon's own limits (`--max-running` and the like), which answer 429 |
| Unary envd responses | relayed chunked by E2B's proxy | relayed as envd sends them (`Content-Length`); the bodies are the same |
| Directory sizes, PIDs, the PTY's output chunks | | differ, being another guest: they are the guest kernel's and filesystem's |
| A port nobody listens on yet | | `502 {"message": "The sandbox is running but port is not open"}` (hosted's proxy wording, JSON only). The probe's port step retries past it while `python3 -m http.server` starts |

## Not in the traces

**Reaching it.**

- The SDKs need `E2B_SANDBOX_URL` pointed at the E2B listener. Without it they send envd
  traffic to `https://49983-<id>.<domain>`, which needs wildcard DNS and a certificate; the
  E2B listener serves plain HTTP only. Signed file URLs then carry no sandbox ID and are
  routed by their signature, as [e2b.md section 6](e2b.md#6-pointing-the-sdks-at-another-server)
  describes. One that matches no running sandbox's token is 401; an unsigned one names no
  sandbox at all and reaches the control plane, which answers 404.
- Port traffic is `http://<port>-<id>.<domain>` on the same listener, not `https`. `*.localhost`
  resolves to the loopback address with systemd-resolved, browsers and Node; plain glibc may not.
- HTTP/1.1, and HTTP/2 without TLS (h2c, prior knowledge). No TLS at all.

**Auth.**

- `X-API-Key` is the daemon's root token (`<data>/token`) or one of its API keys (`wispd
  keys`), the same keys as the Sprites API. A read-scoped key may only `GET`; anything else is
  403. There are no teams: every key sees every E2B sandbox.
- The 401 message names wisp's keys rather than E2B's dashboard.
- envd's own auth is envd's, unchanged: the access token, signed URLs, Basic auth for the user.
- `network.allowPublicTraffic: false` and its `e2b-traffic-access-token` are not honoured:
  ports are reachable by anyone who can reach the listener, as on hosted E2B by default.

**Lifecycle.**

- A timeout is enforced by the engine's deadline sweep, which runs every 30 seconds, so a
  sandbox may outlive its `endAt` by up to that long. A sandbox killed by its timeout is
  deleted, disk and all, as hosted.
- `--e2b-max-timeout` (24 h by default) bounds `timeout`, where hosted has a per-team limit
  (1 h on the hobby tier).
- Pause is wisp's warm suspend: a memory snapshot. `{"memory": false}` suspends and then drops
  the snapshot, so the next connect cold-boots on the sandbox's disk. A paused sandbox has no
  timeout and is kept until it is killed, as hosted. But its memory snapshot goes cold after
  `--warm-ttl` (1 h by default), like any suspended sprite's: a connect after that boots it
  afresh with its files and without its processes, where hosted keeps the memory.
- `autoPause` (the SDK's `onTimeout: 'pause'`) is honoured: the sandbox is suspended at its
  timeout, reads as paused at once, and a connect resumes it. `autoResume` is stored and
  reported in `lifecycle`, but traffic to a paused sandbox gets the proxy's 502 rather than
  resuming it; only a connect does.
- Processes started through envd survive a pause and resume (the VM's memory is restored)
  and are lost on a cold boot, as on hosted E2B. Streams open across a pause end with
  `unavailable`, as hosted.
- When `sandboxd` stops it suspends every running VM. An E2B sandbox stays `running` as far
  as the API is concerned (its timeout keeps counting) and its VM resumes on its next envd or
  port request.
- The deprecated `POST /sandboxes` (v1, 15 s default timeout), `GET /sandboxes`,
  `POST /sandboxes/{id}/resume` and `POST /sandboxes/{id}/refreshes` are served; `/v2` create,
  list and connect are what the SDKs use.

**Not built.** Templates other than `base` (and the template build API: `POST /v3/templates`
and the rest answer 404), snapshots and `fork`, volumes, `PUT /sandboxes/{id}/network`,
`mcp`, `secure: false` and secrets, sandbox logs and events, `GET /sandboxes/metrics`,
`autoPauseMemory`. An unknown template is `404 template '<name>' not found`, as hosted.

**The guest.**

- envd runs with `-isnotfc` (it is not on E2B's Firecracker hosts), so it never polls E2B's
  metadata service for its token: the front end hands it the token, env vars, default user
  `user` and workdir `/home/user` with `POST /init` after every start of the VM, cold or warm.
  `/init` and envd's other internal endpoints (`/freeze`, `/upgrade`, ...) are not reachable
  through the listener (404).
- The front end sets `E2B_SANDBOX=true`, `E2B_SANDBOX_ID` and `E2B_TEMPLATE_ID` in the
  commands' environment, as hosted envd does; the marker file `/run/e2b/.E2B_SANDBOX` still
  says `false`.
- The hostname is `e2b` and the user `user` (uid 1000), as hosted. The image is wisp's own,
  modelled on E2B's `base` ([images.md](../images.md#the-e2b-image)): Node 22 where E2B's has
  Node 20, and a `sprite` account at uid 1001 for wisp's own agent.

**wisp's side.** E2B sandboxes are engine records with `api: "e2b"`: they share the store, the
volume, the daemon's limits, backups and the event stream (where they appear by ID, with no
name), but not the Sprites API, whose lists and lookups do not see them. The web dashboard and
`wispd status` list sprites only; E2B sandboxes do not appear there yet.
