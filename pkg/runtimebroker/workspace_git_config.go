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

package runtimebroker

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"regexp"
	"strings"
)

// Workspace sync never carries a repository's configuration (see
// transfer.ClassifyWorkspaceSyncPath): a workspace downloaded onto a broker
// for the first time gets the repository's history but no .git/config. The
// broker writes a minimal one itself, from what it knows, so the checkout
// works and can push.

// minimalGitConfigCore is the [core] section of a minimal repository
// configuration for a SHA-1 repository. It holds no key that names a
// program, a path or a command (no hooksPath, fsmonitor, sshCommand,
// editor, pager, alias or filter). Every value the broker writes comes from
// a fixed template here, the hub-known clone URL, or a branch name and
// object format read from the repository's HEAD and strictly validated;
// nothing is copied from the source repository's configuration.
const minimalGitConfigCore = "[core]\n" +
	"\trepositoryformatversion = 0\n" +
	"\tfilemode = true\n" +
	"\tbare = false\n" +
	"\tlogallrefupdates = true\n"

// sha256GitConfigCore is the [core] and [extensions] sections for a
// repository whose objects use SHA-256 (format version 1 is required for
// the extension).
const sha256GitConfigCore = "[core]\n" +
	"\trepositoryformatversion = 1\n" +
	"\tfilemode = true\n" +
	"\tbare = false\n" +
	"\tlogallrefupdates = true\n" +
	"[extensions]\n" +
	"\tobjectformat = sha256\n"

// branchNameChars is the only character class a branch name written into
// the configuration may use. None of them needs escaping inside a quoted
// subsection name (only '"' and '\\' would); quotedSubsectionSafe checks
// this.
const branchNameChars = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789._/-"

// quotedSubsectionSafe reports whether every character of chars may appear
// unescaped in a quoted git configuration subsection name.
func quotedSubsectionSafe(chars string) bool {
	return !strings.ContainsAny(chars, "\"\\\n\x00")
}

func init() {
	if !quotedSubsectionSafe(branchNameChars) {
		panic("branchNameChars needs escaping in a quoted subsection")
	}
}

// maxBranchNameLen bounds a branch name written into the configuration.
const maxBranchNameLen = 200

// validBranchName reports whether name may be written as a branch
// subsection: only branchNameChars, at most maxBranchNameLen bytes, git's
// check-ref-format rules for the whole name (no "..", no leading '-' or
// '/', no trailing '/' or '.') and for every '/'-separated component (not
// empty, no leading '.', no ".lock" suffix).
func validBranchName(name string) bool {
	if name == "" || len(name) > maxBranchNameLen {
		return false
	}
	for _, c := range name {
		if !strings.ContainsRune(branchNameChars, c) {
			return false
		}
	}
	if strings.Contains(name, "..") || strings.HasPrefix(name, "-") || strings.HasPrefix(name, "/") ||
		strings.HasSuffix(name, "/") || strings.HasSuffix(name, ".") {
		return false
	}
	for _, comp := range strings.Split(name, "/") {
		if comp == "" || strings.HasPrefix(comp, ".") || strings.HasSuffix(comp, ".lock") {
			return false
		}
	}
	return true
}

// headInfo is what ensureMinimalGitConfig reads from a repository's HEAD.
type headInfo struct {
	branch string // the branch HEAD names, when valid
	sha256 bool   // the object ids are 64 hex digits
}

// maxRefFileBytes bounds the HEAD and loose ref files read.
const maxRefFileBytes = 4096

