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
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
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
	cmd := hostGit(args...)
	if dir != "" {
		cmd.Dir = dir
	}
	err := cmd.Run()
	return err == nil
}

// ErrGitDubiousOwnership is matched (via errors.Is) by the error CheckGitRepoDir
// returns when git refuses a repository because it is owned by a different
// user and is not listed in the safe.directory setting.
var ErrGitDubiousOwnership = errors.New("git refused the repository because of its ownership")

// GitOwnershipError reports that git refused a repository because of its
// ownership. It matches ErrGitDubiousOwnership with errors.Is.
type GitOwnershipError struct {
	// Dir is the directory git was run in.
	Dir string
	// SuggestedArg is the safe.directory argument from git's own suggested
	// command, kept verbatim (git shell-quotes it when the path needs it),
	// or empty if it could not be read.
	SuggestedArg string
}

func (e *GitOwnershipError) Error() string {
	if e.SuggestedArg != "" {
		return fmt.Sprintf("git refused the repository because it is owned by a different user; "+
			"if you trust it, run: git config --global --add safe.directory %s", e.SuggestedArg)
	}
	return fmt.Sprintf("git refused the repository at %s because it is owned by a different user; "+
		"if you trust it, run: git config --global --add safe.directory <path> "+
		"(use the repository's top-level directory)", e.Dir)
}

// Is reports whether target is ErrGitDubiousOwnership.
func (e *GitOwnershipError) Is(target error) bool {
	return target == ErrGitDubiousOwnership
}

// CheckGitRepoDir reports whether dir (the current directory when empty) is
// inside a git work tree. Unlike IsGitRepoDir it tells "not a repository"
// apart from git refusing a repository because of its ownership: in that
// case it returns false and a *GitOwnershipError. Any other git failure
// returns (false, nil), as IsGitRepoDir does.
func CheckGitRepoDir(dir string) (bool, error) {
	cmd := hostGit("rev-parse", "--is-inside-work-tree")
	if dir != "" {
		cmd.Dir = dir
	}
	// hostGit's environment (HostGitEnv) sets LC_ALL=C, so the stderr match is
	// locale-stable.
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Run(); err == nil {
		return true, nil
	}
	msg := stderr.String()
	if !isGitOwnershipRefusal(msg) {
		return false, nil
	}
	checked := dir
	if checked == "" {
		checked, _ = os.Getwd()
	}
	if abs, err := filepath.Abs(checked); err == nil {
		checked = abs
	}
	return false, &GitOwnershipError{Dir: checked, SuggestedArg: parseSafeDirectorySuggestion(msg)}
}

// isGitOwnershipRefusal reports whether git's stderr describes a refusal
// because of repository ownership. Git 2.36.2 and 2.37.1 onward print
// "fatal: detected dubious ownership in repository at '<path>'"; 2.35.2 to
// 2.36.1 and 2.37.0 print "fatal: unsafe repository ('<path>' is owned by
// someone else)". Only a line that starts with one of these prefixes
// counts, so other git errors that echo a path containing similar words
// are not mistaken for a refusal. The prefixes are stable under LC_ALL=C.
func isGitOwnershipRefusal(stderr string) bool {
	for _, line := range strings.Split(stderr, "\n") {
		if strings.HasPrefix(line, "fatal: detected dubious ownership in repository at ") ||
			strings.HasPrefix(line, "fatal: unsafe repository (") {
			return true
		}
	}
	return false
}

// parseSafeDirectorySuggestion returns the argument of git's suggested
// "git config --global --add safe.directory <arg>" line verbatim, keeping
// any shell quoting git applied, or "" if there is no such line or the
// argument is not a complete shell word (for example a quoted path that
// contains a newline, which git prints across several lines).
func parseSafeDirectorySuggestion(stderr string) string {
	const marker = "--add safe.directory "
	for _, line := range strings.Split(stderr, "\n") {
		i := strings.Index(line, marker)
		if i < 0 {
			continue
		}
		arg := strings.TrimSpace(line[i+len(marker):])
		if !isCompleteShellWord(arg) {
			return ""
		}
		return arg
	}
	return ""
}

// isCompleteShellWord reports whether s is a non-empty single shell word in
// the form git prints (plain characters, single-quoted runs and
// backslash-escaped characters) with every quote closed and no newline.
func isCompleteShellWord(s string) bool {
	if s == "" || strings.ContainsAny(s, "\r\n") {
		return false
	}
	inQuote := false
	for i := 0; i < len(s); i++ {
		switch c := s[i]; {
		case c == '\'':
			inQuote = !inQuote
		case inQuote:
		case c == '\\':
			if i+1 == len(s) {
				return false
			}
			i++
		case c == ' ' || c == '\t':
			return false
		}
	}
	return !inQuote
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
	cmd := hostGit("rev-parse", "--show-toplevel")
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
	cmd := hostGit("rev-parse", "--git-common-dir")
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
	cmd := hostGit("check-ignore", "-q", path)
	if dir != "" {
		cmd.Dir = dir
	}
	err := cmd.Run()
	return err == nil
}

// CreateWorktree creates a new git worktree at the specified path with a new
// branch, with no broker-recorded origin for the git-lfs endpoint pin (the
// endpoint is derived from the live remote.origin.url).
// Prefer CreateWorktreeWithOrigin from broker/hub start paths, which pass the
// recorded origin.
func CreateWorktree(path, branch string) error {
	return CreateWorktreeWithOrigin(path, branch, "")
}

// CreateWorktreeWithOrigin is CreateWorktree with the broker-recorded origin URL
// for the project (the hub project's repository). It is used to pin the git-lfs
// endpoint from a trusted source rather than the live, agent-writable
// remote.origin.url, and to skip the smudge when the live origin no longer
// matches the recorded one. Pass "" when no recorded origin is available.
func CreateWorktreeWithOrigin(path, branch, recordedOrigin string) error {
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
	// We run from root to ensure --relative-paths are calculated from root.
	// HostGitEnv sets LC_ALL=C, which keeps git's messages in English, so the
	// matches below hold. The checkout runs without repository hooks or
	// repository-defined filter commands (HostGitCommand): agents can write
	// this repository.
	gitEnv := HostGitEnv(os.Environ())
	ctx := context.Background()
	// git-lfs on a host-side checkout would otherwise take its endpoint from
	// a tracked .lfsconfig; pin the endpoint from the trusted (broker-recorded)
	// origin instead (lfs.url via -c outranks .lfsconfig). When the endpoint
	// cannot be derived (for example an ssh remote) or the live origin no longer
	// matches the recorded one, skip the smudge and warn.
	lfsOpt := lfsOptionForLinkedCheckout(root, branch, recordedOrigin)
	opts := HostGitOptions{Env: gitEnv, Policy: RepoPolicyLinked, SkipLFSSmudge: lfsOpt.skipSmudge, LFSURL: lfsOpt.url}
	cmd, err := HostGitCommand(ctx, root, opts, "worktree", "add", "--relative-paths", "-b", branch, path)
	if err != nil {
		return fmt.Errorf("failed to create worktree: %w", err)
	}
	if output, err := cmd.CombinedOutput(); err != nil {
		outputStr := string(output)
		// If branch already exists, try to just add it
		if strings.Contains(outputStr, "already exists") {
			cmd, err = HostGitCommand(ctx, root, opts, "worktree", "add", "--relative-paths", path, branch)
			if err != nil {
				return fmt.Errorf("failed to create worktree: %w", err)
			}
			if output, err := cmd.CombinedOutput(); err != nil {
				outputStr = string(output)
				if strings.Contains(outputStr, "already checked out") || strings.Contains(outputStr, "already used by worktree") {
					holder, _ := FindWorktreeByBranchIn(root, branch)
					return WorktreeBranchHeldError(branch, holder)
				}
				return fmt.Errorf("failed to create worktree: %s", strings.TrimSpace(outputStr))
			}
			if lfsOpt.skipSmudge {
				warnLFSPointersNotFetched(path)
			}
			return nil
		}
		return fmt.Errorf("failed to create worktree: %s", strings.TrimSpace(outputStr))
	}
	if lfsOpt.skipSmudge {
		warnLFSPointersNotFetched(path)
	}
	return nil
}

