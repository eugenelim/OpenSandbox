#!/usr/bin/env bash
# S-5: does a Docker daemon run inside a Kata microVM, and what does it cost?
set -u; . "$(dirname "$0")/common.sh"; guard_spike_docker || exit 1
N=ade-dind-spike; docker rm -f $N >/dev/null 2>&1
echo "host kernel: $(docker run --rm alpine uname -r)"
docker run -d --name $N --runtime kata-runtime --privileged -e DOCKER_TLS_CERTDIR= docker:dind >/dev/null || { echo "RESULT S-5: kata container failed to start"; exit 2; }
echo "kata guest kernel: $(docker exec $N uname -r)  (different from host = real VM)"
for i in $(seq 1 40); do docker exec $N docker info >/dev/null 2>&1 && break; sleep 2; done
docker exec $N docker info >/dev/null 2>&1 || { echo "RESULT S-5: inner dockerd did not become ready"; docker logs --tail 20 $N; docker rm -f $N >/dev/null; exit 3; }
t=$(date +%s); docker exec $N docker run --rm alpine echo inner-container-ok; echo "inner run secs: $(( $(date +%s)-t ))"
docker exec $N sh -c 'free -m | head -2'
docker stats --no-stream --format 'sandbox mem: {{.MemUsage}}' $N
docker rm -f $N >/dev/null; echo "RESULT S-5: pass"
