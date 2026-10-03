# Modal front end: sandbox lifecycle and management (research for the full build-out)

Client: modal 1.6.0. Client paths are relative to `site-packages/` (`modal/...` and `modal_proto/...`).
Wisp paths are relative to the repo root. Proto shapes were dumped from `modal_proto/api_pb2.py` and
`task_command_router_pb2.py` with the venv's python. Effort sizes: **S** is half a day or less, **M** is 1-3 days,
**L** is more than 3 days or needs a design decision first.
Every new RPC also costs a line in `frontend/modal/modalpb/prune.py` `KEEP` (:19-45) plus a `gen.sh` rerun.
A new RPC that is read-only also goes in `readOnly` (`frontend/modal/modal.go:218`).

## Summary

- The lifecycle core already works for both V1 and V2: create (subset), `from_id`, `wait`, `poll`, `returncode`,
  `terminate` and `detach`. That is 15 control RPCs (`frontend/modal/control.go`). `terminate(wait=True)` needs no
  new RPC, but nothing tests it yet.
- **Biggest correctness gap: `Sandbox.create` silently ignores many fields** that it should either honor or refuse.
  The gate at `control.go:235-250` covers only pty, gpu, mounts, secrets, volumes, ports, name and tags.
  - These are dropped without a word: `block_network`, `outbound_cidr_allowlist`, `outbound_domain_allowlist`,
    `idle_timeout`, `readiness_probe`, `inbound_cidr_allowlist`, `custom_domain`, `include_oidc_identity_token`,
    `experimental_options`, cpu/memory *limits*, `region`, `cloud` and `runtime`.
  - `block_network=True` therefore gives a sandbox open egress. That is a security bug and should be fixed first (S).
- **V1 `env=` is broken today.** V1 turns `env` into `Secret.from_dict`, which calls `SecretGetOrCreate`, and that
  RPC is UNIMPLEMENTED (`modal/sandbox.py:899-901`, `modal/secret.py:216-233`). V2 sends env inline, so it works.
- Missing management RPCs, all M or smaller:
  - name, `from_name` and `set_name`: `SandboxGetFromName[V2]`, `SandboxSetName`;
  - tags: `SandboxTags{Get,Set}[V2]`;
  - `Sandbox.list`: `SandboxList[V2]`, with `before_timestamp` pagination;
  - readiness: router `SandboxWaitUntilReady`;
  - connect tokens: `SandboxCreateConnectToken[V2]`;
  - entrypoint logs: `AppFetchLogs`/`AppCountLogs`;
  - ephemeral apps (`with app.run()`): `AppCreate`, `AppPublish`, `AppHeartbeat`, `AppClientDisconnect`.
- **Large, needing design first:**
  - sidecar containers: `SandboxContainerCreateV2` and the router `TaskContainer*`. A wisp VM has one rootfs;
  - exit snapshots: `SandboxGetExitSnapshot[V2]`. These need a checkpoint taken before the delete, and a
    snapshot-to-image path;
  - CIDR egress allowlists: `internal/netpolicy` is domain-only;
  - `_experimental_outbound_policy`: header rewriting with secrets.
- The engine already provides most of what the M items need:
  - egress rules: `engine/egress.go:310` (`SetNetworkPolicy`), and `store.Record.NetworkRules` at create;
  - port dial: `engine/portdial.go:28`;
  - checkpoints: `engine/lifecycle_checkpoints.go:73`;
  - lifecycle policy and deadline: `engine/lifecycle_rules.go:64,88`;
  - per-record front-end metadata: `Record.Ext`.
- Rough total to reach E2B/Daytona-level lifecycle parity, without sidecars, exit snapshots or CIDR:
  **about 10-14 dev-days**.

## Table

