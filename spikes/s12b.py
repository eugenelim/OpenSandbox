import asyncio, json, subprocess
from datetime import timedelta
from opensandbox import Sandbox
from opensandbox.config import ConnectionConfig
cfg = ConnectionConfig(api_key="spike-key", domain="127.0.0.1:18080", protocol="http", use_server_proxy=True)
res = {}
def cname(i): return subprocess.run(["docker","ps","--filter",f"name={i}","--format","{{.Names}}"],capture_output=True,text=True).stdout.split()[0]
def api(c, body, token="tok-B"):
    cmd = "curl -s -m 10 -N -X POST http://localhost:44772/command -H 'Content-Type: application/json' -H 'X-EXECD-ACCESS-TOKEN: %s' -d '%s'" % (token, json.dumps(body))
    r = subprocess.run(["docker","exec","-u","0",c,"sh","-c",cmd],capture_output=True,text=True,timeout=60).stdout
    return [json.loads(l)["text"] for l in r.splitlines() if l.startswith('{"type":"stdout"')]
PROBE = "for p in /proc/[0-9]*; do tr '\\\\0' '\\\\n' < $p/environ 2>/dev/null; done | grep -c EXECD_ACCESS_TOKEN; id -u"
async def main():
    B = await Sandbox.create("ade-spike-base:0", entrypoint=["tail","-f","/dev/null"], env={"EXECD_ACCESS_TOKEN":"tok-B"}, timeout=timedelta(minutes=10), connection_config=cfg, skip_health_check=True)
    try:
        c = cname(B.id); await asyncio.sleep(4)
        res["plain_command_uid1000_sees_token_in_any_readable_environ"] = api(c, {"command": PROBE, "uid": 1000, "gid": 1000})
        res["exec_env_-u_command_uid1000_sees_token"] = api(c, {"command": "exec env -u EXECD_ACCESS_TOKEN sh -c \"%s\"" % PROBE, "uid": 1000, "gid": 1000})
        res["uid1000_call_to_execd_after_unset_no_token"] = api(c, {"command": "exec env -u EXECD_ACCESS_TOKEN sh -c \"curl -s -m 5 -o /dev/null -w %{http_code} -X POST localhost:44772/command -H 'Content-Type: application/json' -d '{\\\\\\\"command\\\\\\\":\\\\\\\"id\\\\\\\"}'\"", "uid": 1000, "gid": 1000})
    finally:
        await B.kill(); await B.close(); print(json.dumps(res, indent=1))
asyncio.run(main())
