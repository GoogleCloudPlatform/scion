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

package api

import (
	"path"
	"strings"
)

// IsGitHubTokenEnvKey reports whether key is a GitHub token environment
// variable that git's credential helper, gh CLI, or sciontool gh-wrapper
// would pick up instead of a minted GitHub App installation token
// (Stage 2 design §5.1a).
func IsGitHubTokenEnvKey(key string) bool {
	switch key {
	case "GITHUB_TOKEN", "GH_TOKEN", "GH_ENTERPRISE_TOKEN", "GITHUB_ENTERPRISE_TOKEN":
		return true
	default:
		return false
	}
}

// IsBlockedInstallationGitHubEnvKey reports whether key must never be
// delivered from user/project/config/host storage when an agent's
// GitHubTokenPreference is "installation", and must always be stripped from
// API response views of AppliedConfig / InlineConfig / PendingConfig.
//
// This is the single shared definition used by the Hub, store serializers,
// Runtime Broker, and agent env builder.
func IsBlockedInstallationGitHubEnvKey(key string) bool {
	return IsGitHubTokenEnvKey(key) || key == "SCION_USER_GITHUB_TOKEN"
}

// blockedInstallationGitHubCredentialRelPaths are the home-relative file paths
// where gh CLI, git-credential-store, and libcurl/git read stored GitHub or
// HTTP git credentials:
//   - .config/gh/hosts.yml (and .yaml): gh CLI authentication store
//   - .git-credentials and .config/git/credentials: git-credential-store(1)
//     home and XDG credential files
//   - .netrc: libcurl / git netrc credential file
//
// Stored file-type secrets targeting any of these paths (under ~/, /root/,
// /home/<user>/, or /Users/<user>/, plus /etc/netrc) are withheld from
// installation-only agents on every push, pull, and list door so personal
// GitHub credentials cannot bypass the installation-only delivery contract.
var blockedInstallationGitHubCredentialRelPaths = map[string]struct{}{
	".config/gh/hosts.yml":    {},
	".config/gh/hosts.yaml":   {},
	".git-credentials":        {},
	".config/git/credentials": {},
	".netrc":                  {},
}

// IsBlockedInstallationGitHubCredentialFileTarget reports whether target is a
// file-secret target path that carries gh CLI or git/libcurl credentials and
// must be withheld from installation-only GitHub token agents.
func IsBlockedInstallationGitHubCredentialFileTarget(target string) bool {
	t := strings.TrimSpace(target)
	if t == "" {
		return false
	}
	t = strings.ReplaceAll(t, "\\", "/")

	var rel string
	switch {
	case strings.HasPrefix(t, "~/"):
		// Clean the remainder after ~/ using a leading slash so repeated
		// slashes and "." segments collapse without resolving beyond ~.
		cleaned := path.Clean("/" + strings.TrimPrefix(t, "~/"))
		rel = strings.TrimPrefix(cleaned, "/")
	case strings.HasPrefix(t, "/"):
		cleaned := path.Clean(t)
		lower := strings.ToLower(cleaned)
		if lower == "/etc/netrc" {
			return true
		}
		switch {
		case strings.HasPrefix(lower, "/root/"):
			rel = cleaned[len("/root/"):]
		case strings.HasPrefix(lower, "/home/"):
			rest := cleaned[len("/home/"):]
			_, after, ok := strings.Cut(rest, "/")
			if !ok || after == "" {
				return false
			}
			rel = after
		case strings.HasPrefix(lower, "/users/"):
			rest := cleaned[len("/users/"):]
			_, after, ok := strings.Cut(rest, "/")
			if !ok || after == "" {
				return false
			}
			rel = after
		default:
			return false
		}
	default:
		return false
	}

	_, blocked := blockedInstallationGitHubCredentialRelPaths[strings.ToLower(rel)]
	return blocked
}