| Method / feature | RPC(s) (service) | Status today | Effort |
|---|---|---|---|
| `App.lookup(name, create_if_missing, environment_name)` | `AppGetOrCreate` (MC) | done (`control.go:41`) | - |
| Environments (`MODAL_ENVIRONMENT`, `environment_name=`) | `EnvironmentGetOrCreate` (MC) | done; envs only namespace app names (`control.go:97`) | - |
| `with app.run():` ephemeral app + sandboxes | `AppCreate`, `AppPublish`, `AppHeartbeat` (loop), `AppClientDisconnect`, (+`AppGetLogs` stream if output enabled) (MC) | missing | M |
| Input-plane token | `AuthTokenGet` (MC) | done (`control.go:103`) | - |
| `modal token set` verify / `Client.hello` | `ClientHello` (MC) | missing (not on sandbox path) | S |
| `Sandbox.create` V2 (default) | `AuthTokenGet`, `SandboxCreateV2` (MC) | done for a subset; many fields ignored silently (below) | see per-param |
| `Sandbox.create` V1 (`MODAL_SANDBOX_V2=0`, `gpu=`, NFS, deprecated `pty_info=`) | `SandboxCreate`, then poll `SandboxGetTaskId` (MC) | done; `env=` broken (`SecretGetOrCreate`) | S |
| create `name=` / `Sandbox.from_name` | create field + `SandboxGetFromNameV2`, then V1 `SandboxGetFromName` on NotFound (MC) | missing (create refuses `name`, `control.go:247`) | M |
| `_experimental_set_name` | `SandboxSetName` (MC) | missing | S (with name store) |
| create `tags=` / `set_tags` / `get_tags` | create field, `SandboxTagsSet[V2]`, `SandboxTagsGet[V2]` (MC) | missing (create refuses tags, `control.go:249`) | S-M |
| `Sandbox.list(app_id, tags)` | `SandboxListV2` (V2 cfg), `SandboxList` (V1 cfg) (MC), paged | missing | M |
| `from_id` | `SandboxWait[V2]` timeout=0 (MC) | done | - |
| `wait(raise_on_termination)` / `poll` / `returncode` | `SandboxWait[V2]` (timeout 10 / 0) | done | - |
| `terminate(wait=)` | `SandboxTerminate[V2]` (+ wait loop) | done (wait=True untested) | S (test) |
| `detach()` | none (client-side; closes router channel) | done (nothing to do) | - |
| `wait_until_ready` + `readiness_probe=` | router `SandboxWaitUntilReady` (TCR) | missing; probe ignored silently | M |
| `idle_timeout=` | create field; result `GENERIC_STATUS_IDLE_TIMEOUT`? | ignored silently | M |
| `block_network` / `outbound_domain_allowlist` | create field `network_access` | ignored silently (**open egress**) | S-M |
| `outbound_cidr_allowlist` (and deprecated `cidr_allowlist`) | create field | ignored silently | M-L (netpolicy has no CIDRs) |
| `_experimental_set_outbound_network_policy` | router `TaskSetNetworkAccess` (TCR) | missing | S (after the above) |
| `_experimental_outbound_policy` / `_experimental_update_outbound_policy` | create field / router `TaskSetOutboundPolicy` | ignored / missing | L (refuse) |
| `inbound_cidr_allowlist` | create field | ignored silently | S (enforce at ingress proxy; tunnels owner) |
| `cpu=(req, limit)`, `memory=(req, limit)` | `resources.milli_cpu[_max]`, `memory_mb[_max]` | request only; limit ignored | S |
| `gpu=` | forces V1; `resources.gpu_config` | refused (UNIMPLEMENTED) | keep refusing |
| `region=`, `cloud=`, `runtime=`, `verbose=` | `scheduler_placement`, `cloud_provider_str`, `runtime`, `verbose` | ignored | S (store/report) |
| `custom_domain=`, `proxy=`, `include_oidc_identity_token=` | create fields (`proxy` loads via `ProxyGet` first) | ignored / `proxy` fails at ProxyGet | S (refuse) |
| `experimental_options=` | `experimental_options_v2` map | ignored | S (allowlist keys) |
| `create_connect_token(user_metadata, port)` | `SandboxCreateConnectToken[V2]` (MC) | missing | M |
| `sb.logs.fetch()` / `.tail()` (entrypoint logs) | `AppCountLogs`, `AppFetchLogs` (MC) | missing; entrypoint output discarded | M |
| `_experimental_get_exit_snapshot` (+ `experimental_options={"enable_exit_snapshot": True}`) | `SandboxGetExitSnapshot[V2]` long poll (MC) | missing | L |
| Sidecars: `_experimental_sidecars.create/get/list`, `SidecarContainer.wait/poll/terminate` | `SandboxContainerCreateV2` (MC, V2 default) or `TaskContainerCreate`; `TaskContainerGet/List/Wait/Terminate` (TCR) | missing | L |
| `reload_volumes` | router `TaskReloadVolumes` | missing (volumes) | L (with volumes) |
| `SandboxGetResourceUsage`, MC `SandboxWaitUntilReady`, `SandboxGetLogs` (V1 sb.stdout: stdio owner) | defined in proto, not called by 1.6.0 on these paths | n/a | - |

MC = `modal.client.ModalClient`; TCR = `modal.task_command_router.TaskCommandRouter`.

---

## Cross-cutting client behavior (applies to every item)

- **Retries and idempotency** (`modal/_utils/grpc_utils.py:198-206,605-640`):
  - Every unary call is retried up to 3 times on `DEADLINE_EXCEEDED/UNAVAILABLE/CANCELLED/INTERNAL/UNKNOWN`.
  - Every attempt carries the same `x-idempotency-key` (a UUID per logical call) and `x-retry-attempt`.
  - The default retry has no attempt timeout.
  - Problem: wisp's `create` answers `codes.Internal` on a boot failure (`control.go:~311-322`). That makes the
    client re-run a create. If a create is slow and the client cancels it, the retry can leak a duplicate sandbox.
  - **Recommendation:** for non-idempotent RPCs, remember the response by `x-idempotency-key` for a few minutes.
    The RPCs are `SandboxCreate[V2]`, `SandboxSetName`, `SandboxContainerCreateV2`, `SandboxCreateConnectToken`
    and `AppCreate`. Also use `Unavailable`/`ResourceExhausted` deliberately (S).
- **Status-to-exception map** (`modal/_grpc_client.py:27-44`):

  | gRPC status | Python exception |
  |---|---|
  | `NOT_FOUND` | `NotFoundError` |
  | `ALREADY_EXISTS` | `AlreadyExistsError` |
  | `FAILED_PRECONDITION`, `ABORTED` | `ConflictError` |
  | `INVALID_ARGUMENT`, `OUT_OF_RANGE` | `InvalidError` |
  | `RESOURCE_EXHAUSTED` | `ResourceExhaustedError` |
  | `PERMISSION_DENIED` | `PermissionDeniedError` |
  | `UNAUTHENTICATED` | `AuthError` |
  | `UNIMPLEMENTED` | `UnimplementedError` |
  | `INTERNAL` | `InternalError` (retried) |

- **V1 vs V2 routing is by ID shape** (`modal/sandbox.py:237-253`). V1 is `sb-` plus exactly 22 `[0-9A-Za-z]`
  characters; anything else is V2. Every per-sandbox method picks `...V2` and the `x-modal-auth-token` header from
  `self._is_v2`. Wisp mints `sb-`+24 for V2 and `sb-`+22 for V1 (`frontend/modal/meta.go` `newSandboxID`). Keep
  that. Wisp never verifies `x-modal-auth-token`; it checks the token secret on every call anyway (`modal.go:200-212`).
