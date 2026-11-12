#!/usr/bin/env bash
# Read-only. Starts nothing. Prints whether it is sensible to start the spike VM now.
set -u; . "$(dirname "$0")/common.sh"
mem_gib=$(( $(sysctl -n hw.memsize) / 1073741824 )); cpus=$(sysctl -n hw.ncpu)
echo "host: ${mem_gib} GiB RAM, ${cpus} CPUs, $(df -h "$HOME" | tail -1 | awk '{print $4}') free on home"
echo "colima profiles:"; colima list 2>&1 | sed 's/^/  /'
if colima list 2>/dev/null | awk 'NR>1{print $1,$2}' | grep -q "^default Running"; then
  echo "NOTE: the default VM is running. Keep the spike VM at or below 6 GiB, or stop the default VM first."
  echo "      Suggested: ADE_SPIKE_MEM=6 ADE_SPIKE_CPUS=4 (Kata + gVisor + one make ci; skip the 50-sandbox pool run)."
else
  echo "Default VM is stopped: ADE_SPIKE_MEM=12 ADE_SPIKE_CPUS=8 is fine for all spikes."
fi
colima list 2>/dev/null | awk 'NR>1{print $1}' | grep -qx "$SPIKE_PROFILE" && echo "profile $SPIKE_PROFILE already exists (delete with 99-teardown.sh)"
command -v uv >/dev/null && echo "uv: ok" || echo "uv: missing"
[ -x "$SPIKE_DIR/venv/bin/opensandbox-server" ] && echo "spike venv: ok" || echo "spike venv: missing (run 05-setup-venv.sh; it needs no Docker)"
exit 0