// isObjectID reports whether s is 40 or 64 lowercase hex digits.
func isObjectID(s string) bool {
	if len(s) != 40 && len(s) != 64 {
		return false
	}
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// readSmallRegular reads name in dir when it is a regular file of at most
// limit bytes.
func readSmallRegular(dir *os.Root, name string, limit int64) (string, bool) {
	info, err := dir.Lstat(name)
	if err != nil || !info.Mode().IsRegular() || info.Size() > limit {
		return "", false
	}
	b, err := dir.ReadFile(name)
	if err != nil {
		return "", false
	}
	return string(b), true
}

// readHeadInfo reads the branch HEAD names (when valid) and the object
// format from the repository in gitDir. An id is looked up in the loose
// ref, then in packed-refs; an unborn branch reads as SHA-1.
func readHeadInfo(gitDir *os.Root) headInfo {
	var hi headInfo
	head, ok := readSmallRegular(gitDir, "HEAD", maxRefFileBytes)
	if !ok {
		return hi
	}
	head = strings.TrimSuffix(head, "\n")
	if isObjectID(head) {
		hi.sha256 = len(head) == 64
		return hi
	}
	name, ok := strings.CutPrefix(head, "ref: refs/heads/")
	if !ok || !validBranchName(name) {
		return hi
	}
	hi.branch = name
	if id, ok := readSmallRegular(gitDir, "refs/heads/"+name, maxRefFileBytes); ok && isObjectID(strings.TrimSuffix(id, "\n")) {
		hi.sha256 = len(strings.TrimSuffix(id, "\n")) == 64
		return hi
	}
	if packed, ok := readSmallRegular(gitDir, "packed-refs", 64<<20); ok {
		for _, line := range strings.Split(packed, "\n") {
			if id, ref, ok := strings.Cut(line, " "); ok && ref == "refs/heads/"+name && isObjectID(id) {
				hi.sha256 = len(id) == 64
				break
			}
		}
	}
	return hi
}

// scpCloneURL matches the scp-style ssh remote the hub keeps as a clone URL
// ("git@host:org/repo"; see util.NormalizeCloneURL).
var scpCloneURL = regexp.MustCompile(`^git@[A-Za-z0-9.\-\[\]:]+:[A-Za-z0-9._~/\-]+$`)

// errUnsafeCloneURL marks a clone URL that is not written into a
// repository configuration.
var errUnsafeCloneURL = errors.New("clone URL is not a network git remote")

// validateOriginURL checks that cloneURL can be written as remote.origin.url:
// an http(s)://, ssh:// or git:// URL, or an scp-style git@host:path remote.
// Local paths, file:// URLs and transport-helper forms such as ext:: or
// file:: are refused (a fetch from them can run a program or read the
// broker's filesystem), and so are characters that could end the value or
// start a new configuration line.
func validateOriginURL(cloneURL string) error {
	if cloneURL == "" || strings.HasPrefix(cloneURL, "-") || strings.Contains(cloneURL, "::") {
		return errUnsafeCloneURL
	}
	for _, r := range cloneURL {
		if r < 0x21 || r > 0x7e || r == '"' || r == '\\' || r == ';' || r == '#' {
			return errUnsafeCloneURL
		}
	}
	for _, scheme := range []string{"https://", "http://", "ssh://", "git://"} {
		if strings.HasPrefix(strings.ToLower(cloneURL), scheme) && len(cloneURL) > len(scheme) {
			return nil
		}
	}
	if scpCloneURL.MatchString(cloneURL) {
		return nil
	}
	return errUnsafeCloneURL
}

// minimalGitConfig returns the minimal repository configuration: core
// settings (SHA-256 when hi says so), an origin remote when originURL is
// not empty (already validated), and a tracking entry for hi.branch when
// there is an origin and a branch.
func minimalGitConfig(originURL string, hi headInfo) string {
	cfg := minimalGitConfigCore
	if hi.sha256 {
		cfg = sha256GitConfigCore
	}
	if originURL != "" {
		cfg += "[remote \"origin\"]\n" +
			"\turl = " + originURL + "\n" +
			"\tfetch = +refs/heads/*:refs/remotes/origin/*\n"
		if hi.branch != "" && validBranchName(hi.branch) {
			cfg += "[branch \"" + hi.branch + "\"]\n" +
				"\tremote = origin\n" +
				"\tmerge = refs/heads/" + hi.branch + "\n"
		}
	}
	return cfg
}

// ensureMinimalGitConfig writes a minimal .git/config into the workspace at
// workspaceDir when its root .git is a real directory without a config. The
// origin remote is set from cloneURL when it passes validateOriginURL;
// otherwise the configuration has no remote and the returned error (which
// names no path) says why. An existing config, a .git file (a worktree's
// gitdir pointer), a symlink or a missing .git is left alone. It reports
// whether it wrote a configuration.
func ensureMinimalGitConfig(workspaceDir, cloneURL string) (bool, error) {
	root, err := os.OpenRoot(workspaceDir)
	if err != nil {
		return false, err
	}
	defer func() { _ = root.Close() }()

	info, err := root.Lstat(".git")
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !info.IsDir() {
		return false, nil
	}
	if _, err := root.Lstat(".git/config"); err == nil || !errors.Is(err, fs.ErrNotExist) {
		return false, err
	}

	origin := ""
	urlErr := validateOriginURL(cloneURL)
	if cloneURL != "" && urlErr == nil {
		origin = cloneURL
	}

	gitDir, err := root.OpenRoot(".git")
	if err != nil {
		return false, err
	}
	defer func() { _ = gitDir.Close() }()
	opened, err := gitDir.Stat(".")
	if err != nil || !os.SameFile(info, opened) {
		return false, errors.New("workspace .git changed while writing its configuration")
	}
	f, err := gitDir.OpenFile("config", os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return false, err
	}
	_, werr := f.WriteString(minimalGitConfig(origin, readHeadInfo(gitDir)))
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		return false, werr
	}
	if cloneURL != "" && urlErr != nil {
		return true, fmt.Errorf("origin remote not set: %w", urlErr)
	}
	return true, nil
}