// lfsCheckoutOption is how a host-side linked checkout handles git-lfs.
type lfsCheckoutOption struct {
	url        string // pinned lfs.url (empty when none)
	skipSmudge bool   // skip the LFS smudge instead
}

// lfsOptionForLinkedCheckout decides the git-lfs handling for a linked
// checkout at root:
//   - pin the endpoint (and its auth mode to basic) derived from the
//     broker-recorded origin when it is http(s), the live remote.origin.url
//     still matches it, AND the checked-out .lfsconfig neither names its own
//     endpoint nor sets any lfs.<url>.access; otherwise
//   - skip the smudge (an ssh/scp or absent/mismatched origin, or a tracked
//     .lfsconfig that names an endpoint or an access mode — its objects may
//     legitimately live on a separate server, or it may select an auth mode we
//     will not use; either way we do not pin and never contact it).
//
// recordedOrigin is the broker-recorded origin for the project. The live
// remote.origin.url is agent-writable in the shared repository config until the
// config narrowing applies, and host-side CreateWorktree runs before the
// container in any case, so the endpoint is derived from the recorded origin and
// we refuse to pin when the live origin has been pointed elsewhere. When
// recordedOrigin is empty (no hub record available) we fall back to the live
// origin.
func lfsOptionForLinkedCheckout(root, branch, recordedOrigin string) lfsCheckoutOption {
	// Read the .lfsconfig of the ref actually being checked out: the existing
	// branch when it already exists, otherwise HEAD (a new branch is created
	// from HEAD, so it has HEAD's tree).
	ref := "HEAD"
	if branch != "" && BranchExistsIn(root, branch) {
		ref = "refs/heads/" + branch
	}
	// A tracked .lfsconfig that sets an endpoint or an access mode (or is
	// unparseable): do not pin (the origin endpoint may not serve these objects,
	// and an access key could select ambient credentials) and do not contact it;
	// skip.
	if lfsConfigNamesEndpoint(root, ref) {
		return lfsCheckoutOption{skipSmudge: true}
	}
	// Derive from the broker-recorded origin, never trust the live origin
	// alone. If a recorded origin is present but the live origin no longer
	// normalises to it, the repository the agent points at is not the one the
	// hub recorded — skip rather than pin.
	origin := strings.TrimSpace(recordedOrigin)
	if origin == "" {
		origin = strings.TrimSpace(GetGitRemoteDir(root))
	} else {
		live := strings.TrimSpace(GetGitRemoteDir(root))
		if live == "" || NormalizeGitRemote(live) != NormalizeGitRemote(origin) {
			return lfsCheckoutOption{skipSmudge: true}
		}
	}
	if origin == "" {
		return lfsCheckoutOption{skipSmudge: true}
	}
	if endpoint, ok := DeriveLFSEndpoint(origin); ok {
		return lfsCheckoutOption{url: endpoint}
	}
	return lfsCheckoutOption{skipSmudge: true}
}

// lfsConfigNamesEndpoint reports whether the .lfsconfig committed at ref sets
// an LFS endpoint (lfs.url, lfs.pushurl or remote.<name>.lfsurl) or any
// per-endpoint lfs.<url>.access. It reads <ref>:.lfsconfig as data (host-side
// git with the fixed pins). Only a genuinely absent file is "no endpoint"
// (false). A file that is present but unreadable or unparseable, or an entry that
// is not a blob, is treated as naming an endpoint (true, fail closed: skip-smudge
// rather than pin against an endpoint we could not vet). "Absent" and
// "unreadable" are distinguished with ls-tree, because `git show` reports the
// same error for both.
func lfsConfigNamesEndpoint(root, ref string) bool {
	ls, err := hostGit("-C", root, "ls-tree", ref, "--", ".lfsconfig").Output()
	if err != nil {
		return true // cannot read the tree entry: fail closed (skip)
	}
	entry := strings.TrimSpace(string(ls))
	if entry == "" {
		return false // no committed .lfsconfig at this ref (genuinely absent)
	}
	// The entry exists. It must be a blob to be a real .lfsconfig; a tree or a
	// gitlink (submodule) at that path is unexpected — fail closed.
	if fields := strings.Fields(entry); len(fields) < 2 || fields[1] != "blob" {
		return true
	}
	out, err := hostGit("-C", root, "show", ref+":.lfsconfig").Output()
	if err != nil {
		return true // present but unreadable: fail closed (skip)
	}
	vars, err := parseGitConfigFile(out)
	if err != nil {
		return true // present but unparseable: fail closed (skip)
	}
	for _, v := range vars {
		if v.Section == "lfs" && !v.HasSubsection && (v.Name == "url" || v.Name == "pushurl") {
			return true
		}
		if v.Section == "remote" && v.HasSubsection && v.Name == "lfsurl" {
			return true
		}
		// Any per-endpoint lfs.<url>.access in the committed .lfsconfig: skip the
		// smudge rather than pin. git-lfs matches lfs.<url>.access keys by URL
		// equivalence (explicit :443, trailing slash, …), so a pinned
		// lfs.<endpoint>.access=basic cannot reliably override one spelled
		// differently; refusing to pin (and never contacting the endpoint)
		// closes that without reimplementing git-lfs's URL normalisation.
		if v.Section == "lfs" && v.HasSubsection && v.Name == "access" {
			return true
		}
	}
	return false
}

// warnLFSPointersNotFetched scans the checkout at path for git-lfs pointer
// files (left in place when the smudge was skipped) and logs a warning that
// names them and says how to fetch the objects. It only runs when the
// repository tracks LFS content (a .gitattributes assigns filter=lfs), looks
// only at regular files (never following a symlink or opening a device/FIFO),
// and reads a bounded prefix. It is best effort and never fails the checkout.
func warnLFSPointersNotFetched(path string) {
	if !repoTracksLFS(path) {
		return
	}
	var pointers []string
	const maxList = 50
	_ = filepath.WalkDir(path, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if d.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		// Regular files only: WalkDir's DirEntry is lstat-based, so a symlink
		// reports a non-regular type and is skipped without being followed. If a
		// regular file is swapped for a symlink/FIFO after this lstat but before
		// the read, isLFSPointerFile opens with O_NOFOLLOW|O_NONBLOCK and an fstat
		// regular-file check, so the swap is not followed either.
		if !d.Type().IsRegular() {
			return nil
		}
		if isLFSPointerFile(p) {
			if rel, relErr := filepath.Rel(path, p); relErr == nil {
				pointers = append(pointers, rel)
			} else {
				pointers = append(pointers, p)
			}
			if len(pointers) >= maxList {
				return filepath.SkipAll
			}
		}
		return nil
	})
	if len(pointers) == 0 {
		return
	}
	slog.Warn("git-lfs objects were not fetched on the host checkout; the files hold LFS pointers. Fetch them yourself with 'git lfs pull' in the checkout",
		"path", path, "pointer_files", pointers, "command", "git lfs pull")
}

