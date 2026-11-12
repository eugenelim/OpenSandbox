#!/usr/bin/env bash
# DO NOT RUN while the default VM is in use for other work and memory is tight.
# Creates a SEPARATE colima profile with nested virtualisation; the default profile is not touched.
set -eu; . "$(dirname "$0")/common.sh"
[ "${ADE_SPIKE_CONFIRM:-}" = yes ] || { echo "set ADE_SPIKE_CONFIRM=yes to start the $SPIKE_PROFILE VM"; exit 1; }
MEM="${ADE_SPIKE_MEM:-6}"; CPUS="${ADE_SPIKE_CPUS:-4}"; DISK="${ADE_SPIKE_DISK:-60}"
colima start --profile "$SPIKE_PROFILE" --vm-type vz --nested-virtualization --runtime docker \
  --cpus "$CPUS" --memory "$MEM" --disk "$DISK" --mount "$(cd "$(dirname "$0")/.." && pwd):r"
colima ssh -p "$SPIKE_PROFILE" -- sh -c 'ls -l /dev/kvm || echo "NO /dev/kvm: nested virtualisation is not available; Kata cannot run"'
