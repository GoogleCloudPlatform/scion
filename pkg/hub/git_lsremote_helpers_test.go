// Copyright 2026 Google LLC
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

package hub

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

// errGitLsRemoteDisabled is returned by the package-wide hermetic ls-remote
// runner installed in TestMain. resolveGitHubRef treats any error as "git
// unavailable" and keeps the naive branch/path parse, so tests that never
// stub the seam still behave deterministically, without touching the network.
var errGitLsRemoteDisabled = errors.New("pkg/hub tests: real git ls-remote is disabled (ptone/scion#3670); use stubGitLsRemote")

// installHermeticGitLsRemote replaces pkg/config's git ls-remote runner for
// the whole pkg/hub test binary so no test can shell out to a real
// `git ls-remote https://github.com/...` (which hit the network and could hang
// with no deadline, ptone/scion#3670). Called from TestMain; returns a restore
// func.
func installHermeticGitLsRemote() (restore func()) {
	return config.SetGitLsRemoteForTest(func(context.Context, string) ([]byte, error) {
		return nil, errGitLsRemoteDisabled
	})
}

// gitLsRemoteStub records the repo URLs passed to a stubbed ls-remote runner.
type gitLsRemoteStub struct {
	mu   sync.Mutex
	urls []string
}

// URLs returns a copy of the repo URLs the stub was called with.
func (s *gitLsRemoteStub) URLs() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.urls...)
}

// stubGitLsRemote makes pkg/config's git ls-remote runner return the canned
// `git ls-remote --heads` output for the rest of the test, restoring the
// previous runner via t.Cleanup. Tests should assert on the returned stub's
// URLs so a regression that bypasses the seam (and would exec real git) fails
// loudly instead of silently hitting the network.
//
// Not safe for t.Parallel(): it mutates package-global state in pkg/config.
func stubGitLsRemote(t *testing.T, output string) *gitLsRemoteStub {
	t.Helper()
	stub := &gitLsRemoteStub{}
	restore := config.SetGitLsRemoteForTest(func(_ context.Context, repoURL string) ([]byte, error) {
		stub.mu.Lock()
		stub.urls = append(stub.urls, repoURL)
		stub.mu.Unlock()
		return []byte(output), nil
	})
	t.Cleanup(restore)
	return stub
}

// errGitSparseCheckoutDisabled is returned by the package-wide hermetic
// sparse-checkout runner installed in TestMain. FetchRemoteTemplate only
// reaches the sparse checkout when the tarball download fails, so a test that
// hits it without stubbing gets a clear error instead of a real `git fetch`.
var errGitSparseCheckoutDisabled = errors.New("pkg/hub tests: real git sparse checkout is disabled (ptone/scion#3750); use stubGitSparseCheckout")

// installHermeticGitSparseCheckout replaces pkg/config's sparse git checkout
// runner for the whole pkg/hub test binary so no test can shell out to a real
// `git fetch https://github.com/...` (ptone/scion#3750). Called from TestMain;
// returns a restore func.
func installHermeticGitSparseCheckout() (restore func()) {
	return config.SetGitSparseCheckoutForTest(func(context.Context, *config.GitHubURLParts, string, string) error {
		return errGitSparseCheckoutDisabled
	})
}

// gitSparseCheckoutCall is one recorded call to a stubbed sparse checkout.
type gitSparseCheckoutCall struct {
	Parts    config.GitHubURLParts
	DestPath string
	Token    string
}

// gitSparseCheckoutStub records calls made to a stubbed sparse checkout.
type gitSparseCheckoutStub struct {
	mu    sync.Mutex
	calls []gitSparseCheckoutCall
}

// Calls returns a copy of the recorded calls.
func (s *gitSparseCheckoutStub) Calls() []gitSparseCheckoutCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]gitSparseCheckoutCall(nil), s.calls...)
}

// stubGitSparseCheckout makes pkg/config's sparse checkout runner write files
// (path relative to the checked-out folder -> contents) into destPath for the
// rest of the test, restoring the previous runner via t.Cleanup. Use it for
// tests that exercise the git fallback after a failed tarball download, and
// assert on the returned stub's Calls.
//
// Not safe for t.Parallel(): it mutates package-global state in pkg/config.
func stubGitSparseCheckout(t *testing.T, files map[string]string) *gitSparseCheckoutStub {
	t.Helper()
	stub := &gitSparseCheckoutStub{}
	restore := config.SetGitSparseCheckoutForTest(func(_ context.Context, parts *config.GitHubURLParts, destPath, token string) error {
		stub.mu.Lock()
		stub.calls = append(stub.calls, gitSparseCheckoutCall{Parts: *parts, DestPath: destPath, Token: token})
		stub.mu.Unlock()
		for rel, body := range files {
			p := filepath.Join(destPath, filepath.FromSlash(rel))
			if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
				return err
			}
			if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
				return err
			}
		}
		return nil
	})
	t.Cleanup(restore)
	return stub
}
