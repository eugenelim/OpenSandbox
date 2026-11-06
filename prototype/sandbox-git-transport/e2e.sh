set -eu
W=$(mktemp -d); cd "$W"; W=$(pwd -P)
export GIT_CONFIG_GLOBAL=$W/gitconfig; printf '[user]\n name=t\n email=t@t\n[init]\n defaultBranch=main\n' > $GIT_CONFIG_GLOBAL
mkdir -p ws/repo && git -C ws/repo init -q && echo a > ws/repo/a && git -C ws/repo add . && git -C ws/repo commit -qm one
PORT=$((20000 + RANDOM % 20000))
EXECD_ACCESS_TOKEN=tok EXECD_GIT_REPO_ROOT=$W/ws /tmp/execd-proto --port $PORT >$W/execd.log 2>&1 &
EP=$!; trap 'kill $EP 2>/dev/null' EXIT
for i in $(seq 50); do curl -s localhost:$PORT/ping >/dev/null && break; sleep .2; done
export PATH=/tmp/gitproto/bin:$PATH OPENSANDBOX_EXECD_TOKEN=tok
git clone -q $W/ws/repo client && cd client
git remote add sb opensandbox://127.0.0.1:$PORT$W/ws/repo
echo "--- push feature into sandbox"
git checkout -q -b feature && echo c > c && git add . && git commit -qm two
git push sb feature:refs/heads/feature
git -C $W/ws/repo log --oneline refs/heads/feature
echo "--- agent commits in sandbox, client fetches"
git -C $W/ws/repo checkout -q feature && echo d > $W/ws/repo/d && git -C $W/ws/repo add . && git -C $W/ws/repo commit -qm agent
git fetch sb
git log --oneline sb/feature
echo "--- push to checked-out branch is refused"
git push sb feature:refs/heads/feature || true
echo "--- wrong token"
OPENSANDBOX_EXECD_TOKEN=bad git fetch sb 2>&1 | tail -2 || true
echo E2E_DONE
