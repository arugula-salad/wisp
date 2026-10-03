# E2B API survey (phase 0)

What an E2B-compatible front-end on wisp's engine has to reproduce so that the **official,
unmodified** E2B SDKs work against it. Written 2026-10-02 from the sources below and checked
against hosted E2B with the probe suite in `e2e/providers/e2b/` (golden traces in
`e2e/providers/e2b/golden/`, referenced below as `py.json#N` / `js.json#N` by `seq`).

## Sources read

| What | Where | Version |
| --- | --- | --- |
| JS SDK (`e2b` on npm) | github.com/e2b-dev/E2B `packages/js-sdk/src` | 2.52.0 (commit `a9e893bc`, 2026-10-02) |
| Python SDK (`e2b` on PyPI) | same repo, `packages/python-sdk/e2b` | 2.52.0 |
| Control-plane OpenAPI | github.com/e2b-dev/infra `spec/openapi.yml` (also vendored as `E2B/spec/openapi.yml`) | infra commit `92197909`, 2026-10-03 |
| API server | infra `packages/api/internal/handlers/` (`sandbox_create.go`, `sandbox_kill.go`, `sandbox_pause.go`, ...) | same |
| Sandbox proxy routing | infra `packages/shared/pkg/proxy/host.go`, `packages/orchestrator/pkg/proxy/proxy.go` | same |
| envd (in-guest daemon) | infra `packages/envd/` (`main.go`, `internal/api/*.go`, `internal/services/*`), specs `packages/envd/spec/{envd.yaml,process/process.proto,filesystem/filesystem.proto}` | envd 0.9.0 at HEAD; **hosted base template runs envd 0.6.10** |
| envd boot unit | infra `packages/orchestrator/pkg/template/build/core/rootfs/files/envd.service.tpl` | same |

Both repositories are Apache-2.0.

Key SDK files: `js-sdk/src/connectionConfig.ts` (URL resolution), `src/api/index.ts` (REST
client, error mapping), `src/sandbox/sandboxApi.ts` (every control-plane call),
`src/sandbox/index.ts` (envd transport, headers, signed URLs), `src/envd/rpc.ts` (Connect error
mapping, Basic auth), `src/envd/versions.ts` (feature gates), `src/sandbox/commands/*.ts`,
`src/sandbox/filesystem/index.ts`; Python mirrors these in `e2b/connection_config.py`,
`e2b/sandbox_domains.py`, `e2b/sandbox_sync/main.py`, `e2b/envd/client_shared.py`.

## 1. Two planes, three hosts

The SDK talks to two different servers:

1. **Control plane** (`api.<domain>`): REST + JSON, API-key auth. Sandbox CRUD, timeouts,
   pause/resume, list, metrics, templates.
2. **envd** (one per sandbox, port 49983 in the guest), reached through E2B's sandbox proxy:
   Connect RPC (process and filesystem services) plus a few plain HTTP endpoints (`/files`,
   `/health`). Auth is a per-sandbox access token returned by the control plane.

Plus user traffic to sandbox ports: `https://<port>-<sandboxID>.<domain>`, routed by the same
proxy.

### Base URL resolution (`connectionConfig.ts`, `connection_config.py`)

| Setting | Env var | Default |
| --- | --- | --- |
| API key | `E2B_API_KEY` | none; the SDK refuses to call the API without one (`api/index.ts`). Format is no longer validated client-side. |
| Domain | `E2B_DOMAIN` | `e2b.app` |
| API URL | `E2B_API_URL` (marked `@internal` but honoured) | `https://api.${domain}`, or `http://localhost:3000` in debug mode |
| envd URL | `E2B_SANDBOX_URL` (`@internal`, honoured) | see below |
| Debug | `E2B_DEBUG=true` | false. Debug makes `create()` skip the API entirely (fake id `debug_sandbox_id`, envd version `99.99.99`), sends envd traffic to `http://localhost:49983`, and turns kill/setTimeout/metrics into no-ops. Useful only for a single local envd. |
| HTTP version | `E2B_HTTP_VERSION` = `1.1` or `2` | `2` (Node: undici `allowH2`; Python: pyqwest/reqwest). h2 is negotiated by ALPN on TLS only; plain `http://` targets use HTTP/1.1. |
| Retries | `E2B_CONNECTION_RETRIES` (py) / `retries` option | 3 retries on 429 (only with integer `Retry-After`), 502, 503 and connect failures; creates are not retried on 502. |
| User-Agent suffix | `E2B_USER_AGENT_SOURCE` | none |

