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
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// parseDefaultBranch extracts the default branch name from `git ls-remote --symref` output.
// The expected format is: "ref: refs/heads/<branch>\tHEAD"
// Returns the branch name or empty string if not found.
func parseDefaultBranch(output string) string {
	for _, line := range strings.Split(output, "\n") {
		if strings.HasPrefix(line, "ref: refs/heads/") && strings.Contains(line, "\tHEAD") {
			branch := strings.TrimPrefix(line, "ref: refs/heads/")
			branch = strings.TrimSuffix(branch, "\tHEAD")
			return strings.TrimSpace(branch)
		}
	}
	return ""
}

// detectDefaultBranch probes a git remote to detect its default branch.
// Returns the branch name or empty string on failure.
func (s *Server) detectDefaultBranch(cloneURL string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "ls-remote", "--symref", cloneURL, "HEAD")
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	output, err := cmd.Output()
	if err != nil {
		return ""
	}
	return parseDefaultBranch(string(output))
}

// resolveDefaultBranch determines the default branch for a project.
// The git remote is the authoritative source — it always probes the remote
// first so that branch renames (e.g. main → opencode-support) are picked
// up automatically. The stored label is only used as a fallback when the
// remote is unreachable or the detection command fails.
func (s *Server) resolveDefaultBranch(cloneURL, storedBranch string) string {
	if cloneURL != "" {
		if detected := s.detectDefaultBranch(cloneURL); detected != "" {
			return detected
		}
	}
	if storedBranch != "" {
		return storedBranch
	}
	return "main"
}

// sanitizeGitOutput removes sensitive data (tokens) from git output.
func sanitizeGitOutput(output, token string) string {
	if token == "" {
		return output
	}
	return strings.ReplaceAll(output, token, "TOKEN_REDACTED")
}

// isAuthError checks if git output indicates an authentication failure.
func isAuthError(output string) bool {
	lower := strings.ToLower(output)
	return strings.Contains(lower, "authentication") ||
		strings.Contains(lower, "authorization") ||
		strings.Contains(lower, "403") ||
		strings.Contains(lower, "401") ||
		strings.Contains(lower, "access denied")
}

// buildAuthenticatedURL constructs a git clone URL with embedded credentials.
func buildAuthenticatedURL(cloneURL, token string) string {
	if token == "" {
		return cloneURL
	}
	if strings.HasPrefix(cloneURL, "https://") {
		if strings.Contains(cloneURL, "github.com") {
			return strings.Replace(cloneURL, "https://", "https://"+token+"@", 1)
		}
		return strings.Replace(cloneURL, "https://", "https://x-access-token:"+token+"@", 1)
	}
	return cloneURL
}

// formatCloneError returns a user-friendly error for git clone failures.
func formatCloneError(output, token string) error {
	sanitized := sanitizeGitOutput(output, token)
	return fmt.Errorf("git clone failed: %s", strings.TrimSpace(sanitized))
}
