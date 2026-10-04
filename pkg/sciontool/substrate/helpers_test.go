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

package substrate

import "testing"

// TestIsWithinAgentHome is the direct unit test for isWithinAgentHome's own
// containment boundary — the regression test for the mutation the test
// review found survives: replacing the component-wise filepath.Rel
// comparison with a bare strings.HasPrefix(path, agentHomeDir) would wrongly
// accept a sibling directory that merely starts with the same characters
// ("/home/scion-other", "/home/scionX"), since neither is actually inside
// "/home/scion". A ".."-escape that cleans back outside home must also be
// refused.
func TestIsWithinAgentHome(t *testing.T) {
	withAgentHomeFixture(t, "/home/scion")

	cases := []struct {
		name string
		path string
		want bool
	}{
		{"home itself", "/home/scion", true},
		{"ordinary descendant", "/home/scion/.config/foo", true},
		{"deeper descendant", "/home/scion/a/b/c", true},
		{"sibling with a suffix character, no separator", "/home/scionX/file", false},
		{"sibling with a dash suffix", "/home/scion-other/file", false},
		{"sibling with a dash suffix, bare dir", "/home/scion-other", false},
		{"unrelated path entirely", "/etc/passwd", false},
		{"dotdot escape to a sibling", "/home/scion/../scion-other/file", false},
		{"dotdot escape to an unrelated root path", "/home/scion/../../etc/passwd", false},
		{"dotdot that cleans back inside home", "/home/scion/a/../b", true},
		{"parent of home", "/home", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isWithinAgentHome(tc.path); got != tc.want {
				t.Errorf("isWithinAgentHome(%q) = %v, want %v (agentHomeDir=%q)", tc.path, got, tc.want, agentHomeDir)
			}
		})
	}
}