envd base URL (`getSandboxUrl`, identical logic in both SDKs):

1. `E2B_SANDBOX_URL` if set, **verbatim, for every sandbox**;
2. debug: `http://localhost:49983`;
3. if the sandbox domain is one of `e2b.app`, `e2b.dev`, `e2b.pro`, `e2b-staging.dev`
   (hard-coded list, `supportedDomains` / `SUPPORTED_SANDBOX_DOMAINS`) and not in a browser:
   the shared host `https://sandbox.<domain>`;
4. otherwise `https://49983-<sandboxID>.<domain>` (always **https**).

The *sandbox domain* is the `domain` field of the create/connect response if non-null, else
the configured domain. Hosted E2B returns no `domain` (absent in every trace), so the SDK falls
back to `E2B_DOMAIN`.

Signed upload/download URLs use `getSandboxDirectUrl`: same as above but skipping step 3, so
hosted they are `https://49983-<id>.e2b.app/files?...`; with `E2B_SANDBOX_URL` set they are
`$E2B_SANDBOX_URL/files?...` and carry **no sandbox identifier at all** (see section 6).

`getHost(port)` = `<port>-<sandboxID>.<sandboxDomain>` (no scheme, no port), or
`localhost:<port>` in debug. It ignores `E2B_SANDBOX_URL`.

### Routing on the E2B side (`shared/pkg/proxy/host.go`)

Every envd request the SDK makes carries `E2b-Sandbox-Id: <id>` and `E2b-Sandbox-Port: 49983`.
The proxy honours those headers **only when the Host is `localhost`, an IP literal, or
`sandbox.<domain>`**; otherwise it parses the Host as `<port>-<sandboxID>.<anything>` (left-most
label split on `-`). Sandbox IDs must match `^[a-z0-9]+$`. User traffic to ports may need
header `e2b-traffic-access-token` when the sandbox was created with restricted public traffic
(`network.allowPublicTraffic=false`); not needed by default (`orchestrator/pkg/proxy/proxy.go`).

This header-routing rule is the hook for a local server: run one listener, set
`E2B_SANDBOX_URL=http://127.0.0.1:<port>`, route envd calls on `E2b-Sandbox-Id`, route other
Hosts on `<port>-<id>.`, exactly as E2B's proxy does.

## 2. Control plane REST

Auth: header `X-API-Key: <key>` (spec `securitySchemes.ApiKeyAuth`). Also sent on every call
(JS): `lang`, `lang_version`, `package_version`, `publisher`, `sdk_runtime`, `system`,
`browser`, `User-Agent: e2b-js-sdk/<v>` (Python: `e2b-python-sdk/<v>`). These are
informational; nothing needs to parse them. Bodies are JSON; list query arrays use form style,
`explode: false` (comma-separated).

Error body everywhere: `{"code": <int http status>, "message": "<text>"}` (optional
`error_code` string). The SDK maps by **HTTP status**, not by body (`apiErrorFromCode`):
401 -> `AuthenticationError`, 429 -> `RateLimitError`, 503 -> `ServiceBusyError`, 404 ->
call-specific `SandboxNotFoundError` (or `false` for kill), anything else -> `SandboxError`
with `"<status>: <message>"`. `message` is shown to users.

Endpoints the SDK's `Sandbox` uses (all verified in the traces unless marked *spec only*):

