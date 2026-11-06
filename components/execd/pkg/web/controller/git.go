// Copyright 2026 The OpenSandbox Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

//go:build linux

package controller

// PROTOTYPE (OSEP-0027 Sandbox Git Transport): throwaway implementation used to
// harden the proposal. Not production code. Configuration comes from the
// environment here; the proposal puts it in the execd TOML file.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/alibaba/opensandbox/execd/pkg/util/pathutil"
	"github.com/alibaba/opensandbox/execd/pkg/web/model"
)

const (
	gitRepoRootEnv   = "EXECD_GIT_REPO_ROOT"
	gitInboundEnv    = "EXECD_GIT_INBOUND_PREFIXES"
	gitOutboundEnv   = "EXECD_GIT_OUTBOUND_PREFIXES"
	gitMaxBundleEnv  = "EXECD_GIT_MAX_BUNDLE_BYTES"
	gitRunAsEnv      = "EXECD_GIT_RUN_AS"
	gitBinEnv        = "EXECD_GIT_BIN"
	gitTimeoutEnv    = "EXECD_GIT_TIMEOUT_SECONDS"
	gitMaxConcEnv    = "EXECD_GIT_MAX_CONCURRENT"
	gitUnsafeTestEnv = "EXECD_GIT_UNSAFE_FOR_TEST" // negative control only
	gitMinRateEnv    = "EXECD_GIT_MIN_UPLOAD_BPS"
	gitMinWindowEnv  = "EXECD_GIT_MIN_UPLOAD_WINDOW_SECONDS"

	gitDefaultInbound  = "refs/heads/"
	gitDefaultOutbound = "refs/heads/,refs/tags/"
	gitDefaultMaxBytes = int64(256 << 20)
	zeroOID            = "0000000000000000000000000000000000000000"
)

var gitObjectIDRe = regexp.MustCompile(`^[0-9a-f]{40}$`)

var includeRe = regexp.MustCompile(`(?im)^\s*\[\s*include(if)?[\s"\]]`)

const maxRefNameBytes = 200

func runAsOf(p gitPolicy) string {
	if p.runAs != "" {
		return p.runAs
	}
	return "x:x"
}

type gitPolicy struct {
	repoRoot string
	inbound  []string
	outbound []string
	maxBytes int64
	runAs    string
	timeout  time.Duration
	maxConc  int
	minRate  int64
	minWin   time.Duration
}

func loadGitPolicy() gitPolicy {
	split := func(v, def string) []string {
		if v == "" {
			v = def
		}
		var out []string
		for _, p := range strings.Split(v, ",") {
			if p = strings.TrimSpace(p); p != "" {
				out = append(out, p)
			}
		}
		return out
	}
	p := gitPolicy{
		repoRoot: os.Getenv(gitRepoRootEnv),
		inbound:  split(os.Getenv(gitInboundEnv), gitDefaultInbound),
		outbound: split(os.Getenv(gitOutboundEnv), gitDefaultOutbound),
		maxBytes: gitDefaultMaxBytes,
		runAs:    os.Getenv(gitRunAsEnv),
		timeout:  300 * time.Second,
		maxConc:  2,
		minRate:  64 << 10,
		minWin:   30 * time.Second,
	}
	if v, err := strconv.ParseInt(os.Getenv(gitMaxBundleEnv), 10, 64); err == nil && v > 0 {
		p.maxBytes = v
	}
	if v, err := strconv.Atoi(os.Getenv(gitTimeoutEnv)); err == nil && v > 0 {
		p.timeout = time.Duration(v) * time.Second
	}
	if v, err := strconv.ParseInt(os.Getenv(gitMinRateEnv), 10, 64); err == nil && v > 0 {
		p.minRate = v
	}
	if v, err := strconv.Atoi(os.Getenv(gitMinWindowEnv)); err == nil && v > 0 {
		p.minWin = time.Duration(v) * time.Second
	}
	if v, err := strconv.Atoi(os.Getenv(gitMaxConcEnv)); err == nil && v > 0 {
		p.maxConc = v
	}
	return p
}

func hasPrefix(prefixes []string, ref string) bool {
	for _, p := range prefixes {
		if strings.HasPrefix(ref, p) {
			return true
		}
	}
	return false
}

// Per-repository locks keyed on device+inode, plus a global slot count.
var (
	gitLockMu sync.Mutex
	gitLocks  = map[[2]uint64]bool{}
	gitSlots  int
)

func acquireGit(key [2]uint64, max int) bool {
	gitLockMu.Lock()
	defer gitLockMu.Unlock()
	if gitLocks[key] || gitSlots >= max {
		return false
	}
	gitLocks[key] = true
	gitSlots++
	return true
}

func releaseGit(key [2]uint64) {
	gitLockMu.Lock()
	defer gitLockMu.Unlock()
	delete(gitLocks, key)
	gitSlots--
}

type GitController struct {
	*basicController
}

func NewGitController(ctx *gin.Context) *GitController {
	return &GitController{basicController: newBasicController(ctx)}
}

type gitError struct {
	status  int
	code    model.ErrorCode
	message string
}

func (e *gitError) Error() string { return e.message }

func gitErr(status int, code model.ErrorCode, format string, a ...any) *gitError {
	return &gitError{status: status, code: code, message: fmt.Sprintf(format, a...)}
}

func (c *GitController) fail(err *gitError) {
	switch err.status {
	case http.StatusRequestTimeout, http.StatusRequestEntityTooLarge, http.StatusTooManyRequests:
		// Without this the Go server drains an unread request body (up to
		// 256 KiB) before it sends the response, which a slow client can stall.
		c.ctx.Header("Connection", "close")
	}
	if id := c.ctx.GetHeader("X-Request-ID"); id != "" {
		c.ctx.Header("X-Request-ID", id)
	}
	c.RespondError(err.status, err.code, err.message)
}

