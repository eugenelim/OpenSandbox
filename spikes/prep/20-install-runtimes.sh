#!/usr/bin/env bash
# UNTESTED. Installs gVisor (runsc) and Kata into the ade-spike VM only and registers them with that VM's Docker:
#   runsc        gVisor
#   kata-runtime Kata with QEMU (the OpenSandbox 'kata' mapping)
#   kata-clh     Kata with Cloud Hypervisor
# kata-fc (Firecracker) is NOT registered for Docker: OpenSandbox maps Firecracker only on Kubernetes (RuntimeClass kata-fc).
set -eu; . "$(dirname "$0")/common.sh"; guard_spike_docker
colima ssh -p "$SPIKE_PROFILE" -- sudo bash -s <<'VM'
set -eu
[ -e /dev/kvm ] || { echo "NO /dev/kvm: nested virtualisation is not available; Kata cannot run here. Stopping."; exit 3; }
ARCH=$(uname -m)
# --- gVisor (apt repository, as in OpenSandbox docs/guides/secure-container.md; the bucket has no 'latest' alias)
curl -fsSL https://gvisor.dev/archive.key | gpg --batch --yes --dearmor -o /usr/share/keyrings/gvisor-archive-keyring.gpg
echo "deb [arch=$(dpkg --print-architecture) signed-by=/usr/share/keyrings/gvisor-archive-keyring.gpg] https://storage.googleapis.com/gvisor/releases release main" > /etc/apt/sources.list.d/gvisor.list
apt-get update -qq && apt-get install -y -qq runsc
runsc --version | head -2
runsc install -- --overlay2=none
# --- Kata static release (latest tag resolved at run time and recorded)
TAG=$(curl -fsSL https://api.github.com/repos/kata-containers/kata-containers/releases/latest | sed -n 's/.*"tag_name": *"\([^"]*\)".*/\1/p' | head -1)
echo "kata tag: $TAG"
case "$ARCH" in aarch64) KA=arm64;; *) KA=amd64;; esac
cd /var/tmp && df -h /var/tmp | tail -1
apt-get install -y -qq zstd
# assets are .tar.zst; kata-go-static is the Go runtime build (configuration-qemu.toml, configuration-clh.toml, configuration-fc.toml)
curl -fsSLO https://github.com/kata-containers/kata-containers/releases/download/$TAG/kata-go-static-$TAG-$KA.tar.zst
tar --zstd -xf kata-go-static-$TAG-$KA.tar.zst -C /
ls /opt/kata/share/defaults/kata-containers/ | head -20
# Kata 4.3.1 rejects a wrapper-set KATA_CONF_FILE; the shim picks its config from its own name, so symlink it
for h in qemu clh; do ln -sf /opt/kata/bin/containerd-shim-kata-v2 /usr/local/bin/containerd-shim-kata-$h-v2; done
# nested Apple CPU: no PMU property; guest boot is slow under nested virt
D=/opt/kata/share/defaults/kata-containers
sed -i 's/^cpu_features = "pmu=off"/cpu_features = ""/' $D/configuration-qemu.toml
sed -i 's/^#\?dial_timeout = .*/dial_timeout = 300/' $D/configuration-qemu.toml $D/configuration-clh.toml
ln -sf /opt/kata/bin/kata-runtime /usr/local/bin/kata-runtime
python3 - <<'PY'
import json,os
p='/etc/docker/daemon.json'; d=json.load(open(p)) if os.path.exists(p) else {}
rt=d.setdefault('runtimes',{})
rt['kata-runtime']={'runtimeType':'io.containerd.kata-qemu.v2'}
rt['kata-clh']={'runtimeType':'io.containerd.kata-clh.v2'}
json.dump(d,open(p,'w'),indent=2)
PY
kata-runtime check || true
systemctl restart docker
docker info --format '{{json .Runtimes}}' | tr ',' '\n' | grep -E 'runsc|kata' || true
VM
