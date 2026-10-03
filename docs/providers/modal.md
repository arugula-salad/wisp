# Modal: feasibility of the unmodified `modal` client against wisp

Client studied: **modal 1.6.0** wheel (Apache-2.0, per `modal-1.6.0.dist-info/METADATA` `License-Expression: Apache-2.0` and `licenses/LICENSE`).
Paths below are relative to the installed package (`site-packages/modal/...`).

**Verdict: GO for the sandbox subset.** I confirmed it empirically during phase 0. A throwaway fake server,
about 100 lines of Python on grpclib, ran the target script end to end with the stock client and printed `'hi\n'`.
The whole script uses **8 RPCs** on the default path. No Modal account or credentials were used. Four runs were done:
the V2 default, V1 (`MODAL_SANDBOX_V2=0`), image-join with task-id polling and `p.wait()`, and one port serving
both gRPC services. The tables below come from the captured RPC logs. The fake is not committed. Rebuilding it
as the phase 5 probe harness takes about an hour.

---

## 1. Transport and config

| Item | Finding | Ref |
|---|---|---|
| Stack | gRPC over HTTP/2 using **grpclib** (pure-Python asyncio), protobuf codec | `_utils/grpc_utils.py:17-26` |
| Schemes | `http://` = plaintext h2c, `https://` = TLS, `unix://path` = UDS. Port defaults to 80/443 | `_utils/grpc_utils.py:358-380` |
| Plaintext OK? | **Yes** for the control plane, to any host | same |
| Server URL | `server_url` setting, overridden by env `MODAL_SERVER_URL`. Default `https://api.modal.com,https://api.modal2.com`. A comma-separated list means staggered failover | `config.py:116,355`, `_utils/grpc_utils.py:409` |
| Config file | `~/.modal.toml`, or the path in `MODAL_CONFIG_PATH`. Each profile is a section, selected with `MODAL_PROFILE`. Any setting can also come from `MODAL_<SETTING>` | `config.py:1-30,93-96,121` |
| Environment | `MODAL_ENVIRONMENT`, sent as `environment_name` in `AppGetOrCreate`/`EnvironmentGetOrCreate`. If unset it is `""` | `config.py:370`, `_environments.py:418` |
| Sandbox backend | **`sandbox_v2` defaults to True** (`MODAL_SANDBOX_V2=0` gives V1). V2 is used unless gpu, NFS or pty is requested | `config.py:366`, `sandbox.py:853` |
| Retries | `UNAVAILABLE/INTERNAL/UNKNOWN/CANCELLED/DEADLINE_EXCEEDED` are retried. gRPC status maps to a Python exception (e.g. `NOT_FOUND`->`NotFoundError`, `UNAUTHENTICATED`->`AuthError`) | `_utils/grpc_utils.py:197-203`, `_grpc_client.py:27-43` |
| Server warnings | Sent as trailing metadata `x-modal-warning` (percent-encoded) and printed by the client | `_utils/grpc_utils.py:338-355` |

Minimal client env:
`MODAL_SERVER_URL=http://localhost:<port> MODAL_TOKEN_ID=<anything> MODAL_TOKEN_SECRET=<anything>`

## 2. Auth, headers, handshake, secondary endpoints

**Headers on every control-plane call.** These come from `client.py:41-82` and `grpc_utils.py:388-397,630`:
`x-modal-token-id`, `x-modal-token-secret`, `x-modal-client-version: 1.6.0`, `x-modal-client-type: 1`,
`x-modal-python-version`, `x-modal-node`, `x-modal-platform`, `x-modal-host` (the hostname from the URL), `x-modal-timestamp`,
and `user-agent: modal-client/1.6.0 (...)`. OAuth headers (`x-modal-refresh-token` etc.) are sent instead when OAuth is configured.
Tokens are required client-side. Without them `Client.from_env` raises `AuthError` before any RPC (`client.py:313-342`). Any strings work.

**No ClientHello.** `ClientHello` (`client.py:230-241`) is only called by `modal token`/`Client.verify`. It is **not** in this flow.
There is no client-side version gate. Version policy lives entirely on the server: it can reject or warn based on `x-modal-client-version`.
One client-side check does apply. The image builder version from the server must be in the client's supported set
`{"2023.12","2024.04","2024.10","2025.06"}`, otherwise the client raises `VersionError` (`_image.py:72,323-340`).

