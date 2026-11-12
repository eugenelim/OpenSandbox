#!/usr/bin/env bash
# One throwaway EKS cluster: Kata on a bare-metal node (kata-deploy), then F5 (Fargate without sidecar). Deletes the cluster on exit.
set -u; . "$(dirname "$0")/common.sh"; guard_aws; need helm
CL=ade-spike-combined; OUT="$RES/combined-$TS.txt"; mkdir -p "$RES"
cfg=$(mktemp); render_eksctl "$HERE/eksctl/combined.eksctl.yaml" "$cfg"
SG=""
cleanup() {
  [ "${ADE_KEEP:-}" = yes ] && { echo "keeping cluster $CL"; return; }
  eksctl delete cluster --name $CL --region "$AWS_REGION" --wait
  [ -n "$SG" ] && aws ec2 delete-security-group --group-id "$SG" --region "$AWS_REGION" 2>/dev/null
  rm -f "$cfg"
}
trap cleanup EXIT
probe() { # $1 label
  echo "-- $1"
  for c in \
   "direct_ip_1.1.1.1:env -u HTTP_PROXY -u HTTPS_PROXY curl -s -m 6 -o /dev/null -w %{http_code} http://1.1.1.1/" \
   "direct_dns_8.8.8.8:dig +time=3 +tries=1 @8.8.8.8 example.com +short" \
   "metadata_169.254.169.254:env -u HTTP_PROXY -u HTTPS_PROXY curl -s -m 5 -o /dev/null -w %{http_code} http://169.254.169.254/" \
   "proxy_allowed_pypi:curl -s -m 10 -o /dev/null -w %{http_code} https://pypi.org/simple/" \
   "proxy_unlisted_example:curl -s -m 10 -o /dev/null -w %{http_code} https://example.com/" \
   "resolver_long_label:dig +time=3 +tries=1 aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa.example.invalid +short" ; do
    name=${c%%:*}; cmd=${c#*:}
    printf '%s => ' "$name"; kubectl -n fargate-spike exec app-no-sidecar -c app -- sh -c "$cmd; echo \" rc=\$?\"" 2>&1 | tr '\n' ' '; echo
  done
}
ts() { date +%s; }
eksctl create cluster -f "$cfg" || exit 1
kubectl create namespace kata-spike; kubectl create namespace fargate-spike
{
echo "== node"; kubectl get nodes -L node.kubernetes.io/instance-type,ade/runtime 2>&1; kubectl get node -l ade/runtime=kata -o jsonpath='{.items[0].status.nodeInfo.kernelVersion}{"\n"}'
echo "== kata-deploy"
helm install kata-deploy oci://ghcr.io/kata-containers/kata-deploy-charts/kata-deploy --namespace kube-system --set 'nodeSelector.ade/runtime=kata' --wait --timeout 15m 2>&1 | tail -5
kubectl -n kube-system get ds,pods -l name=kata-deploy 2>&1 | tail -5; kubectl get runtimeclass 2>&1
for rc in kata-qemu kata-clh; do
  echo "== K1/K2 plain pod $rc"; s=$(ts)
  kubectl -n kata-spike run p-$rc --image=public.ecr.aws/docker/library/alpine:3.20 --restart=Never --overrides="{\"spec\":{\"runtimeClassName\":\"$rc\"}}" --command -- sh -c 'uname -r; nproc; grep MemTotal /proc/meminfo; sleep 600' 2>&1 | tail -1
  kubectl -n kata-spike wait --for=condition=Ready pod/p-$rc --timeout=300s 2>&1 | tail -1; echo "ready after $(( $(ts)-s )) s"; kubectl -n kata-spike logs p-$rc 2>&1 | head -4
  kubectl -n kata-spike describe pod p-$rc 2>&1 | grep -iE "warning|failed|error" | head -4
done
for rc in kata-clh kata-qemu; do
  echo "== E1 sidecar dns+nft under $rc"
  sed -e "s/runtimeClassName: gvisor/runtimeClassName: $rc/" -e "s/name: gvisor-dns-nft/name: sc-$rc/" -e "s/namespace: gvisor-spike/namespace: kata-spike/" -e "s/spike: gvisor/spike: kata/" "$HERE/manifests/e1a-gvisor-dns-nft.yaml" | kubectl apply -f - 2>&1 | tail -1
  if kubectl -n kata-spike wait --for=condition=Ready pod/sc-$rc --timeout=420s 2>&1 | tail -1; then sleep 15; egress_probe kata-spike sc-$rc; fi
  kubectl -n kata-spike describe pod sc-$rc 2>&1 | grep -iE "warning|failed|error" | head -4; kubectl -n kata-spike logs sc-$rc -c egress 2>&1 | tail -6
done
echo "== S-5 docker-in-Kata (kata-clh)"
kubectl apply -f - <<YAML 2>&1 | tail -1
apiVersion: v1
kind: Pod
metadata: { name: dind, namespace: kata-spike }
spec:
  runtimeClassName: kata-clh
  restartPolicy: Never
  containers:
  - name: dind
    image: docker:dind
    securityContext: { privileged: true }
    env: [{ name: DOCKER_TLS_CERTDIR, value: "" }]
YAML
kubectl -n kata-spike wait --for=condition=Ready pod/dind --timeout=420s 2>&1 | tail -1; sleep 20
for i in $(seq 1 30); do kubectl -n kata-spike exec dind -- docker info >/dev/null 2>&1 && break; sleep 3; done
kubectl -n kata-spike exec dind -- docker run --rm public.ecr.aws/docker/library/alpine:3.20 echo inner-ok 2>&1 | tail -3
kubectl -n kata-spike exec dind -- uname -r 2>&1 | tail -1
echo "== F5 (Fargate without sidecar)"
kubectl apply -f "$HERE/manifests/f5-egress-proxy.yaml" 2>&1 | tail -3
kubectl -n egress-proxy rollout status deploy/squid --timeout=300s 2>&1 | tail -1
kubectl apply -f "$HERE/manifests/f5-app-no-sidecar.yaml" 2>&1 | tail -1
kubectl -n fargate-spike wait --for=condition=Ready pod/app-no-sidecar --timeout=420s 2>&1 | tail -1; sleep 15
echo "== stage 1 (no security-group restriction)"; probe "baseline"
echo "== stage 2 (restrictive security group)"
VPC=$(aws eks describe-cluster --name $CL --region "$AWS_REGION" --query cluster.resourcesVpcConfig.vpcId --output text)
CIDR=$(aws ec2 describe-vpcs --vpc-ids "$VPC" --region "$AWS_REGION" --query 'Vpcs[0].CidrBlock' --output text)
SG=$(aws ec2 create-security-group --group-name ade-spike-sandbox --description "ade spike sandbox egress" --vpc-id "$VPC" --region "$AWS_REGION" --query GroupId --output text)
aws ec2 revoke-security-group-egress --group-id "$SG" --region "$AWS_REGION" --ip-permissions 'IpProtocol=-1,IpRanges=[{CidrIp=0.0.0.0/0}]' >/dev/null
for port in 3128 443; do aws ec2 authorize-security-group-egress --group-id "$SG" --region "$AWS_REGION" --ip-permissions "IpProtocol=tcp,FromPort=$port,ToPort=$port,IpRanges=[{CidrIp=$CIDR}]" >/dev/null; done
aws ec2 authorize-security-group-egress --group-id "$SG" --region "$AWS_REGION" --ip-permissions "IpProtocol=udp,FromPort=53,ToPort=53,IpRanges=[{CidrIp=$CIDR}]" >/dev/null
cat <<YAML | kubectl apply -f - 2>&1 | tail -2
apiVersion: vpcresources.k8s.aws/v1beta1
kind: SecurityGroupPolicy
metadata: { name: sandbox-sg, namespace: fargate-spike }
spec:
  podSelector: { matchLabels: { role: sandbox } }
  securityGroups: { groupIds: ["$SG"] }
YAML
kubectl -n fargate-spike delete pod app-no-sidecar --wait=true >/dev/null 2>&1
kubectl apply -f "$HERE/manifests/f5-app-no-sidecar.yaml" 2>&1 | tail -1
kubectl -n fargate-spike wait --for=condition=Ready pod/app-no-sidecar --timeout=420s 2>&1 | tail -1; sleep 15
probe "restricted"
kubectl -n fargate-spike describe pod app-no-sidecar 2>&1 | grep -iE "warning|failed|securitygroup|error" | head -6
} 2>&1 | tee "$OUT"
echo "results: $OUT"
