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

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func gitRouter() *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.GET("/git/capabilities", func(c *gin.Context) { NewGitController(c).Capabilities() })
	r.GET("/git/refs", func(c *gin.Context) { NewGitController(c).ListRefs() })
	r.POST("/git/bundle/create", func(c *gin.Context) { NewGitController(c).CreateBundle() })
	r.POST("/git/bundle/receive", func(c *gin.Context) { NewGitController(c).ReceiveBundle() })
	return r
}

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=t", "-c", "user.email=t@t", "-c", "commit.gpgsign=false"}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, string(out))
	return strings.TrimSpace(string(out))
}

func newRepo(t *testing.T, root, name string) string {
	t.Helper()
	dir := filepath.Join(root, name)
	require.NoError(t, os.MkdirAll(dir, 0o755))
	git(t, dir, "init", "-q", "-b", "main")
	require.NoError(t, os.WriteFile(filepath.Join(dir, "a.txt"), []byte("a\n"), 0o644))
	git(t, dir, "add", ".")
	git(t, dir, "commit", "-q", "-m", "one")
	return dir
}

func do(r *gin.Engine, method, url string, body []byte) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, url, bytes.NewReader(body))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func gitEnv(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not found in PATH")
	}
	root, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	t.Setenv(gitRepoRootEnv, root)
	t.Setenv(gitReadRefsEnv, "")
	t.Setenv(gitWriteRefsEnv, "")
	t.Setenv(gitMaxBundleEnv, "")
	return root
}

func TestGitDisabledWithoutRoot(t *testing.T) {
	t.Setenv(gitRepoRootEnv, "")
	w := do(gitRouter(), http.MethodGet, "/git/refs?path=/tmp", nil)
	require.Equal(t, http.StatusNotImplemented, w.Code)
	w = do(gitRouter(), http.MethodGet, "/git/capabilities", nil)
	require.Contains(t, w.Body.String(), `"enabled":false`)
}

func TestGitRoundTrip(t *testing.T) {
	root := gitEnv(t)
	sandbox := newRepo(t, root, "sandbox")
	git(t, sandbox, "checkout", "-q", "-b", "agent/work")
	require.NoError(t, os.WriteFile(filepath.Join(sandbox, "b.txt"), []byte("b\n"), 0o644))
	git(t, sandbox, "add", ".")
	git(t, sandbox, "commit", "-q", "-m", "two")
	git(t, sandbox, "update-ref", "refs/secret/x", "HEAD")
	tip := git(t, sandbox, "rev-parse", "HEAD")
	base := git(t, sandbox, "rev-parse", "main")

	r := gitRouter()
	w := do(r, http.MethodGet, "/git/refs?path="+sandbox, nil)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.Contains(t, w.Body.String(), "refs/heads/agent/work")
	require.NotContains(t, w.Body.String(), "refs/secret", "refs outside the read policy must be hidden")

	// Incremental bundle: client already has main.
	body, _ := json.Marshal(bundleCreateRequest{Path: sandbox, Refs: []string{"refs/heads/agent/work"}, Haves: []string{base, strings.Repeat("0", 40)}})
	w = do(r, http.MethodPost, "/git/bundle/create", body)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	bundle := w.Body.Bytes()
	require.Equal(t, "application/x-git-bundle", w.Header().Get("Content-Type"))

	client := newRepo(t, root, "client")
	// Client needs the base commit for verification: share history.
	git(t, client, "fetch", "-q", sandbox, "main:refs/heads/base")
	f := filepath.Join(t.TempDir(), "in.bundle")
	require.NoError(t, os.WriteFile(f, bundle, 0o600))
	git(t, client, "bundle", "verify", f)
	git(t, client, "fetch", "-q", f, "refs/heads/agent/work:refs/remotes/sandbox/agent/work")
	require.Equal(t, tip, git(t, client, "rev-parse", "refs/remotes/sandbox/agent/work"))

	// Nothing new: empty bundle -> 204.
	body, _ = json.Marshal(bundleCreateRequest{Path: sandbox, Refs: []string{"refs/heads/agent/work"}, Haves: []string{tip}})
	require.Equal(t, http.StatusNoContent, do(r, http.MethodPost, "/git/bundle/create", body).Code)

	// Inbound: client commits on a feature branch and pushes it into the sandbox.
	git(t, client, "checkout", "-q", "-b", "feature")
	require.NoError(t, os.WriteFile(filepath.Join(client, "c.txt"), []byte("c\n"), 0o644))
	git(t, client, "add", ".")
	git(t, client, "commit", "-q", "-m", "three")
	newTip := git(t, client, "rev-parse", "HEAD")
	out := filepath.Join(t.TempDir(), "out.bundle")
	git(t, client, "bundle", "create", out, "refs/heads/feature", "^"+base)
	data, _ := os.ReadFile(out)
	url := "/git/bundle/receive?path=" + sandbox + "&ref=refs/heads/feature:refs/heads/feature"
	w = do(r, http.MethodPost, url, data)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.Equal(t, newTip, git(t, sandbox, "rev-parse", "refs/heads/feature"))
}

