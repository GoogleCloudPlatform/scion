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
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// IsGitRepo returns true if the current working directory is inside a git repository.
func IsGitRepo() bool {
	return IsGitRepoDir("")
}

// IsGitRepoDir returns true if the specified directory is inside a git repository.
func IsGitRepoDir(dir string) bool {
	args := []string{"rev-parse", "--is-inside-work-tree"}
	cmd := exec.Command("git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	err := cmd.Run()
	return err == nil
}

// GetGitVersion returns the git version string and the path to the git binary used.
func GetGitVersion() (string, string, error) {
	gitPath := os.Getenv("SCION_GIT_BINARY")
	if gitPath == "" {
		var err error
		gitPath, err = exec.LookPath("git")
		if err != nil {
			return "", "", err
		}
	}
	cmd := exec.Command(gitPath, "--version")
	output, err := cmd.CombinedOutput()
	if err != nil {
		return "", gitPath, err
	}
	// Output is usually "git version 2.47.0"
	version := strings.TrimPrefix(strings.TrimSpace(string(output)), "git version ")
	return version, gitPath, nil
}

// CheckGitVersion returns an error if the git version is less than 2.47.0.
func CheckGitVersion() error {
	version, gitPath, err := GetGitVersion()
	if err != nil {
		return fmt.Errorf("failed to get git version: %w", err)
	}

	if err := CompareGitVersion(version, 2, 47); err != nil {
		return fmt.Errorf("git version 2.47.0 or newer is required; scion requires worktree support with relative paths (found %s at %s)", version, gitPath)
	}

	return nil
}

// CompareGitVersion returns an error if the version string is less than major.minor
func CompareGitVersion(version string, minMajor, minMinor int) error {
	// Simple version comparison
	// Format is expected to start with major.minor
	parts := strings.Split(version, ".")
	if len(parts) < 2 {
		return fmt.Errorf("unexpected git version format: %s", version)
	}

	var major, minor int
	if _, err := fmt.Sscanf(parts[0], "%d", &major); err != nil {
		return fmt.Errorf("failed to parse git major version from %s: %w", parts[0], err)
	}
	if _, err := fmt.Sscanf(parts[1], "%d", &minor); err != nil {
		return fmt.Errorf("failed to parse git minor version from %s: %w", parts[1], err)
	}

	if major < minMajor || (major == minMajor && minor < minMinor) {
		return fmt.Errorf("version %s is less than %d.%d", version, minMajor, minMinor)
	}

	return nil
}

// RepoRoot returns the absolute path to the root of the git repository.
func RepoRoot() (string, error) {
	return RepoRootDir("")
}

// RepoRootDir returns the absolute path to the root of the git repository for the specified directory.
func RepoRootDir(dir string) (string, error) {
	args := []string{"rev-parse", "--show-toplevel"}
	cmd := exec.Command("git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	output, err := cmd.CombinedOutput()
	if err != nil {
		// If rev-parse fails, it might be because we're in a .git directory.
		// Try running from parent.
		if dir != "" {
			parent := filepath.Dir(dir)
			if parent != dir {
				return RepoRootDir(parent)
			}
		}
		return "", err
	}
	return strings.TrimSpace(string(output)), nil
}

// GetCommonGitDir returns the absolute path to the common git directory (the main .git dir).
func GetCommonGitDir(dir string) (string, error) {
	args := []string{"rev-parse", "--git-common-dir"}
	cmd := exec.Command("git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	output, err := cmd.CombinedOutput()
	if err != nil {
		return "", err
	}
	commonDir := strings.TrimSpace(string(output))
	if !filepath.IsAbs(commonDir) {
		if dir == "" {
			var err error
			dir, err = os.Getwd()
			if err != nil {
				return "", err
			}
		}
		commonDir = filepath.Join(dir, commonDir)
	}
	return filepath.Clean(commonDir), nil
}

// IsIgnored returns true if the given path is ignored by git.
func IsIgnored(path string) bool {
	cmd := exec.Command("git", "check-ignore", "-q", path)
	err := cmd.Run()
	return err == nil
}

// CreateWorktree creates a new git worktree at the specified path with a new branch.
func CreateWorktree(path, branch string) error {
	root, err := RepoRootDir(filepath.Dir(path))
	if err != nil {
		return fmt.Errorf("failed to find repo root for worktree: %w", err)
	}

	// git worktree add --relative-paths -b <branch> <path>
	// We run from root to ensure --relative-paths are calculated from root
	cmd := exec.Command("git", "worktree", "add", "--relative-paths", "-b", branch, path)
	cmd.Dir = root
	if output, err := cmd.CombinedOutput(); err != nil {
		outputStr := string(output)
		// If branch already exists, try to just add it
		if strings.Contains(outputStr, "already exists") {
			cmd = exec.Command("git", "worktree", "add", "--relative-paths", path, branch)
			cmd.Dir = root
			if output, err := cmd.CombinedOutput(); err != nil {
				outputStr = string(output)
				if strings.Contains(outputStr, "already checked out") {
					return fmt.Errorf("branch '%s' is already checked out in another worktree", branch)
				}
				return fmt.Errorf("failed to create worktree: %s", strings.TrimSpace(outputStr))
			}
			return nil
		}
		return fmt.Errorf("failed to create worktree: %s", strings.TrimSpace(outputStr))
	}
	return nil
}

// RemoveWorktree removes a git worktree at the specified path.
func RemoveWorktree(path string, deleteBranch bool) (bool, error) {
	var branchName string
	var repoRoot string
	branchDeleted := false

	if deleteBranch {
		// Get the common git dir (main repo's .git dir)
		cmd := exec.Command("git", "-C", path, "rev-parse", "--git-common-dir")
		output, err := cmd.Output()
		if err == nil {
			commonDir := strings.TrimSpace(string(output))
			if !filepath.IsAbs(commonDir) {
				// If relative, it's relative to the worktree root
				commonDir = filepath.Join(path, commonDir)
			}
			repoRoot = filepath.Dir(commonDir)
		}

		// Get branch name
		cmd = exec.Command("git", "-C", path, "branch", "--show-current")
		output, err = cmd.Output()
		if err == nil {
			branchName = strings.TrimSpace(string(output))
		}
	}

	// Remove the worktree.
	// We run this from the system root or anywhere to ensure we're not "in" the dir
	Debugf("RemoveWorktree: running git worktree remove for %s", path)
	worktreeRemoveStart := time.Now()
	cmd := exec.Command("git", "worktree", "remove", path, "--force")
	if err := cmd.Run(); err != nil {
		Debugf("RemoveWorktree: git worktree remove failed in %v: %v", time.Since(worktreeRemoveStart), err)
		return false, err
	}
	Debugf("RemoveWorktree: git worktree remove completed in %v", time.Since(worktreeRemoveStart))

	if deleteBranch && branchName != "" && repoRoot != "" {
		// Now delete the branch from the main repo
		Debugf("RemoveWorktree: deleting branch %s", branchName)
		branchDeleteStart := time.Now()
		cmd := exec.Command("git", "-C", repoRoot, "branch", "-D", branchName)
		if err := cmd.Run(); err == nil {
			branchDeleted = true
			Debugf("RemoveWorktree: branch delete completed in %v", time.Since(branchDeleteStart))
		} else {
			Debugf("RemoveWorktree: branch delete failed in %v: %v", time.Since(branchDeleteStart), err)
		}
	}
	return branchDeleted, nil
}

// PruneWorktrees prunes worktree information for worktrees that no longer exist.
func PruneWorktrees() error {
	cmd := exec.Command("git", "worktree", "prune")
	return cmd.Run()
}

// FindWorktreeByBranch returns the absolute path of the worktree checked out to the specified branch.
// It returns an empty string if not found.
func FindWorktreeByBranch(branchName string) (string, error) {
	cmd := exec.Command("git", "worktree", "list", "--porcelain")
	output, err := cmd.Output()
	if err != nil {
		return "", err
	}

	blocks := strings.Split(string(output), "\n\n")
	targetRef := "refs/heads/" + branchName

	for _, block := range blocks {
		lines := strings.Split(block, "\n")
		var path string
		var branch string
		for _, line := range lines {
			if strings.HasPrefix(line, "worktree ") {
				path = strings.TrimPrefix(line, "worktree ")
				if strings.HasPrefix(path, "\"") {
					if unquoted, err := strconv.Unquote(path); err == nil {
						path = unquoted
					}
				}
			} else if strings.HasPrefix(line, "branch ") {
				branch = strings.TrimPrefix(line, "branch ")
			}
		}
		if branch == targetRef {
			return path, nil
		}
	}
	return "", nil
}

// BranchExists returns true if the branch exists in the repository.
func BranchExists(branchName string) bool {
	cmd := exec.Command("git", "show-ref", "--verify", "--quiet", "refs/heads/"+branchName)
	err := cmd.Run()
	return err == nil
}

// GetGitRemote returns the origin remote URL of the current repository.
// Returns empty string if not in a git repo or no origin remote exists.
func GetGitRemote() string {
	return GetGitRemoteDir("")
}

// GetGitRemoteDir returns the origin remote URL of the repository at the specified directory.
func GetGitRemoteDir(dir string) string {
	cmd := exec.Command("git", "remote", "get-url", "origin")
	if dir != "" {
		cmd.Dir = dir
	}
	output, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(output))
}

// ExtractRepoName extracts the repository name from a git remote URL.
// Handles SSH (git@github.com:org/repo.git) and HTTPS (https://github.com/org/repo.git) formats.
func ExtractRepoName(remoteURL string) string {
	if remoteURL == "" {
		return ""
	}

	// Remove trailing .git
	remoteURL = strings.TrimSuffix(remoteURL, ".git")

	// Handle SSH format: git@github.com:org/repo
	if strings.Contains(remoteURL, ":") && strings.Contains(remoteURL, "@") {
		parts := strings.Split(remoteURL, ":")
		if len(parts) == 2 {
			pathParts := strings.Split(parts[1], "/")
			if len(pathParts) > 0 {
				return pathParts[len(pathParts)-1]
			}
		}
	}

	// Handle HTTPS format: https://github.com/org/repo
	parts := strings.Split(remoteURL, "/")
	if len(parts) > 0 {
		return parts[len(parts)-1]
	}

	return remoteURL
}

// NormalizeGitRemote normalizes a git remote URL to a canonical form for consistent matching.
// It removes protocols (https, http, ssh, git), handles SSH format (git@host:path),
// removes the .git suffix, and lowercases the host portion.
// Examples:
//   - https://github.com/org/repo.git -> github.com/org/repo
//   - git@github.com:org/repo.git -> github.com/org/repo
func NormalizeGitRemote(remote string) string {
	if remote == "" {
		return ""
	}

	// Lowercase for consistent prefix/suffix matching
	remote = strings.ToLower(remote)

	// Remove protocol prefix
	remote = strings.TrimPrefix(remote, "https://")
	remote = strings.TrimPrefix(remote, "http://")
	remote = strings.TrimPrefix(remote, "ssh://")
	remote = strings.TrimPrefix(remote, "git://")

	// Handle SSH format (git@host:path)
	if strings.HasPrefix(remote, "git@") {
		remote = strings.TrimPrefix(remote, "git@")
		remote = strings.Replace(remote, ":", "/", 1)
	}

	// Remove .git suffix
	remote = strings.TrimSuffix(remote, ".git")

	return remote
}
