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

//go:build !windows

package controller

// PROTOTYPE (OSEP "Sandbox Git Transport"): throwaway implementation used to
// harden the proposal. Not production code.

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"

	"github.com/bmatcuk/doublestar/v4"
	"github.com/gin-gonic/gin"

	"github.com/alibaba/opensandbox/execd/pkg/util/pathutil"
	"github.com/alibaba/opensandbox/execd/pkg/web/model"
)

const (
	gitRepoRootEnv      = "EXECD_GIT_REPO_ROOT"
	gitReadRefsEnv      = "EXECD_GIT_READ_REFS"
	gitWriteRefsEnv     = "EXECD_GIT_WRITE_REFS"
	gitMaxBundleEnv     = "EXECD_GIT_MAX_BUNDLE_BYTES"
	gitDefaultReadRefs  = "refs/heads/**,refs/tags/**"
	gitDefaultWriteRefs = "refs/heads/**"
	gitDefaultMaxBytes  = int64(256 << 20)
)

var gitObjectIDRe = regexp.MustCompile(`^[0-9a-f]{40}([0-9a-f]{24})?$`)

// gitPolicy is resolved from the sandbox environment on every request. The
// caller can only narrow it; it never widens it.
type gitPolicy struct {
	repoRoot  string
	readRefs  []string
	writeRefs []string
	maxBytes  int64
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
		repoRoot:  os.Getenv(gitRepoRootEnv),
		readRefs:  split(os.Getenv(gitReadRefsEnv), gitDefaultReadRefs),
		writeRefs: split(os.Getenv(gitWriteRefsEnv), gitDefaultWriteRefs),
		maxBytes:  gitDefaultMaxBytes,
	}
	if v, err := strconv.ParseInt(os.Getenv(gitMaxBundleEnv), 10, 64); err == nil && v > 0 {
		p.maxBytes = v
	}
	return p
}

func refAllowed(patterns []string, ref string) bool {
	for _, p := range patterns {
		if ok, _ := doublestar.Match(p, ref); ok {
			return true
		}
	}
	return false
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

func (c *GitController) fail(err *gitError) { c.RespondError(err.status, err.code, err.message) }

// resolveRepo returns a repository directory that lies under the configured
// root with no symlink in any component, and whose .git is a real directory.
// The feature is off (404-style NOT_SUPPORTED) until a root is configured.
func (c *GitController) resolveRepo(p gitPolicy, raw string) (string, *gitError) {
	if p.repoRoot == "" {
		return "", gitErr(http.StatusNotImplemented, model.ErrorCodeNotSupported, "git transport is not enabled")
	}
	if raw == "" {
		return "", gitErr(http.StatusBadRequest, model.ErrorCodeMissingQuery, "missing 'path'")
	}
	expanded, err := pathutil.ExpandPath(raw)
	if err != nil {
		return "", gitErr(http.StatusBadRequest, model.ErrorCodeInvalidRequest, "invalid path")
	}
	abs, err := filepath.Abs(expanded)
	if err != nil {
		return "", gitErr(http.StatusBadRequest, model.ErrorCodeInvalidRequest, "invalid path")
	}
	root, err := filepath.EvalSymlinks(p.repoRoot)
	if err != nil {
		return "", gitErr(http.StatusInternalServerError, model.ErrorCodeRuntimeError, "git repo root unavailable")
	}
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", gitErr(http.StatusNotFound, model.ErrorCodeFileNotFound, "repository not found")
	}
	// Requiring the resolved path to equal the cleaned input rejects any
	// symlink in the path, not just one that escapes the root.
	if real != filepath.Clean(abs) {
		return "", gitErr(http.StatusForbidden, model.ErrorCodeInvalidRequest, "symlinks are not allowed in repository path")
	}
	if real != root && !strings.HasPrefix(real, root+string(filepath.Separator)) {
		return "", gitErr(http.StatusForbidden, model.ErrorCodeInvalidRequest, "repository is outside the configured root")
	}
	fi, err := os.Lstat(filepath.Join(real, ".git"))
	if err != nil || !fi.IsDir() {
		return "", gitErr(http.StatusNotFound, model.ErrorCodeFileNotFound, "not a git repository")
	}
	return real, nil
}

