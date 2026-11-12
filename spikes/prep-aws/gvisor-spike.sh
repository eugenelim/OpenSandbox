#!/usr/bin/env bash
# Spike E. Throwaway EKS cluster with a gVisor node group (UNTESTED bootstrap), runs E1/E2, deletes the cluster.
set -u; . "$(dirname "$0")/common.sh"; guard_aws
CL=ade-spike-gvisor; OUT="$RES/gvisor-$TS.txt"; mkdir -p "$RES"
cfg=$(mktemp); UD=$(sed 's/"/\\"/g' "$HERE/eksctl/gvisor-nodegroup.userdata.sh" | awk '{printf "%s\\n",$0}')
python3 - "$HERE/eksctl/gvisor.eksctl.yaml" "$cfg" "$AWS_REGION" "$HERE/eksctl/gvisor-nodegroup.userdata.sh" <<'PY'
import sys,re
src,dst,region,ud=sys.argv[1:5]
t=open(src).read().replace("REGION_PLACEHOLDER",region)
body=open(ud).read().strip().splitlines()
cmds="\n".join("  - "+__import__("json").dumps(l) for l in body if l.strip() and not l.startswith("#!"))
t=re.sub(r'  preBootstrapCommands:\n  - .*\n',"  preBootstrapCommands:\n"+cmds+"\n",t)
open(dst,"w").write(t)
PY
cleanup() { [ "${ADE_KEEP:-}" = yes ] && { echo "keeping cluster $CL"; return; }; eksctl delete cluster --name $CL --region "$AWS_REGION" --wait; rm -f "$cfg"; }
trap cleanup EXIT
render_eksctl "$cfg" "$cfg.r" && mv "$cfg.r" "$cfg"
eksctl create cluster -f "$cfg" || exit 1
kubectl create namespace gvisor-spike; kubectl apply -f "$HERE/manifests/e0-runtimeclass-gvisor.yaml"
{
echo "== E2 gVisor active?"; kubectl apply -f "$HERE/manifests/e2-gvisor-active.yaml"; kubectl -n gvisor-spike wait --for=condition=Ready pod/gvisor-active --timeout=300s 2>&1 | tail -1; sleep 8; kubectl -n gvisor-spike logs gvisor-active 2>&1 | head -8
for f in e1a-gvisor-dns-nft:gvisor-dns-nft e1b-gvisor-dns:gvisor-dns; do
  m=${f%%:*}; p=${f#*:}; echo "== $m"; kubectl apply -f "$HERE/manifests/$m.yaml" 2>&1 | tail -2
  if kubectl -n gvisor-spike wait --for=condition=Ready pod/$p --timeout=420s 2>&1 | tail -1; then sleep 15; egress_probe gvisor-spike $p; fi
  kubectl -n gvisor-spike describe pod $p 2>&1 | grep -iE "warning|failed|error" | head -6; kubectl -n gvisor-spike logs $p -c egress 2>&1 | tail -8
done
} 2>&1 | tee "$OUT"
echo "results: $OUT"
