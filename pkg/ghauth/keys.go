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

// Package ghauth holds the naming convention for GitHub credentials stored
// as project secrets. Skill resolution and remote template and harness-config
// imports both look credentials up by these names, so they share this one
// implementation.
package ghauth

import "strings"

// DefaultKey is the generic GitHub credential name, used when no
// repo-specific or owner-level credential is set.
const DefaultKey = "GITHUB_TOKEN"

// NormalizeName uppercases a GitHub owner or repository name and replaces
// hyphens and dots with underscores, producing a valid env-var-style key
// segment.
func NormalizeName(name string) string {
	s := strings.ToUpper(name)
	s = strings.ReplaceAll(s, "-", "_")
	s = strings.ReplaceAll(s, ".", "_")
	return s
}

// RepoKey returns the repo-specific credential name for owner/repo:
// GH_{OWNER}__{REPO}. A trailing ".git" on repo is dropped first, so
// "templates.git" (as in a clone URL) names the same credential as
// "templates"; GitHub repository names cannot end in ".git". A repo named
// exactly ".git" is kept as is rather than reduced to an empty name.
func RepoKey(owner, repo string) string {
	if len(repo) > len(".git") && strings.HasSuffix(strings.ToLower(repo), ".git") {
		repo = repo[:len(repo)-len(".git")]
	}
	return "GH_" + NormalizeName(owner) + "__" + NormalizeName(repo)
}

// OwnerKey returns the owner-level credential name for owner: GH_{OWNER}.
func OwnerKey(owner string) string {
	return "GH_" + NormalizeName(owner)
}
