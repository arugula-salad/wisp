# Differences from hosted Modal

What wisp's Modal front end (`frontend/modal`, served by `sandboxd --modal-listen`) does
differently from hosted Modal. It came out of a time-boxed spike: the specification is
[modal.md](modal.md), the outcome is its [Spike result](modal.md#spike-result), and how to point
the client at it is [Using the Modal client](../modal-client.md).

The yardstick is `e2e/providers/modal/run.sh`: the unmodified `modal` 1.6.0 Python client runs
the spike's target script (V2 and V1 sandboxes) and 14 further checks against `sandboxd`. No
traces of hosted Modal were recorded (no Modal account was used), so unlike E2B this list is
from the client's source and the protocol, not a trace comparison.

Getting to the other providers' quality bar (all of the API that sandboxes use, the Go and JS
SDKs, golden traces of hosted Modal) is planned in
[plans/modal-parity.md](../plans/modal-parity.md).

**Known issues**, found by that plan's spike and fixed in its Phase 0:

- `Sandbox.create` ignores fields it cannot honour instead of refusing them. Notably
  `block_network=True` and the outbound allowlists are accepted and the sandbox gets open
  egress; also `idle_timeout`, `readiness_probe`, `custom_domain` and cpu/memory limits.
- Exec runs with stdin on `/dev/null`, so a command that reads stdin (`sb.exec("cat")`) ends at
  once instead of waiting, as it would on hosted Modal.
- Exec output past 64 MiB per stream is dropped without an error.
- On V1 (`MODAL_SANDBOX_V2=0`), `Sandbox.create(env=...)` goes through `SecretGetOrCreate`, which is not served (read from the client's code; the V1 run does not pass `env=`).

## What exists

Only what a sandbox with exec needs: 19 RPCs of the client's 277. Everything else answers
`UNIMPLEMENTED`, which the client raises as `modal.exception.UnimplementedError` naming the
method (`unknown method SandboxTagsSetV2 for service modal.client.ModalClient`).

| Works | Not built |
| --- | --- |
| `App.lookup` (with and without `create_if_missing`) | Functions, classes, `modal run` / `deploy` / `serve`, web endpoints |
| `Image.debian_slim()` (any `python_version`) | Every other image: `from_registry`, `pip_install`, `apt_install`, `run_commands`, `add_local_*`, Dockerfiles, `Image.from_id` |
| `Sandbox.create(*args, app=, image=, timeout=, workdir=, env=, cpu=, memory=)` | `secrets=` (named secrets; `env=` works), volumes, mounts, bucket mounts, `gpu=`, `pty=`, `encrypted_ports`/`unencrypted_ports`/`tunnels()`, `name=`, tags, `idle_timeout`, readiness probes, `block_network`/`cidr_allowlist`/outbound policy |
| `sb.exec(...)` with `stdout`/`stderr` (PIPE, DEVNULL, STDOUT), `env=`, `workdir=`, `timeout=`; `p.stdout.read()`, `p.stderr.read()`, iteration, `p.wait()`, `p.poll()`, `p.returncode` | `p.stdin` (`TaskExecStdinWrite*`), PTY execs, `sb.stdout`/`sb.stderr`/`sb.stdin` (the entrypoint's stdio), `sb.open()`/`sb.filesystem` and `sb.ls`/`mkdir`/`rm`, `sb.snapshot_*`, `sb.mount_image`, `sb.reload_volumes` |
| `Sandbox.from_id`, `sb.wait()`, `sb.poll()`, `sb.returncode`, `sb.terminate()` | `Sandbox.list`, `Sandbox.from_name`, `sb.set_tags`/`get_tags`, `sb.get_logs`, connect tokens |
| V2 sandboxes (the 1.6.0 default) and V1 (`MODAL_SANDBOX_V2=0`) | Modal's dashboard, `modal` CLI commands other than as a library |

## Behaviour

**Images.** Nothing is built. `ImageGetOrCreate` recognizes the command list
`Image.debian_slim()` sends and answers "built" at once; the sandbox then runs on
`images/modal`, prebuilt from `python:3.14.2-slim-bookworm` with debian_slim's steps. Any other
recipe fails at `Sandbox.create` with `modal.exception.ConflictError` (gRPC
`FAILED_PRECONDITION`): `this Modal-compatible server cannot build an image layered on another
(.pip_install, .run_commands, ...): it serves Image.debian_slim() only, prebuilt`. Hosted Modal
builds whatever is asked.

- **The Python version is always 3.14.** The client puts the *caller's* local Python series in
  the `FROM` line (`python:3.12.10-slim-bookworm` from a 3.12 client); the server accepts any
  `python:3.X` there and serves the same 3.14 image. Hosted Modal gives you the version you ran
  with.
- The image ID is a hash of the commands, so clients with different Pythons get different IDs
  for the same guest image.
- The workaround for a missing layer is an `exec`: the sandbox is a VM and execs run as root, so
  `pip install` and `apt-get install` should work in it when the daemon has guest networking.
  This was not exercised: the spike's gate ran with `--net=false`.

**Exec.** Commands run through wisp-agent's exec API as **root** (Modal's default user),
through `sudo -H` and `env`, because wisp-agent runs exec sessions as the guest's `sprite`
account. So:

- The environment is sudo's reset one (`HOME=/root`, `PATH` with `/usr/local/bin`, `USER`,
  `LOGNAME`, `SHELL`), plus `LANG=C.UTF-8`, the sandbox's `env=`, then the exec's `env=`.
  Hosted Modal's container environment has more (the image's `ENV`, `MODAL_*` variables such as
  `MODAL_SANDBOX_ID`, `MODAL_TASK_ID`, `MODAL_REGION`); none of those are set.
