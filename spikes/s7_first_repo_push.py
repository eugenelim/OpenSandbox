import asyncio, json, os, subprocess, time
from datetime import timedelta
from opensandbox import Sandbox
from opensandbox.config import ConnectionConfig
from opensandbox.models.execd import RunCommandOpts
cfg = ConnectionConfig(api_key="spike-key", domain="127.0.0.1:18081", protocol="http", use_server_proxy=False)
SRC = "/tmp/spikes/ar-clone"; TAG = "agentbundle-v0.60.2"
res = {}
def sh(*a, env=None):
    r = subprocess.run(a, capture_output=True, text=True, env=env); return r.returncode, (r.stdout + r.stderr).strip()[-250:]
async def agent(sb, c):
    ex = await sb.commands.run(c, opts=RunCommandOpts(uid=1000, gid=1000, envs={"HOME": "/home/agent"}))
    return "\n".join(m.text for m in ex.logs.stdout).strip()[:300], "\n".join(m.text for m in ex.logs.stderr).strip()[:200]
async def main():
    env = {"EXECD_GIT_REPO_ROOT": "/workspace", "EXECD_GIT_RUN_AS": "1000:1000", "EXECD_GIT_MAX_BUNDLE_BYTES": str(512<<20)}
    sb = await Sandbox.create("ade-spike-base:0", entrypoint=["tail","-f","/dev/null"], env=env, timeout=timedelta(minutes=15), connection_config=cfg)
    try:
        ep = await sb.get_endpoint(44772)
        open("/tmp/spikes/gc","w").write("[user]\n name=dev\n email=d@x\n")
        e = dict(os.environ, OSB_BASE=f"http://{ep.endpoint}", PATH="/tmp/spikes/h:" + os.environ["PATH"], OSB_EXECD_TOKEN="", GIT_CONFIG_GLOBAL="/tmp/spikes/gc", OSB_MAX_BUNDLE_BYTES=str(512<<20))
        # read-only use of the source repo: temporary remote via -c, no config change
        t = time.time()
        res["push"] = sh("git","-C",SRC,"-c","remote.sb.url=osb://x/workspace/repo","push","sb","release:refs/heads/main", env=e)
        res["push_secs"] = round(time.time()-t, 1)
        res["src_config_untouched"] = sh("git","-C",SRC,"remote")[1].split()
        res["in_sandbox"] = await agent(sb, "cd /workspace/repo && git checkout -q -f main && git log -1 --format='%h %s' && ls | wc -l && du -sh .git | cut -f1 && cat packages/agentbundle/pyproject.toml | grep -m1 '^version'")
        res["tools_present"] = await agent(sb, "for t in python3 uv node npm make ruff mypy; do command -v $t >/dev/null && echo has:$t || echo missing:$t; done | tr '\\n' ' '")
    finally:
        await sb.kill(); await sb.close(); print(json.dumps(res, indent=1, default=str))
asyncio.run(main())