| SDK call | Request | Success | Errors the SDK interprets |
| --- | --- | --- | --- |
| `Sandbox.create(template?, opts)` | `POST /v2/sandboxes` body `NewSandboxV2` | **201** `Sandbox` | any non-2xx -> error; if `envdVersion < 0.1.0` the SDK kills it and throws `TemplateError` |
| `Sandbox.connect(id, {timeoutMs})`, `sbx.connect()` | `POST /v2/sandboxes/{id}/connect` body `{"timeout"?: s, "memory"?: false}` | **200** (was running) / **201** (resumed) `Sandbox` | 404 -> `SandboxNotFoundError("Paused sandbox <id> not found")` |
| `getInfo` / `isRunning`-adjacent | `GET /sandboxes/{id}` | 200 `SandboxDetail` | 404 -> `SandboxNotFoundError` |
| `kill` | `DELETE /sandboxes/{id}` | **204** | 404 -> returns `false` |
| `setTimeout(ms)` | `POST /sandboxes/{id}/timeout` body `{"timeout": seconds}` | 204 | 404 |
| `pause({keepMemory})` / `betaPause` | `POST /sandboxes/{id}/pause` body `{}` or `{"memory": false}` | 204 -> `true` | **409 -> `false`** (already paused), 404 |
| `getMetrics({start,end})` | `GET /sandboxes/{id}/metrics?start=&end=` (unix seconds) | 200 `SandboxMetric[]` (may be `[]` right after boot) | 404 |
| `Sandbox.list({query,limit,nextToken,order})` | `GET /v2/sandboxes?metadata=<urlencoded k=v&...>&state=running,paused&template=&startedAfter=&order=&limit=&nextToken=` | 200 `ListedSandbox[]`; pagination via response header `X-Next-Token` (absent on last page); also sends `X-Total-Running` | |
| `updateNetwork` | `PUT /sandboxes/{id}/network` | 204 | 404, 409 |
| `fork`, `createSnapshot`, `listSnapshots`, `deleteSnapshot` | `POST /sandboxes/{id}/fork`, `POST /sandboxes/{id}/snapshots`, `GET /snapshots`, `DELETE /templates/{id}` | *spec only* | newer features; skip in v1 |

Request/response shapes (spec `components.schemas`, confirmed by traces):

```jsonc
// POST /v2/sandboxes  (js.json#1)
{"templateID": "base", "timeout": 300, "metadata": {"k": "v"}, "envVars": {"K": "V"},
 // optional: "autoPause": bool, "autoPauseMemory": bool, "autoResume": {"enabled": bool},
 // "allow_internet_access": bool, "network": {...}, "mcp": {...}, "iam": {...}, "volumeMounts": [...]
}
// 201 Sandbox
{"alias": "base", "clientID": "6532622b", "envdAccessToken": "<token>", "envdVersion": "0.6.10",
 "sandboxID": "izxxon8hhxd6ssugudjex", "templateID": "rki5dems9wqfm4r03t7g"}
 // spec also allows "trafficAccessToken" (nullable) and "domain" (nullable); hosted omits both

// GET /sandboxes/{id} -> SandboxDetail (py.json#2)
{"alias": "base", "clientID": "6532622b", "cpuCount": 2, "diskSizeMB": 23301, "memoryMB": 512,
 "startedAt": "2026-10-03T00:38:42.322085214Z", "endAt": "2026-10-03T00:43:42.322085214Z",
 "envdAccessToken": "<token>", "envdVersion": "0.6.10",
 "lifecycle": {"autoResume": false, "onTimeout": "kill"}, "metadata": {...},
 "sandboxID": "...", "state": "running", "templateID": "...", "volumeMounts": []}
 // ListedSandbox (GET /v2/sandboxes) is the same minus envdAccessToken and lifecycle

// GET /sandboxes/{id}/metrics (py.json#42)
[{"cpuCount": 2, "cpuUsedPct": 27.35, "diskTotal": 22757814272, "diskUsed": 1652707328,
  "memCache": 73605120, "memTotal": 501293056, "memUsed": 56668160,
  "timestamp": "2026-10-03T00:38:40Z", "timestampUnix": 1790987920}]
```

