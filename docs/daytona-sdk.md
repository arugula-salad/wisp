# Using the Daytona SDKs

`sandboxd` serves the [Daytona](https://daytona.io) API beside the Sprites API (and the E2B
API), from the same engine, so the official, unmodified Daytona SDKs (Python `daytona`, TypeScript
`@daytonaio/sdk`) create and drive sandboxes on your machine: one-shot commands and code runs,
sessions (shells whose commands share a working directory and environment, with logs you can
follow), files, preview URLs, labels, auto-stop, stop, start and delete. What differs from hosted
Daytona is in [providers/daytona-differences.md](providers/daytona-differences.md).

Daytona's own daemon is AGPL-licensed, so it is not in the guest. The API the SDKs reach inside
a sandbox (Daytona calls it the toolbox) is served by `sandboxd` itself, on the host, on top of
wisp's guest agent. It was written from the Apache-2.0 SDKs and Daytona's public docs.

## Run it

`sandboxd` takes every `wispd` flag, and the Daytona listener is off until you give it an address:

```sh
make build                            # bin/wispd and bin/sandboxd
./scripts/build-image.sh daytona      # <data>/images/daytona.ext4: the sandbox userland
./bin/sandboxd --daytona-listen 127.0.0.1:7842
```

The Sprites API is on `--listen` as with `wispd` (127.0.0.1:7788), the E2B API on `--e2b-listen`,
the Daytona API on `--daytona-listen`. They share one data directory, one store and the same API
keys. A sandbox belongs to the API that made it: Daytona sandboxes are not in the Sprites or E2B
APIs' lists, nor theirs in Daytona's.

| Flag | Default | |
| --- | --- | --- |
| `--daytona-listen` | off | the Daytona API (`/api`), the toolbox (`/toolbox/<id>/...`) and preview URLs |
| `--daytona-domain` | `daytona.localhost` | preview URLs are `http://<port>-<id>.<this>:<listen port>`, which the listener serves |
| `--daytona-image` | `<data>/images/daytona.ext4` | the disk every Daytona sandbox starts as |
| `--daytona-url` | the request's `Host` | how clients reach the listener, for the `toolboxProxyUrl` each sandbox reports; set it behind a proxy |

## Point the SDK at it

```sh
export DAYTONA_API_KEY=$(cat ~/.local/share/wisp/token)   # the root token, or a key from `wispd keys create`
export DAYTONA_API_URL=http://127.0.0.1:7842/api
```

The SDKs read `DAYTONA_API_URL` from the process environment only: one in a `.env` file is
ignored, with a warning. Nothing else needs setting: there is no target or organization.

```python
from daytona import Daytona, CreateSandboxFromSnapshotParams, SessionExecuteRequest

daytona = Daytona()
sbx = daytona.create(CreateSandboxFromSnapshotParams(language="python", auto_stop_interval=15))
print(sbx.process.exec("whoami && pwd").result)          # daytona / /home/daytona
print(sbx.process.code_run("print(6 * 7)").result)        # 42
sbx.fs.upload_file(b"hi", "hello.txt")                    # relative to /home/daytona

sbx.process.create_session("dev")
sbx.process.execute_session_command("dev", SessionExecuteRequest(command="cd /tmp && export X=1"))
r = sbx.process.execute_session_command("dev", SessionExecuteRequest(command="pwd; echo $X"))
print(r.stdout)                                           # /tmp 1: the session kept both

sbx.process.execute_session_command("dev", SessionExecuteRequest(
    command="python3 -m http.server 8080", run_async=True))
link = sbx.get_preview_link(8080)                         # http://8080-<id>.daytona.localhost:7842
# curl -H "x-daytona-preview-token: $TOKEN" <link.url>

sbx.stop(); sbx.start()                                   # files kept, processes not
sbx.delete()
```

```ts
import { Daytona } from '@daytonaio/sdk'
const daytona = new Daytona()
const sbx = await daytona.create({ language: 'typescript' })
console.log((await sbx.process.codeRun('console.log(1 + 1)')).result)
await sbx.delete()
```

Preview URLs are plain HTTP on the Daytona listener, under `--daytona-domain`. `*.localhost`
resolves to the loopback address with systemd-resolved, browsers and Node; plain glibc may not,
in which case send the request to the listener with the preview host as `Host`. A private
sandbox's preview wants the `token` from `get_preview_link` as the `x-daytona-preview-token`
header (or an API key as a bearer token); a `public=True` sandbox's wants nothing.

## Inside a sandbox

The disk is `images/daytona` ([images.md](images.md#the-daytona-image)): Ubuntu 24.04 with
Python 3, Node.js 22, TypeScript (`tsx`, `ts-node`), git and build tools, user `daytona` (uid
1000, passwordless sudo), home and working directory `/home/daytona`, hostname `daytona`.
Commands run as `daytona`, with the sandbox's env vars. There is no daemon of Daytona's in it:
a toolbox request becomes a wisp-agent exec or filesystem call over the VM's vsock.

A sandbox's `auto_stop_interval` (15 minutes unless set; 0 is never) is wisp's idle rule with
the action *stop*: after that long with no toolbox or preview traffic and nothing running in the
guest, its VM is stopped cold, keeping its disk. `stop()` does the same on demand; `start()`
boots it again. Stopped means stopped: toolbox and preview requests answer 409 until a start.

## Testing it

The Daytona probe suite runs both SDKs through every step above, then deletes anything it left:

```sh
cd e2e/providers/daytona
DAYTONA_API_URL=http://127.0.0.1:7842/api DAYTONA_API_KEY=$(cat <data>/token) ./run.sh
```

There are no golden traces from hosted Daytona to compare with (no account yet), so where the
SDKs leave the server's behaviour open, the choice made is listed as unverified in
[daytona-differences.md](providers/daytona-differences.md#unverified-against-hosted).
