#!/bin/bash
# UNTESTED node bootstrap: install gVisor and register it with containerd. Adapt to the AMI's containerd config mechanism:
#  - AL2023 EKS AMI: nodeadm builds /etc/containerd/config.toml; a drop-in under /etc/containerd/config.d/ is expected to be imported (verify), or use NodeConfig spec.containerd.config.
#  - Ubuntu EKS AMI: edit /etc/containerd/config.toml directly.
set -eux
ARCH=$(uname -m); URL=https://storage.googleapis.com/gvisor/releases/release/latest/${ARCH}
cd /tmp
for f in runsc containerd-shim-runsc-v1; do curl -fsSLO $URL/$f; curl -fsSLO $URL/$f.sha512; sha512sum -c $f.sha512; chmod a+rx $f; mv $f /usr/local/bin/; done
mkdir -p /etc/containerd/config.d
cat > /etc/containerd/config.d/runsc.toml <<'TOML'
[plugins."io.containerd.grpc.v1.cri".containerd.runtimes.runsc]
  runtime_type = "io.containerd.runsc.v1"
TOML
grep -q 'config.d' /etc/containerd/config.toml || echo 'WARNING: config.d is not imported by /etc/containerd/config.toml; register runsc another way' >> /var/log/ade-gvisor-bootstrap.log
systemctl restart containerd || true
