# Plan: Modal at parity with the other providers

Goal: the unmodified Modal SDKs (Python `modal` 1.6.0, Go and JS 0.11.0) work against
`sandboxd --modal-listen`, publicly at `https://modal.sandbox.inevitable.fyi`, with the same
quality bar the E2B, Vercel and Daytona front ends met: probe suites per SDK, golden traces
recorded against hosted Modal, a recording of sandboxd that matches them apart from a written
differences doc, and an off-network acceptance run. Written 2026-10-03, after a spike.

Today's state: the Phase 5 spike (`frontend/modal`, PR #48) serves 19 of the client's 277 RPCs,
enough for `Sandbox.create`, `exec`, `read`, `wait` and `terminate` on a prebuilt `debian_slim`.
It is off by default and not deployed.

## The spike

Five reports in [modal-spike/](modal-spike/), each read from the installed 1.6.0 client (and,
for images, a fake server that logged what the client really sends). File:line references in
them are to the 1.6.0 wheel and to wisp at `b3d697f`.

| Report | Covers |
|---|---|
| [lifecycle.md](modal-spike/lifecycle.md) | Every `Sandbox.create` parameter, names, tags, list, readiness, idle timeout, apps, connect tokens, sidecars, idempotency |
| [exec-fs.md](modal-spike/exec-fs.md) | Exec, stdin, PTY, sandbox-level stdio, and the full contract of `/__modal/.bin/modal-sandbox-fs-tools`, which `sb.filesystem` execs |
| [images.md](modal-spike/images.md) | All 30 Image constructors and methods, the build protocol, mounts and blobs, a builder design |
| [net-storage.md](modal-spike/net-storage.md) | Tunnels, network policy, secrets, volumes, snapshots, going public |
| [sdks-quality.md](modal-spike/sdks-quality.md) | Go/JS SDKs, real `.proto` source, the validation bar, recording against hosted, drift, licensing |

What it found that changes the picture:

- **Two silent bugs in the spike.** `create` ignores fields it can't honour, so
  `block_network=True` gets open egress (lifecycle §, net-storage §1). Exec runs with stdin on
  `/dev/null`, so `sb.exec("cat").wait()` returns at once where hosted blocks (exec-fs §4). Not
  exploitable today (Modal isn't deployed); both are fixed first.
- **Real `.proto` source exists.** modal-client tags Python releases `py/v1.6.0`
  (`a3f75167a9`); its protos match the wheel's descriptors exactly, and the Go/JS 0.11.0 SDKs use
  identical ones. The descriptor dump in `modalpb/gen.sh` can go.
- **Files are a binary, not RPCs.** `sb.filesystem` execs a Modal-private tool at
  `/__modal/.bin/modal-sandbox-fs-tools`; its 7-command JSON contract is reconstructed in
  exec-fs §3. We ship our own at that path, as we ship `sprite-env`.
- **Images are the product.** `debian_slim()` tracks the caller's Python (3.10–3.14) and nearly
  all real code chains `pip_install`/`apt_install`/`run_commands`. Each chained call is a layer,
  sent as Dockerfile text, re-sent every run, so IDs must be deterministic or nothing caches.
- **Go/JS differ from Python on the router URL.** On localhost they require `https://` and dial
  it in plaintext; Python treats `https://` as TLS. The server picks the scheme per
  `x-modal-client-type`.
- **The guest kernel has no FUSE or virtio-fs**, which rules out live volume mounts without
  kernel work; hence copy-in/copy-out.

## Decisions made (2026-10-03)

| Question | Decision |
|---|---|
| Ground truth | **Record against hosted Modal** (Starter account, golden traces), as for E2B and Vercel. You judged the ToS §1.3(b) question (sdks-quality §4) |
| SDKs | **Python 1.6.0, Go 0.11.0, JS 0.11.0** |
| Images | **Full builder**: build steps run in Firecracker builder VMs, layers committed as reflink clones; no user code on the host. Multi-stage `from_dockerfile` refused with a clear error |
| Ports | **HTTPS tunnels via Traefik** by Host, random per-port labels under `*.modal.sandbox.inevitable.fyi`. `unencrypted_ports` and non-HTTP TLS refused |
| Volumes | **V2 only, copy-in at boot / copy-out on commit and terminate**, blocks on local disk |
| Secrets | **Named store, encrypted at rest** with a key file; env and secret values moved off argv and out of the record |
| Extras in scope | **Filesystem snapshots** (incl. exit snapshots, `snapshot_directory`, `mount_image`), **CIDR egress allowlists**, **ephemeral apps and `idle_timeout`** |
| Out of scope | Sidecar containers, memory snapshots, `run_function`, Functions/Cls, GPU, NFS, cloud bucket mounts, raw TCP ports, outbound header-rewriting policy. Each answers `UNIMPLEMENTED` or a clear refusal and gets a line in the differences doc |
| Versions | **Warn** (`x-modal-warning`) for any client but Python 1.6.0 / Go and JS 0.11.0; `--modal-strict-version` refuses them. **Drift CI**: scheduled proto diff plus the probe suite on new releases. The pin moves only when you decide |

## Defaults taken (say if you disagree)

The spike reports list about 60 smaller decisions. These are the defaults this plan assumes:

- **Refuse, don't ignore.** Any create field we can't honour fails with `FAILED_PRECONDITION`
  naming it. Placement hints (`region`, `cloud`) are accepted with a warning; unknown
  `experimental_options` keys warn.
- **VM size** is `max(request, limit)` for CPU and memory (a Firecracker size is a hard limit).
- **Idempotency:** honour `x-idempotency-key` on every create, since the client retries
  `INTERNAL`.
- **`idle_timeout`** ends with `IDLE_TIMEOUT`; activity is exec, stdio and tunnel traffic,
  tracked in the front end. Ephemeral apps: app stop or heartbeat loss terminates its sandboxes.
- **Pre-delete work** (exit snapshots, final logs) gets an engine deadline callback rather than
  a front-end timer, so the engine's restart-safe lease stays the one clock.
- **fs-tools** is a separate static binary (`cmd/modal-fs-tools`), installed from the initramfs
  onto a tmpfs at `/__modal` for Modal VMs only. `mode` is full `st_mode`; reads above the
  per-stream cap fail with `FileTooLarge`, and exec output past the cap becomes an error instead
  of silent truncation.
- **Exec** always opens stdin (the agent's WebSocket, as Daytona's front end uses). Entrypoint
  stdio is kept in a bounded ring (4 MiB per stream) for 7 days after exit, like results. V1
  sandbox stdio is implemented (cheap once V2 is).
- **Builder:** builds run as root; the base image's `USER` applies to sandbox commands, as
  Docker's does. Builder VMs get open egress minus private, host and metadata ranges; 2 vCPU,
  4 GiB, 30 min timeout, two concurrent builds, counted against `--max-sprites`. Secrets enter
  the cache key by content hash and are masked in build logs. Images pulled with one caller's
  registry credentials are cached per credential, never shared. GPU builds fail. All five Python
  bases are prebuilt (about 3 GB); `images/modal` is retired. Image disks and blobs unused for 30
  days are collected; builds count in the disk guard.
- **Reflink volume required** for the builder (XFS or btrfs; `/bulk` is XFS). Without one,
  builds are refused rather than full-copied.
- **Tunnels:** a separate HTTP/1.1 Service for tunnel traffic so websockets work; gRPC on an
  `h2c` Service.
- **Domain allowlists:** Modal's `*.x` includes the apex `x` (two wisp rules).
- **Public auth:** API keys and the root token both work publicly, as on the other APIs, with
  auth-failure rate limiting added for all of them.
- **Snapshots:** images made by `snapshot_filesystem` outlive their sandbox (an engine export
  API) and follow the image GC rule. `mount_image` copies in (no slot or overlay work).
- **Proto source:** fetch `.proto` from `py/v1.6.0`; keep `check.py` against the wheel.
- **Recording proxy** in Go, reusing `modalpb`, so it also records sandboxd for regression
  diffs.

## Phases

Estimates are agent dev-days. Phases 1, 2, 3 and 4 can run in parallel after Phase 0: they touch
different code (router and agent; control plane; a new builder package; netpolicy).

### Phase 0: foundations and ground truth (about 6 days)

- **Safety fixes:** refuse what `create` can't honour; enforce `block_network` and domain
  allowlists through the record's network rules (small: the engine already does the rest);
  honour `x-idempotency-key`; make exec output past the cap an error; correct
  `docs/providers/modal.md`'s PTY claim.
- **Protos:** `gen.sh` fetches `api.proto` and `task_command_router.proto` at `py/v1.6.0`;
  widen `prune.py`'s KEEP list for everything in scope below.
- **Version policy:** warnings and `--modal-strict-version`; Go/JS checked on
  `x-modal-libmodal-version`.
- **Health:** `GET /healthz` on the Modal listener (beside gRPC), and `grpc.health.v1`. Raise
  grpc-go's 4 MiB receive limit.
- **Recording proxy:** local h2c in, TLS to `api.modal.com` out; rewrites the router URL in the
  three responses that carry it and routes router calls back by JWT; scrubs tokens.
- **Probe suites, written against hosted first:** Python (`probe.py`, extended), Go
  (`e2e/providers/modal/go`, also wisp's in-repo Go test client), JS (`probe.mjs`). One step per
  feature in the phases below, so each phase's gate is "its steps pass". Record golden traces
  into `e2e/providers/modal/golden/`.
- **Gate:** probe suites green against hosted; goldens saved; spike suite still passes; bugs
  fixed with tests.
- **Needs from you:** a Modal account and token (see below).

### Phase 1: exec, stdio, PTY, files (about 8 days)

- Exec over the agent WebSocket with stdin; `TaskExecStdinWrite`, `TaskExecStdinWriteStream`,
  `TaskExecStdinStatus`; PTY execs.
- Sandbox-level stdio: `SandboxStdioReadV2`, `SandboxStdinWriteV2`, V1 `SandboxGetLogs` and
  `SandboxStdinWrite`; entrypoint output retained; router access for finished sandboxes.
- `cmd/modal-fs-tools`: all 7 commands to the exec-fs §3 contract, watch on raw inotify and
  exiting when stdin closes.
- **Gate:** exec, stdin, PTY, stdio and filesystem steps pass on all three SDKs; recording
  matches goldens.

### Phase 2: lifecycle and management (about 10 days)

- Names, `set_name`, `from_name` (V1 and V2, since the client falls back only on NotFound),
  tags, `Sandbox.list` (strict `before_timestamp`, or the client loops).
- Readiness probes and `wait_until_ready`; `idle_timeout`; ephemeral apps (`app.run()`,
  heartbeats); connect tokens; CPU/memory sizing.
- Persistence for finished sandboxes: names and tags in `state.json`, logs in capped files
  beside it.
- **Gate:** lifecycle steps pass on all three SDKs; recording matches goldens.

### Phase 3: the image builder (about 18 days)

Following images §6, minus its phase 8:

1. Image store: recipe index, deterministic `im-` IDs, metadata, failure records, GC, disk-guard
   admission. `ImageGetOrCreate` async, `ImageJoinStreaming` (resume by `entry_id`, ~55 s
   streams), `ImageFromId`.
2. Builder VMs: parse the Dockerfile text; RUN, ENV, WORKDIR, ARG, SHELL, CMD, ENTRYPOINT, USER;
   commit each layer as a reflink clone; stream logs. `FROM` through the existing OCI image
   cache. Prebuild the five Python bases. This alone unlocks most real code.
3. Mounts and blobs: content-addressed store; `MountPutFile`, `MountGetOrCreate`,
   `MountBatchedCheckExistence`; `BlobCreate` answered **multipart only** (single-part uploads
   fail the client's host check on a public server); a signed HTTP upload endpoint on the same
   public URL. `COPY` from context files; `add_local_*(copy=True)`.
4. `COPY --from=<image>` (uv installs); runtime mounts written into the VM before the
   entrypoint.
5. `add_python`: python-build-standalone fetched with pinned sha256, served as global mounts.
6. Secrets in builds (needs Phase 5's store) and private registries (static, ECR, GCP).
7. `publish`/`from_name`, tags, `ImageBuildChainGet`.

- **Gate:** image steps (30 cases from images §2, each SDK's `from_registry`) pass; a failing
  build reports like hosted; a second run of the same recipe is a cache hit with the same ID.

### Phase 4: network and tunnels (about 8 days)

- CIDR allowlists in `internal/netpolicy` (shared by every API: prefixes, and "DNS open,
  connect gated"); runtime `TaskSetNetworkAccess` and `TaskSetOutboundPolicy` over
  `Engine.SetNetworkPolicy`; header-rewriting policy refused.
- Tunnels: `encrypted_ports` and `h2_ports` served by Host under the public wildcard with random
  per-port labels; `SandboxGetTunnelsV2` and V1.
- **Gate:** network and tunnel steps pass; the netpolicy suites for every other API still pass
  (`scripts/verify-network-policy.sh`, e2e netpolicy).

### Phase 5: secrets and volumes (about 9 days)

- Encrypted secret store; `SecretGetOrCreate`, `GetInfo`, `List`, `Update`, `Delete`; values
  delivered without argv and kept out of `Record.Ext` (also fixes the existing `env=` path and
  V1 `env=`).
- Volumes V2: block store with signed HTTP PUT/GET of 8 MiB sha256 blocks, the `Volume*2` RPCs,
  list, delete, rename; copy-in at boot, copy-out on `commit` and terminate, `reload`.
- **Gate:** secret and volume steps pass on all three SDKs.

### Phase 6: filesystem snapshots (about 6 days)

- Engine export API so a checkpoint can outlive its sandbox; `snapshot_filesystem` returns a
  usable Image; exit snapshots through the deadline callback; `snapshot_directory` and
  `mount_image` by copy-in.
- **Gate:** snapshot steps pass; a snapshot image boots a new sandbox after the original is gone.

### Phase 7: going public (about 4 days)

- sandboxd: `--modal-public-url` (router URL from it, scheme per client type), and the Modal
  disks built for `/bulk/sandboxd`.
- home-cloud: `modal` in `sandboxd_apis` (port 7794), Service with `scheme: h2c` plus an
  HTTP/1.1 Service for tunnels, certificate for `modal.<d>` and `*.modal.<d>`, IngressRoutes,
  probes.
- Acceptance: all three SDK suites from off-network against `https://modal.sandbox.inevitable.fyi`;
  `try_modal()` in `try.py`; the site's Modal card un-grayed with its tested versions.
- **Gate:** the H11-style run is green and the differences doc is complete.

### Phase 8: drift CI (about 2 days)

- A scheduled job (GitHub Actions, no KVM) diffs new `py/` tags' protos against the served
  subset and opens an issue on change.
- On a new release, the probe suite runs on a KVM host (geek) against a test sandboxd; results
  in the issue. 1.6.1's `sandbox_token` field is the first thing it will catch.

**Total: about 71 dev-days** serially; with Phases 1–4 in parallel, roughly 6–7 weeks elapsed.

## Who does what and how it runs

As in [multi-provider-burndown.md](multi-provider-burndown.md): this session orchestrates and
reviews; one implementer agent per phase in its own worktree, watched in illogical panes;
`/code-review` on every PR. Test stacks are the same: agent stacks from `scripts/dev-data.sh`
on `--net=false`, the gate stack on pool 1 (`sandboxd --net-pool 1 --modal-listen ...`), never
production. Builder and volume work needs a reflink volume, so their test stacks live on
`/bulk`, not `/tmp`.

Nothing merges with a red `make test`, a red Sprites e2e run, or a red suite for any other
provider: Phases 1, 4 and 5 touch the guest agent, netpolicy and record storage, which every API
shares.

## Needs from you

- **A Modal account and token** for Phase 0's recordings, kept in
  `~/.config/wisp/modal-hosted.env` (0600). Starter is free with $30/month credits; all the
  recordings should cost well under that.
- **DNS:** none expected; `*.modal.sandbox.inevitable.fyi` should resolve through the existing
  wildcard, as `*.sprites.` did. Checked in Phase 7.
- **When each phase's PR is up:** review, or tell the orchestrator to merge on a green gate.

## Risks

- **ToS:** recording against hosted is your call (made). If Modal objects, the goldens stay as
  they are and further work falls back to client-source truth.
- **Drift:** a client release can break users who don't pin. Mitigated by warnings, the pin, and
  Phase 8.
- **Builder disk use** on `/bulk` (bases about 3 GB, plus layers). Covered by GC and disk-guard
  admission; watch it after launch.
- **Hosted details we can only see in traces** (about 30 items across the reports' "unverifiable"
  lists: FileInfo formats, ID rules, error texts, build environment). Phase 0's recordings
  settle most; the rest go in the differences doc.
