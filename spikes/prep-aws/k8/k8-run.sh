#!/usr/bin/env bash
# K8-K11 + nested-node checks on cluster ade-spike-k8 (run after eksctl create). Log: stdout.
export AWS_PROFILE=${AWS_PROFILE:?set AWS_PROFILE} AWS_REGION=${AWS_REGION:-us-east-1} AWS_PAGER=""
HERE="$(cd "$(dirname "$0")" && pwd)"; PA="$HERE/.."; PY=${PY:-/tmp/spikes/venv-rc2/bin/python}; REPO=${OPENSANDBOX_REPO:?set OPENSANDBOX_REPO}; NS=kata-spike
IMG=public.ecr.aws/docker/library/python:3.12-slim; ALP=public.ecr.aws/docker/library/alpine:3.20
sec(){ echo; echo "== $*"; }
runpod(){ # name runtime node-selector-json-or-empty image cmd...
  local n=$1 rc=$2 sel=$3 img=$4; shift 4
  local rcj=""; [ -n "$rc" ] && rcj="\"runtimeClassName\":\"$rc\""
  local ov="{\"spec\":{$rcj${rcj:+${sel:+,}}${sel:+\"nodeSelector\":$sel}}}"
  kubectl -n $NS run $n --image=$img --restart=Never --overrides="$ov" --command -- "$@" >/dev/null 2>&1
}
waitdone(){ for i in $(seq 1 100); do ph=$(kubectl -n $NS get pod $1 -o jsonpath='{.status.phase}' 2>/dev/null); [ "$ph" = Succeeded -o "$ph" = Failed ] && return; sleep 3; done; }
CLUSTER=${CLUSTER:-ade-spike-k9}
sec tag; "$PA/tag-resources.sh" $CLUSTER 2>&1 | tail -3 | cut -c1-160
sec nodes; kubectl get nodes -L node.kubernetes.io/instance-type,ade/node 2>&1 | cut -c1-150
sec kata-deploy
helm install kata-deploy oci://ghcr.io/kata-containers/kata-deploy-charts/kata-deploy --namespace kube-system --set 'nodeSelector.ade/runtime=kata' --wait --timeout 15m 2>&1 | tail -2
sleep 90; kubectl get runtimeclass kata-qemu kata-clh 2>&1 | cut -c1-80
kubectl create namespace $NS >/dev/null 2>&1; kubectl create namespace opensandbox >/dev/null 2>&1
for nd in metal nested; do N=$(kubectl get node -l ade/node=$nd -o name | head -1 | cut -d/ -f2); kubectl debug node/$N -q --image=$ALP --profile=sysadmin -- sleep 14400 >/dev/null 2>&1; done; sleep 12
MN=$(kubectl get node -l ade/node=metal -o name | head -1 | cut -d/ -f2); NN=$(kubectl get node -l ade/node=nested -o name | head -1 | cut -d/ -f2)
DM=$(kubectl get pods -o name | grep "node-debugger-$MN" | head -1 | cut -d/ -f2); DN=$(kubectl get pods -o name | grep "node-debugger-$NN" | head -1 | cut -d/ -f2)
for D in $DM $DN; do echo "debug pod $D on $(kubectl get pod $D -o jsonpath='{.spec.nodeName}')"; done
diag(){ # label debug-pod: node-side evidence for kills
  echo "-- diag $1"; kubectl exec $2 -- nsenter -t 1 -m -p -- sh -c 'echo "mem: $(grep -E "MemAvailable" /proc/meminfo)"; dmesg 2>/dev/null | grep -iE "out of memory|oom-kill|killed process" | tail -4 | cut -c1-200; journalctl -u containerd -u kubelet --since "-6min" --no-pager 2>/dev/null | grep -aiE "kata|shim" | grep -aiE "error|warn|killed|exit" | tail -6 | cut -c1-240' 2>&1 | head -14
}
sec ecr-for-K9
REG="$(aws sts get-caller-identity --query Account --output text).dkr.ecr.$AWS_REGION.amazonaws.com"
aws ecr create-repository --repository-name opensandbox-snapshots --tags Key=${ADE_TAG_KEY:?},Value=${ADE_TAG_VALUE:?} Key=Owner,Value=${ADE_OWNER:?} Key=Project,Value=ade-spike Key=Environment,Value=demo Key=Expires,Value=${ADE_EXPIRES:?} --query repository.repositoryName --output text 2>&1 | tail -1
kubectl -n opensandbox create secret docker-registry snap-push --docker-server="$REG" --docker-username=AWS --docker-password="$(aws ecr get-login-password)" 2>&1 | tail -1
cat > /tmp/snap-values.yaml <<Y
opensandbox-controller:
  controller:
    snapshot:
      registry: "$REG"
      snapshotPushSecret: snap-push
      resumePullSecret: snap-push
      imageURITemplate: "{{.Registry}}/opensandbox-snapshots:{{.SnapshotUID}}-{{.ContainerName}}-{{.ArtifactKind}}"
