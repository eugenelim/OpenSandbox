import asyncio, json, os, subprocess, tempfile, time
from datetime import timedelta
from opensandbox import Sandbox
from opensandbox.config import ConnectionConfig
from opensandbox.models.execd import RunCommandOpts
cfg = ConnectionConfig(api_key="spike-key", domain="127.0.0.1:18081", protocol="http", use_server_proxy=False)
res = {}
def sh(*a, env=None, cwd=None):
    r = subprocess.run(a, capture_output=True, text=True, env=env, cwd=cwd); return r.returncode, (r.stdout + r.stderr).strip()[-300:]
async def agent(sb, c):
    ex = await sb.commands.run(c, opts=RunCommandOpts(uid=1000, gid=1000, envs={"HOME": "/home/agent"}))
    return "\n".join(m.text for m in ex.logs.stdout).strip()[:200], ("\n".join(m.text for m in ex.logs.stderr).strip()[:200])
async def main():
    env = {"EXECD_GIT_REPO_ROOT": "/workspace", "EXECD_GIT_RUN_AS": "1000:1000", "EXECD_GIT_MAX_BUNDLE_BYTES": str(512<<20)}
    sb = await Sandbox.create("ade-spike-base:0", entrypoint=["tail","-f","/dev/null"], env=env, timeout=timedelta(minutes=10), connection_config=cfg)
    try:
        ep = await sb.get_endpoint(44772)
        e = dict(os.environ, OSB_BASE=f"http://{ep.endpoint}", PATH="/tmp/spikes/h:" + os.environ["PATH"], OSB_EXECD_TOKEN="", GIT_CONFIG_GLOBAL="/tmp/spikes/gc", OSB_MAX_BUNDLE_BYTES=str(512<<20))
        open("/tmp/spikes/gc","w").write("[user]\n name=dev\n email=d@x\n[init]\n defaultBranch=main\n")
        d = tempfile.mkdtemp(prefix="dev6-"); g = lambda *a: sh("git","-C",d,*a,env=e)
        g("init","-q"); open(f"{d}/a.txt","w").write("hello\n"); open(f"{d}/blob.bin","wb").write(os.urandom(60_000_000)); g("add","."); g("commit","-qm","base")
        g("remote","add","sb","osb://x/workspace/repo")
        t=time.time(); res["push"] = g("push","sb","main:refs/heads/main"); res["push_secs"]=round(time.time()-t,1)
        res["sandbox_owner"] = await agent(sb, "stat -c %u /workspace/repo/.git; cd /workspace/repo && git checkout -q -f main 2>&1; ls")
        res["agent_commit"] = await agent(sb, "cd /workspace/repo && git checkout -q -b agent/s6 && head -c 15000000 /dev/urandom > big.bin && echo agent >> a.txt && git add -A && git -c user.email=a@x -c user.name=agent commit -qm 'agent change' && git rev-parse --short HEAD")
        t=time.time(); res["fetch"] = g("fetch","sb"); res["fetch_secs"]=round(time.time()-t,1)
        res["dev_has_agent_branch"] = g("log","-1","--format=%s","refs/remotes/sb/agent/s6")
        # symlink escape via the git endpoint: agent plants a symlinked .git entry
        res["plant_symlink_gitdir"] = await agent(sb, "mkdir -p /workspace/evil && ln -s /root /workspace/evil/.git && echo planted")
        e2 = dict(e); 
        r = subprocess.run(["curl","-s","-m","10","-w","\\n%{http_code}",f"http://{ep.endpoint}/v1/git/refs?path=/workspace/evil"],capture_output=True,text=True); res["refs_on_symlinked_gitdir"] = r.stdout.strip()[-200:]
        r = subprocess.run(["curl","-s","-m","10","-w","\\n%{http_code}",f"http://{ep.endpoint}/v1/git/refs?path=/etc"],capture_output=True,text=True); res["refs_outside_root"] = r.stdout.strip()[-200:]
    finally:
        await sb.kill(); await sb.close(); print(json.dumps(res, indent=1, default=str))
asyncio.run(main())
