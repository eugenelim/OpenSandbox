"""S-11b: session-bound ref-aware git proxy. Local only: no Docker, no VM."""
import base64, http.client, json, os, shutil, subprocess, sys, time

D = "/tmp/spikes/s11"; SECRET = "REAL-SECRET-NEVER-IN-SANDBOX"
BPORT, PPORT, CAP = 18111, 18112, 30_000_000
shutil.rmtree(f"{D}/run2", ignore_errors=True); os.makedirs(f"{D}/run2/projects")
open(f"{D}/run2/gitconfig", "w").write("[user]\n name=t\n email=t@t\n[init]\n defaultBranch=main\n")
ENV = dict(os.environ, GIT_CONFIG_GLOBAL=f"{D}/run2/gitconfig", GIT_CONFIG_NOSYSTEM="1", GIT_TERMINAL_PROMPT="0")


def sh(*a, cwd=None):
    r = subprocess.run(a, cwd=cwd, env=ENV, capture_output=True, text=True); return r.returncode, (r.stdout + r.stderr).strip()


def bare(name):
    p = f"{D}/run2/projects/{name}"; sh("git", "init", "-q", "--bare", p); sh("git", "--git-dir", p, "config", "http.receivepack", "true")
    seed = f"{D}/run2/seed-{name}"; sh("git", "init", "-q", seed); open(f"{seed}/a", "w").write(name); sh("git", "add", ".", cwd=seed); sh("git", "commit", "-qm", "base", cwd=seed)
    sh("git", "push", "-q", p, "main:refs/heads/main", cwd=seed); return p


A, B = bare("repoA.git"), bare("repoB.git")
SESS = f"{D}/run2/sessions.json"
base = {"T1": {"repo": "repoA.git", "allow": r"refs/heads/agent/s1(?:/[A-Za-z0-9._-]+)*"},
        "T2": {"repo": "repoA.git", "allow": r"refs/heads/agent/s2(?:/[A-Za-z0-9._-]+)*"},
        "T3": {"repo": "repoB.git", "allow": r"refs/heads/agent/s3(?:/[A-Za-z0-9._-]+)*"}}
json.dump(base, open(SESS, "w"))
be = subprocess.Popen([sys.executable, f"{D}/backend.py", f"{D}/run2/projects", str(BPORT), SECRET, f"{D}/run2/backend.log"])
px = subprocess.Popen([sys.executable, f"{D}/proxy2.py", str(PPORT), str(BPORT), SECRET, SESS, f"{D}/run2/proxy.log", str(CAP)])
time.sleep(1.5); results = []


def check(name, ok, detail=""):
    results.append((name, ok, detail)); print(("PASS " if ok else "FAIL ") + name + (f"  [{detail}]" if detail else ""), flush=True)


def refs(p): return dict(l.split(" ", 1)[::-1] for l in sh("git", "--git-dir", p, "for-each-ref", "--format=%(objectname) %(refname)")[1].splitlines())
def url(tok, repo): return f"http://s:{tok}@127.0.0.1:{PPORT}/{repo}"
def posts():  # receive-pack requests that carry more than a probe or a ref update (a pack): body over 1,000 bytes
    out = []
    for l in open(f"{D}/run2/backend.log").read().splitlines():
        if l.startswith("POST") and "receive-pack" in l and int(l.rsplit("body=", 1)[1]) > 1000:
            out.append(l)
    return out


def clone(tok, repo, name):
    d = f"{D}/run2/{name}"; rc, out = sh("git", "clone", "-q", url(tok, repo), d); return rc, out, d


def push_new(d, tok, repo, ref, size=0, branch="work"):
    sh("git", "checkout", "-q", "-B", branch, "origin/main", cwd=d); open(f"{d}/{branch}.txt", "w").write(ref)
    if size: open(f"{d}/{branch}.bin", "wb").write(os.urandom(size))
    sh("git", "add", ".", cwd=d); sh("git", "commit", "-qm", ref, cwd=d)
    return sh("git", "push", url(tok, repo), f"HEAD:{ref}", cwd=d)


