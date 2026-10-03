# Modal spike: other SDKs, the quality bar, version tracking, licensing

Research date 2026-10-03. Read-only: nothing in the wisp repo was changed. Scratch material (the
modal-client checkout at `go/v0.11.0`, compiled descriptor sets, the comparison scripts) is in
`../protocmp/` and `../mc/` next to this file.

## Summary

1. **libmodal is archived.** The JS and Go SDKs moved into the monorepo
   **github.com/modal-labs/modal-client** (Apache-2.0), which now holds `py/`, `js/`, `go/` and
   **`modal_proto/*.proto`**. The current releases are **npm `modal@0.11.0`** and
   **`github.com/modal-labs/modal-client/go@v0.11.0`**. Both were released 2026-09-29, 38 minutes
   after Python 1.6.0.
2. **Real .proto source exists for 1.6.0.** Tag **`py/v1.6.0`** (commit `a3f75167a9`) exists. The
   phase-5 note missed it because it looked for `v1.6.0`; tags since 1.3.5 carry a `py/` prefix.
   I compiled `modal_proto/api.proto` and `task_command_router.proto` from that tag with protoc and
   compared them with the 1.6.0 wheel's embedded descriptors. **They are byte-identical** once
   `json_name` and source info are stripped: 575 messages and 254 methods, plus 47 messages and
   23 methods. The `.proto` files at `go/v0.11.0` and `js/v0.11.0` are identical to `py/v1.6.0`. So
   **Python 1.6.0, JS 0.11.0 and Go 0.11.0 speak exactly the same protocol.**
3. **JS and Go would work against sandboxd with two gaps.**
   - **(a) Router URL scheme on localhost.** JS and Go *require* an `https://` router URL; against
     localhost they then dial it in **plaintext**. Python takes `http://` as plaintext but treats
     `https://` as TLS, so no single URL works for both on localhost. The fix is to choose the
     scheme per `x-modal-client-type`.
   - **(b) Images.** JS and Go have no `debian_slim()`. Every image is `FROM <registry tag>`, so
     they need `Image.from_registry` support, which wisp's existing OCI pull and flatten cache
     makes feasible.

   Their default examples also use the sandbox's own stdin and stdout, which wisp has not built.
4. **The quality bar.** The other providers have:
   - probe suites per SDK (py and js), run against the hosted provider;
   - **golden traces recorded through a logging reverse proxy**;
   - the same suite rerun against sandboxd with `--record` to compare;
   - a `*-differences.md` "seen in the traces" table;
   - acceptance from off-network through the public TLS path (H11, `try.py`).

   Modal has only a Python probe (target plus 14 checks), **no hosted recording**, and a
   differences doc that is "from the client's source, not a trace comparison". It is not in H11 or
   `try.py`.
5. **Recording hosted Modal is cheap and feasible.**
   - Cost: the Starter plan is free with $30/month in credits. Sandboxes cost $0.0000394 per
     core-second, so a full probe run costs cents.
   - Mechanism: a gRPC proxy speaks h2c on localhost (`MODAL_SERVER_URL=http://127.0.0.1:N`) and
     TLS upstream to api.modal.com. It must rewrite `command_router_access.url` in three response
     types and route router calls back by JWT.
6. **Release cadence.** Python stable releases came every 2 to 6 weeks in 2026: 1.4.0 on 03-25,
   then 1.4.1, 1.4.2, 1.4.3, 1.5.0 on 06-09, 1.5.1 to 1.5.5, and 1.6.0 on 09-29. Nightly `.devN`
   builds come out daily, and JS/Go follow on npm as `-dev.N`. Drift is real and fast. Each minor
   adds 14 to 39 RPCs and changes 25 to 55 types. **Four days after 1.6.0, `main` already changed
   `SandboxCreateV2Request`/`Response` and `TaskExecStartRequest`**, adding `secret_sources` and a
   new `sandbox_token` that is "sent as the x-modal-sandbox-token metadata header".
7. **Recommendation.** Support **Python 1.6.0 (primary), then Go 0.11.0 and JS 0.11.0**, all
   protocol-identical. Switch codegen to the tagged `.proto` source, and keep `check.py` as a
   guard. Use the Go SDK as an in-repo Go test client. Record goldens against hosted Modal for
   Python (and JS) with a gRPC recording proxy.

---

## 1. Modal's non-Python SDKs

### Where they live and which versions exist