**Input-plane auth token (V2).** `AuthTokenGet` returns a JWT, which is attached as the `x-modal-auth-token` header on every `Sandbox*V2` call.
The client only decodes `exp` and does not verify the signature (`_utils/auth_token_manager.py:118`, `sandbox.py:1173-1177`).

**Task command router (separate gRPC service).** All exec and stdio goes through it, in both V1 and V2. There is no `ContainerExec` fallback in 1.6.0.
- Service `modal.task_command_router.TaskCommandRouter` has 23 methods (`modal_proto/task_command_router_grpc.py`).
- The client gets `{url, jwt}` in one of three ways:
  - (V2) inline in `SandboxCreateV2Response.command_router_access`;
  - (V2) else from `SandboxGetCommandRouterAccess`;
  - (V1) from `TaskGetCommandRouterAccess(task_id)`.
  Refs: `_utils/task_command_router_client.py:217-243,324-365`.
- It dials that URL on a **new channel**. Calls carry only `authorization: Bearer <jwt>`, not the token headers (`:428-430`). The JWT is not verified client-side; `exp` is parsed for refresh (`:71-85,885-919`).
- **TLS rule (`:272-288`).**
  - `http://` router URLs are accepted **only if** `MODAL_SERVER_URL`'s host is `localhost`, `127.0.0.1`, `::1` or `172.21.0.1` (`client.py:155-158`).
  - Against those hosts, `https://` is also accepted with verification disabled.
  - For any other host the router URL must be `https://` with a cert the client trusts.
- **One port can serve both services.** The service paths differ (`/modal.client.ModalClient/*` and `/modal.task_command_router.TaskCommandRouter/*`), and the one-port run confirmed it.

## 3. RPC sequence for the target script (captured, V2 default)

| # | RPC (service) | Key request fields (observed) | Response fields the client reads | Purpose | Fake? | Notes |
|---|---|---|---|---|---|---|
| 1 | `AppGetOrCreate` (ModalClient, unary) | `app_name="demo"`, `object_creation_type=CREATE_IF_MISSING`, `environment_name=""` | `app_id` | `App.lookup` (`app.py:375-381`) | trivial | Upsert name -> `ap-...` |
| 2 | `EnvironmentGetOrCreate` (unary) | `deployment_name=""` (empty) | `environment_id`, `metadata.name`, **`metadata.settings.image_builder_version`** | Image builder version (`_image.py:664-668`) | trivial | **Must return e.g. `"2025.06"`**. An empty value gives `VersionError` (verified) |
| 3 | `ImageGetOrCreate` (unary) | `image.dockerfile_commands[]`, `app_id`, `builder_version="2025.06"`, `namespace=GLOBAL` (see §4) | `image_id`, `result.status`, optional `metadata` | Resolve/build image (`_image.py:751-764`) | moderate | If `result.status=SUCCESS` the client skips #3b |
| 3b | `ImageJoinStreaming` (unary->stream) | `image_id`, `timeout=55`, `last_entry_id` | stream of `{task_logs[].data, entry_id, result.status, metadata}` | Build logs and final status (`_image.py:448-479`) | moderate | Only called when #3 returns no status. The client loops until some message has `result.status`, and retries 3x on error |
| 4 | `AuthTokenGet` (unary) | empty | `token` (JWT with `exp`) | Input-plane token (`auth_token_manager.py:118`) | trivial | Any JWT-shaped string with `exp` |
| 5 | `SandboxCreateV2` (unary, +`x-modal-auth-token`) | `app_id`, `definition{entrypoint_args=["sleep","infinity"], image_id, timeout_secs=300, resources, network_access{OPEN}, open_ports}` | `sandbox_id`, `task_id`, `command_router_access{url,jwt}`, `tunnels`, `metadata` | Boot the VM (`sandbox.py:1175-1196`) | moderate | **sandbox_id shape decides V1 or V2**: `sb-`+22 base62 chars is V1, anything else is V2 (`sandbox.py:237-253`). V2 task ids end in `V` (`task_command_router_client.py:209-214`) |
| 5b | `SandboxGetTaskIdV2` (unary) | `sandbox_id` | `task_id`, `task_result` | Only if #5 omitted `task_id`. Polled until non-empty (`sandbox.py:2088-2125`) | trivial | Optional |
| 5c | `SandboxGetCommandRouterAccess` (unary) | `sandbox_id` | `url`, `jwt` | Only if #5 omitted `command_router_access` | trivial | Optional |
| 6 | `TaskExecStart` (TaskCommandRouter, unary, Bearer) | `task_id`, `exec_id` (client UUID, idempotency key), `command_args=["echo","hi"]`, `stdout_config=PIPE`, `stderr_config=PIPE`, opt `workdir`, `env`, `timeout_secs`, `pty_info` | (empty) | Start the process (`sandbox.py:2349-2416`) | moderate | Maps to wisp exec. Should be idempotent on `exec_id` |
| 7 | `TaskExecStdioRead` (unary->stream, Bearer) | `task_id`, `exec_id`, `offset`, `file_descriptor=STDOUT` | stream of `{data}` | `p.stdout.read()` (`task_command_router_client.py:1051-1080`) | moderate | **Clean end of stream = EOF.** On a transient error the client reconnects with `offset` = bytes already received. The server must buffer per fd and serve from that offset. Only stdout was read here |
| 8 | `SandboxTerminateV2` (unary, +`x-modal-auth-token`) | `sandbox_id` | `existing_result` (ignored) | `sb.terminate()` (`sandbox.py:2056-2062`) | trivial | |

