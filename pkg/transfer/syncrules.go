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

import "strings"

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
// subtree is git data (except objects/info, see objectsInfo).
var gitDataTrees = map[string]bool{"refs": true, "objects": true}

const (
	dotGit      = ".git"
	dotScion    = ".scion"
	dsStore     = ".DS_Store"
	objectsInfo = "objects/info"
)

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
	for _, p := range parts {
		switch p {
		case "", ".", "..":
			return WorkspaceSyncNever
		case dotScion, dsStore:
			return WorkspaceSyncNever
		}
	}
	for i, p := range parts {
		if p == dotGit && i > 0 {
			// A .git below the root (submodule gitdir file, nested
			// repository), including one inside the root .git.
			return WorkspaceSyncNever
		}
	}
	if parts[0] != dotGit {
		return WorkspaceSyncContent
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
	inner := strings.Join(parts[1:], "/")
	if inner == objectsInfo || strings.HasPrefix(inner, objectsInfo+"/") {
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

// WorkspaceSyncContentRcloneRules are rclone filter rules (FilterRule
// syntax, first match wins) that pass exactly the content class. An
// unanchored pattern matches the name at any depth, so they drop every
// .git, .scion and .DS_Store entry wherever it sits, file or directory.
var WorkspaceSyncContentRcloneRules = []string{
	"- " + dotGit,
	"- " + dotGit + "/**",
	"- " + dotScion,
	"- " + dotScion + "/**",
	"- " + dsStore,
	"- " + dsStore + "/**",
	"+ **",
}

// WorkspaceSyncGitDataRcloneRules are rclone filter rules (FilterRule
// syntax, first match wins) that pass exactly the git-data class. The
// never-synced names are listed first so a .scion or .DS_Store element
// inside refs or objects is dropped as ClassifyWorkspaceSyncPath drops it.
var WorkspaceSyncGitDataRcloneRules = []string{
	"- " + dotScion,
	"- " + dotScion + "/**",
	"- " + dsStore,
	"- " + dsStore + "/**",
	"- /" + dotGit + "/" + objectsInfo + "/**",
	"- /" + dotGit + "/" + objectsInfo,
	"- /" + dotGit + "/**/" + dotGit,
	"- /" + dotGit + "/**/" + dotGit + "/**",
	"+ /" + dotGit + "/HEAD",
	"+ /" + dotGit + "/packed-refs",
	"+ /" + dotGit + "/index",
	"+ /" + dotGit + "/refs/**",
	"+ /" + dotGit + "/objects/**",
	"- **",
}