const (
	codeRefDenied     model.ErrorCode = "GIT_REF_DENIED"
	codePathDenied    model.ErrorCode = "GIT_PATH_DENIED"
	codeRepoNotFound  model.ErrorCode = "GIT_REPO_NOT_FOUND"
	codeCheckedOut    model.ErrorCode = "GIT_REF_CHECKED_OUT"
	codeNotFF         model.ErrorCode = "GIT_NOT_FAST_FORWARD"
	codeStaleLease    model.ErrorCode = "GIT_STALE_LEASE"
	codeRefLocked     model.ErrorCode = "GIT_REF_LOCKED"
	codeInitNotEmpty  model.ErrorCode = "GIT_INIT_TARGET_NOT_EMPTY"
	codeTooLarge      model.ErrorCode = "GIT_TOO_LARGE"
	codePrereqMissing model.ErrorCode = "GIT_PREREQUISITE_MISSING"
	codeBundleInvalid model.ErrorCode = "GIT_BUNDLE_INVALID"
	codeBusy          model.ErrorCode = "GIT_BUSY"
	codeUploadStalled model.ErrorCode = "GIT_UPLOAD_STALLED"
	codeTimeout       model.ErrorCode = "GIT_TIMEOUT"
)

// repo is a resolved repository. gitdir is an open descriptor on <repo>/.git;
// git is handed that descriptor, never a path.
type repo struct {
	gitdir *os.File
	uid    uint32
	gid    uint32
	key    [2]uint64
}

func (r *repo) Close() { _ = r.gitdir.Close() }

func statIDs(fi os.FileInfo) (uid, gid uint32, dev, ino uint64) {
	st := fi.Sys().(*syscall.Stat_t)
	return st.Uid, st.Gid, uint64(st.Dev), st.Ino
}

func isSymlink(fi os.FileInfo) bool { return fi.Mode()&os.ModeSymlink != 0 }

// openRepo resolves raw under the configured root without following any
// symlink, rejects redirects inside .git, and checks that everything git
// will read belongs to one owner.
func openRepo(p gitPolicy, raw string) (*repo, *gitError) {
	if p.repoRoot == "" {
		return nil, gitErr(http.StatusNotImplemented, model.ErrorCodeNotSupported, "git transport is not enabled")
	}
	if raw == "" {
		return nil, gitErr(http.StatusBadRequest, model.ErrorCodeMissingQuery, "missing 'path'")
	}
	expanded, err := pathutil.ExpandPath(raw)
	if err != nil {
		return nil, gitErr(http.StatusBadRequest, model.ErrorCodeInvalidRequest, "invalid path")
	}
	abs, err := filepath.Abs(expanded)
	if err != nil {
		return nil, gitErr(http.StatusBadRequest, model.ErrorCodeInvalidRequest, "invalid path")
	}
	rootPath, err := filepath.EvalSymlinks(p.repoRoot) // operator-controlled
	if err != nil {
		return nil, gitErr(http.StatusInternalServerError, model.ErrorCodeRuntimeError, "git repo root unavailable")
	}
	rel, err := filepath.Rel(rootPath, abs)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return nil, gitErr(http.StatusForbidden, codePathDenied, "repository is outside the configured root")
	}
	root, err := os.OpenRoot(rootPath)
	if err != nil {
		return nil, gitErr(http.StatusInternalServerError, model.ErrorCodeRuntimeError, "git repo root unavailable")
	}
	defer root.Close()

	// os.Root refuses escapes but allows symlinks that stay inside the root;
	// this feature allows none, so check every component.
	cur := ""
	if rel != "." {
		for _, part := range strings.Split(rel, string(filepath.Separator)) {
			cur = filepath.Join(cur, part)
			fi, err := root.Lstat(cur)
			if err != nil {
				return nil, gitErr(http.StatusNotFound, codeRepoNotFound, "repository not found")
			}
			if isSymlink(fi) {
				return nil, gitErr(http.StatusForbidden, codePathDenied, "symlinks are not allowed in repository path")
			}
		}
	}
	dir, err := root.OpenRoot(rel)
	if err != nil {
		return nil, gitErr(http.StatusForbidden, codePathDenied, "cannot open repository beneath the root")
	}
	defer dir.Close()

	gfi, err := dir.Lstat(".git")
	if err != nil {
		return nil, gitErr(http.StatusNotFound, codeRepoNotFound, "not a git repository")
	}
	if isSymlink(gfi) {
		return nil, gitErr(http.StatusForbidden, codePathDenied, ".git is a symlink")
	}
	if !gfi.IsDir() {
		return nil, gitErr(http.StatusForbidden, codePathDenied, "path is not a main working tree (linked worktree or file .git)")
	}
	uid, gid, dev, ino := statIDs(gfi)
	if why := gitUnusable(); why != "" {
		return nil, gitErr(http.StatusNotImplemented, model.ErrorCodeNotSupported, "git transport is disabled: %s", why)
	}
	target := uid
	if p.runAs != "" {
		a, _, _ := strings.Cut(p.runAs, ":")
		if u, err := strconv.ParseUint(a, 10, 32); err == nil {
			target = uint32(u)
		}
	}
	if os.Geteuid() != 0 && uint32(os.Geteuid()) != target {
		return nil, gitErr(http.StatusNotImplemented, model.ErrorCodeNotSupported, "git transport is disabled: execd cannot switch to the repository identity")
	}
	if uid == 0 && !strings.HasPrefix(runAsOf(p), "0:") {
		return nil, gitErr(http.StatusForbidden, codePathDenied, "repository is owned by uid 0; set run_as to 0:0 to allow")
	}
	// Every entry directly under .git other than the contents of objects and
	// refs must belong to the target uid and not be group or other writable.
	entries, err := fs.ReadDir(dir.FS(), ".git")
	if err != nil {
		return nil, gitErr(http.StatusForbidden, codePathDenied, "cannot read .git")
	}
	for _, e := range entries {
		fi, err := dir.Lstat(".git/" + e.Name())
		if err != nil {
			continue
		}
		if isSymlink(fi) && (e.Name() == "objects" || e.Name() == "refs" || e.Name() == "HEAD") {
			return nil, gitErr(http.StatusForbidden, codePathDenied, ".git/%s is a symlink", e.Name())
		}
		if u, g, _, _ := statIDs(fi); u != uid || g != gid {
			return nil, gitErr(http.StatusForbidden, codePathDenied, "mixed ownership under .git: %s", e.Name())
		}
		if fi.Mode().Perm()&0o022 != 0 && !isSymlink(fi) {
			return nil, gitErr(http.StatusForbidden, codePathDenied, ".git/%s is group or other writable", e.Name())
		}
	}
	if cfg, err := dir.ReadFile(".git/config"); err == nil && includeRe.Match(cfg) {
		return nil, gitErr(http.StatusForbidden, codePathDenied, ".git/config has an include section")
	}
	for _, name := range []string{"commondir", "objects/info/alternates", "info/grafts", "shallow"} {
		if _, err := dir.Lstat(".git/" + name); err == nil {
			return nil, gitErr(http.StatusForbidden, codePathDenied, ".git/%s is not supported", name)
		}
	}
	f, err := dir.Open(".git")
	if err != nil {
		return nil, gitErr(http.StatusInternalServerError, model.ErrorCodeRuntimeError, "cannot open .git")
	}
	return &repo{gitdir: f, uid: uid, gid: gid, key: [2]uint64{dev, ino}}, nil
}