// initRepo creates an empty repository at raw for the first push into a
// sandbox. The target must be absent or an empty directory, its parent must be
// a symlink-free path under the root, and the new directory is handed to the
// owner of the root so the agent can use it.
func (c *GitController) initRepo(p gitPolicy, raw string) (string, *gitError) {
	if p.repoRoot == "" {
		return "", gitErr(http.StatusNotImplemented, model.ErrorCodeNotSupported, "git transport is not enabled")
	}
	expanded, err := pathutil.ExpandPath(raw)
	if err != nil || raw == "" {
		return "", gitErr(http.StatusBadRequest, model.ErrorCodeInvalidRequest, "invalid path")
	}
	abs, err := filepath.Abs(expanded)
	if err != nil {
		return "", gitErr(http.StatusBadRequest, model.ErrorCodeInvalidRequest, "invalid path")
	}
	root, err := filepath.EvalSymlinks(p.repoRoot)
	if err != nil {
		return "", gitErr(http.StatusInternalServerError, model.ErrorCodeRuntimeError, "git repo root unavailable")
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(abs))
	if err != nil || parent != filepath.Dir(abs) {
		return "", gitErr(http.StatusForbidden, model.ErrorCodeInvalidRequest, "symlinks are not allowed in repository path")
	}
	if abs == root || !strings.HasPrefix(abs, root+string(filepath.Separator)) {
		return "", gitErr(http.StatusForbidden, model.ErrorCodeInvalidRequest, "repository is outside the configured root")
	}
	if fi, err := os.Lstat(abs); err == nil {
		entries, rerr := os.ReadDir(abs)
		if !fi.IsDir() || rerr != nil || len(entries) > 0 {
			return "", gitErr(http.StatusConflict, model.ErrorCodeInvalidRequest, "init target exists and is not an empty directory")
		}
	} else if err := os.Mkdir(abs, 0o755); err != nil {
		return "", gitErr(http.StatusInternalServerError, model.ErrorCodeRuntimeError, "cannot create repository directory")
	}
	if err := chownToRepoOwner(abs, root); err != nil {
		return "", gitErr(http.StatusInternalServerError, model.ErrorCodeRuntimeError, "cannot prepare repository directory")
	}
	if _, err := runGit(abs, "init", "-q"); err != nil {
		return "", gitErr(http.StatusInternalServerError, model.ErrorCodeRuntimeError, "%s", err.Error())
	}
	return abs, nil
}

// gitCmd builds a git invocation that ignores ambient and repo-local
// behaviour that could execute code (hooks, fsmonitor, gc, helpers). When
// execd runs as root, git drops to the repository owner.
func gitCmd(repo string, args ...string) (*exec.Cmd, error) {
	full := append([]string{
		"-C", repo,
		"-c", "core.hooksPath=/dev/null",
		"-c", "core.fsmonitor=false",
		"-c", "gc.auto=0",
		"-c", "maintenance.auto=false",
		"-c", "protocol.allow=never",
		"-c", "protocol.file.allow=always", // a bundle path is a file transport; the path is always execd-generated
		"-c", "transfer.fsckObjects=true",
		"-c", "uploadpack.allowFilter=false",
	}, args...)
	cmd := exec.Command("git", full...)
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=/nonexistent",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_TERMINAL_PROMPT=0",
		"GIT_NO_REPLACE_OBJECTS=1",
		"LC_ALL=C",
	}
	if err := dropToOwner(cmd, repo); err != nil {
		return nil, err
	}
	return cmd, nil
}

func dropToOwner(cmd *exec.Cmd, repo string) error {
	if os.Geteuid() != 0 {
		return nil
	}
	fi, err := os.Stat(repo)
	if err != nil {
		return err
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok || st.Uid == 0 {
		return nil
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: st.Uid, Gid: st.Gid}}
	return nil
}

func runGit(repo string, args ...string) (string, error) {
	cmd, err := gitCmd(repo, args...)
	if err != nil {
		return "", err
	}
	var out, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &stderr
	if err := cmd.Run(); err != nil {
		return out.String(), fmt.Errorf("git %s: %w: %s", args[0], err, strings.TrimSpace(stderr.String()))
	}
	return out.String(), nil
}

