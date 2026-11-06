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
6. Delta search dominates the silent time before the first byte of `bundle/create` on large incompressible data. `pack.window=0` fixes the time but makes a bundle of 200 small agent commits about 19 times larger (76,753 B against 1,459,406 B in the same run). `core.bigFileThreshold=4m` fixes the time (264 MiB of random blobs: 24.6 s down to 4.7 s in a plain git run) and leaves small-commit bundles the same size or smaller.
7. `git fetch` from a bundle with `transfer.fsckObjects=true` accepted the malformed commit on git 2.34 and 2.39 and rejected it on 2.47. `git index-pack --strict` rejected it on all three, so the client helper should import the same way execd does.
8. Checks added after review, all passing: every `.git` entry other than `objects` and `refs` must be owned by the target uid and not group or other writable (including `config.worktree`); `include` and `includeIf` sections in `.git/config` are refused; a repository owned by uid 0 needs `run_as=0:0`; ref names are capped at 200 bytes so that 64 mappings fit in the 32 KiB query limit; error bodies carry no repository or staging path.
9. While a body is uploading, the staged bytes exist only as an unlinked descriptor: no file is visible in the temp directory, root can read them through `/proc/self/fd` (control), and a process running as the agent uid cannot.
10. With `Expect: 100-continue`, a request refused with 429 uploads nothing (curl sent 0 bytes). Without it, curl uploaded 1.8 MB of a 64 MiB body before reading the 429. The header saves the upload; it was not needed to see the status in this run.
