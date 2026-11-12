import asyncio, base64, hashlib, os, subprocess, tempfile, sys, json
from datetime import timedelta
from opensandbox import Sandbox
from opensandbox.config import ConnectionConfig
from opensandbox.models.execd import RunCommandOpts
from opensandbox.models.filesystem import WriteEntry

cfg = ConnectionConfig(api_key="spike-key", domain="127.0.0.1:18080", protocol="http", use_server_proxy=True)
sh = lambda *a, **k: subprocess.run(a, check=True, capture_output=True, text=True, **k).stdout.strip()
res = {}

async def out(sb, cmd, uid=None, wd=None):
    ex = await sb.commands.run(cmd, opts=RunCommandOpts(uid=uid, gid=uid, working_directory=wd, envs={"HOME":"/home/agent"} if uid else None) if uid is not None or wd else None)
    so = "\n".join(m.text for m in ex.logs.stdout); se = "\n".join(m.text for m in ex.logs.stderr)
    return so, se, ex.error

import time
async def main():
    # developer repo with a binary file, 2 commits
    d = tempfile.mkdtemp(prefix="dev-"); g = lambda *a: sh("git", "-C", d, *a)
    g("init", "-q", "-b", "main"); g("config", "user.email", "d@x"); g("config", "user.name", "dev")
    open(f"{d}/a.txt", "w").write("hello\n"); open(f"{d}/blob.bin", "wb").write(os.urandom(int(sys.argv[1]) if len(sys.argv) > 1 else 5_000_000))
    g("add", "."); g("commit", "-qm", "base"); base = g("rev-parse", "HEAD")
    open(f"{d}/a.txt", "a").write("more\n"); g("commit", "-qam", "second")
    g("bundle", "create", f"{d}.bundle", "main"); bun = open(f"{d}.bundle", "rb").read()
    h_in = hashlib.sha256(bun).hexdigest(); res["bundle_bytes"] = len(bun)

    sb = await Sandbox.create("ade-spike-base:0", entrypoint=["tail", "-f", "/dev/null"], timeout=timedelta(minutes=10), connection_config=cfg, skip_health_check=False)
    try:
        so, _, _ = await out(sb, "id -u; whoami; ls -ld /workspace /tmp"); res["exec_identity"] = so.split("\n")[:2]
        # staging dir root-owned, agent cannot write to it
        await out(sb, "mkdir -p /var/ade-staging && chmod 755 /var/ade-staging")
        t1=time.time(); await sb.files.write_files([WriteEntry(path="/var/ade-staging/in.bundle", data=bun, mode=644)]); res["inbound_secs"]=round(time.time()-t1,1)
        so, _, _ = await out(sb, "sha256sum /var/ade-staging/in.bundle"); res["inbound_hash_ok"] = so.split()[0] == h_in
        # agent (uid 1000) clones, commits
        so, se, err = await out(sb, "git clone -q -b main /var/ade-staging/in.bundle /workspace/repo && cd /workspace/repo && git checkout -q -b agent/s1 && echo agent >> a.txt && head -c 15000000 /dev/urandom > big.bin && git add big.bin && git -c user.email=a@x -c user.name=agent commit -qam 'agent change' && git rev-parse HEAD", uid=1000)
        res["agent_clone_commit"] = (so.strip()[:12], se.strip()[:200], str(err)[:100])
        t0=time.time()
        # outbound over command stdout, base64 (binary safe?)
        so, se, err = await out(sb, f"cd /workspace/repo && git bundle create - agent/s1 ^{base} 2>/dev/null | base64 -w0", uid=1000)
        raw = base64.b64decode(so.strip()); res["outbound_cmd_bytes"] = len(raw); res["outbound_secs"] = round(time.time()-t0,1)
        open(f"{d}.out.bundle", "wb").write(raw)
        # developer verifies and fetches the thin bundle
        v = subprocess.run(["git", "-C", d, "bundle", "verify", f"{d}.out.bundle"], capture_output=True, text=True); res["bundle_verify"] = (v.returncode, v.stderr.strip()[-80:])
        f = subprocess.run(["git", "-C", d, "fetch", f"{d}.out.bundle", "agent/s1:refs/heads/agent/s1"], capture_output=True, text=True); res["dev_fetch"] = (f.returncode, f.stderr.strip()[-200:])
        res["dev_has_agent_commit"] = subprocess.run(["git","-C",d,"log","-1","--format=%s","agent/s1"],capture_output=True,text=True).stdout.strip()
        # symlink escape probe: agent plants symlink to a root-only file, Gateway reads via files API
        await out(sb, "echo TOPSECRET > /root/secret.txt || true; chmod 600 /root/secret.txt || true")
        await out(sb, "ln -s /root/secret.txt /workspace/repo/link.txt", uid=1000)
        try:
            data = await sb.files.read_bytes("/workspace/repo/link.txt"); res["symlink_read_via_files_api"] = data[:20]
        except Exception as e:
            res["symlink_read_via_files_api"] = f"blocked: {type(e).__name__}: {str(e)[:100]}"
        # can the agent replace the staging file or plant a symlink there?
        so, se, err = await out(sb, "ln -sf /root/secret.txt /var/ade-staging/out.bundle; echo rc=$?", uid=1000); res["agent_write_staging"] = (so.strip(), se.strip()[:100])
        # git as root over an agent-owned repo: safe.directory protects?
        so, se, err = await out(sb, "cd /workspace/repo && git log -1 --format=%s 2>&1 | head -2"); res["root_git_in_agent_repo"] = (so.strip()[:140])
    finally:
        await sb.kill(); await sb.close()
        print(json.dumps(res, indent=1, default=str))
asyncio.run(main())
