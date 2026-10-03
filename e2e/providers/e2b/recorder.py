#!/usr/bin/env python3
"""Logging reverse proxy for recording what the official E2B SDKs send and receive.

    recorder.py --listen 127.0.0.1:8790 --out golden/py.jsonl [--domain e2b.app]
    recorder.py --pretty golden/py.jsonl > golden/py.json

Point an SDK at it with:

    E2B_API_URL=http://127.0.0.1:8790      control plane  -> https://api.<domain>
    E2B_SANDBOX_URL=http://127.0.0.1:8790  envd           -> https://sandbox.<domain>
    E2B_HTTP_VERSION=1.1                   (plain-HTTP origin; this proxy speaks HTTP/1.1)

Routing, per request:
  * an `E2b-Sandbox-Id` header (every envd call the SDK makes through its Sandbox object)
    goes to https://sandbox.<domain>, which routes on those headers;
  * no routing header but an envd path (/files, /health, /process.*, /filesystem.*): the
    signed upload/download URLs the SDK builds from E2B_SANDBOX_URL carry no headers, so
    they go to https://49983-<last sandbox id seen>.<domain>;
  * a Host header naming a sandbox port host (`<port>-<id>.<domain>`, sent by the probes
    for the port check) goes to https://<that host>;
  * everything else goes to https://api.<domain>.

Bodies stream both ways (Connect server-streams stay open for minutes), and each
`application/connect+json` envelope is recorded as a separate frame with its arrival
time. Secrets are scrubbed before anything is written: auth headers, `*Token` JSON
fields, `signature` query values, and every literal token value seen in a response.
Stdlib only.
"""
import argparse, base64, gzip, http.client, http.server, json, os, re, socketserver, ssl, sys, threading, time
import urllib.parse

SECRET_HEADERS = {"authorization", "x-api-key", "x-access-token", "e2b-traffic-access-token",
                  "cookie", "set-cookie", "x-admin-token", "proxy-authorization"}
HOP = {"connection", "keep-alive", "proxy-connection", "te", "trailer", "transfer-encoding", "upgrade"}
ENVD_PATH = re.compile(r"^/(files|health|metrics|envs|process\.Process/|filesystem\.Filesystem/)")
PORT_HOST = re.compile(r"^(\d+)-([a-z0-9]+)\.(.+)$")
TOKEN_KEY = re.compile(r"(?i)(token|apikey|api_key|secret|password)$")
MAX_BODY = 64 * 1024