type gitResult struct {
	stdout string
	stderr string
	err    error
}

// gitCmd builds a git invocation with the repository passed as descriptor 3.
// Extra inherited files start at descriptor 4.
func (r *repo) gitCmd(ctx context.Context, p gitPolicy, args ...string) (*exec.Cmd, error) {
	bin := os.Getenv(gitBinEnv)
	if bin == "" {
		bin = "git"
	}
	pre := []string{"--git-dir=/proc/self/fd/3"}
	if os.Getenv(gitUnsafeTestEnv) == "" {
		pre = append(pre,
			"-c", "core.hooksPath=/dev/null",
			"-c", "core.fsmonitor=false",
			"-c", "core.alternateRefsCommand=",
			"-c", "gc.auto=0",
			"-c", "maintenance.auto=false",
			"-c", "protocol.allow=never",
			"-c", "transfer.fsckObjects=true",
			"-c", "transfer.unpackLimit=1",
			"-c", "fetch.unpackLimit=1",
			"-c", "pack.threads=1",
			"-c", "pack.windowMemory=64m",
		)
	}
	if len(args) > 0 && args[0] == "bundle" {
		// Objects over 4 MiB are not delta-searched. The search dominates the
		// silent time before the first response byte on large incompressible
		// blobs, and small text objects still delta normally.
		pre = append(pre, "-c", "core.bigFileThreshold=4m")
	}
	cmd := exec.CommandContext(ctx, bin, append(pre, args...)...)
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=/nonexistent",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_TERMINAL_PROMPT=0",
		"GIT_NO_REPLACE_OBJECTS=1",
		"LC_ALL=C",
	}
	cmd.ExtraFiles = []*os.File{r.gitdir}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	uid, gid := r.uid, r.gid
	if p.runAs != "" {
		a, b, ok := strings.Cut(p.runAs, ":")
		u, e1 := strconv.ParseUint(a, 10, 32)
		g, e2 := strconv.ParseUint(b, 10, 32)
		if !ok || e1 != nil || e2 != nil {
			return nil, errors.New("invalid run_as")
		}
		uid, gid = uint32(u), uint32(g)
	}
	if os.Geteuid() == 0 && uid != 0 {
		cmd.SysProcAttr.Credential = &syscall.Credential{Uid: uid, Gid: gid}
	}
	return cmd, nil
}

func (r *repo) git(ctx context.Context, p gitPolicy, stdin io.Reader, args ...string) gitResult {
	cmd, err := r.gitCmd(ctx, p, args...)
	if err != nil {
		return gitResult{err: err}
	}
	var out, errb bytes.Buffer
	cmd.Stdin, cmd.Stdout, cmd.Stderr = stdin, &out, &errb
	err = cmd.Run()
	return gitResult{stdout: out.String(), stderr: strings.TrimSpace(errb.String() + " " + errString(err)), err: err}
}

func exitCode(err error) int {
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode()
	}
	return -1
}

type cappedWriter struct {
	w      io.Writer
	left   int64
	over   bool
	cancel context.CancelFunc
}

func (c *cappedWriter) Write(b []byte) (int, error) {
	if int64(len(b)) > c.left {
		c.over = true
		c.cancel()
		return 0, errors.New("size cap exceeded")
	}
	c.left -= int64(len(b))
	return c.w.Write(b)
}

// staging returns an unlinked temporary file: only an open descriptor exists.
func staging() (*os.File, error) {
	f, err := os.CreateTemp("", "execd-git-")
	if err != nil {
		return nil, err
	}
	_ = os.Remove(f.Name())
	return f, nil
}

func validRefName(r *repo, p gitPolicy, ref string) bool {
	if len(ref) > maxRefNameBytes || !strings.HasPrefix(ref, "refs/") || strings.ContainsAny(ref, "\x00\n \t") {
		return false
	}
	res := r.git(context.Background(), p, nil, "check-ref-format", ref)
	return res.err == nil
}

