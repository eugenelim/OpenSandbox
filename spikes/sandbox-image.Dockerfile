FROM debian:bookworm-slim
RUN apt-get update -qq && apt-get install -y -qq --no-install-recommends git ca-certificates curl dnsutils iputils-ping >/dev/null && rm -rf /var/lib/apt/lists/* && useradd -m -u 1000 agent && mkdir -p /workspace && chown agent /workspace
