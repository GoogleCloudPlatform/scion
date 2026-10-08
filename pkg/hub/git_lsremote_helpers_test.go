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