- **Create path selection** (`modal/sandbox.py:853`): V2 when `config sandbox_v2` is true (the default) **and**
  `gpu is None`, `not network_file_systems` and `pty_info is None`.
  - The pty test is on the *deprecated* `pty_info` param only. `pty=True` stays on V2 and sends
    `definition.pty_info` (`:1066-1068`). So `docs/providers/modal.md` §1 "V2 unless ... pty" is slightly wrong.
  - Wisp refuses PTY sandboxes (`control.go:235`); that belongs to the stdio owner.
- **Result to returncode** (`modal/sandbox.py:256-263`): `UNSPECIFIED` gives None, `TIMEOUT` gives 124,
  `TERMINATED` gives 137, and anything else gives `result.exitcode`. So `IDLE_TIMEOUT` (7), `INIT_FAILURE` (5) and
  `INTERNAL_FAILURE` (6) all return `exitcode`. Wisp sets -1 for `INIT_FAILURE` (`control.go:380`).

---

## Per-item notes

### 1. `App.lookup` and environments — done
- Client: `modal/app.py:340-389`.
  - Sends `AppGetOrCreateRequest{app_name, environment_name=_get_environment_name() ("" when unset), object_creation_type=CREATE_IF_MISSING|UNSPECIFIED}`.
  - Reads `app_id` and `handle_metadata` (into `AppInfo._from_proto`).
  - The client checks names first: `check_object_name` (`modal/_utils/name_utils.py:60`) allows alnum, `-._` and
    fewer than 64 characters.
- Wisp: `control.go:41-80`. It gives NotFound without create, AlreadyExists for `CREATE_FAIL_IF_EXISTS`, and
  `ap-`+22 IDs. Apps are never stopped or listed (`docs/providers/modal-differences.md`).
- Environment: `EnvironmentGetOrCreate{deployment_name}` returns `environment_id`, `metadata.name` and
  `settings.image_builder_version`. This is done (`control.go:97`). An `""` environment maps to `main`
  (`control.go:29-36`). That mapping must be applied consistently in from_name, list and SandboxTagsSet(V1), all of
  which send `environment_name`.

### 2. `with app.run():` (ephemeral app), missing, M
- This is the other common way to get an `app` for `Sandbox.create`. Client: `modal/runner.py:369-500`.
- Calls:
  - `AppCreate{description, environment_name, app_state=EPHEMERAL|DETACHED, tags}`, which reads `app_id`,
    `app_page_url` and `app_logs_url` (`runner.py:88-112`);
  - `EnvironmentGetOrCreate`;
  - `AppPublish{app_id, name, app_state, function_ids={}, ...}`, which reads `url` and `server_warnings`
    (`runner.py:294-308`);
  - `AppHeartbeat{app_id}`, every `HEARTBEAT_INTERVAL` (`runner.py:61-66,436-438`);
  - `AppClientDisconnect{app_id, reason, exception}` at exit (`runner.py:329-341`);
  - with `modal.enable_output()`, also a streaming `AppGetLogs`.
- Semantics to implement: when an EPHEMERAL app disconnects, or misses heartbeats, it stops and its sandboxes are
  **terminated**. UNVERIFIED: Modal's documented behavior is that sandboxes die with the ephemeral app; the
  heartbeat-miss timeout is unknown.
- Wisp mapping: an app record with a state, a heartbeat watchdog, and `end()` (`control.go:472`) for each of its
  sandboxes.

### 3. Auth, tokens and workspace
- `AuthTokenGet` is done (`control.go:103`).
  - The client decodes only `exp`, refreshes at 50-60% of the lifetime, and retries 3x on the first fetch
    (`modal/_utils/auth_token_manager.py:117-153`).
  - An empty token raises `ExecutionError`.
  - A missing `exp` falls back to 20 minutes.
- `ClientHello` (`modal/client.py:230-241`) is called only by `Client.verify` and `hello`, i.e. `modal token set`
  and `modal token new`. Not on the sandbox path. Optional, S: an empty response; `server_warnings` gets printed.
- `WorkspaceNameLookup` (`modal/config.py:233`, `modal/_workspace.py:102`) is CLI profile display only. Skip.
- Credentials are required client-side (`modal/client.py:313-342`). Wisp checks the secret against daemon keys
  and ignores the ID (`modal.go:200-212`).

### 4. `Sandbox.create`, per parameter (public signature `modal/sandbox.py:626-665`)
V2 builds its definition at `modal/sandbox.py:1130-1171` and V1 at `:557-597`. Wisp's create is
`control.go:197-330`.

