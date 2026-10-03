# E2B conformance probe for the official Python SDK (`e2b`, pinned in run.sh).
#
# Usage: python probe.py   (target comes from the SDK's own env vars: E2B_API_KEY,
#   E2B_DOMAIN, E2B_API_URL, E2B_SANDBOX_URL; see run.sh)
# Probe-only knobs:
#   E2B_PROBE_RUN        run id put in sandbox metadata, so run.sh can find leaks
#   E2B_PROBE_PORT_VIA   http://host:port to send the port check through (the recorder);
#                        the sandbox host from get_host() is then sent as the Host header
#   E2B_PROBE_PORT_SCHEME  https (default) or http, for the get_host() URL
#   E2B_PROBE_PAUSE      0 to skip pause/resume
# Prints one "[py] <step>: OK|FAILED|SKIP ..." line per step. Exit status = failures.
import http.client, os, sys, time, traceback, urllib.parse, urllib.request
from importlib.metadata import version
from e2b import Sandbox, SandboxQuery, CommandExitException as CommandExitError, NotFoundException as NotFoundError

RUN = os.environ.get("E2B_PROBE_RUN", f"py-{os.getpid()}")
META = {"probe": "wisp-e2b", "run": RUN}
fails = 0


def step(name, fn):
    global fails
    t0 = time.time()
    try:
        msg = fn()
        print(f"[py] {name}: OK ({int((time.time()-t0)*1000)}ms){' ' + msg if msg else ''}", flush=True)
    except Skip as e:
        print(f"[py] {name}: SKIP {e}", flush=True)
    except Exception as e:
        fails += 1
        print(f"[py] {name}: FAILED {e!r}", flush=True)
        traceback.print_exc(limit=3)


class Skip(Exception):
    pass


def check(cond, what):
    if not cond:
        raise AssertionError(what)


def http_get(url_or_host_path, host=None, method="GET", body=None, headers=None):
    """GET either directly or through E2B_PROBE_PORT_VIA with an explicit Host header."""
    via = os.environ.get("E2B_PROBE_PORT_VIA")
    u = urllib.parse.urlsplit(url_or_host_path)
    if via and host:
        v = urllib.parse.urlsplit(via)
        c = http.client.HTTPConnection(v.hostname, v.port, timeout=20)
        c.putrequest(method, u.path + (("?" + u.query) if u.query else "") or "/", skip_host=True)
        c.putheader("Host", host)
    else:
        cls = http.client.HTTPSConnection if u.scheme == "https" else http.client.HTTPConnection
        c = cls(u.hostname, u.port, timeout=20)
        c.putrequest(method, (u.path or "/") + (("?" + u.query) if u.query else ""))
    for k, v in (headers or {}).items():
        c.putheader(k, v)
    if body is not None:
        c.putheader("Content-Length", str(len(body)))
    c.endheaders(body)
    r = c.getresponse()
    data = r.read()
    c.close()
    return r.status, data


sbx = None
state = {}


def create():
    global sbx
    sbx = Sandbox.create(timeout=300, metadata=META, envs={"PROBE_ENV": "from-create"})
    check(sbx.sandbox_id, "no sandbox id")
    info = sbx.get_info()
    state["end0"] = info.end_at
    return f"id={sbx.sandbox_id} template={info.template_id} name={getattr(info, 'name', None)} envd={info.envd_version} domain={sbx.sandbox_domain} cpu={info.cpu_count} mem={info.memory_mb}"


def exec_stream():
    out, err, times = [], [], []
    r = sbx.commands.run("for i in 1 2 3; do echo out-$i; echo err-$i >&2; sleep 0.4; done; echo $PROBE_ENV; whoami; pwd",
                         on_stdout=lambda s: (out.append(s), times.append(time.time())),
                         on_stderr=lambda s: err.append(s))
    check(r.exit_code == 0, f"exit {r.exit_code}")
    check("out-1" in r.stdout and "out-3" in r.stdout, r.stdout)
    check("err-2" in r.stderr, r.stderr)
    check("from-create" in r.stdout, "create-time env var missing: " + r.stdout)
    check(len(times) >= 2 and times[-1] - times[0] > 0.5, f"stdout not streamed: {len(times)} callbacks")
    lines = r.stdout.strip().splitlines()
    return f"{len(out)} stdout / {len(err)} stderr callbacks spread over {times[-1]-times[0]:.1f}s; user={lines[-2]} cwd={lines[-1]}"


def exec_exit_code():
    try:
        sbx.commands.run("echo partial; echo bad >&2; exit 7")
        raise AssertionError("no CommandExitError for exit 7")
    except CommandExitError as e:
        check(e.exit_code == 7, f"exit_code={e.exit_code}")
        check("partial" in e.stdout and "bad" in e.stderr, f"{e.stdout!r} {e.stderr!r}")
    r = sbx.commands.run("pwd; echo $FOO", cwd="/tmp", envs={"FOO": "bar"}, user="root")
    check(r.stdout.split() == ["/tmp", "bar"], r.stdout)
    return "exit 7 -> CommandExitError; cwd/envs/user honoured"