| | Source | Latest stable | Dev channel | Notes |
|---|---|---|---|---|
| libmodal (old home) | github.com/modal-labs/libmodal | `modal-js/v0.7.2`, `modal-go` v0.7.3 on the Go proxy | — | **Archived.** Its README says: "The JS and Go SDKs has migrated to https://github.com/modal-labs/modal-client … Go SDK releases will now be at …/modal-client/go". Old Go import `github.com/modal-labs/libmodal/modal-go` |
| JS/TS | `modal-client/js`, npm **`modal`** | **0.11.0** (2026-09-29T01:59Z) | `next` = 0.11.1-dev.3 (daily) | npm dist-tag `latest` = 0.11.0. The npm `modal@1.0.0`–`1.2.0` versions are an unrelated 2015 package under the same name, so do not pin `^1`. Deps: nice-grpc, protobufjs, cbor-x, smol-toml, uuid, long. Codegen is ts-proto from `../modal_proto` at `prepare` |
| Go | `modal-client/go`, module **`github.com/modal-labs/modal-client/go`** | **v0.11.0** (proxy list: …v0.10.0, v0.10.1, v0.11.0) | — | `go 1.25.0`, grpc 1.83.2, protobuf 1.36.11. Generated pb is in `go/proto/modal_proto` (package `pb`), generated with **`default_api_level=API_OPAQUE`** (builders and getters, not open structs), 2.8 MB |
| Python | `modal-client/py`, PyPI `modal` | **1.6.0** (2026-09-29T01:17Z) | 1.6.1.dev0–dev3 (daily) | Tags `py/vX.Y.Z`. Before 1.3.5 they were `vX.Y.Z` |

Tags: `py/v1.6.0` = `a3f75167a9` ("Release v1.6.0 of the Python SDK (#62839)", 2026-09-29T01:15Z).
`go/v0.11.0` and `js/v0.11.0` = `8f5ea792ad` ("Release 0.11.0 of the JS and Go SDKs (#62889)",
01:53Z). JS and Go are versioned in lockstep (`go/version.go`: "Keep this in sync … js/package.json").

### Do they share the protos, and at which commit do they match 1.6.0?

Yes. All three are generated from the monorepo's `modal_proto/`. Verification (scripts in
`../protocmp/`):

- At `py/v1.6.0`, `api.proto` is 160,016 bytes and `task_command_router.proto` is 16,073 bytes.
- They are identical to the `go/v0.11.0` and `js/v0.11.0` copies (`cmp`).
- `protoc --include_imports -o` on them, compared with `modal_proto.api_pb2.DESCRIPTOR` from the
  installed 1.6.0 wheel, is **IDENTICAL** for both files: 575 messages and 254 methods, and 47
  messages and 23 methods.
- `main` (2026-10-03) differs already. The diff is in `../protocmp/`; see section 3.

So the 1.6.0-equivalent JS/Go releases are **0.11.0**. Each Python release has a JS/Go release cut
minutes later from almost the same commit, but nothing guarantees identity, so check each pair
(same diff script).

### Configuration and transport (from the source at go/v0.11.0)

| | Python 1.6.0 | Go 0.11.0 | JS 0.11.0 |
|---|---|---|---|
| Server URL | `MODAL_SERVER_URL`, `~/.modal.toml` `server_url`; default `https://api.modal.com,https://api.modal2.com` | `MODAL_SERVER_URL` > profile > `https://api.modal.com:443` (`go/config.go`) | same (`js/src/config.ts:141`) |
| Tokens | `MODAL_TOKEN_ID`/`MODAL_TOKEN_SECRET` (both required client-side) | same; also `MODAL_OAUTH_*` | same |
| Other env | `MODAL_CONFIG_PATH`, `MODAL_PROFILE`, `MODAL_ENVIRONMENT`, `MODAL_SANDBOX_V2` | the same plus `MODAL_IMAGE_BUILDER_VERSION` (skips the EnvironmentGetOrCreate lookup), `MODAL_MAX_THROTTLE_WAIT`, `MODAL_SANDBOX_CHANNEL_IDLE_TIMEOUT`, `MODAL_LOGLEVEL` | same as Go |
| Control plane plaintext | `http://` = h2c, to any host | `http://` = `insecure.NewCredentials()`, any host (`client.go:370-377`) | `createChannel("http://…")` = insecure, any host |
| **Router URL** | `http://` allowed **only** if the server host is localhost/127.0.0.1/::1/172.21.0.1 (plaintext). `https://` = TLS, with verification off on localhost | **must be `https://`**, otherwise "task router URL must be https". On localhost it then dials with **`insecure.NewCredentials()`, i.e. plaintext h2c**, not TLS (`task_command_router_client.go:253-271`) | same as Go (`task_command_router_client.ts:337-364`, `ChannelCredentials.createInsecure()`) |
| Headers | `x-modal-client-type: 1`, `x-modal-client-version: 1.6.0` | `x-modal-client-type: 9` (LIBMODAL_GO), **`x-modal-client-version: 1.0.0`** ("Behaves like this Python SDK version"), `x-modal-libmodal-version: modal-go/0.11.0` | type `8` (LIBMODAL_JS), version `1.0.0`, `x-modal-libmodal-version: modal-js/0.11.0` |
| V2 sandboxes | default (`MODAL_SANDBOX_V2`) | default true | default true |
| `x-modal-warning` | trailer, printed | trailer, logged (`server_warnings.go`) | same (`server_warnings.ts`) |

