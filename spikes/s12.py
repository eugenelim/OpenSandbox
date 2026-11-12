import asyncio, json, subprocess
from datetime import timedelta
from opensandbox import Sandbox
from opensandbox.config import ConnectionConfig
cfg = ConnectionConfig(api_key="spike-key", domain="127.0.0.1:18080", protocol="http", use_server_proxy=True)
res = {}
def dx(c, cmd, user="1000"):
    r = subprocess.run(["docker", "exec", "-u", user, c, "sh", "-c", cmd], capture_output=True, text=True, timeout=60)
    return (r.stdout + r.stderr).strip()[:300]
def cname(sb_id):
    return subprocess.run(["docker", "ps", "--filter", f"name={sb_id}", "--format", "{{.Names}}"], capture_output=True, text=True).stdout.split()[0]
def ip(c):
    return subprocess.run(["docker", "inspect", "-f", "{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}", c], capture_output=True, text=True).stdout.strip()
CMD = lambda body, hdr="": "curl -s -m 8 -N -X POST http://HOST:44772/command -H 'Content-Type: application/json' " + hdr + " -d '" + json.dumps(body) + "'"
async def main():
    A = await Sandbox.create("ade-spike-base:0", entrypoint=["tail", "-f", "/dev/null"], timeout=timedelta(minutes=10), connection_config=cfg)
    B = await Sandbox.create("ade-spike-base:0", entrypoint=["tail", "-f", "/dev/null"], env={"EXECD_ACCESS_TOKEN": "tok-B"}, timeout=timedelta(minutes=10), connection_config=cfg, skip_health_check=True)
    try:
        ca, cb = cname(A.id), cname(B.id); ipa, ipb = ip(ca), ip(cb)
        await asyncio.sleep(4)
        res["A_no_token_uid1000_runs_command_as"] = dx(ca, CMD({"command": "id"}).replace("HOST","localhost"))
        res["B_token_set_uid1000_no_header"] = dx(cb, CMD({"command": "id"}).replace("HOST","localhost"))
        res["B_token_set_uid1000_wrong_header"] = dx(cb, CMD({"command": "id"}, "-H 'X-EXECD-ACCESS-TOKEN: guess'").replace("HOST","localhost"))
        res["B_authenticated_child_env_has_token"] = dx(cb, CMD({"command": "env | grep -c EXECD_ACCESS_TOKEN; id -u", "uid": 1000, "gid": 1000}, "-H 'X-EXECD-ACCESS-TOKEN: tok-B'").replace("HOST","localhost"), user="0")
        res["B_uid1000_can_read_token_from_any_proc_environ"] = dx(cb, "for p in /proc/[0-9]*; do tr '\\0' '\\n' < $p/environ 2>/dev/null | grep -c EXECD_ACCESS_TOKEN; done | sort | uniq -c")
        res["B_container_env_visible_to_docker"] = subprocess.run(["docker", "inspect", "-f", "{{range .Config.Env}}{{println .}}{{end}}", cb], capture_output=True, text=True).stdout.count("EXECD_ACCESS_TOKEN")
        res["cross_sandbox_B_to_A_no_token_command_exec"] = dx(cb, CMD({"command": "hostname"}).replace("HOST",ipa))
        res["cross_sandbox_A_to_B_token_set"] = dx(ca, CMD({"command": "hostname"}).replace("HOST",ipb))
        res["bridge_ips"] = [ipa, ipb]
    finally:
        for s in (A, B):
            try: await s.kill(); await s.close()
            except Exception as e: res.setdefault("cleanup_errors", []).append(str(e)[:80])
        print(json.dumps(res, indent=1))
asyncio.run(main())