| Param | Wire | Wisp today | Proposed |
|---|---|---|---|
| `*args` | `entrypoint_args` (client caps total at 64 KiB, `:266-275`) | runs as root exec; sandbox ends on exit (`control.go:333-353`) | done; output must be kept for logs/stdout (see 10) |
| `app` | `app_id` (required; else `InvalidError` client-side, `:384-414`) | NotFound if unknown | done |
| `name` | `definition.name` (optional) | refused | see 5 |
| `tags` | `tags[]` on request | refused | see 6 |
| `image` | `image_id` (+ `mount_ids` for `add_local_*` layers) | debian_slim only | image owner |
| `env` | V2: `ephemeral_secrets` StringMap (`:1048-1057,1168`); V1: `Secret.from_dict`, then `SecretGetOrCreate`, then `secret_ids` | V2 works; **V1 fails with UnimplementedError** | implement `SecretGetOrCreate` for `ANONYMOUS_OWNED_BY_APP`/`EPHEMERAL` with `env_dict` (store env and return `st-` id), and accept those `secret_ids` in create (S). Named `Secret.from_name` stays refused unless a secret store is wanted (decision) |
| `secrets` | V2: from_dict secrets are folded into `ephemeral_secrets` client-side (`modal/secret.py:589-604`); named ones are loaded (`SecretGetOrCreate`) and go in `secret_ids` | named refused (`control.go:241`) | as above |
| `network_file_systems` | forces V1, `nfs_mounts` | refused | keep refused |
| `timeout` (default 300) | `timeout_secs` uint32 | done; max 24 h, then INVALID_ARGUMENT (`control.go:257-263`) | done |
| `idle_timeout` | `idle_timeout_secs` (optional) | **ignored** | see 9 |
| `workdir` | `workdir` (client requires absolute, `:1022`) | done (`env -C`; a missing dir fails at exec time) | UNVERIFIED whether hosted creates it |
| `gpu` | forces V1, `resources.gpu_config` | refused | keep refused |
| `cloud`, `region` | `cloud_provider_str`, `scheduler_placement.regions` | ignored | store and report in `SandboxInfo.regions`; refuse unknown? (decision) |
| `cpu` float or (req, limit) | `milli_cpu`, `milli_cpu_max` (`modal/_resources.py:18-33`; client checks limit >= req) | `ceil(milli_cpu/1000)` vCPUs; max ignored (`control.go:268-271`) | vCPU count *is* a hard limit, so use `ceil(max(req, limit)/1000)` (decision) |
| `memory` int or (req, limit) | `memory_mb`, `memory_mb_max` (`_resources.py:34-43`) | RAM = `memory_mb`; max ignored | RAM = `max(req, limit)`; optionally `ResourcesPolicy.Memory.LimitMB` (`engine/lifecycle_policy.go:39-50`) = limit (decision) |
| `runtime` (`"gvisor"`/`"vm"`, `modal/types.py:59`) | `runtime` | ignored | accept `"vm"`/None; refuse `"gvisor"`? (decision) |
| `block_network` | `network_access{BLOCKED}` (V2 `:1076-1085`, V1 `:288-293`) | **ignored: egress stays open** | see 8 |
| `outbound_cidr_allowlist` / deprecated `cidr_allowlist` | `network_access{ALLOWLIST, allowed_cidrs}` | ignored | see 8 |
| `outbound_domain_allowlist` | `network_access{ALLOWLIST, allowed_domains}` (wildcard `*.` and bare `*`) | ignored | see 8 |
| `_experimental_outbound_policy` | `outbound_policy` (+ secrets loaded) | ignored (secrets would fail first) | refuse (UNIMPLEMENTED) |
| `inbound_cidr_allowlist` | `inbound_cidr_allowlist[]` (client rejects with `block_network`) | ignored | enforce in the tunnel/connect-token proxy (tunnels owner) |
| `volumes` (Volume / CloudBucketMount) | `volume_mounts`, `cloud_bucket_mounts` (+ `cloud_bucket_mount_credentials`) | refused (`control.go:243`, `:202`) | volumes owner |
| `pty` | V2: `pty_info` | refused | stdio owner |
| `encrypted_ports` / `h2_ports` / `unencrypted_ports` | `open_ports` (client rejects with `block_network`) | refused | tunnels owner |
| `custom_domain` | `custom_domain` | ignored | refuse non-empty (S) |
| `proxy` | `proxy_id`, after `ProxyGet` load | fails at ProxyGet (UNIMPLEMENTED) | keep |
| `include_oidc_identity_token` | bool, so the guest gets `MODAL_IDENTITY_TOKEN` | ignored | refuse true (S), or mint a wisp JWT (decision) |
| `readiness_probe` | `Probe{tcp_port \| exec_command.argv, interval_ms}` | ignored | see 7 |
| `verbose` | bool | ignored | fine |
| `experimental_options` | `experimental_options_v2` map<string,string> (values `str()`-ed) | ignored | known keys: `enable_exit_snapshot` (`:1599`), `vm_sidecar_memory_reserve_mib` (`:3151-3152`); refuse unknown keys or warn via `x-modal-warning` (decision) |
| `_experimental_enable_snapshot` | `enable_snapshot` | ignored | snapshots owner |
| `client`, `environment_name` (deprecated, ignored by the server: env comes from the app), `pty_info` (deprecated, forces V1) | - | - | - |
| `i6pn` (only `_experimental_create`, not public `create`) | `i6pn_enabled` | ignored | refuse true |
| `nonpreemptible` | **not a 1.6.0 `Sandbox.create` param.** `SchedulerPlacement.nonpreemptible` exists in the proto but sandboxes never set it | - | - |

- The V2 response is read at `modal/sandbox.py:1179-1195`. The client takes:
  - `sandbox_id`;
  - `task_id`, which is cached, so `_get_task_id` never polls;
  - `command_router_access`, checked with `HasField`;
  - `metadata` (`app_id`, and `result` if present);
  - `tunnels`, but only when `len(tunnels) == len(open_ports)`.
- V1 (`:599-617`) hydrates and then polls `SandboxGetTaskId` every 0.5 s for up to 21 minutes
  (`_SANDBOX_SCHEDULING_TIMEOUT`, `:99`).
  - `INTERNAL`, `UNAVAILABLE`, `DEADLINE_EXCEEDED` and `ConnectionError` count as transient (`:2102-2116`).
  - A `task_result` with no `task_id` raises `ConflictError(task_result.exception)` (`:2117-2119`).
  - Any other failure makes the client call `terminate()` and re-raise. A timeout becomes
    `ResourceExhaustedError("Insufficient capacity")`.
  - Wisp answers `task_id` at once, which is fine.
- **Proposed rule:** the create gate (`control.go:235-250`) should refuse every field it does not honor. A
  sandbox should never silently get *more* permission than asked for. Add an explicit check per field (S).

### 5. Names: `name=`, `Sandbox.from_name`, `_experimental_set_name`, missing, M
- `from_name(app_name, name, environment_name=None)` is at `modal/sandbox.py:1346-1384`.
  - When `sandbox_v2` is set it first calls `SandboxGetFromNameV2{sandbox_name, app_name, environment_name}`
    with `x-modal-auth-token` (`:1387-1419`).
  - **Only `NotFoundError` falls through** to V1 `SandboxGetFromName`. So UNIMPLEMENTED on V2 breaks from_name
    outright. Implement both; they share one handler.
  - It reads `sandbox_id` and `metadata`, which become `_new_hydrated(...)`.
  - The docstring says "NotFoundError if no running sandbox exists with the given name". So only running
    sandboxes match.
