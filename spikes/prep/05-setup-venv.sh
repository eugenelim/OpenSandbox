#!/usr/bin/env bash
# Local only, no Docker: installs the server and Python SDK from the fork checkout into a scratch venv.
set -eu; . "$(dirname "$0")/common.sh"
mkdir -p "$SPIKE_DIR"; uv venv -q --python 3.12 "$SPIKE_DIR/venv"
uv pip install -q --python "$SPIKE_DIR/venv/bin/python" "$REPO/server" "$REPO/sdks/sandbox/python"
echo ok