Y
sec opensandbox-install
( cd "$REPO/manifests/charts" && helm dependency build opensandbox 2>&1 | tail -1 && helm install opensandbox opensandbox --namespace opensandbox-system --create-namespace -f "$HERE/values-k8.yaml" -f /tmp/snap-values.yaml --timeout 8m 2>&1 | tail -2 )
kubectl -n opensandbox-system rollout status deploy/opensandbox-controller-manager --timeout=300s 2>&1 | tail -1
kubectl -n opensandbox-system rollout status deploy/opensandbox-server --timeout=300s 2>&1 | tail -1
kubectl -n opensandbox-system get pods 2>&1 | cut -c1-110
sec K8-K9
kubectl -n opensandbox-system port-forward svc/opensandbox-server 18080:80 >/tmp/pf.log 2>&1 &
PF=$!; sleep 6
echo "-- raw create probe (status, headers, body)"
curl -s -i -X POST http://127.0.0.1:18080/v1/sandboxes -H 'Content-Type: application/json' -d "{\"image\":{\"uri\":\"$IMG\"},\"entrypoint\":[\"tail\",\"-f\",\"/dev/null\"],\"timeout\":600,\"resourceLimits\":{\"cpu\":\"500m\",\"memory\":\"512Mi\"}}" 2>&1 | head -25 | cut -c1-300
echo "-- openapi paths"; curl -s http://127.0.0.1:18080/openapi.json | python3 -c "import sys,json;print(sorted(json.load(sys.stdin).get('paths',{}))[:12])" 2>&1 | cut -c1-300
kubectl -n opensandbox-system logs deploy/opensandbox-server --tail=40 2>&1 | grep -vE "GET /health" | tail -12 | cut -c1-260
"$PY" "$HERE/k8.py" "$IMG" 2>&1 | tail -70
kubectl -n opensandbox get pods -o custom-columns=NAME:.metadata.name,RC:.spec.runtimeClassName,NODE:.spec.nodeName 2>&1 | head -5
kill $PF 2>/dev/null
sec soak-90s
runpod soak-clh kata-clh '{"ade/node":"metal"}' $ALP sleep 3600; runpod soak-qemu kata-qemu '{"ade/node":"metal"}' $ALP sleep 3600
kubectl -n $NS wait --for=condition=Ready pod/soak-clh pod/soak-qemu --timeout=180s 2>&1 | tail -1; sleep 90
kubectl -n $NS get pods soak-clh soak-qemu --no-headers | cut -c1-80; diag metal-after-soak $DM
kubectl -n $NS delete pod soak-clh soak-qemu --wait=true >/dev/null 2>&1
sec K10-guest-oom
kubectl -n $NS apply -f - >/dev/null 2>&1 <<Y
apiVersion: v1
kind: Pod
metadata: { name: oom, namespace: $NS }
spec:
  runtimeClassName: kata-clh
  nodeSelector: { ade/node: metal }
  restartPolicy: Never
  containers:
  - name: c
    image: $IMG
    command: ["python3","-c","b=bytearray(1500*1024*1024); print('allocated', len(b))"]
    resources: { limits: { memory: 512Mi }, requests: { memory: 512Mi } }
Y
waitdone oom; kubectl -n $NS get pod oom -o jsonpath='{.status.phase} {.status.containerStatuses[0].state}{"\n"}' | cut -c1-260; kubectl -n $NS logs oom 2>&1 | tail -2
sec K10-hypervisor-kill
runpod victim kata-clh '{"ade/node":"metal"}' $ALP sleep 3600; runpod bystander-clh kata-clh '{"ade/node":"metal"}' $ALP sleep 3600; runpod bystander-runc "" '{"ade/node":"metal"}' $ALP sleep 3600
kubectl -n $NS wait --for=condition=Ready pod/victim pod/bystander-clh pod/bystander-runc --timeout=180s 2>&1 | tail -1
D=$DM; N=$(kubectl get pod $D -o jsonpath='{.spec.nodeName}')
echo "killing cloud-hypervisor processes on $N (all kata-clh VMs there)"; kubectl exec $D -- nsenter -t 1 -m -p -- sh -c 'pgrep -af "[c]loud-hypervisor|[q]emu-system" | cut -c1-80; pkill -9 -f "[c]loud-hypervisor"; sleep 1; echo remaining: $(pgrep -f "[c]loud-hypervisor" | wc -l)' 2>&1 | tr '\n' ' '; echo
sleep 20; kubectl -n $NS get pods victim bystander-clh bystander-runc --no-headers 2>&1 | cut -c1-90
kubectl -n $NS get pod victim -o jsonpath='{.status.containerStatuses[0].state}{"\n"}' | cut -c1-200
kubectl -n $NS delete pod victim bystander-clh bystander-runc oom --wait=false >/dev/null 2>&1
sec K11-volumes
kubectl -n $NS create configmap cm1 --from-literal=a=1 >/dev/null 2>&1; kubectl -n $NS create secret generic sec1 --from-literal=a=1 >/dev/null 2>&1
for rc in runc kata-clh; do
  RCL=""; [ $rc != runc ] && RCL="runtimeClassName: $rc"
  kubectl -n $NS apply -f - >/dev/null 2>&1 <<Y