- Create with `name`:
  - "Unique within an app". The docstring says `AlreadyExistsError` if one with the same name already exists, so
    return `ALREADY_EXISTS`.
  - The client checks the name with `check_object_name`.
- `_experimental_set_name(name)` sends `SandboxSetName{sandbox_id, name}` and is V2 only (`:1506-1531`).
  Documented semantics:
  - set once; the same name again is idempotent success;
  - another running sandbox in the app holds the name: `AlreadyExistsError` (ALREADY_EXISTS);
  - this sandbox already has a different name, or is not running: `ConflictError` (FAILED_PRECONDITION).
- Wisp: keep `Name` in `meta` (`Record.Ext["modal"]`, `meta.go`). Also keep a name index
  `(env, app_id, name) -> sandbox_id` in `state.json` (`state.go`), cleared in `state.finish`.
  - The engine's name namespace `store.GetByName(api, name)` (`internal/store/store.go:277`) is per API, not per
    app, so don't use it.
  - Precedent: daytona and e2b keep their names in Ext as well.
- UNVERIFIED: whether a finished sandbox's name is freed immediately (assume yes, given the docstring's
  "running"); and the exact error texts.

### 6. Tags: create `tags=`, `set_tags`, `get_tags`, missing, S-M
- `get_tags` sends `SandboxTagsGet[V2]{sandbox_id}` and reads `tags[] {tag_name, tag_value}` (`:1457-1472`).
- `set_tags`, at `:1474-1504`:
  - V2 sends `SandboxTagsSetV2{sandbox_id, tags}`.
  - V1 sends `SandboxTagsSet{environment_name, sandbox_id, tags}`.
  - It **replaces the whole set**; `{}` clears it. The response is ignored.
- Create sends `tags[]` on the request, not in the definition (`:593-597`, `:1164-1171`).
- Used by list filtering (7). Store the tags in `meta.Tags` and mirror them into `sandboxState`, so a finished
  sandbox can still answer `get_tags`.
- UNVERIFIED:
  - the limits on tag count and key/value length;
  - whether tags on a finished sandbox can be read or set. Recommend: get works, set gives FAILED_PRECONDITION.
- Precedent for filter semantics: e2b `metadata` filtering (`frontend/e2b/control.go:612-640`).

### 7. `Sandbox.list(app_id=None, tags=None)`, missing, M
- Client: `modal/sandbox.py:2601-2710`.
  - With `sandbox_v2` (the default) it calls `SandboxListV2` (with `x-modal-auth-token`). Otherwise it calls
    `SandboxList`.
  - Request: `{app_id or "", before_timestamp, environment_name, include_finished=False, tags}`.
  - Under V2, `environment_name` is sent only when `app_id` is empty, and the client warns that listing without
    an `app_id` is deprecated.
- Paging:
  - The client loops until `resp.sandboxes` is empty.
  - The next page uses `before_timestamp = sandboxes[-1].created_at`, a float in epoch seconds.
  - The server **must** return only `created_at < before_timestamp` when it is non-zero, newest first. Otherwise
    the client loops forever.
  - Make `created_at` unique per sandbox, for example by nudging ties by 1 microsecond, or ties drop entries.
  - Page size: 50? UNVERIFIED.
- The client reads `id`, `metadata` (passed to `_new_hydrated`) and `task_info.result` (as `_result`).
  `metadata.app_id` **must be filled**. Otherwise `sb.logs` on a listed sandbox raises
  `ExecutionError("app_id should have been set")` (`:2712-2722`).
- Also fill `created_at`, `app_id`, `tags`, `name`, `image_id`, `timeout_secs`, `idle_timeout_secs`,
  `readiness_probe`, `ready_at`, `regions` and `task_info{id, started_at}` for fidelity. The client ignores them.
- Filter: "at least these tags", a subset match. `include_finished` is always false from 1.6.0, but support it
  from the state file anyway (cheap).
- Wisp source: `store.Records()` (`internal/store/store.go:362`), filtered to `API=="modal"` and joined with
  `meta`. Precedent: e2b list with cursor and filters (`frontend/e2b/control.go:600-700`).
- UNVERIFIED:
  - The V2 `_experimental_create` docstring says "V2 sandboxes ... are not currently returned by
    `Sandbox.list()`" (`:1012-1013`), but `_experimental_list` says it lists both. Hosted behavior is unclear.
    Wisp should list both.

### 8. Network: `block_network`, outbound allowlists, live policy change; S-M (domains), M-L (CIDR)
- **Today:** `def.NetworkAccess` is never read, so `block_network=True` sandboxes have open egress. When the
  daemon runs with `--net=false` (the e2e gate) nothing has a network anyway, which hides the gap.
- Mapping onto `internal/netpolicy` (domain-only allowlist with DNS and a transparent proxy;
  `internal/netpolicy/rules.go:24-60`):

  | Modal `NetworkAccess` | wisp `NetworkRules` | Notes |
  |---|---|---|
  | `OPEN` (or `UNSPECIFIED`) | no rules | |
  | `BLOCKED` | `[{domain:"*", action:"deny"}]` | restrictive per `Compile`. Check that direct-IP TCP is refused too; the proxy only connects to remembered addresses (package doc) |
  | `ALLOWLIST` + domains | `[{domain:d, action:"allow"}...]` | `*.x` and `*` are supported natively |
  | `ALLOWLIST` + CIDRs | no equivalent | netpolicy has no CIDR rules |

  - For CIDRs: either refuse with UNIMPLEMENTED (S), or add a CIDR allow-set to the enforcer/proxy and the nft
    divert (M-L).
  - `outbound_cidr_allowlist=[]` is documented to mean "no external egress" (`:1031-1033`). That maps to
    `BLOCKED`-like behavior with an empty CIDR list.