**Consequence for wisp on localhost.** wisp tells every client `http://127.0.0.1:<port>` today,
which the Go and JS SDKs refuse. Two fixes:

- **(Recommended)** Choose the router URL scheme from the `x-modal-client-type` of the RPC that
  hands it out (`SandboxCreateV2`, `SandboxGetCommandRouterAccess`,
  `TaskGetCommandRouterAccess`). Type 1 gets `http://host:port`; types 7, 8 and 9 get
  `https://host:port` on the same plaintext listener, since libmodal dials it insecurely.
- Alternatively, sniff TLS against the HTTP/2 preface on the router port and serve both.

Behind a real TLS front (`https://modal.sandbox.inevitable.fyi`) all three clients do ordinary
verified TLS, and one `https` router URL serves them all.

### Sandbox features they cover, and the RPCs they call (Go 0.11.0, by grep; JS mirrors it)

- **ModalClient:**
  - setup: `AppGetOrCreate`, `EnvironmentGetOrCreate`, `AuthTokenGet`;
  - images: `ImageGetOrCreate`, `ImageJoinStreaming`, `ImageFromId`, `ImageGetByTag`,
    `ImagePublish`, `ImageDelete`;
  - sandbox lifecycle: `SandboxCreate[V2]`, `SandboxGetTaskId[V2]`, `SandboxWait[V2]`,
    `SandboxTerminate[V2]`;
  - sandbox metadata: `SandboxList[V2]`, `SandboxGetFromName[V2]`, `SandboxSetName`,
    `SandboxTagsGet/Set[V2]`, `SandboxGetTunnels[V2]`;
  - sandbox stdio and logs: `SandboxStdinWrite`, `SandboxGetLogs`;
  - snapshots: `SandboxSnapshot*`, `SandboxGetExitSnapshot[V2]`, `SandboxRestore[V2]`;
  - extras: `SandboxCreateConnectToken[V2]`, `SandboxContainerCreateV2` (sidecars), `ProxyGet`;
  - other objects: `SecretGetOrCreate`, `VolumeGetOrCreate`, `QueueGetOrCreate`, …, and the
    Function and Cls RPCs.
- **TaskCommandRouter:**
  - exec: `TaskExecStart`, `TaskExecStdioRead`, `TaskExecWait`, `TaskExecStdinWrite[Stream]`,
    `TaskExecStdinStatus`;
  - directories and snapshots: `TaskMount/UnmountDirectory`, `TaskSnapshot{Directory,Filesystem,Memory}`;
  - containers: `TaskContainer{Create,Get,List,Wait,Terminate}`;
  - settings: `TaskSetNetworkAccess`, `TaskSetOutboundPolicy`, `TaskReloadVolumes`;
  - access: `TaskGetCommandRouterAccess`.
- **Images:**
  - Only `Images.FromRegistry(tag)`, `FromAwsEcr`, `FromGcpArtifactRegistry`, `FromID`,
    `FromName` and `.DockerfileCommands([...])`.
  - **There is no `debian_slim`.** `ImageGetOrCreate` carries
    `dockerfile_commands = ["FROM <tag>", ...]` (`go/image.go:400-415`).
  - The examples use `alpine:3.21`, `python:3.13-slim` and `python:3.12-alpine`.
- **Filesystem:** all three SDKs implement `sb.filesystem` / `sandbox_fs` by **exec'ing a
  Modal-supplied binary in the guest, `/__modal/.bin/modal-sandbox-fs-tools`**, with a JSON
  command (`go/sandbox_fs.go:18,201-359`; `py modal/sandbox_fs.py:39`). Building fs support means
  shipping a compatible tool at that path. The command JSON is in the SDK source, and a hosted
  recording would capture real request and response pairs.
- **Canonical examples:** `go/examples/*` and `js/examples/*`, about 34 each (sandbox, sandbox-exec,
  sandbox-poll, sandbox-named, sandbox-tunnels, sandbox-fs, sandbox-secrets, …). They are a
  ready-made probe list.

### Would they work against a server built from 1.6.0's protos?

**On the wire, yes.** The protos are identical. Against today's sandboxd, the Go and JS
`sandbox-exec` flow would fail on:

1. the router URL scheme (above);
2. `ImageGetOrCreate` with `FROM alpine:3.21` / `FROM python:3.13-slim`, which wisp answers
   `FAILED_PRECONDITION` (debian_slim only);
3. `sandbox.go` examples that use `sb.Stdin`/`sb.Stdout`, the entrypoint's stdio, which wisp has
   not built.

`x-modal-client-version: 1.0.0` is harmless today because wisp checks no version. **A
version gate must not reject it** (see section 3).

### Can libmodal or modal-client replace the descriptor-dump approach?

Yes, in one of three ways:

