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
		{"acme-corp", ".git", "GH_ACME_CORP___GIT"},
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

// TestLookup verifies the convention order: the repo key beats the owner key,
// empty values count as unset, and GITHUB_TOKEN is never consulted.
func TestLookup(t *testing.T) {
	tests := []struct {
		name      string
		values    map[string]string
		wantValue string
		wantName  string
	}{
		{"repo key beats owner key", map[string]string{"GH_ACME_CORP__TEMPLATES": "fake-repo", "GH_ACME_CORP": "fake-owner"}, "fake-repo", "GH_ACME_CORP__TEMPLATES"},
		{"owner key when no repo key", map[string]string{"GH_ACME_CORP": "fake-owner"}, "fake-owner", "GH_ACME_CORP"},
		{"empty repo value falls through", map[string]string{"GH_ACME_CORP__TEMPLATES": "", "GH_ACME_CORP": "fake-owner"}, "fake-owner", "GH_ACME_CORP"},
		{"default key is not consulted", map[string]string{DefaultKey: "fake-default"}, "", ""},
		{"another owner is ignored", map[string]string{"GH_OTHER": "fake-other"}, "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var asked []string
			gotValue, gotName := Lookup("acme-corp", "templates", func(name string) string {
				asked = append(asked, name)
				return tt.values[name]
			})
			if gotValue != tt.wantValue || gotName != tt.wantName {
				t.Errorf("Lookup = (%q, %q), want (%q, %q)", gotValue, gotName, tt.wantValue, tt.wantName)
			}
			for _, name := range asked {
				if name == DefaultKey {
					t.Errorf("Lookup asked for %s", DefaultKey)
				}
			}
		})
	}
}
