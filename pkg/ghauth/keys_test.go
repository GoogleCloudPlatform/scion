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

package ghauth

import "testing"

// TestNormalizeName verifies the character normalization used in
// convention-based key derivation.
func TestNormalizeName(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"scion-frontiers", "SCION_FRONTIERS"},
		{"scion-repo-contrib", "SCION_REPO_CONTRIB"},
		{"GoogleCloudPlatform", "GOOGLECLOUDPLATFORM"},
		{"ptone", "PTONE"},
		{"my-org", "MY_ORG"},
		{"my.repo", "MY_REPO"},
		{"my_repo", "MY_REPO"},
		{"my.special.repo", "MY_SPECIAL_REPO"},
		{"a-b.c_d", "A_B_C_D"},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			if got := NormalizeName(tt.in); got != tt.want {
				t.Errorf("NormalizeName(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

// TestRepoKey verifies the repo-specific key derivation (GH_OWNER__REPO).
func TestRepoKey(t *testing.T) {
	tests := []struct {
		owner, repo string
		want        string
	}{
		{"scion-frontiers", "scion-repo-contrib", "GH_SCION_FRONTIERS__SCION_REPO_CONTRIB"},
		{"GoogleCloudPlatform", "scion", "GH_GOOGLECLOUDPLATFORM__SCION"},
		{"ptone", "scion", "GH_PTONE__SCION"},
		{"my-org", "my.repo", "GH_MY_ORG__MY_REPO"},
		{"my-org", "my.special.repo", "GH_MY_ORG__MY_SPECIAL_REPO"},
		{"acme-corp", "templates.git", "GH_ACME_CORP__TEMPLATES"},
		{"acme-corp", "templates.GIT", "GH_ACME_CORP__TEMPLATES"},
		{"acme-corp", "my.git.tools", "GH_ACME_CORP__MY_GIT_TOOLS"},
	}
	for _, tt := range tests {
		t.Run(tt.owner+"/"+tt.repo, func(t *testing.T) {
			if got := RepoKey(tt.owner, tt.repo); got != tt.want {
				t.Errorf("RepoKey(%q, %q) = %q, want %q", tt.owner, tt.repo, got, tt.want)
			}
		})
	}
}

// TestOwnerKey verifies the owner-level key derivation (GH_OWNER).
func TestOwnerKey(t *testing.T) {
	tests := []struct {
		owner string
		want  string
	}{
		{"scion-frontiers", "GH_SCION_FRONTIERS"},
		{"GoogleCloudPlatform", "GH_GOOGLECLOUDPLATFORM"},
		{"ptone", "GH_PTONE"},
		{"my-org", "GH_MY_ORG"},
	}
	for _, tt := range tests {
		t.Run(tt.owner, func(t *testing.T) {
			if got := OwnerKey(tt.owner); got != tt.want {
				t.Errorf("OwnerKey(%q) = %q, want %q", tt.owner, got, tt.want)
			}
		})
	}
}
