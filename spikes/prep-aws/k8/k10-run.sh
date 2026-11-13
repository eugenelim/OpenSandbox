#!/usr/bin/env bash
# One pass over the remaining Kata-on-EKS questions on a cluster with node groups: metal, nested (c8i.xlarge), nested-l (c8i.4xlarge), plus a Fargate profile.
# Egress on the cluster security group is revoked by organization automation about 24 minutes after the cluster is ready, so every image is pre-pulled early
# and public images go through the ECR pull-through cache. Log: stdout. Needs ADE_TAG_* and OPENSANDBOX_REPO.
export AWS_PROFILE=${AWS_PROFILE:?set AWS_PROFILE} AWS_REGION=${AWS_REGION:-us-east-1} AWS_PAGER=""
HERE="$(cd "$(dirname "$0")" && pwd)"; PA="$HERE/.."; PY=${PY:-/tmp/spikes/venv-rc2/bin/python}; REPO=${OPENSANDBOX_REPO:?set OPENSANDBOX_REPO}
CLUSTER=${CLUSTER:-ade-spike-k10}; NS=kata-spike
ACCT=$(aws sts get-caller-identity --query Account --output text); REG="$ACCT.dkr.ecr.$AWS_REGION.amazonaws.com"
ALP=$REG/ecr-public/docker/library/alpine:3.20; BB=$REG/ecr-public/docker/library/busybox:1.37; PYI=$REG/ecr-public/docker/library/python:3.12-slim
OSV=release-1.2.1-rc.2
sec(){ echo; echo "== $(date -u +%H:%M:%S) $*"; }
runpod(){ local n=$1 rc=$2 sel=$3 img=$4; shift 4
  local rcj=""; [ -n "$rc" ] && rcj="\"runtimeClassName\":\"$rc\""
  local ov="{\"spec\":{$rcj${rcj:+${sel:+,}}${sel:+\"nodeSelector\":$sel}}}"
  kubectl -n $NS run $n --image=$img --restart=Never --overrides="$ov" --command -- "$@" >/dev/null 2>&1; }
waitdone(){ for i in $(seq 1 100); do ph=$(kubectl -n $NS get pod $1 -o jsonpath='{.status.phase}' 2>/dev/null); [ "$ph" = Succeeded -o "$ph" = Failed ] && return; sleep 3; done; }
dbg(){ kubectl get pods -o name | grep "node-debugger-$1" | head -1 | cut -d/ -f2; }
nsx(){ local D=$1; shift; kubectl exec $D -- nsenter -t 1 -m -n -p -- sh -c "$*" 2>&1; }
memavail(){ nsx $1 "grep MemAvailable /proc/meminfo" | awk '{print int($2/1024)}'; }
diag(){ echo "-- diag $1"; nsx $2 'echo "mem: $(grep MemAvailable /proc/meminfo)"; dmesg 2>/dev/null | grep -iE "out of memory|oom-kill|killed process" | tail -4 | cut -c1-200; journalctl -u containerd -u kubelet --since "-5min" --no-pager 2>/dev/null | grep -aiE "kata|shim" | grep -aiE "error|killed|exit" | grep -aviE "Block device|cleanup|dead shim|cgroup" | tail -5 | cut -c1-240' | head -12; }
prepull(){ local D=$1; shift; for im in "$@"; do r=$(nsx $D "case '$im' in *.dkr.ecr.*) C=\"--user AWS:\$(aws ecr get-login-password --region $AWS_REGION)\"; REF='$im';; */*.*/*|*.*/*) C=; REF='$im';; *) C=; REF=docker.io/'$im';; esac; ctr -n k8s.io images pull --platform linux/amd64 \$C \$REF >/dev/null 2>&1 && echo ok || echo FAIL" | tail -1); echo "  $r $(echo $im | cut -c1-90)"; done; }