func TestGitReceiveRefRules(t *testing.T) {
	root := gitEnv(t)
	sandbox := newRepo(t, root, "sandbox")
	git(t, sandbox, "branch", "keep")
	client := newRepo(t, root, "client")
	git(t, client, "fetch", "-q", sandbox, "main:refs/heads/base")
	git(t, client, "checkout", "-q", "-b", "feature")
	out := filepath.Join(t.TempDir(), "o.bundle")
	git(t, client, "bundle", "create", out, "refs/heads/feature")
	data, _ := os.ReadFile(out)
	r := gitRouter()
	recv := func(q string, b []byte) *httptest.ResponseRecorder {
		return do(r, http.MethodPost, "/git/bundle/receive?path="+sandbox+q, b)
	}

	cases := map[string]struct {
		q    string
		code int
	}{
		"dst outside write policy": {"&ref=refs/heads/feature:refs/tags/v1", http.StatusForbidden},
		"checked-out branch":       {"&ref=refs/heads/feature:refs/heads/main", http.StatusConflict},
		"option-like ref":          {"&ref=--upload-pack=x:refs/heads/y", http.StatusBadRequest},
		"traversal in ref":         {"&ref=refs/heads/feature:refs/heads/../../x", http.StatusBadRequest},
		"src not in bundle":        {"&ref=refs/heads/nope:refs/heads/y", http.StatusUnprocessableEntity},
		"missing ref parameter":    {"", http.StatusBadRequest},
		"malformed mapping":        {"&ref=refs/heads/feature", http.StatusBadRequest},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			w := recv(tc.q, data)
			require.Equal(t, tc.code, w.Code, w.Body.String())
		})
	}
	require.Equal(t, http.StatusUnprocessableEntity, recv("&ref=refs/heads/feature:refs/heads/y", []byte("not a bundle")).Code)

	// Non-fast-forward needs an explicit force; existing ref untouched until then.
	require.Equal(t, http.StatusOK, recv("&ref=refs/heads/feature:refs/heads/keep2", data).Code)
}

func TestGitSizeCap(t *testing.T) {
	root := gitEnv(t)
	sandbox := newRepo(t, root, "sandbox")
	t.Setenv(gitMaxBundleEnv, "100")
	r := gitRouter()

	body, _ := json.Marshal(bundleCreateRequest{Path: sandbox, Refs: []string{"refs/heads/main"}})
	require.Equal(t, http.StatusRequestEntityTooLarge, do(r, http.MethodPost, "/git/bundle/create", body).Code)

	req := httptest.NewRequest(http.MethodPost, "/git/bundle/receive?path="+sandbox+"&ref=refs/heads/main:refs/heads/x", strings.NewReader(strings.Repeat("x", 500)))
	req.ContentLength = -1 // chunked: only the streaming cap can catch it
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	require.Equal(t, http.StatusRequestEntityTooLarge, w.Code, w.Body.String())
}

