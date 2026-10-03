# Differences from hosted Daytona

What wisp's Daytona front end (`frontend/daytona`, served by `sandboxd --daytona-listen`) does
differently from hosted Daytona, on purpose or because it is not built. The specification it
follows is [daytona.md](daytona.md), its "core v1 surface"; how to point the SDKs at it is
[Using the Daytona SDKs](../daytona-sdk.md).

The yardstick is the probe suite in `e2e/providers/daytona/`: the official Python and
TypeScript SDKs (0.220.0) pass all 22 steps against `sandboxd`. Unlike E2B there are **no golden
traces** of hosted Daytona (no account), so every behaviour the SDKs do not pin down is a choice,
listed under [Unverified against hosted](#unverified-against-hosted).

**Licensing.** Daytona's server, runner, proxy and in-sandbox daemon are AGPL-3.0. None of their
source was read. The front end is written from the Apache-2.0 SDKs (`daytona`,
`daytona-api-client`, `daytona-toolbox-api-client`, `@daytonaio/sdk`), their generated models and
Daytona's public docs, and no Daytona code runs in the guest.

## Unverified against hosted

Where the SDKs' code, models and docs leave the server's behaviour open, this is what was chosen,
following the SDK's own documentation and examples where they say anything. Each wants checking
against a trace of hosted Daytona.

| | wisp's choice | Why |
| --- | --- | --- |
| One-shot exec (`process.exec`) | runs `/bin/sh -c <command>`, so pipes, quotes, `&&` and `$VARS` work | the SDK documents `command` as a "shell command" and its examples use shell syntax |
| Its `result` | stdout and stderr together, as the guest saw them (each in its own order; the two pipes are not ordered against each other) | the survey's reading; the SDK's docstring says "standard output", and `artifacts.stdout` is a copy of `result` either way |
| A command past its `timeout` | killed, and the request answers `408` with code `PROCESS_EXECUTION_TIMEOUT` (`DaytonaProcessExecutionTimeoutError`) | the error code exists in the toolbox client's enum; the alternative, an exit code, has no documented value |
| `code_run` | the code is written to a temporary file and run with `python3`, `node` or `tsx` (by the `code-toolbox-language` label), `argv` after it, `envs` in the environment. `artifacts.charts` is always empty | the SDK sends the language; chart capture is hosted's matplotlib hook |
| Session state | each command starts in the working directory and with the exported variables the session's last finished command left. Shell functions, aliases, `set` options and unexported variables do not carry over | the SDK documents sessions as persistent shells; this is the part of a shell that one bash per command can keep (see below) |
| Commands in one session | do not wait for each other: an async command still running does not hold up the next. A command starts from the newest saved state, so an older command finishing late does not undo a newer one's `cd` | a waiting session would deadlock the common "start a server async, then curl it" pattern |
| A session command's stdin | open until the command exits; `send_session_command_input` writes to it and never closes it | the input endpoint has no EOF |
| Session command log follow | a WebSocket of binary frames, each one chunk of one stream behind its 3-byte prefix (`\x01\x01\x01` stdout, `\x02\x02\x02` stderr), everything so far first, closed with 1000 when the command has exited | the SDKs' demultiplexer; when the server closes, and with which code, is not visible from the client |
| `GET .../logs` without follow | JSON `{output, stdout, stderr}`; plain-text `output` only to a client that accepts `text/plain` and not JSON | the generated client accepts both and both SDKs ask for JSON first |
| Input to a finished command | `410` with code `COMMAND_ALREADY_COMPLETED` | the code exists; the status is a guess |
| `create_folder` | `mkdir -p`; `mode` (octal, default 755) is applied to the directory created | |
| `FileInfo.mode` / `permissions` | `mode` as `ls -l` shows it (`-rw-r--r--`, `drwxr-xr-x`; Go's `FileMode` string), `permissions` in octal (`0644`); `modTime` RFC 3339 | both are strings in the model with no documented format |
| Bulk download | one part per path, in the order asked for: `name="file"` with the bytes, or `name="error"` with a JSON `{message, statusCode, code, source}`, `filename` the path as asked for | the SDKs' parsers |
| Toolbox call on a sandbox that is not started | `409` with code `CONFLICT`; the VM is not woken | |
| Preview without a token | `401` JSON | hosted shows a browser page for some clients |
| Errors | `{statusCode, message, error, code, source, path, method, timestamp}`, `source` `DAYTONA_API` or `DAYTONA_DAEMON` | the toolbox client's `ErrorResponse` model plus the fields the SDK's error mapper reads |
| `create` | answers `200` once the VM has booted, already `started` | the SDK waits for `started` either way |
| `delete` | synchronous; answers the sandbox with state `destroyed`, after which `GET` is 404 | the SDK takes a 404 while waiting on a delete as destroyed |
| A sandbox whose VM is down without an API stop (auto-stop, a daemon restart) | `stopped` | |
| Names | unique among Daytona sandboxes; a create without one is named its ID; a taken name is 409 | |

## Reaching it

- `DAYTONA_API_URL` is `http://<listener>/api`. Plain HTTP (and h2c); no TLS.
- `toolboxProxyUrl` is `http://<the Host the request came to>/toolbox`, or `--daytona-url` +
  `/toolbox`; the toolbox is path-routed (`/toolbox/<id>/...`), as hosted.
- The Socket.IO event stream (`/api/socket.io/`) answers 404, so the SDKs poll
  `GET /sandbox/{id}` for state changes, as they do when the stream is unavailable on hosted.
- Preview URLs are `http://<port>-<id>.<--daytona-domain>:<port>` on the same listener, not
  `https://` on a proxy domain. The token is per sandbox, the same for every port, and does not
  expire. What the front end consumed to let a request in never reaches the app: the
  `x-daytona-preview-token` header, the `DAYTONA_SANDBOX_AUTH_KEY` query parameter, and an
  `Authorization` that carried a daemon key (one that is not a daemon key is the app's, and
  goes through). The port
  in the Host must be a number from 1 to 65535 (leading zeros are dropped; anything else is
  400), and the path is cleaned before it is proxied, keeping a trailing slash.

## Auth

- The bearer key is the daemon's root token (`<data>/token`) or one of its API keys (`wispd
  keys`), the same keys as the Sprites and E2B APIs. JWT and organizations are not supported:
  `X-Daytona-Organization-ID` is ignored, and every sandbox reports organization `wisp`.
- A read-scoped key may only `GET`, and may not open a log-follow WebSocket.
- The sandbox's preview token, as `?DAYTONA_SANDBOX_AUTH_KEY=`, opens a session command's log
  follow WebSocket and nothing else in the toolbox. The SDK's browser and serverless runtimes,
  which cannot send headers, use it there. A read key can get the token (`preview-url` is a GET),
  so it is worth no more than a read key: never exec, files or input.

## Sandboxes

- One snapshot: the default (`daytona`, the `images/daytona` disk). Another snapshot name is
  404; `CreateSandboxFromImageParams` (`buildInfo`), volumes, GPUs and a `user` other than
  `daytona` are 400.
- `cpu` and `memory` (GiB) size the VM; the defaults are hosted's, 1 vCPU and 1 GiB. `disk` is
  reported as the image's size (20 GiB by default) and cannot be changed; asking for more is 400.
- `autoStopInterval` is enforced by the engine's idle rule with action *stop* (a cold stop,
  disk kept). Idle means no toolbox or preview request in flight and nothing attached in the
  guest, so **a session command still running keeps the sandbox up**, as does an open preview
  connection. Control-plane reads do not count as activity; `POST /sandbox/{id}/last-activity`
  does.
- `autoDeleteInterval` 0 (`ephemeral`) is enforced: an API stop deletes the sandbox before it
  answers (with state `destroyed`), and an auto-stop deletes it just after. Other auto-delete values and `autoArchiveInterval` are kept and reported,
  not enforced. `autoPauseInterval` above 0 turns auto-stop off (pause is not built).
- `networkBlockAll` and `networkAllowList` are kept and reported, not enforced (wisp's own
  network policy can do this; it is not wired up yet).
- `--max-sprites` bounds how many sandboxes, of every API, exist on the host; a create past it
  is `429` (`DaytonaRateLimitError`). Hosted's limits are per organization quotas.
- A stop the engine fails answers 500 and leaves the sandbox not marked stopped.
- Stop is a cold stop: the guest syncs its disk and the VM is killed; `force` makes no
  difference. Processes and sessions end; files stay. Start boots it afresh.
- When `sandboxd` stops, running VMs are suspended; Daytona sandboxes read as `stopped` until a
  `start` (which resumes them).
- Reported as constants: `organizationId` and `runnerId` `wisp`, `target` `local` unless given,
  `backupState` `None`, `daemonVersion` `wisp-0.1.0`, `kvm` false, `volumes` [].

## Toolbox

- There is no Daytona daemon in the guest. Every toolbox call is translated, host-side, to
  wisp-agent's exec (over its exec WebSocket) and filesystem API.
- Sessions live in `sandboxd`'s memory: their commands and logs are lost when `sandboxd`
  restarts or the sandbox stops. Each command keeps up to 8 MiB of output; past that the oldest
  goes. The session's saved state is in `/tmp/.wisp-daytona/sessions/<id>` in the guest.
- Session commands run with `bash`; one-shot commands with `sh` (dash).
- `list_files` lists one level: `depth` is ignored.
- `user-home-dir` and `work-dir` are both `/home/daytona`; relative paths in file calls and
  `cwd` resolve against it.
- Exit code 137 is reported for a session command whose VM went away under it.

## Not built

Socket.IO events; archive, pause, fork, resize, snapshot-from-sandbox, backup, recover; the TTL,
auto-pause and auto-archive endpoints beyond what is above; the snapshots, volumes, object
storage, secrets and warm-pool APIs; signed preview URLs and signed file URLs
(`download_url`/`upload_url`, which need the signing key); SSH access; metrics and telemetry;
build logs. In the toolbox: PTY, git, LSP, the code interpreter, computer use,
`files/find`, `files/search`, `files/replace`, `files/permissions`, `files/upload-v2`, the
entrypoint session, `/env`, `/port`, `/system/metrics`. All of these answer 404.

## wisp's side

Daytona sandboxes are engine records with `api: "daytona"` and their metadata in
`Record.Ext["daytona"]`. They share the store, the volume, the daemon's limits, backups and the
event stream (by ID, with no name), but not the Sprites or E2B APIs, whose lists and lookups do
not see them. The web dashboard and `wispd status` list sprites only.
