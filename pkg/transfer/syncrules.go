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

package transfer

import (
	"strings"
	"unicode"
)

// This file is the single exclude list for workspace sync: the manifest a
// broker builds, every rclone transfer of a workspace to or from the bucket
// (pkg/gcp), the confined apply of a downloaded workspace, and the CLI's
// sync-from download all classify paths with ClassifyWorkspaceSyncPath or
// with the rclone rules below, which a parity test checks against it.
//
// Three classes:
//   - content: ordinary workspace files, synced in every direction;
//   - git data: the repository's history (HEAD, packed-refs, index, refs,
//     objects) in the workspace-root .git directory. It is carried only by
//     the legs that ship a whole repository between hosts (workspace
//     upload, hub create-time upload, broker create-time download, hub
//     sync-back), never deleted on the receiver, and never written into a
//     user's checkout by the CLI;
//   - never synced: everything else under any .git (config, hooks, info,
//     modules, worktrees, logs, objects/info such as alternates, and every
//     .git below the root, such as a submodule's gitdir file), every .scion
//     entry at any depth (the project's own configuration, wherever the
//     project sits in the repository), and .DS_Store.
//
// Names are compared without regard to case (and ignoring invisible
// format characters), because a case-insensitive receiver (macOS, or a
// Windows checkout) treats ".GIT" or ".Scion" as the real entry. The git
// data allowlist is matched exactly: a case variant of an allowed name is
// never synced.

// WorkspaceSyncClass is how workspace sync treats a workspace-relative path.
type WorkspaceSyncClass int

const (
	// WorkspaceSyncContent is an ordinary workspace file or directory.
	WorkspaceSyncContent WorkspaceSyncClass = iota
	// WorkspaceSyncGitData is repository history in the root .git directory.
	WorkspaceSyncGitData
	// WorkspaceSyncNever is never synced in any direction.
	WorkspaceSyncNever
)

// gitDataFiles are the files directly under the root .git that carry
// repository state as plain data.
var gitDataFiles = map[string]bool{"HEAD": true, "packed-refs": true, "index": true}

// gitDataTrees are the directories directly under the root .git whose whole
// subtree is git data (except objects/info, in any letter case).
var gitDataTrees = map[string]bool{"refs": true, "objects": true}

const (
	dotGit   = ".git"
	dotScion = ".scion"
	dsStore  = ".DS_Store"
)

// isName reports whether elem names the entry name on a case-insensitive
// filesystem: equal under Unicode case folding once invisible format
// characters (such as zero-width joiners, which some filesystems ignore)
// are dropped.
func isName(elem, name string) bool {
	if strings.EqualFold(elem, name) {
		return true
	}
	stripped := strings.Map(func(r rune) rune {
		if unicode.Is(unicode.Cf, r) {
			return -1
		}
		return r
	}, elem)
	return stripped != elem && strings.EqualFold(stripped, name)
}

// ClassifyWorkspaceSyncPath classifies rel, a slash-separated path relative
// to the workspace root. isDir reports whether rel names a directory; it
// only matters for the directories leading to git data (.git, .git/refs,
// .git/objects), which are classified as git data so a walk descends into
// them. A path that is not canonical (empty, absolute, or holding "." or
// ".." elements) is never synced.
func ClassifyWorkspaceSyncPath(rel string, isDir bool) WorkspaceSyncClass {
	if rel == "" || strings.HasPrefix(rel, "/") {
		return WorkspaceSyncNever
	}
	parts := strings.Split(rel, "/")
	for i, p := range parts {
		switch {
		case p == "" || p == "." || p == "..":
			return WorkspaceSyncNever
		case isName(p, dotScion) || isName(p, dsStore):
			return WorkspaceSyncNever
		case i > 0 && isName(p, dotGit):
			// A .git below the root (submodule gitdir file, nested
			// repository), including one inside the root .git.
			return WorkspaceSyncNever
		}
	}
	if !isName(parts[0], dotGit) {
		return WorkspaceSyncContent
	}
	if parts[0] != dotGit {
		// A case variant of the root .git.
		return WorkspaceSyncNever
	}
	// Under the root .git.
	if len(parts) == 1 {
		if isDir {
			return WorkspaceSyncGitData
		}
		// A .git file is a worktree's or submodule's pointer to a gitdir
		// on the host.
		return WorkspaceSyncNever
	}
	if len(parts) > 2 && parts[1] == "objects" && isName(parts[2], "info") {
		return WorkspaceSyncNever
	}
	if len(parts) == 2 {
		if gitDataFiles[parts[1]] && !isDir {
			return WorkspaceSyncGitData
		}
		if gitDataTrees[parts[1]] && isDir {
			return WorkspaceSyncGitData
		}
		return WorkspaceSyncNever
	}
	if gitDataTrees[parts[1]] {
		return WorkspaceSyncGitData
	}
	return WorkspaceSyncNever
}

// IsWorkspaceSyncContent reports whether rel is ordinary workspace content.
func IsWorkspaceSyncContent(rel string, isDir bool) bool {
	return ClassifyWorkspaceSyncPath(rel, isDir) == WorkspaceSyncContent
}

// ciGlob returns an rclone glob matching name in any letter case (each
// letter becomes a two-letter character class).
func ciGlob(name string) string {
	var b strings.Builder
	for _, r := range name {
		lo, up := unicode.ToLower(r), unicode.ToUpper(r)
		if lo != up {
			b.WriteString("[" + string(lo) + string(up) + "]")
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// WorkspaceSyncContentRcloneRules are rclone filter rules (FilterRule
// syntax, first match wins) that pass exactly the content class for ASCII
// names. An unanchored pattern matches the name at any depth, so they drop
// every .git, .scion and .DS_Store entry in any letter case wherever it
// sits, file or directory. (Variants that differ only by non-ASCII case
// folding or invisible characters pass these rules; the receiving side's
// ApplyStagedWorkspace refuses them.)
var WorkspaceSyncContentRcloneRules = []string{
	"- " + ciGlob(dotGit),
	"- " + ciGlob(dotGit) + "/**",
	"- " + ciGlob(dotScion),
	"- " + ciGlob(dotScion) + "/**",
	"- " + ciGlob(dsStore),
	"- " + ciGlob(dsStore) + "/**",
	"+ **",
}

// WorkspaceSyncGitDataRcloneRules are rclone filter rules (FilterRule
// syntax, first match wins) that pass exactly the git-data class for ASCII
// names. The never-synced names are listed first, in any letter case, so a
// .scion, .DS_Store, .git or objects/info element inside refs or objects
// is dropped as ClassifyWorkspaceSyncPath drops it. The allowlist itself
// is matched exactly.
var WorkspaceSyncGitDataRcloneRules = []string{
	"- " + ciGlob(dotScion),
	"- " + ciGlob(dotScion) + "/**",
	"- " + ciGlob(dsStore),
	"- " + ciGlob(dsStore) + "/**",
	"- /" + dotGit + "/objects/" + ciGlob("info") + "/**",
	"- /" + dotGit + "/objects/" + ciGlob("info"),
	"- /" + dotGit + "/**/" + ciGlob(dotGit),
	"- /" + dotGit + "/**/" + ciGlob(dotGit) + "/**",
	"+ /" + dotGit + "/HEAD",
	"+ /" + dotGit + "/packed-refs",
	"+ /" + dotGit + "/index",
	"+ /" + dotGit + "/refs/**",
	"+ /" + dotGit + "/objects/**",
	"- **",
}
