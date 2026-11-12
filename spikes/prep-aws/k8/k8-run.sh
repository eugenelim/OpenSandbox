#!/usr/bin/env bash
# K8-K11 + nested-node checks on cluster ade-spike-k8 (run after eksctl create). Log: stdout.
export AWS_PROFILE=${AWS_PROFILE:?set AWS_PROFILE} AWS_REGION=${AWS_REGION:-us-east-1} AWS_PAGER=""
REPO=${OPENSANDBOX_REPO:?set OPENSANDBOX_REPO to an OpenSandbox checkout}
HERE="$(cd "$(dirname "$0")" && pwd)"; PA="$HERE/.."; PY=/tmp/spikes/venv/bin/python; NS=kata-spike
IMG=public.ecr.aws/docker/library/python:3.12-slim; ALP=public.ecr.aws/docker/library/alpine:3.20
sec(){ echo; echo "== $*"; }
runpod(){ # name runtime node-selector-json-or-empty image cmd...
  local n=$1 rc=$2 sel=$3 img=$4; shift 4
  local ov="{\"spec\":{\"runtimeClassName\":\"$rc\"${sel:+,\"nodeSelector\":$sel}}}"
  kubectl -n $NS run $n --image=$img --restart=Never --overrides="$ov" --command -- "$@" >/dev/null 2>&1
}
waitdone(){ for i in $(seq 1 100); do ph=$(kubectl -n $NS get pod $1 -o jsonpath='{.status.phase}' 2>/dev/null); [ "$ph" = Succeeded -o "$ph" = Failed ] && return; sleep 3; done; }
sec tag; "$PA/tag-resources.sh" ade-spike-k8 2>&1 | tail -3 | cut -c1-160
sec nodes; kubectl get nodes -L node.kubernetes.io/instance-type,ade/node 2>&1 | cut -c1-150
sec kata-deploy
helm install kata-deploy oci://ghcr.io/kata-containers/kata-deploy-charts/kata-deploy --namespace kube-system --set 'nodeSelector.ade/runtime=kata' --wait --timeout 15m 2>&1 | tail -2
sleep 90; kubectl get runtimeclass kata-qemu kata-clh 2>&1 | cut -c1-80
kubectl create namespace $NS >/dev/null 2>&1; kubectl create namespace opensandbox >/dev/null 2>&1
sec opensandbox-install
( cd "$REPO/manifests/charts" && helm dependency build opensandbox 2>&1 | tail -1 && helm install opensandbox opensandbox --namespace opensandbox-system --create-namespace -f "$HERE/values-k8.yaml" --timeout 8m 2>&1 | tail -2 )
kubectl -n opensandbox-system rollout status deploy/opensandbox-controller-manager --timeout=300s 2>&1 | tail -1
kubectl -n opensandbox-system rollout status deploy/opensandbox-server --timeout=300s 2>&1 | tail -1
kubectl -n opensandbox-system get pods 2>&1 | cut -c1-110
sec K8-K9
kubectl -n opensandbox-system port-forward svc/opensandbox-server 18080:80 >/tmp/pf.log 2>&1 &
PF=$!; sleep 6
"$PY" "$HERE/k8.py" "$IMG" 2>&1 | tail -70
kubectl -n opensandbox get pods -o custom-columns=NAME:.metadata.name,RC:.spec.runtimeClassName,NODE:.spec.nodeName 2>&1 | head -5
kill $PF 2>/dev/null
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
N=$(kubectl -n $NS get pod victim -o jsonpath='{.spec.nodeName}'); kubectl debug node/$N -q --image=$ALP --profile=sysadmin -- sleep 600 >/dev/null 2>&1; sleep 8; D=$(kubectl get pods -o name | grep node-debugger | head -1 | cut -d/ -f2)
echo "killing cloud-hypervisor processes on $N (all kata-clh VMs there)"; kubectl exec $D -- chroot /host sh -c 'pgrep -a cloud-hypervisor | wc -l; pkill -9 cloud-hypervisor; sleep 1; pgrep -a cloud-hypervisor | wc -l' 2>&1 | tr '\n' ' '; echo
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
for rc in kata-clh kata-qemu; do
  s=$(date +%s); runpod nest-$rc $rc '{"ade/node":"nested"}' $ALP sh -c 'uname -r; nproc; grep MemTotal /proc/meminfo; sleep 3600'
  kubectl -n $NS wait --for=condition=Ready pod/nest-$rc --timeout=300s >/dev/null 2>&1; echo "$rc nested ready after $(( $(date +%s)-s )) s: $(sleep 5; kubectl -n $NS logs nest-$rc 2>&1 | tr '\n' ' ')"
done
for rc in kata-clh; do
  runpod k5n $rc '{"ade/node":"nested"}' $IMG sh -c 'cd /tmp && python3 - <<P
import time,os
t=time.perf_counter()
for i in range(2000): open("/tmp/f%d"%i,"w").write(str(i))
print("nested write2000_s=%.2f"%(time.perf_counter()-t))
P'
  waitdone k5n; kubectl -n $NS logs k5n 2>&1 | tail -1
done
kubectl -n $NS apply -f <(sed -e "s/runtimeClassName: gvisor/runtimeClassName: kata-clh/" -e "s/name: gvisor-dns-nft/name: sc-nested/" -e "s/namespace: gvisor-spike/namespace: $NS/" -e "s/spike: gvisor/spike: kata/" -e "s/^spec:/spec:\n  nodeSelector: { ade\/node: nested }/" "$PA/manifests/e1a-gvisor-dns-nft.yaml") 2>&1 | tail -1
kubectl -n $NS wait --for=condition=Ready pod/sc-nested --timeout=300s 2>&1 | tail -1; sleep 20
. "$PA/common.sh"; egress_probe $NS sc-nested
sec done