type gitRef struct {
	Name string `json:"name"`
	SHA  string `json:"sha"`
}

func (c *GitController) withRepo(p gitPolicy, raw string) (*repo, bool) {
	r, gerr := openRepo(p, raw)
	if gerr != nil {
		c.fail(gerr)
		return nil, false
	}
	return r, true
}

// GET /v1/git/refs
func (c *GitController) ListRefs() {
	p := loadGitPolicy()
	r, ok := c.withRepo(p, c.ctx.Query("path"))
	if !ok {
		return
	}
	defer r.Close()
	prefixes := c.ctx.QueryArray("prefix")
	res := r.git(c.ctx.Request.Context(), p, nil, "for-each-ref", "--format=%(objectname) %(refname)")
	if res.err != nil {
		log.Printf("git for-each-ref failed: %s", res.stderr)
		c.RespondError(http.StatusInternalServerError, model.ErrorCodeRuntimeError, "git operation failed")
		return
	}
	refs := []gitRef{}
	for _, line := range strings.Split(strings.TrimSpace(res.stdout), "\n") {
		sha, name, ok := strings.Cut(line, " ")
		if !ok || !hasPrefix(p.outbound, name) {
			continue
		}
		if len(prefixes) > 0 && !hasPrefix(prefixes, name) {
			continue
		}
		refs = append(refs, gitRef{Name: name, SHA: sha})
	}
	out := gin.H{"refs": refs}
	if h := r.git(c.ctx.Request.Context(), p, nil, "symbolic-ref", "-q", "HEAD"); h.err == nil {
		if name := strings.TrimSpace(h.stdout); hasPrefix(p.outbound, name) {
			out["head"] = name
		}
	}
	c.RespondSuccess(out)
}

type bundleCreateRequest struct {
	Path  string   `json:"path"`
	Refs  []string `json:"refs"`
	Haves []string `json:"haves"`
}

func (c *GitController) busy(p gitPolicy, r *repo) bool {
	if !acquireGit(r.key, p.maxConc) {
		c.ctx.Header("Retry-After", "5")
		c.fail(gitErr(http.StatusTooManyRequests, codeBusy, "another transfer is in progress"))
		return true
	}
	return false
}

// POST /v1/git/bundle/create
func (c *GitController) CreateBundle() {
	p := loadGitPolicy()
	var req bundleCreateRequest
	if err := json.NewDecoder(io.LimitReader(c.ctx.Request.Body, 1<<20)).Decode(&req); err != nil {
		c.RespondError(http.StatusBadRequest, model.ErrorCodeInvalidRequest, "invalid JSON body")
		return
	}
	if len(req.Refs) == 0 || len(req.Refs) > 256 || len(req.Haves) > 4096 {
		c.RespondError(http.StatusBadRequest, model.ErrorCodeInvalidRequest, "refs must have 1..256 entries; haves at most 4096")
		return
	}
	for _, h := range req.Haves {
		if !gitObjectIDRe.MatchString(h) {
			c.RespondError(http.StatusBadRequest, model.ErrorCodeInvalidRequest, "invalid object id in haves")
			return
		}
	}
	r, ok := c.withRepo(p, req.Path)
	if !ok {
		return
	}
	defer r.Close()
	for _, ref := range req.Refs {
		if !validRefName(r, p, ref) {
			c.RespondError(http.StatusBadRequest, model.ErrorCodeInvalidRequest, "invalid ref name")
			return
		}
		if !hasPrefix(p.outbound, ref) {
			c.fail(gitErr(http.StatusForbidden, codeRefDenied, "ref not readable: %s", ref))
			return
		}
	}
	if c.busy(p, r) {
		return
	}
	defer releaseGit(r.key)
	ctx, cancel := context.WithTimeout(c.ctx.Request.Context(), p.timeout)
	defer cancel()

	// Drop haves the sandbox does not have: git refuses unknown revisions.
	var excludes []string
	if len(req.Haves) > 0 {
		var in strings.Builder
		for _, h := range req.Haves {
			in.WriteString(h + "^{commit}\n")
		}
		res := r.git(ctx, p, strings.NewReader(in.String()), "cat-file", "--batch-check")
		for i, line := range strings.Split(strings.TrimSpace(res.stdout), "\n") {
			if i < len(req.Haves) && !strings.HasSuffix(line, "missing") && line != "" {
				excludes = append(excludes, "^"+req.Haves[i])
			}
		}
	}

	stage, err := staging()
	if err != nil {
		c.RespondError(http.StatusInternalServerError, model.ErrorCodeRuntimeError, "cannot create staging file")
		return
	}
	defer stage.Close()
	gctx, gcancel := context.WithCancel(ctx)
	defer gcancel()
	cw := &cappedWriter{w: stage, left: p.maxBytes, cancel: gcancel}
	args := append([]string{"bundle", "create", "-"}, req.Refs...)
	args = append(args, excludes...)
	cmd, err := r.gitCmd(gctx, p, args...)
	if err != nil {
		c.RespondError(http.StatusInternalServerError, model.ErrorCodeRuntimeError, err.Error())
		return
	}
	var errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = cw, &errb
	runErr := cmd.Run()
	if cw.over {
		c.fail(gitErr(http.StatusRequestEntityTooLarge, codeTooLarge, "bundle exceeds the %d byte cap", p.maxBytes))
		return
	}
	if runErr != nil {
		if strings.Contains(errb.String(), "empty bundle") {
			c.ctx.Status(http.StatusNoContent)
			return
		}
		log.Printf("git bundle create failed: %v: %s", runErr, strings.TrimSpace(errb.String()))
		c.RespondError(http.StatusInternalServerError, model.ErrorCodeRuntimeError, "git operation failed")
		return
	}
	size, _ := stage.Seek(0, io.SeekEnd)
	_, _ = stage.Seek(0, io.SeekStart)
	c.ctx.Header("Content-Type", "application/x-git-bundle")
	c.ctx.Header("Content-Length", strconv.FormatInt(size, 10))
	c.ctx.Status(http.StatusOK)
	_, _ = io.Copy(c.ctx.Writer, stage)
}