class Recorder:
    def __init__(self, out, domain):
        self.out, self.domain = out, domain
        self.lock = threading.Lock()
        self.seq = 0
        self.t0 = time.time()
        self.secrets = set(filter(None, [os.environ.get("E2B_API_KEY"), os.environ.get("E2B_ACCESS_TOKEN")]))
        self.last_sandbox = None

    def next_seq(self):
        with self.lock:
            self.seq += 1
            return self.seq

    def learn(self, obj):
        """Remember token values from JSON so they can be scrubbed anywhere they reappear."""
        if isinstance(obj, dict):
            for k, v in obj.items():
                if isinstance(v, str) and TOKEN_KEY.search(k) and len(v) >= 8:
                    self.secrets.add(v)
                self.learn(v)
        elif isinstance(obj, list):
            for v in obj:
                self.learn(v)

    def scrub_obj(self, obj):
        if isinstance(obj, dict):
            return {k: ("<redacted>" if isinstance(v, str) and v and TOKEN_KEY.search(k) else self.scrub_obj(v))
                    for k, v in obj.items()}
        if isinstance(obj, list):
            return [self.scrub_obj(v) for v in obj]
        if isinstance(obj, str):
            return self.scrub_str(obj)
        return obj

    def scrub_str(self, s):
        for sec in self.secrets:
            s = s.replace(sec, "<redacted>")
        return re.sub(r"(signature=)[^&\s\"]+", r"\1<redacted>", s)

    def headers(self, items):
        out = []
        for k, v in items:
            lk = k.lower()
            if lk in SECRET_HEADERS:
                note = ""
                if lk == "authorization" and v.startswith("Basic "):
                    try:  # envd selects the Unix user with Basic auth, "<user>:" and no password
                        user, _, pw = base64.b64decode(v[6:]).decode().partition(":")
                        if not pw:
                            note = f" (basic, user={user!r})"
                    except Exception:
                        pass
                v = "<redacted>" + note
            out.append(f"{k}: {self.scrub_str(v)}")
        return out

    def body(self, raw, ctype, encoding=""):
        if not raw:
            return None
        if encoding == "gzip":
            try:
                raw = gzip.decompress(raw)
            except Exception:
                pass
        trunc = len(raw) > MAX_BODY
        data = raw[:MAX_BODY]
        if "json" in (ctype or ""):
            try:
                obj = json.loads(data)
                self.learn(obj)
                return {"json": self.scrub_obj(obj)}
            except Exception:
                pass
        try:
            text = data.decode("utf-8")
            return {"text": self.scrub_str(text), **({"truncated_from": len(raw)} if trunc else {})}
        except UnicodeDecodeError:
            return {"base64": base64.b64encode(data).decode(), **({"truncated_from": len(raw)} if trunc else {})}

    def frame(self, flags, payload):
        """One Connect envelope. Process output is base64 (proto bytes in JSON); a decoded
        copy is added next to it for reading, the wire value stays in "json"."""
        try:
            obj = json.loads(payload)
        except Exception:
            return {"flags": flags, "base64": base64.b64encode(payload).decode()}
        self.learn(obj)
        f = {"flags": flags, "json": self.scrub_obj(obj)}
        data = (obj.get("event") or {}).get("data") if isinstance(obj, dict) else None
        if isinstance(data, dict):
            try:
                f["decoded"] = {k: self.scrub_str(base64.b64decode(v).decode("utf-8", "replace")) for k, v in data.items()}
            except Exception:
                pass
        return f

    def envelopes(self, raw):
        frames = []
        while len(raw) >= 5:
            n = int.from_bytes(raw[1:5], "big")
            frames.append(self.frame(raw[0], raw[5:5 + n]))
            raw = raw[5 + n:]
        return frames

    def write(self, rec):
        line = json.dumps(rec, sort_keys=False)
        line = self.scrub_str(line)  # last line of defence: any literal secret anywhere
        with self.lock:
            with open(self.out, "a") as f:
                f.write(line + "\n")