Additional calls seen in the `p.wait()` run:
- `TaskExecWait` (`task_id`, `exec_id`, returns `code`/`signal`; the client calls with a 60s timeout and re-calls).
- A second `TaskExecStdioRead` for STDERR.
- `TaskExecPoll` is used by `p.poll()`.

V1 path (`MODAL_SANDBOX_V2=0`):
`AppGetOrCreate`, `EnvironmentGetOrCreate`, `ImageGetOrCreate`, `SandboxCreate` (returns `sandbox_id`), `SandboxGetTaskId` (returns `task_id`), `TaskGetCommandRouterAccess` (returns `url`, `jwt`), `TaskExecStart`, `TaskExecStdioRead`, `SandboxTerminate`.
That is 9 RPCs, with no AuthTokenGet.

Not called by this script: `ClientHello`, `AppHeartbeat`/`AppClientDisconnect` (`lookup` does not run an app), blob/S3 upload URLs, mounts, and `SandboxWait`.

Likely next RPCs for realistic use, to plan for:

| RPC | Used by |
|---|---|
| `SandboxWaitV2` | `sb.wait()`, `sb.poll()` |
| `SandboxStdioReadV2` / `SandboxStdinWriteV2` (router) | Sandbox top-level stdio |
| `TaskExecStdinWrite` / `TaskExecStdinWriteStream` | `p.stdin` |
| `SandboxGetTunnelsV2` | `sb.tunnels()`, for ports |
| `SandboxTagsSetV2` / `SandboxTagsGetV2` | Sandbox tags |
| `SandboxListV2`, `SandboxGetFromNameV2` | Listing and lookup |
| `ImageFromId`, `ImageGetOrCreate` with `image_registry_config` | `Image.from_registry` |
| fs ops | `sandbox_fs.py` (not inspected in detail) |

## 4. Image building

`Image.debian_slim(python_version=None)` is defined at `_image.py:2577-2634`. With builder `2025.06` and local Python 3.14 it sends exactly these dockerfile commands:
```
FROM python:3.14.2-slim-bookworm          # python patch = local interpreter series, pinned via builder/base-images.json
RUN apt-get update
RUN apt-get install -y gcc gfortran build-essential
RUN pip install --upgrade pip wheel uv    # base-images.json "package_tools"
RUN echo 'debconf debconf/frontend select Noninteractive' | debconf-set-selections
CMD ["sleep", "172800"]
```
- On builders `<= 2024.04` it also writes `/modal_requirements.txt` and runs `RUN pip install -r ...`, which installs modal's runtime deps. On `>= 2024.10`, modal requirements are **mounted at runtime** for Functions only (`_image.py:2593-2630`).
- **Sandboxes need nothing from modal inside the image.** The entrypoint runs as given, and exec goes through the router, i.e. wisp's agent. So the image can be any rootfs with `sleep`/`echo`.
- The Python version in the `FROM` line varies with the **caller's** local Python (`_dockerhub_python_version`, `_image.py:206`; supported series listed at `:78-81`). So the content hash of the request differs across client machines.
- Request fields: `image{dockerfile_commands[], context_files, base_images, gpu_config, image_registry_config, secret_ids, ...}`, `app_id`, `builder_version`, `namespace`, `force_build`, `ignore_cache`.

Minimal fake:

| Need | Approach |
|---|---|
| `ImageGetOrCreate` | Hash `(dockerfile_commands, builder_version)`, then return `{image_id, result.status=SUCCESS}` immediately. That makes `ImageJoinStreaming` optional |
| Mapping to a rootfs | Keep a table from the `FROM` line or a recognised command list to a prebaked wisp rootfs (e.g. `python:3.X-slim-bookworm` gives a debian-slim+python rootfs). Ignore the `RUN` lines or require that they are a known prefix |
| Unknown command list | Fail with `result{status=FAILURE, exception="unsupported image"}`, or with gRPC `INVALID_ARGUMENT` |
| Honest version | Later, either really execute `FROM`+`RUN` (OCI pull, then run the steps in a builder VM and snapshot), or support only `from_registry` / `FROM` with no `RUN` |

## 5. Go build plan, size and risks

**Protos.** The wheel ships **no `.proto` files**, only generated `api_pb2.py`/`api_grpc.py`.
- Option 1: take `modal_proto/api.proto` and `task_command_router.proto` from github.com/modal-labs/modal-client at tag `v1.6.0`.
- Option 2: dump the embedded `FileDescriptorProto`s from `modal_proto/api_pb2.py` and `task_command_router_pb2.py`.

Details:
- Packages are `modal.client` and `modal.task_command_router`, both with `go_package = github.com/modal-labs/modal/go/proto`. Override with `M` flags or use buf.
- Dependencies are only Google well-known types.
- Size: `ModalClient` has **254 RPCs** across 575 messages, and `TaskCommandRouter` has 23. `protoc-gen-go` + `protoc-gen-go-grpc` will work as-is.
- Embed `Unimplemented*Server`. Unhandled RPCs return `UNIMPLEMENTED`, which the client raises as `UnimplementedError`.
- grpc-go serves h2c fine. grpclib does not use ALPN for plaintext, and the Python-to-grpclib test confirms prior-knowledge h2c works.

**Work estimate (sandbox MVP).**

| Area | RPCs | Effort |
|---|---|---|
| Identity: App/Env/AuthToken | `AppGetOrCreate`, `EnvironmentGetOrCreate`, `AuthTokenGet` | trivial (in-memory maps) |
| Image | `ImageGetOrCreate` (+ `ImageJoinStreaming`) | moderate. The policy for mapping commands to a rootfs is the real design question |
| Sandbox lifecycle | `SandboxCreateV2`, `SandboxTerminateV2`, `SandboxGetTaskIdV2`, `SandboxGetCommandRouterAccess`, `SandboxWaitV2` (+ the V1 equivalents if wanted) | moderate. Maps directly onto wisp create/kill |
| Exec | `TaskExecStart`, `TaskExecStdioRead` (offset-resumable server stream), `TaskExecWait`, `TaskExecPoll`, `TaskExecStdinWrite[Stream]` | moderate. Needs a per-exec stdout/stderr ring buffer with byte offsets and an idempotent `exec_id` |
| Ports/tunnels | `SandboxGetTunnelsV2` | moderate |

That is about 10-15 RPCs for a credible "sandbox + exec" MVP, roughly 1-2 kLOC of Go plus codegen.

**Hard parts and risks.**
1. **Private, unstable API.** It is not a public contract. The default flipped to V2 sandboxes and router-only exec in recent versions, and 1.6.0 has no `ContainerExec` path. **Pin the client version** and run the captured-sequence test in CI against each new modal release.
2. **Router TLS when not on localhost.** Remote users need an `https://` router URL with a trusted cert. Plaintext only works when `MODAL_SERVER_URL` points at localhost/127.0.0.1/::1 (an SSH tunnel is fine).
3. **Stdio semantics.** The stream end is EOF, resume is offset-based, stdout and stderr are separate streams, and `TaskExecWait` uses long-poll timeouts. Get these right or `read()` hangs or duplicates output.
4. **Image fidelity.** Real Dockerfile execution is a large project, so start with mapping to prebaked rootfs images.
5. **Server-side policy surface.** The server must emit a valid `image_builder_version`, sandbox ids in V2 shape (not `sb-`+22 base62), and V2 task ids ending in `V`, or must consistently use the V1 shapes.

**Out of scope.**
- Functions/Cls (container entrypoint, function inputs/outputs, `AppHeartbeat`, mount and blob upload via presigned URLs).
- Volumes, NFS, Secrets (beyond ignoring `secret_ids`), Dict/Queue.
- Snapshots and `_experimental_*`.
- GPU (forces V1), PTY (forces V1), `modal deploy`/`modal run`, OAuth/token-flow CLI, and the dashboard.