- The working directory defaults to `/`. Hosted Modal uses the image's `WORKDIR`; for
  `debian_slim` that is believed to be `/` as well, but this was not checked against hosted Modal.
- A command that does not exist exits 127 (from `env`), with `env: 'x': No such file or
  directory` on stderr. Hosted Modal may refuse the exec instead; unverified.
- Exit statuses are numbers up to 255. A command killed by a signal reports `128 + signal` as
  its *code*, not as a signal, so `p.returncode` is the same (`128 + n`) either way.
- An exec's `timeout=` kills it (`SIGKILL` after the agent's one-second grace), and its exit is
  then 137.
- Output is kept in memory, whole, per stream, for as long as the sandbox runs, so a reader can
  resume from any byte offset after a dropped stream, as the client does. Past 64 MiB on one
  stream of one command, that stream is truncated: the rest of its output is dropped.
- Exec IDs are idempotency keys, as hosted: a second `TaskExecStart` with the same ID is a no-op.

**Sandboxes.**

- **The entrypoint is run** (as root, like an exec) and the sandbox ends when it exits:
  `SUCCESS` with its code if 0, otherwise `FAILURE` with its code, as hosted. Its output is
  discarded, since `sb.stdout` is not implemented. Without an entrypoint the sandbox runs
  until its timeout. An entrypoint that never starts, or whose connection to the guest drops,
  ends the sandbox with `INIT_FAILURE` (returncode -1), and the result's exception says why.
  A drop caused by the daemon stopping does not count: that sandbox runs on (below).
- Timeouts (`timeout=`, default 300 s, at most 24 h) end the sandbox with `TIMEOUT` (returncode
  124); `terminate()` with `TERMINATED` (137). Both delete the VM and its disk. There is no
  `idle_timeout`, and sandboxes are never suspended for being idle.
- A sandbox is a Firecracker VM, not a gVisor container. `cpu=` rounds up to whole vCPUs and
  `memory=` is the VM's RAM in MiB; with neither, the engine's defaults apply (`--vcpus`,
  `--mem-mib`). Asking for more CPUs than the host has, or more memory than the engine allows
  a VM (host RAM less its headroom), fails with `InvalidError`. The guest kernel, `/proc` and the rest are wisp's, and the hostname is `modal`.
- How a sandbox ended is kept for 7 days after it ends, in `<data>/modal/state.json`, so that
  `from_id`, `wait` and `poll` still answer. A sandbox deleted some other way (for example by an
  operator) reads as `TERMINATED`. The front end hears of every deletion, including the
  engine's own at the deadline, which comes within about 30 s of it, so every sandbox gets a
  result and its commands' output is freed.
- If the daemon restarts, running sandboxes' commands, the entrypoint included, are cut off
  (their VMs suspend with the daemon, as every sandbox's do); the sandbox itself carries on
  until its timeout.
- Sandbox IDs have the shapes the client routes on: V2 sandboxes are `sb-` and 24 characters
  (anything but V1's shape), V1 sandboxes `sb-` and 22, and task IDs are `ta-` and the same
  characters, with `V` appended for V2. They use `[a-z0-9]` only.
- `--max-sprites` counts Modal sandboxes with every other kind; past it, a create fails with
  `RESOURCE_EXHAUSTED` (`ResourceExhaustedError`).

**Reaching it.**

- One listener serves both gRPC services (control plane and task command router), over h2c
  only, with no TLS. The client accepts the plaintext router URL it is handed only when
  `MODAL_SERVER_URL`'s host is `localhost`, `127.0.0.1`, `::1` or `172.21.0.1`. A remote client
  needs an SSH tunnel, or a TLS front with `--modal-router-url https://...` and a certificate it
  trusts.
- `MODAL_SERVER_URL` must be set. The default is `api.modal.com`, and a stray `~/.modal.toml`
  profile can point elsewhere (`run.sh` sets `MODAL_CONFIG_PATH` to an empty file).

**Auth.**

- `MODAL_TOKEN_SECRET` is checked against the daemon's root token and API keys (`wispd keys`);
  `MODAL_TOKEN_ID` is ignored and may be anything. A read-only key may look (wait, poll,
  `from_id`) but not create, exec or terminate (`PermissionDeniedError`). Hosted Modal's
  workspaces, environments and OAuth do not exist: `MODAL_ENVIRONMENT` only namespaces app
  names, and every environment reports builder version 2025.06.
- The router's JWTs are signed by a key that is new every time the daemon starts, and expire
  after an hour. The client fetches a new one when the router refuses it, as it does with
  hosted Modal's.
- Apps hold nothing but their names. They are never stopped and cannot be listed or deleted.

**Versions.** The protocol is Modal's private API, generated from the 1.6.0 wheel
(`frontend/modal/modalpb`). Other client versions may send other RPCs or fields; only 1.6.0 was
run. See the [Spike result](modal.md#spike-result).
