import asyncio, json
from datetime import timedelta
from opensandbox import Sandbox
from opensandbox.config import ConnectionConfig
from opensandbox.models.sandboxes import NetworkPolicy, NetworkRule
cfg = ConnectionConfig(api_key="spike-key", domain="127.0.0.1:18080", protocol="http", use_server_proxy=True)
res = {}
async def sh(sb, c):
    ex = await sb.commands.run(c); return ("\n".join(m.text for m in ex.logs.stdout) + "|" + "\n".join(m.text for m in ex.logs.stderr)).strip()[:160], (ex.error.value if ex.error else "ok")
async def main():
    pol = NetworkPolicy(default_action="deny", egress=[NetworkRule(action="allow", target="pypi.org")])
    sb = await Sandbox.create("ade-spike-base:0", entrypoint=["tail","-f","/dev/null"], timeout=timedelta(minutes=5), network_policy=pol, connection_config=cfg, ready_timeout=timedelta(seconds=60))
    try:
        tests = {
          "allowed_pypi_https": "curl -s -m 8 -o /dev/null -w '%{http_code}' https://pypi.org/simple/",
          "unlisted_example_com": "curl -s -m 8 -o /dev/null -w '%{http_code}' https://example.com/ ; echo rc=$?",
          "unlisted_dns": "getent hosts example.com || echo dns-failed",
          "direct_ip_1.1.1.1": "curl -s -m 6 -o /dev/null -w '%{http_code}' http://1.1.1.1/ ; echo rc=$?",
          "metadata_169.254.169.254": "curl -s -m 5 -o /dev/null -w '%{http_code}' http://169.254.169.254/ ; echo rc=$?",
          "github": "curl -s -m 6 -o /dev/null -w '%{http_code}' https://github.com/ ; echo rc=$?",
          "rfc1918_gateway": "curl -s -m 5 -o /dev/null -w '%{http_code}' http://192.168.5.2/ ; echo rc=$?",
        }
        for k, c in tests.items(): res[k] = await sh(sb, c)
        # runtime mutation: allow github, re-test
        await sb.patch_egress_rules([NetworkRule(action="allow", target="github.com")])
        res["after_patch_github"] = await sh(sb, tests["github"])
        res["policy"] = str(await sb.get_egress_policy())[:300]
    finally:
        await sb.kill(); await sb.close(); print(json.dumps(res, indent=1, default=str))
asyncio.run(main())
