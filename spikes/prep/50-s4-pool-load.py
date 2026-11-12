"""S-4 load: claim latency and memory per sandbox at N warm sandboxes. Refuses the default VM.
Usage: DOCKER_HOST=unix://$HOME/.colima/ade-spike/docker.sock python 50-s4-pool-load.py <server_port> <N> [<N> ...]"""
import asyncio, json, os, signal, statistics, subprocess, sys, time
from datetime import timedelta
from opensandbox import Sandbox, SandboxPoolAsync, InMemoryAsyncPoolStateStore, PoolCreationSpec
from opensandbox.config import ConnectionConfig
from opensandbox.pool_types import AcquirePolicy

dh = os.environ.get("DOCKER_HOST", "")
if "colima/ade-spike" not in dh:
    sys.exit("refusing: DOCKER_HOST must point at the ade-spike VM socket")
port, sizes = sys.argv[1], [int(x) for x in sys.argv[2:]] or [10, 25]
cfg = ConnectionConfig(api_key="spike-key", domain=f"127.0.0.1:{port}", protocol="http", use_server_proxy=True)

def stats():
    out = subprocess.run(["docker", "stats", "--no-stream", "--format", "{{.Name}} {{.MemUsage}}"], capture_output=True, text=True).stdout
    tot, n = 0.0, 0
    for line in out.splitlines():
        name, usage = line.split(" ", 1)
        if name.startswith("sandbox-"):
            v = usage.split("/")[0].strip(); f = float(''.join(c for c in v if c.isdigit() or c == '.'))
            tot += f * (1024 if v.endswith("GiB") else 1 if v.endswith("MiB") else 1/1024); n += 1
    return round(tot, 1), n

def vm_free_mib():
    out = subprocess.run(["docker", "run", "--rm", "alpine", "sh", "-c", "grep MemAvailable /proc/meminfo"], capture_output=True, text=True).stdout
    return int(out.split()[1]) // 1024

async def run(n, image):
    pool = SandboxPoolAsync(pool_name=f"load{n}", owner_id="gw", max_idle=n, state_store=InMemoryAsyncPoolStateStore(), connection_config=cfg,
                            creation_spec=PoolCreationSpec(image=image, entrypoint=["tail", "-f", "/dev/null"]), warmup_create_qps=5)
    held, res = [], {"N": n}
    try:
        if vm_free_mib() < 1500 + 60 * n: res["aborted"] = f"VM free memory too low for N={n}"; return res
        t0 = time.time(); await pool.start()
        while (await pool.snapshot()).idle_count < n and time.time() - t0 < 600: await asyncio.sleep(1)
        res["warm_secs"] = round(time.time() - t0, 1); res["idle"] = (await pool.snapshot()).idle_count
        res["mem_mib_total_idle"], res["containers"] = stats(); res["mem_mib_per_sandbox"] = round(res["mem_mib_total_idle"] / max(res["containers"], 1), 1)
        async def claim():
            t = time.time(); s = await pool.acquire(sandbox_timeout=timedelta(minutes=10), policy=AcquirePolicy.FAIL_FAST); return s, time.time() - t
        r = await asyncio.gather(*[claim() for _ in range(n)], return_exceptions=True)
        ok = [x for x in r if not isinstance(x, Exception)]; held += [x[0] for x in ok]; lat = sorted(x[1] for x in ok)
        res["claimed"] = len(ok); res["errors"] = sorted({type(x).__name__ for x in r if isinstance(x, Exception)})
        if lat: res["claim_p50"], res["claim_p95"], res["claim_max"] = round(statistics.median(lat), 2), round(lat[int(0.95 * (len(lat) - 1))], 2), round(lat[-1], 2)
        t1 = time.time()
        while (await pool.snapshot()).idle_count < n and time.time() - t1 < 600: await asyncio.sleep(1)
        res["refill_secs"] = round(time.time() - t1, 1)
        res["mem_mib_total_after_refill"], res["containers_after"] = stats()
    finally:
        for s in held:
            try: await s.kill(); await s.close()
            except Exception: pass
        await pool.shutdown(graceful=False)
        # shutdown leaves idle sandboxes (seen in S-4): delete every container this run created
        subprocess.run("docker rm -f $(docker ps -aq --filter name=sandbox-) >/dev/null 2>&1", shell=True)
    return res

async def main():
    image = os.environ.get("SPIKE_IMAGE", "ade-spike-base:0")
    for n in sizes:
        print(json.dumps(await run(n, image), indent=1), flush=True)
asyncio.run(main())
