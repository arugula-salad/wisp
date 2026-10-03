# Daytona: API surface the official SDKs depend on

Phase 0 survey for a Daytona-compatible front-end (see `docs/plans/multi-provider-burndown.md`).
Goal: the unmodified Python `daytona` and TypeScript `@daytonaio/sdk` packages work against wisp
with only `DAYTONA_API_URL` and `DAYTONA_API_KEY` changed.

The core v1 surface (section 8) is built: `frontend/daytona`, served by `sandboxd
--daytona-listen` ([Using the Daytona SDKs](../daytona-sdk.md)). Where it differs from what this
survey describes, and every choice made where the survey says "unverified", is in
[daytona-differences.md](daytona-differences.md).

## Sources and licensing

| Artifact | Version read | License | Notes |
| --- | --- | --- | --- |
| `daytona` (PyPI) | 0.220.0 | Apache-2.0 | Hand-written SDK. Paths below are relative to the wheel (`daytona/...`); upstream lives in `daytonaio/daytona` under `libs/sdk-python/src/daytona/`. |
| `daytona-api-client` (PyPI) | 0.220.0 | Apache-2.0 | OpenAPI-generated control-plane client (repo `daytona/clients`). Pydantic models are the de facto response schema. |
| `daytona-toolbox-api-client` (PyPI) | 0.220.0 | Apache-2.0 | OpenAPI-generated client for the in-sandbox daemon ("toolbox"). |
| `@daytonaio/sdk` (npm) | 0.220.0 | Apache-2.0 | Same structure as Python; read `cjs/Daytona.js`, `cjs/Sandbox.js`, `cjs/Process.js`, `cjs/utils/WebSocket.js`. |
| Daytona server (API, runner, proxy, daemon) | not read | **AGPL-3.0** | Not consulted. Everything here comes from the SDKs and generated clients. No server code may enter this repo, and the AGPL daemon must not be shipped in our guest image. |

All behaviour below is described from the client side. Where the client can't tell us what the
server does, the text says "unverified" (golden traces need a Daytona key).

## 1. Endpoint and target resolution

| Setting | Config field | Env var | Default | Notes |
| --- | --- | --- | --- | --- |
| API base URL | `api_url` (`server_url` deprecated) | `DAYTONA_API_URL`, then `DAYTONA_SERVER_URL` (deprecated, warns) | `https://app.daytona.io/api` | **Process env only.** A `DAYTONA_API_URL` in `.env`/`.env.local` is ignored, with a warning (`_utils/env.py`, `warn_if_dotenv_api_url_ignored`). The base includes the `/api` path, and every control-plane path below is appended to it. |
| Target (region) | `target` | `DAYTONA_TARGET` | unset, so the server picks the org default | Sent as `target` in the `POST /sandbox` body. The SDK doesn't route on it. |
| API key | `api_key` | `DAYTONA_API_KEY` | none | Required unless JWT is used. |
| JWT + org | `jwt_token`, `organization_id` | `DAYTONA_JWT_TOKEN`, `DAYTONA_ORGANIZATION_ID` | none | Dashboard-style auth. |
| Polling mode | `use_deprecated_polling` | `DAYTONA_USE_DEPRECATED_POLLING` | false | When true, the SDK skips the Socket.IO event stream (section 3). |
| OTel | `otel_enabled` | `DAYTONA_OTEL_ENABLED` | false | Client-side only. |

Source: `daytona/_sync/daytona.py` `Daytona.__init__`; TS `cjs/Daytona.js` lines ~120-245.

## 2. Auth and common headers

Every control-plane request (and, through a copied header set, every toolbox request) carries these headers:

| Header | Value | Server must |
| --- | --- | --- |
| `Authorization` | `Bearer <api_key or jwt>` | Validate it. Map to wisp API keys. |
| `X-Daytona-Source` | `sdk-python` / `sdk-typescript` | Ignore. |
| `X-Daytona-SDK-Version` | e.g. `0.220.0` | Ignore. |
| `X-Daytona-Organization-ID` | org id (JWT mode only) | Ignore, or use as namespace. |
| `User-Agent` | `sdk-python/<ver>` | Ignore. |

