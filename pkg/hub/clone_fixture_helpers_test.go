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
	"path/filepath"
	"testing"
)

// allowLocalCloneFixture lets the local-backend shared-workspace clone use
// the local repository at path (an absolute, clean path, matched exactly)
// for the rest of the test, and only inside this test binary. The
// registration is removed when the test ends.
func allowLocalCloneFixture(tb testing.TB, path string) {
	tb.Helper()
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		tb.Fatalf("allowLocalCloneFixture: %q must be an absolute, clean path", path)
	}
	localCloneFixturesMu.Lock()
	localCloneFixtures[path]++
	localCloneFixturesMu.Unlock()
	tb.Cleanup(func() {
		localCloneFixturesMu.Lock()
		defer localCloneFixturesMu.Unlock()
		if localCloneFixtures[path]--; localCloneFixtures[path] <= 0 {
			delete(localCloneFixtures, path)
		}
	})
}
