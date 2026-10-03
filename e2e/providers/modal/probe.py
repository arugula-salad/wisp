"""The checks beyond the target that ride on the same RPCs: exit codes,
stderr, env and workdir, a second exec, Sandbox.from_id, poll and wait,
terminate's result, a sandbox's timeout and its entrypoint's exit, an exec
timeout, a large output, and an image this server cannot build.

Prints one PASS/FAIL line per check; the exit status is the number of failures.
"""

import sys
import time
import traceback

import modal

failures = 0


def check(name, fn):
    global failures
    t0 = time.monotonic()
    try:
        fn()
        print(f"PASS {name} ({time.monotonic() - t0:.1f}s)", flush=True)
    except Exception as e:
        failures += 1
        print(f"FAIL {name}: {type(e).__name__}: {e}", flush=True)
        traceback.print_exc(file=sys.stderr)


def eq(got, want):
    assert got == want, f"got {got!r}, want {want!r}"


app = modal.App.lookup("wisp-spike", create_if_missing=True)
image = modal.Image.debian_slim()
sb = modal.Sandbox.create("sleep", "infinity", app=app, image=image)
print(f"sandbox {sb.object_id}", flush=True)


def echo():
    p = sb.exec("echo", "hi")
    eq(p.stdout.read(), "hi\n")
    eq(p.wait(), 0)
    eq(p.returncode, 0)


def stderr_and_exit_code():
    p = sb.exec("sh", "-c", "echo out; echo oops >&2; exit 3")
    eq(p.stdout.read(), "out\n")
    eq(p.stderr.read(), "oops\n")
    eq(p.wait(), 3)
    eq(p.returncode, 3)


def poll_while_running():
    p = sb.exec("sleep", "2")
    eq(p.poll(), None)
    eq(p.wait(), 0)
    eq(p.poll(), 0)


def as_root_with_python():
    p = sb.exec("sh", "-c", "id -un; echo $HOME; python3 -c 'import sys; print(sys.version_info[0])'")
    eq(p.stdout.read(), "root\n/root\n3\n")
    eq(p.wait(), 0)


def env_and_workdir():
    p = sb.exec("sh", "-c", "echo $FOO; pwd", env={"FOO": "bar"}, workdir="/tmp")
    eq(p.stdout.read(), "bar\n/tmp\n")


def state_persists():
    eq(sb.exec("sh", "-c", "echo kept > /root/f").wait(), 0)
    eq(sb.exec("cat", "/root/f").stdout.read(), "kept\n")


def missing_command():
    p = sb.exec("no-such-command")
    p.stdout.read()
    eq(p.wait(), 127)


def large_output():
    p = sb.exec("sh", "-c", "head -c 3000000 /dev/zero | tr '\\0' a; echo")
    out = p.stdout.read()
    eq(len(out), 3000001)
    eq(p.wait(), 0)


def exec_timeout():
    p = sb.exec("sleep", "60", timeout=2)
    t0 = time.monotonic()
    try:
        code = p.wait()
    except modal.exception.ExecTimeoutError:
        code = "timeout"
    assert time.monotonic() - t0 < 15, "the exec outlived its timeout"
    assert code in ("timeout", 137, -1), f"got {code!r}"


def from_id():
    other = modal.Sandbox.from_id(sb.object_id)
    eq(other.object_id, sb.object_id)
    eq(other.poll(), None)
    p = other.exec("echo", "again")
    eq(p.stdout.read(), "again\n")


def terminate():
    eq(sb.poll(), None)
    sb.terminate()
    sb.wait(raise_on_termination=False)
    eq(sb.returncode, 137)
    eq(modal.Sandbox.from_id(sb.object_id).poll(), 137)


def sandbox_timeout():
    s = modal.Sandbox.create("sleep", "infinity", app=app, image=image, timeout=5)
    t0 = time.monotonic()
    try:
        s.wait()
        raise AssertionError("wait() returned; want SandboxTimeoutError")
    except modal.exception.SandboxTimeoutError:
        pass
    assert 3 < time.monotonic() - t0 < 30, f"timed out after {time.monotonic() - t0:.1f}s"
    eq(s.returncode, 124)


def entrypoint_exit():
    s = modal.Sandbox.create("sh", "-c", "sleep 1; exit 7", app=app, image=image)
    s.wait(raise_on_termination=False)
    eq(s.returncode, 7)


def unknown_image():
    try:
        modal.Sandbox.create("true", app=app, image=modal.Image.debian_slim().pip_install("requests"))
        raise AssertionError("created; want an error")
    except modal.exception.ConflictError as e:
        assert "prebuilt" in str(e), str(e)


for name, fn in [
    ("echo hi, wait, returncode", echo),
    ("stderr and a non-zero exit", stderr_and_exit_code),
    ("poll while running", poll_while_running),
    ("root, HOME, python3", as_root_with_python),
    ("env and workdir", env_and_workdir),
    ("files persist between execs", state_persists),
    ("missing command exits 127", missing_command),
    ("3 MB of output", large_output),
    ("exec timeout", exec_timeout),
    ("Sandbox.from_id", from_id),
    ("terminate: returncode 137", terminate),
    ("timeout=5 on create", sandbox_timeout),
    ("entrypoint exit code", entrypoint_exit),
    ("unbuildable image is refused", unknown_image),
]:
    check(name, fn)

print(f"{failures} failure(s)")
sys.exit(failures)