type refMapping struct {
	src, dst, lease string
	force           bool
}

func parseMapping(s string) (refMapping, bool) {
	var m refMapping
	if strings.HasPrefix(s, "+") {
		m.force = true
		s = s[1:]
	}
	if i := strings.LastIndex(s, "@"); i >= 0 {
		m.lease = s[i+1:]
		s = s[:i]
		if !gitObjectIDRe.MatchString(m.lease) {
			return m, false
		}
	}
	var ok bool
	m.src, m.dst, ok = strings.Cut(s, ":")
	return m, ok
}

type gitRefUpdate struct {
	Ref string `json:"ref"`
	Old string `json:"old,omitempty"`
	New string `json:"new"`
}

// POST /v1/git/bundle/receive?path=&ref=[+]src:dst[@old]&init=true
func (c *GitController) ReceiveBundle() {
	p := loadGitPolicy()
	raw := c.ctx.Query("path")
	if len(c.ctx.Request.URL.RawQuery) > 32<<10 {
		c.RespondError(http.StatusRequestURITooLong, model.ErrorCodeInvalidRequest, "query too long")
		return
	}
	specs := c.ctx.QueryArray("ref")
	if len(specs) == 0 || len(specs) > 64 {
		c.RespondError(http.StatusBadRequest, model.ErrorCodeInvalidRequest, "ref must have 1..64 entries")
		return
	}
	var maps []refMapping
	for _, s := range specs {
		m, ok := parseMapping(s)
		if !ok || !(strings.HasPrefix(m.src, "refs/") || gitObjectIDRe.MatchString(m.src)) || !strings.HasPrefix(m.dst, "refs/") ||
			strings.ContainsAny(m.src+m.dst, "\x00\n \t") {
			c.RespondError(http.StatusBadRequest, model.ErrorCodeInvalidRequest, "ref must be [+]<src>:<dst>[@<old-sha>]")
			return
		}
		if !hasPrefix(p.inbound, m.dst) {
			c.fail(gitErr(http.StatusForbidden, codeRefDenied, "ref not writable: %s", m.dst))
			return
		}
		maps = append(maps, m)
	}
	if cl := c.ctx.Request.ContentLength; cl > p.maxBytes {
		c.fail(gitErr(http.StatusRequestEntityTooLarge, codeTooLarge, "bundle is %d bytes, over the %d byte cap", cl, p.maxBytes))
		return
	}

	var r *repo
	var gerr *gitError
	if c.ctx.Query("init") == "true" {
		if k, ok := initKey(raw); ok {
			if !tryLock(k) {
				c.ctx.Header("Retry-After", "5")
				c.fail(gitErr(http.StatusTooManyRequests, codeBusy, "another transfer is creating this repository"))
				return
			}
			defer unlock(k)
		}
		r, gerr = initRepo(p, raw, maps[0].dst)
	} else {
		r, gerr = openRepo(p, raw)
	}
	if gerr != nil {
		c.fail(gerr)
		return
	}
	defer r.Close()
	for _, m := range maps {
		if (!gitObjectIDRe.MatchString(m.src) && !validRefName(r, p, m.src)) || !validRefName(r, p, m.dst) {
			c.RespondError(http.StatusBadRequest, model.ErrorCodeInvalidRequest, "invalid ref name")
			return
		}
	}
	if c.busy(p, r) {
		return
	}
	defer releaseGit(r.key)
	ctx, cancel := context.WithTimeout(c.ctx.Request.Context(), p.timeout)
	defer cancel()

	stage, err := staging()
	if err != nil {
		c.RespondError(http.StatusInternalServerError, model.ErrorCodeRuntimeError, "cannot create staging file")
		return
	}
	defer stage.Close()
	body := &minRateReader{r: http.MaxBytesReader(c.ctx.Writer, c.ctx.Request.Body, p.maxBytes), start: time.Now(), min: p.minRate, window: p.minWin}
	if _, err := io.Copy(stage, body); err != nil {
		if errors.Is(err, errUploadStalled) {
			c.fail(gitErr(http.StatusRequestTimeout, codeUploadStalled, "upload rate fell below the minimum"))
			return
		}
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			c.fail(gitErr(http.StatusRequestEntityTooLarge, codeTooLarge, "bundle exceeds the size cap"))
			return
		}
		c.RespondError(http.StatusBadRequest, model.ErrorCodeInvalidRequest, "cannot read bundle body")
		return
	}
	_, _ = stage.Seek(0, io.SeekStart)

	// git bundle unbundle does not fsck, even with transfer.fsckObjects, so
	// execd reads the bundle header itself and imports the pack through
	// index-pack --strict.
	size, _ := stage.Seek(0, io.SeekEnd)
	_, _ = stage.Seek(0, io.SeekStart)
	var heads map[string]string
	var prereqs []string
	var herr error
	if size == 0 {
		// No bundle: every source must be an object the repository already has.
		heads = map[string]string{}
		var in strings.Builder
		for _, m := range maps {
			if !gitObjectIDRe.MatchString(m.src) {
				c.fail(gitErr(http.StatusUnprocessableEntity, codeBundleInvalid, "an empty body needs object IDs as sources"))
				return
			}
			heads[m.src] = m.src
			in.WriteString(m.src + "^{commit}\n")
		}
		if chk := r.git(ctx, p, strings.NewReader(in.String()), "cat-file", "--batch-check"); strings.Contains(chk.stdout, "missing") {
			c.fail(gitErr(http.StatusUnprocessableEntity, codePrereqMissing, "the repository does not have that object"))
			return
		}
		goto apply
	}
	heads, prereqs, herr = parseBundleHeader(stage)
	if herr != nil {
		c.fail(gitErr(http.StatusUnprocessableEntity, codeBundleInvalid, "bundle rejected: %s", herr.Error()))
		return
	}
	if len(prereqs) > 0 {
		var in strings.Builder
		for _, o := range prereqs {
			in.WriteString(o + "^{commit}\n")
		}
		chk := r.git(ctx, p, strings.NewReader(in.String()), "cat-file", "--batch-check")
		if strings.Contains(chk.stdout, "missing") {
			c.fail(gitErr(http.StatusUnprocessableEntity, codePrereqMissing, "the repository lacks commits the bundle needs"))
			return
		}
	}
	if res := r.git(ctx, p, stage, "index-pack", "--stdin", "--fix-thin", "--strict"); res.err != nil {
		if ctx.Err() != nil {
			c.fail(gitErr(http.StatusGatewayTimeout, codeTimeout, "the transfer exceeded the time limit"))
			return
		}
		log.Printf("index-pack rejected bundle: %s", res.stderr)
		c.fail(gitErr(http.StatusUnprocessableEntity, codeBundleInvalid, "bundle rejected: objects failed verification"))
		return
	}