- **A. (Recommended)** `gen.sh` fetches `modal_proto/{api,task_command_router}.proto` from
  `modal-labs/modal-client` at **pinned commit `a3f75167a9` (tag `py/v1.6.0`)**, then prunes and
  compiles.
  - **Keep `check.py`** against the wheel as the guard that tag and wheel agree. They do today.
  - Gains: the real comments, option and field order, and reviewable diffs between releases
    (`git diff py/v1.6.0 py/v1.7.0 -- modal_proto`).
  - The pruner then works on `.proto` source, or on the protoc descriptor set of it, instead of
    on Python modules. It no longer needs the wheel in the codegen step, only in the check.
- **B.** Import `github.com/modal-labs/modal-client/go/proto/modal_proto` (package `pb`) directly
  as a Go dependency at v0.11.0. It already includes `ModalClientServer` and
  `TaskCommandRouterServer` interfaces with `Unimplemented*Server`. Costs:
  - it is the **opaque API**, so wisp's handlers must move to builders and getters;
  - it adds 2.8 MB of generated code and the SDK module's go.mod to the build graph;
  - wisp's version then moves only when Modal tags a Go release.
  This is simplest for tracking, heavier for binary size and code churn.
- **C.** Keep the descriptor dump (today). It works, but has no comments and no diffable history.

Whichever is chosen, `LICENSE` (Apache-2.0) stays alongside. The modal-client repo's LICENSE is
the same as the wheel's.

### Recommendation on SDK support

| SDK | Support? | Why |
|---|---|---|
| Python `modal==1.6.0` | **Yes, primary** (exists) | most users; the reference |
| Go `modal-client/go@v0.11.0` | **Yes, second.** Mainly as **wisp's own in-repo test client**: a Go e2e test (build tag `e2e`) can drive sandboxd in-process with the official SDK. It is the cheapest way to get a typed, CI-able SDK suite, and it matches wisp's Go stack | same protocol; needs the router-scheme fix and `from_registry` |
| JS/TS npm `modal@0.11.0` | **Yes, third**, with a `probe.mjs` like E2B, Vercel and Daytona have | parity with the other providers' py+js suites; same two prerequisites |
| libmodal (`modal-go`, `modal-js` < 0.7.3) | No | archived; older protocol (`CLIENT_TYPE_LIBMODAL=7`) |
| Community Ruby (`anthonycorletti/modal-rb`) | No | unofficial |

Pin JS and Go to the release that matches the Python pin (0.11.0 ↔ 1.6.0) and move all three
together.

---

## 2. The quality bar, and the Modal equivalent

### How E2B, Vercel and Daytona were validated

From `docs/plans/multi-provider-burndown.md`, `e2e/providers/*` and `docs/providers/*-differences.md`:

1. **Survey doc** per provider (`docs/providers/<p>.md`), with SDK versions and the files read.
2. **Probe suites per official SDK, Python and JS**, in `e2e/providers/<p>/probe.py` and
   `probe.mjs`, with pinned `requirements.txt` and `package.json`. `run.sh` installs them into a
   cached venv and `node_modules`.
   - E2B covers 13 steps per SDK, 26 in total: create, exec_stream, exit codes,
     background_kill, stdin_pty, files, upload_download, port, set_timeout, metrics, list,
     pause_resume, kill.
   - Plus `sweep.py`, which counts sandboxes leaked on the account.
   - The exit status is the failure count.
3. **Run against the hosted provider first.** E2B py/js 26/26, Vercel JS 16/16 and py 11/11.
4. **Golden traces recorded through a logging reverse proxy against hosted:**
   - `e2b/recorder.py`, about HTTP/1.1. It routes control-plane and envd traffic, splits Connect
     envelopes into timed frames, and scrubs secrets (auth headers, `*Token` fields, signatures,
     every token value seen).
   - `vercel/record_proxy.py` writes one JSON file per exchange: `NNN-METHOD-path.json`, with
     NDJSON chunks and their ms offsets.
   - Outputs: `e2b/golden/{py,js}.json` plus `results.txt`, and `vercel/golden/{js,py}/`.
   - Daytona has no golden traces (AGPL: spec and SDK behaviour only, no source read).
5. **The same suite runs against sandboxd unmodified.** `--record` with
   `E2B_RECORD_UPSTREAM=http://127.0.0.1:…` records sandboxd's side for a diff against hosted.
6. **A differences doc** (`e2b-differences.md`) whose first section is **"Seen in the traces"**: a
   hosted-against-wisp table of field values, headers and statuses. "Not in the traces" covers
   the rest.
7. **Phase gate:** "the probe suite passes locally and matches the golden traces apart from the
   differences recorded".
8. **Final gate:** every API on one sandboxd, all SDK suites run concurrently, 0 errors in the
   daemon log (`~/ws/gate-all.sh`).
9. **Public path (home-cloud H11):**
   - The suites ran against `https://{e2b,vercel,daytona}.sandbox.inevitable.fyi` through
     Traefik TLS, and `try.py` ran from off-network on a hosted Fly sprite (2026-10-03).
   - It includes a >60 s upload and an input stream held >60 s; websecure `readTimeout` was
     raised to 10m.
   - It checks 401 without a key on every control plane.
   - `platform/geek/sandboxd-site/try.py` (served at `https://sandbox.inevitable.fyi/try.py`)
     runs create, exec, serve-a-port and delete for sprites, e2b, daytona and vercel with pinned
     SDKs.

