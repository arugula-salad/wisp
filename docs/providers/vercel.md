# Vercel Sandbox: API survey for a compatible front-end

Phase 0 survey for serving the official, unmodified Vercel Sandbox SDKs from wisp's engine.
Written 2026-10-02 from SDK source and Vercel's docs, then checked against hosted Vercel with the
probe suite in [`e2e/providers/vercel/`](../../e2e/providers/vercel) (golden traces in its
`golden/` directory). Anything marked **observed** comes from those traces, not from docs.

## Sources read

| What | Version | Where |
| --- | --- | --- |
| JS SDK `@vercel/sandbox` | 3.5.1 (npm, 2026-09-28) | [github.com/vercel/sandbox](https://github.com/vercel/sandbox) tag `@vercel/sandbox@3.5.1` (commit `85d53ff`), `packages/vercel-sandbox/src/`: `api-client/api-client.ts`, `api-client/base-client.ts`, `api-client/with-retry.ts`, `api-client/validators.ts`, `api-client/file-writer.ts`, `sandbox.ts`, `session.ts`, `command.ts`, `snapshot.ts`, `filesystem.ts`, `utils/get-credentials.ts`, `utils/normalizePath.ts`, `utils/resolveSignal.ts`, `constants.ts` |
| Python SDK | `vercel-sandbox` 0.7.0, `vercel-internal-core` 0.2.0, `vercel-oidc` 0.9.0 | PyPI wheels. The `vercel` package (0.11.4) has no sandbox code of its own: its README says `vercel.sandbox` is "delivered by the separately owned `vercel-sandbox` dependency". Files: `vercel/sandbox/_internal/api_client.py`, `options.py`, `async_runtime.py`, `vercel/_internal/core/session.py`, `vercel/oidc/credentials.py` |
| Images | same repo, `images/{ubuntu,universal,node,python,al-base,al-node,al-python}/Dockerfile` | |
| Docs | fetched 2026-10-02 | [vercel.com/docs/sandbox](https://vercel.com/docs/sandbox), [concepts](https://vercel.com/docs/sandbox/concepts), [persistence](https://vercel.com/docs/sandbox/concepts/persistent-sandboxes), [images](https://vercel.com/docs/sandbox/concepts/images), [pricing and limits](https://vercel.com/docs/sandbox/pricing), [REST: Sandboxes](https://vercel.com/docs/rest-api/sandboxes) |

Older SDK majors (1.x/2.x, which addressed sandboxes by ID) were not surveyed. Target 3.x.

## The model in one paragraph

A **sandbox** is a named, long-lived record in a project (`name` unique per project). A
**session** is one running VM of that sandbox, with an ID like `sbx_…`. Everything that touches
the VM (commands, files, stop, extend, snapshot) is addressed by **session ID**:
`/v2/sandboxes/sessions/{sessionId}/…`. Everything about the record (get, list, update, delete,
fork) is addressed by **name**: `/v2/sandboxes/{name}`. Sandboxes are persistent by default: stop
snapshots the filesystem, and the next call that needs a VM resumes it in a new session from that
snapshot. `persistent: false` discards the filesystem on stop.

## Base URL: where the SDKs send requests, and how to redirect them

**JS SDK: not overridable.** `APIClient` (`api-client/api-client.ts:85`) sets
`baseUrl: params.baseUrl ?? "https://vercel.com/api"`, but no public entry point passes a
`baseUrl`: `Sandbox.create/get/list/fork/getOrCreate`, `Snapshot`, `Drive`, `Session` and
`Command` all build `new APIClient({ teamId, token, fetch? })`. No environment variable is read for
it. Options, least invasive first:

1. **Preload a `fetch` shim (what the probe suite does).** `BaseClient` captures
   `params.fetch ?? globalThis.fetch` when each client is constructed (`base-client.ts:38`), and
   every request goes through it. Patching `globalThis.fetch` before the SDK runs, via
   `NODE_OPTIONS=--import=/path/target.mjs`, rewrites `https://vercel.com/api/...` to our host.
   No change to the SDK or the app; 30 lines (`e2e/providers/vercel/target.mjs`). Cost: a Node flag
   in the app's environment. Works for Node only (also Bun/Deno with their own preload mechanisms).
2. **The public `fetch` option.** `Sandbox.create/get/list/fork/getOrCreate`, `Snapshot.get/list`
   and `Drive.*` accept `fetch` (`WithFetchOptions`). A wrapper that rewrites the URL works for
   clients built from those calls, but the lazily built clients (`ensureClient()` in `Sandbox`,
   `Session`, `Command`, `Snapshot`; used after workflow deserialization and for `delete()`,
   `listSessions()` on a deserialized sandbox) ignore it and fall back to `globalThis.fetch`.
   Cost: app code change, and incomplete.
3. **HTTP(S)_PROXY does not work.** `BaseClient.request` passes its own undici
   `Agent({ bodyTimeout: 0 })` as `dispatcher` (`base-client.ts:17,63`), which bypasses any env or
   global proxy dispatcher.
4. **DNS + TLS.** Resolve `vercel.com` to our server and trust a local CA for it
   (`NODE_EXTRA_CA_CERTS`). No app change, but it hijacks `vercel.com` for the whole machine (the
   OIDC refresh path and anything else that talks to Vercel) and needs a certificate for a domain
   we do not own. Last resort.

**Python SDK: overridable.** `SandboxServiceOptions(base_url=...)` (`sandbox/_internal/options.py`,
default `DEFAULT_SANDBOX_API_BASE_URL = "https://vercel.com/api"`), passed on an SDK session:

```python
from vercel.api import session
from vercel.sandbox import SandboxServiceOptions
async with session(service_options=[SandboxServiceOptions(base_url="http://127.0.0.1:7820/api")]):
    ...
```

There is no environment variable for it. (Other `vercel` Python services default to
`https://api.vercel.com`; Vercel's REST docs also use `api.vercel.com`. Both SDKs use
`https://vercel.com/api` for Sandbox; a server should mount everything under an `/api` prefix and
ignore the prefix choice.)

**Port URLs: hardcoded host in JS.** `Session.domain(port)` (`session.ts`) returns
`https://${route.subdomain}.vercel.run` and ignores `route.url`. Python exposes `route.url`
verbatim, so it is free-form. For JS, the options are: our `fetch` shim also rewrites
`https://<sub>.vercel.run` (only helps fetches made in the same process, which is how the probe
checks ports); real DNS for `*.vercel.run` plus a trusted cert (same cost as above); or a
`subdomain` value that smuggles a host, e.g. `subdomain: "127.0.0.1:7821/s/sb-abc#"` →
`https://127.0.0.1:7821/s/sb-abc#.vercel.run` (the fragment is dropped). That still forces
`https`, so the server needs TLS with a cert the client trusts. Recommendation: serve `route.url`
correctly for Python, document the shim for JS, and treat the `vercel.run` host as a known
difference.

## Authentication: what arrives on the wire

Every request (both SDKs, **observed**):

- `Authorization: Bearer <token>`
- `?teamId=<team>` on **every** request, merged into the query (`api-client.ts:139`;
  Python `_request`, `api_client.py`). REST docs also allow `?slug=`; the SDKs never send it.
- The project travels separately per call: `projectId` in the JSON body of create/fork,
  `?projectId=` on get/update/delete of a named sandbox, `?project=` on list sandboxes/sessions/
  snapshots, `?projectId=` on drives.
- `content-type: application/json` on all JS requests (even GETs), except `fs/write`.
- `user-agent: vercel/sandbox/3.5.1 (Node.js/v22.23.2; linux/x64)` (JS; with
  ` agent/<name>` appended when `@vercel/detect-agent` recognises an AI agent, unless
  `VERCEL_SANDBOX_TELEMETRY_DISABLED` or `VERCEL_TELEMETRY_DISABLED` is set) or
  `vercel-sandbox/0.7.0 (Python/3.14.4 …; Linux/x86_64)`.

Where the SDKs get credentials:

- **JS**: either all three of `{ token, teamId, projectId }` passed to the call (partial sets
  throw), or a Vercel **OIDC token** from `VERCEL_OIDC_TOKEN` / the Vercel runtime via
  `@vercel/oidc`. It does **not** read `VERCEL_TOKEN`. In local dev with neither, it starts an
  interactive device-auth flow (`utils/dev-credentials.ts`) unless `CI` is set or
  `NODE_ENV=production`.
- If the token is a JWT whose payload has `owner_id`, JS treats it as OIDC: `teamId` is replaced by
  the `owner_id` claim and `projectId` by `project_id`, and before **every** request it calls
  `getVercelOidcToken()` to refresh (errors ignored) (`api-client.ts:94–127`).
- **Python**: `SandboxServiceOptions(credentials_factory=...)`, else `vercel.oidc.get_credentials()`,
  which takes `VERCEL_OIDC_TOKEN` (+ claims or `VERCEL_PROJECT_ID`/`VERCEL_TEAM_ID`) or
  `VERCEL_TOKEN` + `VERCEL_PROJECT_ID` + `VERCEL_TEAM_ID` from the environment.

What our server must accept: any bearer token we issue, `teamId` on every request (it can be a
fixed string we hand out with the token), and a project ID wherever it appears. **Issue opaque
(non-JWT) tokens**, or JWTs without an `owner_id` claim, so the JS SDK never enters the OIDC
refresh path. Map (team, project) to wisp's namespace for the Vercel front-end; names are unique
per project.

## Errors, retries and status codes the SDKs interpret

Error body (both SDKs parse it; JS puts `error.message` into `APIError.message`):

```json
{"error": {"code": "not_found", "message": "Named sandbox 'x' not found for this project."}}
```

Codes and statuses that change SDK behaviour:

| Status + code | Where | SDK behaviour |
| --- | --- | --- |
| 404 any | `GET /v2/sandboxes/{name}` | `Sandbox.getOrCreate` creates the sandbox |
| 404 any | `POST …/fs/read` | `readFile*` returns `null` (JS); `SandboxPathNotFoundError` (Python) |
| 410 any | any session call | `withResume`: call `GET /v2/sandboxes/{name}?resume=true`, retry once on the new session. **Observed** body `{"code":"sandbox_stopped","message":"Sandbox has stopped execution and is no longer available"}` (`py/021`) |
| 422 `sandbox_stopping` / `sandbox_snapshotting` | any session call | same resume-and-retry |
| 410 `snapshot_not_found` | `GET /v2/sandboxes/{name}` | `getOrCreate` deletes the named sandbox and recreates it. (**Observed**: resuming a non-persistent sandbox that has no snapshot answers **400** `snapshot_not_found`, "Cannot resume sandbox: no snapshot available.") |
| 429 | anything | retried (2 retries, 400 ms base, ×2) honouring `Retry-After` up to 20 s; longer gives up |
| 5xx / network error | anything | retried the same way, so deterministic failures must not be 5xx |

Other **observed** errors: `400 executable_not_found` (`[invalid_argument] executable file not
found in $PATH: <cmd>`) from `POST …/cmd` (it does not start a process with exit code 127);
`400 file_error` from mkdir (`error creating directory: /vercel/probe-dir: File exists`,
`error creating directory: No such file or directory`); `400 bad_request` for invalid list/snapshot
parameters (``Invalid request: `namePrefix` is only valid when `sortBy` is `name` ``,
``` `expiration` must be 0 (no expiration) or >= 86400000 ms (1 day). ```). REST docs list 400,
401, 402, 403, 404, 409, 410, 422, 429 ("The concurrency limit has been exceeded."), 500 and 502.

Hosted responses carry `x-ratelimit-limit/remaining/reset` (**observed**: limit 20 for create,
delete sandbox and delete snapshot; 1000 for the rest). The SDKs ignore them.

Response validation: JS validates success bodies with zod (`validators.ts`), objects are not
strict (extra fields pass), but required fields must be present with the right types; timestamps
are **numbers** (ms since epoch), although the REST docs' examples show strings. Python validates
with pydantic the same way.

## Objects

All timestamps are integer milliseconds since the epoch. IDs **observed**: session
`sbx_` + 28 base62 chars, command `cmd_` + 28 lowercase hex, snapshot `snap_` + 28 base62, route
subdomain `sb-` + 12 lowercase base36. The SDKs treat all of them as opaque strings.

**Sandbox** (`validators.ts` `Sandbox`; required in bold): **name**, **persistent**, region,
failoverRegions, networkId, vcpus, memory (MB), runtime, image, timeout (ms; the configured
per-session timeout), networkPolicy, totalEgressBytes, totalIngressBytes, totalActiveCpuDurationMs,
totalDurationMs, **createdAt**, **updatedAt**, expiresAt, **currentSessionId**, currentSnapshotId,
**status** (session status enum), statusUpdatedAt, cwd, tags, mounts, snapshotExpiration,
keepLastSnapshots. **Observed** extra: `architecture: "amd64"`; `runtime: "node22"` even for the
default image.

**Session**: **id**, **memory**, **vcpus**, **region**, runtime, **timeout**, **status** ∈
`pending | running | stopping | stopped | failed | aborted | snapshotting`, **requestedAt**,
startedAt, requestedStopAt, stoppedAt, abortedAt, duration, sourceSnapshotId, snapshottedAt,
**createdAt**, **cwd**, **updatedAt**, interactivePort, networkPolicy, activeCpuDurationMs,
networkTransfer `{ingress, egress}`. **Observed** extras: `architecture`, `sourceSandboxName`,
`projectId`, `interactivePort: 26661`. Python additionally requires the session in a stop response
to have the ID it asked to stop.

**Route**: `{url, subdomain, port}` (+ `system: bool` in Python). **Observed**
`{"url":"https://sb-55drmefroggp.vercel.run","subdomain":"sb-55drmefroggp","port":3000}`.

**Command**: **id**, **name** (the executable), **args**, **cwd**, **sessionId**, **exitCode**
(`null` while running), durationMs (optional), **startedAt**.

**Snapshot**: **id**, **sourceSessionId**, **region**, regions, **status** ∈
`created | deleted | failed`, **sizeBytes**, expiresAt, **createdAt**, **updatedAt**, lastUsedAt,
creationMethod (**observed** `"manual"`), parentId.

**Pagination**: `{"count": n, "next": "<cursor>" | null}`; the next page is requested with
`?cursor=`.

## Endpoints the SDKs call

Paths are relative to the base URL (`https://vercel.com/api`). `{sid}` is a session ID. Every
request also carries `?teamId=`. "Golden" points at the trace file under
`e2e/providers/vercel/golden/`.

### Sandboxes (by name)

| Method, path | Request | Response | Golden |
| --- | --- | --- | --- |
| `POST /v3/sandboxes` (JS when `runtime` is unset; Python always) / `POST /v2/sandboxes` (JS when legacy `runtime` is set) | JSON: `projectId`, `name?`, `ports` (JS always sends, default `[]`), `source?`, `timeout?` (ms), `resources? {vcpus, memory?}`, `runtime?` \| `image?`, `persistent?`, `networkPolicy?`, `networkId?`, `env?`, `tags?` (≤5), `snapshotExpiration?`, `keepLastSnapshots? {count, expiration?, deleteEvicted?}`, `mounts?`, `region?`, `failoverRegions?`, plus any `__private` params | `200 {sandbox, session, routes, resumed?}` | `js/001`, `js/032` |
| `GET /v2/sandboxes/{name}?projectId=&resume=true\|false` | | `200 {sandbox, session, routes, resumed?}`; `resume=true` starts a new session if stopped | `js/028`; `py/022` (resume, 400); `js/030`, `py/023` (404) |
| `GET /v2/sandboxes?project=&limit=&sortBy=createdAt\|name\|statusUpdatedAt&sortOrder=&namePrefix=&cursor=&tags=k:v` | `namePrefix` requires `sortBy=name` | `200 {sandboxes: [Sandbox], pagination}` | `js/029`, `py/019` |
| `PATCH /v2/sandboxes/{name}?projectId=` | `persistent, resources, timeout, networkPolicy, networkId, tags, ports, snapshotExpiration, keepLastSnapshots, currentSnapshotId, region, failoverRegions, mounts` | `200 {sandbox, routes?}` | not exercised |
| `DELETE /v2/sandboxes/{name}?projectId=&deleteOrphanSnapshots=true` | | `200 {sandbox}` | `js/037`, `py/025` |
| `POST /v2/sandboxes/{name}/fork?projectId=` | like create, minus `source`/`runtime` | `200 {sandbox, session, routes}` | not exercised |

`source` is one of `{type:"git", url, depth?, revision?, username?, password?}`,
`{type:"tarball", url}`, `{type:"snapshot", snapshotId}` (snapshot excludes runtime/image).
Resources: 1 or an even number of vCPUs up to the plan max, 2048 MB per vCPU, default 2 vCPUs
(docs). **Observed** create latency: 250 ms fresh; 8.7 s from a snapshot.

### Sessions (by session ID)

| Method, path | Request | Response | Golden |
| --- | --- | --- | --- |
| `GET /v2/sandboxes/sessions/{sid}` | | `{session, routes}` | (Python `refresh`) |
| `GET /v2/sandboxes/sessions?project=&name=&limit=&cursor=&sortOrder=` | | `{sessions, pagination}` | not exercised |
| `POST /v2/sandboxes/sessions/{sid}/stop` | none (JS) or `{}` (Python) | `200 {session, sandbox?, snapshot?}`; returns when stopped (~1–2.5 s **observed**); **idempotent** on a stopped session (200, same body) | `js/034`, `js/035`, `py/020`, `py/024` |
| `POST /v2/sandboxes/sessions/{sid}/extend-timeout` | `{duration}` (ms) | `200 {session}` with `timeout` increased by `duration`; the sandbox's configured `timeout` is unchanged | `js/027`, `py/018` |
| `POST /v2/sandboxes/sessions/{sid}/snapshot` | `{expiration?}` (0 = never, else ≥ 86 400 000) or no body | **`201`** `{snapshot, session}`; session goes to `snapshotting` and then stops; 3.7 s and an 821 MB snapshot for the default image (**observed**) | `js/031` |
| `POST /v2/sandboxes/sessions/{sid}/network-policy` | policy object | `{session}` | not exercised |
| `POST /v2/sandboxes/sessions/{sid}/interactive` | `{}` | `{url, token}` (PTY over a socket; used by `openInteractive` and the CLI) | not exercised |

### Commands

| Method, path | Request | Response | Golden |
| --- | --- | --- | --- |
| `POST /v2/sandboxes/sessions/{sid}/cmd` (waited) | `{command, args, cwd?, env, sudo, wait:true, logs:true, timeout?}`; Python also sends `?wait=true&logs=true` | `200`, `content-type: application/x-ndjson` (**JS compares the header for exact equality**, so no `; charset`), streamed | `js/003`, `py/002` |
| `POST /v2/sandboxes/sessions/{sid}/cmd` (detached) | same without `wait`/`logs` | `200 {command}` with `exitCode: null` | `js/007`, `py/003` |
| `GET /v2/sandboxes/sessions/{sid}/cmd` | | `{commands: [Command]}` (Python `query_processes`) | not exercised |
| `GET /v2/sandboxes/sessions/{sid}/cmd/{cid}` | `?wait=true` blocks until exit (Python sends `wait=false` explicitly otherwise) | `200 {command}` | `js/009`, `js/011` |
| `GET /v2/sandboxes/sessions/{sid}/cmd/{cid}/logs` | | `application/x-ndjson` stream of log lines from the **start** of the command's output; ends when the command exits | `js/008`, `py/004` |
| `POST /v2/sandboxes/sessions/{sid}/cmd/{cid}/kill` | `{signal: <number>}` (JS maps names: HUP 1, INT 2, QUIT 3, KILL 9, TERM 15, CONT 18, STOP 19; default 15) | `200 {command}` (still `exitCode: null`) | `js/010`, `py/005` |

Waited-command stream, one JSON object per line (**observed**, `js/003`):

```
{"command":{"id":"cmd_…","name":"sh","args":[…],"cwd":"/vercel","sessionId":"sbx_…","startedAt":…,"exitCode":null}}
{"data":"err1\n","stream":"stderr"}
{"data":"out1\n","stream":"stdout"}
{"data":"out2\n","stream":"stdout"}
{"command":{…,"exitCode":3,"durationMs":1007}}
```

The first line must be the command; the stream ends right after a second `command` object whose
`exitCode` is a number (Python rejects output after it, a third command object, or a different
ID). Log lines can be split at arbitrary byte boundaries in `data` but are whole JSON lines. An
error mid-stream is `{"stream":"error","data":{"code":"…","message":"…"}}` and the SDKs throw
`StreamError`/`SandboxStreamError`. Ending the stream without the final command object is an error
(`stream_ended_early`). The `logs` endpoint uses the same line format without the command
objects. Each line is flushed as produced (**observed** 150 ms to first line, then real-time).

Exit codes (**observed**): normal exit status; signal deaths are reported shell-style as 128+n
(SIGTERM 143, SIGKILL 137); a trapped SIGTERM reports the trap's exit status. Commands run as the
default user with `HOME=/vercel` and cwd `/vercel` unless `cwd` is given; `env` is merged over the
sandbox's `env`; `sudo: true` runs as root (uid 0). `timeout` (ms) kills with SIGKILL on expiry
(per SDK docs).

### Files

| Method, path | Request | Response | Golden |
| --- | --- | --- | --- |
| `POST /v2/sandboxes/sessions/{sid}/fs/write` | body: **gzip-compressed tar**, `content-type: application/gzip`, `x-cwd: /` (extract dir). Python sends it with `transfer-encoding: chunked` | `200 {}` | `js/015`, `py/007` |
| `POST /v2/sandboxes/sessions/{sid}/fs/read` | `{path, cwd?}` | `200`, `content-type: application/octet-stream` (JS **requires** it, else throws), raw bytes; missing file `404 {"error":{"code":"not_found","message":"File not found."}}` | `js/016`, `js/019` |
| `POST /v2/sandboxes/sessions/{sid}/fs/mkdir` | `{path, cwd?}`; Python adds `recursive: true` | `200 {}`; JS (no `recursive`) gets `400 file_error` for an existing dir or a missing parent | `js/020`–`024`, `py/012` |

Tar entry names are relative to `x-cwd` (`/`): both SDKs resolve relative paths against the
session `cwd` **client-side**, so `probe/hello.txt` arrives as `vercel/probe/hello.txt` and
`/tmp/x` as `tmp/x`. File modes in the tar headers are honoured (0755 stayed executable); tar
uid/gid are 0 but files end up owned by the default user (`ubuntu`) (**observed**). Python writes
mtime 0. `fs/read` takes relative paths as-is and resolves them against the session cwd
server-side. Parent directories are created by extraction.

Everything else in the JS `sandbox.fs` (readdir, stat, rm, rename, chmod, exists, …) and in Python's
`fs.exists/is_file/is_dir/listdir/remove/rename` is implemented **client-side with commands**
(`ls`, `stat`, `test`, `rm`, `mv`, small `sh -c` scripts; see `filesystem.ts` and `py/013`), so
they need nothing beyond `cmd`, `fs/read` and `fs/write`, but they do need a POSIX userland with
GNU coreutils-compatible flags in the guest.

### Snapshots and drives

| Method, path | Response |
| --- | --- |
| `GET /v2/sandboxes/snapshots?project=&name=&limit=&cursor=&sortOrder=` | `{snapshots, pagination}` |
| `GET /v2/sandboxes/snapshots/{id}` | `{snapshot}` |
| `DELETE /v2/sandboxes/snapshots/{id}` | `200 {snapshot}` with `status: "deleted"` (`js/036`) |
| `GET /v2/sandboxes/snapshots/tree?project=&snapshotId=&limit=&sortOrder=` | `{snapshots: [{snapshot, siblings, count}], anchor?, pagination}` |
| `GET /v2/sandboxes/drives?projectId=…`, `POST/DELETE /v2/sandboxes/drives/{name}` | drives (private beta); skip |

The REST index also lists `POST /v3/sandboxes/sessions/{sid}/snapshot`, `POST /v3/…/fork` and
`POST /v4/sandboxes`; SDK 3.5.1 does not call them.

## Lifecycle semantics

- Create returns a session already `running` (**observed** 250 ms). Default session timeout 5 min;
  max 45 min (Hobby) / 24 h (Pro, Enterprise) per session. On timeout the session stops.
- Stop: persistent sandboxes snapshot on stop (`stop` response carries `snapshot`); non-persistent
  ones discard the filesystem. Stopped sessions answer `410 sandbox_stopped` to session calls.
- Resume: the SDK resumes **lazily**. On 410 / 422 (`sandbox_stopping`, `sandbox_snapshotting`) it
  calls `GET /v2/sandboxes/{name}?resume=true`, takes the new `session` and `routes`, and retries
  the call once. `resumed: true` in that response fires the user's `onResume`. A new session gets
  a new `sbx_` ID and a fresh timeout. `stop()` and `update()` never auto-resume.
- Resuming a sandbox with no snapshot (non-persistent) fails with `400 snapshot_not_found`.
- Manual snapshot stops the session (status `snapshotting`, then `stopped`); further session calls
  then auto-resume from it.
- Sandbox retention: non-resumable sandboxes are removed after 14 days idle; snapshots expire 30
  days after last use by default.
- `Sandbox.getOrCreate({name})`: GET by name; 404 → create; 410 `snapshot_not_found` → DELETE then
  create.

## Guest environment (images)

- Default image `vercel/sandbox/universal` (Ubuntu 26.04, Node 24 LTS, Python 3.14, coding agents).
  Other managed images: `node:22|24|26`, `python:3.14`, `ubuntu`, `arch`; custom OCI images from
  Vercel Container Registry. **Observed**: `Ubuntu 26.04.1 LTS`, kernel `6.18.49`, `node` at
  `/usr/local/bin/node`, `python3` present.
- User `ubuntu` (uid 1000, in `sudo` and other groups), passwordless sudo, `HOME=/vercel`, working
  directory `/vercel` (`images/ubuntu/Dockerfile`: `usermod --home /vercel ubuntu`,
  `WORKDIR /vercel`). Session and command `cwd` are `/vercel`.
- Legacy `runtime` values (`node22`, `node24`, `node26`, `python3.13`, sent to `/v2/sandboxes`)
  map to the Amazon Linux 2023 images (`images/al-*`, user `vercel-sandbox`). The docs no longer
  describe them; the SDK marks `runtime` deprecated. A v1 front-end can accept them and boot the
  default image.
- Ports: declared up front in `ports` (max 15 per the SDK docs), each gets a route; the sandbox
  process just listens on `0.0.0.0:<port>`. Traffic reaches it at `https://<subdomain>.vercel.run`
  (**observed** 200 from `python3 -m http.server 3000` within ~1 s).

## Minimum viable surface for v1, in order

1. **Auth and envelope**: accept `Authorization: Bearer`, `teamId` on every request, the
   `/api` prefix, `{"error":{"code","message"}}` bodies, ms timestamps, and no 5xx for client
   errors.
2. **Create** `POST /v3/sandboxes` (and `/v2/sandboxes` with `runtime`) → `{sandbox, session,
   routes}` with a running session; honour `name`, `ports`, `timeout`, `resources.vcpus`, `env`,
   `persistent`, `tags`; ignore the rest gracefully.
3. **Waited command** `POST …/cmd` with the exact NDJSON stream (command line, log lines, final
   command line, exact `application/x-ndjson`).
4. **Detached command, get (+`?wait=true`), logs stream, kill.**
5. **Files**: `fs/write` (gzip tar, `x-cwd`), `fs/read` (octet-stream, 404), `fs/mkdir`.
6. **Stop** (idempotent, returns `{session, sandbox}`), **get by name** (`?resume=`), **delete**,
   **list** (`project`, `namePrefix`+`sortBy=name`, pagination).
7. **Extend timeout** and the per-session timeout that enforces it.
8. **Routes**: `routes[]` with a `url` we can actually serve (Python works directly; JS through the
   shim or the `vercel.run` workarounds above).
9. **Persistence and resume**: 410 `sandbox_stopped` on stopped sessions, `GET ?resume=true`
   starting a new session from the last filesystem state; stop of a persistent sandbox taking a
   snapshot. This maps onto wisp's checkpoint/restore and is the default mode in the SDKs, so it
   should land soon after v1.
10. **Snapshots**: `POST …/snapshot` (201), create with `source.type=snapshot`, get/list/delete.
11. Later or never: fork, `PATCH` update, sessions list, network policy, interactive PTY, git and
    tarball sources, drives, regions, tags filtering, snapshot trees.

## Probe suite and golden traces

`e2e/providers/vercel/run.sh` runs both SDKs against hosted Vercel (default), a compatible server
(`VERCEL_SANDBOX_URL=http://host:port/api`), or, with `RECORD=1`, hosted Vercel through
`record_proxy.py`, which rewrites `golden/js` and `golden/py` (one JSON file per exchange, NDJSON
chunks with arrival times, secrets and team/project IDs replaced by `<redacted>`). Not recorded:
traffic to `*.vercel.run` (the port check fetches it directly) and the interactive PTY endpoint
(not exercised). The run that produced the committed traces (2026-10-02) passed every step in
both SDKs and left no sandbox running and no snapshot undeleted.

## Surprises

- The JS SDK has no base-URL knob at all, and `domain()` hardcodes `vercel.run` while the API
  returns a perfectly good `route.url`.
- The JS SDK sends `content-type: application/json` on every request, including GETs, and checks
  `content-type` of NDJSON responses by exact string equality.
- `namePrefix` on list is rejected unless `sortBy=name`; snapshot `expiration` must be 0 or at
  least a day. Both are server-side validation the SDK does not do.
- A missing executable is a 400 from `cmd`, not a command with exit code 127.
- The JS SDK's `getOrCreate` treats `snapshot_not_found` as 410, but resuming a sandbox that never
  had a snapshot returns 400 with that code.
- A one-vCPU sandbox from the default image snapshots to ~821 MB, and creating from a snapshot
  took ~8.7 s against ~0.25 s for a fresh sandbox.
- Persistence is on by default, so every `stop()` costs snapshot storage unless the caller passes
  `persistent: false`.
