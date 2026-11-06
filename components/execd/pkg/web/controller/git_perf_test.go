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
	"encoding/json"
	"fmt"
	"math/rand"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// Timing experiment, not a correctness test: GIT_PERF=1 enables it.
func TestPerfTimings(t *testing.T) {
	if os.Getenv("GIT_PERF") == "" {
		t.Skip("set GIT_PERF=1")
	}
	root := env(t)
	r := gitRouter()
	run := func(name, src string, bytesMB int) {
		sb := src
		if sb == "" {
			sb = newRepo(t, root, "sb-"+name)
			rng := rand.New(rand.NewSource(1))
			for i := 0; i < bytesMB/8+1; i++ {
				buf := make([]byte, 8<<20)
				rng.Read(buf)
				require.NoError(t, os.WriteFile(filepath.Join(sb, fmt.Sprintf("blob%03d.bin", i)), buf, 0o644))
			}
			git(t, sb, "add", ".")
			git(t, sb, "commit", "-q", "-m", "blobs")
			own(t, sb)
		}
		body, _ := json.Marshal(bundleCreateRequest{Path: sb, Refs: []string{"refs/heads/main"}})
		t0 := time.Now()
		w := do(r, http.MethodPost, "/v1/git/bundle/create", body)
		create := time.Since(t0)
		require.Equal(t, http.StatusOK, w.Code, w.Body.String()[:min(200, w.Body.Len())])
		size := w.Body.Len()

		dst := filepath.Join(os.Getenv(gitRepoRootEnv), "dst-"+name)
		t1 := time.Now()
		w2 := recv(r, dst, "&init=true&ref=refs/heads/main:refs/heads/main", w.Body.Bytes())
		receive := time.Since(t1)
		require.Equal(t, http.StatusOK, w2.Code, w2.Body.String())
		fmt.Printf("PERF %-10s bundle=%6.1f MiB  create(first byte)=%6.2fs  receive(total silent)=%6.2fs\n",
			name, float64(size)/(1<<20), create.Seconds(), receive.Seconds())
	}
	t.Setenv(gitMaxBundleEnv, strings.Repeat("9", 10))
	for _, mb := range []int{32, 128, 256, 512} {
		run(fmt.Sprintf("rand%dMB", mb), "", mb)
	}
	if real := os.Getenv("GIT_PERF_REPO"); real != "" {
		t.Setenv(gitRepoRootEnv, filepath.Dir(real))
		run("realrepo", real, 0)
	}
}
