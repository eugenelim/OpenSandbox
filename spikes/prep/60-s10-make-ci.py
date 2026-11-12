"""Run a repository's `make ci` at a release tag inside a sandbox; record wall time, exit codes and peak memory.
Usage: DOCKER_HOST=unix://$HOME/.colima/ade-spike/docker.sock python 60-s10-make-ci.py <server_port> [tag]
Needs: fork-built execd image injected by the server (config-proto-execd.toml), image ade-stack-pynode:0 (stack-pynode.Dockerfile)."""
import asyncio, json, os, shutil, subprocess, sys, tempfile, threading, time
from datetime import timedelta
from opensandbox import Sandbox
from opensandbox.config import ConnectionConfig
from opensandbox.models.execd import RunCommandOpts

if "colima/ade-spike" not in os.environ.get("DOCKER_HOST", ""):
    sys.exit("refusing: DOCKER_HOST must point at the ade-spike VM socket")
port = sys.argv[1]; TAG = sys.argv[2] if len(sys.argv) > 2 else "agentbundle-v0.60.2"
SRC = os.path.expanduser(os.environ["CI_REPO_DIR"])  # repository whose `make ci` is timed
cfg = ConnectionConfig(api_key="spike-key", domain=f"127.0.0.1:{port}", protocol="http", use_server_proxy=False)
H = os.path.join(os.path.dirname(os.path.abspath(__file__)), "..", "git-remote-osb.OSB_BASE-patched")
peak = {"mib": 0.0}; stop = threading.Event()

def sampler():
    while not stop.is_set():
        out = subprocess.run(["docker", "stats", "--no-stream", "--format", "{{.Name}} {{.MemUsage}}"], capture_output=True, text=True).stdout
        for line in out.splitlines():
            n, u = line.split(" ", 1)
            if n.startswith("sandbox-"):
                v = u.split("/")[0].strip(); f = float("".join(c for c in v if c.isdigit() or c == "."))
                peak["mib"] = max(peak["mib"], f * (1024 if v.endswith("GiB") else 1 if v.endswith("MiB") else 1 / 1024))
        stop.wait(5)

async def main():
    tmp = tempfile.mkdtemp(prefix="ci-clone-")           # throwaway clone: the helper writes remote config
    subprocess.run(["git", "clone", "-q", "--no-hardlinks", SRC, tmp + "/r"], check=True)
    subprocess.run(["git", "-C", tmp + "/r", "branch", "-f", "release", TAG], check=True)
    shutil.copy(H, tmp + "/git-remote-osb"); os.chmod(tmp + "/git-remote-osb", 0o755)
    res = {"tag": TAG}
    threading.Thread(target=sampler, daemon=True).start()
    sb = await Sandbox.create("ade-stack-pynode:0", entrypoint=["tail", "-f", "/dev/null"], timeout=timedelta(minutes=90), connection_config=cfg,
                              env={"EXECD_GIT_REPO_ROOT": "/workspace", "EXECD_GIT_RUN_AS": "1000:1000", "EXECD_GIT_MAX_BUNDLE_BYTES": str(512 << 20)})
    try:
        ep = await sb.get_endpoint(44772)
        e = dict(os.environ, OSB_BASE=f"http://{ep.endpoint}", PATH=tmp + ":" + os.environ["PATH"], OSB_EXECD_TOKEN="", OSB_MAX_BUNDLE_BYTES=str(512 << 20))
        t = time.time(); r = subprocess.run(["git", "-C", tmp + "/r", "-c", "remote.sb.url=osb://x/workspace/repo", "push", "sb", "release:refs/heads/main"], capture_output=True, text=True, env=e)
        res["push"] = {"rc": r.returncode, "secs": round(time.time() - t, 1)}
        async def step(name, cmd, mins=60):
            t = time.time()
            ex = await sb.commands.run(cmd, opts=RunCommandOpts(uid=1000, gid=1000, working_directory="/workspace/repo", timeout=timedelta(minutes=mins),
                                       envs={"HOME": "/home/agent", "PATH": "/workspace/.venv/bin:/usr/local/bin:/usr/bin:/bin"}))
            so = "\n".join(m.text for m in ex.logs.stdout)[-400:]; res[name] = {"secs": round(time.time() - t, 1), "exit": ex.error.value if ex.error else "0", "tail": so}
        await step("checkout", "git checkout -q -f main && git log -1 --format=%h")
        await step("install", "python -m venv /workspace/.venv && pip install -q -r tools/requirements.txt -e packages/agentbundle -e packages/credbroker -e packages/jsonl-otlp-exporter mypy pytest > /tmp/i.out 2>&1; rc=$?; tail -2 /tmp/i.out; exit $rc")
        await step("npm_ci", "npm ci --prefix docs-site > /tmp/n.out 2>&1; rc=$?; tail -2 /tmp/n.out; exit $rc", 20)
        await step("make_ci", "make ci > /tmp/ci.out 2>&1; rc=$?; tail -25 /tmp/ci.out; exit $rc", 60)
        res["peak_sandbox_mem_mib"] = round(peak["mib"], 0)
    finally:
        stop.set(); await sb.kill(); await sb.close(); shutil.rmtree(tmp, ignore_errors=True)
        print(json.dumps(res, indent=1, default=str))
asyncio.run(main())