// repoTracksLFS reports whether a .gitattributes in the checkout root assigns
// the lfs filter to anything. A cheap gate so a non-LFS checkout is not
// scanned file by file. It reads the file safely (regular files only, never
// following a symlink or blocking on a FIFO/device), so a tracked
// .gitattributes that is a symlink to a FIFO cannot hang the checkout.
func repoTracksLFS(path string) bool {
	data, ok := readRegularFilePrefix(filepath.Join(path, ".gitattributes"), 1<<16)
	if !ok {
		return false
	}
	return strings.Contains(string(data), "filter=lfs")
}

// isLFSPointerFile reports whether the regular file at p begins with the
// git-lfs pointer header.
func isLFSPointerFile(p string) bool {
	data, ok := readRegularFilePrefix(p, 64)
	if !ok {
		return false
	}
	return strings.HasPrefix(string(data), "version https://git-lfs.")
}

// readRegularFilePrefix reads up to limit bytes from a regular file at path.
// On unix it opens with O_NOFOLLOW|O_NONBLOCK (so a symlink cannot redirect it
// and a FIFO/device cannot block it); on other platforms it refuses a symlink
// or non-regular file with Lstat before opening. On every platform it then
// confirms via fstat that the opened file is regular. ok is false for a
// symlink, a non-regular file, or any error.
func readRegularFilePrefix(path string, limit int64) ([]byte, bool) {
	f, err := openRegularFileNoFollow(path)
	if err != nil {
		return nil, false
	}
	defer func() { _ = f.Close() }()
	fi, err := f.Stat()
	if err != nil || !fi.Mode().IsRegular() {
		return nil, false
	}
	data, err := io.ReadAll(io.LimitReader(f, limit))
	if err != nil {
		return nil, false
	}
	return data, true
}

