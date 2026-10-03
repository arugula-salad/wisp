# Daytona conformance probe for the official Python SDK (`daytona`, pinned in
# requirements.txt), through its public API only.
#
# Usage: python probe.py   (target from the SDK's own env: DAYTONA_API_URL and
#   DAYTONA_API_KEY, which it reads from the process environment only; see run.sh)
# Probe-only knobs:
#   DAYTONA_PROBE_RUN    run id put in a sandbox label, so sweep.py can find leaks
#   DAYTONA_PROBE_STOP   0 to skip stop/start
# Prints one "[py] <step>: OK|FAILED|SKIP ..." line per step. Exit status = failures.
import asyncio, http.client, os, sys, time, traceback, urllib.parse
from daytona import (CodeRunParams, CreateSandboxFromSnapshotParams, Daytona, DaytonaError,
                     DaytonaNotFoundError, FileDownloadRequest, FileUpload, ListSandboxesQuery,
                     SandboxState, SessionExecuteRequest)

RUN = os.environ.get("DAYTONA_PROBE_RUN", f"py-{os.getpid()}")
LABELS = {"probe": "wisp-daytona", "run": RUN}
fails = 0


class Skip(Exception):
    pass


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
        traceback.print_exc(limit=4)


def check(cond, what):
    if not cond:
        raise AssertionError(what)


def http_get(url, headers=None):
    u = urllib.parse.urlsplit(url)
    cls = http.client.HTTPSConnection if u.scheme == "https" else http.client.HTTPConnection
    c = cls(u.hostname, u.port, timeout=20)
    # The preview host (<port>-<id>.daytona.localhost) need not resolve: connect
    # to the API's address and send the preview host as Host. *.localhost does
    # resolve to loopback on most systems, but not everywhere.
    api = urllib.parse.urlsplit(os.environ.get("DAYTONA_API_URL", ""))
    if u.hostname.endswith(".localhost") and api.hostname:
        c = cls(api.hostname, u.port or api.port, timeout=20)
    c.putrequest("GET", (u.path or "/") + (("?" + u.query) if u.query else ""), skip_host=True)
    c.putheader("Host", u.netloc)
    for k, v in (headers or {}).items():
        c.putheader(k, v)
    c.endheaders()
    r = c.getresponse()
    data = r.read()
    c.close()
    return r.status, data


daytona = Daytona()
sbx = None
state = {}


def create():
    global sbx
    params = CreateSandboxFromSnapshotParams(language="python", labels=LABELS, env_vars={"PROBE_ENV": "from-create"},
                                             auto_stop_interval=30)
    sbx = daytona.create(params, timeout=120)
    check(sbx.id, "no sandbox id")
    check(sbx.state == SandboxState.STARTED, f"state {sbx.state}")
    check(sbx.labels.get("code-toolbox-language") == "python", f"labels {sbx.labels}")
    check(sbx.labels.get("run") == RUN, f"labels {sbx.labels}")
    check(sbx.auto_stop_interval == 30, f"auto_stop_interval {sbx.auto_stop_interval}")
    return f"id={sbx.id} name={sbx.name} user={sbx.user} cpu={sbx.cpu} mem={sbx.memory} disk={sbx.disk} target={sbx.target}"


def get_list():
    got = daytona.get(sbx.id)
    check(got.id == sbx.id and got.state == SandboxState.STARTED, f"get: {got.id} {got.state}")
    by_name = daytona.get(sbx.name)
    check(by_name.id == sbx.id, "get by name")
    ids = [s.id for s in daytona.list(ListSandboxesQuery(labels={"run": RUN}))]
    check(ids == [sbx.id], f"list by label: {ids}")
    none = list(daytona.list(ListSandboxesQuery(labels={"run": RUN + "-nothing"})))
    check(none == [], f"list by another label: {none}")
    try:
        daytona.get("00000000-0000-4000-8000-000000000000")
        raise AssertionError("get of a missing sandbox succeeded")
    except DaytonaNotFoundError:
        pass
    return f"listed {len(ids)}"


def labels_autostop():
    out = sbx.set_labels({**LABELS, "code-toolbox-language": "python", "extra": "yes"})
    check(out.get("extra") == "yes", f"set_labels: {out}")
    sbx.set_autostop_interval(45)
    sbx.refresh_data()
    check(sbx.labels.get("extra") == "yes", f"labels after refresh: {sbx.labels}")
    check(sbx.auto_stop_interval == 45, f"auto_stop_interval after set: {sbx.auto_stop_interval}")
    return ""


