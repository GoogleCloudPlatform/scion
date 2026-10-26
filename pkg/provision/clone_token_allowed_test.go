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

import "testing"

// CloneTokenAllowed is the one decision every sciontool clone makes before
// offering the project's git token: only an https clone URL naming the
// recorded remote's host and owner/repo, and nothing without a recorded
// remote.
func TestCloneTokenAllowed(t *testing.T) {
	const remote = "github.com/org/repo"
	for _, tc := range []struct {
		name, remote, url string
		want              bool
	}{
		{"matching remote", remote, "https://github.com/org/repo.git", true},
		{"matching, no .git, trailing slash", remote, "https://github.com/org/repo/", true},
		{"matching, host case differs", remote, "https://GitHub.com/org/repo.git", true},
		{"recorded remote in URL form", "https://github.com/org/repo.git", "https://github.com/org/repo", true},
		{"enterprise host with port", "ghe.example.com:8443/team/app", "https://ghe.example.com:8443/team/app.git", true},
		{"label naming another host", remote, "https://mirror.example/org/repo.git", false},
		{"another host carrying the remote as a path", remote, "https://mirror.example/github.com/org/repo.git", false},
		{"same host, other owner", remote, "https://github.com/other/repo.git", false},
		{"same host, other repo", remote, "https://github.com/org/other.git", false},
		{"same host, other port", "ghe.example.com:8443/team/app", "https://ghe.example.com/team/app.git", false},
		{"no token remote", "", "https://github.com/org/repo.git", false},
		{"remote without a path", "github.com", "https://github.com/org/repo.git", false},
		{"http", remote, "http://github.com/org/repo.git", false},
		{"ssh", remote, "ssh://git@github.com/org/repo.git", false},
		{"scp form", remote, "git@github.com:org/repo.git", false},
		{"schemeless", remote, "github.com/org/repo", false},
		{"user info", remote, "https://user@github.com/org/repo.git", false},
		{"query", remote, "https://github.com/org/repo.git?x=1", false},
		{"fragment", remote, "https://github.com/org/repo.git#x", false},
		{"empty URL", remote, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := CloneTokenAllowed(tc.remote, tc.url); got != tc.want {
				t.Errorf("CloneTokenAllowed(%q, %q) = %v, want %v", tc.remote, tc.url, got, tc.want)
			}
		})
	}
}
