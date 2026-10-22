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
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// This file is the single exclude list for workspace sync: the manifest a
// broker builds, every rclone transfer of a workspace to or from the bucket
// (pkg/gcp), the confined apply of a downloaded workspace, and the CLI's
// sync-from download all classify paths with ClassifyWorkspaceSyncPath or
// with the rclone rules below. The classifier and the rules are built from
// the same reserved-name patterns, and a parity test checks them against
// each other.
//
// Three classes:
//   - content: ordinary workspace files, synced in every direction;
//   - git data: the repository's history in the workspace-root .git
//     directory. It is carried only by the legs that ship a whole
//     repository between hosts (workspace upload, hub create-time upload,
//     broker create-time download, hub sync-back), and never written into a
//     user's checkout by the CLI. It has two parts: the ref state (HEAD,
//     packed-refs, index, shallow, refs/**), synced as one unit so a packed or
//     deleted ref does not come back, and the objects (objects/**), only
//     ever copied;
//   - never synced: everything else under any .git (config, hooks, info,
//     modules, worktrees, logs, objects/info such as alternates, and every
//     .git below the root, such as a submodule's gitdir file), every .scion
//     entry at any depth (the project's own configuration, wherever the
//     project sits in the repository), and .DS_Store.
//
// Reserved names (.git, .scion, .DS_Store, and info under objects) are
// matched the way git matches .git when it verifies a path: without regard
// to case, skipping the invisible characters some filesystems ignore,
// allowing the trailing dots, spaces and ":stream" suffix NTFS ignores, and
// including the 8.3 short names NTFS can give them. A receiver whose
// filesystem treats such a variant as the real entry therefore never gets
// it. The git data allowlist itself is matched exactly: a variant of an
// allowed name is never synced.

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

// gitStateFiles are the files directly under the root .git that, with
// refs/**, make up the ref state. shallow lists the commits a shallow
// clone's history stops at.
var gitStateFiles = map[string]bool{"HEAD": true, "packed-refs": true, "index": true, "shallow": true}

const (
	dotGit   = ".git"
	dotScion = ".scion"
	dsStore  = ".DS_Store"
	refsDir  = "refs"
	objsDir  = "objects"
	infoDir  = "info"
)

// Building blocks of the reserved-name patterns (Go regexp syntax, also
// valid inside an rclone "{{...}}" glob section, which they never close:
// no "}}" appears in them).
const (
	// ignorable matches invisible format characters (Unicode Cf, which
	// holds the code points HFS+ ignores in names, such as U+200C).
	ignorable = `\p{Cf}*`
	// ntfsTail matches what NTFS ignores after a name: trailing dots and
	// spaces, and an alternate data stream suffix.
	ntfsTail = `[. \p{Cf}]*(?::[^/]*)?`
)

// spelled returns a pattern for name with ignorable characters allowed
// before, between and after its letters.
func spelled(name string) string {
	var b strings.Builder
	b.WriteString(ignorable)
	for _, r := range name {
		b.WriteString(regexp.QuoteMeta(string(r)))
		b.WriteString(ignorable)
	}
	return b.String()
}

// reservedPattern returns the pattern of one path element that a
// case-insensitive or NTFS filesystem may resolve to the reserved name, with
// its 8.3 short-name forms.
func reservedPattern(name string, shortNames ...string) string {
	alts := append([]string{spelled(name)}, shortNames...)
	return `(?i:` + strings.Join(alts, "|") + `)` + ntfsTail
}

