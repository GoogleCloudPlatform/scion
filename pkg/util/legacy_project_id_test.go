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

package util

import (
	"strings"
	"testing"
)

// TestLegacyGitProjectID_Fidelity pins the IDs that the code before random
// project IDs produced for these raw remotes: the UUID v5 of
// NormalizeGitRemote(remote) in legacyProjectIDNamespace, at the parent of
// commit dc22fcc90. The literals were computed by running that code
// unchanged, and checked with an independent UUID v5 implementation.
func TestLegacyGitProjectID_Fidelity(t *testing.T) {
	cases := []struct {
		raw  string
		want string
	}{
		{"https://github.com/acme/widgets.git", "5fd0688b-4db9-5dc0-a2eb-bc4b5cc804b8"},
		{"git@github.com:ptone/gamegame.git", "8f151607-ea19-56c9-b302-4f8aa07d91b5"},
		{"https://gitlab.example.com/group/sub/repo", "4f68d4c9-6667-55c6-9cff-b6987dfe890a"},
		{"git.example.com:8443/team/app", "42665479-ca4c-5c2c-8780-742ee8c94a2f"},
	}
	for _, tc := range cases {
		got, ok := LegacyGitProjectID(NormalizeGitRemote(tc.raw))
		if !ok || got != tc.want {
			t.Errorf("LegacyGitProjectID(NormalizeGitRemote(%q)) = %q, %v; want %q, true", tc.raw, got, ok, tc.want)
		}
	}
}

// TestLegacyGitProjectID_TrailingSlashNotReproduced pins a known gap: the
// legacy code did not strip a trailing '/', so a project whose raw remote
// had one got an ID from that form. The stored, normalized remote does not
// derive it, so such a resume still fails the identity check.
func TestLegacyGitProjectID_TrailingSlashNotReproduced(t *testing.T) {
	cases := []struct {
		raw    string
		legacy string
	}{
		{"https://github.com/acme/widgets/", "19e6e757-7674-5766-a0cf-418a5533278c"},
		{"https://github.com/acme/widgets.git/", "d394b23a-f125-56e1-9c87-b2abfc39e4a5"},
	}
	for _, tc := range cases {
		got, ok := LegacyGitProjectID(NormalizeGitRemote(tc.raw))
		if !ok {
			t.Fatalf("LegacyGitProjectID(NormalizeGitRemote(%q)) refused", tc.raw)
		}
		if got == tc.legacy {
			t.Errorf("LegacyGitProjectID(NormalizeGitRemote(%q)) = the legacy ID of the unnormalized form", tc.raw)
		}
	}
}

func TestLegacyGitProjectID_Refused(t *testing.T) {
	refused := []string{
		"",
		"github.com",
		"github.com/acme",
		"GitHub.com/acme/widgets",
		"https://github.com/acme/widgets",
		"github.com/acme/widgets.git",
		"github.com/acme/widgets/",
		"user@github.com/acme/widgets",
		"[::1]/acme/widgets",
		"github.com/acme/wid gets",
		"github.com/acme/wid%20gets",
		"github.com/acme/widgets?x=1",
		"github.com/acme/widgets#x",
		"github.com/acme//widgets",
		"github.com/acme/../widgets",
		"github.com/./widgets",
		"github.com\\acme/widgets",
		"/github.com/acme/widgets",
		"-github.com/acme/widgets",
		"github.com:/acme/widgets",
		"github.com:x1/acme/widgets",
		"github.com/acme/widgets\x00",
		"github.com/acme/widgets\n",
		"github.com/" + strings.Repeat("a", 250) + "/widgets",
	}
	for _, r := range refused {
		if id, ok := LegacyGitProjectID(r); ok {
			t.Errorf("LegacyGitProjectID(%q) = %q, true; want refused", r, id)
		}
	}
}