def background():
    h = sbx.commands.run("echo started; sleep 600", background=True)
    time.sleep(1)
    procs = sbx.commands.list()
    check(any(p.pid == h.pid for p in procs), f"pid {h.pid} not in {[p.pid for p in procs]}")
    mine = next(p for p in procs if p.pid == h.pid)
    check(sbx.commands.kill(h.pid), "kill returned False")
    try:
        h.wait()
        raise AssertionError("killed process exited 0")
    except CommandExitError as e:
        code = e.exit_code
    check(not any(p.pid == h.pid for p in sbx.commands.list()), "still listed after kill")
    check(sbx.commands.kill(h.pid) is False, "second kill should be False")
    return f"pid={h.pid} cmd={mine.cmd} args={mine.args} -> killed, exit_code={code}"


def stdin_pty():
    # stdin + reconnect (Process.Connect) + CloseStdin
    h = sbx.commands.run("cat", background=True, stdin=True)
    sbx.commands.send_stdin(h.pid, "line-1\n")
    h2 = sbx.commands.connect(h.pid)
    sbx.commands.send_stdin(h.pid, "line-2\n")
    sbx.commands.close_stdin(h.pid)
    r = h2.wait()
    check(r.stdout == "line-1\nline-2\n" or r.stdout == "line-2\n", f"cat via reconnect saw {r.stdout!r}")
    # PTY: Start with pty, SendInput(pty), Update (resize), output comes back as pty bytes
    from e2b import PtySize
    p = sbx.pty.create(PtySize(rows=24, cols=80))
    sbx.pty.resize(p.pid, PtySize(rows=30, cols=100))
    sbx.pty.send_stdin(p.pid, b"stty size; echo pty-$((40+2))\nexit\n")
    buf = bytearray()
    p.wait(on_pty=lambda d: buf.extend(d))
    out = buf.decode(errors="replace")
    check("pty-42" in out and "30 100" in out, out[-200:])
    return f"stdin+reconnect saw {r.stdout!r}; pty resized to 30x100 and echoed"


def files():
    base = f"/tmp/probe-{RUN}"
    w = sbx.files.write(f"{base}/a/hello.txt", "hello e2b\n")
    check(w.path == f"{base}/a/hello.txt", w.path)
    check(sbx.files.read(f"{base}/a/hello.txt") == "hello e2b\n", "read mismatch")
    check(sbx.files.make_dir(f"{base}/dir") is True, "make_dir")
    check(sbx.files.make_dir(f"{base}/dir") is False, "make_dir twice should be False")
    names = sorted(e.name for e in sbx.files.list(base))
    check(names == ["a", "dir"], names)
    deep = sorted(e.path for e in sbx.files.list(base, depth=2))
    check(f"{base}/a/hello.txt" in deep, deep)
    info = sbx.files.get_info(f"{base}/a/hello.txt")
    check(info.size == 10 and info.type.value == "file", f"{info}")
    sbx.files.rename(f"{base}/a/hello.txt", f"{base}/dir/moved.txt")
    check(not sbx.files.exists(f"{base}/a/hello.txt") and sbx.files.exists(f"{base}/dir/moved.txt"), "rename")
    sbx.files.remove(f"{base}/dir/moved.txt")
    check(not sbx.files.exists(f"{base}/dir/moved.txt"), "remove")
    try:
        sbx.files.read(f"{base}/nope")
        raise AssertionError("read of missing file succeeded")
    except NotFoundError:
        pass
    rel = sbx.files.write("probe-rel.txt", "rel")
    return f"write/read/list(depth 1,2)/stat/rename/remove OK; mode={oct(info.mode)} owner={info.owner}; relative path resolved to {rel.path}"


def upload_download():
    blob = bytes(range(256)) * 64
    sbx.files.write(f"/tmp/probe-{RUN}/blob.bin", blob)
    got = sbx.files.read(f"/tmp/probe-{RUN}/blob.bin", format="bytes")
    check(bytes(got) == blob, "binary roundtrip mismatch")
    # Signed URLs, fetched with plain HTTP (no SDK headers).
    up = sbx.upload_url(f"/tmp/probe-{RUN}/via-url.txt", use_signature_expiration=120)
    down = sbx.download_url(f"/tmp/probe-{RUN}/via-url.txt", use_signature_expiration=120)
    boundary = "probeboundary"
    body = (f"--{boundary}\r\nContent-Disposition: form-data; name=\"file\"; filename=\"via-url.txt\"\r\n"
            f"Content-Type: application/octet-stream\r\n\r\nuploaded-by-url\r\n--{boundary}--\r\n").encode()
    st, data = http_get(up, method="POST", body=body, headers={"Content-Type": f"multipart/form-data; boundary={boundary}"})
    check(st == 200, f"upload_url POST -> {st} {data[:200]!r}")
    st, data = http_get(down)
    check(st == 200 and data == b"uploaded-by-url", f"download_url GET -> {st} {data[:200]!r}")
    return f"{len(blob)} byte binary roundtrip; signed upload/download URLs OK"


