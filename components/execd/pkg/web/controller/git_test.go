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

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

const agentUID = 1000

func gitRouter() *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.GET("/v1/git/capabilities", func(c *gin.Context) { NewGitController(c).Capabilities() })
	r.GET("/v1/git/refs", func(c *gin.Context) { NewGitController(c).ListRefs() })
	r.POST("/v1/git/bundle/create", func(c *gin.Context) { NewGitController(c).CreateBundle() })
	r.POST("/v1/git/bundle/receive", func(c *gin.Context) { NewGitController(c).ReceiveBundle() })
	return r
}

// git runs test-side git as root against repositories owned by the agent uid.
func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir, "-c", "safe.directory=*", "-c", "user.name=t", "-c", "user.email=t@t", "-c", "commit.gpgsign=false"}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL="+testGlobalConfig(t), "GIT_CONFIG_NOSYSTEM=1")
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, string(out))
	return strings.TrimSpace(string(out))
}

func testGlobalConfig(t *testing.T) string {
	t.Helper()
	p := filepath.Join(os.TempDir(), "gt-global-gitconfig")
	require.NoError(t, os.WriteFile(p, []byte("[safe]\n\tdirectory = *\n"), 0o644))
	return p
}

func own(t *testing.T, path string) {
	t.Helper()
	out, err := exec.Command("chown", "-R", strconv.Itoa(agentUID)+":"+strconv.Itoa(agentUID), path).CombinedOutput()
	require.NoError(t, err, string(out))
}

func newRepo(t *testing.T, root, name string) string {
	t.Helper()
	dir := filepath.Join(root, name)
	require.NoError(t, os.MkdirAll(dir, 0o755))
	git(t, dir, "init", "-q", "-b", "main")
	require.NoError(t, os.WriteFile(filepath.Join(dir, "a.txt"), []byte("a\n"), 0o644))
	git(t, dir, "add", ".")
	git(t, dir, "commit", "-q", "-m", "one")
	own(t, dir)
	return dir
}

func commit(t *testing.T, dir, file string) string {
	t.Helper()
	require.NoError(t, os.WriteFile(filepath.Join(dir, file), []byte(file+"\n"), 0o644))
	git(t, dir, "add", ".")
	git(t, dir, "commit", "-q", "-m", file)
	own(t, dir)
	return git(t, dir, "rev-parse", "HEAD")
}

func do(r *gin.Engine, method, url string, body []byte) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, url, bytes.NewReader(body))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func env(t *testing.T) string {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("needs root to run git as a different owner")
	}
	root, err := os.MkdirTemp("", "gt-")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	require.NoError(t, os.Chmod(root, 0o755))
	own(t, root)
	for _, k := range []string{gitInboundEnv, gitOutboundEnv, gitMaxBundleEnv, gitRunAsEnv, gitBinEnv, gitTimeoutEnv, gitMaxConcEnv, gitUnsafeTestEnv} {
		t.Setenv(k, "")
	}
	t.Setenv(gitRepoRootEnv, root)
	return root
}

func bundleOf(t *testing.T, repo string, args ...string) []byte {
	t.Helper()
	f := filepath.Join(t.TempDir(), "b.bundle")
	git(t, repo, append([]string{"bundle", "create", f}, args...)...)
	b, err := os.ReadFile(f)
	require.NoError(t, err)
	return b
}

func recv(r *gin.Engine, path, q string, b []byte) *httptest.ResponseRecorder {
	return do(r, http.MethodPost, "/v1/git/bundle/receive?path="+path+q, b)
}

