#!/usr/bin/env bash
# UNTESTED. Kata benchmark (plan K2, K3, K5, K6) in the ade-spike VM. Relative numbers only.
# Usage: ./35-kata-bench.sh [runtime ...]   default: runc runsc kata-runtime kata-clh
set -u; . "$(dirname "$0")/common.sh"; guard_spike_docker || exit 1
RT=${*:-runc runsc kata-runtime kata-clh}; N=${BENCH_N:-20}; OUT="$SPIKE_DIR/kata-bench-$(date +%Y%m%d-%H%M%S).txt"; mkdir -p "$SPIKE_DIR"
now_ms() { python3 -c 'import time;print(int(time.time()*1000))'; }
vm_free_mib() { docker run --rm alpine sh -c 'grep MemAvailable /proc/meminfo' | awk '{print int($2/1024)}'; }
{
echo "date $(date -u +%FT%TZ)  host kernel: $(docker run --rm alpine uname -r)"
for r in $RT; do
  echo "== $r"
  docker run --rm --runtime "$r" alpine uname -r > /tmp/k.out 2>&1 || { echo "  runtime unavailable: $(head -c 160 /tmp/k.out)"; continue; }
  echo "  K1 guest kernel: $(cat /tmp/k.out)"
  # K2 start latency
  ts=(); for i in $(seq 1 $N); do a=$(now_ms); docker run --rm --runtime "$r" alpine true >/dev/null 2>&1; ts+=($(( $(now_ms)-a ))); done
  sorted=($(printf '%s\n' "${ts[@]}" | sort -n)); echo "  K2 start ms: median=${sorted[$((N/2))]} p95=${sorted[$((N*95/100))]} max=${sorted[$((N-1))]}"
  # K3 idle memory: 1, 5, 10 sleeping containers
  base=$(vm_free_mib); echo "  K3 free MiB before: $base"
  for n in 1 5 10; do
    ids=$(for i in $(seq 1 $n); do docker run -d --runtime "$r" alpine sleep 600; done); sleep 5
    free=$(vm_free_mib); echo "  K3 $n idle sandboxes: VM free MiB $free (used $((base-free)) => $(( (base-free)/n )) MiB each)"
    docker rm -f $ids >/dev/null 2>&1; sleep 3
  done
  # K5 small-file workload inside the sandbox
  a=$(now_ms); docker run --rm --runtime "$r" alpine sh -c 'apk add -q git >/dev/null 2>&1; cd /tmp && git init -q w && cd w && git config user.email a@b && git config user.name n && for i in $(seq 1 2000); do echo $i > f$i; done && git add . && git commit -qm x && git clone -q /tmp/w /tmp/c' >/dev/null 2>&1
  echo "  K5 2,000-file git add/commit/clone: $(( $(now_ms)-a )) ms (includes apk add git)"
done
# K6 Docker in the sandbox (Kata runtimes only)
for r in $RT; do case "$r" in kata*)
  echo "== K6 docker-in-sandbox on $r"; docker rm -f ade-dind >/dev/null 2>&1
  docker run -d --name ade-dind --runtime "$r" --privileged -e DOCKER_TLS_CERTDIR= docker:dind >/dev/null 2>&1 || { echo "  could not start dind"; continue; }
  for i in $(seq 1 40); do docker exec ade-dind docker info >/dev/null 2>&1 && break; sleep 2; done
  docker exec ade-dind docker info >/dev/null 2>&1 && { a=$(now_ms); docker exec ade-dind docker run --rm alpine echo inner-ok; echo "  inner container run: $(( $(now_ms)-a )) ms (includes image pull)"; } || echo "  inner dockerd did not become ready"
  docker rm -f ade-dind >/dev/null 2>&1;; esac; done
} 2>&1 | tee "$OUT"
echo "results: $OUT"