### Where Modal is today

- **SDK coverage:** `e2e/providers/modal/` has `run.sh`, `target.py` and `probe.py`. That is
  **Python only**: the target (V2 and V1) plus 14 checks.
- **Hosted validation:** none. There is no `recorder`, no `golden/`, and no hosted run.
  `modal-differences.md` says: "No traces of hosted Modal were recorded … this list is from the
  client's source and the protocol, not a trace comparison." Several items in it are marked
  *unverified*: the default workdir, a missing command (127 against a refused exec), and the
  `MODAL_*` env vars in the container.
- **Version handling:** no version check.
- **Deployment:** not in the home-cloud deploy, which lists it under "Deferred: Modal". It is not
  in `try.py`, and there is no `--modal-public-url` flag (only `--modal-router-url`).

### What parity for Modal requires

1. **Probe suites**
   - `probe.py`: extend to the surface wisp claims, plus the next items (stdin, tunnels, fs, tags,
     list, from_name) as they land.
   - **`probe.mjs`** on `modal@0.11.0`, mirroring the Go/JS `examples/sandbox*.ts`.
   - Optionally **`probe_test.go`** on `modal-client/go@v0.11.0`.
   - **`sweep.py`**: terminate leftover sandboxes on the hosted account (`Sandbox.list` per app),
     and stop the probe app.
   - Probe image: use `Image.debian_slim()` for py, but **`from_registry("python:3.13-slim")`**
     for all three, so the suites are comparable.
2. **Hosted run plus golden traces**
   - Needs a Modal account and token in `~/.config/arugula/providers.env` (`MODAL_TOKEN_ID`,
     `MODAL_TOKEN_SECRET`).
   - **Pricing (modal.com/pricing, 2026-10-03):** Starter $0/month with **$30/month free
     credits**, 100 containers / 10 GPU concurrency, 3 seats. Sandbox CPU is $0.00003942 per
     core-second and memory $0.00000667 per GiB-second.
   - A probe run of about 20 sandbox-minutes at 1 core and 1 GiB costs about **$0.06**.
   - Third-party pages disagree on whether a card is needed to unlock the credits; check at
     sign-up.
3. **A gRPC recording proxy** (`e2e/providers/modal/record_proxy.py` or a Go program); design
   below.
4. **Comparison:** run the same suites with `--record` against sandboxd. A normalising differ
   (IDs, timestamps, JWTs, hosts) reports field-level differences per RPC. A differences doc then
   gets a "Seen in the traces" section that replaces the *unverified* items.
5. **Public path acceptance (H11 for Modal)**, per home-cloud's "Deferred: Modal":
   - a sandboxd listener on `127.0.0.1:7794`;
   - a **`--modal-public-url`** flag that defaults `--modal-router-url`, as W1 did for the
     others;
   - `modal.sandbox.inevitable.fyi` (no wildcard) with its IngressRoute Service using
     `scheme: h2c`;
   - clients connect TLS with ALPN h2.

   Then run `MODAL_SERVER_URL=https://modal.sandbox.inevitable.fyi ./run.sh` from off-network.

   Modal-specific checks:
   - `TaskExecStdioRead` server-streams and `TaskExecWait` 60 s long-polls survive Traefik
     (`readTimeout` is already 10m);
   - an exec with a long-running stdout stream;
   - `UNAUTHENTICATED` (AuthError) without or with a bad secret;
   - with a non-localhost server the router URL must be `https` with a trusted cert. This is the
     real test that `--modal-router-url` / `--modal-public-url` is correct.
6. **`try.py`:** add `"modal==1.6.0"` to the script deps and a `try_modal()`. I resolved the
   deps with modal, e2b, daytona, sprites and vercel-sandbox together (`uv pip compile`, py3.12):
   modal 1.6.0, grpclib 0.4.9, protobuf 6.33.6, **no conflicts**.

   `try_modal()`:
   - sets `MODAL_SERVER_URL=https://modal.{DOMAIN}`, `MODAL_TOKEN_ID=wisp`,
     `MODAL_TOKEN_SECRET=KEY`, and `MODAL_CONFIG_PATH` set to a temp file;
   - then `App.lookup`, `Sandbox.create(image=debian_slim)`, `exec("sh","-c","uname -sr && python3 --version")`
     and `terminate()`.

   Port-serving waits for tunnels (`SandboxGetTunnelsV2`), which wisp has not built.

### How the gRPC recording proxy would work

The topology (Python):

```
modal client --h2c--> proxy :8795 (MODAL_SERVER_URL=http://127.0.0.1:8795)
                       |--TLS h2--> api.modal.com:443          (/modal.client.ModalClient/*)
                       '--TLS h2--> <router host from response> (/modal.task_command_router.TaskCommandRouter/*)
```

