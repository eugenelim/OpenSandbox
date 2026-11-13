# Pooled-authentication test: claims from a warm Kata pool through the OpenSandbox server. Usage: OS_PORT=18090 python pool.py
import httpx, json, os, secrets, time, concurrent.futures as cf
S = "http://127.0.0.1:%s/v1" % os.environ.get("OS_PORT", "18090")
POOL = os.environ.get("POOL", "pool-a"); POOLTOK = os.environ.get("POOL_TOKEN", "pool-a-shared-secret")
IMG = "public.ecr.aws/docker/library/python:3.12-slim"
out = {}
def rec(k, v): out[k] = v; print(k, "=>", json.dumps(v, default=str)[:400], flush=True)
def claim(tag):
    tok = secrets.token_hex(16); t = time.time()
    r = httpx.post(S + "/sandboxes", json={"image": {"uri": IMG}, "entrypoint": ["tail", "-f", "/dev/null"], "timeout": 900,
        "resourceLimits": {"cpu": "500m", "memory": "512Mi"}, "extensions": {"poolRef": POOL}, "env": {"EXECD_ACCESS_TOKEN": tok}}, timeout=90)
    if r.status_code >= 300: return {"tag": tag, "err": r.status_code, "body": r.text[:300]}
    sid = r.json()["id"]
    for _ in range(120):
        j = httpx.get(S + "/sandboxes/" + sid, timeout=30).json()
        if j.get("status", {}).get("state") == "Running": break
        time.sleep(0.5)
    return {"tag": tag, "id": sid, "tok": tok, "claim_secs": round(time.time() - t, 2)}
def execd(sb, path, tok=None, method="GET", body=None):
    h = {"X-EXECD-ACCESS-TOKEN": tok} if tok else {}
    return httpx.request(method, "%s/sandboxes/%s/proxy/44772%s" % (S, sb["id"], path), headers=h, json=body, timeout=120)
def run(sb, cmd):
    r = execd(sb, "/command", sb["tok"], "POST", {"command": cmd, "timeout": 60000})
    texts = []
    for line in r.text.splitlines():
        line = line.strip()
        if line.startswith("data:"): line = line[5:].strip()
        if not line.startswith("{"): continue
        try:
            j = json.loads(line)
            if j.get("type") in ("stdout", "stderr"): texts.append(j.get("text") or "")
        except Exception: pass
    return (r.status_code, "".join(texts).strip()[:300])
def ip_of(sb):
    r = httpx.get("%s/sandboxes/%s/endpoints/44772" % (S, sb["id"]), params={"use_server_proxy": "false"}, timeout=30)
    return r.json().get("endpoint", "").split(":")[0] if r.status_code == 200 else None
PYGET = "python3 -c \"import urllib.request,sys\ntry:\n r=urllib.request.urlopen(sys.argv[1],timeout=6);print(r.status)\nexcept Exception as e: print('ERR',type(e).__name__,str(e)[:60])\" "
# 1. concurrent claims
t = time.time()
with cf.ThreadPoolExecutor(3) as ex: cl = list(ex.map(claim, ["A", "B", "C"]))
rec("claims", [{k: v for k, v in c.items() if k != "tok"} for c in cl]); rec("claims_wall_secs", round(time.time() - t, 2))
ok = [c for c in cl if "id" in c]
if len(ok) < 2: raise SystemExit("not enough claims succeeded")
A, B = ok[0], ok[1]
# 2. execd authentication per claim
rec("A_ping_no_token", execd(A, "/ping").status_code); rec("A_ping_own_token", execd(A, "/ping", A["tok"]).status_code); rec("A_ping_B_token", execd(A, "/ping", B["tok"]).status_code)
rec("tokens_distinct", A["tok"] != B["tok"])
def cmd_status(sb, tok): return execd(sb, "/command", tok, "POST", {"command": "id -u", "timeout": 10000}).status_code
rec("A_command_no_token", cmd_status(A, None)); rec("A_command_wrong_token", cmd_status(A, "not-the-token")); rec("A_command_B_token", cmd_status(A, B["tok"])); rec("A_command_own_token", cmd_status(A, A["tok"]))
ipA, ipB = ip_of(A), ip_of(B); rec("pod_ips", [ipA, ipB])
rec("A_runs_command", run(A, "id -u; uname -r"))
# 3. cross-sandbox from inside A
if ipB:
    rec("A_to_B_execd_no_token", run(A, PYGET + "http://%s:44772/ping" % ipB))
    rec("A_to_B_sidecar_no_token", run(A, PYGET + "http://%s:18080/policy" % ipB))