sec egress-endpoints-and-warm
"$PA/egress-endpoints.sh" up $CLUSTER 2>&1 | tail -6
for im in "docker/library/alpine 3.20" "docker/library/busybox 1.37" "docker/library/python 3.12-slim"; do "$PA/egress-endpoints.sh" warm $im; done
VPC=$(aws ec2 describe-vpcs --filters "Name=tag:alpha.eksctl.io/cluster-name,Values=$CLUSTER" --query 'Vpcs[0].VpcId' --output text)
sec remediation-probe-sg
PSG=$(aws ec2 create-security-group --group-name ade-spike-remediation-probe-$(date +%s) --description "probe: which egress rules does auto-remediation revoke" --vpc-id $VPC --tag-specifications "ResourceType=security-group,Tags=[{Key=${ADE_TAG_KEY:?},Value=${ADE_TAG_VALUE:?}},{Key=Owner,Value=${ADE_OWNER:?}},{Key=Project,Value=ade-spike},{Key=Expires,Value=${ADE_EXPIRES:?}}]" --query GroupId --output text)
aws ec2 revoke-security-group-egress --group-id $PSG --ip-permissions 'IpProtocol=-1,IpRanges=[{CidrIp=0.0.0.0/0}]' >/dev/null
for P in "IpProtocol=tcp,FromPort=443,ToPort=443,IpRanges=[{CidrIp=0.0.0.0/0,Description=probe-https-open}]" "IpProtocol=tcp,FromPort=80,ToPort=80,IpRanges=[{CidrIp=10.0.0.0/8,Description=probe-private-cidr}]" "IpProtocol=-1,IpRanges=[{CidrIp=0.0.0.0/0,Description=probe-all-open}]" "IpProtocol=udp,FromPort=53,ToPort=53,IpRanges=[{CidrIp=1.1.1.1/32,Description=probe-dns-one-host}]"; do aws ec2 authorize-security-group-egress --group-id $PSG --ip-permissions "$P" --query Return --output text; done
echo "probe sg $PSG created $(date -u +%H:%M:%S)Z with 4 egress rules: https-open, private-cidr, all-open, one-host-dns"; echo $PSG > /tmp/probe-sg.txt
sec nodes
kubectl get nodes -L node.kubernetes.io/instance-type,ade/node 2>&1 | cut -c1-150
kubectl create namespace $NS >/dev/null 2>&1; kubectl create namespace opensandbox >/dev/null 2>&1; kubectl create namespace fargate-spike >/dev/null 2>&1
for nd in metal nested nested-l; do N=$(kubectl get node -l ade/node=$nd -o name | head -1 | cut -d/ -f2); eval "N_${nd//-/_}=$N"; kubectl debug node/$N -q --image=public.ecr.aws/docker/library/alpine:3.20 --profile=sysadmin -- sleep 28800 >/dev/null 2>&1; done; sleep 14
DM=$(dbg $N_metal); DS=$(dbg $N_nested); DL=$(dbg $N_nested_l); echo "debug pods: metal=$DM nested=$DS nested-l=$DL"

sec kata-deploy-install-keep-layers
cat > /tmp/keep-layers.toml <<'Y'
[plugins."io.containerd.cri.v1.images"]
  discard_unpacked_layers = false
Y
helm install kata-deploy oci://ghcr.io/kata-containers/kata-deploy-charts/kata-deploy --namespace kube-system --set 'nodeSelector.ade/runtime=kata' --set-file containerd.userDropIn=/tmp/keep-layers.toml --wait --timeout 12m 2>&1 | tail -2
sleep 45; kubectl get runtimeclass kata-qemu kata-clh 2>&1 | cut -c1-60
sec repro-first-cluster-conditions
echo "first cluster: plain Kata pods created right after install on metal, with a Fargate profile present; watch for exit 137 within seconds"
for rc in kata-qemu kata-clh; do runpod rp-$rc $rc '{"ade/node":"metal"}' $BB sh -c 'echo up; sleep 3600'; done
sleep 100; kubectl -n $NS get pods rp-kata-qemu rp-kata-clh --no-headers | cut -c1-70; kubectl -n $NS delete pod rp-kata-qemu rp-kata-clh --wait=false >/dev/null 2>&1

sec prepull-images-on-all-kata-nodes
IMGS_PUB="ghcr.io/kata-containers/k8s-job-dispatcher:0.3.0 quay.io/kata-containers/kata-deploy:4.3.1 quay.io/kata-containers/kubectl:latest opensandbox/server:$OSV opensandbox/controller:$OSV opensandbox/execd:$OSV opensandbox/egress:$OSV opensandbox/image-committer:$OSV"
for D in $DM $DS $DL; do echo "-- $D"; prepull $D $IMGS_PUB $ALP $BB $PYI; done

sec opensandbox-install
cat > /tmp/os-values.yaml <<Y
opensandbox-controller:
  controller:
    image: { repository: opensandbox/controller, tag: $OSV }
    snapshot:
      imageCommitterImage: "opensandbox/image-committer:$OSV"
      registry: "$REG"
      snapshotPushSecret: snap-push
      resumePullSecret: snap-push
      imageURITemplate: "{{.Registry}}/opensandbox-snapshots:{{.SnapshotUID}}-{{.ContainerName}}-{{.ArtifactKind}}"
