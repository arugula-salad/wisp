# mini-sprites: disposable workspaces on a dependable host

Date: 2026-09-22  
Status: Proposed

## Objective

Make mini-sprites a practical home for coding agents, experiments, and PR previews: prepare a workspace once, fork it cheaply, delegate access, and clean it up automatically while keeping the host predictable and easy to diagnose.

The project already provides persistent Firecracker VMs, suspend/resume, exec sessions, services, filesystem APIs, checkpoints, and network policies. The reviewed checkout also contains uncommitted S3 backup and recovery work. These proposals build on that foundation; they do not assume the backup work has shipped.

## Proposed features

### 1. Fork a sprite from a checkpoint

**Problem:** New sprites start from the base image, so users must repeat dependency installation and workspace setup for each experiment.

**Initial scope:** Create an independent sprite from a selected checkpoint. Use a reflink where supported and the existing sparse-copy fallback elsewhere. Assign a new identity and network address, record its source checkpoint, and boot cold. Make environment and policy inheritance explicit; do not implicitly copy access tokens or expose a public URL. Named templates can follow once checkpoint forks are reliable.

**Acceptance:** A user can prepare a workspace, checkpoint it, and fork multiple copies. Writes and deletion in one copy do not affect the source or siblings. Forks continue working after the source checkpoint is deleted. Failed creation cleans up its allocations and partial files.

### 2. Manage host capacity

**Problem:** Per-sprite memory settings do not provide an aggregate host budget. Concurrent wakes and accumulated snapshots can exhaust shared resources.

**Initial scope:** Add a running-memory admission budget, a concurrent-boot limit, a minimum free-disk reserve, and a total warm-snapshot budget. Account for concurrent reservations and VM overhead. Retire the oldest eligible warm snapshots when necessary, preserving their disks. Start with clear, retryable capacity errors; a bounded wake queue can follow.

**Acceptance:** Concurrent wake requests cannot bypass the configured admission budget. Reservations are released after failure or shutdown. Disk-consuming operations check available headroom and report capacity failures clearly. Snapshot eviction never deletes persistent sprite disks or checkpoints, and diagnostics explain the resulting cold boot. Document that admission accounting is not a guarantee against all host memory pressure.

### 3. Explain sprite lifecycle and host health

**Problem:** Understanding an unexpected cold boot, a sprite that stays awake, or a stale backup requires piecing together logs.

**Initial scope:** Provide a diagnostic API and command exposing keep-awake reasons, last activity, recent lifecycle transitions and causes, warm-restore fallback reasons, capacity reservations, snapshot size, and backup age/errors when configured. Add host metrics for capacity, wake latency, failures, and lifecycle state. Keep event history bounded and identify stale or unavailable measurements.

**Acceptance:** Inspecting a sleeping sprite does not wake it. A user can explain why a sprite is running, why its last wake was cold, and whether its configured backup is current. Diagnostics do not expose environment values or credentials. A dashboard is optional follow-up work.

### 4. Expiring workspace leases

**Problem:** Temporary experiments and previews remain on disk until someone remembers to delete them. Existing task expiration controls keep-awake holds, not workspace lifetime.

**Initial scope:** Add opt-in workspace expiration, renewal, and a protected flag. Persist expiration across daemon restarts and expose it in status. Warn through lifecycle events before expiry. Define expiry as deletion even when a workspace is active; protection blocks automatic deletion. Reuse backup tombstones when backups are configured.

**Acceptance:** Persistent sprites remain unchanged by default. Expired, unprotected sprites are cleaned up after normal operation or daemon restart. Renewal and protection changes are serialized with deletion so the outcome is predictable. Expiry never implies a fresh backup exists; status clearly distinguishes the last completed backup from current state.

### 5. Scoped, revocable access tokens

**Problem:** A shared host token grants too much access when handing a single workspace to an agent or collaborator.

**Initial scope:** Retain administrator access and add tokens restricted to specific sprite identities and operation groups, with expiration and individual revocation. Store token verifiers rather than recoverable token values. Apply authorization consistently to REST, WebSocket exec, multiplexed control, filesystem operations, proxies, and authenticated sprite URLs.

**Acceptance:** A token for one sprite cannot list or manipulate unrelated sprites. Deleting and recreating a sprite with the same name does not transfer access. Revocation and expiry reject new requests and terminate affected long-lived sessions within a documented bound. Credentials stay out of logs and diagnostics.

### 6. Service readiness and bounded logs

**Problem:** A running process or an open port does not establish that an application is ready to serve requests. Dependency start order alone cannot express readiness.

**Initial scope:** Add optional HTTP or command readiness probes with timeouts and bounded retry intervals. Allow dependencies to wait for readiness. Gate sprite-URL traffic on the target service's readiness with a bounded wait and a useful failure response. Add configurable service-log rotation and retention.

**Acceptance:** A cold-booted application receives traffic after its configured readiness check succeeds. Failed dependencies produce actionable state rather than indefinite waits. Existing services without probes preserve their current behavior. Background probes do not keep otherwise idle sprites awake, and retained logs stay within configured limits.

## Delivery order

1. **Checkpoint forks:** establish the central user workflow and exercise disk independence and failure cleanup.
2. **Capacity management and lifecycle diagnostics:** make increased workspace density predictable and explainable.
3. **Workspace leases:** complete the disposable-workspace workflow using the preceding diagnostics and lifecycle controls.
4. **Scoped tokens:** enable delegated agent access; move this earlier if sharing the host becomes an immediate requirement.
5. **Service readiness and log retention:** improve the reliability of previews and hosted development services.

The first cohesive release is **forks + capacity management + diagnostics + expiration**. Its demonstration should prepare one workspace, fork several experiments, show capacity handling, explain their lifecycle state, and automatically remove expired copies.

## Constraints and validation

- Preserve existing Sprites CLI/SDK behavior. Introduce extensions without requiring upstream clients to understand them.
- Keep the initial scope single-host. Defer multi-host scheduling, continuous remote block storage, memory-state forks, and a full dashboard.
- Serialize new operations with existing lifecycle transitions, checkpoints, deletion, and backup activity.
- Validate concurrent admission, independent fork disks, expiry/renewal races, authorization across all transports, and readiness after cold boot. Use real microVM integration coverage where mocks cannot establish the behavior.
- Measure fork latency on reflink and plain storage separately. Track capacity rejections, wake failures, snapshot evictions, and expired-workspace cleanup rather than setting unsupported performance promises.

## Implementation anchors

- Creation and API routing: `internal/server/api.go`
- Reflink and sparse-copy storage: `internal/server/storage.go`
- Checkpoints: `internal/server/checkpoints.go`
- Lifecycle and reservations: `internal/server/lifecycle.go`
- Existing resource policies: `internal/server/policy_limits.go`
- Persistent sprite metadata: `internal/store/store.go`
- Services and HTTP routing: `internal/agent/services.go`, `internal/server/urlproxy.go`
- Backup integration in the reviewed checkout: `internal/server/backup.go`