- Set `Record.NetworkRules` in the `store.Sprite` passed to `engine.Create`. Boot admission then insists the
  helper can enforce it (`engine/egress.go:215`). Map `engine.ErrUnenforceable` to FAILED_PRECONDITION with a
  clear message.
- `_experimental_set_outbound_network_policy` is at `modal/sandbox.py:1533-1563`.
  - It calls router `TaskSetNetworkAccess{task_id, network_access}`, either `ALLOWLIST` or `OPEN`. It cannot
    re-block.
  - Wisp: `engine.SetNetworkPolicy(id, rules, policy)` (`engine/egress.go:310`), which is live.
  - The client doc says "Established connections that the new policy no longer permits are terminated".
    UNVERIFIED that wisp's proxy kills existing flows; probably not. Document it, or implement (S-M).
- `_experimental_update_outbound_policy` sends router `TaskSetOutboundPolicy`. Refuse it.
- Semantic difference (UNVERIFIED): Modal's domain allowlist appears to be TLS-SNI on port 443. The sidecar
  docstring says "restrict the sidecar's outbound TLS connections (port 443) to these SNI domains"
  (`:3148-3149`). Wisp's is DNS-based on all ports. Document it.

### 9. `idle_timeout`, missing (silently ignored), M
- The wire is `definition.idle_timeout_secs` (optional uint32). The client has no idle-specific handling.
  `wait()` raises only for `TIMEOUT` and `TERMINATED`, so an idle-ended sandbox just returns, with `returncode`
  equal to `result.exitcode`.
- UNVERIFIED, all hosted:
  - what counts as activity. Modal docs say roughly "no active exec, no stdin writes, no open tunnel connections";
    the entrypoint itself does not count;
  - which status it ends with (`GENERIC_STATUS_IDLE_TIMEOUT`=7 exists);
  - the exit code (0?).
- The engine's idle watcher can't be used as-is:
  - it suspends or stops, but cannot delete (`internal/store/policy.go:56-70`);
  - and the entrypoint exec holds the VM busy forever, because `agentExec` acquires the VM for the command's
    life (`frontend/modal/exec.go:59`; `engine/lifecycle.go:130-150,701-760`).
- So implement it in the front end. Track the last activity per sandbox: exec start/end (`router.go:98`), stdin
  writes, and tunnel/connect-token proxied connections. A timer then ends the sandbox with result
  `IDLE_TIMEOUT` and calls `end()`.
- Decision: `IDLE_TIMEOUT` vs `TERMINATED` as the result status.

### 10. `wait_until_ready` + `readiness_probe`, missing, M
- Client: `modal/sandbox.py:1894-1926`.
  - `timeout <= 0` raises `InvalidError` client-side.
  - Then `_get_task_id(raise_if_task_complete=True)`. A finished sandbox gives `ConflictError`, which wisp's
    `SandboxGetTaskId` already produces (`control.go:485-495`).
  - Then the router is resolved. `NotFoundError` there is re-raised as `ConflictError`.
  - Finally router `SandboxWaitUntilReady{task_id, timeout(float)}`, which returns `{ready_at(double)}`.
- The router call goes through `_unary_call_with_deadline` (`modal/_utils/task_command_router_client.py:789-799,1151-1177`).
  - It retries transient errors until the deadline.
  - `DEADLINE_EXCEEDED`, or the deadline passing, raises `modal.exception.TimeoutError("Timeout expired")`.
  - The server should block until ready or until `timeout`. On timeout it returns `DEADLINE_EXCEEDED`, which is
    excluded from retry.
- Wisp: on create, store the `Probe` in `meta` and run a probe loop at `interval_ms` (default 100) from boot.
  - `tcp_port`: `engine.DialPort(ctx, m, port)` (`engine/portdial.go:28`); success when it connects.
  - `exec_command.argv`: `f.run` (agent exec as root, `exec.go:59`); success on exit 0.
  - Record `ready_at` and wake the waiters. Also report it in `SandboxInfo.ready_at`.
- UNVERIFIED:
  - the error when no probe was configured. Suggest FAILED_PRECONDITION: "sandbox has no readiness probe";
  - whether the probe time counts against an exec probe's own timeout;
  - whether the sandbox ends while probing (no result on hosted?).

### 11. `from_id`, `wait`, `poll`, `returncode`, `terminate`, `detach` (done, small follow-ups)
- `from_id` (`:1422-1455`) sends `SandboxWait[V2]{sandbox_id, timeout=0}`.
  - It reads `metadata`, which goes to `_new_hydrated` and sets `_app_id` for logs.
  - It sets `result` only if `status != 0`.
  - An unknown ID gives NOT_FOUND, then `NotFoundError`.
  - Wisp: `control.go:530-548` fills both `result` and `metadata{app_id, result}`.
- `wait(raise_on_termination=True)` (`:1866-1892`) loops `SandboxWait[V2]{timeout=10}` with no client deadline.
  `TIMEOUT` raises `SandboxTimeoutError`; `TERMINATED` raises `SandboxTerminatedError` only when raising on
  termination. Wisp holds each call for up to `min(timeout, 50s)` (`control.go:526-548`).
- `poll()` (`:2067-2086`) is `SandboxWait{timeout=0}`.
- `terminate(wait=False)` (`:2040-2065`) sends `SandboxTerminate[V2]{sandbox_id}`.
  - The response's `existing_result` is ignored by the client.
  - With `wait=True` it then runs `wait(raise_on_termination=False)` and returns `returncode`: 137 for a
    terminate, or the earlier result if the sandbox had already finished.
  - Wisp ends synchronously, deleting the VM (`control.go:556-571`). Add an e2e check for `wait=True` (S).