opensandbox-server:
  server:
    image: { repository: opensandbox/server, tag: $OSV }
    env:
    - { name: OPENSANDBOX_INSECURE_SERVER, value: "YES" }
  configToml: |
    [server]
    host = "0.0.0.0"
    port = 80
    api_key = ""
    [log]
    level = "INFO"
    [runtime]
    type = "kubernetes"
    execd_image = "opensandbox/execd:$OSV"
    [kubernetes]
    kubeconfig_path = ""
    namespace = "opensandbox"
    informer_resync_seconds = 300
    informer_watch_timeout_seconds = 60
    workload_provider = "batchsandbox"
    batchsandbox_template_file = "/etc/opensandbox/example.batchsandbox-template.yaml"
    [secure_runtime]
    type = "${SECURE_TYPE:-kata}"
    k8s_runtime_class = "${SECURE_RC:-kata-clh}"
    [egress]
    image = "opensandbox/egress:$OSV"
    mode = "dns+nft"
Y
aws ecr create-repository --repository-name opensandbox-snapshots --tags Key=$ADE_TAG_KEY,Value=$ADE_TAG_VALUE Key=Owner,Value=$ADE_OWNER Key=Project,Value=ade-spike Key=Expires,Value=$ADE_EXPIRES --query repository.repositoryName --output text 2>&1 | tail -1
rotate_secret(){ kubectl -n opensandbox delete secret snap-push >/dev/null 2>&1; kubectl -n opensandbox create secret docker-registry snap-push --docker-server="$REG" --docker-username=AWS --docker-password="$(aws ecr get-login-password)" 2>&1 | tail -1; }
rotate_secret
( cd "$REPO/manifests/charts" && helm dependency build opensandbox 2>&1 | tail -1 && helm install opensandbox opensandbox --namespace opensandbox-system --create-namespace -f /tmp/os-values.yaml --timeout 8m 2>&1 | tail -1 )
kubectl -n opensandbox-system rollout status deploy/opensandbox-controller-manager --timeout=300s 2>&1 | tail -1
kubectl -n opensandbox-system rollout status deploy/opensandbox-server --timeout=300s 2>&1 | tail -1

sec nested-concurrency-6-vms-on-8GiB
B=$(memavail $DS); echo "nested node MemAvailable before: $B MiB"
for i in 1 2; do runpod cc-clh$i kata-clh '{"ade/node":"nested"}' $BB sh -c 'sleep 3600'; runpod cc-qemu$i kata-qemu '{"ade/node":"nested"}' $BB sh -c 'sleep 3600'; done
runpod cc-py kata-clh '{"ade/node":"nested"}' $PYI python3 -c 'import time
t=time.perf_counter()
for i in range(2000): open("/tmp/f%d"%i,"w").write(str(i))
print("write2000_s=%.2f"%(time.perf_counter()-t)); time.sleep(3600)'
kubectl -n $NS apply -f <(sed -e "s/runtimeClassName: gvisor/runtimeClassName: kata-clh/" -e "s/name: gvisor-dns-nft/name: cc-sidecar/" -e "s/namespace: gvisor-spike/namespace: $NS/" -e "s/spike: gvisor/spike: kata/" -e "s#public.ecr.aws/docker/library/alpine:3.20#$ALP#" -e "s#image: opensandbox/egress:v1.1.7#image: opensandbox/egress:$OSV#" -e "s/^spec:/spec:\n  nodeSelector: { ade\/node: nested }/" "$PA/manifests/e1a-gvisor-dns-nft.yaml") 2>&1 | tail -1
kubectl -n $NS wait --for=condition=Ready pod --all --timeout=240s 2>&1 | tail -1; sleep 90
kubectl -n $NS get pods --no-headers | grep -E "^cc-" | cut -c1-70; A=$(memavail $DS); echo "nested MemAvailable with 6 VMs: $A MiB (used $((B-A)) MiB)"; echo "cc-py: $(kubectl -n $NS logs cc-py 2>&1 | tail -1)"; diag nested-6vms $DS
kubectl -n $NS delete pod --all --wait=true >/dev/null 2>&1

sec density-on-c8i-4xlarge
B=$(memavail $DL); echo "nested-l MemAvailable before: $B MiB"
for N in 10 20 40; do
  s=$(date +%s); for i in $(seq 1 $N); do kubectl -n $NS get pod dn-$i >/dev/null 2>&1 || runpod dn-$i kata-clh '{"ade/node":"nested-l"}' $BB sh -c 'sleep 3600'; done
  kubectl -n $NS wait --for=condition=Ready pod -l run --timeout=600s >/dev/null 2>&1; e=$(( $(date +%s)-s )); sleep 20
  R=$(kubectl -n $NS get pods --no-headers | grep -c " Running "); A=$(memavail $DL)
  echo "N=$N: Running=$R, ready in $e s, MemAvailable $A MiB (used $((B-A)) MiB => $(( (B-A)/N )) MiB per VM)"
