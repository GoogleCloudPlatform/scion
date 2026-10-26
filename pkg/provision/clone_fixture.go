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

package provision

import (
	"sync"
	"testing"
)

// localCloneFixtures holds the clone URLs tests registered with
// AllowLocalCloneFixture, with a count per URL so tests that share a URL
// can each register and clean up.
var localCloneFixtures = struct {
	sync.Mutex
	urls map[string]int
}{urls: map[string]int{}}

// AllowLocalCloneFixture lets the calling test clone the local repository
// named by url (exactly as it is passed in GitClone.URL) through
// ProvisionShared, which otherwise refuses every URL CheckHostCloneURL
// refuses. The clone of a registered URL runs without the hardened
// transport settings, since a local path needs the file transport. The
// registration ends when the test ends.
//
// It is honoured only in a test binary (testing.Testing): a production
// binary never treats any URL as a fixture, and nothing in a request, a
// project label or the environment can register one.
func AllowLocalCloneFixture(tb testing.TB, url string) {
	tb.Helper()
	localCloneFixtures.Lock()
	localCloneFixtures.urls[url]++
	localCloneFixtures.Unlock()
	tb.Cleanup(func() {
		localCloneFixtures.Lock()
		defer localCloneFixtures.Unlock()
		if localCloneFixtures.urls[url]--; localCloneFixtures.urls[url] <= 0 {
			delete(localCloneFixtures.urls, url)
		}
	})
}

// isLocalCloneFixture reports whether url was registered with
// AllowLocalCloneFixture by a test that is still running. Always false
// outside a test binary.
func isLocalCloneFixture(url string) bool {
	if !testing.Testing() {
		return false
	}
	localCloneFixtures.Lock()
	defer localCloneFixtures.Unlock()
	return localCloneFixtures.urls[url] > 0
}

// checkProvisionCloneURL is the clone-URL check every ProvisionShared call
// passes: a git-backed input's URL must pass CheckHostCloneURL (https,
// ssh or git only), whichever caller provisions and whether or not this
// call would clone.
func checkProvisionCloneURL(in ProvisionInput) error {
	if in.GitClone == nil || in.GitClone.URL == "" || isLocalCloneFixture(in.GitClone.URL) {
		return nil
	}
	return CheckHostCloneURL(in.GitClone.URL)
}