apiVersion: v1
kind: Pod
metadata: { name: vol-$rc, namespace: $NS }
spec:
  $RCL
  nodeSelector: { ade/node: metal }
  restartPolicy: Never
  securityContext: { runAsUser: 1000, runAsGroup: 1000, fsGroup: 1000 }
  containers:
  - name: c
    image: $IMG
    command: ["python3","-c","import os,time\nfor p in ['/','/ed','/mem','/hp','/cm','/sec']:\n  d=(p.rstrip('/')+'/t') if p!='/' else '/tmp/t'\n  try:\n    os.makedirs(d,exist_ok=True); t=time.perf_counter()\n    for i in range(2000):\n      open(d+'/f%d'%i,'w').write(str(i))\n    print(p,'write2000_s=%.2f'%(time.perf_counter()-t),'mode',oct(os.stat(os.path.dirname(d) if p!='/' else '/tmp').st_mode)[-4:],'uid',os.stat(os.path.dirname(d) if p!='/' else '/tmp').st_uid,'gid',os.stat(os.path.dirname(d) if p!='/' else '/tmp').st_gid)\n  except Exception as e: print(p,'ERR',type(e).__name__,e)"]
    volumeMounts:
    - { name: ed, mountPath: /ed }
    - { name: mem, mountPath: /mem }
    - { name: hp, mountPath: /hp }
    - { name: cm, mountPath: /cm }
    - { name: sec, mountPath: /sec }
  volumes:
  - { name: ed, emptyDir: {} }
  - { name: mem, emptyDir: { medium: Memory, sizeLimit: 512Mi } }
  - { name: hp, hostPath: { path: /tmp/ade-hp, type: DirectoryOrCreate } }
  - { name: cm, configMap: { name: cm1 } }
  - { name: sec, secret: { secretName: sec1 } }
Y
  waitdone vol-$rc; echo "-- $rc"; kubectl -n $NS logs vol-$rc 2>&1 | cut -c1-160
done
kubectl -n $NS delete pod vol-runc vol-kata-clh --wait=false >/dev/null 2>&1
sec nested-node
node_mem(){ kubectl exec $DN -- nsenter -t 1 -m -p -- sh -c 'grep -E "MemTotal|MemAvailable" /proc/meminfo' | tr '\n' ' '; echo; }
echo "nested node memory: $(node_mem)"
RES=',"containers":[{"name":"c","resources":{"requests":{"memory":"256Mi"}}}]'
for rc in kata-clh kata-qemu; do
  s=$(date +%s); runpod nest-$rc $rc '{"ade/node":"nested"}' $ALP sh -c 'uname -r; nproc; grep MemTotal /proc/meminfo; sleep 3600'
  kubectl -n $NS wait --for=condition=Ready pod/nest-$rc --timeout=300s >/dev/null 2>&1; echo "$rc nested ready after $(( $(date +%s)-s )) s: $(sleep 5; kubectl -n $NS logs nest-$rc 2>&1 | tr '\n' ' ')"
  sleep 45; echo "after 45 s: $(kubectl -n $NS get pod nest-$rc --no-headers | cut -c1-60) | $(node_mem)"
  kubectl -n $NS delete pod nest-$rc --wait=true >/dev/null 2>&1
