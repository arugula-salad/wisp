# Burn-down brief: multi-provider sandbox server

Plan: [Multi-provider sandbox server](https://claude.ai/code/artifact/7f9b8675-3b0a-453b-b3a5-ca2833cdb6b3)
(engine extracted from wisp; Sprites, E2B, Vercel Sandbox, Daytona front-ends; Modal spike).
This brief covers how the plan gets executed: who does what, in what order, and what has to be
true before each step moves on. Written 2026-10-02.

## Decisions assumed (change them before phase 1 starts)

- **Everything stays in this repo until phase 2 ships.** The engine becomes the importable package
  `github.com/arugula-salad/wisp/engine` (not `internal/`), so it can move to its own module
  later without a rewrite. `cmd/wispd` stays Sprites-only. The meta daemon is a new `cmd/` binary
  that mounts every front-end. Splitting out a new top-level project can happen after that, once
  the interface has stopped moving.
- **Namespaces are per API.** A sandbox belongs to the API that created it. The store and web UI
  are shared.
- **Upstream gets nothing new until a phase gate passes.** Work happens on branches, one stacked
  PR per slice. Nothing merges to `main` with a red `make test` or e2e run.

## Who does what

| Role | Who | Runs where |
| --- | --- | --- |
| Orchestrator: owns the engine interface, slices the work, reviews every PR, keeps the status table below | This Claude session | Here |
| Refactor implementer: one slice at a time | One Claude Code agent started with illogical `start_agent`, so you can watch it and take over | Its own git worktree |
| Research and probe-suite writers | Claude subagents (`Agent`, worktree isolation) | Background, in parallel |
| Daemons and e2e runs | illogical `run` panes, named per stack | This host (needs `/dev/kvm`) |
| Reviewer | `/code-review` on each PR, then the orchestrator | Here |

**Phase 1 is serial on purpose.** Every slice edits `internal/server`, and parallel refactors of
the same 6.6k lines would fight over merges. The parallelism goes to the work that doesn't touch
that code: provider research and conformance suites, which run during phase 1 so phase 2 starts
with its spec already written.

## Test stacks

Only one wispd per host can own the tap pool, so:

**Production is live on this host. Never test against it.** `wisp.service` (user unit) runs the
installed wispd from `~/.local/lib/wisp`. It owns the `msbr0`/`mstap*` tap pool and wisp-netd,
holds 89 sprites in `~/.local/share/wisp`, and serves public URL domains. Ports 7788 and 7789
belong to its socket forwarders. No phase of this plan restarts or redeploys it unless you say so.

- **Agent stacks:** `./scripts/dev-data.sh ~/ws/<n>`, then
  `wispd --data ~/ws/<n> --listen 127.0.0.1:78<n> --net=false`, with n from 10 upward.
  - Each implementer gets one for quick e2e loops.
  - They live on the root filesystem: `/tmp` is tmpfs with about 19 GB free, too little for sprite disks.
  - The netpolicy subtests skip here.
- **Gate stack (pool 1):** `wispd --net-pool 1 --data ~/ws/1 --listen 127.0.0.1:7802`, on bridge
  `msbr1` (10.210.0.0/16), which `sudo WISP_POOL=1 scripts/setup-host.sh` created on 2026-10-02. A
  gate run is the full `go test -tags e2e ./e2e/` with `SPRITES_E2E_URL`/`TOKEN`/`IDLE_TIMEOUT`
  set, plus `scripts/probe-sdks.sh` with `SPRITES_API_URL`/`SPRITE_TOKEN`, both against :7802.
  On this host `/tmp` is a quota-limited tmpfs shared with other sessions, so run tests that
  write VM disks with `TMPDIR=~/ws/tmp`. Rebuild the stack's initrd with `WISP_DATA=~/ws/1 ./scripts/build-initrd.sh`; plain
  `make initrd` writes to production's data dir.
- **Real-data test for slice 1.4:** reflink-copy production's sprite data on its XFS volume, copy
  the store JSON, and boot a `--net=false` stack on the copy. Production's own files are never
  opened for writing.

The host has 32 cores and 121 GB RAM (about 50 GB available), which is enough for 3 or 4 agent
stacks alongside the gate stack.

## Phase 0: research and probes (starts now, alongside phase 1)

| Track | Output | Needs from you |
| --- | --- | --- |
| E2B API survey: REST control plane, envd Connect RPC, auth, sandbox IDs, host routing, template build, pause/resume | `docs/providers/e2b.md`, with SDK versions and the source files read | none |
| E2B probe suite: Python and JS SDKs; create, exec with streaming, files, ports, pause/resume, delete | `e2e/providers/e2b/`, run once against hosted E2B and saved as golden traces | **E2B API key** |
| Vercel Sandbox survey and probe suite | `docs/providers/vercel.md`, `e2e/providers/vercel/` | **Vercel token and team** |
| Daytona survey (spec and SDK behaviour only; the source is AGPL) | `docs/providers/daytona.md` | Daytona key, later |
| Modal feasibility note: how much of the platform `modal.Sandbox.create()` touches | `docs/providers/modal.md` | none |

Without keys, the probe suites still get written. The golden traces wait until you supply keys.

Credentials live outside the repo in `~/.config/arugula/providers.env` (mode 600), which the
probe suites source: `E2B_API_KEY`; `VERCEL_TOKEN`, `VERCEL_TEAM_ID` and `VERCEL_PROJECT_ID`, or a
`VERCEL_OIDC_TOKEN`. A Vercel AI Gateway key does not authenticate Sandbox. Agents check which
names are set and never print the values.

## Phase 1: extract the engine, in stacked slices

Order: seams are cut inside `package server` first (1.1–1.4), and the package move comes last
(1.5), because moving first would force exporting every internal the handlers touch today.

From the seam map: about 55% of `internal/server` is engine code already. Most mixed files split
at the handler / `*Locked` boundary. The hard parts are the store, the locks and the back-references.

| Slice | Change | Risk |
| --- | --- | --- |
| 0 | Configurable network pool (`--net-pool N`, `WISP_POOL=N`) so a networked test wispd runs beside production. You run one sudo command afterwards. | Low |
| 1.1 | Encapsulate locks, still inside `package server`: replace the ~10 sites that take `rt.mu` and call `rt.m.*` or `agentCall` directly (checkpoints, checkpoint mounts, spawn, leases, policy limits, backup, diskguard) with `Lifecycle` methods that take the lock themselves. | **High.** This is where the races live. |
| 1.2 | Break the back-references: `backupManager` and `leases` stop holding `*Server`, and the lifecycle owns backups and the expiry sweep. | Medium |
| 1.3 | Events keyed by ID: the lifecycle publishes by sandbox ID, and a hook installed by the front-end fills in name and parent. The event stream and `sprite.*` types are wisp's own contract (docs/events.md), so they stay. `LimitError` is already provider-neutral; only its HTTP rendering moves to handlers. | Low |
| 1.4 | Split `store.Sprite` into an ID-keyed engine record (disk, network, checkpoints, mounts, image, lineage, policy) and Sprites front-end metadata (name, URL settings, domains, labels, spawn, parent, expiry). Existing JSON must read back with no migration step. | **High:** it touches real on-disk data |
| 1.5 | Move to `engine/`: `Lifecycle` and the files the seam map marks ENGINE move out. `Server` holds `*engine.Engine`. By now it is mechanical, with a small export surface. | Low |
| 1.6 | Per-sandbox lifecycle policy (idle suspend, timeout, stop), replacing the single global idle rule. Sprites keeps today's behaviour as its default. | Medium |

Out of phase 1 on purpose: **exec and filesystem stay a passthrough.** `proxy` forwards to the
guest agent, whose API effectively *is* the Sprites wire format. The engine exposes
`AgentTransport` and `DialPort` as primitives, and nothing more for now. Whether E2B runs its own
open-source `envd` in the guest or wisp-agent learns its protocol is the first decision of phase 2.
That decision shapes any neutral exec API, so designing one now would be a guess.

**Per-slice loop:**

1. I write the slice spec: files, target interface, invariants.
2. The implementer agent does the slice in its worktree, running `make test` and e2e on its own stack.
3. `/code-review`, then my review, then fixes.
4. Gate-stack e2e in a visible pane, then merge.

**Phase 1 gate:**

- `make test`, the full `make e2e` and `probe-sdks.sh` all pass on the gate stack.
- A copy of the real data dir boots and wakes its existing sprites.
- No Sprites API diff: the e2e suite and the JS, Python and Go SDKs pass unchanged.

## Phase 2 onward

- **Phase 2 (E2B):** settle the envd decision, add `frontend/e2b`, add the meta daemon binary,
  and route by Host (`<port>-<id>.e2b.localhost`). Gate: the E2B probe suite passes locally and
  matches the golden traces apart from the differences recorded in `docs/providers/e2b-differences.md`.
- **Phases 3 and 4 (Vercel, Daytona):** the same shape. Once the phase 2 gate holds, Vercel and
  Daytona can run as two parallel implementer agents, because each one lives in its own
  `frontend/` package and only reads the engine.
- **Phase 5 (Modal):** a time-boxed spike, decided by the phase 0 feasibility note.

## What I need from you

- [x] Confirm the decisions assumed above (same repo, `engine/` as a public package).
- [x] Vercel credentials (`VERCEL_TOKEN`, `VERCEL_TEAM_ID`, `VERCEL_PROJECT_ID`) in providers.env.
- [x] `E2B_API_KEY` in providers.env.
- [x] Permission to push branches and open PRs on `arugula-salad/wisp`.
- [x] Network gate: option (a), a second tap pool. Pool 1 was created 2026-10-02.

## Status

| Item | State |
| --- | --- |
| Plan doc | Done |
| Seam map of `internal/server` | Done (summarised in phase 1) |
| Phase 0: E2B survey + probes | Done: PR #37 merged. Decision: run E2B's own envd in the guest. 26/26 SDK steps pass against hosted E2B |
| Phase 0: Vercel survey + probes | Done: PR #35 merged. JS 16/16, Python 11/11 against hosted Vercel. The JS SDK needs a fetch-rewriting preload to retarget |
| Phase 0: Daytona + Modal surveys | Done: PR #32 merged. Daytona is about 1.5x E2B; Modal is a go for a time-boxed spike |
| Slice 0: network pool | Done: PR #34 merged. Gate on pool 1: e2e 21 pass / 0 fail, SDK probes 0 failures |
| Slice 1.1: lock encapsulation | Done: PR #36 merged. Gate on pool 1: 21 pass / 0 fail, probes 0 failures. Also fixed `make e2e`, which had hardcoded production's URL |
| Slice 1.2: back-references, Lifecycle owns create/delete | Done: PR #39 |
| Slices 1.3+1.4: engine Record, store keyed by ID, events by record | Done: PR #40. All 89 production records round-trip byte-identically; three real sprites boot unchanged; downgrade works |
| Slice 1.6: per-sandbox lifecycle policy | Done: PR #41 (done before 1.5, so the move came last) |
| Slice 1.5: `engine` package | Done: PR #44. `engine.Engine`; Sprites metadata reaches it only through hooks. **Phase 1 gate met** |
| netpolicy test flakes | Fixed: PR #43 (dial-time RST in a test helper; a UDP/TCP port pick race) |
| Phase 2a: envd in the guest | Done: PR #42. envd 0.9.0 pinned; `/etc/wisp/services.d` system services; `images/e2b` |
| Phase 2b: E2B front-end + `cmd/sandboxd` | Done: PR #46. **Phase 2 gate met**: the unmodified E2B SDKs (py and js 2.52.0) pass 26/26 against sandboxd, and the recording matches hosted apart from documented differences. The review found and fixed a port-injection hole |
| Phase 3: Vercel front-end | Running (illogical pane 128, worktree `wisp-wt/vercel-frontend`) |
| Phase 4: Daytona front-end | Running (illogical pane 129, worktree `wisp-wt/daytona-frontend`) |
| Phase 5: Modal spike | Running (illogical pane 133+, worktree `wisp-wt/modal-spike`) |