try:
    rc, out = sh("git", "clone", "-q", f"http://127.0.0.1:{PPORT}/repoA.git", f"{D}/run2/anon"); check("no credentials: clone refused", rc != 0, out[-60:])
    rc, out = sh("git", "clone", "-q", url("WRONG", "repoA.git"), f"{D}/run2/wrong"); check("wrong token: clone refused", rc != 0, out[-60:])
    rc, out, w1 = clone("T1", "repoA.git", "w1"); check("session 1 clones its repository", rc == 0, out[-60:])
    rc, out = push_new(w1, "T1", "repoA.git", "refs/heads/agent/s1/x"); check("session 1 pushes to its own prefix", rc == 0 and "refs/heads/agent/s1/x" in refs(A), out[-60:])
    rc, out = push_new(w1, "T1", "repoA.git", "refs/heads/agent/s2/x", branch="w2"); check("session 1 cannot push to session 2's prefix", rc != 0 and "refs/heads/agent/s2/x" not in refs(A), out[-70:])
    rc, out = push_new(w1, "T1", "repoA.git", "refs/heads/main", branch="w3"); check("session 1 cannot push to main", rc != 0 and refs(A)["refs/heads/main"] == sh("git", "--git-dir", A, "rev-parse", "main")[1])
    rc, out, w2 = clone("T2", "repoA.git", "w2c"); rc2, out2 = push_new(w2, "T2", "repoA.git", "refs/heads/agent/s2/x"); check("session 2 pushes to its own prefix", rc2 == 0 and "refs/heads/agent/s2/x" in refs(A))
    rc, out = push_new(w2, "T2", "repoA.git", "refs/heads/agent/s1/y", branch="w9"); check("session 2 cannot push to session 1's prefix", rc != 0 and "refs/heads/agent/s1/y" not in refs(A))
    rc, out, _ = clone("T1", "repoB.git", "w1b"); check("session 1 cannot read another repository (repoB)", rc != 0, out[-70:])
    rc, out, w3 = clone("T3", "repoB.git", "w3"); rc3, _ = push_new(w3, "T3", "repoB.git", "refs/heads/agent/s3/z"); check("session 3 works on repoB only", rc == 0 and rc3 == 0 and "refs/heads/agent/s3/z" in refs(B))
    c = http.client.HTTPConnection("127.0.0.1", PPORT); auth = base64.b64encode(b"s:T1").decode()
    c.request("GET", "/repoA.git/../repoB.git/info/refs?service=git-upload-pack", headers={"Authorization": "Basic " + auth}); st = c.getresponse().status
    check("path traversal into another repository is refused", st == 403, f"status {st}")
    # size cap
    before = len(posts()); rc, out = push_new(w2, "T2", "repoA.git", "refs/heads/agent/s2/big", size=40_000_000, branch="bigw")
    check("40 MB push over the 30 MB session cap is refused and no pack reaches the origin", rc != 0 and "refs/heads/agent/s2/big" not in refs(A) and len(posts()) == before, f"POSTs before={before} after={len(posts())}")
    rc, out = push_new(w2, "T2", "repoA.git", "refs/heads/agent/s2/mid", size=20_000_000, branch="midw"); check("20 MB push under the cap succeeds", rc == 0 and "refs/heads/agent/s2/mid" in refs(A), out[-50:])
    # revocation
    new = dict(base); del new["T1"]; time.sleep(0.05); json.dump(new, open(SESS, "w")); os.utime(SESS, None)
    rc, out = push_new(w1, "T1", "repoA.git", "refs/heads/agent/s1/after", branch="rv"); check("revoked token stops working at once", rc != 0 and "refs/heads/agent/s1/after" not in refs(A), out[-60:])
    rc, out = sh("git", "ls-remote", url("T2", "repoA.git")); check("other sessions unaffected by the revocation", rc == 0 and "refs/heads/main" in out)
finally:
    be.terminate(); px.terminate()
    json.dump([{"name": n, "ok": o, "detail": d} for n, o, d in results], open(f"{D}/run2/results.json", "w"), indent=1)
    print(f"\n{sum(o for _, o, _ in results)}/{len(results)} passed")
