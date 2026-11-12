FROM node:24-bookworm-slim AS node
FROM python:3.12-slim-bookworm
COPY --from=node /usr/local/bin/node /usr/local/bin/node
COPY --from=node /usr/local/lib/node_modules /usr/local/lib/node_modules
RUN ln -s ../lib/node_modules/npm/bin/npm-cli.js /usr/local/bin/npm && ln -s ../lib/node_modules/npm/bin/npx-cli.js /usr/local/bin/npx \
 && apt-get update -qq && apt-get install -y -qq --no-install-recommends git make ca-certificates curl >/dev/null && rm -rf /var/lib/apt/lists/* \
 && pip install -q --no-cache-dir uv ruff mypy pytest \
 && useradd -m -u 1000 agent && mkdir -p /workspace && chown agent /workspace
