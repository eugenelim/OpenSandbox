Throwaway prototype for the Sandbox Git Transport proposal. Not production code and not intended to merge.

- execd endpoints: `components/execd/pkg/web/controller/git*.go`, routes in `components/execd/pkg/web/router_git*.go`. Run `go test ./pkg/web/controller -run TestGit` in `components/execd`.
- `git-remote-opensandbox`: Python remote helper that speaks to the prototype endpoints (`/git/...`, env `EXECD_GIT_REPO_ROOT`, plain HTTP, token from `OPENSANDBOX_EXECD_TOKEN`).
- `e2e.sh`: starts a built execd, then runs a real `git push` and `git fetch` through the helper. Expects the execd binary at `/tmp/execd-proto` and the helper on `PATH`.

The OSEP draft supersedes several choices made here (`/v1/git` prefix, config file instead of env vars, descriptor-based paths, execd-owned staging, agent-identity git). The prototype keeps the earlier shape on purpose.
