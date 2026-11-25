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

import "testing"

func TestIsBlockedInstallationGitHubEnvKey(t *testing.T) {
	blocked := []string{
		"GITHUB_TOKEN",
		"GH_TOKEN",
		"GH_ENTERPRISE_TOKEN",
		"GITHUB_ENTERPRISE_TOKEN",
		"SCION_USER_GITHUB_TOKEN",
	}
	for _, k := range blocked {
		if !IsBlockedInstallationGitHubEnvKey(k) {
			t.Errorf("IsBlockedInstallationGitHubEnvKey(%q) = false, want true", k)
		}
	}

	allowed := []string{
		"",
		"SCION_GITHUB_APP_ENABLED",
		"SCION_GITHUB_TOKEN_EXPIRY",
		"SCION_GITHUB_TOKEN_PATH",
		"ANTHROPIC_API_KEY",
		"MY_GITHUB_TOKEN",
		"github_token",
	}
	for _, k := range allowed {
		if IsBlockedInstallationGitHubEnvKey(k) {
			t.Errorf("IsBlockedInstallationGitHubEnvKey(%q) = true, want false", k)
		}
	}

	got := BlockedInstallationGitHubEnvKeys()
	if len(got) != len(blocked) {
		t.Fatalf("BlockedInstallationGitHubEnvKeys() len = %d, want %d", len(got), len(blocked))
	}
	for _, k := range got {
		if !IsBlockedInstallationGitHubEnvKey(k) {
			t.Errorf("BlockedInstallationGitHubEnvKeys() returned %q which is not blocked by predicate", k)
		}
	}
	// Mutating the returned slice must not affect subsequent calls.
	got[0] = "MUTATED"
	if BlockedInstallationGitHubEnvKeys()[0] == "MUTATED" {
		t.Errorf("BlockedInstallationGitHubEnvKeys() returned an uncopied backing slice")
	}
}

func TestIsBlockedInstallationGitHubCredentialFileTarget(t *testing.T) {
	blocked := []string{
		"~/.config/gh/hosts.yml",
		"~/.config/gh/hosts.yaml",
		"~//.config/./gh/hosts.yml",
		"~/.CONFIG/GH/HOSTS.YML",
		"~/.git-credentials",
		"~/.config/git/credentials",
		"~/.netrc",
		"~/.NETRC",
		"  ~/.netrc  ",
		"/root/.config/gh/hosts.yml",
		"/root/.git-credentials",
		"/root/.config/git/credentials",
		"/root/.netrc",
		"/home/scion/.config/gh/hosts.yml",
		"/home/agent/.git-credentials",
		"/home/agent/.config/git/credentials",
		"/home/agent/.netrc",
		"/Users/dev/.config/gh/hosts.yml",
		"/etc/netrc",
		"/etc//netrc",
		`~\.config\gh\hosts.yml`,
	}
	for _, target := range blocked {
		if !IsBlockedInstallationGitHubCredentialFileTarget(target) {
			t.Errorf("IsBlockedInstallationGitHubCredentialFileTarget(%q) = false, want true", target)
		}
	}

	allowed := []string{
		"",
		"~/.config/gh/config.yml",
		"~/.gitconfig",
		"~/.ssh/id_rsa",
		"~/.aws/credentials",
		"~/.scion/trigger-event.json",
		"/home/scion/project/.netrc",
		"/tmp/.netrc",
		"/etc/hosts",
		".netrc",
		"~",
		"/home/scion",
	}
	for _, target := range allowed {
		if IsBlockedInstallationGitHubCredentialFileTarget(target) {
			t.Errorf("IsBlockedInstallationGitHubCredentialFileTarget(%q) = true, want false", target)
		}
	}
}