// WorktreeBranchHeldError reports that branch cannot be checked out because
// another worktree, at holder, still holds it. holder may be empty when the
// holding worktree is not known. Only the entry at an agent's own worktree
// path is ever removed automatically, so when the holding worktree is
// really gone its entry has to be removed by hand, which the message says.
func WorktreeBranchHeldError(branch, holder string) error {
	if holder == "" {
		return fmt.Errorf("branch '%s' is already checked out in another worktree", branch)
	}
	return fmt.Errorf("branch '%s' is already checked out in another worktree at %s; if that worktree no longer exists, run git worktree remove %q in the repository and try again", branch, holder, holder)
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
	rel, err := filepath.Rel(resolvedBase, resolvedCandidate) //winsec:not-containment gated on Windows: only RemoveWorktree (agent deletion, broker-side) reaches it
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
// repoRoot and branch come from the caller, never from the worktree: no git
// command runs inside the worktree, and its own metadata files (the .git
// pointer, the admin directory's commondir and HEAD) never choose which
// repository is changed or which branch is deleted. repoRoot is the main
// repository the worktree belongs to; when it is empty, or is not a main
// repository (see BaseRepoGitCommand), the worktree entry and the branch
// are left alone. branch is the branch to delete; when it is empty no
// branch is deleted. The branch is deleted only when the worktree's admin
// directory in repoRoot shows the worktree checked out on exactly that
// branch (worktreeOnBranch), and only through DeleteBranchIn.
//
// Instead of using "git worktree remove" (which does its own directory
// deletion and can trigger macOS autofs timeouts on symlinks pointing to
// container-internal paths), this function:
//  1. Checks, while the worktree exists, whether it is on branch in repoRoot.
//  2. Removes the worktree directory using RemoveAllSafe (which uses unlinkat
//     to avoid autofs triggers).
//  3. Removes the now-stale record of this worktree only
//     (RemoveWorktreeEntryIn), never a repository-wide prune.
//  4. Deletes branch, if step 1 matched.
//
// It reports whether the branch was deleted.
func RemoveWorktree(base, path, repoRoot, branch string) (bool, error) {
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

	if repoRoot != "" {
		if _, err := baseRepoGitDir(repoRoot); err != nil {
			slog.Warn("remove worktree: not a main repository; leaving its worktree entry and branch alone",
				"path", resolvedPath, "repo_root", repoRoot, "error", err)
			repoRoot = ""
		}
	}

	// Read before the directory (which holds the .git pointer) is removed.
	onBranch := false
	if repoRoot != "" && branch != "" {
		var reason string
		onBranch, reason = worktreeOnBranch(resolvedPath, repoRoot, branch)
		if !onBranch {
			slog.Warn("remove worktree: worktree is not on the expected branch of the repository; the branch is not deleted",
				"path", resolvedPath, "repo_root", repoRoot, "branch", branch, "reason", reason)
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

	// Drop git's record of the directory we just deleted. Only this
	// worktree's own entry is removed: a repository-wide prune would also
	// drop the entries of other worktrees whose directories happen to be
	// missing or unreadable from this process (ptone/scion#5060). Without a
	// repository root there is no entry to target, so nothing is done.
	if repoRoot != "" {
		Debugf("RemoveWorktree: removing the worktree entry for %s in %s", resolvedPath, repoRoot)
		if err := RemoveWorktreeEntryIn(repoRoot, resolvedPath); err != nil {
			slog.Warn("remove worktree: could not remove the worktree entry", "path", resolvedPath, "repo_root", repoRoot, "error", err)
		}
	}

	if !onBranch {
		return false, nil
	}
	Debugf("RemoveWorktree: deleting branch %s", branch)
	branchDeleteStart := time.Now()
	deleted := DeleteBranchIn(repoRoot, branch)
	Debugf("RemoveWorktree: branch delete (deleted: %v) completed in %v", deleted, time.Since(branchDeleteStart))
	return deleted, nil
}

// worktreeOnBranch reports whether the worktree at path is a linked
// worktree of the main repository at repoRoot checked out on branch, reading
// the files git would read but without running git in the worktree. Each
// file is read with readSmallRegularFile: a symlink, a FIFO or other special
// file, or content over smallGitFileMax bytes fails the check. All of these
// must hold:
//   - path/.git is a "gitdir:" file naming a directory directly under
//     repoRoot/.git/worktrees (compared with symlinks resolved);
//   - that admin directory's commondir file names repoRoot/.git;
//   - its HEAD file is exactly "ref: refs/heads/<branch>".
//
// The reason names the first check that failed.
func worktreeOnBranch(path, repoRoot, branch string) (bool, string) {
	data, err := readSmallRegularFile(filepath.Join(path, ".git"), smallGitFileMax)
	if err != nil {
		return false, fmt.Sprintf("read worktree .git file: %v", err)
	}
	line := strings.TrimSpace(string(data))
	gitdir, ok := strings.CutPrefix(line, "gitdir:")
	if !ok || strings.ContainsAny(line, "\n") {
		return false, "worktree .git is not a single gitdir line"
	}
	gitdir = strings.TrimSpace(gitdir)
	if !filepath.IsAbs(gitdir) {
		gitdir = filepath.Join(path, gitdir)
	}
	adminDir, err := filepath.EvalSymlinks(gitdir)
	if err != nil {
		return false, fmt.Sprintf("resolve worktree gitdir: %v", err)
	}
	commonDir, err := filepath.EvalSymlinks(filepath.Join(repoRoot, ".git"))
	if err != nil {
		return false, fmt.Sprintf("resolve repository .git: %v", err)
	}
	if filepath.Dir(adminDir) != filepath.Join(commonDir, "worktrees") {
		return false, "worktree gitdir is not an admin directory of the repository"
	}
	data, err = readSmallRegularFile(filepath.Join(adminDir, "commondir"), smallGitFileMax)
	if err != nil {
		return false, fmt.Sprintf("read commondir: %v", err)
	}
	named := strings.TrimSpace(string(data))
	if !filepath.IsAbs(named) {
		named = filepath.Join(adminDir, named)
	}
	if resolved, err := filepath.EvalSymlinks(named); err != nil || resolved != commonDir {
		return false, "commondir does not name the repository"
	}
	data, err = readSmallRegularFile(filepath.Join(adminDir, "HEAD"), smallGitFileMax)
	if err != nil {
		return false, fmt.Sprintf("read HEAD: %v", err)
	}
	if strings.TrimSpace(string(data)) != "ref: refs/heads/"+branch {
		return false, "HEAD is not the expected branch"
	}
	return true, ""
}

// baseRepoGitTimeout bounds each git command RemoveWorktreeEntryIn and
// DeleteBranchIn run, so a repository file that makes git wait (for
// example a FIFO) cannot hold the caller forever.
var baseRepoGitTimeout = 2 * time.Minute

// baseRepoGitDir returns repoRoot's .git directory once repoRoot is shown to
// be a main repository: an absolute path whose .git is a directory (not a
// gitdir file) without a commondir file. It returns an error otherwise.
func baseRepoGitDir(repoRoot string) (string, error) {
	if repoRoot == "" || !filepath.IsAbs(repoRoot) {
		return "", fmt.Errorf("repository root %q must be an absolute path", repoRoot)
	}
	gitDir := filepath.Join(repoRoot, ".git")
	fi, err := os.Stat(gitDir)
	if err != nil {
		return "", fmt.Errorf("repository root %q: %w", repoRoot, err)
	}
	if !fi.IsDir() {
		return "", fmt.Errorf("repository root %q: .git is not a directory", repoRoot)
	}
	if _, err := os.Lstat(filepath.Join(gitDir, "commondir")); err == nil {
		return "", fmt.Errorf("repository root %q: .git has a commondir file", repoRoot)
	}
	return gitDir, nil
}

// BaseRepoGitCommand returns a git command that acts on the main repository
// at repoRoot and nothing else. GIT_DIR and GIT_COMMON_DIR are pinned to
// repoRoot/.git, so git does not discover the repository from the current
// directory or follow any worktree's metadata; GIT_* variables inherited
// from the process are dropped except the set HostGitEnv keeps (the host
// user's own config and transport/auth — GIT_CONFIG_GLOBAL/SYSTEM/NOSYSTEM,
// GIT_SSH*, GIT_ASKPASS, GIT_SSL_CA*, GIT_ALLOW_PROTOCOL,
// GIT_CEILING_DIRECTORIES, ...) and LC_ALL=C is set (HostGitEnv); core.hooksPath points at an empty
// location (os.DevNull), so hook files in the repository are not run; and
// the file-system monitor is off. It returns an error, and no command, when
// repoRoot is not a main repository (baseRepoGitDir), and, as HostGitCommand
// does, when args is empty or its first argument is empty or an option
// (errHostGitArgsOption).
//
// git still reads the repository's own configuration. core.hooksPath does
// not cover hooks defined in configuration (hook.<name>.command with
// hook.<name>.event, directly or through include.path), so such a hook in
// the repository's configuration still runs for the commands built here
// (for example reference-transaction on branch -D). Making the repository
// configuration read-only from agent containers is a follow-up.
func BaseRepoGitCommand(ctx context.Context, repoRoot string, args ...string) (*exec.Cmd, error) {
	if len(args) == 0 || args[0] == "" || strings.HasPrefix(args[0], "-") {
		return nil, errHostGitArgsOption
	}
	gitDir, err := baseRepoGitDir(repoRoot)
	if err != nil {
		return nil, err
	}
	// Host-side git on the base repository runs with the fixed settings and the
	// scrubbed environment every host git call uses (HostGitArgs/HostGitEnv): no
	// hooks, no fsmonitor, and credential helpers supplied through the environment
	// are dropped, with the repository pinned by GIT_DIR/GIT_COMMON_DIR rather than
	// by anything the worktree can rewrite. HostGitEnv's LC_ALL=C keeps porcelain
	// output parseable.
	full := append(append(HostGitArgs(), "-C", repoRoot), args...)
	cmd := exec.CommandContext(ctx, "git", full...)
	cmd.WaitDelay = 5 * time.Second
	cmd.Env = append(HostGitEnv(os.Environ()), "GIT_DIR="+gitDir, "GIT_COMMON_DIR="+gitDir)
	return cmd, nil
}

// RemoveWorktreeEntryIn removes git's record of the single worktree at path
// from the repository at repoRoot, using "git worktree remove" (without
// --force). Unlike a repository-wide "git worktree prune", it never touches
// the entries of other worktrees.
//
// It only acts once the worktree directory is gone: when path still exists
// it does nothing. Without --force, git refuses to delete a checkout that
// has modified or untracked files. A clean directory that reappears at path
// between the existence check and the git call (for example a concurrent
// re-create at the same path) is still deleted by git, so callers rely on
// their own ordering: call this only after the worktree at path has been
// removed. It does nothing when git has no entry for path
// (already removed or never registered), and inside agent containers
// (SCION_HOST_UID set), where a host path can look missing while the
// worktree is in use. path must be absolute, so the existence check and git
// see the same location.
//
// git matches a path with more than one missing trailing component only by
// its literal text against the recorded entry, and whether that recorded
// path is symlink-resolved depends on the layout when the worktree was
// created. So the cleaned path is tried first and, only if git does not
// know it, again with its existing ancestor's symlinks resolved; both name
// the same location. Any other failure (for example a locked entry) is
// returned; callers must not fall back to a repository-wide prune.
func RemoveWorktreeEntryIn(repoRoot, path string) error {
	if os.Getenv("SCION_HOST_UID") != "" {
		return nil
	}
	if repoRoot == "" || path == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), baseRepoGitTimeout)
	defer cancel()
	return RemoveWorktreeEntry(ctx, path, func(ctx context.Context, args ...string) (*exec.Cmd, error) {
		return BaseRepoGitCommand(ctx, repoRoot, args...)
	})
}

// RemoveWorktreeEntry is the contract of RemoveWorktreeEntryIn without its
// agent-container guard, for callers that run git their own way (for
// example with extra -c settings or environment for a shared checkout).
// newCmd builds the git command for the given arguments, already pointed
// at the repository; RemoveWorktreeEntry never adds -C itself. It appends
// LC_ALL=C to the command's environment so git's messages can be matched,
// whatever environment newCmd built (one from HostGitEnv already has it).
//
// It acts only when path is absent (Lstat reports it does not exist), runs
// "git worktree remove <path>" without --force, first with the cleaned path
// and, only if git does not know it, again with its existing ancestor's
// symlinks resolved. "is not a working tree" after the last attempt means
// git has no entry for path and is not an error. Before any git call, a
// path whose existing part resolves to anything other than the agent's own
// path (a link below the directory holding the agent directories) is
// refused with an error, since git would follow the same link. Any other
// failure is returned; callers must not fall back to a repository-wide
// prune.
func RemoveWorktreeEntry(ctx context.Context, path string, newCmd func(ctx context.Context, args ...string) (*exec.Cmd, error)) error {
	if path == "" {
		return nil
	}
	if !filepath.IsAbs(path) {
		return fmt.Errorf("worktree entry %q: path must be absolute", path)
	}
	// Clean first, so git gets exactly the path the checks below approve:
	// git would resolve a ".." after a link physically, not lexically.
	path = filepath.Clean(path)
	if _, err := os.Lstat(path); err == nil {
		return nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("worktree entry %s: %w", path, err)
	}
	// Resolve first: git itself follows symlinks in the existing part of
	// path, so a path whose existing part resolves to a path other than the
	// agent's own directory is refused before any git call.
	resolved, state, err := resolveMissingPath(path)
	if err != nil {
		return fmt.Errorf("worktree entry %s: %w", path, err)
	}
	if state == resolvedOutside {
		return fmt.Errorf("worktree entry %s: resolves to a path other than the agent's own directory; left alone", path)
	}
	known, err := removeWorktreeEntryAt(ctx, newCmd, path)
	if err != nil || known {
		return err
	}
	if state != resolvedWithin || resolved == path {
		return nil
	}
	_, err = removeWorktreeEntryAt(ctx, newCmd, resolved)
	return err
}

// removeWorktreeEntryAt runs "git worktree remove target" through newCmd.
// It reports known=false, with no error, when git has no worktree at
// target.
func removeWorktreeEntryAt(ctx context.Context, newCmd func(ctx context.Context, args ...string) (*exec.Cmd, error), target string) (known bool, err error) {
	cmd, err := newCmd(ctx, "worktree", "remove", target)
	if err != nil {
		return true, fmt.Errorf("git worktree remove %s: %w", target, err)
	}
	if cmd.Env == nil {
		cmd.Env = os.Environ()
	}
	// newCmd may build any environment, so LC_ALL=C is appended here as well
	// (os/exec keeps the last of duplicate entries).
	cmd.Env = append(cmd.Env, "LC_ALL=C")
	out, err := cmd.CombinedOutput()
	if err != nil {
		if strings.Contains(string(out), "is not a working tree") {
			return false, nil
		}
		return true, fmt.Errorf("git worktree remove %s: %w: %s", target, err, strings.TrimSpace(string(out)))
	}
	return true, nil
}

// resolveState is what resolveMissingPath found.
type resolveState int

const (
	// resolvedLiteralOnly: no resolved form; only the cleaned path is
	// used. The deepest existing ancestor is above the bound (so git can
	// only match the path's text) or cannot be resolved (a dangling link,
	// which git cannot follow either).
	resolvedLiteralOnly resolveState = iota
	// resolvedWithin: the resolved form keeps the path's own position under
	// the bound.
	resolvedWithin
	// resolvedOutside: the existing part of the path resolves somewhere
	// else (outside the bound, or to another directory under it); the path
	// must not be used at all.
	resolvedOutside
)

// resolveMissingPath returns path with the symlinks of its deepest existing
// ancestor resolved and the missing tail rejoined, keeping resolution within
// the agent directory. The bound is path's grandparent, the directory that
// holds the agent directories (agents/ for agents/<name>/workspace, the
// shared checkout for worktrees/<name>); it must exist even when the agent's
// own directory is already gone. Links on the bound or above it are
// resolved, but no component below the bound may be a link: the resolved
// form must name the agent's own path, never a sibling agent's directory or
// anything outside. An Lstat error other than "does not exist" on the way up
// is returned.
func resolveMissingPath(path string) (resolved string, state resolveState, err error) {
	clean := filepath.Clean(path)
	bound := filepath.Dir(filepath.Dir(clean))
	dir := clean
	var tail []string
	for {
		if _, err := os.Lstat(dir); err == nil {
			break
		} else if !errors.Is(err, fs.ErrNotExist) {
			return "", resolvedLiteralOnly, err
		}
		if dir == bound {
			return "", resolvedLiteralOnly, nil
		}
		tail = append([]string{filepath.Base(dir)}, tail...)
		dir = filepath.Dir(dir)
	}
	resolvedBound, err := filepath.EvalSymlinks(bound)
	if err != nil {
		return "", resolvedLiteralOnly, nil
	}
	resolvedDir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return "", resolvedLiteralOnly, nil
	}
	// The existing ancestor must keep its position under the bound once
	// resolved: no component between the bound and the missing tail may be
	// a link, so the resolved form names the agent's own path and never a
	// sibling's or anything outside.
	lexRel, lexErr := filepath.Rel(bound, dir)
	rel, relErr := filepath.Rel(resolvedBound, resolvedDir)
	if lexErr != nil || relErr != nil || rel != lexRel {
		return "", resolvedOutside, nil
	}
	return filepath.Join(append([]string{resolvedDir}, tail...)...), resolvedWithin, nil
}

