import asyncio, time, json
from datetime import timedelta
from opensandbox import Sandbox, SandboxPoolAsync, InMemoryAsyncPoolStateStore, PoolCreationSpec
from opensandbox.config import ConnectionConfig
from opensandbox.pool_types import AcquirePolicy
cfg = ConnectionConfig(api_key="spike-key", domain="127.0.0.1:18080", protocol="http", use_server_proxy=True)
res = {}
async def timed(coro):
    t = time.time(); r = await coro; return r, round(time.time() - t, 2)
async def main():
    sb, t = await timed(Sandbox.create("ade-spike-base:0", entrypoint=["tail","-f","/dev/null"], timeout=timedelta(minutes=10), connection_config=cfg))
    res["cold_create_secs"] = t; await sb.kill(); await sb.close()
    pool = SandboxPoolAsync(pool_name="spike", owner_id="gw-1", max_idle=4, state_store=InMemoryAsyncPoolStateStore(), connection_config=cfg,
        creation_spec=PoolCreationSpec(image="ade-spike-base:0", entrypoint=["tail","-f","/dev/null"]), warmup_create_qps=4)
    held = []
    try:
        t0 = time.time(); await pool.start()
        while (await pool.snapshot()).idle_count < 4 and time.time() - t0 < 90: await asyncio.sleep(0.5)
        res["warm_to_4_secs"] = round(time.time() - t0, 1); res["idle_after_warm"] = (await pool.snapshot()).idle_count
        # burst: 4 concurrent FAIL_FAST claims
        r = await asyncio.gather(*[timed(pool.acquire(sandbox_timeout=timedelta(minutes=5), policy=AcquirePolicy.FAIL_FAST)) for _ in range(4)], return_exceptions=True)
        ok = [x for x in r if not isinstance(x, Exception)]; held += [x[0] for x in ok]
        res["burst4_failfast"] = {"ok": len(ok), "errors": [type(x).__name__ for x in r if isinstance(x, Exception)], "claim_secs": [x[1] for x in ok]}
        res["distinct_ids"] = len({s.id for s in held})
        # buffer now empty: next claim with DIRECT_CREATE falls back to create
        s5, t5 = await timed(pool.acquire(sandbox_timeout=timedelta(minutes=5), policy=AcquirePolicy.DIRECT_CREATE)); held.append(s5); res["claim_when_empty_direct_create_secs"] = t5
        # replenish
        t1 = time.time()
        while (await pool.snapshot()).idle_count < 4 and time.time() - t1 < 90: await asyncio.sleep(0.5)
        res["refill_to_4_secs"] = round(time.time() - t1, 1)
        # one-use: claimed sandbox works and is not handed out twice
        ex = await held[0].commands.run("echo claimed-ok"); res["claimed_runs"] = ex.logs.stdout[0].text.strip()
        res["snapshot"] = str(await pool.snapshot())[:200]
    finally:
        for s in held:
            try: await s.kill(); await s.close()
            except Exception: pass
        await pool.shutdown(graceful=False)
        print(json.dumps(res, indent=1, default=str))
asyncio.run(main())