func TestGitPathSafety(t *testing.T) {
	root := gitEnv(t)
	outside, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	newRepo(t, outside, "elsewhere")
	sandbox := newRepo(t, root, "sandbox")
	require.NoError(t, os.Symlink(filepath.Join(outside, "elsewhere"), filepath.Join(root, "link")))
	r := gitRouter()

	require.Equal(t, http.StatusForbidden, do(r, http.MethodGet, "/git/refs?path="+filepath.Join(root, "link"), nil).Code, "symlinked repo")
	require.Equal(t, http.StatusForbidden, do(r, http.MethodGet, "/git/refs?path="+filepath.Join(outside, "elsewhere"), nil).Code, "outside root")
	require.Equal(t, http.StatusForbidden, do(r, http.MethodGet, "/git/refs?path="+sandbox+"/../../..", nil).Code, "traversal")

	// .git that is a symlink to another repository is refused.
	other := newRepo(t, root, "other")
	require.NoError(t, os.RemoveAll(filepath.Join(other, ".git")))
	require.NoError(t, os.Symlink(filepath.Join(sandbox, ".git"), filepath.Join(other, ".git")))
	require.Equal(t, http.StatusNotFound, do(r, http.MethodGet, "/git/refs?path="+other, nil).Code, ".git symlink")
}

func TestGitHostileRepoConfigDoesNotExecute(t *testing.T) {
	root := gitEnv(t)
	sandbox := newRepo(t, root, "sandbox")
	marker := filepath.Join(t.TempDir(), "pwned")
	hooks := filepath.Join(sandbox, "evil-hooks")
	require.NoError(t, os.MkdirAll(hooks, 0o755))
	script := "#!/bin/sh\ntouch " + marker + "\n"
	for _, h := range []string{"reference-transaction", "post-checkout", "pre-auto-gc"} {
		require.NoError(t, os.WriteFile(filepath.Join(hooks, h), []byte(script), 0o755))
	}
	fsmon := filepath.Join(sandbox, "fsmon.sh")
	require.NoError(t, os.WriteFile(fsmon, []byte(script), 0o755))
	git(t, sandbox, "config", "core.hooksPath", hooks)
	git(t, sandbox, "config", "core.fsmonitor", fsmon)
	git(t, sandbox, "config", "gc.auto", "1")
	git(t, sandbox, "config", "core.sshCommand", fsmon)

	r := gitRouter()
	require.Equal(t, http.StatusOK, do(r, http.MethodGet, "/git/refs?path="+sandbox, nil).Code)
	body, _ := json.Marshal(bundleCreateRequest{Path: sandbox, Refs: []string{"refs/heads/main"}})
	require.Equal(t, http.StatusOK, do(r, http.MethodPost, "/git/bundle/create", body).Code)

	client := newRepo(t, root, "client")
	git(t, client, "fetch", "-q", sandbox, "main:refs/heads/base")
	git(t, client, "checkout", "-q", "-b", "feature")
	out := filepath.Join(t.TempDir(), "o.bundle")
	git(t, client, "bundle", "create", out, "refs/heads/feature")
	data, _ := os.ReadFile(out)
	w := do(r, http.MethodPost, "/git/bundle/receive?path="+sandbox+"&ref=refs/heads/feature:refs/heads/feature", data)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.NoFileExists(t, marker, "repo-local hooks/fsmonitor must not run")
}

func TestGitInitOnFirstPush(t *testing.T) {
	root := gitEnv(t)
	client := newRepo(t, root, "client")
	tip := git(t, client, "rev-parse", "HEAD")
	out := filepath.Join(t.TempDir(), "o.bundle")
	git(t, client, "bundle", "create", out, "refs/heads/main")
	data, _ := os.ReadFile(out)
	r := gitRouter()
	q := func(path string) string {
		return "/git/bundle/receive?init=true&ref=refs/heads/main:refs/heads/main&path=" + path
	}

	target := filepath.Join(root, "fresh")
	w := do(r, http.MethodPost, q(target), data)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.Equal(t, tip, git(t, target, "rev-parse", "refs/heads/main"))

	// A second init over the now-populated directory is refused.
	require.Equal(t, http.StatusConflict, do(r, http.MethodPost, q(target), data).Code)
	// init cannot target the root itself, a path outside it, or a symlinked parent.
	require.Equal(t, http.StatusForbidden, do(r, http.MethodPost, q(root), data).Code)
	outside := t.TempDir()
	require.Equal(t, http.StatusForbidden, do(r, http.MethodPost, q(filepath.Join(outside, "x")), data).Code)
	require.NoError(t, os.Symlink(outside, filepath.Join(root, "lnk")))
	require.Equal(t, http.StatusForbidden, do(r, http.MethodPost, q(filepath.Join(root, "lnk", "x")), data).Code)
	require.NoDirExists(t, filepath.Join(outside, "x"))
}
