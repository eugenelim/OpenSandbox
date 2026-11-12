"""Throwaway git smart-HTTP origin: runs `git http-backend` as CGI and requires the real credential.
Usage: backend.py <projects-root> <port> <secret> <logfile>"""
import base64, os, subprocess, sys
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

ROOT, PORT, SECRET, LOG = sys.argv[1], int(sys.argv[2]), sys.argv[3], open(sys.argv[4], "a")
EXPECT = "Basic " + base64.b64encode(f"x-access-token:{SECRET}".encode()).decode()


class H(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.0"

    def log_message(self, *a):  # quiet
        pass

    def handle_one(self):
        if self.headers.get("Authorization") != EXPECT:
            LOG.write(f"{self.command} {self.path} 401\n"); LOG.flush()
            self.send_response(401); self.send_header("WWW-Authenticate", 'Basic realm="git"')
            self.send_header("Content-Length", "0"); self.end_headers(); return
        path, _, qs = self.path.partition("?")
        n = int(self.headers.get("Content-Length") or 0)
        body = self.rfile.read(n) if n else b""
        LOG.write(f"{self.command} {path}?{qs} body={len(body)}\n"); LOG.flush()
        env = dict(os.environ, GIT_PROJECT_ROOT=ROOT, GIT_HTTP_EXPORT_ALL="1", REQUEST_METHOD=self.command,
                   PATH_INFO=path, QUERY_STRING=qs, CONTENT_TYPE=self.headers.get("Content-Type", ""),
                   CONTENT_LENGTH=str(len(body)), REMOTE_USER="agent", REMOTE_ADDR="127.0.0.1",
                   GIT_PROTOCOL=self.headers.get("Git-Protocol", ""))
        if self.headers.get("Content-Encoding"):
            env["HTTP_CONTENT_ENCODING"] = self.headers["Content-Encoding"]
        p = subprocess.run(["git", "http-backend"], input=body, env=env, capture_output=True)
        head, _, out = p.stdout.partition(b"\r\n\r\n")
        status, hdrs = 200, []
        for line in head.split(b"\r\n"):
            k, _, v = line.partition(b": ")
            if k.lower() == b"status":
                status = int(v.split()[0])
            elif k:
                hdrs.append((k.decode(), v.decode()))
        self.send_response(status)
        for k, v in hdrs:
            self.send_header(k, v)
        self.send_header("Content-Length", str(len(out))); self.end_headers(); self.wfile.write(out)

    do_GET = do_POST = handle_one


ThreadingHTTPServer(("127.0.0.1", PORT), H).serve_forever()