- **Why localhost:** with `MODAL_SERVER_URL` on 127.0.0.1, Python accepts a **plaintext
  `http://` router URL**. Go and JS accept `https://` and dial it in plaintext. So the proxy
  needs no TLS certificate at all on the client side.
- **Rewriting the router URL.** The proxy must decode and re-encode three response types and
  replace `url` with `http://127.0.0.1:8795` for Python, or `https://127.0.0.1:8795` for JS and
  Go, keyed on `x-modal-client-type`:
  - `SandboxCreateV2Response.command_router_access.url`;
  - `SandboxGetCommandRouterAccessResponse.url`;
  - `TaskGetCommandRouterAccessResponse.url`.

  It remembers `jwt → original upstream URL`. Router calls carry `authorization: Bearer <jwt>`,
  so the proxy routes each one to its upstream by JWT. A single listener is enough, since the
  service paths differ.

  JWTs are refreshed by a fresh `*GetCommandRouterAccess` call, which goes through the same
  rewrite, so the map stays current. Also record `sandbox_id → upstream` as a fallback.
- **Nothing else needs rewriting for sandboxes.** V2 sandbox RPCs go to the same control-plane
  stub, with `x-modal-auth-token` metadata (`sandbox.py:1176…`). The tunnel URLs
  (`*.modal.host`) are data only.
- **Implementation choice:**
  - **Python** on **grpclib**, the same library as the client, using the wheel's `modal_proto`
    for decode/encode. The generic handler forwards raw bytes and decodes only for logging and
    for the three rewrites.
  - Or **Go**, with `grpc.UnknownServiceHandler` plus a raw-bytes codec and `protojson` from
    the full generated pb (option B above, or a full gen).
- **Streaming:** forward `unary→stream` (`ImageJoinStreaming`, `TaskExecStdioRead`,
  `SandboxGetLogs`) and `stream→unary` (`TaskExecStdinWriteStream`) frame by frame with arrival
  offsets, like the E2B recorder's Connect frames.
- **Metadata:** record headers and trailers, including `grpc-status`, `grpc-message` and
  `x-modal-warning`, and the status codes of errors.
- **Scrubbing:**
  - drop `x-modal-token-id/secret`, `x-modal-auth-token`, `authorization` and
    `x-modal-*oauth*`;
  - replace JWT-shaped strings in bodies (`AuthTokenGetResponse.token`,
    `command_router_access.jwt`, and in future `sandbox_token`) with `<jwt exp=…>`, keeping only
    the decoded `exp`/claims shape;
  - scrub every literal token value seen, as `recorder.py` does.
- **Output:**
  - one JSONL line per call: `{seq, t, service, method, req_headers, req (protojson),
    resp_headers, frames[{t, msg}], trailers, status}`;
  - plus `--pretty`, as for E2B;
  - plus `--upstream http://127.0.0.1:<sandboxd>` mode, so the same proxy records wisp's side
    with no rewriting.
- **Gotchas:**
  - the client's default server list is two URLs, so set exactly one;
  - `x-modal-host` carries the URL's hostname (`127.0.0.1`), which hosted Modal might use. If
    hosted rejects it, rewrite it to `api.modal.com` upstream;
  - grpclib does prior-knowledge h2c, which grpc-go and grpclib servers both handle;
  - hosted image builds are slow and cached per workspace, so the first run's
    `ImageJoinStreaming` differs from later ones. Record twice and keep the warm one as golden,
    or both.

---

## 3. Version tracking

### Release cadence (PyPI JSON, 1.x)

- Stable releases:
  - 1.0.0 2025-05-16 · 1.1.0 07-17 · 1.2.0 10-09 · 1.3.0 12-20;
  - 1.4.0 2026-03-25 · 1.4.1 03-31 · 1.4.2 04-16 · 1.4.3 05-18;
  - 1.5.0 06-09 · 1.5.1 06-23 · 1.5.2 07-10 · 1.5.3 07-23 · 1.5.4 08-12 · 1.5.5 08-28;
  - **1.6.0 09-29**.

  That is a minor about every 3 months and a patch every 2 to 4 weeks.
- Since March 2026 there are **nightly `.devN` pre-releases** (about 01:00 UTC daily). npm
  `modal@next` gets `-dev.N` on the same rhythm.
- `RELEASING.md`: JS and Go releases are cut with `inv update-version-go-js` and tagged
  `js/vX` and `go/vX` from `js/package.json`.

### Measured protocol drift (descriptor diff between tags; `../protocmp/drift.py`)