var (
	gitPattern   = reservedPattern(dotGit, `git~[1-9]`)
	scionPattern = reservedPattern(dotScion, `scion~[1-9]`, `sc[0-9a-f][0-9a-f][0-9a-f][0-9a-f]~[1-9]`)
	dsPattern    = reservedPattern(dsStore, `ds_sto~[1-9]`)
	infoPattern  = reservedPattern(infoDir)

	gitElem   = regexp.MustCompile(`^(?:` + gitPattern + `)$`)
	scionElem = regexp.MustCompile(`^(?:` + scionPattern + `)$`)
	dsElem    = regexp.MustCompile(`^(?:` + dsPattern + `)$`)
	infoElem  = regexp.MustCompile(`^(?:` + infoPattern + `)$`)
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
	for i, p := range parts {
		switch {
		case p == "" || p == "." || p == "..":
			return WorkspaceSyncNever
		case scionElem.MatchString(p) || dsElem.MatchString(p):
			return WorkspaceSyncNever
		case i > 0 && gitElem.MatchString(p):
			// A .git below the root (submodule gitdir file, nested
			// repository), including one inside the root .git.
			return WorkspaceSyncNever
		}
	}
	if !gitElem.MatchString(parts[0]) {
		return WorkspaceSyncContent
	}
	if parts[0] != dotGit {
		// A variant of the root .git.
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
	if len(parts) > 2 && parts[1] == objsDir && infoElem.MatchString(parts[2]) {
		return WorkspaceSyncNever
	}
	if len(parts) == 2 {
		switch {
		case gitStateFiles[parts[1]] && !isDir:
			return WorkspaceSyncGitData
		case (parts[1] == refsDir || parts[1] == objsDir) && isDir:
			return WorkspaceSyncGitData
		}
		return WorkspaceSyncNever
	}
	if parts[1] == refsDir || parts[1] == objsDir {
		return WorkspaceSyncGitData
	}
	return WorkspaceSyncNever
}

// IsWorkspaceSyncContent reports whether rel is ordinary workspace content.
func IsWorkspaceSyncContent(rel string, isDir bool) bool {
	return ClassifyWorkspaceSyncPath(rel, isDir) == WorkspaceSyncContent
}

// IsWorkspaceGitObject reports whether rel is git data in the objects part
// (copied, never deleted); other git data is ref state (synced as a unit).
func IsWorkspaceGitObject(rel string, isDir bool) bool {
	return ClassifyWorkspaceSyncPath(rel, isDir) == WorkspaceSyncGitData &&
		strings.HasPrefix(rel, dotGit+"/"+objsDir+"/")
}

// IsWorkspaceGitState reports whether rel is git data in the ref state
// (HEAD, packed-refs, index, shallow, refs/**).
func IsWorkspaceGitState(rel string, isDir bool) bool {
	return ClassifyWorkspaceSyncPath(rel, isDir) == WorkspaceSyncGitData &&
		!IsWorkspaceGitObject(rel, isDir) && rel != dotGit && rel != dotGit+"/"+objsDir
}

// rcloneElem returns an rclone glob element for a reserved-name pattern.
func rcloneElem(pattern string) string { return "{{" + pattern + "}}" }

// neverRules are the rclone rules (FilterRule syntax) that drop the
// reserved names at any depth, file or directory.
func neverRules(patterns ...string) []string {
	var out []string
	for _, p := range patterns {
		out = append(out, "- "+rcloneElem(p), "- "+rcloneElem(p)+"/**")
	}
	return out
}

// WorkspaceSyncContentRcloneRules are rclone filter rules (FilterRule
// syntax, first match wins) that pass exactly the content class: every
// .git, .scion and .DS_Store entry, in any of the variants the classifier
// matches, is dropped at any depth.
var WorkspaceSyncContentRcloneRules = append(neverRules(gitPattern, scionPattern, dsPattern), "+ **")

// WorkspaceSyncGitStateRcloneRules are rclone filter rules that pass
// exactly the ref state: HEAD, packed-refs, index and shallow in the root .git, and
// refs/**, without any reserved name inside.
var WorkspaceSyncGitStateRcloneRules = append(append(neverRules(scionPattern, dsPattern),
	"- /"+dotGit+"/**/"+rcloneElem(gitPattern),
	"- /"+dotGit+"/**/"+rcloneElem(gitPattern)+"/**"),
	"+ /"+dotGit+"/HEAD",
	"+ /"+dotGit+"/packed-refs",
	"+ /"+dotGit+"/index",
	"+ /"+dotGit+"/shallow",
	"+ /"+dotGit+"/"+refsDir+"/**",
	"- **",
)

// WorkspaceSyncGitObjectRcloneRules are rclone filter rules that pass
// exactly the objects part of git data: objects/** in the root .git,
// without objects/info (alternates) or any reserved name inside.
var WorkspaceSyncGitObjectRcloneRules = append(append(neverRules(scionPattern, dsPattern),
	"- /"+dotGit+"/"+objsDir+"/"+rcloneElem(infoPattern),
	"- /"+dotGit+"/"+objsDir+"/"+rcloneElem(infoPattern)+"/**",
	"- /"+dotGit+"/**/"+rcloneElem(gitPattern),
	"- /"+dotGit+"/**/"+rcloneElem(gitPattern)+"/**"),
	"+ /"+dotGit+"/"+objsDir+"/**",
	"- **",
)

// WorkspaceGitDataUnsupported returns why the repository in the workspace
// at workspace cannot travel as git data, or "" when it can (or when the
// workspace has no .git directory). A repository whose refs are in reftable
// format (.git/reftable), a partial clone (promisor packs), or one with a
// split index (.git/sharedindex.*) depends on files or configuration that
// workspace sync never carries, so a receiver would get a broken
// repository; its git data is skipped instead. Only names are inspected,
// never followed through symlinks.
func WorkspaceGitDataUnsupported(workspace string) string {
	gitDir := filepath.Join(workspace, dotGit)
	info, err := os.Lstat(gitDir)
	if err != nil || !info.IsDir() {
		return ""
	}
	if _, err := os.Lstat(filepath.Join(gitDir, "reftable")); err == nil {
		return "the repository stores its refs in reftable format"
	}
	if m, _ := filepath.Glob(filepath.Join(gitDir, objsDir, "pack", "*.promisor")); len(m) > 0 {
		return "the repository is a partial clone (promisor packs)"
	}
	if m, _ := filepath.Glob(filepath.Join(gitDir, "sharedindex.*")); len(m) > 0 {
		return "the repository uses a split index"
	}
	return ""
}