rec("A_to_own_sidecar_no_token", run(A, PYGET + "http://127.0.0.1:18080/policy"))
# 4. egress: default deny, then policy through the control plane
rec("A_egress_default_deny", run(A, PYGET + "https://example.com"))
pr = httpx.patch("%s/sandboxes/%s/networkpolicy" % (S, A["id"]), json=[{"action": "allow", "target": "example.com"}], timeout=60)
rec("patch_policy_via_server", [pr.status_code, pr.text[:300]])
pr2 = httpx.get("%s/sandboxes/%s/networkpolicy" % (S, A["id"]), timeout=30); rec("get_policy_via_server", [pr2.status_code, pr2.text[:300]])
time.sleep(3); rec("A_egress_after_allow", run(A, PYGET + "https://example.com"))
# 5. policy through the sidecar directly with the pool-wide token (from B, a different sandbox)
if ipA:
    rec("B_reads_A_policy_with_pool_token", run(B, "python3 -c \"import urllib.request\nr=urllib.request.Request('http://%s:18080/policy',headers={'OPENSANDBOX-EGRESS-AUTH':'%s'})\ntry:\n print(urllib.request.urlopen(r,timeout=6).read()[:200])\nexcept Exception as e: print('ERR',type(e).__name__,str(e)[:80])\"" % (ipA, POOLTOK)))
# 6. Gateway-equivalent control path from a neutral pod outside the sandboxes (stands in for the Gateway)
import subprocess
def ctl(code):
    r = subprocess.run(["kubectl", "-n", "opensandbox", "exec", "ctl", "--", "python3", "-c", code], capture_output=True, text=True, timeout=90)
    return (r.stdout.strip() or r.stderr.strip())[:200]
def side(ip, method, body=None, tok=POOLTOK):
    return ctl("import urllib.request,json\nd=%r\nr=urllib.request.Request('http://%s:18080/policy',data=(json.dumps(d).encode() if d is not None else None),method='%s',headers={'OPENSANDBOX-EGRESS-AUTH':%r,'Content-Type':'application/json'})\ntry:\n x=urllib.request.urlopen(r,timeout=8);print(x.status,x.read()[:150])\nexcept Exception as e: print('ERR',type(e).__name__,str(e)[:90])" % (body, ip, method, tok))
subprocess.run(["kubectl", "-n", "opensandbox", "run", "ctl", "--image=" + IMG, "--restart=Never", "--command", "--", "sleep", "3600"], capture_output=True)
subprocess.run(["kubectl", "-n", "opensandbox", "wait", "--for=condition=Ready", "pod/ctl", "--timeout=120s"], capture_output=True)
rec("ctl_reads_A_policy_with_pool_token", side(ipA, "GET"))
rec("ctl_reads_B_policy_with_same_token", side(ipB, "GET"))
rec("ctl_reads_A_policy_without_token", side(ipA, "GET", tok=""))
rec("ctl_patch_A_allow_example", side(ipA, "PATCH", [{"action": "allow", "target": "example.com"}]))
time.sleep(3); rec("A_egress_after_sidecar_allow", run(A, PYGET + "https://example.com"))
rec("B_egress_unchanged", run(B, PYGET + "https://example.com"))
rec("ctl_delete_A_rule", side(ipA, "DELETE", ["example.com"]))
time.sleep(3); rec("A_egress_after_narrow", run(A, PYGET + "https://example.com"))
rec("ctl_execd_A_no_token_command", ctl("import urllib.request,json\nr=urllib.request.Request('http://%s:44772/command',data=json.dumps({'command':'id -u'}).encode(),method='POST',headers={'Content-Type':'application/json'})\ntry:\n print(urllib.request.urlopen(r,timeout=8).status)\nexcept Exception as e: print('ERR',type(e).__name__,str(e)[:60])" % ipA))
# 7. cleanup and refill
for c in ok: httpx.delete(S + "/sandboxes/" + c["id"], timeout=30)
subprocess.run(["kubectl", "-n", "opensandbox", "delete", "pod", "ctl", "--wait=false"], capture_output=True)
print("RESULT_JSON", json.dumps(out, default=str))