def info():
    home, wd = sbx.get_user_home_dir(), sbx.get_work_dir()
    check(home == "/home/daytona" and wd == "/home/daytona", f"home={home} workdir={wd}")
    r = sbx.process.exec("whoami && echo $HOME && pwd && echo $PROBE_ENV")
    check(r.exit_code == 0, f"exit {r.exit_code}: {r.result}")
    check(r.result.split() == ["daytona", "/home/daytona", "/home/daytona", "from-create"], f"result {r.result!r}")
    return ""


def exec_():
    r = sbx.process.exec("echo hello; echo oops >&2; exit 3")
    check(r.exit_code == 3, f"exit code {r.exit_code}")
    check("hello" in r.result and r.artifacts.stdout == r.result, f"result {r.result!r}")
    r = sbx.process.exec("pwd; echo $GREETING", cwd="/tmp", env={"GREETING": "hi there"})
    check(r.exit_code == 0 and r.result.split("\n")[:2] == ["/tmp", "hi there"], f"cwd/env: {r.result!r}")
    r = sbx.process.exec("echo 'quoted | piped' | tr a-z A-Z")
    check(r.result.strip() == "QUOTED | PIPED", f"shell syntax: {r.result!r}")
    t0 = time.time()
    try:
        sbx.process.exec("sleep 30", timeout=2)
        raise AssertionError("a command past its timeout returned")
    except DaytonaError as e:
        check(time.time() - t0 < 15, f"timeout took {time.time()-t0:.1f}s")
        state["timeout_err"] = type(e).__name__
    return f"timeout -> {state['timeout_err']}"


def code_run():
    r = sbx.process.code_run('import sys\nx, y = 10, 20\nprint(f"Sum: {x + y}")\nprint(sys.argv[1:])',
                             params=CodeRunParams(argv=["a", "b c"], env={"X": "1"}))
    check(r.exit_code == 0, f"exit {r.exit_code}: {r.result}")
    check(r.result.split("\n")[:2] == ["Sum: 30", "['a', 'b c']"], f"result {r.result!r}")
    r = sbx.process.code_run("raise SystemExit(4)")
    check(r.exit_code == 4, f"exit code {r.exit_code}")
    return ""


def sessions():
    sid = "probe-session"
    sbx.process.create_session(sid)
    r = sbx.process.execute_session_command(sid, SessionExecuteRequest(command="cd /tmp && export FOO=bar && mkdir -p probe-dir"))
    check(r.exit_code == 0, f"first command: {r.exit_code} {r.output!r}")
    r = sbx.process.execute_session_command(sid, SessionExecuteRequest(command="pwd; echo $FOO; echo to-stderr >&2"))
    check(r.exit_code == 0 and r.cmd_id, f"second command: {r}")
    check(r.stdout.split() == ["/tmp", "bar"], f"session state carried over: stdout {r.stdout!r}")
    check(r.stderr.strip() == "to-stderr", f"stderr {r.stderr!r}")
    r = sbx.process.execute_session_command(sid, SessionExecuteRequest(command="exit 7"))
    check(r.exit_code == 7, f"exit code {r.exit_code}")
    r = sbx.process.execute_session_command(sid, SessionExecuteRequest(command="pwd"))
    check(r.stdout.strip() == "/tmp", f"state after an exit: {r.stdout!r}")

    # Async, with the log stream followed over the WebSocket.
    cmd = "for i in 1 2 3; do echo out$i; echo err$i >&2; sleep 0.3; done"
    r = sbx.process.execute_session_command(sid, SessionExecuteRequest(command=cmd, run_async=True))
    cid = r.cmd_id
    check(cid and r.exit_code is None, f"async: {r}")
    out, err = [], []
    asyncio.run(sbx.process.get_session_command_logs_async(sid, cid, out.append, err.append))
    check("".join(out).split() == ["out1", "out2", "out3"], f"followed stdout {out!r}")
    check("".join(err).split() == ["err1", "err2", "err3"], f"followed stderr {err!r}")
    c = sbx.process.get_session_command(sid, cid)
    check(c.exit_code == 0 and c.command == cmd, f"command: {c}")
    logs = sbx.process.get_session_command_logs(sid, cid)
    check(logs.stdout.split() == ["out1", "out2", "out3"] and logs.stderr.split() == ["err1", "err2", "err3"], f"logs {logs}")

    # Input to a running command.
    r = sbx.process.execute_session_command(sid, SessionExecuteRequest(command="read line; echo got:$line", run_async=True))
    time.sleep(0.5)
    sbx.process.send_session_command_input(sid, r.cmd_id, "typed\n")
    for _ in range(50):
        c = sbx.process.get_session_command(sid, r.cmd_id)
        if c.exit_code is not None:
            break
        time.sleep(0.1)
    logs = sbx.process.get_session_command_logs(sid, r.cmd_id)
    check(c.exit_code == 0 and logs.stdout.strip() == "got:typed", f"input: exit {c.exit_code} {logs.stdout!r}")

    s = sbx.process.get_session(sid)
    check(s.session_id == sid and len(s.commands) == 6, f"session: {len(s.commands)} commands")
    check(sid in [x.session_id for x in sbx.process.list_sessions()], "list_sessions")
    sbx.process.delete_session(sid)
    try:
        sbx.process.get_session(sid)
        raise AssertionError("deleted session still there")
    except DaytonaNotFoundError:
        pass
    return f"{len(s.commands)} commands"