func codeOf(w *httptest.ResponseRecorder) string {
	var m struct {
		Code string `json:"code"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &m)
	return m.Code
}

func TestDisabledByDefault(t *testing.T) {
	t.Setenv(gitRepoRootEnv, "")
	require.Equal(t, http.StatusNotImplemented, do(gitRouter(), http.MethodGet, "/v1/git/refs?path=/tmp", nil).Code)
	require.Contains(t, do(gitRouter(), http.MethodGet, "/v1/git/capabilities", nil).Body.String(), `"enabled":false`)
}

func TestRoundTripAndIdentityDrop(t *testing.T) {
	root := env(t)
	sb := newRepo(t, root, "sandbox")
	base := git(t, sb, "rev-parse", "HEAD")
	git(t, sb, "checkout", "-q", "-b", "agent/work")
	tip := commit(t, sb, "b.txt")
	git(t, sb, "update-ref", "refs/secret/x", "HEAD")
	own(t, sb)

	// A wrapper records the uid git runs as, then execs the real binary.
	marker := filepath.Join(root, "uid")
	wrapper := filepath.Join(root, "git-wrapper")
	require.NoError(t, os.WriteFile(wrapper, []byte("#!/bin/sh\nid -u >> "+marker+"\nexec /usr/bin/git \"$@\"\n"), 0o755))
	t.Setenv(gitBinEnv, wrapper)
	require.NoError(t, os.WriteFile(marker, nil, 0o666))
	require.NoError(t, os.Chmod(marker, 0o666))

	r := gitRouter()
	w := do(r, http.MethodGet, "/v1/git/refs?path="+sb, nil)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.Contains(t, w.Body.String(), "refs/heads/agent/work")
	require.NotContains(t, w.Body.String(), "refs/secret")

	body, _ := json.Marshal(bundleCreateRequest{Path: sb, Refs: []string{"refs/heads/agent/work"}, Haves: []string{base, strings.Repeat("0", 40)}})
	w = do(r, http.MethodPost, "/v1/git/bundle/create", body)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	client := newRepo(t, root, "client")
	git(t, client, "fetch", "-q", sb, "main:refs/heads/base")
	f := filepath.Join(t.TempDir(), "in.bundle")
	require.NoError(t, os.WriteFile(f, w.Body.Bytes(), 0o600))
	git(t, client, "bundle", "verify", f)
	git(t, client, "fetch", "-q", f, "refs/heads/agent/work:refs/remotes/sandbox/agent/work")
	require.Equal(t, tip, git(t, client, "rev-parse", "refs/remotes/sandbox/agent/work"))

	body, _ = json.Marshal(bundleCreateRequest{Path: sb, Refs: []string{"refs/heads/agent/work"}, Haves: []string{tip}})
	require.Equal(t, http.StatusNoContent, do(r, http.MethodPost, "/v1/git/bundle/create", body).Code)

	git(t, client, "checkout", "-q", "-b", "feature")
	newTip := commit(t, client, "c.txt")
	b := bundleOf(t, client, "refs/heads/feature", "^"+base)
	w = recv(r, sb, "&ref=refs/heads/feature:refs/heads/feature", b)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.Equal(t, newTip, git(t, sb, "rev-parse", "refs/heads/feature"))

	uids, _ := os.ReadFile(marker)
	require.NotEmpty(t, strings.TrimSpace(string(uids)))
	for _, u := range strings.Fields(string(uids)) {
		require.Equal(t, strconv.Itoa(agentUID), u, "git must never run as execd's uid")
	}
}

func TestOwnershipAndPaths(t *testing.T) {
	root := env(t)
	sb := newRepo(t, root, "sandbox")
	r := gitRouter()

	require.NoError(t, os.Chown(filepath.Join(sb, ".git", "config"), 0, 0))
	require.Equal(t, http.StatusForbidden, do(r, http.MethodGet, "/v1/git/refs?path="+sb, nil).Code, "root-owned config in an agent repo")
	own(t, sb)

	outside := t.TempDir()
	newRepo(t, outside, "elsewhere")
	require.NoError(t, os.Symlink(filepath.Join(outside, "elsewhere"), filepath.Join(root, "link")))
	require.Equal(t, http.StatusForbidden, do(r, http.MethodGet, "/v1/git/refs?path="+filepath.Join(root, "link"), nil).Code)
	require.Equal(t, http.StatusForbidden, do(r, http.MethodGet, "/v1/git/refs?path="+filepath.Join(outside, "elsewhere"), nil).Code)
	require.Equal(t, http.StatusForbidden, do(r, http.MethodGet, "/v1/git/refs?path="+sb+"/../../..", nil).Code)

	other := newRepo(t, root, "other")
	require.NoError(t, os.RemoveAll(filepath.Join(other, ".git")))
	require.NoError(t, os.Symlink(filepath.Join(sb, ".git"), filepath.Join(other, ".git")))
	require.Equal(t, http.StatusForbidden, do(r, http.MethodGet, "/v1/git/refs?path="+other, nil).Code, ".git symlink")

	for name, setup := range map[string]func(dir string){
		"alternates": func(d string) {
			require.NoError(t, os.MkdirAll(filepath.Join(d, ".git/objects/info"), 0o755))
			require.NoError(t, os.WriteFile(filepath.Join(d, ".git/objects/info/alternates"), []byte(sb+"/.git/objects\n"), 0o644))
		},
		"commondir": func(d string) {
			require.NoError(t, os.WriteFile(filepath.Join(d, ".git/commondir"), []byte("../x\n"), 0o644))
		},
		"grafts": func(d string) {
			require.NoError(t, os.MkdirAll(filepath.Join(d, ".git/info"), 0o755))
			require.NoError(t, os.WriteFile(filepath.Join(d, ".git/info/grafts"), nil, 0o644))
		},
		"objects symlink": func(d string) {
			require.NoError(t, os.RemoveAll(filepath.Join(d, ".git/objects")))
			require.NoError(t, os.Symlink(filepath.Join(sb, ".git/objects"), filepath.Join(d, ".git/objects")))
		},
	} {
		d := newRepo(t, root, "redir-"+strings.ReplaceAll(name, " ", "-"))
		setup(d)
		own(t, d)
		require.Equal(t, http.StatusForbidden, do(r, http.MethodGet, "/v1/git/refs?path="+d, nil).Code, name)
	}
}

// A concurrent attacker flips a path component between a real repo and a
// symlink to another repository while requests are in flight. No response may
// ever list the other repository's refs.
func TestPathSwapRace(t *testing.T) {
	root := env(t)
	good := newRepo(t, root, "good")
	evil := newRepo(t, root, "evil")
	git(t, evil, "branch", "stolen")
	own(t, evil)
	require.NoError(t, os.MkdirAll(filepath.Join(root, "slot"), 0o755))
	require.NoError(t, os.Rename(good, filepath.Join(root, "slot", "repo")))
	r := gitRouter()
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		real := filepath.Join(root, "slot", "repo")
		held := filepath.Join(root, "slot", "repo.real")
		for {
			select {
			case <-stop:
				return
			default:
			}
			_ = os.Rename(real, held)
			_ = os.Symlink(evil, real)
			time.Sleep(200 * time.Microsecond)
			_ = os.Remove(real)
			_ = os.Rename(held, real)
		}
	}()
	for i := 0; i < 300; i++ {
		w := do(r, http.MethodGet, "/v1/git/refs?path="+filepath.Join(root, "slot", "repo"), nil)
		require.NotContains(t, w.Body.String(), "stolen", "a swapped component leaked another repository")
	}
	close(stop)
	wg.Wait()
}

func TestRefRulesLeaseAtomicLock(t *testing.T) {
	root := env(t)
	sb := newRepo(t, root, "sandbox")
	base := git(t, sb, "rev-parse", "HEAD")
	git(t, sb, "branch", "keep")
	own(t, sb)
	client := newRepo(t, root, "client")
	git(t, client, "fetch", "-q", sb, "main:refs/heads/base")
	git(t, client, "checkout", "-q", "-b", "feature")
	one := commit(t, client, "f1")
	b1 := bundleOf(t, client, "refs/heads/feature", "^"+base)
	r := gitRouter()
	q := func(s string) string { return "&ref=" + s }

	cases := map[string]struct {
		q    string
		code int
		c    string
	}{
		"dst outside inbound prefixes": {q("refs/heads/feature:refs/tags/v1"), http.StatusForbidden, "GIT_REF_DENIED"},
		"checked-out branch":           {q("refs/heads/feature:refs/heads/main"), http.StatusConflict, "GIT_REF_CHECKED_OUT"},
		"option-like src":              {q("--upload-pack=x:refs/heads/y"), http.StatusBadRequest, ""},
		"traversal dst":                {q("refs/heads/feature:refs/heads/../../x"), http.StatusBadRequest, ""},
		"src not in bundle":            {q("refs/heads/nope:refs/heads/y"), http.StatusUnprocessableEntity, "GIT_BUNDLE_INVALID"},
		"no ref":                       {"", http.StatusBadRequest, ""},
		"malformed":                    {q("refs/heads/feature"), http.StatusBadRequest, ""},
		"bad lease sha":                {q("refs/heads/feature:refs/heads/y@zz"), http.StatusBadRequest, ""},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			w := recv(r, sb, tc.q, b1)
			require.Equal(t, tc.code, w.Code, w.Body.String())
			if tc.c != "" {
				require.Equal(t, tc.c, codeOf(w))
			}
		})
	}
	require.Equal(t, "GIT_BUNDLE_INVALID", codeOf(recv(r, sb, q("refs/heads/feature:refs/heads/y"), []byte("not a bundle"))))

	// Create, then fast-forward, then a non-fast-forward needs +, then leases.
	require.Equal(t, http.StatusOK, recv(r, sb, q("refs/heads/feature:refs/heads/f"), b1).Code)
	two := commit(t, client, "f2")
	b2 := bundleOf(t, client, "refs/heads/feature", "^"+one)
	require.Equal(t, http.StatusOK, recv(r, sb, q("refs/heads/feature:refs/heads/f"), b2).Code)
	require.Equal(t, two, git(t, sb, "rev-parse", "refs/heads/f"))
	// Rewind to one: not a fast-forward.
	git(t, client, "checkout", "-q", "-B", "rewound", one)
	b3 := bundleOf(t, client, "refs/heads/rewound", "^"+base)
	w := recv(r, sb, q("refs/heads/rewound:refs/heads/f"), b3)
	require.Equal(t, "GIT_NOT_FAST_FORWARD", codeOf(w))
	require.Equal(t, "GIT_STALE_LEASE", codeOf(recv(r, sb, q("%2Brefs/heads/rewound:refs/heads/f@"+one), b3)), "wrong lease")
	require.Equal(t, http.StatusOK, recv(r, sb, q("%2Brefs/heads/rewound:refs/heads/f@"+two), b3).Code, "right lease")
	// Replay of the same leased force after the agent committed on f is refused.
	git(t, sb, "update-ref", "refs/heads/f", commit(t, sb, "agent-work"))
	require.Equal(t, "GIT_STALE_LEASE", codeOf(recv(r, sb, q("%2Brefs/heads/rewound:refs/heads/f@"+two), b3)), "replay must not rewind agent work")

	// Atomic: one bad lease among two mappings changes nothing.
	before := git(t, sb, "rev-parse", "refs/heads/keep")
	w = recv(r, sb, q("refs/heads/rewound:refs/heads/keep")+q("refs/heads/rewound:refs/heads/new1@"+two), b3)
	require.Equal(t, http.StatusConflict, w.Code)
	require.Equal(t, before, git(t, sb, "rev-parse", "refs/heads/keep"))
	require.Error(t, exec.Command("git", "-C", sb, "-c", "safe.directory=*", "rev-parse", "-q", "--verify", "refs/heads/new1").Run())

	// A held lock reports GIT_REF_LOCKED and execd deletes nothing.
	lock := filepath.Join(sb, ".git/refs/heads/lk.lock")
	require.NoError(t, os.WriteFile(lock, nil, 0o644))
	own(t, sb)
	w = recv(r, sb, q("refs/heads/rewound:refs/heads/lk"), b3)
	require.Equal(t, "GIT_REF_LOCKED", codeOf(w), w.Body.String())
	require.FileExists(t, lock)
}

func TestPrerequisiteMissing(t *testing.T) {
	root := env(t)
	sb := newRepo(t, root, "sandbox")
	client := newRepo(t, root, "client")
	commit(t, client, "x1") // not in the sandbox
	second := commit(t, client, "x2")
	b := bundleOf(t, client, "refs/heads/main", "HEAD~1..HEAD")
	_ = second
	w := recv(gitRouter(), sb, "&ref=refs/heads/main:refs/heads/inc", b)
	require.Equal(t, http.StatusUnprocessableEntity, w.Code)
	require.Equal(t, "GIT_PREREQUISITE_MISSING", codeOf(w), w.Body.String())
}

func TestWorktreesAndInit(t *testing.T) {
	root := env(t)
	mainRepo := newRepo(t, root, "main")
	wt := filepath.Join(root, "wt")
	git(t, mainRepo, "worktree", "add", "-q", "-b", "side", wt)
	own(t, root)
	r := gitRouter()
	require.Equal(t, http.StatusForbidden, do(r, http.MethodGet, "/v1/git/refs?path="+wt, nil).Code, "linked worktree path")
	require.Contains(t, do(r, http.MethodGet, "/v1/git/refs?path="+mainRepo, nil).Body.String(), "refs/heads/side")

	client := newRepo(t, root, "client")
	git(t, client, "checkout", "-q", "-b", "side2")
	commit(t, client, "s")
	b := bundleOf(t, client, "refs/heads/side2")
	w := recv(r, mainRepo, "&ref=refs/heads/side2:refs/heads/side", b)
	require.Equal(t, "GIT_REF_CHECKED_OUT", codeOf(w), "branch checked out in a linked worktree")

	// init: first push lands on its branch, second push to it is checked-out.
	bm := bundleOf(t, client, "refs/heads/main")
	target := filepath.Join(root, "fresh")
	w = recv(r, target, "&init=true&ref=refs/heads/main:refs/heads/main", bm)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.Equal(t, "refs/heads/main", git(t, target, "symbolic-ref", "HEAD"))
	st, err := os.Stat(target)
	require.NoError(t, err)
	require.Equal(t, uint32(agentUID), st.Sys().(*syscall.Stat_t).Uid)
	git(t, target, "reset", "-q", "--hard")
	require.FileExists(t, filepath.Join(target, "a.txt"))
	require.Equal(t, "GIT_REF_CHECKED_OUT", codeOf(recv(r, target, "&ref=refs/heads/main:refs/heads/main", bm)))
	require.Equal(t, "GIT_INIT_TARGET_NOT_EMPTY", codeOf(recv(r, target, "&init=true&ref=refs/heads/main:refs/heads/main", bm)))
	outside := t.TempDir()
	require.Equal(t, http.StatusForbidden, recv(r, filepath.Join(outside, "x"), "&init=true&ref=refs/heads/main:refs/heads/main", bm).Code)
	require.NoError(t, os.Symlink(outside, filepath.Join(root, "lnk")))
	require.Equal(t, http.StatusForbidden, recv(r, filepath.Join(root, "lnk", "x"), "&init=true&ref=refs/heads/main:refs/heads/main", bm).Code)
	require.NoDirExists(t, filepath.Join(outside, "x"))
}

func TestSizeCapsAndBusy(t *testing.T) {
	root := env(t)
	sb := newRepo(t, root, "sandbox")
	t.Setenv(gitMaxBundleEnv, "100")
	r := gitRouter()
	body, _ := json.Marshal(bundleCreateRequest{Path: sb, Refs: []string{"refs/heads/main"}})
	w := do(r, http.MethodPost, "/v1/git/bundle/create", body)
	require.Equal(t, http.StatusRequestEntityTooLarge, w.Code, w.Body.String())
	require.Equal(t, "GIT_TOO_LARGE", codeOf(w))
	req := httptest.NewRequest(http.MethodPost, "/v1/git/bundle/receive?path="+sb+"&ref=refs/heads/main:refs/heads/x", strings.NewReader(strings.Repeat("x", 500)))
	req.ContentLength = -1
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	require.Equal(t, http.StatusRequestEntityTooLarge, rec.Code)

	t.Setenv(gitMaxBundleEnv, "")
	rp, gerr := openRepo(loadGitPolicy(), sb)
	require.Nil(t, gerr)
	require.True(t, acquireGit(rp.key, 2))
	w = recv(r, sb, "&ref=refs/heads/main:refs/heads/x", []byte("x"))
	require.Equal(t, http.StatusTooManyRequests, w.Code)
	require.Equal(t, "GIT_BUSY", codeOf(w))
	require.NotEmpty(t, w.Header().Get("Retry-After"))
	releaseGit(rp.key)
	rp.Close()
}

func plant(t *testing.T, sb string) string {
	t.Helper()
	marker := filepath.Join(filepath.Dir(sb), "pwned-"+filepath.Base(sb)+"-"+strconv.Itoa(int(time.Now().UnixNano()%1000000)))
	hooks := filepath.Join(sb, "evil-hooks")
	require.NoError(t, os.MkdirAll(hooks, 0o755))
	script := "#!/bin/sh\ntouch " + marker + "\n"
	for _, h := range []string{"reference-transaction", "post-checkout", "pre-auto-gc", "post-merge"} {
		require.NoError(t, os.WriteFile(filepath.Join(hooks, h), []byte(script), 0o755))
	}
	fsmon := filepath.Join(sb, "fsmon.sh")
	require.NoError(t, os.WriteFile(fsmon, []byte(script), 0o755))
	git(t, sb, "config", "core.hooksPath", hooks)
	git(t, sb, "config", "core.fsmonitor", fsmon)
	git(t, sb, "config", "gc.auto", "1")
	git(t, sb, "config", "core.sshCommand", fsmon)
	// Alternate-refs command runs when git inspects alternates during fetch.
	git(t, sb, "config", "core.alternateRefsCommand", fsmon)
	own(t, sb)
	return marker
}

func exercise(t *testing.T, root, sb string) {
	r := gitRouter()
	do(r, http.MethodGet, "/v1/git/refs?path="+sb, nil)
	body, _ := json.Marshal(bundleCreateRequest{Path: sb, Refs: []string{"refs/heads/main"}})
	do(r, http.MethodPost, "/v1/git/bundle/create", body)
	client := newRepo(t, root, "client-"+strconv.Itoa(int(time.Now().UnixNano()%100000)))
	git(t, client, "checkout", "-q", "-b", "feature")
	commit(t, client, "z")
	b := bundleOf(t, client, "refs/heads/feature")
	recv(r, sb, "&ref=refs/heads/feature:refs/heads/feature", b)
}

func TestHostileConfigAndNegativeControl(t *testing.T) {
	root := env(t)
	sb := newRepo(t, root, "sandbox")
	marker := plant(t, sb)
	exercise(t, root, sb)
	require.NoFileExists(t, marker, "repo-local hooks, fsmonitor, gc, and alternate-refs command must not run")

	// Negative control: with the overrides off the same fixture must fire.
	root2 := env(t)
	sb2 := newRepo(t, root2, "sandbox")
	marker2 := plant(t, sb2)
	t.Setenv(gitUnsafeTestEnv, "1")
	exercise(t, root2, sb2)
	require.FileExists(t, marker2, "fixture must execute the planted script when overrides are disabled")
}

// A commit with a malformed author line is rejected on import only when fsck
// is enabled for unbundle.
func TestCorruptObjectRejectedByFsck(t *testing.T) {
	root := env(t)
	sb := newRepo(t, root, "sandbox")
	client := newRepo(t, root, "client")
	tree := git(t, client, "rev-parse", "HEAD^{tree}")
	cmd := exec.Command("git", "-C", client, "-c", "safe.directory=*", "hash-object", "-t", "commit", "-w", "--literally", "--stdin")
	cmd.Stdin = strings.NewReader("tree " + tree + "\nauthor not a valid author line\ncommitter t <t@t> 0 +0000\n\nbad\n")
	out, err := cmd.Output()
	require.NoError(t, err)
	bad := strings.TrimSpace(string(out))
	git(t, client, "update-ref", "refs/heads/bad", bad)
	b := bundleOf(t, client, "refs/heads/bad")
	r := gitRouter()
	w := recv(r, sb, "&ref=refs/heads/bad:refs/heads/bad", b)
	require.Equal(t, http.StatusUnprocessableEntity, w.Code, "malformed commit must be rejected on import: "+w.Body.String())
	require.Equal(t, "GIT_BUNDLE_INVALID", codeOf(w))
	require.Error(t, exec.Command("git", "-C", sb, "-c", "safe.directory=*", "rev-parse", "-q", "--verify", "refs/heads/bad").Run())
}
