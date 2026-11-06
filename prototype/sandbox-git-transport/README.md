Throwaway prototype for the Sandbox Git Transport proposal (OSEP-0027). Not production code and not intended to merge.

- execd endpoints under `/v1/git`: `components/execd/pkg/web/controller/git.go`, routes in `components/execd/pkg/web/router_git.go`. Linux only. Configuration is read from `EXECD_GIT_*` environment variables here; the proposal puts it in the execd TOML file.
- Tests need Linux and root, because they run git as a different owner to check the identity drop. Run them in a container, for example `docker run --rm -v "$PWD":/src -w /src/components/execd golang:1.25 go test ./pkg/web/controller -count=1`. `GIT_PERF=1` enables the timing experiment in `git_perf_test.go`.
- `git-remote-opensandbox` and `e2e.sh` are the first-round Python remote helper and its script. They target the earlier `/git` routes and were not updated.

What the prototype established, in the order it mattered:

1. `git bundle unbundle` accepts a malformed commit even with `transfer.fsckObjects=true`. `git fetch` from the bundle and `git index-pack --strict` reject it. The prototype reads the bundle header itself and imports the pack with `index-pack --stdin --fix-thin --strict`.
2. A git process that dropped to the repository owner cannot reopen a root-owned `0600` staging file through `/proc/self/fd/N`. The bundle goes to git on stdin and comes back on stdout instead. An inherited directory descriptor for `--git-dir=/proc/self/fd/3` does work.
3. A bundle header can contain a `HEAD` line as well as `refs/` lines.
4. A `+` in a query string decodes to a space unless it is sent as `%2B`.
5. `os.Root` plus an `Lstat` walk resolves repository paths with no leak under a concurrent symlink-swap race test, and rejects linked worktrees, `commondir`, alternates, grafts, and symlinked `.git` entries.
6. `pack.window=0` cuts the silent time before the first byte of `bundle/create` on large incompressible data from 24.7 s to 7.5 s at 264 MiB, with the same bundle size.
