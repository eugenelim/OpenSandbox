#!/usr/bin/env bash
# Intended order once the default VM is free (or a 6 GiB spike VM is acceptable). Not run automatically.
cat <<'TXT'
 0. ./00-preflight.sh                      read-only; says what size is safe
 1. ./05-setup-venv.sh                     local venv (no Docker)
 2. ADE_SPIKE_CONFIRM=yes ./10-start-vm.sh start the separate ade-spike profile (nested virt)
 3. ./20-install-runtimes.sh               gVisor + Kata inside the spike VM only
 4. ./30-s5-docker-in-kata.sh              S-5 (see ../../kata-test-plan.md for the full matrix)
    ./35-kata-bench.sh                       K2, K3, K5, K6 relative numbers across runc, runsc, kata-runtime, kata-clh
 5. ./40-s2-gvisor-egress.sh               S-2 (gVisor, then Kata)
 6. build images in the spike VM:          docker build -t ade-spike-base:0 ../sandbox-image.Dockerfile dir; docker build -t ade-stack-pynode:0 (../stack-pynode.Dockerfile);
                                           docker build -t ade-execd-proto:0 from the prototype copy (see ../13-spike-results.md)
 7. server with config-proto-execd.toml pointing at the spike socket, then:
    python 50-s4-pool-load.py <port> 10 25 [50]      (50 only if the VM has >= 12 GiB)
    python 60-s10-make-ci.py <port>
 8. ADE_SPIKE_CONFIRM=yes ./99-teardown.sh
TXT