| From → to | Methods | Types added / removed / changed |
|---|---|---|
| py/v1.4.0 → py/v1.5.0 | 201 → 224 (+23) | +61 / −0 / 25 changed |
| py/v1.5.0 → py/v1.5.5 | 224 → 263 (+39, 1 signature changed) | +72 / −0 / 55 changed |
| py/v1.5.5 → py/v1.6.0 | 263 → 277 (+14, incl. `SandboxContainerCreateV2`, `TaskSetOutboundPolicy`) | +43 / −1 / 30 changed (incl. `Sandbox`, `ProxyInfo`, `SandboxGetExitSnapshotResponse`) |
| py/v1.6.0 → main (2026-10-03) | 277 → 277 | +1 (`SecretSource`) / 9 changed: **`SandboxCreateV2Request`, `SandboxCreateV2Response` (+`sandbox_token`, "Sent as the x-modal-sandbox-token metadata header"), `TaskExecStartRequest` (+`secret_sources`)** |

Changes are additive in proto terms: fields and RPCs are added, rarely removed. A server built
from 1.6.0 therefore keeps parsing newer clients' messages, because unknown fields are ignored.
The breakage comes from **behaviour**:

- a newer client calls an RPC wisp does not serve;
- it starts *requiring* a new response field. A likely example is the `sandbox_token` echoed as
  `x-modal-sandbox-token`;
- it changes the debian_slim recipe or the builder version;
- it changes the ID-shape rules.

### Detecting drift

1. **Per-release proto diff (cheap, no KVM).**
   - A scheduled CI job (daily or weekly) lists tags `py/v*`, `js/v*` and `go/v*` newer than the
     pin.
   - For each, it fetches `modal_proto/*.proto`, compiles, and diffs against the pin with the
     descriptor differ.
   - It reports added and removed RPCs, and every changed message **in wisp's kept set**: the
     19 RPCs and the types they reach, i.e. `prune.py`'s `KEEP` closure.
   - It also diffs the wheel's embedded descriptors (`pip download modal==X`, then a `check.py`
     variant), to catch tag/wheel mismatches.
   - Output: an issue or PR comment, "1.6.1: no change in served subset" or "SandboxCreateV2Response +sandbox_token".
2. **Behavioural, on KVM.** Run `e2e/providers/modal/run.sh` with requirements overridden to the
   new release, against a sandboxd gate stack.
   - It cannot run on GitHub-hosted runners (no `/dev/kvm`). Use a self-hosted runner on this
     host, or the existing illogical/`~/ws/gate-all.sh` routine as a scheduled job.
   - It also catches recipe drift: the debian_slim `FROM` line or steps.
3. **Hosted re-record** (on demand, when 1 or 2 flags something). Rerun the probe suites against
   hosted Modal with the new client to see what the server now sends.
4. Optionally watch npm `modal` dist-tag `latest` and the Go proxy `@v/list` too. They normally
   move together with Python.

### What "pinned to 1.6.0" means for users

- **Client side:**
  - users install `modal==1.6.0`, with `npm i modal@0.11.0` and
    `go get github.com/modal-labs/modal-client/go@v0.11.0` for the others;
  - `docs/modal-client.md` says so;
  - `gen.sh` and `e2e/providers/modal/requirements.txt` hold the pin and move together;
  - with option A, the pin also covers the git commit `a3f75167a9`.
- **Server side** (not built today; wisp ignores the version headers). Proposed policy, using
  only mechanisms the clients already understand:
  - **Python** (`x-modal-client-type: 1`): `x-modal-client-version == 1.6.0` passes silently.
    Any other version is served, but every response carries an **`x-modal-warning`** trailer
    (percent-encoded, printed once by the client):
    `"this server implements modal 1.6.0's protocol; you are running 1.X.Y — pin modal==1.6.0"`.
  - **Strict mode** (a `--modal-strict-version` flag): refuse other versions with
    `FAILED_PRECONDITION` (the client raises `ConflictError`) and the same message, before any
    side effects. Recommend warn-by-default: proto changes are additive, so near versions often
    work.
  - **Go and JS** (`x-modal-client-type` 8/9): **do not check `x-modal-client-version`**, which
    is a constant `1.0.0` there. Check `x-modal-libmodal-version`
    (`modal-go/0.11.0`, `modal-js/0.11.0`) with the same warn/strict rule.
  - **Unknown RPC:** `UNIMPLEMENTED` already names the method. Adding "(this server implements
    modal 1.6.0)" to the status message would make drift failures self-explaining.

---

## 4. Licensing and ToS facts (no legal advice)

**Licensing**

- The `modal` wheel 1.6.0 has `License-Expression: Apache-2.0`, and wisp already copies its
  LICENSE next to `modalpb`.
- **modal-labs/modal-client** (py, js, go, and `modal_proto/*.proto`) is **Apache-2.0** (GitHub
  license API). npm `modal@0.11.0` declares `Apache-2.0`. libmodal (archived) is Apache-2.0.
- So the `.proto` sources and the generated Go pb are Apache-2.0. Redistribution needs the
  license and NOTICE handling Apache-2.0 asks for: keep LICENSE and mark modified files.

