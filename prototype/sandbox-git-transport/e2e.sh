# Runs inside a Linux container as root: execd (prototype) + git-remote-osb with real git.
set -u
export GIT_CONFIG_GLOBAL=/tmp/gc GIT_CONFIG_NOSYSTEM=1; printf "[safe]\n directory=*\n[user]\n name=t\n email=t@t\n[init]\n defaultBranch=main\n" > /tmp/gc
W=/tmp/e2e; rm -rf $W; mkdir -p $W/ws; chmod 755 $W $W/ws; chown 1000:1000 $W/ws
PASS=0; FAIL=0
ok()   { PASS=$((PASS+1)); echo "  ok   $1"; }
bad()  { FAIL=$((FAIL+1)); echo "  FAIL $1"; }
check() { if eval "$2"; then ok "$1"; else bad "$1  [$2]"; fi; }
cd /src/components/execd && go build -o /tmp/execd-proto . || exit 1
PORT=24680
EXECD_ACCESS_TOKEN=tok EXECD_GIT_REPO_ROOT=$W/ws /tmp/execd-proto --port $PORT >$W/execd.log 2>&1 &
EP=$!; trap 'kill $EP 2>/dev/null' EXIT
for i in $(seq 60); do curl -s localhost:$PORT/ping >/dev/null && break; sleep .2; done
export PATH=/src/prototype/sandbox-git-transport:$PATH; ln -sf /src/prototype/sandbox-git-transport/git-remote-osb /usr/local/bin/git-remote-osb
export OSB_EXECD_TOKEN=tok
SB=$W/ws/repo

echo "== client repo, first push creates the sandbox repository"
git init -q $W/client && cd $W/client && echo a > a && git add . && git commit -q -m one
git remote add sb osb://127.0.0.1:$PORT$SB
git push -q sb main:refs/heads/main 2>&1 | tail -2
check "init push created the repository on main" "[ \"\$(git -C $SB rev-parse refs/heads/main 2>/dev/null)\" = \"\$(git rev-parse main)\" ]"
check "sandbox repo is owned by the agent uid" "[ \"\$(stat -c %u $SB/.git)\" = 1000 ]"
git -C $SB reset -q --hard 2>/dev/null; chown -R 1000:1000 $SB

echo "== feature branch push and agent work fetched back"
git checkout -q -b feature && echo c > c && git add . && git commit -q -m two
git push -q sb feature:refs/heads/feature 2>&1 | tail -2
check "feature landed in the sandbox" "[ \"\$(git -C $SB rev-parse refs/heads/feature)\" = \"\$(git rev-parse feature)\" ]"
git -C $SB checkout -q feature && echo d > $SB/d && git -C $SB add . && git -C $SB commit -q -m agent && chown -R 1000:1000 $SB
git fetch -q sb 2>&1 | tail -2
check "agent commit is in refs/remotes/sb/feature" "[ \"\$(git rev-parse refs/remotes/sb/feature)\" = \"\$(git -C $SB rev-parse feature)\" ]"
check "second fetch is incremental (no error, nothing new)" "git fetch -q sb 2>&1 | grep -q . ; [ \$? -ne 0 ]"

echo "== hostile sandbox: tag named like a client branch, branch named tags/x"
AGENT=$(git -C $SB rev-parse feature)
git -C $SB tag main $AGENT 2>/dev/null; git -C $SB branch tags/x $AGENT; chown -R 1000:1000 $SB
LOCAL_MAIN=$(git rev-parse main)
git fetch -q sb 2>&1 | tail -2; git fetch -q sb 2>&1 | tail -2
check "local main still resolves to the client's own commit" "[ \"\$(git rev-parse main)\" = \"$LOCAL_MAIN\" ]"
check "no tags were written to refs/tags/" "[ -z \"\$(git for-each-ref refs/tags)\" ]"
check "sandbox tag is under refs/osb/sb/tags/main" "git rev-parse -q --verify refs/osb/sb/tags/main >/dev/null"
check "sandbox branch tags/x is under refs/remotes/sb/tags/x, disjoint from tags" "git rev-parse -q --verify refs/remotes/sb/tags/x >/dev/null"
check "remote tagOpt is --no-tags" "[ \"\$(git config remote.sb.tagOpt)\" = --no-tags ]"
check "git log main shows the client's history, not the agent commit" "! git log --format=%s main | grep -q agent"

echo "== hostile sandbox: malformed object"
TREE=$(git -C $SB rev-parse feature^{tree})
BADC=$(printf "tree $TREE\nauthor not valid\ncommitter t <t@t> 0 +0000\n\nbad\n" | git -C $SB hash-object -t commit -w --literally --stdin)
git -C $SB branch bad $BADC; chown -R 1000:1000 $SB
git fetch sb 2>&1 | grep -E "rejected|error" | head -1
check "malformed object: no refs/remotes/sb/bad" "! git rev-parse -q --verify refs/remotes/sb/bad >/dev/null"
git -C $SB branch -D bad -q; chown -R 1000:1000 $SB

echo "== force push with lease"
git checkout -q feature && git reset -q --hard HEAD~1 && echo e > e && git add . && git commit -q -m diverged
out=$(git push sb feature:refs/heads/feature 2>&1); check "non-fast-forward is refused without force" "echo \"\$out\" | grep -q -i -E 'rejected|non-fast'"
out=$(git push --force sb feature:refs/heads/feature 2>&1); check "forced push to the branch the agent has checked out is refused" "echo \"\$out\" | grep -q GIT_REF_CHECKED_OUT"
git -C $SB checkout -q --detach; chown -R 1000:1000 $SB
git push -q --force sb feature:refs/heads/feature 2>&1 | tail -1
check "forced push replaces the branch (lease taken from list)" "[ \"\$(git -C $SB rev-parse refs/heads/feature)\" = \"\$(git rev-parse feature)\" ]"

echo "== refusals"
git clone -q --depth 1 file://$W/client $W/shallow 2>/dev/null; cd $W/shallow && git remote add sb osb://127.0.0.1:$PORT$SB
msg=$(git fetch sb 2>&1); check "shallow client repository is refused" "echo \"\$msg\" | grep -q shallow"
cd $W/client
out=$(git push sb feature:refs/tags/v1 2>&1); echo "    >> $out" | head -3; check "push outside inbound prefixes is denied" "echo \"\$out\" | grep -q GIT_REF_DENIED"
out=$(OSB_EXECD_TOKEN=bad git fetch sb 2>&1); check "wrong token is rejected" "echo \"\$out\" | grep -q -i -E 'list failed|401'"
check "error output carries a request id" "echo \"\$out\" | grep -q 'request '"
echo; echo "RESULT pass=$PASS fail=$FAIL"
