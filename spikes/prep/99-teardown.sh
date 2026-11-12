#!/usr/bin/env bash
# Removes ONLY the ade-spike profile. Never touches the default VM.
set -eu; . "$(dirname "$0")/common.sh"
[ "${ADE_SPIKE_CONFIRM:-}" = yes ] || { echo "set ADE_SPIKE_CONFIRM=yes to delete the $SPIKE_PROFILE profile"; exit 1; }
colima delete --profile "$SPIKE_PROFILE" --force