def port():
    sbx.files.write("/tmp/www/index.html", "port-ok\n")
    h = sbx.commands.run("cd /tmp/www && python3 -m http.server 8080", background=True)
    state["web"] = h
    host = sbx.get_host(8080)
    scheme = os.environ.get("E2B_PROBE_PORT_SCHEME", "https")
    last = None
    for _ in range(30):
        try:
            st, data = http_get(f"{scheme}://{host}/", host=host)
            last = (st, data[:120])
            if st == 200 and data == b"port-ok\n":
                return f"GET {scheme}://{host}/ -> 200"
        except Exception as e:
            last = repr(e)
        time.sleep(1)
    raise AssertionError(f"port 8080 never served: {last}")


def set_timeout():
    sbx.set_timeout(600)
    end = sbx.get_info().end_at
    from datetime import datetime, timezone
    left = (end - datetime.now(timezone.utc)).total_seconds()
    check(550 < left < 650, f"endAt {end} is {left:.0f}s away, wanted ~600")
    return f"endAt moved to now+{left:.0f}s"


def metrics():
    m = sbx.get_metrics()
    return f"{len(m)} samples" + (f", last cpu={m[-1].cpu_used_pct}% mem={m[-1].mem_used}" if m else "")


def listing():
    items = Sandbox.list(query=SandboxQuery(metadata={"run": RUN})).next_items()
    ids = [s.sandbox_id for s in items]
    check(sbx.sandbox_id in ids, f"{sbx.sandbox_id} not in {ids}")
    s = items[ids.index(sbx.sandbox_id)]
    check(s.metadata.get("probe") == "wisp-e2b", s.metadata)
    return f"found by metadata; state={s.state}"


def pause_resume():
    if os.environ.get("E2B_PROBE_PAUSE", "1") == "0":
        raise Skip("E2B_PROBE_PAUSE=0")
    sbx.files.write("/tmp/before-pause.txt", "kept")
    h = sbx.commands.run("sleep 900", background=True)
    check(sbx.pause() is True, "pause returned False")
    st = sbx.get_info().state
    check(st.value == "paused" if hasattr(st, "value") else st == "paused", f"state {st}")
    again = sbx.pause()
    sbx.connect()
    st2 = sbx.get_info().state
    check(sbx.files.read("/tmp/before-pause.txt") == "kept", "file lost across pause")
    alive = any(p.pid == h.pid for p in sbx.commands.list())
    r = sbx.commands.run("echo resumed")
    check(r.stdout.strip() == "resumed", r.stdout)
    sbx.commands.kill(h.pid)
    return f"paused (state={st}), second pause -> {again}, connect resumed (state={st2}), files kept, process survived={alive}"


def kill():
    check(sbx.kill() is True, "kill returned False")
    try:
        sbx.get_info()
        raise AssertionError("get_info after kill succeeded")
    except NotFoundError:
        pass
    check(Sandbox.kill(sbx.sandbox_id) is False, "second kill should be False")
    try:
        Sandbox.connect(sbx.sandbox_id)
        raise AssertionError("connect after kill succeeded")
    except NotFoundError as e:
        msg = str(e)
    return f"killed; get_info -> NotFound; kill again -> False; connect -> {msg[:80]}"


print(f"[py] sdk e2b=={version('e2b')} api={os.environ.get('E2B_API_URL') or 'https://api.' + os.environ.get('E2B_DOMAIN', 'e2b.app')}"
      f" sandbox_url={os.environ.get('E2B_SANDBOX_URL', '(default)')}", flush=True)
try:
    step("create", create)
    if sbx:
        for name, fn in [("exec_stream", exec_stream), ("exec_exit_code", exec_exit_code),
                         ("background_kill", background), ("stdin_pty", stdin_pty), ("files", files),
                         ("upload_download", upload_download), ("port", port),
                         ("set_timeout", set_timeout), ("metrics", metrics), ("list", listing),
                         ("pause_resume", pause_resume), ("kill", kill)]:
            step(name, fn)
finally:
    if sbx:
        try:
            Sandbox.kill(sbx.sandbox_id)
        except Exception as e:
            print(f"[py] cleanup kill: {e!r}")
print(f"[py] == {fails} failure(s)", flush=True)
sys.exit(fails)