- `detach()` (`:1305-1326`) is client-only. It closes the router channel, and later calls raise `ClientClosed`.
  `wait()` and the exit-snapshot lookup still work, because they use the private `__client`. No server work.
- `returncode` is a client-side mapping (Cross-cutting).

### 12. `create_connect_token(user_metadata=None, port=8080)`, missing, M
- Client: `modal/sandbox.py:1967-2003`.
  - Dict metadata is JSON-dumped.
  - The client validates the port (1-65535).
  - Request: `SandboxCreateConnectToken[V2]{sandbox_id, user_metadata, port}`.
  - It reads `url` and `token` into `SandboxConnectCredentials`.
- Semantics come from the docstring only: an HTTP proxy routes requests carrying the token to `port`, adding
  `user_metadata` to the forwarded request headers.
- UNVERIFIED:
  - how the token is presented (Modal docs show `Authorization: Bearer <token>`);
  - the header name the metadata is forwarded under;
  - URL shape and expiry;
  - whether one URL serves all tokens.
- Wisp mapping: a token table `token -> (sandbox, port, metadata, expiry)` and an HTTP proxy on the Modal listener
  or a `--modal-connect-url` host. It dials the guest with `engine.DialPort` and must enforce
  `inbound_cidr_allowlist`. The daytona preview proxy is the template (`frontend/daytona/preview.go:55-120`).
  Coordinate with the tunnels owner; it is the same ingress plumbing.

### 13. Entrypoint logs: `sb.logs.fetch/tail`, missing, M
- `get_logs` does not exist in 1.6.0. The `sb.logs` property (`:2724-2740`) returns `_SandboxLogsManager`
  (`modal/_logs_manager.py:807-870`).
- Query data (`:2712-2722`) needs `_app_id` (from hydrate metadata) and `task_id`. The task id is resolved once
  through `SandboxGetTaskId[V2]`; no task id gives `ExecutionError` (`:2127-2146`). Filters are
  `LogsFilters(task_id=...)`.
- `fetch(since, until, source, search_text)` (`modal/_logs.py:399-503`):
  1. `AppCountLogs{app_id, since, until, bucket_secs, source, task_id, search_text, ...}` returns
     `buckets[]{bucket_start_at, stdout_logs, stderr_logs, system_logs}`.
  2. The client refines ranges, then calls `AppFetchLogs{app_id, since, until, limit, source, task_id, ...}`,
     which returns `batches[]{task_id, items[]{data, timestamp | timestamp_ns, file_descriptor, container_id}}`.
  3. The range is at most `_MAX_FETCH_RANGE` (days), and each fetch is capped at `_FETCH_LIMIT`.
- `tail(n)` calls only `AppFetchLogs`, widening the lookback (`modal/_logs.py:318-396`).
- Wisp today *discards* the entrypoint's output (`docs/providers/modal-differences.md`). Keep a bounded,
  timestamped ring per sandbox in the front end, persisted or not (decision). It would serve `AppFetchLogs`, a
  trivial `AppCountLogs` (one bucket with counts), and also V2 `sb.stdout` (`SandboxStdioReadV2`, the stdio
  owner). Share one buffer.
- Logs come from the entrypoint only, not execs (docstring).
- UNVERIFIED: retention after the sandbox ends; `system` (`FILE_DESCRIPTOR_INFO`) entries; `search_text` semantics.

### 14. Exit snapshot: `_experimental_get_exit_snapshot(timeout=None)`, missing, L
- Opt-in through `experimental_options={"enable_exit_snapshot": True}`, which arrives as the string `"True"` in
  `experimental_options_v2`.
- Client: `:1593-1676`. It loops `SandboxGetExitSnapshot[V2]{sandbox_id, timeout<=10}` as a long poll, with an
  attempt timeout of `timeout+5` and `total_timeout=remaining`.
  - `success.image_id` returns `Image` (an empty id raises `InternalError`).
  - `error{ERROR_CODE_TIMEOUT|FILESYSTEM_INCONSISTENT}` raises `SnapshotCreationError`; other codes raise
    `InternalError`.
  - `pending` re-polls.
  - A missing outcome raises `InternalError`.
  - The docstring says not-enabled raises `InvalidError`, so the server sends INVALID_ARGUMENT (UNVERIFIED).
- Wisp: on every end path (entrypoint exit, terminate, timeout, idle) take
  `engine.CreateCheckpoint(rec, ...)` (`engine/lifecycle_checkpoints.go:73`) **before** `life.Delete`.
  - Problem: the timeout path is currently the engine's own deadline delete (`DeadlineDelete`, `control.go:293`).
    That fires `OnDelete` *after* the disk is gone. The front end would need its own deadline timer, or a
    pre-delete hook in the engine. The engine is unchanged so far (decision).
  - The checkpoint must then become an `im-...` usable as `Sandbox.create(image=)` or `mount_image`. That is the
    snapshots owner's image registry.

### 15. Sidecar containers: `_experimental_sidecars`, missing, L
- Create (`:3110-3285`):
  - On V2 (default `use_control_plane_sidecar_create=True`, `modal/config.py:391`) it calls MC
    `SandboxContainerCreateV2{sandbox_id, container_name, definition{entrypoint_args, image_id, secret_ids, workdir, volume_mounts, cloud_bucket_mounts, network_access, pty_info, resources.memory_mb}, ephemeral_secrets, cloud_bucket_mount_credentials}`.
  - On V1, or with `MODAL_USE_CONTROL_PLANE_SIDECAR_CREATE=0`, it calls TCR
    `TaskContainerCreate{task_id, container_name, image_id, args, env, workdir, secret_ids, volume_mounts, network_access, pty_info, memory_reserve_consume_mib}`.
  - Both read `{container_id, container_name}`.
  - The name `"main"` is reserved, and the image must already be hydrated (built/`from_id`/snapshot).