**Modal Terms of Service** (https://modal.com/legal/terms, "Last Updated: May 2026")

- **§1.10, "Modal Client":** SDKs at github.com/modal-labs/modal-client ("Modal Tools") "are
  community-developed and made available under open source licensing terms … are not part of
  the Service, are not subject to this Agreement".
- **§1.3** restrictions: the customer will not "(b) reverse engineer, decompile, disassemble, or
  otherwise attempt to discover the underlying structure, ideas, or algorithms of the Service or
  any software used to provide or make the Service available; (c) rent, resell or otherwise
  allow any third party direct access to or use of the Service".
- **Facts relevant here:**
  - wisp's implementation so far is derived from the Apache-2.0 client and its protos (§1.10
    material), not from the Service.
  - **Recording golden traces against hosted Modal means using the Service under the ToS with
    a Modal account**, and capturing the server's responses to learn its behaviour. That is the
    activity §1.3(b) addresses. The E2B and Vercel recordings had no equivalent clause
    checked.
  - The ToS has no explicit clause on benchmarking or competing products, except §1.7, which
    reserves Modal's right to build competing things.
  - Whether a recording run falls under §1.3(b) is a question for the owner or counsel. The
    probes could instead be limited to SDK-visible behaviour (results, exceptions, timings) with
    no wire capture, which gives less fidelity.
- "Modal" is a trademark. wisp's docs already describe it as "Modal-compatible", not as Modal.

---

## Decisions for the owner

1. **Proto source.**
   - **(A) (recommended)** Fetch `.proto` from modal-client at `py/v1.6.0` (`a3f75167a9`) and keep
     `check.py` against the wheel.
   - (B) Depend on `modal-client/go@v0.11.0`'s generated `pb` (opaque API, a rewrite of the
     handlers).
   - (C) Keep the descriptor dump.
2. **Which SDKs to support.** Python 1.6.0 + Go 0.11.0 + JS 0.11.0, which is recommended. Or
   Python only. Go/JS support requires:
   - **(i)** the router URL scheme chosen per `x-modal-client-type` (small);
   - **(ii)** `Image.from_registry` / `FROM <tag>` via wisp's OCI image cache (moderate). This is
     also the single biggest gain for Python users;
   - **(iii)** sandbox entrypoint stdio, which the Go/JS headline examples use.
3. **Record against hosted Modal?** It needs a Modal account and token (Starter: free with
   $30/month credits; a run costs cents), plus a decision on the ToS §1.3(b) question. Without
   it, the differences doc stays source-derived and its "unverified" items remain.
4. **Build the gRPC recording proxy** (localhost h2c to TLS, router URL rewrite plus JWT routing,
   scrubbing) in Python on grpclib or in Go. It is also useful on its own to record sandboxd's
   side for regression diffs.
5. **Version policy.** Warn by default via `x-modal-warning` for Python ≠ 1.6.0, with an optional
   strict `FAILED_PRECONDITION` mode. Go/JS are checked on `x-modal-libmodal-version`, never on
   the constant `x-modal-client-version: 1.0.0`.
6. **Drift CI.**
   - A no-KVM scheduled proto-diff job against new `py/` tags (and nightlies?), scoped to the
     served subset.
   - Plus a self-hosted KVM job running `run.sh` on new releases.
   - Decide who gets paged and whether the pin moves on every minor or only on demand. 1.6.1's
     `sandbox_token` is the first thing to watch.
7. **Public deploy.** Add `--modal-public-url`, the `modal.sandbox.inevitable.fyi` h2c
   IngressRoute and an H11 Modal run, plus `try_modal()` in `try.py` (deps resolve cleanly).
   Or keep Modal deferred, as home-cloud's plan does today.

## Sources

- github.com/modal-labs/libmodal (archived; README migration note)
- github.com/modal-labs/modal-client: tags `py/v1.6.0`, `go/v0.11.0`, `js/v0.11.0`;
  `modal_proto/`, `go/config.go`, `go/client.go`, `go/task_command_router_client.go`,
  `go/image.go`, `go/sandbox_fs.go`, `js/src/config.ts`, `js/src/client.ts`,
  `js/src/task_command_router_client.ts`, `RELEASING.md`, `go/scripts/gen-proto.sh`
- https://pypi.org/pypi/modal/json; `npm view modal`; https://proxy.golang.org/github.com/modal-labs/modal-client/go/@v/list
- modal 1.6.0 wheel: `_utils/task_command_router_client.py:261-300`, `client.py:155-158`,
  `_utils/grpc_utils.py:335-355`, `sandbox.py`, `sandbox_fs.py:39`
- https://modal.com/pricing (2026-10-03); https://modal.com/legal/terms (§1.3, §1.7, §1.10; May 2026)
- wisp: `docs/plans/multi-provider-burndown.md`, `docs/providers/{modal,modal-differences,e2b-differences}.md`,
  `e2e/providers/{e2b,vercel,daytona,modal}/`, `frontend/modal/modalpb/{gen.sh,prune.py,check.py}`,
  `cmd/sandboxd/main.go`
- home-cloud: `docs/sandboxd-deploy-plan.md` (H11, "Deferred: Modal"), `platform/geek/sandboxd-site/try.py`
