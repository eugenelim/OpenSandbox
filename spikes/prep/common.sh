# Sourced by the prep scripts. Safety: never talk to the default colima VM.
SPIKE_PROFILE=ade-spike
SPIKE_SOCK="$HOME/.colima/$SPIKE_PROFILE/docker.sock"
SPIKE_DIR="${SPIKE_DIR:-/tmp/spikes}"
REPO="${OPENSANDBOX_REPO:?set OPENSANDBOX_REPO to an OpenSandbox checkout}"
guard_spike_docker() {
  export DOCKER_HOST="unix://$SPIKE_SOCK"
  [ -S "$SPIKE_SOCK" ] || { echo "spike VM socket $SPIKE_SOCK not found: start the $SPIKE_PROFILE profile first (10-start-vm.sh)"; return 1; }
  case "$DOCKER_HOST" in *colima/default*) echo "refusing: points at the default VM"; return 1;; esac
}
