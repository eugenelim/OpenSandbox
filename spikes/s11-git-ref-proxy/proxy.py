"""Ref-aware git smart-HTTP proxy (spike S-11). Holds the real credential; the sandbox holds none.
Parses only the command section of a receive-pack request and rejects any ref outside the allow pattern,
before the pack is forwarded. Fails closed on anything it cannot parse strictly.
Usage: proxy.py <port> <backend-port> <secret> <allowed-ref-regex> <logfile>"""
import base64, http.client, json, re, sys, zlib
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

PORT, BPORT, SECRET = int(sys.argv[1]), int(sys.argv[2]), sys.argv[3]
ALLOW = re.compile(sys.argv[4])
LOG = open(sys.argv[5], "a")
REAL = "Basic " + base64.b64encode(f"x-access-token:{SECRET}".encode()).decode()
REPO = "/repo.git"
OID = rb"(?:[0-9a-f]{40}|[0-9a-f]{64})"


class Deny(Exception):
    pass


class Body:
    """Request body reader: handles Content-Length and chunked, optional gzip, keeps the raw bytes to forward."""

    def __init__(s, h):
        s.f = h.rfile
        s.chunked = "chunked" in h.headers.get("Transfer-Encoding", "").lower()
        s.left = int(h.headers.get("Content-Length") or 0)
        s.gz = h.headers.get("Content-Encoding", "").lower() == "gzip"
        s.d = zlib.decompressobj(31) if s.gz else None
        s.raw, s.plain, s.eof = bytearray(), bytearray(), False

    def _more(s):
        if s.eof:
            return False
        if s.chunked:
            line = s.f.readline()
            try:
                size = int(line.split(b";")[0].strip() or b"0", 16)
            except ValueError:
                raise Deny("bad chunk header")
            if size == 0:
                s.f.readline(); s.eof = True; return False
            data = s.f.read(size); s.f.read(2)
        else:
            if s.left <= 0:
                s.eof = True; return False
            data = s.f.read(min(65536, s.left)); s.left -= len(data)
            if not data:
                s.eof = True; return False
        s.raw += data
        try:
            s.plain += s.d.decompress(data) if s.d else data
        except zlib.error:
            raise Deny("bad gzip")
        return True

    def take(s, n):
        while len(s.plain) < n:
            if not s._more():
                raise Deny("truncated command section")
        out = bytes(s.plain[:n]); del s.plain[:n]; return out

    def drain(s):
        while s._more():
            pass


def parse_commands(body):
    cmds, first = [], True
    while True:
        hdr = body.take(4)
        if not re.fullmatch(rb"[0-9a-f]{4}", hdr):
            raise Deny("bad pkt-line length")
        n = int(hdr, 16)
        if n == 0:
            break
        if n < 4 or n > 65520:
            raise Deny("bad pkt-line length")
        payload = body.take(n - 4)
        if payload.startswith(b"shallow "):
            continue
        if payload.startswith(b"push-cert"):
            raise Deny("signed push not allowed")
        if first:
            payload, _, _caps = payload.partition(b"\0"); first = False
        elif b"\0" in payload:
            raise Deny("NUL in later command")
        m = re.fullmatch(rb"(" + OID + rb") (" + OID + rb") (.+)", payload, re.S)
        if not m:
            raise Deny("bad command")
        try:
            ref = m.group(3).decode("utf-8")
        except UnicodeDecodeError:
            raise Deny("ref not utf-8")
        cmds.append((m.group(1).decode(), m.group(2).decode(), ref))
    return cmds


def check_ref(old, new, ref):
    if not ALLOW.fullmatch(ref):
        raise Deny(f"ref outside allowed pattern: {ref!r}")
    if ".." in ref or "//" in ref or "@{" in ref or ref.endswith("/") or ref.endswith(".lock") or any(ord(c) < 32 or c in " ~^:?*[\\" for c in ref):
        raise Deny(f"unsafe ref name: {ref!r}")
    if set(new) == {"0"}:
        raise Deny(f"delete not allowed: {ref!r}")


class H(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def log_message(self, *a):
        pass

    def deny(self, why, **kw):
        LOG.write(json.dumps({"decision": "deny", "path": self.path, "why": why, **kw}) + "\n"); LOG.flush()
        msg = f"denied by git proxy policy: {why}\n".encode()
        self.send_response(403); self.send_header("Content-Type", "text/plain"); self.send_header("Content-Length", str(len(msg)))
        self.send_header("Connection", "close"); self.end_headers(); self.wfile.write(msg); self.close_connection = True

    def forward(self, body_bytes, extra=None):
        c = http.client.HTTPConnection("127.0.0.1", BPORT, timeout=60)
        hdrs = {"Authorization": REAL, "Content-Length": str(len(body_bytes))}
        for k in ("Content-Type", "Git-Protocol", "Accept"):
            if self.headers.get(k):
                hdrs[k] = self.headers[k]
        if extra:
            hdrs.update(extra)
        c.request(self.command, self.path, body=body_bytes if self.command == "POST" else None, headers=hdrs)
        r = c.getresponse(); data = r.read()
        self.send_response(r.status)
        for k, v in r.getheaders():
            if k.lower() in ("content-type", "cache-control", "expires", "pragma"):
                self.send_header(k, v)
        self.send_header("Content-Length", str(len(data))); self.end_headers(); self.wfile.write(data)

    def route(self):
        path, _, qs = self.path.partition("?")
        if path == f"{REPO}/info/refs" and self.command == "GET" and re.fullmatch(r"service=git-(upload|receive)-pack", qs):
            return self.forward(b"")
        if path == f"{REPO}/git-upload-pack" and self.command == "POST":
            body = Body(self)
            try:
                body.drain()
            except Deny as e:
                return self.deny(str(e))
            return self.forward(bytes(body.raw), {"Content-Encoding": "gzip"} if body.gz else None)
        if path == f"{REPO}/git-receive-pack" and self.command == "POST":
            body = Body(self)
            try:
                cmds = parse_commands(body)
                for old, new, ref in cmds:
                    check_ref(old, new, ref)
                if not cmds:
                    # git sends a 4-byte "0000" probe before a large chunked push to check authentication; nothing else may be empty
                    body.drain()
                    if bytes(body.raw) != b"0000":
                        raise Deny("no commands")
            except Deny as e:
                return self.deny(str(e))
            body.drain()
            LOG.write(json.dumps({"decision": "allow", "refs": [c[2] for c in cmds], "bytes": len(body.raw)}) + "\n"); LOG.flush()
            return self.forward(bytes(body.raw), {"Content-Encoding": "gzip"} if body.gz else None)
        return self.deny("endpoint not allowed (dumb protocol and other paths are blocked)")

    do_GET = do_POST = route


ThreadingHTTPServer(("127.0.0.1", PORT), H).serve_forever()