// DeleteBranchIn deletes a git branch from the main repository at repoRoot,
// through BaseRepoGitCommand (repository pinned, hook files not run; see
// there for configuration-defined hooks) with "--" before
// the branch name. A branch name that is empty or starts with "-" is
// refused. Returns true if the branch was deleted, false otherwise.
func DeleteBranchIn(repoRoot, branchName string) bool {
	if branchName == "" || strings.HasPrefix(branchName, "-") {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), baseRepoGitTimeout)
	defer cancel()
	cmd, err := BaseRepoGitCommand(ctx, repoRoot, "branch", "-D", "--", branchName)
	if err != nil {
		return false
	}
	return cmd.Run() == nil
}

// FindWorktreeByBranchIn returns the absolute path of the worktree checked
// out to the specified branch in the repository at repoRoot. It returns an
// empty string if not found. The lookup runs against repoRoot, never the
// process's current directory, so it gives the same answer whether the caller
// runs inside the repository (a local CLI) or outside it (a runtime broker).
func FindWorktreeByBranchIn(repoRoot, branchName string) (string, error) {
	if repoRoot == "" {
		return "", fmt.Errorf("find worktree for branch %q: no repository directory given", branchName)
	}
	cmd := hostGit("-C", repoRoot, "worktree", "list", "--porcelain")
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

	cmd := hostGit("-C", repoRoot, "worktree", "list", "--porcelain")
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

// BranchExistsIn returns true if the branch exists in the repository at
// repoRoot. Like FindWorktreeByBranchIn, it never consults the process's
// current directory; an empty repoRoot reports false.
func BranchExistsIn(repoRoot, branchName string) bool {
	if repoRoot == "" {
		return false
	}
	cmd := hostGit("-C", repoRoot, "show-ref", "--verify", "--quiet", "refs/heads/"+branchName)
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
	cmd := hostGit("remote", "get-url", "origin")
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

	// Handle SSH shorthand format (git@host:path → host/path). A bracketed
	// IPv6 host (git@[::1]:path) is split after its closing ']'; one followed
	// by '/' or the end (from ssh://git@[::1]/path) is left as is. A bracketed
	// host that is not an IPv6 literal, or is followed by anything else
	// (git@[x@y]:path, git@[::1]x@y:path), is left without the userinfo strip
	// below, so any '@' in it remains (callers that persist the result refuse
	// a normalized remote with '@').
	malformedBracket := false
	if strings.HasPrefix(remote, "git@") {
		remote = strings.TrimPrefix(remote, "git@")
		if strings.HasPrefix(remote, "[") {
			end := strings.Index(remote, "]")
			switch {
			case end < 0 || !isBracketedIPv6(remote[:end+1]):
				malformedBracket = true
			case strings.HasPrefix(remote[end+1:], ":"):
				remote = remote[:end+1] + "/" + remote[end+2:]
			case end+1 == len(remote) || remote[end+1] == '/':
				// ssh://git@[::1]/path: nothing to split.
			default:
				malformedBracket = true
			}
		} else {
			remote = strings.Replace(remote, ":", "/", 1)
		}
	}

	// Strip user info (user@, user:pass@) for scheme-based URLs.
	// This handles token-authenticated HTTPS URLs like x-access-token:TOKEN@github.com/...
	if atIdx := strings.Index(remote, "@"); atIdx >= 0 && !malformedBracket {
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

// StripGitURLCredentials removes credentials from a git remote URL so it can
// be stored or displayed safely (for example in a readable project label).
//
//   - http(s):// and git:// URLs lose their userinfo entirely, e.g.
//     https://user:TOKEN@github.com/org/repo.git -> https://github.com/org/repo.git
//   - ssh:// URLs keep the login name (it selects the SSH account and is not
//     a secret) but lose any password: ssh://git:pw@host/x -> ssh://git@host/x
//   - SCP-style shorthand (git@host:org/repo) carries no password and is
//     returned unchanged, as is anything without a "scheme://" prefix.
//
// The userinfo ends at the first '@' that is preceded by a valid login (no
// '/' before the first ':') and followed by a host (no '@' or '/') — the RFC
// 3986 authority with a lenient password. So a password containing an
// unencoded '/' or '@' (https://u:p/w@host/org/repo) is still removed rather
// than left in place, and a '@' after the start of the path (.../repo@v1,
// .../repo@github.com/x) is never treated as userinfo.
//
// The URL is edited textually rather than round-tripped through net/url so
// that the rest of it is preserved byte-for-byte.
func StripGitURLCredentials(remote string) string {
	schemeEnd := strings.Index(remote, "://")
	if schemeEnd < 0 {
		return remote
	}
	scheme := strings.ToLower(remote[:schemeEnd])
	authorityStart := schemeEnd + len("://")
	rest := remote[authorityStart:]
	limit := strings.IndexAny(rest, "?#")
	if limit < 0 {
		limit = len(rest)
	}
	at := userinfoEnd(rest[:limit])
	if at < 0 {
		return remote
	}
	userinfo, hostAndPath := rest[:at], rest[at+1:]
	if scheme == "ssh" {
		if colon := strings.Index(userinfo, ":"); colon >= 0 {
			userinfo = userinfo[:colon]
		}
		if userinfo != "" && !strings.ContainsAny(userinfo, "/@") {
			return remote[:authorityStart] + userinfo + "@" + hostAndPath
		}
	}
	return remote[:authorityStart] + hostAndPath
}

// userinfoEnd returns the index of the '@' ending the userinfo of s (a URL
// with its "scheme://" prefix, query and fragment removed), or -1 if s has no
// userinfo. It picks the first '@' such that the login before it (up to the
// first ':') contains no '/', and the host after it (up to the next '/') is
// non-empty and contains no '@'. Once a '/' appears in the login position the
// path has started, so no later '@' can end the userinfo.
//
// A '/' after the first ':' is ambiguous: "host:8443/org/repo@v1" is a port
// followed by a path with '@', while "user:pa/ss@host/repo" is a password
// with an unencoded '/'. When the text before that '/' is a valid host:port
// (a 1-65535 port without leading zeros), s is read as RFC 3986 does, with no
// userinfo. Otherwise it cannot be a valid authority, so the '@' is taken to
// end a password and the credential is stripped (fail closed). Callers that
// persist the result must still reject '@' in the path, since a password
// that looks like a port ("user:8443/x@host/repo") is left in place.
func userinfoEnd(s string) int {
	for from := 0; ; {
		i := strings.Index(s[from:], "@")
		if i < 0 {
			return -1
		}
		at := from + i
		userinfo := s[:at]
		login, _, _ := strings.Cut(userinfo, ":")
		if strings.Contains(login, "/") {
			return -1
		}
		if authority, _, ok := strings.Cut(userinfo, "/"); ok && isHostAndPort(authority) {
			return -1
		}
		host, _, _ := strings.Cut(s[at+1:], "/")
		if host != "" && !strings.Contains(host, "@") {
			return at
		}
		from = at + 1
	}
}

// isHostAndPort reports whether s is a non-empty host followed by ':' and a
// port of 1-65535 without leading zeros (e.g. "host:8443", "[::1]:8443").
func isHostAndPort(s string) bool {
	colon := strings.LastIndex(s, ":")
	if colon <= 0 {
		return false
	}
	port := s[colon+1:]
	if port == "" || len(port) > 5 || port[0] == '0' {
		return false
	}
	n := 0
	for _, c := range port {
		if c < '0' || c > '9' {
			return false
		}
		n = n*10 + int(c-'0')
	}
	return n <= 65535
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

// CloneSharedWorkspace clones a git repository into the specified workspace path
// for use as a shared workspace project. It configures git identity and
// optionally authenticates with a token.
//
// The token is never embedded in the clone URL (which would persist it in the
// workspace .git/config, readable by agents). Instead it is supplied through a
// credential helper scoped to the clone URL's host (ScopedCredentialHelperEnv),
// and only for an http(s) URL; for any other URL the token is ignored. The
// clone runs with the LFS smudge skipped, so git-lfs never contacts an endpoint
// from a tracked .lfsconfig with the host's credentials.
//
// If the requested branch does not exist on the remote, the clone falls back to
// the remote's default branch and creates the requested branch locally.
func CloneSharedWorkspace(workspacePath, cloneURL, branch, token string) error {
	return cloneSharedWorkspace(workspacePath, cloneURL, branch, token, HostGitHardening{})
}

// HostGitHardening is what a host-side clone or fetch of a URL the host
// does not choose itself adds to the base host pins.
type HostGitHardening struct {
	// Config holds key=value settings passed as -c after HostGitArgs (and,
	// on a fetch, after the network transport allow-list), so a setting
	// here overrides a base pin of the same key.
	Config []string
	// Env is appended to the command environment.
	Env []string
	// NoCredentialHelpers runs git with no credential helper, no configured
	// extra HTTP header and no askpass (NoCredentialHelperEnv); the token is
	// not used.
	NoCredentialHelpers bool
}

// configArgs returns h.Config as -c arguments.
func (h HostGitHardening) configArgs() []string {
	var args []string
	for _, kv := range h.Config {
		args = append(args, "-c", kv)
	}
	return args
}

// CloneSharedWorkspaceHardened is CloneSharedWorkspace for a clone URL the
// host does not choose itself, with h applied to every clone subcommand.
func CloneSharedWorkspaceHardened(workspacePath, cloneURL, branch, token string, h HostGitHardening) error {
	return cloneSharedWorkspace(workspacePath, cloneURL, branch, token, h)
}

func cloneSharedWorkspace(workspacePath, cloneURL, branch, token string, h HostGitHardening) error {
	gitArgs := h.configArgs()
	baseEnv := append(HostGitEnv(os.Environ()), "GIT_TERMINAL_PROMPT=0", "GIT_LFS_SKIP_SMUDGE=1")
	baseEnv = append(baseEnv, h.Env...)
	env := baseEnv
	if h.NoCredentialHelpers {
		env = NoCredentialHelperEnv(baseEnv, cloneURL)
	} else if token != "" {
		if scoped, ok := ScopedCredentialHelperEnv(baseEnv, cloneURL, token); ok {
			env = scoped
		}
		// Non-http(s) URL: the token is ignored, matching the previous
		// behaviour where it was only ever injected for https.
	}

	runClone := func(args ...string) ([]byte, error) {
		cmd := hostGit(args...)
		cmd.Env = env
		return cmd.CombinedOutput()
	}

	// "--" ends the options: a URL or path starting with "-" is never read
	// as one.
	args := append(append([]string{}, gitArgs...), "clone")
	if branch != "" {
		args = append(args, "--branch", branch)
	}
	args = append(args, "--", cloneURL, workspacePath)
	output, err := runClone(args...)

	if err != nil && branch != "" && isRemoteBranchNotFound(string(output)) {
		// The branch doesn't exist on the remote yet. Clone the default branch
		// instead and create the requested branch locally.
		_ = os.RemoveAll(workspacePath)
		output, err = runClone(append(append([]string{}, gitArgs...), "clone", "--", cloneURL, workspacePath)...)
		if err != nil {
			sanitized := strings.TrimSpace(sanitizeGitOutput(string(output), token))
			gitErr := ClassifyGitError(sanitized)
			if guidance := gitErr.UserGuidance(); guidance != "" {
				return &GitError{Kind: gitErr.Kind, Message: fmt.Sprintf("git clone failed: %s (%s)", sanitized, guidance)}
			}
			return &GitError{Kind: gitErr.Kind, Message: fmt.Sprintf("git clone failed: %s", sanitized)}
		}

		// Create the requested branch locally.
		checkoutCmd := hostGit("-C", workspacePath, "checkout", "-b", branch)
		checkoutCmd.Env = env
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

	// Pin remote.origin.url to the clean clone URL. The token was never in the
	// URL, so this is belt-and-braces; a failure to assert it is FATAL rather
	// than logged, so a credential can never be left in the shared .git/config.
	if err := gitConfig(workspacePath, "remote.origin.url", cloneURL); err != nil {
		return fmt.Errorf("failed to set a clean remote.origin.url: %w", err)
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

// PullSharedWorkspace updates the current branch of the shared workspace
// at workspacePath from cloneURL, the project's clone URL as the hub knows
// it, fast-forward only. It optionally uses a token for authentication and
// sanitizes credentials from error output. Returns structured commit
// information.
//
// Agents can write the workspace's .git, so the remote is cloneURL rather
// than the workspace's configured remote, and git runs through
// HostGitCommand: no repository hooks, no repository-defined filter
// commands, and a workspace config that sets transport, credential or
// include keys is refused (RepoConfigRefusedError). The branch fetched is the
// current branch's configured upstream branch, or the current branch's own
// name; it is fetched into refs/remotes/origin/<branch>.
func PullSharedWorkspace(workspacePath, cloneURL, token string) (*PullResult, error) {
	return pullSharedWorkspace(workspacePath, cloneURL, token, HostGitHardening{})
}

// PullSharedWorkspaceHardened is PullSharedWorkspace for a clone URL the
// host does not choose itself, with h applied to the fetch.
func PullSharedWorkspaceHardened(workspacePath, cloneURL, token string, h HostGitHardening) (*PullResult, error) {
	return pullSharedWorkspace(workspacePath, cloneURL, token, h)
}

func pullSharedWorkspace(workspacePath, cloneURL, token string, h HostGitHardening) (*PullResult, error) {
	pullErr := func(msg string) error {
		msg = sanitizeGitOutput(msg, token)
		gitErr := ClassifyGitError(msg)
		if guidance := gitErr.UserGuidance(); guidance != "" {
			return &GitError{Kind: gitErr.Kind, Message: fmt.Sprintf("git pull failed: %s (%s)", msg, guidance)}
		}
		return &GitError{Kind: gitErr.Kind, Message: fmt.Sprintf("git pull failed: %s", msg)}
	}
	if cloneURL == "" {
		return nil, pullErr("the shared workspace has no clone URL")
	}

	baseEnv := append(HostGitEnv(os.Environ()), "GIT_TERMINAL_PROMPT=0")

	// The fetch talks to the clone URL and may need the token. Scope the
	// credential helper to that URL's host only, so git never offers the
	// token to any other endpoint — in particular an LFS endpoint a tracked
	// .lfsconfig might name. The merge/checkout step below gets no credential
	// config at all and skips the LFS smudge, so no host-side checkout can
	// leak the token to an agent-chosen endpoint.
	fetchEnv := append(append([]string{}, baseEnv...), h.Env...)
	if h.NoCredentialHelpers {
		fetchEnv = NoCredentialHelperEnv(fetchEnv, cloneURL)
	} else if token != "" {
		// Only an http(s) clone URL can carry a host-scoped credential helper.
		// For an ssh/scp or local-path URL the token is ignored (git uses its
		// own transport auth), rather than failing the pull.
		if scoped, ok := ScopedCredentialHelperEnv(fetchEnv, cloneURL, token); ok {
			fetchEnv = scoped
		}
	}

	branchOut, err := hostGit("-C", workspacePath, "symbolic-ref", "--quiet", "--short", "HEAD").CombinedOutput()
	if err != nil {
		return nil, pullErr("the workspace is not on a branch: " + strings.TrimSpace(string(branchOut)))
	}
	branch := strings.TrimSpace(string(branchOut))
	remoteBranch := branch
	if merge, err := hostGit("-C", workspacePath, "config", "--get", "branch."+branch+".merge").Output(); err == nil {
		if name := strings.TrimPrefix(strings.TrimSpace(string(merge)), "refs/heads/"); name != "" {
			remoteBranch = name
		}
	}
	trackingRef := "refs/remotes/origin/" + remoteBranch
	if out, err := hostGit("check-ref-format", trackingRef).CombinedOutput(); err != nil {
		return nil, pullErr(fmt.Sprintf("invalid branch name %q: %s", remoteBranch, strings.TrimSpace(string(out))))
	}

	// Capture HEAD before pulling so we can enumerate new commits afterward.
	oldHead, _ := hostGit("-C", workspacePath, "rev-parse", "HEAD").Output()
	oldHeadStr := strings.TrimSpace(string(oldHead))

	ctx := context.Background()
	fetch, err := HostGitCommand(ctx, workspacePath, HostGitOptions{Env: fetchEnv, Network: true, Config: h.Config},
		"fetch", "--no-tags", "--", cloneURL, "+refs/heads/"+remoteBranch+":"+trackingRef)
	if err != nil {
		return nil, pullErr(err.Error())
	}
	if output, err := fetch.CombinedOutput(); err != nil {
		return nil, pullErr(strings.TrimSpace(string(output)))
	}
	// The merge writes the work tree. It gets no credential config (baseEnv,
	// not fetchEnv) and skips the LFS smudge, so git-lfs never contacts an
	// endpoint from a tracked .lfsconfig with the host's credentials.
	merge, err := HostGitCommand(ctx, workspacePath, HostGitOptions{Env: baseEnv, SkipLFSSmudge: true}, "merge", "--ff-only", trackingRef)
	if err != nil {
		return nil, pullErr(err.Error())
	}
	if output, err := merge.CombinedOutput(); err != nil {
		return nil, pullErr(strings.TrimSpace(string(output)))
	}

	newHead, _ := hostGit("-C", workspacePath, "rev-parse", "HEAD").Output()
	newHeadStr := strings.TrimSpace(string(newHead))

	result := &PullResult{}
	if oldHeadStr == "" || newHeadStr == "" || oldHeadStr == newHeadStr {
		return result, nil
	}

	result.Updated = true

	// git log runs no repository-defined command here (no pager, no
	// signature display): this relies on the fetch above, whose network
	// check refuses gpg.*, include and other repository keys outside the
	// allowlist, and on the merge's config check having passed. Keep it
	// after both.
	logCmd := hostGit("-c", "log.showSignature=false", "-C", workspacePath, "log", "--oneline", "--no-decorate",
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
	cmd := hostGit("-C", repoPath, "config", key, value)
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
// deterministic identifier needs such as cache keys. LegacyGitProjectID
// derives the same value for a normalized git remote, but pins its own copy
// of the namespace, so it does not depend on this function.
func HashProjectID(normalized string) string {
	return uuid.NewSHA1(scionNamespace, []byte(normalized)).String()
}

// AppendGitExcludeLine adds line to the info/exclude file of the git
// repository containing dir, unless a line equal to it (ignoring surrounding
// space) is already there. info/exclude is never tracked, so this changes no
// tracked file. It returns the exclude file's path.
//
// A symbolic link or other non-regular file at info or info/exclude is
// refused rather than followed.
func AppendGitExcludeLine(dir, line string) (string, error) {
	// Without --path-format=absolute, which would resolve symbolic links,
	// so that the checks below see a link at info or info/exclude. The
	// result is relative to dir unless the git directory is elsewhere.
	cmd := hostGit("rev-parse", "--git-path", "info/exclude")
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("resolve git exclude file in %s: %w", dir, err)
	}
	excludePath := strings.TrimSpace(string(out))
	if excludePath == "" {
		return "", fmt.Errorf("resolve git exclude file in %s: empty path", dir)
	}
	if !filepath.IsAbs(excludePath) {
		excludePath = filepath.Join(dir, excludePath)
	}

	infoDir := filepath.Dir(excludePath)
	if info, err := os.Lstat(infoDir); err == nil {
		if !info.IsDir() {
			return excludePath, fmt.Errorf("%s is not a directory", infoDir)
		}
	} else if os.IsNotExist(err) {
		if err := os.Mkdir(infoDir, 0755); err != nil {
			return excludePath, fmt.Errorf("create %s: %w", infoDir, err)
		}
	} else {
		return excludePath, err
	}

	var content []byte
	if info, err := os.Lstat(excludePath); err == nil {
		if !info.Mode().IsRegular() {
			return excludePath, fmt.Errorf("%s is not a regular file", excludePath)
		}
		if content, err = os.ReadFile(excludePath); err != nil {
			return excludePath, err
		}
	} else if !os.IsNotExist(err) {
		return excludePath, err
	}

	for _, l := range strings.Split(string(content), "\n") {
		if strings.TrimSpace(l) == line {
			return excludePath, nil
		}
	}
	entry := line + "\n"
	if len(content) > 0 && content[len(content)-1] != '\n' {
		entry = "\n" + entry
	}
	f, err := os.OpenFile(excludePath, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0644)
	if err != nil {
		return excludePath, err
	}
	if _, err := f.WriteString(entry); err != nil {
		_ = f.Close()
		return excludePath, err
	}
	return excludePath, f.Close()
}

// EnsureGitIgnoresFile makes sure that the file name in dir is ignored by
// git when dir is inside a git work tree, by adding name (unanchored, so it
// matches that name in any directory) to the repository's info/exclude when
// no existing rule already ignores it. Outside a work tree, or without git,
// it does nothing.
func EnsureGitIgnoresFile(dir, name string) error {
	cmd := hostGit("rev-parse", "--is-inside-work-tree")
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil || strings.TrimSpace(string(out)) != "true" {
		return nil
	}
	if IsIgnored(dir, name) {
		return nil
	}
	excludePath, err := AppendGitExcludeLine(dir, name)
	if err != nil {
		return err
	}
	if !IsIgnored(dir, name) {
		return fmt.Errorf("the repository at %s un-ignores %s; the rule in %s has no effect", dir, name, excludePath)
	}
	return nil
}

// smallGitFileMax caps how much of a small git metadata file (HEAD, a .git
// pointer file, commondir) is read. Used by readSmallRegularFile callers.
const smallGitFileMax = 4096

// ReadSmallRegularFile reads the file at path when it is a regular file of at
// most limit bytes, rejecting anything else. On unix it opens with O_NOFOLLOW
// and O_NONBLOCK, so a symlink or FIFO at the path cannot redirect or block the
// read; on other platforms it rejects a symlink with an Lstat check before
// opening. It is exported for callers outside this package that read small,
// broker-owned metadata files.
func ReadSmallRegularFile(path string, limit int64) ([]byte, error) {
	return readSmallRegularFile(path, limit)
}

// hardenedGitCommand returns a git command that reads no system or global
// configuration and never prompts, for checks whose answer the broker
// relies on about a repository an agent may be able to write.
func hardenedGitCommand(dir string, args ...string) *exec.Cmd {
	cmd := exec.Command("git", append([]string{"-c", "core.fsmonitor=false"}, args...)...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_TERMINAL_PROMPT=0")
	return cmd
}

// IsRegisteredLinkedWorktree reports whether path is a linked worktree (not
// the main worktree) that git has registered for the repository at
// repoRoot, matched by resolved path equality like IsRegisteredWorktree.
// It runs git with hardenedGitCommand. A failure to list returns
// (false, err); callers must treat that as not verified.
func IsRegisteredLinkedWorktree(repoRoot, path string) (bool, error) {
	resolvedPath, err := filepath.EvalSymlinks(path)
	if err != nil {
		return false, fmt.Errorf("resolve %s: %w", path, err)
	}
	out, err := hardenedGitCommand(repoRoot, "rev-parse", "--path-format=absolute", "--git-common-dir").Output()
	if err != nil {
		return false, fmt.Errorf("git common dir for %s: %w", repoRoot, err)
	}
	resolvedCommonDir, err := filepath.EvalSymlinks(strings.TrimSpace(string(out)))
	if err != nil {
		return false, fmt.Errorf("resolve git common dir: %w", err)
	}
	worktreesDir := filepath.Join(resolvedCommonDir, "worktrees")
	output, err := hardenedGitCommand(repoRoot, "worktree", "list", "--porcelain").Output()
	if err != nil {
		return false, fmt.Errorf("git worktree list: %w", err)
	}
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
			continue
		}
		resolvedWt, err := filepath.EvalSymlinks(wtPath)
		if err != nil || resolvedWt != resolvedPath {
			continue
		}
		if isMainWorktreeOf(wtPath, resolvedCommonDir) {
			return false, nil
		}
		return isLinkedWorktreeOf(wtPath, worktreesDir), nil
	}
	return false, nil
}

// HardenedRepoRootDir is RepoRootDir (git rev-parse --show-toplevel run in
// dir) with hardenedGitCommand, without walking up on failure.
func HardenedRepoRootDir(dir string) (string, error) {
	out, err := hardenedGitCommand(dir, "rev-parse", "--show-toplevel").Output()
	if err != nil {
		return "", fmt.Errorf("git toplevel for %s: %w", dir, err)
	}
	return strings.TrimSpace(string(out)), nil
}
