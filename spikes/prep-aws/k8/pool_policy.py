# Per-claim egress policy on a pooled Kata sandbox, set through the sidecar with the pool-wide token from a neutral pod; target is an in-VPC server (no internet needed).
import httpx, json, os, secrets, subprocess, time
S = "http://127.0.0.1:%s/v1" % os.environ.get("OS_PORT", "18090"); POOL = "pool-a"; POOLTOK = "pool-a-shared-secret"
IMG = "public.ecr.aws/docker/library/python:3.12-slim"; out = {}
def rec(k, v): out[k] = v; print(k, "=>", json.dumps(v, default=str)[:300], flush=True)
def k(*a, t=120): r = subprocess.run(["kubectl", "-n", "opensandbox", *a], capture_output=True, text=True, timeout=t); return (r.stdout.strip() or r.stderr.strip())
SERVER = "import http.server,time\nclass H(http.server.BaseHTTPRequestHandler):\n def do_GET(s):\n  s.send_response(200);s.send_header('Content-Type','text/plain');s.end_headers()\n  for i in range(60):\n   s.wfile.write(b'x\\n');s.wfile.flush();time.sleep(1)\nhttp.server.ThreadingHTTPServer(('0.0.0.0',8000),H).serve_forever()"
k("run", "ctl", "--image=" + IMG, "--restart=Never", "--command", "--", "python3", "-c", SERVER); k("wait", "--for=condition=Ready", "pod/ctl", "--timeout=120s")
ctlip = k("get", "pod", "ctl", "-o", "jsonpath={.status.podIP}"); rec("ctl_ip", ctlip)
tok = secrets.token_hex(16)
r = httpx.post(S + "/sandboxes", json={"image": {"uri": IMG}, "entrypoint": ["tail", "-f", "/dev/null"], "timeout": 900, "resourceLimits": {"cpu": "500m", "memory": "512Mi"}, "extensions": {"poolRef": POOL}, "env": {"EXECD_ACCESS_TOKEN": tok}}, timeout=90)
sid = r.json()["id"]
for _ in range(120):
    if httpx.get(S + "/sandboxes/" + sid, timeout=30).json().get("status", {}).get("state") == "Running": break
    time.sleep(0.5)
ip = httpx.get("%s/sandboxes/%s/endpoints/44772" % (S, sid), params={"use_server_proxy": "false"}, timeout=30).json()["endpoint"].split(":")[0]; rec("A_ip", ip)
def run(cmd, timeout=60000):
    rr = httpx.post("%s/sandboxes/%s/proxy/44772/command" % (S, sid), headers={"X-EXECD-ACCESS-TOKEN": tok}, json={"command": cmd, "timeout": timeout}, timeout=120)
    txt = []
    for line in rr.text.splitlines():
        line = line.strip()
        if line.startswith("{"):
            try:
                j = json.loads(line)
                if j.get("type") in ("stdout", "stderr"): txt.append(j.get("text") or "")
            except Exception: pass
    return "".join(txt).strip()[:200]
FETCH = "python3 -c \"import urllib.request\ntry:\n r=urllib.request.urlopen('http://%s:8000/',timeout=4);print('OK',r.status)\nexcept Exception as e: print('BLOCKED',type(e).__name__,str(e)[:50])\"" % ctlip
def side(method, body=None):
    code = "import urllib.request,json\nd=%r\nr=urllib.request.Request('http://%s:18080/policy',data=(json.dumps(d).encode() if d is not None else None),method='%s',headers={'OPENSANDBOX-EGRESS-AUTH':%r,'Content-Type':'application/json'})\ntry:\n x=urllib.request.urlopen(r,timeout=8);print(x.status,x.read()[:120])\nexcept Exception as e: print('ERR',type(e).__name__,str(e)[:80])" % (body, ip, method, POOLTOK)
    return k("exec", "ctl", "--", "python3", "-c", code)
rec("1_default_deny_to_ctl", run(FETCH))
rec("2_patch_allow_ctl_ip", side("PATCH", [{"action": "allow", "target": ctlip}]))
time.sleep(3); rec("3_after_allow", run(FETCH))
# established connection: start a streaming client in A, narrow mid-stream, see whether data keeps flowing
run("nohup python3 -c \"import urllib.request,time\nr=urllib.request.urlopen('http://%s:8000/',timeout=60)\nn=0\nt=time.time()\nwhile time.time()-t<40:\n d=r.readline()\n if not d: break\n n+=1\n open('/tmp/stream.txt','w').write(str(n)+' '+str(round(time.time()-t)))\" > /dev/null 2>&1 &" % ctlip)
time.sleep(8); rec("4_stream_before_narrow", run("cat /tmp/stream.txt"))
rec("5_delete_rule", side("DELETE", [ctlip]))
time.sleep(10); rec("6_stream_after_narrow_10s", run("cat /tmp/stream.txt")); rec("7_new_connection_after_narrow", run(FETCH))
rec("8_policy_now", side("GET"))
httpx.delete(S + "/sandboxes/" + sid, timeout=30); k("delete", "pod", "ctl", "--wait=false")
print("RESULT_JSON", json.dumps(out, default=str))