def files():
    fs = sbx.fs
    fs.create_folder("probe/sub", "755")
    fs.upload_file(b"hello file", "probe/a.txt")
    fs.upload_files([FileUpload(source=b"one", destination="/home/daytona/probe/sub/1.txt"),
                     FileUpload(source=b"\x00\x01binary\xff", destination="probe/sub/2.bin")])
    names = sorted(f.name for f in fs.list_files("probe"))
    check(names == ["a.txt", "sub"], f"list {names}")
    info = fs.get_file_info("/home/daytona/probe/a.txt")
    check(info.size == 10 and not info.is_dir and info.owner == "daytona", f"info {info}")
    check(fs.get_file_info("probe/sub").is_dir, "info of a directory")
    check(fs.download_file("probe/a.txt") == b"hello file", "download_file")
    res = fs.download_files([FileDownloadRequest(source="probe/sub/1.txt"), FileDownloadRequest(source="probe/sub/2.bin"),
                             FileDownloadRequest(source="probe/missing")])
    check(res[0].result == b"one" and res[1].result == b"\x00\x01binary\xff", f"bulk download {res[:2]}")
    check(res[2].error and res[2].result is None, f"missing file in bulk download: {res[2]}")
    try:
        fs.download_file("probe/missing")
        raise AssertionError("download of a missing file succeeded")
    except DaytonaNotFoundError as e:
        state["missing_err"] = type(e).__name__
    fs.move_files("probe/a.txt", "probe/b.txt")
    check(sorted(f.name for f in fs.list_files("probe")) == ["b.txt", "sub"], "move")
    fs.delete_file("probe/b.txt")
    fs.delete_file("probe", recursive=True)
    try:
        fs.get_file_info("probe")
        raise AssertionError("deleted directory still there")
    except DaytonaNotFoundError:
        pass
    return f"missing -> {state['missing_err']}"


def preview():
    sbx.process.create_session("web")
    sbx.process.execute_session_command("web", SessionExecuteRequest(
        command="mkdir -p /tmp/www && echo preview-ok > /tmp/www/index.html && cd /tmp/www && python3 -m http.server 8080",
        run_async=True))
    link = sbx.get_preview_link(8080)
    check(link.url and link.token, f"link {link}")
    status, body = None, b""
    for _ in range(50):
        status, body = http_get(link.url + "/index.html", {"x-daytona-preview-token": link.token})
        if status == 200:
            break
        time.sleep(0.2)
    check(status == 200 and body.strip() == b"preview-ok", f"preview with token: {status} {body[:200]!r}")
    status, _ = http_get(link.url + "/index.html")
    check(status in (401, 403), f"preview without a token: {status}")
    sbx.process.delete_session("web")
    return link.url


def stop_start():
    if os.environ.get("DAYTONA_PROBE_STOP") == "0":
        raise Skip("DAYTONA_PROBE_STOP=0")
    sbx.process.exec("echo kept > /home/daytona/persist.txt")
    sbx.stop()
    check(sbx.state == SandboxState.STOPPED, f"after stop: {sbx.state}")
    check(daytona.get(sbx.id).state == SandboxState.STOPPED, "get after stop")
    try:
        sbx.process.exec("true")
        raise AssertionError("exec on a stopped sandbox succeeded")
    except DaytonaError:
        pass
    sbx.start()
    check(sbx.state == SandboxState.STARTED, f"after start: {sbx.state}")
    r = sbx.process.exec("cat /home/daytona/persist.txt")
    check(r.result.strip() == "kept", f"file across stop/start: {r.result!r}")
    return ""


def delete():
    sbx.delete()
    try:
        daytona.get(sbx.id)
        raise AssertionError("deleted sandbox still there")
    except DaytonaNotFoundError:
        pass
    return ""


step("create", create)
if sbx is not None:
    for name, fn in [("get_list", get_list), ("labels_autostop", labels_autostop), ("info", info), ("exec", exec_),
                     ("code_run", code_run), ("sessions", sessions), ("files", files), ("preview", preview),
                     ("stop_start", stop_start), ("delete", delete)]:
        step(name, fn)
sys.exit(fails)