The toolbox client is a deep copy of the main client's headers (`_clone_api_client_to_toolbox_api_client`).
**The toolbox and proxy receive the same bearer API key.** That keeps one auth path for wisp.

## 3. Event stream (Socket.IO): optional, but the SDK tries it on construction

`Daytona()` starts a background Socket.IO client (`daytona/internal/event_dispatcher.py`, TS
`EventDispatcher`) unless polling mode is set:

- URL: the origin of `api_url`, with `socketio_path = <api_url path>/socket.io/` (so `/api/socket.io/`),
  `transports=["websocket"]`, `auth={"token": <bearer>}`, query `organizationId`, `source`, `sdkVersion`.
- It listens for `sandbox.created` and `sandbox.state.updated`. The payload is `{sandbox: <Sandbox DTO>}`
  or `{id, newState}`, and the resource id is taken from `sandbox.id` or a top-level `id`.
- **If the connection fails, the SDK falls back to polling.** `_wait_for_state` polls
  `GET /sandbox/{id}` every 0.1 s, backing off to 1 s (`daytona/_sync/sandbox.py` ~1366-1460).

v1 can return 404 on `/api/socket.io/` and lean on polling. Implementing Engine.IO v4 + Socket.IO v5
later cuts latency. Go has libraries for both, and the server side only emits.

## 4. Control plane (REST, JSON, camelCase)

All paths are relative to `api_url`. Path params accept the **id or the name** (`{sandboxIdOrName}`).
Response bodies are JSON with camelCase keys. Generated models keep unknown keys in
`additional_properties`, so extra fields are harmless. **Missing required fields fail pydantic validation.**

### 4.1 Sandbox DTO (`daytona_api_client/models/sandbox.py`)

