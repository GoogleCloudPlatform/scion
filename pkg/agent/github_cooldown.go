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

package agent

import (
	"net/http"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/githubfetch"
)

const (
	// GitHubCooldownDefault is how long a credential identity is held back
	// after a rate-limit response that carries neither Retry-After nor
	// X-RateLimit-Reset.
	GitHubCooldownDefault = githubfetch.CooldownDefault

	// GitHubCooldownMax caps how long a single rate-limit response can hold
	// a credential identity back.
	GitHubCooldownMax = githubfetch.CooldownMax

	// GitHubRateLimitedCode is the ResolveError / ResolveSkillError code
	// reported for a ref that failed because its credential identity is
	// rate limited by GitHub.
	GitHubRateLimitedCode = SkillErrCodeRateLimited
)

// GitHubRateLimitError is an alias for githubfetch.RateLimitError kept while
// callers in pkg/agent and pkg/hub migrate onto pkg/githubfetch.
type GitHubRateLimitError = githubfetch.RateLimitError

// GitHubCooldown is an alias for githubfetch.Cooldown kept while callers in
// pkg/agent and pkg/hub migrate onto pkg/githubfetch.
type GitHubCooldown = githubfetch.Cooldown

// NewGitHubCooldown returns an empty cooldown tracker.
func NewGitHubCooldown(now func() time.Time) *GitHubCooldown {
	return githubfetch.NewCooldown(now)
}

// SharedGitHubCooldown returns the process-wide cooldown tracker.
func SharedGitHubCooldown() *GitHubCooldown {
	return githubfetch.SharedCooldown()
}

// GitHubCooldownIdentity returns the cooldown identity for token.
func GitHubCooldownIdentity(token string) string {
	return githubfetch.CooldownIdentity(token)
}

// GitHubCooldownIdentityForInstallation returns the cooldown identity for a
// GitHub App installation ID.
func GitHubCooldownIdentityForInstallation(installID string) string {
	return githubfetch.CooldownIdentityForInstallation(installID)
}

// GitHubCooldownIdentityIsAnonymous reports whether identity is anonymous.
func GitHubCooldownIdentityIsAnonymous(identity string) bool {
	return githubfetch.CooldownIdentityIsAnonymous(identity)
}

// githubCooldownFor is retained for existing pkg/agent tests.
func githubCooldownFor(resp *http.Response, now time.Time) time.Duration {
	return githubfetch.CooldownDuration(resp.Header, now)
}

func parseRetryAfterSeconds(v string) (int64, bool) {
	return githubfetch.ParseRetryAfterSeconds(v)
}

func secondsUpTo(secs int64, max time.Duration) time.Duration {
	return githubfetch.SecondsUpTo(secs, max)
}

func isGitHubRateLimitResponse(resp *http.Response) (bool, error) {
	return githubfetch.IsHTTPResponseRateLimited(resp)
}
