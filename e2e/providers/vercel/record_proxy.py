#!/usr/bin/env python3
"""A logging reverse proxy for recording golden Vercel Sandbox API traces.

    python3 record_proxy.py --listen 127.0.0.1:0 --out golden/js [--upstream https://vercel.com]

Point an SDK at http://<listen>/api and every exchange is forwarded to the upstream
and written to <out>/NNN-METHOD-path.json: method, path, query, headers, body, status,
response headers, and the response body as the chunks arrived (with millisecond
offsets) so NDJSON streaming is visible. Responses are streamed through as they
arrive, so the SDK sees the same timing it would without the proxy.

Secrets never reach the files: the Authorization header and cookies are dropped,
and the values of VERCEL_TOKEN, VERCEL_OIDC_TOKEN, VERCEL_TEAM_ID and
VERCEL_PROJECT_ID (read from the environment) are replaced with "<redacted>"
wherever they appear. Binary bodies (the gzip tarball sent to fs/write, the octet
stream from fs/read) are stored as base64 plus their size and sha256.

Prints "listening on http://HOST:PORT" once ready.
"""
import argparse
import base64
import hashlib
import http.client
import http.server
import json
import os
import re
import socketserver
import ssl
import sys
import threading
import time
import urllib.parse

SECRET_ENV = ("VERCEL_TOKEN", "VERCEL_OIDC_TOKEN", "VERCEL_TEAM_ID", "VERCEL_PROJECT_ID")
DROP_HEADERS = {"authorization", "cookie", "set-cookie"}
HOP_HEADERS = {"connection", "keep-alive", "proxy-connection", "transfer-encoding", "te", "trailer", "upgrade", "host", "accept-encoding", "content-length"}
TEXT_TYPES = ("json", "ndjson", "text", "javascript", "xml", "html")


def secrets():
    return [v for v in (os.environ.get(k, "") for k in SECRET_ENV) if len(v) >= 6]


def scrub(s):
    for v in secrets():
        s = s.replace(v, "<redacted>")
    return s


def headers_dict(items):
    out = {}
    for k, v in items:
        lk = k.lower()
        if lk in DROP_HEADERS:
            out[lk] = "<redacted>"
            continue
        out[lk] = scrub(v) if lk not in out else out[lk] + ", " + scrub(v)
    return out


def body_record(data, content_type):
    if not data:
        return None
    ct = (content_type or "").lower()
    rec = {"size": len(data), "sha256": hashlib.sha256(data).hexdigest()}
    if any(t in ct for t in TEXT_TYPES):
        try:
            text = scrub(data.decode("utf-8"))
            try:
                rec["json"] = json.loads(text)
            except ValueError:
                rec["text"] = text
            return rec
        except UnicodeDecodeError:
            pass
    rec["base64"] = base64.b64encode(data).decode() if len(data) <= 256 * 1024 else None
    return rec


class Recorder:
    def __init__(self, out):
        self.out = out
        self.n = 0
        self.lock = threading.Lock()
        os.makedirs(out, exist_ok=True)

    def next_name(self, method, path):
        with self.lock:
            self.n += 1
            n = self.n
        slug = re.sub(r"[^A-Za-z0-9]+", "-", path.split("?")[0].replace("/api/", "")).strip("-")[:140]
        return os.path.join(self.out, f"{n:03d}-{method}-{slug}.json")

    def write(self, name, rec):
        with open(name, "w") as f:
            json.dump(rec, f, indent=2, sort_keys=False)
            f.write("\n")