| Field | Required | Type / notes |
| --- | --- | --- |
| `id`, `organizationId`, `name`, `user`, `target` | yes | strings. `user` is the OS user (Daytona's images use `daytona`). |
| `env`, `labels` | yes | `map<string,string>`. Labels must include `code-toolbox-language` (`python`/`javascript`/`typescript`), which the SDK writes on create and reads back on `get`/`list`. |
| `public`, `networkBlockAll`, `kvm` | yes | bool |
| `cpu`, `gpu`, `memory`, `disk` | yes | **int** (`StrictInt`: `1.0` fails). memory/disk in GiB. |
| `toolboxProxyUrl` | yes | Base URL for toolbox calls (section 5). |
| `state` | no | enum below. |
| `desiredState`, `errorReason`, `recoverable`, `backupState`, `autoStopInterval`, `autoArchiveInterval`, `autoDeleteInterval`, `autoPauseInterval`, `autoDestroyAt`, `createdAt`, `updatedAt`, `lastActivityAt`, `snapshot`, `volumes`, `buildInfo`, `daemonVersion`, `runnerId`, ... | no | Fill what we have. Timestamps are ISO-8601 strings. |

`SandboxState`: `creating restoring destroyed destroying started stopped starting stopping error
build_failed pending_build building_snapshot unknown pulling_snapshot archived archiving resizing
snapshotting forking pausing paused resuming`. Unknown values deserialize to
`unknown_default_open_api` instead of raising.

### 4.2 Endpoints

| Method + path | SDK caller | Body / query | Returns | v1? |
| --- | --- | --- | --- | --- |
| `POST /sandbox` | `Daytona.create` | `CreateSandbox`: `name, snapshot, user, env, labels, public, target, cpu, gpu, memory, disk, autoStopInterval, autoArchiveInterval, autoDeleteInterval, autoPauseInterval, ttlMinutes, volumes[{volumeId,mountPath,subpath}], buildInfo{dockerfileContent, contextHashes}, networkBlockAll, networkAllowList, ...` | Sandbox | **yes** |
| `GET /sandbox/{idOrName}` | `get`, `refresh_data`, all waits | | Sandbox. A 404 while waiting on delete counts as destroyed. | **yes** |
| `GET /sandbox` | `list` | `cursor, limit, labels` (JSON string), `states`, `name`, `id`, sort/filters | `{items: [SandboxListItem], nextCursor: string\|null}`. `nextCursor` is required and must be present, even as null. | **yes** |
| `DELETE /sandbox/{idOrName}` | `delete` | | Sandbox | **yes** |
| `POST /sandbox/{idOrName}/start` | `start` (then waits for `started`) | | Sandbox | **yes** |
| `POST /sandbox/{idOrName}/stop` | `stop` (then waits for `stopped`/`destroyed`) | `?force=true` means SIGKILL | Sandbox | **yes** |
| `GET /sandbox/{id}/toolbox-proxy-url` | Sandbox ctor, only if the DTO lacks `toolboxProxyUrl` | | `{url}` | **yes** (trivial) |
| `GET /sandbox/{idOrName}/ports/{port}/preview-url` | `get_preview_link` | | `{sandboxId, url, token}` | **yes** |
| `PUT /sandbox/{idOrName}/labels` | `set_labels` | `{labels}` | `{labels}` | yes |
| `POST /sandbox/{idOrName}/autostop/{min}` | `set_autostop_interval` | | Sandbox | yes |
| `POST .../autoarchive/{min}`, `.../autodelete/{min}`, `.../autopause/{min}`, `.../ttl/{min}` | setters | | Sandbox | later (store and echo in v1) |
| `POST /sandbox/{idOrName}/archive` | `archive`, then `refresh_data` | | Sandbox | later |
| `POST .../pause`, `.../recover`, `.../resize`, `.../fork`, `.../snapshot`, `.../backup` | same-named | | Sandbox | later (pause and fork map well to wisp checkpoints) |
| `GET .../ports/{port}/signed-preview-url`, `POST .../signed-preview-url/{token}/expire` | signed previews | `?expiresInSeconds` | `{sandboxId, port, token, url}` | later |
| `GET /sandbox/{id}/signing-key`, `POST .../signing-key/rotate` | `download_url`/`upload_url` (HMAC-signed toolbox file URLs, `_utils/file_url_signing.py`) | | key | later |
| `POST/DELETE .../ssh-access`, `GET /sandbox/ssh-access/validate` | SSH | | | later |
| `GET /sandbox/{id}/build-logs-url` | create with `on_snapshot_create_logs` | | `{url}`, which the SDK streams with `?follow=true` (plain chunked text) | later |
| `GET /sandbox/{id}/telemetry/*`, `GET /config` | metrics (`/config` supplies `analyticsApiUrl`) | | | later |
| `POST/GET/DELETE /snapshots[/{id}]`, `/snapshots/{id}/build-logs-url`, `.../activate` | `daytona.snapshot.*` | `CreateSnapshot{name, imageName, buildInfo, cpu, memory, disk, entrypoint}` | SnapshotDto (`id, general, name, state, size, entrypoint, cpu, gpu, mem, disk, errorReason, createdAt, updatedAt, lastUsedAt` all required, some nullable) | later |
| `GET /object-storage/push-access` | image context upload | | S3 creds (`storageUrl, accessKey, secret, sessionToken, bucket, organizationId`). The SDK then uploads tarballs with `obstore` S3. | later (needs an S3-compatible endpoint) |
| `POST/GET/DELETE /volumes`, `GET /volumes/by-name/{name}` | `daytona.volume.*` | `{name}` | VolumeDto (`id, name, organizationId, state, createdAt, updatedAt, errorReason`). States: `creating ready pending_create pending_delete deleting deleted error`. | later |
| secrets, warm pools, regions, jobs, webhooks, audit | | | | out of scope |

### 4.3 Lifecycle semantics (as the SDK assumes them)

| Concept | Behaviour | wisp mapping |
| --- | --- | --- |
| create | Returns right away in some state. The SDK waits for `started` and fails on `error`/`build_failed`. Default timeout 60 s. | Engine create plus boot. Return `started` synchronously if it's fast. |
| stop / start | `stopped` keeps the filesystem and kills processes | suspend-to-disk or cold stop. Both satisfy the contract. |
| auto-stop | `autoStopInterval` minutes idle (default 15, 0 = off). Activity is toolbox and preview traffic, or `POST /sandbox/{id}/last-activity`. | Per-sandbox idle policy (slice 1.7). |
| auto-archive | Minutes stopped before moving to object storage. `archived` starts slower. | Echo the field. `archive` can be a no-op state rename. |
| auto-delete | Minutes stopped before destroy. Negative = off, 0 = delete on stop ("ephemeral"). | Policy hook. |
| pause / resume | `paused` keeps memory | wisp suspend (Firecracker snapshot). |
| ttl | Wall-clock max lifetime. Sets `autoDestroyAt`. | Expiry sweep. |

## 5. Toolbox (in-sandbox daemon) API

### 5.1 How the SDK reaches it

- The toolbox URL is `{toolboxProxyUrl}/{sandboxId}{path}`. The Python `ToolboxApiClientProxy`
  (`daytona/internal/toolbox_api_client_proxy.py`) prefixes the resource path with `/{sandboxId}` and
  sets the host to `toolboxProxyUrl`. TS sets the axios `baseURL = toolboxProxyUrl + "/" + id`.
- **The routing is path-based, with no per-sandbox hostname,** and the server picks `toolboxProxyUrl`
  per sandbox. For wisp, set it to e.g. `http://127.0.0.1:7790/toolbox` (same listener as the API)
  and route `/toolbox/{id}/...` to the guest agent. The SDK re-reads the field on every refresh, so it
  can change after a restart.
- Auth is the same bearer header. WebSockets (log follow, PTY, interpreter) swap `http` for `ws` on the
  same URL and send the headers. Browser, Deno and serverless TS runtimes can't set headers, so they
  append `?DAYTONA_SANDBOX_AUTH_KEY=<preview token>` and a `X-Daytona-SDK-Version~<v>` subprotocol
  (`cjs/utils/WebSocket.js`). The token comes from `GET .../ports/1/preview-url`.
- Errors: a JSON envelope `{message, code|error_code, source}` with `source="DAYTONA_DAEMON"` maps to
  typed errors such as `FILE_NOT_FOUND`, `PROCESS_EXECUTION_TIMEOUT`, `SESSION_ENDED` and `GIT_*`
  (`daytona/common/errors.py` `CODE_TO_ERROR`). Anything else maps by HTTP status: 400 BadRequest,
  401 Auth, 403 Forbidden, 404 NotFound, 408/504 Timeout, 409 Conflict, 410 Gone, 422, 429 RateLimit,
  500, 502, 503. Control-plane errors use the same envelope with `source="DAYTONA_API"`.

### 5.2 Endpoints by group (paths after `/{sandboxId}`)

| Group | Endpoints | Request / response shape the SDK needs | v1? |
| --- | --- | --- | --- |
| info | `GET /user-home-dir`, `GET /work-dir`, `GET /version` | `{dir}`, `{dir}` | **yes** |
| one-shot exec | `POST /process/execute` | req `{command: string, cwd?, envs?, timeout?(s)}` resp `{exitCode, result}`. `result` is combined output. HTTP timeout = `timeout+5`. `command` is one shell string (SDK docs say "shell command"). **Whether the daemon runs it through `sh -c` or word-splits it is unverified; check with a golden trace.** | **yes** |
| code run | `POST /process/code-run` | `{code, language, argv?, envs?, timeout?}` resp `{exitCode, result, artifacts{charts}}`. `language` comes from the label. | yes (python only: write a temp file and exec) |
| sessions | `POST /process/session` `{sessionId}` (201); `GET /process/session` → `[Session]`; `GET/DELETE /process/session/{sid}`; `POST /process/session/{sid}/exec` `{command, runAsync?, async?(deprecated), suppressInputEcho?}` → `{cmdId, exitCode?, output?, stdout?, stderr?}`; `GET .../command/{cid}` → `{id, command, exitCode?}` (null while running); `GET .../command/{cid}/logs` → `{output, stdout, stderr}`, or with `?follow=true` over WebSocket; `POST .../command/{cid}/input` `{data}` | Session = one long-lived shell. Commands share cwd and env, run in order, and are identified by `cmdId`. The follow WebSocket sends binary frames with stdout chunks prefixed `\x01\x01\x01` and stderr prefixed `\x02\x02\x02` (`daytona/common/process.py`, `_utils/stream.py`) and closes when the command exits. | **yes** |
| entrypoint | `GET /process/session/entrypoint`, `.../entrypoint/logs` | Same shapes, for the image's entrypoint | later |
| PTY | `POST /process/pty`, `GET /process/pty[/{id}]`, `DELETE`, `POST .../resize`, WS `.../pty/{id}/connect`, WS `.../pty/create-connect?cols&rows&...` (envs as base64url JSON in a subprotocol) | Raw terminal bytes over WS, with an exit-control subprotocol | later (wisp already has PTY exec) |
| filesystem | `GET /files?path&depth` → `[FileInfo]`; `GET /files/info?path` → FileInfo; `POST /files/folder?path&mode` (201); `DELETE /files?path&recursive` (204); `POST /files/move?source&destination`; `POST /files/permissions?path&mode&owner&group`; `GET /files/find?path&pattern` (grep) → `[{file,line,content}]`; `GET /files/search?path&pattern` (glob) → `{files}`; `POST /files/replace` `{files, pattern, newValue}` | `FileInfo` = `{name, isDir, size, mode, permissions, owner, group, modTime, modifiedAt?, path?}`. All except `modifiedAt`/`path` are required. | **yes** (list/info/folder/delete/move); later (find/search/replace/permissions) |
| file transfer | `POST /files/bulk-upload`: multipart form, fields `files[i].path` + file part `files[i].file`. `POST /files/bulk-download`: JSON `{paths:[...]}`, answered with **`multipart/form-data`**, one part per path, `name="file"` (or `name="error"` with a JSON error payload), `filename=<source path>`. `GET /files/download?path` and `POST /files/upload-v2?path` are the single-file and signed-URL variants. | The SDK's `download_file` and `upload_file` both go through the bulk endpoints (`daytona/_sync/filesystem.py` ~363-500, ~923-980). The client parses the multipart response itself, so a missing boundary is fatal. | **yes** |
| git | `/git/clone, status, add, commit, push, pull, branches, checkout, history, init, remotes, reset, restore, config, credentials` | JSON. Errors carry `GIT_*` codes. | later (could shell out to `git` in the guest) |
| LSP | `/lsp/start, stop, did-open, did-close, completions, document-symbols, workspacesymbols` | Proxies to language servers in the guest | later / out |
| code interpreter | `POST/GET/DELETE /process/interpreter/context`, WS `GET /process/interpreter/execute` | Stateful Jupyter-like contexts | later |
| computer use | `/computeruse/*` (~33 endpoints: mouse, keyboard, screenshot, a11y, recordings, display) | Needs Xvfb, VNC and xdotool in the image | out |
| misc | `GET /port`, `GET /port/{p}/in-use`, `GET /system/metrics`, `POST /init`, `POST /env` | `update_env` uses `/env` | later |

## 6. Preview URLs (port access)

- `GET /sandbox/{id}/ports/{port}/preview-url` → `{sandboxId, url, token}`. The SDK only passes these
  through. On hosted Daytona the URL is a per-port, per-sandbox hostname on the proxy domain
  (`{port}-{sandboxId}.<proxy domain>`). For private sandboxes the client sends the token as the
  `x-daytona-preview-token` header (public docs, unverified). `public: true` sandboxes skip the token.
- wisp mapping: host routing like E2B (`<port>-<id>.daytona.localhost`) through the shared
  host-routing proxy. `token` can be a per-sandbox HMAC.
- Signed preview URLs (token baked into the hostname, with an expiry) come later.

## 7. Images and snapshots

| SDK input | Wire | wisp v1 |
| --- | --- | --- |
| nothing | `POST /sandbox` without `snapshot`, so the server uses its default snapshot | A configured default image (python3, node, git, user `daytona` with home `/home/daytona`). |
| `CreateSandboxFromSnapshotParams(snapshot="name")` | `snapshot` field | Look up a wisp image by name. Unknown name returns 404 or 400. |
| `CreateSandboxFromImageParams(image="debian:12")` | `buildInfo.dockerfileContent = "FROM debian:12\n"`, state `pending_build` | Support a `FROM <ref>`-only Dockerfile through the existing OCI import. Reject anything else with 400 in v1. |
| `Image.debian_slim(...).pip_install(...)` | A full Dockerfile (`FROM python:3.x-slim-bookworm`, `RUN apt-get ...`), plus `contextHashes` uploaded to S3 via `/object-storage/push-access` | Later. Needs a Dockerfile builder (buildkit or kaniko in a builder VM) and an S3 shim. |

## 8. Core v1 surface vs later

**Core v1, about 26 routes:**

| Area | Routes |
| --- | --- |
| Sandboxes (10) | `POST/GET /sandbox`, `GET/DELETE /sandbox/{id}`, `start`, `stop`, `toolbox-proxy-url`, `ports/{p}/preview-url`, `labels`, `autostop/{n}` |
| Exec (2) | `process/execute`, `process/code-run` (python) |
| Sessions (8) | create, list, get, delete, `exec` (sync and `runAsync`), get command, command logs (GET and WS follow with the 3-byte prefixes), command input |
| Files (8) | `files` list, `info`, `folder`, delete, `move`, `bulk-upload`, `bulk-download` (multipart response), `user-home-dir`/`work-dir` |
| Plumbing | Bearer auth, error envelope `{message, code, source}`, 404 on `/api/socket.io/` (polling fallback), `code-toolbox-language` label echo, strict-int resources |

**Later:** Socket.IO events; archive, pause, fork, resize and snapshot-from-sandbox; the
auto-archive/auto-delete/ttl enforcers (accept and store in v1); the snapshots API and declarative
image builds with the S3 context upload; volumes (host directory or virtio-fs share); signed preview
and file URLs; PTY; git; find/search/replace/permissions; code interpreter; LSP; SSH;
telemetry/metrics. **Out:** computer use, GPUs, warm pools, secrets vault, regions, webhooks.

## 9. Effort estimate relative to E2B

| Dimension | E2B | Daytona | Ratio |
| --- | --- | --- | --- |
| Control plane for SDK basics | ~8 REST routes | ~10 REST routes, with a fatter DTO (~15 required fields, strict ints) | ~1.2x |
| In-sandbox API | `envd` is open source (Apache-2.0). We can run it in the guest or mirror its Connect-RPC protocol. | The daemon is **AGPL**, so we can't embed it. We implement ~18 toolbox routes ourselves, in wisp-agent or as a front-end translator over `AgentTransport`. | ~2x |
| Streaming | Connect server-streaming for process output | Two mechanisms: WS log follow with byte-prefix framing, and multipart download responses | ~1x |
| Session model | Processes with PIDs | Persistent shell sessions with command ids, ordered execution and per-command exit codes. Needs a small session manager (one shell per session, sentinel-delimited commands). | +0.5x |
| Routing | Host-based `<port>-<id>` for both envd and ports | Path-based toolbox (simpler), host-based previews (same as E2B) | ~0.8x |
| Images | Template build API | `FROM`-only in v1, and the declarative builder can wait | ~1x |

**Overall: about 1.5x the E2B front-end for v1.** If the E2B front-end takes N implementer-slices,
budget about 1.5N. The extra cost is all in the toolbox: no reusable daemon, and the session
semantics. The control plane is routine. Main risk: the SDK releases often (0.220 at time of
writing) and validates responses strictly, so the probe suite should pin an SDK version and a
CI job should re-run it against the newest release.
