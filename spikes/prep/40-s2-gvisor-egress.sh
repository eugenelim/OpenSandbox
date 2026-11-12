#!/usr/bin/env bash
# S-2: egress sidecar with gVisor (then Kata) on Docker. Reuses s2_egress.py (domain/port/key come from env).
set -u; . "$(dirname "$0")/common.sh"; guard_spike_docker || exit 1
here="$(cd "$(dirname "$0")" && pwd)"; S="$here/.."
docker image inspect ade-spike-base:0 >/dev/null 2>&1 || docker build -q -t ade-spike-base:0 -f "$S/sandbox-image.Dockerfile" "$S" >/dev/null
for prof in gvisor kata; do
  port=$([ $prof = gvisor ] && echo 18090 || echo 18091); cfg="$here/config-$prof.toml"
  [ -x "$SPIKE_DIR/venv/bin/opensandbox-server" ] || { echo "run 05-setup-venv.sh"; exit 1; }
  "$SPIKE_DIR/venv/bin/opensandbox-server" --config "$cfg" > "$SPIKE_DIR/server-$prof.log" 2>&1 & SP=$!
  for i in $(seq 1 40); do curl -s -m 2 -o /dev/null -H "OPEN-SANDBOX-API-KEY: spike-key" localhost:$port/v1/sandboxes && break; sleep 2; done
  echo "== $prof"; sed "s/127.0.0.1:18080/127.0.0.1:$port/" "$S/s2_egress.py" > "$SPIKE_DIR/s2_$prof.py"
  "$SPIKE_DIR/venv/bin/python" "$SPIKE_DIR/s2_$prof.py" 2>&1 | tail -40
  kill $SP 2>/dev/null; wait $SP 2>/dev/null
  docker rm -f $(docker ps -aq --filter "name=sandbox-") >/dev/null 2>&1
done
