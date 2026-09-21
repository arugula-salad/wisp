# Usage: python probe.py <baseURL> <token> <sprite>
import sys, time, threading, collections, concurrent.futures as cf
import websockets.asyncio.client as wsc
base, token, name = sys.argv[1:4]

opened = collections.Counter()
_orig = wsc.connect
class counting(_orig):                      # connect is a class; record the URI of every socket the SDK opens
    def __init__(self, uri, *a, **k):
        opened[str(uri).split("?")[0].rstrip("/").split("/")[-1]] += 1
        super().__init__(uri, *a, **k)
import sprites.websocket, sprites.control
sprites.websocket.connect = counting
sprites.control.connect = counting
from sprites import SpritesClient

def tally():
    t = dict(opened); opened.clear(); return t

def bounded(fn, secs, what):
    with cf.ThreadPoolExecutor(1) as ex:
        f = ex.submit(fn)
        try: return f.result(timeout=secs)
        except cf.TimeoutError: raise RuntimeError(f"HUNG: {what} still pending after {secs}s")

for control in (False, True):
    sp = SpritesClient(token, base_url=base, control_mode=control).sprite(name)
    label = f"control_mode={control}"
    try:
        for i in range(10):
            out = bounded(lambda: sp.command("sh", "-c", f"echo seq-{i}").output(), 8, f"exec {i}")
            assert out.decode().strip() == f"seq-{i}", out
        with cf.ThreadPoolExecutor(15) as ex:
            outs = list(ex.map(lambda i: sp.command("sh", "-c", f"sleep 0.2; echo par-{i}", timeout=20).output().decode().strip(), range(15)))
        assert outs == [f"par-{i}" for i in range(15)], outs
        code = 0
        try: sp.command("sh", "-c", "exit 7").output()
        except Exception as e:
            code = getattr(e, "exit_code", None)
            code = code() if callable(code) else code
        print(f"[py] {label}: 10 sequential + 15 concurrent execs OK, exit code propagated={code}; sockets opened: {tally()} (mode in use: {sp.use_control_mode()})")
    except Exception as e:
        print(f"[py] {label}: EXEC FAILED: {e!r}; sockets: {tally()}")
    print(f"[py] {label}: proxy: not applicable, the Python SDK has no port-proxy feature")

    try:
        for _ in sp.create_checkpoint("probe"): pass
        cps = sp.list_checkpoints()
        result = {}
        def victim():
            try: sp.command("sleep", "300").output(); result["how"] = "exited 0?!"
            except Exception as e: result["how"] = f"ended: {str(e)[:70] or type(e).__name__}"
        th = threading.Thread(target=victim, daemon=True); th.start(); time.sleep(0.7)
        for _ in sp.restore_checkpoint(cps[0].id): pass
        t0 = time.time(); th.join(15)
        print(f"[py] {label}: exec under a restored VM -> " + (result.get("how", "HUNG: still blocked 15s after the VM was restored away")) + f" (after {int((time.time()-t0)*1000)}ms); sockets: {tally()}")
    except Exception as e:
        print(f"[py] {label}: VANISH: {e!r}; sockets: {tally()}")