type gitRef struct {
	Name string `json:"name"`
	SHA  string `json:"sha"`
}

type gitRefsResponse struct {
	Head string   `json:"head,omitempty"`
	Refs []gitRef `json:"refs"`
}

// GET /git/refs?path=&prefix=
func (c *GitController) ListRefs() {
	p := loadGitPolicy()
	repo, gerr := c.resolveRepo(p, c.ctx.Query("path"))
	if gerr != nil {
		c.fail(gerr)
		return
	}
	prefixes := c.ctx.QueryArray("prefix")
	out, err := runGit(repo, "for-each-ref", "--format=%(objectname) %(refname)")
	if err != nil {
		c.RespondError(http.StatusInternalServerError, model.ErrorCodeRuntimeError, err.Error())
		return
	}
	resp := gitRefsResponse{Refs: []gitRef{}}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		sha, name, ok := strings.Cut(line, " ")
		if !ok || !refAllowed(p.readRefs, name) {
			continue
		}
		if len(prefixes) > 0 && !hasAnyPrefix(name, prefixes) {
			continue
		}
		resp.Refs = append(resp.Refs, gitRef{Name: name, SHA: sha})
	}
	if head, err := runGit(repo, "symbolic-ref", "-q", "HEAD"); err == nil {
		if h := strings.TrimSpace(head); refAllowed(p.readRefs, h) {
			resp.Head = h
		}
	}
	c.RespondSuccess(resp)
}

func hasAnyPrefix(s string, prefixes []string) bool {
	for _, p := range prefixes {
		if strings.HasPrefix(s, p) {
			return true
		}
	}
	return false
}

func validRefName(repo, ref string) bool {
	if !strings.HasPrefix(ref, "refs/") || strings.ContainsAny(ref, "\x00\n ") {
		return false
	}
	_, err := runGit(repo, "check-ref-format", ref)
	return err == nil
}

type bundleCreateRequest struct {
	Path  string   `json:"path"`
	Refs  []string `json:"refs"`
	Haves []string `json:"haves"`
}

