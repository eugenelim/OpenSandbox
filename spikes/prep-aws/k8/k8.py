# K8/K9: OpenSandbox on Kubernetes with secure_runtime kata. Usage: k8.py <image>  (server port-forwarded to 18080)
import asyncio, os, sys, time, json
from datetime import timedelta
from opensandbox import Sandbox
from opensandbox.config import ConnectionConfig
IMG = sys.argv[1] if len(sys.argv) > 1 else "public.ecr.aws/docker/library/python:3.12-slim"
cfg = ConnectionConfig(api_key="spike-key", domain="127.0.0.1:" + os.environ.get("OS_PORT", "18090"), protocol="http", use_server_proxy=True)
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
        await step("outside_rootfs_write", sb.commands.run("echo shm-data > /dev/shm/k8.shm; echo var-data > /var/tmp/k8.var; sync"))
        r = await step("file_read", sb.files.read_file("/tmp/k8.txt")); 
        ex = await step("cmd_files", sb.commands.run("cd /tmp && for i in $(seq 1 2000); do echo $i > f$i; done; ls /tmp | wc -l"))
        await step("info", sb.get_info())
        await step("snapshot", sb.create_snapshot("k9-snap"))
        await step("pause", sb.pause())
        t0 = time.time(); state = ""
        while time.time() - t0 < 240:
            info = await sb.get_info(); state = str(getattr(info.status, "state", info.status))
            if "aused" in state: break
            await asyncio.sleep(5)
        res["pause_poll"] = {"final_state": state, "secs": round(time.time() - t0, 1)}
        r2 = await step("resume", Sandbox.resume(sb.id, connection_config=cfg)) if hasattr(Sandbox, "resume") else None
        if r2:
            ex = await step("cmd_after_resume", r2.commands.run("cat /tmp/k8.txt; cat /dev/shm/k8.shm 2>&1; cat /var/tmp/k8.var 2>&1; uname -r"))
            if ex: res["cmd_after_resume"]["out"] = [l.text.strip() for l in ex.logs.stdout]
            sb = r2
        snap = res.get("snapshot", {}).get("v", "")
        sid = snap.split("id='")[1].split("'")[0] if "id='" in snap else None
        if sid:
            rs = await step("restore_from_snapshot", Sandbox.create(snapshot_id=sid, entrypoint=["tail", "-f", "/dev/null"], timeout=timedelta(minutes=10), connection_config=cfg))
            if rs:
                ex = await step("restore_cmd", rs.commands.run("cat /tmp/k8.txt; cat /dev/shm/k8.shm 2>&1; cat /var/tmp/k8.var 2>&1; uname -r"))
                if ex: res["restore_cmd"]["out"] = [l.text.strip() for l in ex.logs.stdout]
                await step("restore_kill", rs.kill())
        await step("kill", sb.kill())
        try: await sb.close()
        except Exception: pass
    print(json.dumps(res, indent=1, default=str))
asyncio.run(main())
