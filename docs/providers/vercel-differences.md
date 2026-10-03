# Differences from hosted Vercel Sandbox

What wisp's Vercel Sandbox front end (`frontend/vercel`, served by `sandboxd --vercel-listen`)
does differently from hosted Vercel, on purpose or because it is not built. The specification
it follows is [vercel.md](vercel.md); how to point the SDKs at it is
[Using the Vercel Sandbox SDKs](../vercel-sdk.md).

The yardstick is the probe suite in `e2e/providers/vercel/`: the official JS (3.5.1) and Python
(0.7.0) SDKs pass all their steps (16 and 11) against `sandboxd`, and a recorded run has the
same exchanges, statuses, content types and body shapes as hosted Vercel's golden traces
except for IDs, timestamps, hosts and what is listed here.

## Seen in the traces

| | Hosted Vercel | wisp |
| --- | --- | --- |
| `region` | `iad1` | `local` |
| `image` | `vercel/sandbox/universal@sha256:…` | `vercel/sandbox/universal` (the guest is `images/vercel`, below) |
| Guest OS, kernel | Ubuntu 26.04, kernel 6.18 | Ubuntu 24.04 (wisp's base), the host's guest kernel |
| `routes[].url` | `https://<subdomain>.vercel.run` | `http://<subdomain>.<--vercel-domain>:<port>`, `vercel.localhost:7824` by default: plain HTTP on the Vercel listener. The listener also answers `/<subdomain>/…` paths |
| Command output lines | one line per chunk the guest produced, merged where they arrived together | one line per read of the process's pipe: a quick command's output may come as several `stdout` lines where hosted sends one, and the order of stdout against stderr lines written at the same moment can differ. Both SDKs join lines per stream, so the text is the same |
| `kill` | the signal goes to the command's process (observed: a `sh` loop died on SIGTERM only after its `sleep` ended) | the signal goes to the command's process group, as wisp-agent's exec does: children get it too, and `sh` may report a killed child (`Terminated` on stderr) |
| Totals (`totalEgressBytes`, `totalIngressBytes`, `totalActiveCpuDurationMs`, a session's `networkTransfer`, `activeCpuDurationMs`) | measured | `0`. `totalDurationMs` and a session's `duration` are measured |
| Snapshot `sizeBytes` | the compressed snapshot (821 MB for the default image) | the checkpoint's allocated blocks (~870 MB; shared blocks counted in full) |
| `x-ratelimit-*` headers | on every response | absent; nothing is rate limited except by the daemon's own limits (`--max-running` and the like), which answer 429 with `Retry-After` |
| `snapshot` on a persistent sandbox's stop, `creationMethod` | (not in the traces: the probes use non-persistent sandboxes) | a snapshot with `creationMethod: "stop"`; manual ones are `"manual"` |

## Not in the traces

**Reaching it.**

- The JS SDK has no base URL: it needs the `fetch` preload `e2e/providers/vercel/target.mjs`
  (`node --import …/target.mjs`), which rewrites `https://vercel.com/api` to
  `VERCEL_SANDBOX_URL`. Python takes `SandboxServiceOptions(base_url=…)`.
- JS's `sandbox.domain(port)` is hardcoded to `https://<subdomain>.vercel.run` and ignores
  `route.url`. The shim rewrites those URLs too, for fetches in the same process, to
  `<server>/<subdomain>` by default (or `VERCEL_SANDBOX_DOMAIN_TEMPLATE`); anything outside
  the process (a browser, curl) has to use `route.url`.
- Routes are plain HTTP. `*.localhost` resolves to the loopback address with systemd-resolved,
  browsers and Node, but often to `::1` only: on a `127.0.0.1` listener, `sandboxd` also serves
  the Vercel API on `[::1]` at the same port when the domain is a `.localhost` one. Plain glibc
  without systemd-resolved may not resolve `*.localhost` at all; use the `/<subdomain>/` path
  form then.
- The API is served with and without the `/api` prefix.

**Auth.**

- The bearer token is the daemon's root token or one of its API keys (`wispd keys`), the same
  keys as the other APIs. A read-scoped key may only `GET` (and `fs/read`'s `POST`); anything
  else is 403. A bad token is `403 forbidden`.
- `teamId` is accepted and ignored: one daemon is one team. A sandbox remembers the project it
  was created in, and get/list/delete with another `projectId`/`project` do not see it, but
  names are unique across all projects of the daemon (hosted: per project).
- Tokens are opaque, never JWTs, so the JS SDK never enters its OIDC refresh path.

**Lifecycle.**

- A session's timeout is the engine's deadline, with action stop; extend-timeout moves it. A
  session that has timed out is noticed (and, for a persistent sandbox, snapshotted) the next
  time anything looks at the sandbox, and its `stoppedAt` is the deadline.
- Stop of a persistent sandbox checkpoints the disk (the snapshot) and keeps only the latest
  stop snapshot; resume boots the kept disk in a new session. Stop of a non-persistent sandbox
  discards its filesystem as far as the API is concerned (a resume restores its last manual
  snapshot, or is `400 snapshot_not_found`), but the disk itself stays on the volume until the
  sandbox is deleted.
- A session is a cold boot: processes do not survive a stop, as on hosted.
- Snapshots are checkpoints of the sandbox they were taken of: **deleting a sandbox deletes its
  snapshots**, whatever `deleteOrphanSnapshots` says (hosted keeps them until they expire).
  Sandboxes created from a snapshot are independent copies and are unaffected. `expiration` is
  validated and reported but nothing expires snapshots yet.
- Create from a snapshot copies the disk (instant on a reflink volume, a full sparse copy
  otherwise).
  The new sandbox reports the source as its `currentSnapshotId`, as hosted does, but the
  snapshot stays the source sandbox's: a non-persistent sandbox made from it that is then
  stopped cannot resume from it (`400 snapshot_not_found`) until it takes a snapshot of its own.
  A persistent one resumes from its own disk as usual.
- Sandboxes are not removed after 14 days idle.

**Commands.**

- Commands run through wisp-agent's exec: as `ubuntu` (uid 1000) with `HOME=/vercel` in
  `/vercel` by default; `sudo: true` runs `sudo -n -E -- <command>` (root, with the given
  environment; `PATH` is sudo's `secure_path`). A missing executable under `sudo` is sudo's
  error and exit code 1, not `400 executable_not_found`.
- Commands live in the daemon's memory: a daemon restart forgets them (and their output),
  though the processes in a running guest go on. Output past 64 MiB per command is dropped
  from the logs. A command whose VM goes away (a stop) ends with exit code 137.
- `timeout` on a command sends SIGKILL to its process group.
- Interactive PTYs (`POST …/interactive`), network policies, `PATCH`, fork, git and tarball
  sources, drives, snapshot trees and regions are not implemented: they answer 404 (or 400 for
  an unsupported `source.type`).

**Files.**

- `fs/write` extracts the tar host-side: regular files go through the agent's filesystem API
  (mode honoured, parents created, owned by `ubuntu`), directories and links through one
  `mkdir -p`/`ln -sfn` command as `ubuntu`. Files are written by the agent as root and handed to
  `ubuntu`, so a write into a root-owned directory succeeds where an unprivileged extraction
  would not. mtimes are the time of the write.
- `fs/read` reads as root. `fs/mkdir` runs `mkdir` as `ubuntu`; its error messages follow
  hosted's (`error creating directory: <path>: File exists`, `error creating directory: No
  such file or directory`).
