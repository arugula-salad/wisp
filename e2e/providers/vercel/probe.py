"""Probes the official Python Sandbox SDK (vercel-sandbox, pinned in requirements.txt), unmodified.

    VERCEL_SANDBOX_URL=http://127.0.0.1:7820/api python probe.py

Unlike the JS SDK, the Python one takes a base URL: SandboxServiceOptions(base_url=...)
on an SDK session. Credentials come from VERCEL_TOKEN, VERCEL_TEAM_ID and
VERCEL_PROJECT_ID, which the SDK's default resolver reads by itself. The sandbox is
non-persistent and is stopped and destroyed before exit.

Prints one "[py] <step>: OK|FAILED ..." line per step; run.sh counts FAILED.
"""
import asyncio
import os
import time
import urllib.request

from vercel import sandbox
from vercel.api import session
from vercel.sandbox import SandboxApiError, SandboxServiceOptions

BASE = os.environ.get("VERCEL_SANDBOX_URL") or "https://vercel.com/api"
DOMAIN_TEMPLATE = os.environ.get("VERCEL_SANDBOX_DOMAIN_TEMPLATE", "")
PREFIX = f"wisp-probe-py-{int(time.time()):x}"
failures = 0


def log(s):
    print(f"[py] {s}", flush=True)


async def step(name, fn):
    global failures
    t0 = time.monotonic()
    try:
        detail = await fn()
        log(f"{name}: OK ({round((time.monotonic() - t0) * 1000)}ms){' ' + detail if detail else ''}")
    except Exception as e:  # noqa: BLE001 - every failure is reported, not raised
        failures += 1
        extra = ""
        if isinstance(e, SandboxApiError):
            extra = f" status={getattr(e, 'status_code', None)} data={getattr(e, 'data', None)!r}"
        log(f"{name}: FAILED: {type(e).__name__}: {e}{extra}")


def must(cond, msg):
    if not cond:
        raise AssertionError(msg)


def port_url(route):
    if DOMAIN_TEMPLATE:
        return DOMAIN_TEMPLATE.replace("{subdomain}", route.subdomain)
    return route.url


def http_get(url):
    with urllib.request.urlopen(url, timeout=10) as r:  # noqa: S310 - the probe's own sandbox URL
        return r.status, r.read().decode()


async def main():
    global failures
    box = None
    async with session(service_options=[SandboxServiceOptions(base_url=BASE)]):
        try:
            async def create():
                nonlocal box
                box = await sandbox.create_sandbox(
                    name=f"{PREFIX}-a", persistent=False, ports=[3000],
                    execution_time_limit=240, resources=sandbox.SandboxResources(vcpus=1),
                )
                return f"name={box.name} session={box.current_session_id} status={box.status} routes={[(r.port, r.url.split('.')[0][:12] + '...') for r in box.routes]}"
            await step("create", create)
            if box is None:
                return

            async def run_waited():
                r = await box.run_process("sh", ["-c", "echo out1; echo err1 >&2; sleep 1; echo out2; exit 3"], capture_output=True)
                must(r.returncode == 3, f"returncode {r.returncode}")
                must(r.stdout == "out1\nout2\n", f"stdout {r.stdout!r}")
                must(r.stderr == "err1\n", f"stderr {r.stderr!r}")
                return f"id={r.id} returncode={r.returncode} cwd={r.cwd}"
            await step("run_process waited", run_waited)

            async def detached():
                p = await box.create_process("sh", ["-c", "i=0; while true; do i=$((i+1)); echo tick $i; sleep 0.5; done"])
                first = await asyncio.wait_for(p.stdout.readline(), 15)
                second = await asyncio.wait_for(p.stdout.readline(), 15)
                await p.terminate()
                code = await asyncio.wait_for(p.wait(), 30)
                return f"id={p.id} lines={[first.strip(), second.strip()]} returncode after SIGTERM={code}"
            await step("create_process + stdout + terminate + wait", detached)

            async def files():
                await box.fs.write_text("probe/hello.txt", "hello from wisp\n")
                got = await box.fs.read_text("probe/hello.txt")
                must(got == "hello from wisp\n", f"read {got!r}")
                await box.fs.write_bytes("/tmp/probe-abs/bin.dat", bytes([0, 1, 2, 255]))
                must(await box.fs.read_bytes("/tmp/probe-abs/bin.dat") == bytes([0, 1, 2, 255]), "binary roundtrip")
                try:
                    await box.fs.read_text("probe/nope.txt")
                    missing = "no error?!"
                except Exception as e:  # noqa: BLE001
                    missing = type(e).__name__
                return f"missing file -> {missing}"
            await step("fs write/read", files)

            async def mkdir():
                await box.fs.mkdir("probe-dir/nested/deeper")
                must(await box.fs.is_dir("probe-dir/nested/deeper"), "mkdir did not create")
                return "recursive mkdir OK"
            await step("fs mkdir", mkdir)

            async def port():
                await box.fs.write_text("www/index.html", "served-by-sandbox\n")
                await box.create_process("python3", ["-m", "http.server", "3000", "--directory", "www"])
                route = next(r for r in box.routes if r.port == 3000)
                last = ""
                for _ in range(30):
                    try:
                        status, body = await asyncio.to_thread(http_get, port_url(route))
                        if status == 200 and body == "served-by-sandbox\n":
                            return f"route.subdomain={route.subdomain[:8]}... url host suffix={route.url.split('.', 1)[-1]}"
                        last = f"{status} {body[:80]!r}"
                    except Exception as e:  # noqa: BLE001
                        last = str(e)
                    await asyncio.sleep(1)
                raise AssertionError(f"never served: {last}")
            await step("port via route url", port)

            async def extend():
                s = await box.extend_execution_time_limit(60)
                return f"session={s.id}"
            await step("extend_execution_time_limit", extend)

            async def query():
                names = []
                async for s in sandbox.query_sandboxes(query=sandbox.SandboxQueryByName(name_prefix=PREFIX)):
                    names.append(f"{s.name}:{s.status}")
                must(any(n.startswith(box.name + ":") for n in names), f"not listed: {names}")
                return f"{names}"
            await step("query_sandboxes", query)

            async def stop():
                await box.stop()
                return f"status={box.status}"
            await step("stop", stop)

            async def after_stop():
                # A non-persistent sandbox has no filesystem to resume from: this records
                # what the API answers (410 on the stopped session, then a resume attempt).
                try:
                    r = await box.run_process("echo", ["resumed"], capture_output=True)
                    return f"ran in a new session {box.current_session_id}: {r.stdout.strip()!r}"
                except SandboxApiError as e:
                    return f"SandboxApiError status={e.status_code} code={e.code} data={e.data!r}"
                except Exception as e:  # noqa: BLE001
                    return f"{type(e).__name__}: {e}"
            await step("run_process after stop (records behaviour)", after_stop)

            async def missing():
                try:
                    await sandbox.get_sandbox(name=f"{PREFIX}-missing")
                    return "no error?!"
                except SandboxApiError as e:
                    must(e.status_code == 404, f"status {e.status_code}")
                    return f"status=404 code={e.code}"
            await step("get_sandbox missing", missing)
        finally:
            if box is not None:
                try:
                    await box.stop()
                except Exception:  # noqa: BLE001 - already stopped is fine
                    pass
                try:
                    await box.destroy(delete_orphan_snapshots=True)
                    log(f"cleanup: destroyed {box.name}")
                except Exception as e:  # noqa: BLE001
                    failures += 1
                    log(f"cleanup: destroy FAILED: {e}")
    log(f"{failures} failure(s)")


asyncio.run(main())
raise SystemExit(1 if failures else 0)
