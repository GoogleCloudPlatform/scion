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
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/hub/githubapp"
)

// A personal token may go only to an owner/repo on the GitHub host, compared
// on the canonical host: spoofed authorities, other ports and lookalikes are
// refused.
func TestPersonalTokenHostAllowed(t *testing.T) {
	gitHub := githubapp.WebHostForAPIBaseURL("")
	ghes := githubapp.WebHostForAPIBaseURL("https://ghe.example.com/api/v3")
	gheCom := githubapp.WebHostForAPIBaseURL("https://api.acme.ghe.com")
	for _, tc := range []struct {
		name, remote, host string
		want               bool
	}{
		{"normalized github.com remote", "github.com/org/repo", gitHub, true},
		{"https github.com URL", "https://github.com/org/repo.git", gitHub, true},
		{"letter case and trailing dot", "https://GitHub.com./Org/Repo", gitHub, true},
		{"scp-form ssh remote", "git@github.com:org/repo.git", gitHub, true},
		{"ssh URL with a port", "ssh://git@github.com:22/org/repo.git", gitHub, true},
		{"GHES host with GHES configured", "ghe.example.com/team/app", ghes, true},
		{"GHE.com host", "acme.ghe.com/team/app", gheCom, true},

		{"github.com when GHES is configured", "github.com/org/repo", ghes, false},
		{"attacker host", "attacker.example/org/repo", gitHub, false},
		{"github.com as a subdomain of another host", "github.com.attacker.example/org/repo", gitHub, false},
		{"github.com in the path", "attacker.example/github.com/org/repo", gitHub, false},
		{"https on another port", "https://github.com:8443/org/repo.git", gitHub, false},
		{"userinfo naming github.com", "https://github.com@attacker.example/org/repo.git", gitHub, false},
		{"ssh userinfo naming github.com", "ssh://git@github.com@attacker.example/org/repo.git", gitHub, false},
		{"a GitHub-owned but different host", "githubusercontent.com/org/repo", gitHub, false},
		{"punycode lookalike", "xn--gthub-n4a.com/org/repo", gitHub, false},
		{"host without a repository", "github.com", gitHub, false},
		{"owner without a repository", "github.com/org", gitHub, false},
		{"nested path", "github.com/org/repo/extra", gitHub, false},
		{"empty remote", "", gitHub, false},
		{"no GitHub host", "github.com/org/repo", "", false},
		{"unparsable API base", "github.com/org/repo", githubapp.WebHostForAPIBaseURL("https://user@api.github.com"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := personalTokenHostAllowed(tc.remote, tc.host); got != tc.want {
				t.Errorf("personalTokenHostAllowed(%q, %q) = %v, want %v", tc.remote, tc.host, got, tc.want)
			}
		})
	}
}