class Handler(http.server.BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"
    upstream = None  # urllib.parse result
    recorder = None

    def log_message(self, fmt, *args):
        sys.stderr.write("[record] " + scrub(fmt % args) + "\n")

    def _proxy(self):
        method = self.command
        length = int(self.headers.get("content-length") or 0)
        if self.headers.get("transfer-encoding", "").lower() == "chunked":
            body = self._read_chunked()
        else:
            body = self.rfile.read(length) if length else b""
        name = self.recorder.next_name(method, self.path)
        parsed = urllib.parse.urlsplit(self.path)
        query = {k: [scrub(x) for x in v] for k, v in urllib.parse.parse_qs(parsed.query, keep_blank_values=True).items()}
        rec = {
            "request": {
                "method": method,
                "path": parsed.path,
                "query": query,
                "headers": headers_dict(self.headers.items()),
                "body": body_record(body, self.headers.get("content-type")),
            }
        }
        fwd = {k: v for k, v in self.headers.items() if k.lower() not in HOP_HEADERS}
        fwd["Host"] = self.upstream.netloc
        fwd["Accept-Encoding"] = "identity"
        if body or method in ("POST", "PUT", "PATCH"):
            fwd["Content-Length"] = str(len(body))
        if self.upstream.scheme == "https":
            conn = http.client.HTTPSConnection(self.upstream.netloc, timeout=600, context=ssl.create_default_context())
        else:
            conn = http.client.HTTPConnection(self.upstream.netloc, timeout=600)
        t0 = time.monotonic()
        try:
            conn.request(method, self.path, body=body or None, headers=fwd)
            resp = conn.getresponse()
        except Exception as e:  # upstream unreachable
            rec["error"] = scrub(str(e))
            self.recorder.write(name, rec)
            self.send_error(502, "upstream error")
            return
        ctype = resp.getheader("content-type", "")
        self.send_response(resp.status, resp.reason)
        for k, v in resp.getheaders():
            if k.lower() in HOP_HEADERS or k.lower() == "set-cookie":
                continue
            self.send_header(k, v)
        bodyless = method == "HEAD" or resp.status in (204, 304) or 100 <= resp.status < 200
        if bodyless:
            self.send_header("Content-Length", "0")
        else:
            self.send_header("Transfer-Encoding", "chunked")
        self.send_header("Connection", "close")
        self.end_headers()
        chunks, whole = [], bytearray()
        try:
            while not bodyless:
                data = resp.read1(65536)
                if not data:
                    break
                whole += data
                chunks.append((round((time.monotonic() - t0) * 1000), data))
                self.wfile.write(b"%x\r\n%s\r\n" % (len(data), data))
                self.wfile.flush()
            if not bodyless:
                self.wfile.write(b"0\r\n\r\n")
                self.wfile.flush()
        except (BrokenPipeError, ConnectionResetError) as e:
            rec["clientDisconnected"] = str(e)
        finally:
            conn.close()
            self.close_connection = True
        rec["response"] = {
            "status": resp.status,
            "headers": headers_dict(resp.getheaders()),
            "body": body_record(bytes(whole), ctype),
            "elapsedMs": round((time.monotonic() - t0) * 1000),
        }
        if "ndjson" in ctype.lower():
            # One entry per received chunk, so the streaming cadence is preserved.
            rec["response"]["chunks"] = [{"atMs": t, "data": scrub(d.decode("utf-8", "replace"))} for t, d in chunks]
        self.recorder.write(name, rec)

    def _read_chunked(self):
        out = bytearray()
        while True:
            size = int(self.rfile.readline().split(b";")[0].strip(), 16)
            if size == 0:
                while self.rfile.readline() not in (b"\r\n", b"\n", b""):
                    pass
                return bytes(out)
            out += self.rfile.read(size)
            self.rfile.readline()

    do_GET = do_POST = do_PUT = do_PATCH = do_DELETE = do_HEAD = _proxy


class Server(socketserver.ThreadingMixIn, http.server.HTTPServer):
    daemon_threads = True


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--listen", default="127.0.0.1:0")
    ap.add_argument("--out", required=True)
    ap.add_argument("--upstream", default="https://vercel.com")
    a = ap.parse_args()
    Handler.upstream = urllib.parse.urlsplit(a.upstream)
    Handler.recorder = Recorder(a.out)
    host, port = a.listen.rsplit(":", 1)
    srv = Server((host, int(port)), Handler)
    print(f"listening on http://{host}:{srv.server_address[1]}", flush=True)
    srv.serve_forever()


if __name__ == "__main__":
    main()