// POST /git/bundle/create
func (c *GitController) CreateBundle() {
	p := loadGitPolicy()
	var req bundleCreateRequest
	if err := json.NewDecoder(io.LimitReader(c.ctx.Request.Body, 1<<20)).Decode(&req); err != nil {
		c.RespondError(http.StatusBadRequest, model.ErrorCodeInvalidRequest, "invalid JSON body")
		return
	}
	repo, gerr := c.resolveRepo(p, req.Path)
	if gerr != nil {
		c.fail(gerr)
		return
	}
	if len(req.Refs) == 0 || len(req.Refs) > 256 || len(req.Haves) > 4096 {
		c.RespondError(http.StatusBadRequest, model.ErrorCodeInvalidRequest, "refs must have 1..256 entries; haves at most 4096")
		return
	}
	for _, r := range req.Refs {
		if !validRefName(repo, r) {
			c.RespondError(http.StatusBadRequest, model.ErrorCodeInvalidRequest, "invalid ref name")
			return
		}
		if !refAllowed(p.readRefs, r) {
			c.RespondError(http.StatusForbidden, model.ErrorCodeInvalidRequest, "ref not readable: "+r)
			return
		}
	}
	// Drop haves the sandbox does not have: git refuses unknown revisions.
	var excludes []string
	for _, h := range req.Haves {
		if !gitObjectIDRe.MatchString(h) {
			c.RespondError(http.StatusBadRequest, model.ErrorCodeInvalidRequest, "invalid object id in haves")
			return
		}
		if _, err := runGit(repo, "cat-file", "-e", h+"^{commit}"); err == nil {
			excludes = append(excludes, "^"+h)
		}
	}

	tmp, err := os.MkdirTemp("", "execd-git-")
	if err != nil {
		c.RespondError(http.StatusInternalServerError, model.ErrorCodeRuntimeError, "cannot create staging directory")
		return
	}
	defer os.RemoveAll(tmp)
	if err := chownToRepoOwner(tmp, repo); err != nil {
		c.RespondError(http.StatusInternalServerError, model.ErrorCodeRuntimeError, "cannot prepare staging directory")
		return
	}
	file := filepath.Join(tmp, "out.bundle")

	args := append([]string{"bundle", "create", file}, req.Refs...)
	args = append(args, excludes...)
	if _, err := runGit(repo, args...); err != nil {
		if strings.Contains(err.Error(), "empty bundle") {
			c.ctx.Status(http.StatusNoContent)
			return
		}
		c.RespondError(http.StatusInternalServerError, model.ErrorCodeRuntimeError, err.Error())
		return
	}
	f, err := os.Open(file)
	if err != nil {
		c.RespondError(http.StatusInternalServerError, model.ErrorCodeRuntimeError, "cannot read bundle")
		return
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil || !fi.Mode().IsRegular() {
		c.RespondError(http.StatusInternalServerError, model.ErrorCodeRuntimeError, "bundle is not a regular file")
		return
	}
	if fi.Size() > p.maxBytes {
		c.RespondError(http.StatusRequestEntityTooLarge, model.ErrorCodeInvalidRequest,
			fmt.Sprintf("bundle is %d bytes, over the %d byte cap", fi.Size(), p.maxBytes))
		return
	}
	c.ctx.Header("Content-Type", "application/x-git-bundle")
	c.ctx.Header("Content-Length", strconv.FormatInt(fi.Size(), 10))
	c.ctx.Status(http.StatusOK)
	_, _ = io.Copy(c.ctx.Writer, f)
}

type gitRefUpdate struct {
	Ref string `json:"ref"`
	Old string `json:"old,omitempty"`
	New string `json:"new"`
}

type bundleReceiveResponse struct {
	Updated []gitRefUpdate `json:"updated"`
}

// POST /git/bundle/receive?path=&ref=<src>:<dst>[&force=true]; body is the bundle.
func (c *GitController) ReceiveBundle() {
	p := loadGitPolicy()
	var repo string
	var gerr *gitError
	if c.ctx.Query("init") == "true" {
		repo, gerr = c.initRepo(p, c.ctx.Query("path"))
	} else {
		repo, gerr = c.resolveRepo(p, c.ctx.Query("path"))
	}
	if gerr != nil {
		c.fail(gerr)
		return
	}
	specs := c.ctx.QueryArray("ref")
	if len(specs) == 0 || len(specs) > 256 {
		c.RespondError(http.StatusBadRequest, model.ErrorCodeInvalidRequest, "ref must have 1..256 entries")
		return
	}
	force := c.ctx.Query("force") == "true"
	head, _ := runGit(repo, "symbolic-ref", "-q", "HEAD")
	head = strings.TrimSpace(head)
	if _, err := runGit(repo, "rev-parse", "-q", "--verify", "HEAD"); err != nil {
		head = "" // unborn HEAD: nothing is checked out yet, so any branch may be written
	}

	type mapping struct{ src, dst string }
	var maps []mapping
	for _, s := range specs {
		src, dst, ok := strings.Cut(s, ":")
		if !ok || !validRefName(repo, src) || !validRefName(repo, dst) {
			c.RespondError(http.StatusBadRequest, model.ErrorCodeInvalidRequest, "ref must be <src>:<dst> with valid ref names")
			return
		}
		if !refAllowed(p.writeRefs, dst) {
			c.RespondError(http.StatusForbidden, model.ErrorCodeInvalidRequest, "ref not writable: "+dst)
			return
		}
		if dst == head {
			c.RespondError(http.StatusConflict, model.ErrorCodeInvalidRequest, "ref is the checked-out branch: "+dst)
			return
		}
		maps = append(maps, mapping{src, dst})
	}

	if cl := c.ctx.Request.ContentLength; cl > p.maxBytes {
		c.RespondError(http.StatusRequestEntityTooLarge, model.ErrorCodeInvalidRequest,
			fmt.Sprintf("bundle is %d bytes, over the %d byte cap", cl, p.maxBytes))
		return
	}
	tmp, err := os.MkdirTemp("", "execd-git-")
	if err != nil {
		c.RespondError(http.StatusInternalServerError, model.ErrorCodeRuntimeError, "cannot create staging directory")
		return
	}
	defer os.RemoveAll(tmp)
	if err := chownToRepoOwner(tmp, repo); err != nil {
		c.RespondError(http.StatusInternalServerError, model.ErrorCodeRuntimeError, "cannot prepare staging directory")
		return
	}
	file := filepath.Join(tmp, "in.bundle")
	f, err := os.OpenFile(file, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		c.RespondError(http.StatusInternalServerError, model.ErrorCodeRuntimeError, "cannot stage bundle")
		return
	}
	body := http.MaxBytesReader(c.ctx.Writer, c.ctx.Request.Body, p.maxBytes)
	_, copyErr := io.Copy(f, body)
	_ = f.Close()
	if copyErr != nil {
		var mbe *http.MaxBytesError
		if errors.As(copyErr, &mbe) {
			c.RespondError(http.StatusRequestEntityTooLarge, model.ErrorCodeInvalidRequest, "bundle exceeds the size cap")
			return
		}
		c.RespondError(http.StatusBadRequest, model.ErrorCodeInvalidRequest, "cannot read bundle body")
		return
	}
	_ = chownToRepoOwner(file, repo)

	if _, err := runGit(repo, "bundle", "verify", file); err != nil {
		c.RespondError(http.StatusUnprocessableEntity, model.ErrorCodeInvalidRequest, "bundle verification failed: "+err.Error())
		return
	}
	heads, err := runGit(repo, "bundle", "list-heads", file)
	if err != nil {
		c.RespondError(http.StatusUnprocessableEntity, model.ErrorCodeInvalidRequest, err.Error())
		return
	}
	inBundle := map[string]bool{}
	for _, line := range strings.Split(strings.TrimSpace(heads), "\n") {
		if _, name, ok := strings.Cut(line, " "); ok {
			inBundle[name] = true
		}
	}

	args := []string{"fetch", "--no-tags", "--no-write-fetch-head", "--no-recurse-submodules", file}
	var resp bundleReceiveResponse
	olds := map[string]string{}
	for _, m := range maps {
		if !inBundle[m.src] {
			c.RespondError(http.StatusUnprocessableEntity, model.ErrorCodeInvalidRequest, "bundle does not contain "+m.src)
			return
		}
		if out, err := runGit(repo, "rev-parse", "-q", "--verify", m.dst); err == nil {
			olds[m.dst] = strings.TrimSpace(out)
		}
		spec := m.src + ":" + m.dst
		if force {
			spec = "+" + spec
		}
		args = append(args, spec)
	}
	if _, err := runGit(repo, args...); err != nil {
		status := http.StatusInternalServerError
		if strings.Contains(err.Error(), "non-fast-forward") || strings.Contains(err.Error(), "rejected") {
			status = http.StatusConflict
		}
		c.RespondError(status, model.ErrorCodeRuntimeError, err.Error())
		return
	}
	for _, m := range maps {
		out, err := runGit(repo, "rev-parse", "--verify", m.dst)
		if err != nil {
			c.RespondError(http.StatusInternalServerError, model.ErrorCodeRuntimeError, err.Error())
			return
		}
		resp.Updated = append(resp.Updated, gitRefUpdate{Ref: m.dst, Old: olds[m.dst], New: strings.TrimSpace(out)})
	}
	c.RespondSuccess(resp)
}

// GET /git/capabilities
func (c *GitController) Capabilities() {
	p := loadGitPolicy()
	if p.repoRoot == "" {
		c.RespondSuccess(gin.H{"enabled": false})
		return
	}
	ver, err := exec.Command("git", "--version").Output()
	if err != nil {
		c.RespondSuccess(gin.H{"enabled": false})
		return
	}
	c.RespondSuccess(gin.H{
		"enabled":          true,
		"git_version":      strings.TrimSpace(string(ver)),
		"max_bundle_bytes": p.maxBytes,
		"read_refs":        p.readRefs,
		"write_refs":       p.writeRefs,
	})
}