apply:
	checkedOut := map[string]bool{}
	if wt := r.git(ctx, p, nil, "worktree", "list", "--porcelain"); wt.err == nil {
		for _, line := range strings.Split(wt.stdout, "\n") {
			if ref, ok := strings.CutPrefix(line, "branch "); ok {
				checkedOut[ref] = true
			}
		}
	}

	var tx strings.Builder
	tx.WriteString("start\n")
	var updated []gitRefUpdate
	for _, m := range maps {
		newSHA, ok := heads[m.src]
		if !ok {
			c.fail(gitErr(http.StatusUnprocessableEntity, codeBundleInvalid, "bundle does not contain %s", m.src))
			return
		}
		old := zeroOID
		if o := r.git(ctx, p, nil, "rev-parse", "-q", "--verify", m.dst); o.err == nil {
			old = strings.TrimSpace(o.stdout)
		}
		if checkedOut[m.dst] && old != zeroOID {
			c.fail(gitErr(http.StatusConflict, codeCheckedOut, "ref is checked out: %s", m.dst))
			return
		}
		if m.lease != "" && m.lease != old {
			c.fail(gitErr(http.StatusConflict, codeStaleLease, "%s is at %s, expected %s", m.dst, old, m.lease))
			return
		}
		if old != zeroOID && !m.force {
			if a := r.git(ctx, p, nil, "merge-base", "--is-ancestor", old, newSHA); a.err != nil {
				c.fail(gitErr(http.StatusConflict, codeNotFF, "%s is not a fast-forward", m.dst))
				return
			}
		}
		fmt.Fprintf(&tx, "update %s %s %s\n", m.dst, newSHA, old)
		u := gitRefUpdate{Ref: m.dst, New: newSHA}
		if old != zeroOID {
			u.Old = old
		}
		updated = append(updated, u)
	}
	tx.WriteString("prepare\ncommit\n")
	if testBeforeTransaction != nil {
		testBeforeTransaction()
	}
	if up := r.git(ctx, p, strings.NewReader(tx.String()), "update-ref", "--stdin"); up.err != nil {
		if ctx.Err() != nil {
			c.fail(gitErr(http.StatusGatewayTimeout, codeTimeout, "the transfer exceeded the time limit"))
			return
		}
		switch {
		case strings.Contains(up.stderr, "but expected") || strings.Contains(up.stderr, "reference already exists"):
			log.Printf("update-ref stale: %s", up.stderr)
			code := codeNotFF
			for _, m := range maps {
				if m.lease != "" {
					code = codeStaleLease
				}
			}
			c.fail(gitErr(http.StatusConflict, code, "a ref moved during the update"))
		case strings.Contains(up.stderr, ".lock") || strings.Contains(up.stderr, "cannot lock"):
			log.Printf("update-ref lock failure: %s", up.stderr)
			c.fail(gitErr(http.StatusConflict, codeRefLocked, "a ref is locked"))
		case strings.Contains(up.stderr, "expected"):
			log.Printf("update-ref stale: %s", up.stderr)
			code := codeNotFF
			for _, m := range maps {
				if m.lease != "" {
					code = codeStaleLease
				}
			}
			c.fail(gitErr(http.StatusConflict, code, "a ref moved during the update"))
		default:
			log.Printf("update-ref failed: %s", up.stderr)
			c.RespondError(http.StatusInternalServerError, model.ErrorCodeRuntimeError, "git operation failed")
		}
		return
	}
	c.RespondSuccess(gin.H{"updated": updated})
}