- TCR follow-ups:
  - `get` sends `TaskContainerGet{task_id, container_name, include_terminated}` and reads
    `container{container_id, container_name, status(string), result}`;
  - `list` sends `TaskContainerList{task_id, include_terminated}`, and the client drops `"main"`;
  - `wait` loops `TaskContainerWait{task_id, container_id, timeout=10}` and reads `result`;
  - `poll` is the same with `timeout=0`;
  - `terminate` sends `TaskContainerTerminate`.
  - `exec`, filesystem, mount and snapshot calls take `container_id` (other owners).
- Wisp: one Firecracker VM with one rootfs. A sidecar would be a second image's disk attached to the VM (like
  `engine.MountCheckpoint`, `engine/lifecycle_mounts.go:49`). It would run in its own mount namespace (pivot_root
  or chroot) and maybe its own netns ("a fully network-blocked sidecar ... would have no IP", `:3133-3134`). That
  needs new wisp-agent support.
- Recommend UNIMPLEMENTED until someone asks.
- UNVERIFIED: the `status` string values; whether sidecars share the network namespace with main; their lifecycle
  when main exits.

### 16. Sandbox stdout/stderr of the entrypoint (pointer only; stdio owner)
- V2 `sb.stdout`/`stderr`/`stdin` go through router `SandboxStdioReadV2`/`SandboxStdinWriteV2`
  (`modal/sandbox.py:1255-1291`).
- V1 uses MC `SandboxGetLogs` and `SandboxStdinWrite` (`modal/io_streams.py:43,712`).
- Both need the entrypoint's output buffered, which today it is not. Share with item 13.

---

## Decisions for the owner

1. **Refuse vs ignore** for create fields wisp cannot honor. Recommendation: refuse anything that would grant
   more than was asked for (`block_network`, allowlists, `inbound_cidr_allowlist`), or that changes semantics
   (`idle_timeout`, `readiness_probe`, `custom_domain`, `include_oidc_identity_token`, `gvisor` runtime,
   `i6pn`).
   - Accept and report placement hints (`region`, `cloud`, `verbose`).
   - For unknown `experimental_options` keys, warn via `x-modal-warning` trailing metadata
     (`docs/providers/modal.md` §1). The client prints it.
2. **CPU and memory limits.** Should the VM size be the request or the limit? A Firecracker vCPU count and RAM
   size are hard limits, so the recommendation is `max(request, limit)`. The alternative is RAM = limit plus a
   guest cgroup at the request, which inverts Modal's meaning.
3. **CIDR egress.** Extend `internal/netpolicy` and the nft divert (M-L), or refuse `outbound_cidr_allowlist`
   (S)? `outbound_cidr_allowlist=[]` can map to "block" either way.
4. **Idle timeout:**
   - which result status to report (`IDLE_TIMEOUT` vs `TERMINATED`);
   - which activities count. This needs a front-end activity tracker, because the engine's idle rule can't
     delete and sees the entrypoint as busy.
5. **Engine change or front-end timer for pre-delete hooks.** Exit snapshots, and arguably logs retention, need
   to act *before* the deadline delete. Either add an engine pre-delete or deadline callback (the engine is
   untouched so far), or move the deadline into the front end. The second gives up the engine's restart-safe
   lease.
6. **Secrets:**
   - Implement `SecretGetOrCreate` for ephemeral and app-owned `from_dict` secrets only (S, which fixes V1
     `env=`)?
   - Or also a named secret store (`Secret.from_name`), which is a new persistent object type (M)?
7. **Ephemeral apps (`app.run()`):** whether to implement them, and whether app stop/heartbeat-loss terminates
   the app's sandboxes (recommended: yes).
8. **Sidecars:** whether to build them at all (L, needs wisp-agent work), or stay UNIMPLEMENTED.
9. **Persistence:** names, tags and logs for *finished* sandboxes in `state.json` (it is rewritten whole on every
   change, `state.go:update`). That is fine for names and tags; logs should go in separate files with a size cap.
10. **Idempotency:** honor `x-idempotency-key` on non-idempotent creates (recommended), given the client retries
    `INTERNAL`.

## Unverifiable without hosted Modal (UNVERIFIED)

- Sandbox names:
  - whether a finished sandbox's name is freed at once;
  - name uniqueness scope (per app, as the client docstring says; environment-wide?);
  - the exact error messages.
- `SandboxListV2`:
  - page size and ordering tie-breaks;
  - whether V2 sandboxes are listed at all (docstrings conflict, `:1012` vs `:2655`);
  - which `SandboxInfo` fields hosted fills.
- Tags: limits (count, length, charset); behavior on finished sandboxes.
- `idle_timeout`: the activity definition; the result status and exit code.
- `wait_until_ready`: the error with no probe; behavior when the entrypoint exits before ready; whether
  `ready_at` is wall time.
- Connect tokens: how the token is presented, the forwarded metadata header name, URL shape, TTL and revocation.
- Outbound domain allowlist: SNI/443-only (hosted) vs DNS/all-ports (wisp); whether live policy changes cut
  existing connections.
- Workdir that does not exist: does hosted create it or fail the sandbox?
- Ephemeral app: the heartbeat-loss timeout; whether its sandboxes are terminated (and with which status) when
  the app stops.
- Exit snapshot: error codes for not-enabled or a still-running sandbox; how long `pending` lasts.
- Sidecars: `TaskContainerInfo.status` values, networking, lifecycle coupling with main.
- Result statuses: whether hosted ever reports `INIT_FAILURE`/`INTERNAL_FAILURE` for sandboxes, and with what
  `exitcode`. Wisp uses -1 for `INIT_FAILURE`.
- Server-side validation messages and limits on `timeout` (24 h assumed), cpu and memory bounds.