class Handler(http.server.BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"
    rec: Recorder = None

    def log_message(self, *a):
        pass

    def read_request_body(self):
        if self.headers.get("Transfer-Encoding", "").lower() == "chunked":
            buf = b""
            while True:
                size = int(self.rfile.readline().split(b";")[0].strip(), 16)
                if size == 0:
                    while self.rfile.readline() not in (b"\r\n", b"\n", b""):
                        pass
                    return buf
                buf += self.rfile.read(size)
                self.rfile.readline()
        n = int(self.headers.get("Content-Length") or 0)
        return self.rfile.read(n) if n else b""

    def route(self):
        d = self.rec.domain
        sid = self.headers.get("E2b-Sandbox-Id")
        host = (self.headers.get("Host") or "").split(":")[0]
        m = PORT_HOST.match(host)
        if sid:
            self.rec.last_sandbox = sid
            return "sandbox", f"sandbox.{d}"
        if m:
            return "port", host
        if ENVD_PATH.match(self.path) and self.rec.last_sandbox:
            return "envd-direct", f"49983-{self.rec.last_sandbox}.{d}"
        return "api", f"api.{d}"

    def proxy(self):
        rec = self.rec
        seq, t_start = rec.next_seq(), time.time()
        kind, upstream = self.route()
        body = self.read_request_body()
        hdrs = [(k, v) for k, v in self.headers.items() if k.lower() not in HOP and k.lower() != "host"]
        record = {
            "seq": seq, "t": round(t_start - rec.t0, 3), "route": kind, "upstream": upstream,
            "request": {"method": self.command, "path": rec.scrub_str(self.path),
                        "host_sent_by_sdk": self.headers.get("Host"),
                        "headers": rec.headers(self.headers.items())},
        }
        if (self.headers.get("Content-Type") or "").startswith("application/connect+"):
            record["request"]["frames"] = rec.envelopes(body)
        else:
            record["request"]["body"] = rec.body(body, self.headers.get("Content-Type"), self.headers.get("Content-Encoding", ""))
        try:
            conn = http.client.HTTPSConnection(upstream, timeout=600, context=ssl.create_default_context())
            conn.putrequest(self.command, self.path, skip_host=True, skip_accept_encoding=True)
            conn.putheader("Host", upstream)
            for k, v in hdrs:
                if k.lower() != "content-length":
                    conn.putheader(k, v)
            conn.putheader("Content-Length", str(len(body)))
            conn.endheaders(body)
            resp = conn.getresponse()
        except Exception as e:
            record["error"] = f"upstream: {e!r}"
            rec.write(record)
            self.send_error(502, f"recorder: upstream {upstream}: {e}")
            return

        ctype = resp.getheader("Content-Type", "")
        streaming = ctype.startswith("application/connect+")
        record["response"] = {"status": resp.status, "headers": rec.headers(resp.getheaders())}
        self.send_response_only(resp.status, resp.reason)
        for k, v in resp.getheaders():
            if k.lower() not in HOP and k.lower() != "content-length":
                self.send_header(k, v)
        no_body = self.command == "HEAD" or resp.status in (204, 304) or 100 <= resp.status < 200
        if not no_body:
            self.send_header("Transfer-Encoding", "chunked")
        self.end_headers()

        raw, frames, pending = b"", [], b""
        try:
            while not no_body:
                chunk = resp.read1(65536)
                if not chunk:
                    break
                self.wfile.write(b"%x\r\n%s\r\n" % (len(chunk), chunk))
                self.wfile.flush()
                if streaming:
                    pending += chunk
                    while len(pending) >= 5:
                        n = int.from_bytes(pending[1:5], "big")
                        if len(pending) < 5 + n:
                            break
                        f = rec.frame(pending[0], pending[5:5 + n])
                        f["t"] = round(time.time() - t_start, 3)
                        frames.append(f)
                        pending = pending[5 + n:]
                elif len(raw) <= MAX_BODY:
                    raw += chunk
            if not no_body:
                self.wfile.write(b"0\r\n\r\n")
                self.wfile.flush()
        except (BrokenPipeError, ConnectionResetError) as e:
            record["client_disconnected"] = repr(e)
        except Exception as e:
            record["error"] = f"relay: {e!r}"
        finally:
            conn.close()
        if streaming:
            record["response"]["frames"] = frames
        else:
            record["response"]["body"] = rec.body(raw, ctype, resp.getheader("Content-Encoding", ""))
        record["response"]["duration_s"] = round(time.time() - t_start, 3)
        rec.write(record)

    do_GET = do_POST = do_PUT = do_DELETE = do_PATCH = do_HEAD = do_OPTIONS = proxy


class Server(socketserver.ThreadingMixIn, http.server.HTTPServer):
    daemon_threads = True
    allow_reuse_address = True


def pretty(path):
    recs = [json.loads(l) for l in open(path) if l.strip()]
    recs.sort(key=lambda r: r["seq"])
    json.dump(recs, sys.stdout, indent=1)
    sys.stdout.write("\n")


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--listen", default="127.0.0.1:8790")
    ap.add_argument("--out")
    ap.add_argument("--domain", default=os.environ.get("E2B_RECORD_DOMAIN", "e2b.app"))
    ap.add_argument("--pretty")
    a = ap.parse_args()
    if a.pretty:
        return pretty(a.pretty)
    host, port = a.listen.rsplit(":", 1)
    Handler.rec = Recorder(a.out, a.domain)
    srv = Server((host, int(port)), Handler)
    print(f"recorder: {a.listen} -> *.{a.domain}, writing {a.out}", file=sys.stderr, flush=True)
    srv.serve_forever()


if __name__ == "__main__":
    main()