// initRepo creates an empty repository for the first push.
func initRepo(p gitPolicy, raw, firstDst string) (*repo, *gitError) {
	if p.repoRoot == "" {
		return nil, gitErr(http.StatusNotImplemented, model.ErrorCodeNotSupported, "git transport is not enabled")
	}
	expanded, err := pathutil.ExpandPath(raw)
	if err != nil || raw == "" {
		return nil, gitErr(http.StatusBadRequest, model.ErrorCodeInvalidRequest, "invalid path")
	}
	abs, _ := filepath.Abs(expanded)
	rootPath, err := filepath.EvalSymlinks(p.repoRoot)
	if err != nil {
		return nil, gitErr(http.StatusInternalServerError, model.ErrorCodeRuntimeError, "git repo root unavailable")
	}
	rel, err := filepath.Rel(rootPath, abs)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return nil, gitErr(http.StatusForbidden, codePathDenied, "init target must be beneath the root")
	}
	root, err := os.OpenRoot(rootPath)
	if err != nil {
		return nil, gitErr(http.StatusInternalServerError, model.ErrorCodeRuntimeError, "git repo root unavailable")
	}
	defer root.Close()
	parent := filepath.Dir(rel)
	cur := ""
	if parent != "." {
		for _, part := range strings.Split(parent, string(filepath.Separator)) {
			cur = filepath.Join(cur, part)
			fi, err := root.Lstat(cur)
			if err != nil || isSymlink(fi) {
				return nil, gitErr(http.StatusForbidden, codePathDenied, "symlinks are not allowed in repository path")
			}
		}
	}
	rfi, err := root.Lstat(".")
	if err != nil {
		return nil, gitErr(http.StatusInternalServerError, model.ErrorCodeRuntimeError, "cannot stat root")
	}
	uid, gid, _, _ := statIDs(rfi)
	if p.runAs != "" {
		a, b, _ := strings.Cut(p.runAs, ":")
		u, _ := strconv.ParseUint(a, 10, 32)
		g, _ := strconv.ParseUint(b, 10, 32)
		uid, gid = uint32(u), uint32(g)
	}
	if fi, err := root.Lstat(rel); err == nil {
		if !fi.IsDir() || isSymlink(fi) {
			return nil, gitErr(http.StatusConflict, codeInitNotEmpty, "init target is not an empty directory")
		}
		d, err := root.Open(rel)
		if err != nil {
			return nil, gitErr(http.StatusForbidden, codePathDenied, "cannot open init target")
		}
		names, _ := d.Readdirnames(1)
		_ = d.Close()
		if len(names) > 0 {
			return nil, gitErr(http.StatusConflict, codeInitNotEmpty, "init target is not an empty directory")
		}
	} else if err := root.Mkdir(rel, 0o755); err != nil {
		return nil, gitErr(http.StatusInternalServerError, model.ErrorCodeRuntimeError, "cannot create repository directory")
	}
	if os.Geteuid() == 0 {
		if err := root.Lchown(rel, int(uid), int(gid)); err != nil {
			return nil, gitErr(http.StatusInternalServerError, model.ErrorCodeRuntimeError, "cannot prepare repository directory")
		}
	}
	dir, err := root.OpenRoot(rel)
	if err != nil {
		return nil, gitErr(http.StatusForbidden, codePathDenied, "cannot open init target")
	}
	defer dir.Close()
	df, err := dir.Open(".")
	if err != nil {
		return nil, gitErr(http.StatusInternalServerError, model.ErrorCodeRuntimeError, "cannot open init target")
	}
	defer df.Close()
	branch := "main"
	if b, ok := strings.CutPrefix(firstDst, "refs/heads/"); ok {
		branch = b
	}
	bin := os.Getenv(gitBinEnv)
	if bin == "" {
		bin = "git"
	}
	cmd := exec.Command(bin, "-C", "/proc/self/fd/3", "init", "-q", "--initial-branch="+branch, "--object-format=sha1")
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=/nonexistent", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "LC_ALL=C"}
	cmd.ExtraFiles = []*os.File{df}
	if os.Geteuid() == 0 && uid != 0 {
		cmd.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: uid, Gid: gid}}
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		return nil, gitErr(http.StatusInternalServerError, model.ErrorCodeRuntimeError, "git init: %s", strings.TrimSpace(string(out)))
	}
	return openRepo(p, raw)
}

