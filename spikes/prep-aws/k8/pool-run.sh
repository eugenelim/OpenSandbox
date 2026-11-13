#!/usr/bin/env bash
# Pooled-authentication test on EKS (Kata, OpenSandbox pools, egress sidecar). Everything is pulled directly while node internet is still open (about 20 minutes after the cluster is ready).
export AWS_PROFILE=${AWS_PROFILE:?set AWS_PROFILE} AWS_REGION=${AWS_REGION:-us-east-1} AWS_PAGER=""
HERE="$(cd "$(dirname "$0")" && pwd)"; REPO=${OPENSANDBOX_REPO:?set OPENSANDBOX_REPO}; PY=${PY:-/tmp/spikes/venv-rc2/bin/python}; V=release-1.2.1
sec(){ echo; echo "== $(date -u +%H:%M:%S) $*"; }
sec nodes; kubectl get nodes -L ade/node 2>&1 | cut -c1-110
cat > /tmp/keep-layers.toml <<'Y'
[plugins."io.containerd.cri.v1.images"]
  discard_unpacked_layers = false
Y
sec kata-deploy
helm install kata-deploy oci://ghcr.io/kata-containers/kata-deploy-charts/kata-deploy --namespace kube-system --set 'nodeSelector.ade/runtime=kata' --set-file containerd.userDropIn=/tmp/keep-layers.toml --wait --timeout 12m 2>&1 | tail -2
sleep 45; kubectl get runtimeclass kata-clh 2>&1 | cut -c1-60
sec opensandbox
kubectl create namespace opensandbox >/dev/null 2>&1
cat > /tmp/os-values-pool.yaml <<Y
opensandbox-controller:
  controller:
    image: { repository: opensandbox/controller, tag: $V }
opensandbox-server:
  server:
    image: { repository: opensandbox/server, tag: $V }
    env: [{ name: OPENSANDBOX_INSECURE_SERVER, value: "YES" }]
  configToml: |
    [server]
    host = "0.0.0.0"
    port = 80
    api_key = ""
    [log]
    level = "INFO"
    [runtime]
    type = "kubernetes"
    execd_image = "opensandbox/execd:$V"
    [kubernetes]
    kubeconfig_path = ""
    namespace = "opensandbox"
    informer_resync_seconds = 300
    informer_watch_timeout_seconds = 60
    workload_provider = "batchsandbox"
    batchsandbox_template_file = "/etc/opensandbox/example.batchsandbox-template.yaml"
    [secure_runtime]
    type = "kata"
    k8s_runtime_class = "kata-clh"
    [egress]
    image = "opensandbox/egress:$V"
    mode = "dns+nft"
Y
( cd "$REPO/manifests/charts" && helm dependency build opensandbox 2>&1 | tail -1 && helm install opensandbox opensandbox --namespace opensandbox-system --create-namespace -f /tmp/os-values-pool.yaml --timeout 8m 2>&1 | tail -1 )
kubectl -n opensandbox-system rollout status deploy/opensandbox-controller-manager --timeout=300s 2>&1 | tail -1
kubectl -n opensandbox-system rollout status deploy/opensandbox-server --timeout=300s 2>&1 | tail -1
sec pool
kubectl apply -f "$HERE/pool-a.yaml" 2>&1 | tail -1
for i in $(seq 1 60); do n=$(kubectl -n opensandbox get pods -l app=pool-a --no-headers 2>/dev/null | grep -c " 3/3 .*Running\| 2/2 .*Running"); [ "$n" -ge 2 ] && break; sleep 10; done
kubectl -n opensandbox get pods -l app=pool-a -o wide --no-headers 2>&1 | cut -c1-140
kubectl -n opensandbox get pool pool-a 2>&1 | cut -c1-140
sec pool-test
pkill -f "kubectl.*port-forward" 2>/dev/null; sleep 1
kubectl -n opensandbox-system port-forward svc/opensandbox-server ${OS_PORT:-18090}:80 >/tmp/pf-pool.log 2>&1 & PF=$!; sleep 6
OS_PORT=${OS_PORT:-18090} "$PY" "$HERE/pool.py" 2>&1 | tail -45
kill $PF 2>/dev/null
sec server-log
kubectl -n opensandbox-system logs deploy/opensandbox-server --tail=60 2>&1 | grep -vE "GET /health|metrics" | grep -iE "pool|error|warn|networkpolicy|egress" | tail -10 | cut -c1-260
sec done
