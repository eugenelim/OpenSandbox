#!/usr/bin/env bash
# Spike F. Creates a throwaway EKS cluster in the named sandbox account, runs F1-F4, deletes the cluster.
set -u; . "$(dirname "$0")/common.sh"; guard_aws
CL=ade-spike-fargate; OUT="$RES/fargate-$TS.txt"; mkdir -p "$RES"
cfg=$(mktemp); render_eksctl "$HERE/eksctl/fargate.eksctl.yaml" "$cfg"
cleanup() { [ "${ADE_KEEP:-}" = yes ] && { echo "keeping cluster $CL"; return; }; eksctl delete cluster --name $CL --region "$AWS_REGION" --wait; rm -f "$cfg"; }
trap cleanup EXIT
eksctl create cluster -f "$cfg" || exit 1
kubectl create namespace fargate-spike
{
echo "== F1 capability probe"; for p in probe-default probe-net-admin; do kubectl apply -f "$HERE/manifests/f1-cap-probe.yaml" >/dev/null 2>&1; done
for p in probe-default probe-net-admin; do kubectl -n fargate-spike wait --for=condition=Ready pod/$p --timeout=420s 2>&1 | tail -1; sleep 20; echo "-- $p"; kubectl -n fargate-spike logs $p 2>&1 | tail -8; kubectl -n fargate-spike describe pod $p 2>&1 | grep -iE "warning|error|fargate|capab" | head -5; done
for f in f2-egress-dns-nft:sidecar-dns-nft f4-egress-dns:sidecar-dns; do
  m=${f%%:*}; p=${f#*:}; echo "== $m"; kubectl apply -f "$HERE/manifests/$m.yaml" 2>&1 | tail -2
  if kubectl -n fargate-spike wait --for=condition=Ready pod/$p --timeout=420s 2>&1 | tail -1; then sleep 15; egress_probe fargate-spike $p; fi
  kubectl -n fargate-spike describe pod $p 2>&1 | grep -iE "warning|failed|error|forbidden" | head -6; kubectl -n fargate-spike logs $p -c egress 2>&1 | tail -6
done
echo "== F3 privileged init"; kubectl apply -f "$HERE/manifests/f3-privileged-init.yaml" 2>&1 | tail -3; sleep 45; kubectl -n fargate-spike get pod privileged-init 2>&1 | tail -2; kubectl -n fargate-spike describe pod privileged-init 2>&1 | grep -iE "warning|failed|forbidden|privileged" | head -6
} 2>&1 | tee "$OUT"
echo "results: $OUT"
