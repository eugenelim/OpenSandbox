# K8/K9: OpenSandbox on Kubernetes with secure_runtime kata. Usage: k8.py <image>  (server port-forwarded to 18080)
import asyncio, sys, time, json
from datetime import timedelta
from opensandbox import Sandbox
from opensandbox.config import ConnectionConfig
IMG = sys.argv[1] if len(sys.argv) > 1 else "public.ecr.aws/docker/library/python:3.12-slim"
cfg = ConnectionConfig(api_key="spike-key", domain="127.0.0.1:18080", protocol="http", use_server_proxy=True)
res = {}
async def step(name, coro):
    t = time.time()
    try: r = await coro; res[name] = {"ok": True, "secs": round(time.time() - t, 2), "v": str(r)[:200]}; return r
    except Exception as e: res[name] = {"ok": False, "secs": round(time.time() - t, 2), "err": f"{type(e).__name__}: {str(e)[:300]}"}; return None
async def main():
    sb = await step("create", Sandbox.create(IMG, entrypoint=["tail", "-f", "/dev/null"], timeout=timedelta(minutes=20), connection_config=cfg))
    if sb:
        res["sandbox_id"] = sb.id
        ex = await step("cmd_uname", sb.commands.run("uname -r; nproc; grep MemTotal /proc/meminfo; id -u"))
        if ex: res["cmd_uname"]["out"] = [l.text.strip() for l in ex.logs.stdout]
        await step("file_write", sb.files.write_file("/tmp/k8.txt", "hello-k8"))
        r = await step("file_read", sb.files.read_file("/tmp/k8.txt")); 
        ex = await step("cmd_files", sb.commands.run("cd /tmp && for i in $(seq 1 2000); do echo $i > f$i; done; ls /tmp | wc -l"))
        await step("info", sb.get_info())
        await step("snapshot", sb.create_snapshot("k9-snap"))
        await step("pause", sb.pause())
        await asyncio.sleep(20)
        await step("info_after_pause", sb.get_info())
        r2 = await step("resume", Sandbox.resume(sb.id, connection_config=cfg)) if hasattr(Sandbox, "resume") else None
        if r2:
            ex = await step("cmd_after_resume", r2.commands.run("cat /tmp/k8.txt"))
            if ex: res["cmd_after_resume"]["out"] = [l.text.strip() for l in ex.logs.stdout]
            sb = r2
        await step("kill", sb.kill())
        try: await sb.close()
        except Exception: pass
    print(json.dumps(res, indent=1, default=str))
asyncio.run(main())
