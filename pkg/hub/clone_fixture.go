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
	"sync"
	"testing"
)

// localCloneFixtures holds the exact local repository paths that tests in
// this package registered (allowLocalCloneFixture, in a _test.go file) as
// clone sources for the local-backend shared-workspace clone. Only test
// code writes it; in a production binary it is always empty, and
// localCloneFixtureAllowed ignores it outside a test binary.
var (
	localCloneFixturesMu sync.Mutex
	localCloneFixtures   = map[string]int{}
)

// localCloneFixtureAllowed reports whether cloneURL is a local repository a
// test registered as a clone fixture. It is the only exception to the
// hub-host clone URL check, and it holds only inside a test binary
// (testing.Testing) and only for the exact registered path. No project
// label, request field or environment variable reaches it.
func localCloneFixtureAllowed(cloneURL string) bool {
	return localCloneFixtureRegistered(testing.Testing(), cloneURL)
}

func localCloneFixtureRegistered(inTestBinary bool, cloneURL string) bool {
	if !inTestBinary || cloneURL == "" {
		return false
	}
	localCloneFixturesMu.Lock()
	defer localCloneFixturesMu.Unlock()
	return localCloneFixtures[cloneURL] > 0
}