done
diag density $DL; kubectl -n $NS delete pod --all --wait=true >/dev/null 2>&1

sec K9-clh-with-writes-outside-rootfs-and-token-rotation
kubectl -n opensandbox-system port-forward svc/opensandbox-server ${OS_PORT:-18090}:80 >/tmp/pf.log 2>&1 & PF=$!; sleep 6
K9_IMAGE=$PYI "$PY" "$HERE/k8.py" "$PYI" 2>&1 | tail -45
echo "-- rotate the snapshot push secret (new ECR token) and repeat pause/resume"
rotate_secret; K9_IMAGE=$PYI "$PY" "$HERE/k8.py" "$PYI" 2>&1 | grep -E '"pause_poll"|final_state|"resume"|"restore_from_snapshot"|"ok": false|restore_cmd|outside' | head -12
kill $PF 2>/dev/null

sec kata-fc-all-nodes
for D in $DM $DS $DL; do nsx $D 'set -e; mkdir -p /var/lib/containerd/devmapper; cd /var/lib/containerd/devmapper; [ -f data ] || { truncate -s 30G data; truncate -s 2G meta; }; D=$(losetup --find --show data); M=$(losetup --find --show meta); S=$(blockdev --getsize64 $D); dmsetup create kata-thinpool --table "0 $((S/512)) thin-pool $M $D 128 32768 1 skip_block_zeroing"; echo thinpool-ok' | tail -1; done
cat > /tmp/fc-dropin.toml <<'Y'
[plugins."io.containerd.cri.v1.images"]
  discard_unpacked_layers = false
[plugins."io.containerd.snapshotter.v1.devmapper"]
  root_path = "/var/lib/containerd/devmapper"
  pool_name = "kata-thinpool"
  base_image_size = "10GB"
Y
rm -rf /tmp/kd; helm pull oci://ghcr.io/kata-containers/kata-deploy-charts/kata-deploy --untar --untardir /tmp/kd >/dev/null 2>&1
helm upgrade kata-deploy /tmp/kd/kata-deploy -n kube-system -f /tmp/kd/kata-deploy/try-kata-fc.values.yaml --set 'nodeSelector.ade/runtime=kata' --set "job.dispatcherImage.reference=$REG/mirror/kata-job-dispatcher" --set imagePullPolicy=IfNotPresent --set-file containerd.userDropIn=/tmp/fc-dropin.toml --wait --timeout 8m 2>&1 | tail -2
sleep 30; kubectl get runtimeclass kata-fc 2>&1 | cut -c1-60
runpod fc-plain kata-fc '{"ade/node":"nested"}' $BB sh -c 'uname -r; nproc; sleep 600'; kubectl -n $NS wait --for=condition=Ready pod/fc-plain --timeout=150s 2>&1 | tail -1; kubectl -n $NS logs fc-plain 2>&1 | head -2

sec opensandbox-with-kata-fc
sed -i 's/type = "kata"/type = "firecracker"/; s/k8s_runtime_class = "kata-clh"/k8s_runtime_class = "kata-fc"/' /tmp/os-values.yaml
( cd "$REPO/manifests/charts" && helm upgrade opensandbox opensandbox --namespace opensandbox-system -f /tmp/os-values.yaml --timeout 6m 2>&1 | tail -1 )
kubectl -n opensandbox-system rollout status deploy/opensandbox-server --timeout=240s 2>&1 | tail -1
kubectl -n opensandbox-system port-forward svc/opensandbox-server ${OS_PORT:-18090}:80 >/tmp/pf.log 2>&1 & PF=$!; sleep 6
K9_IMAGE=$PYI "$PY" "$HERE/k8.py" "$PYI" 2>&1 | tail -45
kubectl -n opensandbox get pods -o custom-columns=NAME:.metadata.name,RC:.spec.runtimeClassName,NODE:.spec.nodeName 2>&1 | head -6
kill $PF 2>/dev/null

sec remediation-probe-result
PSG=$(cat /tmp/probe-sg.txt); echo "now $(date -u +%H:%M:%S)Z"; aws ec2 describe-security-groups --group-ids $PSG --query 'SecurityGroups[0].IpPermissionsEgress[].[IpProtocol,FromPort,IpRanges[0].CidrIp,IpRanges[0].Description]' --output text 2>&1 | cut -c1-120
aws cloudtrail lookup-events --lookup-attributes AttributeKey=ResourceName,AttributeValue=$PSG --max-results 10 --query 'Events[].[EventTime,EventName,Username]' --output text 2>&1 | head -8 | cut -c1-120
sec done