done
runpod k5n kata-clh '{"ade/node":"nested"}' $IMG python3 -c 'import time
t=time.perf_counter()
for i in range(2000): open("/tmp/f%d"%i,"w").write(str(i))
print("nested write2000_s=%.2f"%(time.perf_counter()-t))'
waitdone k5n; echo "k5n: $(kubectl -n $NS get pod k5n --no-headers | cut -c1-60) $(kubectl -n $NS logs k5n 2>&1 | tail -1)"; kubectl -n $NS delete pod k5n --wait=true >/dev/null 2>&1
kubectl -n $NS apply -f <(sed -e "s/runtimeClassName: gvisor/runtimeClassName: kata-clh/" -e "s/name: gvisor-dns-nft/name: sc-nested/" -e "s/namespace: gvisor-spike/namespace: $NS/" -e "s/spike: gvisor/spike: kata/" -e "s/^spec:/spec:\n  nodeSelector: { ade\/node: nested }/" "$PA/manifests/e1a-gvisor-dns-nft.yaml") 2>&1 | tail -1
kubectl -n $NS wait --for=condition=Ready pod/sc-nested --timeout=300s 2>&1 | tail -1; sleep 20
. "$PA/common.sh"; egress_probe $NS sc-nested; kubectl -n $NS get pod sc-nested --no-headers | cut -c1-70; diag nested-after-sidecar $DN
kubectl -n $NS delete pod sc-nested --wait=true >/dev/null 2>&1
sec K9-note
echo "(K9 ran inside k8.py: create_snapshot, pause, resume against the ECR repository)"; kubectl -n opensandbox get sandboxsnapshots 2>&1 | head -5 | cut -c1-140
kubectl -n opensandbox-system logs deploy/opensandbox-controller-manager --tail=200 2>&1 | grep -iE "snapshot|commit|pause|error" | tail -10 | cut -c1-240
aws ecr list-images --repository-name opensandbox-snapshots --query 'imageIds[].imageTag' --output text 2>&1 | head -3 | cut -c1-200
if [ "${K12:-0}" = 1 ]; then
sec K12-kata-fc
echo "K12 runs on the nested node (KVM inside the VM); the metal node keeps the default shims"
kubectl exec $DN -- nsenter -t 1 -m -p -- sh -c 'set -e; mkdir -p /var/lib/containerd/devmapper; cd /var/lib/containerd/devmapper; [ -f data ] || { truncate -s 30G data; truncate -s 2G meta; }; D=$(losetup --find --show data); M=$(losetup --find --show meta); S=$(blockdev --getsize64 $D); dmsetup create kata-thinpool --table "0 $((S/512)) thin-pool $M $D 128 32768 1 skip_block_zeroing"; dmsetup status kata-thinpool; grep -nE "^imports|SystemdCgroup" /etc/containerd/config.toml | head -3' 2>&1 | tail -4
cat > /tmp/fc-dropin.toml <<'Y'
[plugins."io.containerd.snapshotter.v1.devmapper"]
  root_path = "/var/lib/containerd/devmapper"
  pool_name = "kata-thinpool"
  base_image_size = "10GB"
Y
rm -rf /tmp/kd; helm pull oci://ghcr.io/kata-containers/kata-deploy-charts/kata-deploy --untar --untardir /tmp/kd >/dev/null 2>&1
helm upgrade kata-deploy /tmp/kd/kata-deploy -n kube-system -f /tmp/kd/kata-deploy/try-kata-fc.values.yaml --set 'nodeSelector.ade/node=nested' --set-file containerd.userDropIn=/tmp/fc-dropin.toml --wait --timeout 6m 2>&1 | tail -2
echo "-- install pods"; kubectl -n kube-system get pods --no-headers 2>&1 | grep -E "kata-deploy" | cut -c1-110
for P in $(kubectl -n kube-system get pods --no-headers 2>/dev/null | awk '/kata-deploy-install/ {print $1}'); do echo "-- describe $P"; kubectl -n kube-system describe pod $P 2>&1 | grep -E "Node:|State:|Reason:|Warning|Events|Back-off|Failed" | head -8 | cut -c1-200; echo "-- logs cri"; kubectl -n kube-system logs $P -c cri --tail=15 2>&1 | cut -c1-220; done
echo "-- node containerd journal (nested)"; kubectl exec $DN -- nsenter -t 1 -m -p -- sh -c 'journalctl -u containerd --since "-12min" --no-pager 2>/dev/null | grep -aiE "devmapper|error|fail|panic|plugin" | tail -10 | cut -c1-240; ctr plugins ls 2>/dev/null | grep -i devmapper'
kubectl get runtimeclass kata-fc 2>&1 | cut -c1-80; kubectl get node $NN --show-labels 2>&1 | grep -o "katacontainers[^,]*" | head -3
runpod fc kata-fc '{"ade/node":"nested"}' $ALP sh -c 'uname -r; sleep 600'; kubectl -n $NS wait --for=condition=Ready pod/fc --timeout=240s 2>&1 | tail -1; sleep 10; kubectl -n $NS logs fc 2>&1 | head -2; kubectl -n $NS get pod fc --no-headers | cut -c1-70
kubectl -n $NS describe pod fc 2>&1 | grep -E "Warning|Error|Failed" | head -4 | cut -c1-240
fi
sec ecr-cleanup
aws ecr delete-repository --repository-name opensandbox-snapshots --force --query repository.repositoryName --output text 2>&1 | tail -1
sec done
