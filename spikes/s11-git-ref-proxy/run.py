"""S-11: does a ref-aware git proxy hold up against real git and crafted requests? Local only: no Docker, no VM."""
import gzip, http.client, json, os, shutil, subprocess, sys, time

D = "/tmp/spikes/s11"; SECRET = "REAL-SECRET-NEVER-IN-SANDBOX"
ALLOW = r"refs/heads/agent/s1(?:/[A-Za-z0-9._-]+)*"
BPORT, PPORT = 18101, 18102
shutil.rmtree(f"{D}/run", ignore_errors=True); os.makedirs(f"{D}/run/projects")
open(f"{D}/run/gitconfig", "w").write("[user]\n name=t\n email=t@t\n[init]\n defaultBranch=main\n")
ENV = dict(os.environ, GIT_CONFIG_GLOBAL=f"{D}/run/gitconfig", GIT_CONFIG_NOSYSTEM="1", GIT_TERMINAL_PROMPT="0")
BARE = f"{D}/run/projects/repo.git"


def sh(*a, cwd=None, check=False):
    r = subprocess.run(a, cwd=cwd, env=ENV, capture_output=True, text=True)
    if check and r.returncode:
        raise SystemExit(f"{a} failed: {r.stderr}")
    return r.returncode, (r.stdout + r.stderr).strip()


def refs():
    return dict(l.split(" ", 1)[::-1] for l in sh("git", "--git-dir", BARE, "for-each-ref", "--format=%(objectname) %(refname)")[1].splitlines())


# --- origin with a main branch, receive-pack enabled
sh("git", "init", "-q", "--bare", BARE, check=True); sh("git", "--git-dir", BARE, "config", "http.receivepack", "true", check=True)
seed = f"{D}/run/seed"; sh("git", "init", "-q", seed, check=True)
open(f"{seed}/a.txt", "w").write("hello\n"); sh("git", "add", ".", cwd=seed); sh("git", "commit", "-qm", "base", cwd=seed)
sh("git", "push", "-q", BARE, "main:refs/heads/main", cwd=seed, check=True)
MAIN = refs()["refs/heads/main"]

be = subprocess.Popen([sys.executable, f"{D}/backend.py", f"{D}/run/projects", str(BPORT), SECRET, f"{D}/run/backend.log"])
px = subprocess.Popen([sys.executable, f"{D}/proxy.py", str(PPORT), str(BPORT), SECRET, ALLOW, f"{D}/run/proxy.log"])
time.sleep(1.5)
results = []


def check(name, ok, detail=""):
    results.append((name, ok, detail)); print(("PASS " if ok else "FAIL ") + name + (f"  [{detail}]" if detail else ""), flush=True)


def backend_posts():
    return [l for l in open(f"{D}/run/backend.log").read().splitlines() if "git-receive-pack?" in l and l.startswith("POST")]


