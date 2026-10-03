# Using the Vercel Sandbox SDKs

`sandboxd` serves the [Vercel Sandbox](https://vercel.com/docs/sandbox) API beside the Sprites
API, from the same engine, so the official, unmodified Vercel Sandbox SDKs (JS
`@vercel/sandbox` 3.x, Python `vercel-sandbox`) create and drive sandboxes on your machine:
commands with streamed output, detached commands with logs, kill and wait, files, ports,
timeouts and extensions, stop and resume, persistent sandboxes and snapshots. What differs
from hosted Vercel is in [providers/vercel-differences.md](providers/vercel-differences.md);
the API the front end follows is surveyed in [providers/vercel.md](providers/vercel.md).

## Run it

```sh
make build                          # bin/wispd and bin/sandboxd
./scripts/build-image.sh vercel     # <data>/images/vercel.ext4: Vercel's userland (docs/images.md)
./bin/sandboxd --vercel-listen 127.0.0.1:7824
```

The Vercel API is off unless `--vercel-listen` gives it an address. It shares the data
directory, the store and the API keys with the Sprites API (`--listen`) and the E2B API
(`--e2b-listen`). A sandbox belongs to the API that made it: Vercel sandboxes are not in the
Sprites or E2B lists, nor theirs in Vercel's, and Vercel's names are their own namespace.

| Flag | Default | |
| --- | --- | --- |
| `--vercel-listen` | off | the Vercel API and its sandboxes' routes, e.g. `127.0.0.1:7824` |
| `--vercel-domain` | `vercel.localhost` | a route's `url` is `http://<subdomain>.<this>:<listen port>`, which the listener serves |
| `--vercel-image` | `<data>/images/vercel.ext4` | the disk every Vercel sandbox starts as |
| `--vercel-mem-per-vcpu-mib` | 2048 | guest RAM per vCPU, as hosted gives (`resources.vcpus` is 1 or even, default 2) |
| `--vercel-max-timeout` | 24h | the longest a session may run, extensions included |

Vercel sandboxes are never suspended for being idle: a session runs until its timeout and is
then stopped, as on hosted Vercel.

## Point the SDKs at it

The token is the daemon's root token (`<data>/token`) or a key from `wispd keys create`; any
team and project ID are accepted (one daemon is one team).

**Python** takes a base URL:

```python
import asyncio
from vercel import sandbox
from vercel.api import session
from vercel.sandbox import SandboxServiceOptions

# VERCEL_TOKEN, VERCEL_TEAM_ID and VERCEL_PROJECT_ID from the environment, as for hosted Vercel.
async def main():
    async with session(service_options=[SandboxServiceOptions(base_url="http://127.0.0.1:7824/api")]):
        box = await sandbox.create_sandbox(name="demo", ports=[3000], execution_time_limit=300)
        print((await box.run_process("sh", ["-c", "id; pwd"], capture_output=True)).stdout)
        await box.fs.write_text("hello.txt", "hi\n")
        await box.create_process("python3", ["-m", "http.server", "3000"])
        print(box.routes[0].url)   # http://sb-….vercel.localhost:7824
        await box.stop()           # persistent by default: the filesystem is kept
        await box.destroy()

asyncio.run(main())
```

**JS** has no base-URL option (see [providers/vercel.md](providers/vercel.md#base-url-where-the-sdks-send-requests-and-how-to-redirect-them)):
preload the probe suite's fetch shim, which rewrites `https://vercel.com/api` to the server:

```sh
VERCEL_SANDBOX_URL=http://127.0.0.1:7824 node --import ./e2e/providers/vercel/target.mjs app.mjs
```

```js
import { Sandbox } from '@vercel/sandbox';
const creds = { token: process.env.VERCEL_TOKEN, teamId: 'team_local', projectId: 'prj_local' };
const sbx = await Sandbox.create({ ...creds, name: 'demo', ports: [3000] });
const r = await sbx.runCommand('sh', ['-c', 'echo hello from $(whoami)']);
console.log(await r.stdout());                 // hello from ubuntu
await sbx.stop();
await sbx.runCommand('true');                  // 410, then the SDK resumes it in a new session
await sbx.delete();
```

`sandbox.domain(port)` in JS is always `https://<subdomain>.vercel.run`, whatever the server
says. The shim also rewrites those URLs, for fetches made in the same process, to
`<server>/<subdomain>/…`, a form the listener serves too; set
`VERCEL_SANDBOX_DOMAIN_TEMPLATE` (e.g. `http://{subdomain}.vercel.localhost:7824`) for another.
Python's `route.url` is the server's own and needs nothing.

## Inside a sandbox

The disk is `images/vercel` ([images.md](images.md#the-vercel-image)): Ubuntu 24.04 with
Node.js 24 at `/usr/local/bin/node`, Python 3, git, curl and build tools; user `ubuntu`
(uid 1000, passwordless sudo) with home and working directory `/vercel`. There is no
Vercel daemon in the guest: the front end turns commands into wisp-agent exec sessions and
files into its filesystem API, so commands run as `ubuntu` in `/vercel` (or as root with
`sudo: true`), and files written through the API belong to `ubuntu`.

## Testing it

The probe suite runs both SDKs through every step (JS 16, Python 11):

```sh
cd e2e/providers/vercel
VERCEL_SANDBOX_URL=http://127.0.0.1:7824 VERCEL_TOKEN=$(cat <data>/token) \
  VERCEL_TEAM_ID=team_local VERCEL_PROJECT_ID=prj_local ./run.sh
```

`RECORD=1 VERCEL_RECORD_UPSTREAM=http://127.0.0.1:7824 VERCEL_RECORD_OUT=<dir>` (with the same
variables) records the traffic for comparison with hosted Vercel's traces in `golden/`.
