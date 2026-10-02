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

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// WorktreesSubdir is the fixed layout ensureWorktree/WorktreePath use for a
// worktree-per-agent checkout: <repoRoot>/worktrees/<name>.
const WorktreesSubdir = "worktrees"

// WorktreeIsLexicallyUnderBase reports whether candidate is lexically
// exactly base/worktrees/<single-clean-element> — a single non-empty path
// segment directly under WorktreesSubdir, nothing shallower and nothing
// nested deeper. It does no filesystem or git-relationship probing, so it
// stays usable when the worktree itself may not exist, or may already be
// partially or fully removed (e.g. a teardown/removal gate deciding whether
// a path is even eligible to touch), unlike ValidateWorktreeForBase, which
// requires the worktree to be present and genuine.
//
// Both base and candidate must be non-empty, absolute paths; either being
// empty or relative reports false rather than resolving the relationship
// against the calling process's current working directory.
//
// This function makes no claim about where candidate points on disk. It is
// purely lexical: if WorktreesSubdir or the final <name> element is itself a
// symlink, this check does not see through it, and passing does not
// guarantee that following candidate stays inside base. A caller that acts
// on candidate by following symlinks (for example, a removal gate) must
// apply its own resolved-path check in addition to this one; this function
// only proves the lexical containment relationship.
func WorktreeIsLexicallyUnderBase(base, candidate string) bool {
	_, ok := worktreeRelName(filepath.Clean(base), filepath.Clean(candidate))
	return ok
}

