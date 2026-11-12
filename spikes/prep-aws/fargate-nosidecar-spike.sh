#!/usr/bin/env bash
# Spike F5 (UNTESTED sketch). Can network-level controls replace the egress sidecar where NET_ADMIN is unavailable?
# Stage 1: pod without sidecar, proxy reachable, direct egress still open (baseline: shows enforcement is absent).
# Stage 2: attach a restrictive security group (egress only to the VPC CIDR on 3128 and 53) and retest.
# Known snags: Fargate pulls images over the pod's own ENI (stage 2 needs ECR endpoints in the VPC and a mirrored image);
# the VPC resolver stays reachable, so DNS tunnelling needs Route 53 Resolver DNS Firewall; security groups for Fargate pods
# need the SecurityGroupPolicy CRD (verify support in your cluster version).
set -u; . "$(dirname "$0")/common.sh"; guard_aws
CL=ade-spike-fargate; OUT="$RES/fargate-nosidecar-$TS.txt"; mkdir -p "$RES"
cfg=$(mktemp); render_eksctl "$HERE/eksctl/fargate.eksctl.yaml" "$cfg"
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
eksctl create cluster -f "$cfg" || exit 1
kubectl create namespace fargate-spike
{
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