try:
    URL = f"http://sandbox:placeholder@127.0.0.1:{PPORT}/repo.git"
    # direct access to the origin needs the real credential
    c = http.client.HTTPConnection("127.0.0.1", BPORT); c.request("GET", "/repo.git/info/refs?service=git-upload-pack"); check("origin refuses requests without the real credential", c.getresponse().status == 401)
    # clone through the proxy with only a placeholder
    wc = f"{D}/run/wc"; rc, out = sh("git", "clone", "-q", URL, wc); check("clone through proxy with a placeholder credential", rc == 0, out[:80])
    sh("git", "checkout", "-q", "-b", "agent/s1/work", cwd=wc); open(f"{wc}/f.txt", "w").write("x\n"); sh("git", "add", ".", cwd=wc); sh("git", "commit", "-qm", "work", cwd=wc)
    rc, out = sh("git", "push", "origin", "HEAD:refs/heads/agent/s1/work", cwd=wc); check("push to the allowed branch succeeds", rc == 0 and "refs/heads/agent/s1/work" in refs(), out[-80:])
    rc, out = sh("git", "push", "origin", "HEAD:refs/heads/main", cwd=wc); check("push to main is denied", rc != 0 and refs()["refs/heads/main"] == MAIN, out[-100:])
    rc, out = sh("git", "push", "origin", "--delete", "agent/s1/work", cwd=wc); check("deleting the agent branch is denied", rc != 0 and "refs/heads/agent/s1/work" in refs())
    sh("git", "tag", "v1", cwd=wc); rc, out = sh("git", "push", "origin", "v1", cwd=wc); check("tag push is denied", rc != 0 and "refs/tags/v1" not in refs())
    rc, out = sh("git", "push", "origin", "HEAD:refs/heads/agent/other", cwd=wc); check("push to another agent prefix is denied", rc != 0 and "refs/heads/agent/other" not in refs())
    rc, out = sh("git", "push", "--atomic", "origin", "HEAD:refs/heads/agent/s1/a", "HEAD:refs/heads/main", cwd=wc); check("multi-ref push with one bad ref is denied as a whole", rc != 0 and "refs/heads/agent/s1/a" not in refs())
    open(f"{wc}/f.txt", "w").write("rewritten\n"); sh("git", "commit", "-q", "--amend", "-am", "rewrite", cwd=wc)
    rc, out = sh("git", "push", "-f", "origin", "HEAD:refs/heads/agent/s1/work", cwd=wc); check("force-push to the allowed branch succeeds (policy choice)", rc == 0, out[-60:])
    # large push (chunked transfer) allowed
    sh("git", "checkout", "-q", "-b", "agent/s1/big", cwd=wc); open(f"{wc}/big.bin", "wb").write(os.urandom(20_000_000)); sh("git", "add", ".", cwd=wc); sh("git", "commit", "-qm", "big", cwd=wc)
    t = time.time(); rc, out = sh("git", "push", "origin", "HEAD:refs/heads/agent/s1/big", cwd=wc); big_secs = round(time.time() - t, 1)
    head = sh("git", "rev-parse", "HEAD", cwd=wc)[1]; check("20 MB push to the allowed branch succeeds and arrives intact", rc == 0 and refs().get("refs/heads/agent/s1/big") == head, f"{big_secs}s")
    # large push to main is denied before the pack is forwarded
    before = len(backend_posts()); rc, out = sh("git", "push", "origin", "HEAD:refs/heads/main", cwd=wc)
    check("20 MB push to main is denied and no pack reaches the origin", rc != 0 and len(backend_posts()) == before and refs()["refs/heads/main"] == MAIN, f"origin receive-pack POSTs before={before} after={len(backend_posts())}")
    # crafted requests straight at the proxy
    def pkt(b): return f"{len(b)+4:04x}".encode() + b
    zero = "0" * 40; new = MAIN
    def post(body, headers=None, chunks=None):
        c = http.client.HTTPConnection("127.0.0.1", PPORT, timeout=10)
        h = {"Content-Type": "application/x-git-receive-pack-request"}; h.update(headers or {})
        if chunks:
            h["Transfer-Encoding"] = "chunked"; c.request("POST", "/repo.git/git-receive-pack", body=iter(chunks), headers=h, encode_chunked=True)
        else:
            c.request("POST", "/repo.git/git-receive-pack", body=body, headers=h)
        try:
            return c.getresponse().status
        except Exception as e:
            return f"err:{type(e).__name__}"
    cmd = lambda ref, first=True: pkt((f"{zero} {new} {ref}" + ("\0report-status" if first else "")).encode())
    check("crafted: ref with .. is denied", post(cmd("refs/heads/agent/s1/../../main") + b"0000") == 403)
    check("crafted: NUL smuggled into a later command is denied", post(cmd("refs/heads/agent/s1/ok") + pkt(f"{zero} {new} refs/heads/main\0x".encode()) + b"0000") == 403)
    check("crafted: signed push (push-cert) is denied", post(pkt(b"push-cert\0report-status") + b"0000") == 403)
    check("crafted: truncated pkt-line is denied", post(cmd("refs/heads/agent/s1/ok")[:-3]) == 403)
    check("crafted: bad length prefix is denied", post(b"zzzz" + b"0000") == 403)
    check("crafted: uppercase object ids are denied (strict grammar)", post(pkt(f"{zero.upper()} {new.upper()} refs/heads/agent/s1/ok\0x".encode()) + b"0000") == 403)
    check("crafted: command to main in gzip body is denied", post(gzip.compress(cmd("refs/heads/main") + b"0000"), {"Content-Encoding": "gzip"}) == 403)
    body = cmd("refs/heads/main") + b"0000"; check("crafted: command to main split into 1-byte chunks is denied", post(None, chunks=[body[i:i+1] for i in range(len(body))]) == 403)
    check("crafted: allowed-looking prefix with extra component is denied (agent/s1x)", post(cmd("refs/heads/agent/s1x/y") + b"0000") == 403)
    check("crafted: ref ending in .lock is denied", post(cmd("refs/heads/agent/s1/x.lock") + b"0000") == 403)
    c = http.client.HTTPConnection("127.0.0.1", PPORT); c.request("GET", "/repo.git/HEAD"); check("dumb-protocol path is blocked", c.getresponse().status == 403)
    rc, out = sh("git", "ls-remote", URL); check("ls-remote through the proxy works", rc == 0 and "refs/heads/main" in out)
finally:
    be.terminate(); px.terminate()
    json.dump([{"name": n, "ok": o, "detail": d} for n, o, d in results], open(f"{D}/run/results.json", "w"), indent=1)
    print(f"\n{sum(o for _, o, _ in results)}/{len(results)} passed")