// GET /v1/git/capabilities
func (c *GitController) Capabilities() {
	p := loadGitPolicy()
	if p.repoRoot == "" {
		c.RespondSuccess(gin.H{"enabled": false, "reason": "repo_root is not configured"})
		return
	}
	bin := os.Getenv(gitBinEnv)
	if bin == "" {
		bin = "git"
	}
	if why := gitUnusable(); why != "" {
		c.RespondSuccess(gin.H{"enabled": false, "reason": why})
		return
	}
	gp, _ := exec.LookPath(bin)
	ver, _ := probeGit(gp, "--version")
	if p.runAs != "" && os.Geteuid() != 0 {
		if a, _, _ := strings.Cut(p.runAs, ":"); a != strconv.Itoa(os.Geteuid()) {
			c.RespondSuccess(gin.H{"enabled": false, "reason": "execd cannot switch to the run_as identity"})
			return
		}
	}
	c.RespondSuccess(gin.H{
		"enabled": true, "version": 1, "repo_root": p.repoRoot, "object_format": "sha1",
		"git_version": strings.TrimSpace(string(ver)), "max_bundle_bytes": p.maxBytes,
		"inbound_prefixes": p.inbound, "outbound_prefixes": p.outbound,
	})
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// parseBundleHeader reads a v2 or v3 bundle header from f and leaves f
// positioned at the start of the pack.
func parseBundleHeader(f *os.File) (heads map[string]string, prereqs []string, err error) {
	_, _ = f.Seek(0, io.SeekStart)
	br := bufio.NewReaderSize(io.LimitReader(f, 1<<20), 64<<10)
	read := int64(0)
	line := func() (string, error) {
		l, e := br.ReadString('\n')
		read += int64(len(l))
		return strings.TrimSuffix(l, "\n"), e
	}
	sig, e := line()
	if e != nil || (sig != "# v2 git bundle" && sig != "# v3 git bundle") {
		return nil, nil, errors.New("not a git bundle")
	}
	heads = map[string]string{}
	for i := 0; i < 8192; i++ {
		l, e := line()
		if e != nil {
			return nil, nil, errors.New("truncated bundle header")
		}
		switch {
		case l == "":
			_, _ = f.Seek(read, io.SeekStart)
			return heads, prereqs, nil
		case strings.HasPrefix(l, "@"):
			if l != "@object-format=sha1" {
				return nil, nil, errors.New("unsupported bundle capability: " + l)
			}
		case strings.HasPrefix(l, "-"):
			oid, _, _ := strings.Cut(l[1:], " ")
			if !gitObjectIDRe.MatchString(oid) {
				return nil, nil, errors.New("invalid prerequisite")
			}
			prereqs = append(prereqs, oid)
		default:
			oid, name, ok := strings.Cut(l, " ")
			if !ok || !gitObjectIDRe.MatchString(oid) {
				return nil, nil, errors.New("invalid ref line in bundle header")
			}
			if strings.HasPrefix(name, "refs/") { // a bundle may also carry HEAD
				heads[name] = oid
			}
		}
	}
	return nil, nil, errors.New("bundle header too long")
}

// testBeforeTransaction lets a test move a ref between execd's checks and the
// ref transaction, standing in for an agent running its own git.
var testBeforeTransaction func()

// tryLock takes a key without counting against the global slots.
func tryLock(key [2]uint64) bool {
	gitLockMu.Lock()
	defer gitLockMu.Unlock()
	if gitLocks[key] {
		return false
	}
	gitLocks[key] = true
	return true
}

func unlock(key [2]uint64) {
	gitLockMu.Lock()
	defer gitLockMu.Unlock()
	delete(gitLocks, key)
}

// initKey identifies a not-yet-existing target by its parent's device and
// inode plus the final path component.
func initKey(raw string) ([2]uint64, bool) {
	expanded, err := pathutil.ExpandPath(raw)
	if err != nil {
		return [2]uint64{}, false
	}
	abs, err := filepath.Abs(expanded)
	if err != nil {
		return [2]uint64{}, false
	}
	fi, err := os.Stat(filepath.Dir(abs))
	if err != nil {
		return [2]uint64{}, false
	}
	_, _, dev, ino := statIDs(fi)
	h := uint64(14695981039346656037)
	for _, b := range []byte(filepath.Base(abs)) {
		h = (h ^ uint64(b)) * 1099511628211
	}
	return [2]uint64{dev, ino ^ h}, true
}

// minRateReader ends an upload whose average rate stays under the minimum
// once the window has elapsed.
type minRateReader struct {
	r      io.Reader
	start  time.Time
	n      int64
	min    int64
	window time.Duration
}

var errUploadStalled = errors.New("upload too slow")

func (m *minRateReader) Read(b []byte) (int, error) {
	n, err := m.r.Read(b)
	m.n += int64(n)
	if el := time.Since(m.start); el >= m.window && err == nil && float64(m.n)/el.Seconds() < float64(m.min) {
		return n, errUploadStalled
	}
	return n, err
}

// gitBinaryUnsafe reports why the git binary cannot be trusted: it, its exec
// path, or any parent must not be writable by anyone but root.
func gitBinaryUnsafe() string {
	if os.Geteuid() != 0 {
		return ""
	}
	bin := os.Getenv(gitBinEnv)
	if bin == "" {
		bin = "git"
	}
	path, err := exec.LookPath(bin)
	if err != nil {
		return "git binary not found"
	}
	// The binary is checked before it is ever executed as root.
	if why := writableChain(path); why != "" {
		return why
	}
	if out, err := probeGit(path, "--exec-path"); err == nil {
		return writableChain(strings.TrimSpace(string(out)))
	}
	return ""
}

func writableChain(p string) string {
	if real, err := filepath.EvalSymlinks(p); err == nil {
		p = real
	}
	for cur := p; ; cur = filepath.Dir(cur) {
		fi, err := os.Stat(cur)
		if err != nil {
			return ""
		}
		uid, _, _, _ := statIDs(fi)
		if uid != 0 || fi.Mode().Perm()&0o022 != 0 {
			return "git binary path is writable by a non-root user: " + cur
		}
		if cur == "/" || cur == "." {
			return ""
		}
	}
}

// probeGit runs the already-verified binary with a scrubbed environment, so
// execd's own variables (including its access token) never reach it.
func probeGit(path string, args ...string) ([]byte, error) {
	cmd := exec.Command(path, args...)
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=/nonexistent", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "LC_ALL=C"}
	return cmd.Output()
}

const minGitMajor, minGitMinor = 2, 34

var gitVersionRe = regexp.MustCompile(`git version (\d+)\.(\d+)`)

// gitTooOld reports why a version string is below the minimum, or "".
func gitTooOld(version string) string {
	m := gitVersionRe.FindStringSubmatch(version)
	if m == nil {
		return "cannot parse the git version"
	}
	major, _ := strconv.Atoi(m[1])
	minor, _ := strconv.Atoi(m[2])
	if major < minGitMajor || (major == minGitMajor && minor < minGitMinor) {
		return fmt.Sprintf("git %d.%d is older than the supported minimum %d.%d", major, minor, minGitMajor, minGitMinor)
	}
	return ""
}

// gitUnusable combines the binary checks: safe path first, then version.
func gitUnusable() string {
	if why := gitBinaryUnsafe(); why != "" {
		return why
	}
	bin := os.Getenv(gitBinEnv)
	if bin == "" {
		bin = "git"
	}
	path, err := exec.LookPath(bin)
	if err != nil {
		return "git binary not found"
	}
	out, err := probeGit(path, "--version")
	if err != nil {
		return "git binary not found"
	}
	return gitTooOld(string(out))
}