Observed error bodies: 404 `{"code":404,"message":"Sandbox \"<id>\" doesn't exist or you don't have access to it"}`
(get, delete, connect after kill: py.json#56-59); 409 on second pause
`{"code":409,"message":"Error pausing sandbox - sandbox '<id>' is already paused"}` (py.json#48).

Notes for the implementer:

- `templateID` in the request is a template **name/alias or ID** (default `"base"`, from
  `Sandbox.defaultTemplate`; `"mcp-gateway"` when `mcp` is set). The response's `templateID`
  is the resolved ID and `alias` the name. The SDK only uses `sandboxID`, `envdVersion`,
  `envdAccessToken`, `trafficAccessToken`, `domain` from create/connect.
- `clientID` is deprecated but required by the spec; hosted always returns `"6532622b"`.
- Timeouts are **seconds** on the wire (SDK converts ms). v2 create default 300 s
  (`SandboxTimeoutDefaultV2`; the deprecated v1 `POST /sandboxes` defaults to 15 s). Max is
  per team (1 h hobby, 24 h pro). `setTimeout` sets `endAt = now + timeout` (it can shorten).
- The SDK-side lifecycle option `onTimeout: 'pause'|'kill'` is turned into `autoPause`
  (+ `autoPauseMemory`, `autoResume`) before it reaches the API.
- Sandbox IDs: `"i"` + 20 chars of `[a-z0-9]` (`handlers/sandbox_create.go` `InstanceIDPrefix`,
  `shared/pkg/id.Generate`); observed `in2br7xb7a8ws9bz9t7cy`. Anything matching
  `^[a-z0-9]+$` routes; keep the shape for familiarity.
- Template build API (not for v1): `POST /v3/templates` (name, tags, cpu, mem) -> template +
  build ID; `GET /templates/{id}/files/{hash}` -> presigned upload URL for each layer's tar
  (the SDK PUTs to it); `POST /v2/templates/{id}/builds/{buildID}` with the step list
  (`TemplateBuildStartV2`: from-image, run, copy, env, start/ready cmd); poll
  `GET /templates/{id}/builds/{buildID}/status?logsOffset=`; aliases via
  `GET /templates/aliases/{alias}` (404 free, 403 taken), tags `POST/DELETE /templates/tags`.
  A wisp front-end could later map "from image" templates onto wisp's container-image disks.

## 3. envd

### Transport

- Base URL as in section 1; hosted: `https://sandbox.e2b.app` + routing headers.
- Every envd request carries `E2b-Sandbox-Id`, `E2b-Sandbox-Port: 49983` and
  `X-Access-Token: <envdAccessToken>` (when the API returned one; it always does for v2
  creates). envd rejects other paths with 401
  `{"code":401,"message":"unauthorized access, please provide a valid access token or method signing if supported"}`
  once a token is set; `GET /health`, `GET /files`, `POST /files`, `POST /init` are exempt
  from the header check (`/files` then requires a valid header token *or* signature).
- User selection: `Authorization: Basic base64("<user>:")` (empty password), sent only when the
  caller passes `user` (or always `user` for envd < 0.4.0). No header -> envd's default user,
  set by `/init` (`defaultUser`); hosted that is `user` with `cwd=/home/user` (traces:
  `whoami` -> `user`). Relative paths resolve against the user's home.
- **Connect protocol, JSON codec**, not gRPC and not binary protobuf: JS sets
  `useBinaryFormat: false`; Python installs a custom `_ProtoJSONCodec` "matching the JS SDK"
  (`envd/client_shared.py`). Paths are `POST /<package>.<Service>/<Method>`.
  - Unary: `Content-Type: application/json`, `Connect-Protocol-Version: 1`,
    `Connect-Timeout-Ms: 60000`; body is the proto3-JSON message; response JSON. Errors are
    Connect errors: HTTP status from the Connect code mapping, body
    `{"code":"not_found","message":"..."}`. Observed: `not_found` -> 404, `already_exists`
    -> 409 (py.json#10, #22, #27).
  - Server-streaming (`Start`, `Connect`, `WatchDir`): `Content-Type: application/connect+json`,
    request and response bodies are envelopes `[flags u8][len u32 BE][json]`. The last response
    envelope has flag `0x02` (end-stream) and body `{}` or `{"error":{"code","message"}}`.
    Over HTTP/1.1 the response is chunked; it must be flushed per message (the SDK's
    `onStdout` fires as data arrives; traces show frames 0.4 s apart).
  - Proto3 JSON rules matter: `int64` fields are **strings** (`"size": "10"`), enums are names
    (`"FILE_TYPE_FILE"`, `"SIGNAL_SIGKILL"`), `bytes` are base64, zero values are omitted
    (`EndEvent` has no `exitCode` when it is 0 and no `exited` when false), timestamps RFC 3339.
- Extra headers: `Keepalive-Ping-Interval: 50` on streams (envd then emits a `keepalive`
  event every 50 s; default 90 s). `Connect-Timeout-Ms` on `Start` becomes **the process
  lifetime**: envd runs the process under `context.WithTimeout(Background, timeout)`, so the
  SDK's default 60 s `timeoutMs` kills long commands; a dropped client does not kill the process
  (`envd/internal/services/process/start.go`).
- Python's envd User-Agent is `connectrpc/0.11.1` (not the SDK UA); JS sends `e2b-js-sdk/<v>`.
- Responses may be gzip (`Connect-Accept-Encoding: gzip`; `/files` GET honoured
  `Accept-Encoding: gzip` in the traces). Supporting identity only is fine.

### Process service (`process.proto`, package `process`)

| RPC | SDK use | Notes from traces |
| --- | --- | --- |
| `Start(StartRequest) returns (stream StartResponse)` | `commands.run` (foreground and `background: true`), `pty.create` | Commands are always `{"cmd":"/bin/bash","args":["-l","-c","<cmd>"],"envs"?,"cwd"?}` + `"stdin": false` (or true). PTY: `args ["-i","-l"]`, envs `TERM=xterm-256color, LANG/LC_ALL=C.UTF-8`, `"pty":{"size":{"cols":80,"rows":24}}`. Events: `{"event":{"start":{"pid":N}}}`, `{"event":{"data":{"stdout"|"stderr"|"pty": b64}}}`, `{"event":{"end":{"exitCode":7,"exited":true,"status":"exit status 7","error":"exit status 7"}}}`; killed: `{"end":{"exitCode":-1,"status":"signal: killed","error":"signal: killed"}}`. The SDK raises `CommandExitError` on non-zero exit. |
| `Connect(ConnectRequest{process:{pid\|tag}})` stream | `commands.connect(pid)`, `pty.connect` | Re-attaches: first event is `start` with the same pid, then only output produced *after* attaching (py.json#13 saw only `line-2`). |
| `List` | `commands.list()` | `{"processes":[{"config":{"cmd","args","envs"?,"cwd"?},"pid":N,"tag"?}]}`; `{}` when none. Lists processes started through envd only. |
| `SendInput{process, input:{stdin\|pty: b64}}` | `commands.sendStdin`, `pty.sendInput` | |
| `CloseStdin{process}` | `commands.closeStdin` (SDK refuses below envd 0.5.2, `ENVD_ENVD_CLOSE`) | EOF to a non-PTY process |
| `SendSignal{process, signal}` | `commands.kill(pid)` -> `SIGNAL_SIGKILL`; 404 `not_found` -> `kill` returns `false` | |
| `Update{process, pty:{size}}` | `pty.resize` | |
| `StreamInput` (client stream) | not used by either SDK | |

When a sandbox is paused, open `Start` streams end with
`{"error":{"code":"unavailable","message":"the connection to sandbox <id> ended before the stream completed"}}`
(py.json#45); the processes themselves survive a memory snapshot and are listed after resume
(py.json#52).

### Filesystem service (`filesystem.proto`, package `filesystem`)

`Stat`, `MakeDir`, `Move`, `ListDir{path, depth}`, `Remove`, `WatchDir` (stream),
`CreateWatcher` / `GetWatcherEvents` / `RemoveWatcher` (polling watcher). `EntryInfo` =
`{name, type, path, size(int64 string), mode, permissions ("-rw-r--r--"), owner, group,
modifiedTime, symlinkTarget?, metadata?}`. SDK mappings: `files.exists` = `Stat` with 404 ->
false; `makeDir` returns false on 409 `already_exists`; `list(path,{depth})` = `ListDir` (depth 1
default, recursive entries flattened); `rename` = `Move`; `getInfo` = `Stat`; `watchDir` =
`WatchDir` stream (`recursive` needs envd >= 0.1.4, `include_entry` >= 0.6.3, network mounts
>= 0.6.4).

### Plain HTTP endpoints (`envd.yaml`)

- `GET /health` -> 204. `isRunning()` treats **502 as not running**; Connect errors with
  "terminated"-style messages also trigger a health probe to decide between `TimeoutError`
  and a network error.
- `GET /files?path=&username=&signature=&signature_expiration=` -> raw bytes,
  `Content-Disposition: inline; filename=...`; 404 JSON `{"code":404,"message":"path '...' does not exist"}`
  (envd's REST errors use **integer** `code`, unlike Connect's string codes); 400 for a
  directory, 401 bad user, 406 unsupported encoding.
- `POST /files?path=&username=` upload. Two encodings:
  - `multipart/form-data` with field `file` (the SDKs' default for strings/bytes; filename is
    the destination path; several parts allowed for multi-file `write`);
  - `application/octet-stream` raw body (envd >= 0.5.7; used for streams, gzip
    `Content-Encoding`, or `useOctetStream`); JS sends it chunked.
  Response 200 `[{"name","path","type":"file"}]` with `Content-Type: text/plain` (sic). Parent
  dirs are created. `X-Metadata-<key>` headers become xattrs (envd >= 0.6.2). 507 when disk is
  full (`NotEnoughSpaceError`).
- `POST /files/compose`, `GET /envs`, `GET /metrics`: exist, unused by the core SDK paths.
- Internal, never through the public proxy: `POST /init` (orchestrator hands envd its access
  token, env vars, default user/workdir, time, volume mounts, CA bundle), `/freeze`,
  `/unfreeze`, `/fsfreeze`, `/fsthaw`, `/collapse`, `/upgrade`.

Signed URLs (`sandbox/signature.ts`, envd `internal/api/auth.go`): `uploadUrl(path)` /
`downloadUrl(path)` return `<directUrl>/files?path=..&username=..&signature=v1_<sig>[&signature_expiration=<unix>]`
where `sig = base64(sha256("<path>:<read|write>:<user or ''>:<envdAccessToken>[:<exp>]"))`
with trailing `=` stripped. envd recomputes it from its own token. Probes fetched both with no
SDK headers (py.json#35-36).

### envd version gates in the SDKs (`envd/versions.ts`, `e2b/envd/versions.py`)

| envd version | Feature |
| --- | --- |
| < 0.1.0 | create refuses the template |
| 0.1.4 | recursive `watchDir` |
| 0.3.0 | `stdin: false` allowed on `Start` |
| 0.4.0 | default user comes from envd (no Basic header unless asked); before, SDK always sends `user` |
| 0.5.2 | `CloseStdin` |
| 0.5.7 | octet-stream upload |
| 0.6.2 | file metadata headers |
| 0.6.3 | `include_entry` in watch events |
| 0.6.4 | watching network mounts |

So the version string reported by the API in `envdVersion` controls client behaviour: report the
real version of whatever answers in the guest, and it must be >= 0.6.4 to unlock everything.

## 4. Lifecycle semantics

- **create** blocks until envd answers (hosted ~0.3-0.6 s for `base`), returns 201.
- **timeout**: sandbox dies (or auto-pauses if `autoPause`) at `endAt`. `setTimeout` resets
  `endAt = now + t`. `connect` on a **running** sandbox only extends (spec: "TTL is only
  extended"), default 300 s when the body omits `timeout`.
- **pause** (`POST .../pause`, 204): memory snapshot by default (`{"memory": false}` =
  filesystem-only, resume reboots). State becomes `paused`; `endAt` is set to the pause time
  (py.json#47). A second pause is 409, which the SDK reports as `false`.
- **resume = connect**: `POST /v2/sandboxes/{id}/connect` on a paused sandbox resumes it
  (201). After resume, `startedAt` is reset to the resume time and `endAt = now + timeout`
  (300 s default, *not* the previously set timeout: py.json#50 vs #41). Files and processes
  survived (memory snapshot). The deprecated `POST /sandboxes/{id}/resume` is not used.
- **auto-resume**: with `lifecycle: {onTimeout: 'pause', autoResume: true}`, traffic through
  the proxy to a paused sandbox resumes it. Optional for v1.
- **kill**: 204, then everything 404s (get, kill -> SDK `false`, connect).
- **list** returns running and paused by default; filter by `state`, `metadata`, `template`.
- Sandbox user is `user` (uid 1000, home `/home/user`), hostname `e2b`, `python3` present in
  `base`; port traffic arrives from a proxy IP (`10.12.0.x`), so servers must listen on
  `0.0.0.0` - or rely on envd's forwarder, which re-binds 127.0.0.1-only listeners to eth0 with
  socat (`envd/internal/port/`).

## 5. Ports and host routing

`getHost(8080)` -> `8080-<id>.e2b.app`; the probes fetched `https://8080-<id>.e2b.app/` and
got the in-sandbox `python3 -m http.server` response (py.json#39). For a local server:

- the SDK builds `<port>-<id>.<sandboxDomain>`. The front-end can set `domain` in the
  create/connect response (the SDK prefers it over `E2B_DOMAIN`), e.g. `e2b.localhost:7820`, so
  `getHost` yields `8080-<id>.e2b.localhost:7820` - a host:port the user can put after
  `http://`. (Derived from `getHost` code; not yet exercised.) `*.localhost` resolution depends
  on the client resolver (systemd-resolved and browsers resolve it; plain glibc may not).
- The SDK never makes port requests itself, so scheme and port are up to the caller; the probe
  takes `E2B_PROBE_PORT_SCHEME`.

## 6. Pointing the SDKs at another server

What works with unmodified SDKs (all verified by the recording run, which redirected both SDKs
to a local proxy):

```sh
E2B_API_KEY=<anything the server accepts>
E2B_API_URL=http://127.0.0.1:7820        # control plane
E2B_SANDBOX_URL=http://127.0.0.1:7820    # envd, routed on E2b-Sandbox-Id
E2B_DOMAIN=e2b.localhost                 # only affects getHost() when the API returns no domain
```

Caveats an implementer must design around:

1. **Without `E2B_SANDBOX_URL`, a custom domain forces `https://49983-<id>.<domain>`**, i.e.
   wildcard DNS plus a TLS certificate the client trusts (`NODE_EXTRA_CA_CERTS`; pyqwest uses
   the system store). Plain HTTP is only reachable through `E2B_SANDBOX_URL` (or `E2B_DEBUG`,
   which is single-sandbox).
2. **Signed URLs under `E2B_SANDBOX_URL` carry no sandbox id** (`$E2B_SANDBOX_URL/files?path=..&signature=..`).
   A server can still route them: compute the signature against each live sandbox's token and
   pick the match (the signature covers path, operation, user and token). Unsigned direct URLs
   (no expiration option and no signature) cannot be routed. The recorder in this repo routes
   them to the last sandbox it saw instead.
3. `E2B_SANDBOX_URL` is marked `@internal` in both SDKs; it has been stable since 1.x but is
   not a documented contract.

Recording limitations: none of the SDK's own calls escaped the proxy. Port traffic is not an
SDK call; the probes send it through the recorder with an explicit `Host` header
(`E2B_PROBE_PORT_VIA`), so it is recorded but not "as the SDK would". HTTP/2 framing is not
recorded (the recorder forces `E2B_HTTP_VERSION=1.1`; the semantic content is identical).

## 7. envd: run it in the guest, or reimplement its protocol?

**Recommendation: run the real envd in wisp guests, as a service supervised by wisp-agent, and
have the E2B front-end proxy envd traffic to guest port 49983 over the existing `DialPort`
primitive.** Reimplementing is the fallback only if a blocker below turns out real.

Why:

- **License and build**: Apache-2.0, a single static Go binary
  (`packages/envd`, `go build`), no E2B-side services required.
- **It runs outside Firecracker-on-E2B by design**: flag `-isnotfc` skips the MMDS poll and the
  log exporter (that is how `make start-docker` and `E2B_DEBUG` work). Other flags: `-port`
  (49983), `-no-cgroups` (no-op cgroup manager), `-cgroup-root`, `-verbose`.
- **Token injection is one HTTP call**: `POST /init` with
  `{"accessToken","envVars","defaultUser","defaultWorkdir","timestamp"}` (all optional). With
  `-isnotfc` the first `/init` is accepted unconditionally ("first-time setup"); later ones must
  carry the same token (`internal/api/init.go`), which suits wisp: the host calls `/init` right
  after boot and after each restore, before marking the sandbox routable. (MMDS-based token
  rotation is Firecracker-only and unnecessary.) Wisp already boots Firecracker, so MMDS could
  even be wired later if token rotation on resume is wanted.
- **Protocol surface is large and moving**: nine version gates in two years, proto3-JSON quirks
  (int64-as-string, omitted zero values, base64 bytes), Connect envelopes and error codes,
  keepalives, `Connect-Timeout-Ms` as process lifetime, `Connect` re-attach semantics, PTY,
  signed URLs, xattr metadata, gzip, multipart and octet-stream uploads, polling and streaming
  watchers. Reimplementing all of that in wisp-agent is a long conformance tail; running envd
  gets byte-exact behaviour for free and lets the API report envd's true version.

What envd needs in the guest (and wisp's gaps):

| Need | wisp today | Action |
| --- | --- | --- |
| A supervisor (envd assumes systemd/OpenRC with `Restart=always`) | wisp-agent is PID 1 and already supervises services | add envd as an agent-managed service (`envd -isnotfc -port 49983`, plus `-no-cgroups` if the guest has no delegated cgroup2) |
| User `user` (uid 1000), home `/home/user`, `/bin/bash`, `sudo` for root parity | base image has user `sprite` | an E2B-flavoured base image (or `/init` `defaultUser`) with `user`; SDKs and users assume `/home/user` |
| `/init` from the host after boot and after every restore | none | front-end calls it through `DialPort(49983)` before reporting the sandbox running |
| Network reachability of 49983 and user ports | `DialPort` dials guest ports over vsock | no eth0 needed for envd itself; envd's socat forwarder is irrelevant if `DialPort` reaches 127.0.0.1 in the guest (verify) |
| `socat` (port forwarder), cgroup2 | not in image | optional; forwarder only matters for loopback-only servers reached via eth0 |
| Pause hooks (`/freeze`, `/fsfreeze`) | wisp snapshots whole VMs | not required; optional quality improvement |

Risks: envd's process supervision overlaps wisp-agent's (two in-guest agents; both reap
children, so envd must not run as PID 1 and wisp-agent must not reap envd's children - envd is
a normal child, so this holds). envd's own idle timeout is 640 s per connection, fine behind a
proxy. envd HEAD (0.9.0) includes live self-upgrade and handover code paths we never trigger.

## 8. Minimum viable surface for v1, in order

1. `POST /v2/sandboxes` (template `base` only, `timeout`, `metadata`, `envVars`) returning
   `sandboxID`, `templateID`, `alias`, `clientID`, `envdVersion`, `envdAccessToken`;
   `X-API-Key` auth with the `{"code","message"}` error body.
2. envd in the guest + `/init`; envd routing on `E2b-Sandbox-Id`/`E2b-Sandbox-Port` (and
   `<port>-<id>.` Hosts); `X-Access-Token` passthrough. This alone unlocks `commands.*`,
   `files.*`, `pty.*` with streaming exactly as hosted.
3. `DELETE /sandboxes/{id}` (204/404), `GET /sandboxes/{id}` (SandboxDetail, 404),
   `POST /sandboxes/{id}/timeout` + an expiry reaper honouring `endAt`.
4. `GET /v2/sandboxes` with `metadata`/`state` filters, `X-Next-Token` paging.
5. Port routing for user traffic (`<port>-<id>.<domain>`), plus `domain` in responses so
   `getHost` points at us.
6. `POST /sandboxes/{id}/pause` (204, 409 when paused) and `POST /v2/sandboxes/{id}/connect`
   (200 running / 201 resumed, timeout reset, 404) on top of wisp's suspend/restore; envd
   `/init` after restore.
7. `GET /sandboxes/{id}/metrics` (may return `[]`), signed-URL routing under
   `E2B_SANDBOX_URL` (section 6).
8. Later: `autoPause`/`autoResume` lifecycle, `PUT .../network` (wisp netpolicy),
   templates from container images, snapshots/fork (wisp checkpoints), volumes, secrets.

Gate for phase 2: `e2e/providers/e2b/run.sh` passes against the local server with
`E2B_API_URL`/`E2B_SANDBOX_URL` set, and a `--record` run differs from `golden/` only in IDs,
timestamps, hosts, and differences listed in `docs/providers/e2b-differences.md`.