// ValidateWorktreeForBase confirms that candidate is a genuine git worktree
// of base, laid out at exactly base/worktrees/<name>. Intended as the single
// implementation for any path that turns a stored or discovered candidate
// into a host mount or removal target, so all of them agree on what counts
// as valid.
//
// Both base and candidate must be non-empty, absolute paths; either being
// empty or relative is rejected outright rather than resolved against the
// calling process's current working directory.
//
// Two independent relationships must hold, both naming the same <name>:
//
//  1. Lexical (WorktreeIsLexicallyUnderBase): no symlink resolution. This is
//     the same shape pkg/runtime/common.go's mount builder computes to
//     choose between the worktree dual-mount branch and its full-root
//     fallback, so a pair that only relates after resolving symlinks (e.g.
//     one path reaching the shared checkout through an aliased directory)
//     must still be rejected: accepting it here while common.go's own
//     unresolved computation disagrees would validate one mount decision
//     and produce a different one.
//  2. Resolved: the same relationship after EvalSymlinks on both sides, plus
//     proof of an actual git-worktree relationship rather than a matching
//     directory shape — candidate/.git is a regular gitfile whose "gitdir:"
//     pointer resolves to base/.git/worktrees/<name>, which must exist, and
//     whose own back-link resolves back to candidate/.git.
//
// Returns nil when both hold. Returns a descriptive error, identifying which
// check failed, otherwise — including when candidate does not currently
// exist as a worktree at all (this function expects the worktree to be
// present; see WorktreeIsLexicallyUnderBase for a containment check that
// does not).
func ValidateWorktreeForBase(base, candidate string) error {
	if base == "" || candidate == "" {
		return fmt.Errorf("worktree relationship: base and candidate must both be non-empty")
	}
	if !filepath.IsAbs(base) || !filepath.IsAbs(candidate) {
		return fmt.Errorf("worktree relationship: base and candidate must both be absolute paths")
	}

	lexName, ok := worktreeRelName(filepath.Clean(base), filepath.Clean(candidate))
	if !ok {
		return fmt.Errorf("worktree relationship: %q is not lexically %s/%s/<name>", candidate, base, WorktreesSubdir)
	}

	resolvedBase, err := filepath.EvalSymlinks(base)
	if err != nil {
		return fmt.Errorf("worktree relationship: resolve base: %w", err)
	}
	resolvedCandidate, err := filepath.EvalSymlinks(candidate)
	if err != nil {
		return fmt.Errorf("worktree relationship: resolve candidate: %w", err)
	}

	resolvedName, ok := worktreeRelName(resolvedBase, resolvedCandidate)
	if !ok || resolvedName != lexName {
		return fmt.Errorf("worktree relationship: resolved paths do not name the same worktree as the lexical pair (%q)", lexName)
	}
	name := resolvedName

	gitDirInfo, err := os.Stat(filepath.Join(resolvedBase, ".git"))
	if err != nil || !gitDirInfo.IsDir() {
		return fmt.Errorf("worktree relationship: %s/.git is not a directory", resolvedBase)
	}

	// The candidate's gitfile must be a regular file, not a directory: a
	// directory that merely sits in the right place is not a worktree.
	candidateGitPath := filepath.Join(resolvedCandidate, ".git")
	cgInfo, err := os.Stat(candidateGitPath)
	if err != nil || cgInfo.IsDir() {
		return fmt.Errorf("worktree relationship: %s/.git is not a regular gitfile", resolvedCandidate)
	}
	gitfileContent, err := os.ReadFile(candidateGitPath)
	if err != nil {
		return fmt.Errorf("worktree relationship: read gitfile: %w", err)
	}
	const gitdirPrefix = "gitdir:"
	line := strings.TrimSpace(string(gitfileContent))
	if !strings.HasPrefix(line, gitdirPrefix) {
		return fmt.Errorf("worktree relationship: gitfile does not start with %q", gitdirPrefix)
	}
	pointedAdminDir := strings.TrimSpace(line[len(gitdirPrefix):])
	if !filepath.IsAbs(pointedAdminDir) {
		pointedAdminDir = filepath.Join(resolvedCandidate, pointedAdminDir)
	}
	resolvedPointedAdminDir, err := filepath.EvalSymlinks(pointedAdminDir)
	if err != nil {
		return fmt.Errorf("worktree relationship: resolve gitdir pointer: %w", err)
	}

	adminDir := filepath.Join(resolvedBase, ".git", "worktrees", name)
	adminDirInfo, err := os.Stat(adminDir)
	if err != nil || !adminDirInfo.IsDir() {
		return fmt.Errorf("worktree relationship: admin directory %s does not exist", adminDir)
	}
	resolvedAdminDir, err := filepath.EvalSymlinks(adminDir)
	if err != nil {
		return fmt.Errorf("worktree relationship: resolve admin directory: %w", err)
	}
	if resolvedPointedAdminDir != resolvedAdminDir {
		return fmt.Errorf("worktree relationship: gitfile does not point at the admin directory for %q", name)
	}

	// The admin dir's own back-link must resolve back to the candidate's
	// gitfile. git writes this relative to the admin dir itself.
	backLinkContent, err := os.ReadFile(filepath.Join(adminDir, "gitdir"))
	if err != nil {
		return fmt.Errorf("worktree relationship: read admin back-link: %w", err)
	}
	backLinkTarget := strings.TrimSpace(string(backLinkContent))
	if !filepath.IsAbs(backLinkTarget) {
		backLinkTarget = filepath.Join(adminDir, backLinkTarget)
	}
	resolvedBackLinkTarget, err := filepath.EvalSymlinks(backLinkTarget)
	if err != nil {
		return fmt.Errorf("worktree relationship: resolve admin back-link: %w", err)
	}
	resolvedCandidateGitPath, err := filepath.EvalSymlinks(candidateGitPath)
	if err != nil {
		return fmt.Errorf("worktree relationship: resolve candidate gitfile: %w", err)
	}
	if resolvedBackLinkTarget != resolvedCandidateGitPath {
		return fmt.Errorf("worktree relationship: admin back-link does not resolve to the candidate's gitfile")
	}

	return nil
}

// worktreeRelName reports whether candidate sits at exactly
// "<base>/worktrees/<name>" for a non-empty <name>, and returns that name.
// Both arguments are compared exactly as given — callers pass either the
// lexical (filepath.Clean'd) or the fully resolved (EvalSymlinks'd) form.
//
// Both must be absolute. This is the shared precheck for both exported
// functions in this file: filepath.Rel resolves a relative argument against
// the calling process's current working directory, which is never the
// right base for a stored or broker-supplied candidate, so a relative (or
// empty, which filepath.Clean turns into ".") argument is rejected here
// rather than silently evaluated against an unrelated cwd.
func worktreeRelName(base, candidate string) (string, bool) {
	if !filepath.IsAbs(base) || !filepath.IsAbs(candidate) {
		return "", false
	}
	rel, err := filepath.Rel(base, candidate)
	if err != nil || rel == "." || rel == ".." ||
		strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return "", false
	}
	segments := strings.SplitN(rel, string(filepath.Separator), 3)
	if len(segments) != 2 || segments[0] != WorktreesSubdir || segments[1] == "" {
		return "", false
	}
	return segments[1], true
}
