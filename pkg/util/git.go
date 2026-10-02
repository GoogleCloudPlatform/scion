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
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
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
	// Output is usually "git version 2.48.0"
	version := strings.TrimPrefix(strings.TrimSpace(string(output)), "git version ")
	return version, gitPath, nil
}

// CheckGitVersion returns an error if the git version is less than 2.48.0.
//
// 2.48 is required: `git worktree add --relative-paths` first shipped in
// git 2.48; 2.47.x rejects the flag.
func CheckGitVersion() error {
	version, gitPath, err := GetGitVersion()
	if err != nil {
		return fmt.Errorf("failed to get git version: %w", err)
	}

	if err := CompareGitVersion(version, 2, 48); err != nil {
		return fmt.Errorf("git version 2.48.0 or newer is required; scion requires worktree support with relative paths (found %s at %s)", version, gitPath)
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
func IsIgnored(dir, path string) bool {
	cmd := exec.Command("git", "check-ignore", "-q", path)
	if dir != "" {
		cmd.Dir = dir
	}
	err := cmd.Run()
	return err == nil
}

// CreateWorktree creates a new git worktree at the specified path with a new branch.
func CreateWorktree(path, branch string) error {
	// Guard: refuse to create worktrees inside an agent container.
	// SCION_HOST_UID is set by the runtime when launching containers.
	// Creating worktrees inside containers produces path-identity mismatches
	// because --relative-paths are computed against the container mount layout,
	// not the host filesystem.
	if os.Getenv("SCION_HOST_UID") != "" {
		return fmt.Errorf("cannot create worktree: running inside an agent container (SCION_HOST_UID is set)")
	}

	// Resolve the main repository root via the common git dir. This is correct
	// even when called from within a worktree: GetCommonGitDir returns the
	// shared .git directory of the main repo, whose parent is the true root.
	dir := filepath.Dir(path)
	commonDir, err := GetCommonGitDir(dir)
	if err != nil {
		return fmt.Errorf("failed to find git common dir for worktree: %w", err)
	}
	root := filepath.Dir(commonDir)

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

// ErrPathNotContained is returned by RemoveWorktree when path's resolved
// (symlink-free) location does not lie under base's resolved location.
//
// Callers MUST NOT fall back to a raw recursive removal of path when they see
// this error — the whole point of the check is that path's real, on-disk
// location may not be what its lexical form suggests (an intermediate path
// component, e.g. a worktrees directory or the leaf itself, may be a symlink
// pointing outside base). Falling back to removing path anyway would defeat
// the check entirely.
var ErrPathNotContained = errors.New("path resolves outside the expected base directory")

// pathResolvedUnderBase reports whether candidate, after resolving all
// symlinks, is a strict descendant of base, also resolved. It fails closed
// (returns false) on any resolution error, on base==candidate (removing the
// base itself is never intended), and on non-existent paths — existence must
// be checked by the caller first.
// pathResolvedUnderBase also returns the resolved candidate path so a caller
// that proceeds after a true result can act on that same resolved location
// (see RemoveWorktree) rather than re-resolving it later — closing the
// window between "checked" and "used" during which the lexical path could
// start pointing somewhere else.
func pathResolvedUnderBase(base, candidate string) (ok bool, resolvedCandidate string, err error) {
	resolvedBase, err := filepath.EvalSymlinks(base)
	if err != nil {
		return false, "", fmt.Errorf("resolve base %q: %w", base, err)
	}
	resolvedCandidate, err = filepath.EvalSymlinks(candidate)
	if err != nil {
		return false, "", fmt.Errorf("resolve candidate %q: %w", candidate, err)
	}
	rel, err := filepath.Rel(resolvedBase, resolvedCandidate)
	if err != nil {
		return false, "", nil
	}
	if rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return false, "", nil
	}
	return true, resolvedCandidate, nil
}

// RemoveWorktree removes a git worktree at the specified path.
//
// base is the expected repo/worktree-parent root that path must resolve
// under; both must be non-empty absolute paths. Before touching anything on
// disk, RemoveWorktree resolves symlinks on both base and path and refuses
// (returning ErrPathNotContained) unless path's real location is a strict
// descendant of base's real location. This guards against a path that is
// lexically inside base but, through a symlinked intermediate directory or a
// symlinked leaf, actually resolves outside it — a caller holding such a
// path (e.g. one sourced from stored/discovered state rather than freshly
// created here) must not have it silently honored.
//
// Instead of using "git worktree remove" (which does its own directory
// deletion and can trigger macOS autofs timeouts on symlinks pointing to
// container-internal paths), this function:
//  1. Gathers git metadata (branch name, repo root) while the worktree exists.
//  2. Removes the worktree directory using RemoveAllSafe (which uses unlinkat
//     to avoid autofs triggers).
//  3. Runs "git worktree prune" to clean up the now-stale worktree record.
func RemoveWorktree(base, path string, deleteBranch bool) (bool, error) {
	// Every failure from here through the containment check is a pre-removal
	// validation failure, not a removal-mechanics failure — none of them have
	// proven path safe to act on, so all of them wrap ErrPathNotContained
	// (not just the explicit "not contained" case below). A caller that only
	// treats the explicit case as no-fallback-eligible would otherwise fall
	// back to a raw RemoveAllSafe(path) on, for example, an unresolvable
	// symlink loop — exactly the unvalidated recursive removal this whole
	// check exists to prevent.
	if base == "" || path == "" {
		return false, fmt.Errorf("RemoveWorktree: base and path must both be non-empty: %w", ErrPathNotContained)
	}
	if !filepath.IsAbs(base) || !filepath.IsAbs(path) {
		return false, fmt.Errorf("RemoveWorktree: base and path must both be absolute paths: %w", ErrPathNotContained)
	}

	if _, statErr := os.Lstat(path); statErr != nil {
		if errors.Is(statErr, fs.ErrNotExist) {
			return false, nil // nothing to remove
		}
		return false, fmt.Errorf("RemoveWorktree: lstat %q: %w: %w", path, statErr, ErrPathNotContained)
	}

	contained, resolvedPath, err := pathResolvedUnderBase(base, path)
	if err != nil {
		return false, fmt.Errorf("RemoveWorktree: %w: %w", err, ErrPathNotContained)
	}
	if !contained {
		return false, fmt.Errorf("RemoveWorktree: %q does not resolve under %q: %w", path, base, ErrPathNotContained)
	}

	var branchName string
	var repoRoot string
	branchDeleted := false

	// Get the common git dir (main repo's .git dir) — needed for both
	// pruning and optional branch deletion.
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

	if deleteBranch {
		// Get branch name
		cmd = exec.Command("git", "-C", path, "branch", "--show-current")
		output, err = cmd.Output()
		if err == nil {
			branchName = strings.TrimSpace(string(output))
		}
	}

	// Remove the worktree directory ourselves using RemoveAllSafe, which
	// uses unlinkat for symlinks to avoid triggering macOS autofs timeouts.
	// This replaces "git worktree remove" which uses its own (slow) deletion.
	// Act on resolvedPath (computed above), not the lexical path: using the
	// lexical form here would re-open the check-then-use window the
	// containment check just closed — path's containment was proven for its
	// resolved location, not for whatever the lexical string might resolve to
	// if something changed between the check and this call.
	Debugf("RemoveWorktree: removing worktree directory %s via RemoveAllSafe", resolvedPath)
	removeStart := time.Now()
	if err := RemoveAllSafe(resolvedPath); err != nil {
		Debugf("RemoveWorktree: RemoveAllSafe failed in %v: %v", time.Since(removeStart), err)
		return false, err
	}
	Debugf("RemoveWorktree: directory removal completed in %v", time.Since(removeStart))

	// Prune stale worktree records — git still has a reference to the
	// directory we just deleted; pruning cleans that up.
	if repoRoot != "" {
		Debugf("RemoveWorktree: pruning stale worktree records in %s", repoRoot)
		_ = PruneWorktreesIn(repoRoot)
	} else {
		_ = PruneWorktrees()
	}

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
// It runs from the current working directory.
func PruneWorktrees() error {
	// Guard: skip pruning inside agent containers. SCION_HOST_UID is set by the
	// runtime when launching containers. Pruning inside a container can destroy
	// worktree metadata that appears stale from the container's mount layout but
	// is actively used by sibling agents on the host.
	if os.Getenv("SCION_HOST_UID") != "" {
		return nil
	}
	cmd := exec.Command("git", "worktree", "prune")
	return cmd.Run()
}

// PruneWorktreesIn prunes worktree information from the specified repository root.
func PruneWorktreesIn(repoRoot string) error {
	// Guard: skip pruning inside agent containers (see PruneWorktrees).
	if os.Getenv("SCION_HOST_UID") != "" {
		return nil
	}
	cmd := exec.Command("git", "-C", repoRoot, "worktree", "prune")
	return cmd.Run()
}

// DeleteBranchIn deletes a git branch from the specified repository root.
// Returns true if the branch was deleted, false otherwise.
func DeleteBranchIn(repoRoot, branchName string) bool {
	cmd := exec.Command("git", "-C", repoRoot, "branch", "-D", branchName)
	return cmd.Run() == nil
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

// IsRegisteredWorktree reports whether path is one of the worktrees git
// itself has registered for the repository at repoRoot, via
// `git -C repoRoot worktree list --porcelain`. Matching is by resolved
// (symlink-free) equality against each registered worktree path — not a
// path-prefix guess — because `git worktree add` accepts any destination
// the caller names and there is no fixed location every worktree must live
// under. `git worktree list`'s first entry is always the repository's main
// worktree, but that is not necessarily repoRoot itself: repoRoot may be a
// linked worktree of the same repository (a project can live in one), in
// which case the main worktree's own path never equals repoRoot even though
// both share the same repository. The main worktree is instead recognized
// structurally, the same way a linked one is: its .git is a directory (not
// a linked worktree's gitdir-pointer file), and that directory, resolved,
// is the repository's own common git directory.
//
// A failure listing worktrees (repoRoot is not a git repository, git is
// unavailable, and so on) returns (false, err). Callers must treat that as
// "membership could not be verified," never as "verified true" — this
// function does not fail open.
// splitPorcelainRecords splits git porcelain-format output into its
// blank-line-separated records, normalizing CRLF line endings to LF first.
// A "\r\n\r\n" blank-line separator contains no "\n\n" substring (the two
// newlines have a "\r" between them), so splitting on "\n\n" without this
// normalization would silently fail to separate records at all when git's
// output uses CRLF, and a surviving "\r" on a "worktree <path>" line would
// end up as a trailing byte on the parsed path.
func splitPorcelainRecords(output string) []string {
	normalized := strings.ReplaceAll(output, "\r\n", "\n")
	return strings.Split(normalized, "\n\n")
}

func IsRegisteredWorktree(repoRoot, path string) (bool, error) {
	resolvedPath, err := filepath.EvalSymlinks(path)
	if err != nil {
		return false, fmt.Errorf("resolve %s: %w", path, err)
	}
	commonDir, err := GetCommonGitDir(repoRoot)
	if err != nil {
		return false, fmt.Errorf("git common dir for %s: %w", repoRoot, err)
	}
	resolvedCommonDir, err := filepath.EvalSymlinks(commonDir)
	if err != nil {
		return false, fmt.Errorf("resolve %s: %w", commonDir, err)
	}
	worktreesDir := filepath.Join(resolvedCommonDir, "worktrees")

	cmd := exec.Command("git", "-C", repoRoot, "worktree", "list", "--porcelain")
	output, err := cmd.Output()
	if err != nil {
		return false, fmt.Errorf("git worktree list: %w", err)
	}

	// Porcelain records are separated by a blank line; each holds a
	// "worktree <path>" line and, for one that git considers a candidate
	// for `git worktree prune`, a "prunable ..." line.
	for _, record := range splitPorcelainRecords(string(output)) {
		var wtPath string
		prunable := false
		for _, line := range strings.Split(record, "\n") {
			switch {
			case strings.HasPrefix(line, "worktree "):
				wtPath = strings.TrimPrefix(line, "worktree ")
				if strings.HasPrefix(wtPath, "\"") {
					if unquoted, uerr := strconv.Unquote(wtPath); uerr == nil {
						wtPath = unquoted
					}
				}
			case strings.HasPrefix(line, "prunable"):
				prunable = true
			}
		}
		if wtPath == "" || prunable {
			// A prunable record — git itself considers the registration
			// stale (its gitdir pointer no longer resolves) — is not
			// membership: a plain directory later created at the same path
			// must not inherit the identity of a worktree that no longer
			// really exists there.
			//
			// This specific prunable reason (gitdir file points to a
			// non-existent location) cannot be exercised independently of
			// isLinkedWorktreeOf's own check in a real git-backed test:
			// both read the existence of the exact same file (wtPath's own
			// .git), so recreating that file with content valid enough for
			// isLinkedWorktreeOf to accept also, as an unavoidable side
			// effect, clears this prunable reason in git's own listing
			// before IsRegisteredWorktree ever sees it. Confirmed
			// empirically against a real git repository.
			continue
		}

		resolvedWt, err := filepath.EvalSymlinks(wtPath)
		if err != nil || resolvedWt != resolvedPath {
			continue
		}

		if isMainWorktreeOf(wtPath, resolvedCommonDir) {
			return true, nil
		}
		if isLinkedWorktreeOf(wtPath, worktreesDir) {
			return true, nil
		}
	}
	return false, nil
}

// isMainWorktreeOf reports whether wtPath is the repository's main
// worktree, identified structurally rather than by comparing it against a
// caller-supplied repoRoot: wtPath's .git is a directory (not a linked
// worktree's gitdir-pointer file), and that directory, resolved, is the
// repository's own common git directory (resolvedCommonDir). This still
// works when repoRoot itself names a linked worktree of the same
// repository, a shape where wtPath (the real main worktree) never equals
// repoRoot even though both belong to the same repository.
//
// This checks wtPath's .git at the time of the call; the caller acts on the
// result afterward. A change to wtPath's .git between this check and that
// later use is out of scope here, the same validate-then-act model the rest
// of this package's path checks use.
func isMainWorktreeOf(wtPath, resolvedCommonDir string) bool {
	gitFile := filepath.Join(wtPath, ".git")
	info, err := os.Lstat(gitFile)
	if err != nil || !info.IsDir() {
		return false
	}
	resolvedGitDir, err := filepath.EvalSymlinks(gitFile)
	if err != nil {
		return false
	}
	return resolvedGitDir == resolvedCommonDir
}

// isLinkedWorktreeOf reports whether wtPath's .git file points into
// worktreesDir — the repository's own worktrees/ administrative directory —
// confirming wtPath is a linked worktree git itself created for this
// repository, not merely a directory whose path happens to match a
// registration (matching path alone is exactly what a stale-then-recreated
// directory can satisfy without being a real worktree).
func isLinkedWorktreeOf(wtPath, worktreesDir string) bool {
	gitFile := filepath.Join(wtPath, ".git")
	info, err := os.Lstat(gitFile)
	if err != nil || info.IsDir() {
		return false
	}
	content, err := os.ReadFile(gitFile)
	if err != nil {
		return false
	}
	line := strings.TrimSpace(string(content))
	const prefix = "gitdir: "
	if !strings.HasPrefix(line, prefix) {
		return false
	}
	target := strings.TrimPrefix(line, prefix)
	if !filepath.IsAbs(target) {
		target = filepath.Join(wtPath, target)
	}
	resolvedTarget, err := filepath.EvalSymlinks(target)
	if err != nil {
		return false
	}
	// A real linked worktree's gitdir always names a specific entry under
	// worktrees/, never the worktrees directory itself -- there is no
	// registration that is the whole administrative directory, so a gitdir
	// resolving to exactly worktreesDir is not a match.
	return strings.HasPrefix(resolvedTarget, worktreesDir+string(filepath.Separator))
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
// It removes protocols (https, http, ssh, git), strips user info (credentials, tokens),
// handles SSH format (git@host:path), removes the .git suffix, and lowercases the host.
// This ensures the same repository produces the same normalized string regardless of
// access method (SSH key, HTTPS token, plain HTTPS).
// Examples:
//   - https://github.com/org/repo.git -> github.com/org/repo
//   - git@github.com:org/repo.git -> github.com/org/repo
//   - https://x-access-token:TOKEN@github.com/org/repo.git -> github.com/org/repo
//   - ssh://git@github.com/org/repo.git -> github.com/org/repo
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

	// Handle SSH shorthand format (git@host:path → host/path)
	if strings.HasPrefix(remote, "git@") {
		remote = strings.TrimPrefix(remote, "git@")
		remote = strings.Replace(remote, ":", "/", 1)
	}

	// Strip user info (user@, user:pass@) for scheme-based URLs.
	// This handles token-authenticated HTTPS URLs like x-access-token:TOKEN@github.com/...
	if atIdx := strings.Index(remote, "@"); atIdx >= 0 {
		slashIdx := strings.Index(remote, "/")
		if slashIdx < 0 || atIdx < slashIdx {
			remote = remote[atIdx+1:]
		}
	}

	// Remove trailing slashes and .git suffix
	remote = strings.TrimRight(remote, "/")
	remote = strings.TrimSuffix(remote, ".git")

	return remote
}

// IsGitURL returns true if the string looks like a valid remote git URL.
// Accepts HTTPS, SSH shorthand (git@host:path), ssh://, and git:// schemes.
// Rejects empty strings, local paths, bare hostnames, and strings without a path containing '/'.
func IsGitURL(s string) bool {
	if s == "" {
		return false
	}

	// Reject local paths (absolute or relative)
	if strings.HasPrefix(s, "/") || strings.HasPrefix(s, "./") || strings.HasPrefix(s, "../") {
		return false
	}

	// SSH shorthand: git@host:org/repo
	if strings.HasPrefix(s, "git@") {
		// Must have a colon separating host from path, and the path must contain '/'
		colonIdx := strings.Index(s, ":")
		if colonIdx < 0 || colonIdx == len(s)-1 {
			return false
		}
		path := s[colonIdx+1:]
		return strings.Contains(path, "/")
	}

	// Scheme-based URLs
	for _, scheme := range []string{"https://", "http://", "ssh://", "git://"} {
		if strings.HasPrefix(strings.ToLower(s), scheme) {
			rest := s[len(scheme):]
			// Must have a host and a path with '/'
			// Strip optional user@ prefix for ssh://git@host/path
			if atIdx := strings.Index(rest, "@"); atIdx >= 0 {
				rest = rest[atIdx+1:]
			}
			// Must have host/path with at least one '/' in the path portion
			slashIdx := strings.Index(rest, "/")
			if slashIdx < 1 || slashIdx == len(rest)-1 {
				return false
			}
			return true
		}
	}

	return false
}

// ToHTTPSCloneURL converts any git URL to HTTPS clone form with a .git suffix.
// SSH shorthand and ssh:// URLs are converted; HTTPS URLs are passed through
// (with .git appended if missing). Azure DevOps URLs (dev.azure.com,
// visualstudio.com, or paths containing /_git/) are an exception: .git is
// never appended and is stripped if already present, because ADO encodes the
// repository via the /_git/<repo> path segment.
func ToHTTPSCloneURL(gitURL string) string {
	if gitURL == "" {
		return ""
	}

	result := gitURL

	// Strip known schemes
	for _, scheme := range []string{"https://", "http://", "ssh://", "git://"} {
		if strings.HasPrefix(strings.ToLower(result), scheme) {
			result = result[len(scheme):]
			break
		}
	}

	// Handle SSH shorthand: git@host:org/repo
	if strings.HasPrefix(result, "git@") {
		result = strings.TrimPrefix(result, "git@")
		result = strings.Replace(result, ":", "/", 1)
	}

	// Strip optional user@ prefix (for ssh://git@host/path after scheme removal)
	if atIdx := strings.Index(result, "@"); atIdx >= 0 {
		slashIdx := strings.Index(result, "/")
		if slashIdx < 0 || atIdx < slashIdx {
			result = result[atIdx+1:]
		}
	}

	// Strip trailing slashes
	result = strings.TrimRight(result, "/")

	// Azure DevOps URLs use /_git/ in the path to identify the repo;
	// appending .git would break them. For ADO we strip any erroneous
	// .git suffix. For every other host we ensure the .git suffix is present.
	if isAzureDevOpsURL(result) {
		result = strings.TrimSuffix(result, ".git")
	} else if !strings.HasSuffix(result, ".git") {
		result += ".git"
	}

	return "https://" + result
}

// isAzureDevOpsURL reports whether hostAndPath (without scheme) looks like an
// Azure DevOps clone URL. It matches dev.azure.com, *.visualstudio.com hosts,
// and any path containing the /_git/ segment that ADO uses.
func isAzureDevOpsURL(hostAndPath string) bool {
	lower := strings.ToLower(hostAndPath)
	if strings.HasPrefix(lower, "dev.azure.com/") {
		return true
	}
	// <org>.visualstudio.com/...
	if dotIdx := strings.Index(lower, "."); dotIdx > 0 {
		rest := lower[dotIdx:]
		if strings.HasPrefix(rest, ".visualstudio.com/") || rest == ".visualstudio.com" {
			return true
		}
	}
	// Catch-all: any URL with the /_git/ path segment is ADO-style.
	if strings.Contains(lower, "/_git/") {
		return true
	}
	return false
}

// ExtractOrgRepo extracts the organization and repository name from a git URL.
// It uses NormalizeGitRemote to get the canonical "host/org/repo" form, then
// returns the last two path components.
func ExtractOrgRepo(gitURL string) (org, repo string) {
	normalized := NormalizeGitRemote(gitURL)
	if normalized == "" {
		return "", ""
	}

	parts := strings.Split(normalized, "/")
	if len(parts) < 3 {
		// Not enough segments (need host/org/repo)
		if len(parts) == 2 {
			return "", parts[1]
		}
		return "", ""
	}

	return parts[len(parts)-2], parts[len(parts)-1]
}

// authenticatedCloneURL embeds OAuth2 credentials in an HTTPS clone URL.
//
// It parses the URL rather than substituting on the scheme prefix: a remote
// that already carries userinfo (Azure DevOps emits
// "https://org@dev.azure.com/org/project/_git/repo") would otherwise end up
// with two "@" separators and fail to resolve. url.UserPassword also escapes
// tokens containing reserved characters.
//
// Only https remotes are rewritten. The previous implementation keyed off the
// literal "https://" prefix, so ssh:// and git:// remotes were left alone;
// url.Parse alone would happily inject an OAuth token into those, so the
// scheme is checked explicitly to preserve that behaviour.
//
// The original URL is returned unchanged when there is no token, when it
// cannot be parsed, and when it is not https.
func authenticatedCloneURL(cloneURL, token string) string {
	if token == "" {
		return cloneURL
	}
	parsed, err := url.Parse(cloneURL)
	if err != nil || parsed.Scheme != "https" {
		return cloneURL
	}
	parsed.User = url.UserPassword("oauth2", token)
	return parsed.String()
}

// CloneSharedWorkspace clones a git repository into the specified workspace path
// for use as a shared workspace project. It configures git identity and optionally
// uses a token for authentication.
//
// If the requested branch does not exist on the remote, the clone falls back to
// the remote's default branch and creates the requested branch locally.
func CloneSharedWorkspace(workspacePath, cloneURL, branch, token string) error {
	authURL := authenticatedCloneURL(cloneURL, token)

	args := []string{"clone"}
	if branch != "" {
		args = append(args, "--branch", branch)
	}
	args = append(args, authURL, workspacePath)

	cmd := exec.Command("git", args...)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	output, err := cmd.CombinedOutput()

	if err != nil && branch != "" && isRemoteBranchNotFound(string(output)) {
		// The branch doesn't exist on the remote yet. Clone the default branch
		// instead and create the requested branch locally.
		_ = os.RemoveAll(workspacePath)

		fallbackArgs := []string{"clone", authURL, workspacePath}
		fallbackCmd := exec.Command("git", fallbackArgs...)
		fallbackCmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
		output, err = fallbackCmd.CombinedOutput()
		if err != nil {
			sanitized := strings.TrimSpace(sanitizeGitOutput(string(output), token))
			gitErr := ClassifyGitError(sanitized)
			if guidance := gitErr.UserGuidance(); guidance != "" {
				return &GitError{Kind: gitErr.Kind, Message: fmt.Sprintf("git clone failed: %s (%s)", sanitized, guidance)}
			}
			return &GitError{Kind: gitErr.Kind, Message: fmt.Sprintf("git clone failed: %s", sanitized)}
		}

		// Create the requested branch locally
		checkoutCmd := exec.Command("git", "-C", workspacePath, "checkout", "-b", branch)
		if checkoutOut, checkoutErr := checkoutCmd.CombinedOutput(); checkoutErr != nil {
			return fmt.Errorf("failed to create local branch %q: %s", branch, strings.TrimSpace(string(checkoutOut)))
		}
	} else if err != nil {
		sanitized := strings.TrimSpace(sanitizeGitOutput(string(output), token))
		gitErr := ClassifyGitError(sanitized)
		if guidance := gitErr.UserGuidance(); guidance != "" {
			return &GitError{Kind: gitErr.Kind, Message: fmt.Sprintf("git clone failed: %s (%s)", sanitized, guidance)}
		}
		return &GitError{Kind: gitErr.Kind, Message: fmt.Sprintf("git clone failed: %s", sanitized)}
	}

	if err := gitConfig(workspacePath, "user.name", "Scion"); err != nil {
		return fmt.Errorf("failed to configure git user.name: %w", err)
	}
	if err := gitConfig(workspacePath, "user.email", "agent@scion.dev"); err != nil {
		return fmt.Errorf("failed to configure git user.email: %w", err)
	}

	if token != "" {
		if err := gitConfig(workspacePath, "remote.origin.url", cloneURL); err != nil {
			Debugf("CloneSharedWorkspace: failed to sanitize remote URL: %v", err)
		}
	}

	return nil
}

// isRemoteBranchNotFound checks whether git clone stderr indicates that the
// requested branch does not exist on the remote (as opposed to the repo itself
// not being found or an auth error).
func isRemoteBranchNotFound(stderr string) bool {
	lower := strings.ToLower(stderr)
	return strings.Contains(lower, "remote branch") && strings.Contains(lower, "not found")
}

// PullCommitInfo describes a single commit that was pulled.
type PullCommitInfo struct {
	Hash    string `json:"hash"`
	Subject string `json:"subject"`
}

// PullResult contains the structured result of a git pull operation.
type PullResult struct {
	Updated bool             `json:"updated"`
	Commits []PullCommitInfo `json:"commits,omitempty"`
}

// PullSharedWorkspace runs `git pull` in the specified workspace path to update
// it from the remote. It optionally uses a token for authentication and sanitizes
// credentials from error output. Returns structured commit information.
func PullSharedWorkspace(workspacePath, token string) (*PullResult, error) {
	env := append(os.Environ(), "GIT_TERMINAL_PROMPT=0")

	// If a token is provided, configure a temporary credential helper via env
	if token != "" {
		// Use a one-shot credential helper that provides the token
		helper := fmt.Sprintf("!f() { echo username=oauth2; echo password=%s; }; f", token)
		env = append(env,
			"GIT_CONFIG_COUNT=1",
			"GIT_CONFIG_KEY_0=credential.helper",
			"GIT_CONFIG_VALUE_0="+helper,
		)
	}

	// Capture HEAD before pulling so we can enumerate new commits afterward.
	oldHead, _ := exec.Command("git", "-C", workspacePath, "rev-parse", "HEAD").Output()
	oldHeadStr := strings.TrimSpace(string(oldHead))

	cmd := exec.Command("git", "-C", workspacePath, "pull", "--ff-only")
	cmd.Env = env
	output, err := cmd.CombinedOutput()
	sanitized := sanitizeGitOutput(strings.TrimSpace(string(output)), token)
	if err != nil {
		gitErr := ClassifyGitError(sanitized)
		if guidance := gitErr.UserGuidance(); guidance != "" {
			return nil, &GitError{Kind: gitErr.Kind, Message: fmt.Sprintf("git pull failed: %s (%s)", sanitized, guidance)}
		}
		return nil, &GitError{Kind: gitErr.Kind, Message: fmt.Sprintf("git pull failed: %s", sanitized)}
	}

	newHead, _ := exec.Command("git", "-C", workspacePath, "rev-parse", "HEAD").Output()
	newHeadStr := strings.TrimSpace(string(newHead))

	result := &PullResult{}
	if oldHeadStr == "" || newHeadStr == "" || oldHeadStr == newHeadStr {
		return result, nil
	}

	result.Updated = true

	logCmd := exec.Command("git", "-C", workspacePath, "log", "--oneline", "--no-decorate",
		oldHeadStr+".."+newHeadStr)
	logOut, err := logCmd.Output()
	if err != nil {
		return result, nil
	}

	lines := strings.Split(strings.TrimSpace(string(logOut)), "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, " ", 2)
		info := PullCommitInfo{Hash: parts[0]}
		if len(parts) > 1 {
			info.Subject = parts[1]
		}
		result.Commits = append(result.Commits, info)
	}

	return result, nil
}

// gitConfig sets a git config value in the specified repository.
func gitConfig(repoPath, key, value string) error {
	cmd := exec.Command("git", "-C", repoPath, "config", key, value)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s: %s", key, strings.TrimSpace(string(output)))
	}
	return nil
}

// GitErrorKind classifies git operation failures for appropriate error handling.
type GitErrorKind int

const (
	// GitErrUnknown is the default for unrecognized errors.
	GitErrUnknown GitErrorKind = iota
	// GitErrAuth indicates authentication or authorization failure (401/403).
	GitErrAuth
	// GitErrNotFound indicates the repository or branch was not found (404).
	GitErrNotFound
	// GitErrNetwork indicates a network connectivity issue.
	GitErrNetwork
	// GitErrNonFastForward indicates a pull failed because local commits diverge.
	GitErrNonFastForward
)

// GitError wraps a git operation error with a classified kind and sanitized message.
type GitError struct {
	Kind    GitErrorKind
	Message string
}

func (e *GitError) Error() string {
	return e.Message
}

// ClassifyGitError inspects sanitized git stderr and returns a classified GitError.
func ClassifyGitError(sanitizedStderr string) *GitError {
	lower := strings.ToLower(sanitizedStderr)

	kind := GitErrUnknown
	switch {
	case strings.Contains(lower, "authentication failed") ||
		strings.Contains(lower, "could not read username") ||
		strings.Contains(lower, "invalid credentials") ||
		strings.Contains(lower, "403") ||
		strings.Contains(lower, "401"):
		kind = GitErrAuth
	case strings.Contains(lower, "repository not found") ||
		strings.Contains(lower, "not found") ||
		strings.Contains(lower, "does not exist") ||
		strings.Contains(lower, "404"):
		kind = GitErrNotFound
	case strings.Contains(lower, "could not resolve host") ||
		strings.Contains(lower, "unable to access") ||
		strings.Contains(lower, "connection refused") ||
		strings.Contains(lower, "network is unreachable") ||
		strings.Contains(lower, "timed out"):
		kind = GitErrNetwork
	case strings.Contains(lower, "not possible to fast-forward") ||
		strings.Contains(lower, "non-fast-forward"):
		kind = GitErrNonFastForward
	}

	return &GitError{Kind: kind, Message: sanitizedStderr}
}

// UserGuidance returns a user-facing hint for the error kind.
func (e *GitError) UserGuidance() string {
	switch e.Kind {
	case GitErrAuth:
		return "Check that GITHUB_TOKEN (or GitHub App credentials) are valid and have repository read access."
	case GitErrNotFound:
		return "Verify the repository URL exists and that the token has access to it (private repos require explicit access)."
	case GitErrNetwork:
		return "A network error occurred. Check connectivity and try again."
	case GitErrNonFastForward:
		return "The workspace has local commits that diverge from the remote. Merge or reset manually before pulling."
	default:
		return ""
	}
}

// sanitizeGitOutput removes a token from git output to prevent credential leaks.
func sanitizeGitOutput(output, token string) string {
	if token == "" {
		return output
	}
	return strings.ReplaceAll(output, token, "***")
}

// scionNamespace is a fixed UUID v5 namespace used by HashProjectID for
// deterministic identifier derivation (e.g. cache keys).
var scionNamespace = uuid.MustParse("a1b8e4f0-7c3d-4a1e-9f2b-6d5c8e7a0b1f")

// HashProjectID computes a deterministic identifier from a normalized input
// string.  It uses UUID v5 (SHA-1 based) with a fixed Scion namespace to
// produce a valid UUID that is stable for a given input.
//
// NOTE: Project IDs are random UUIDs (see config.GenerateProjectID).  This
// function is NOT used for project-ID generation — it is retained for other
// deterministic identifier needs such as cache keys.
func HashProjectID(normalized string) string {
	return uuid.NewSHA1(scionNamespace, []byte(normalized)).String()
}
