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
	"os"
	"path/filepath"
	"strings"
)

// TrustedWorktreeAdminName returns the name of the .git/worktrees/<name> admin
// directory that belongs to the agent's workspace, derived ONLY from
// broker-trusted data in the common git dir, never from the agent-writable
// <workspace>/.git gitfile. It enumerates the common dir's worktrees/<name>
// entries and returns the single one whose own gitdir back-reference resolves to
// <workspace>/.git. A name must be a single path element (not "." or ".."). If
// there is no unique match it returns ok=false, and the caller must refuse to
// bind any read-write worktree overlay (fail closed).
//
// This is the control for the read-write own-worktree overlay: the agent must
// not be able to choose, via its own gitfile, which admin dir it gets mounted
// read-write (which could reach a sibling's admin dir, the whole worktrees/
// directory, or — once cleaned — the .git root itself).
func TrustedWorktreeAdminName(gitDir, workspace string) (string, bool) {
	wantGit := filepath.Join(workspace, ".git")
	// The workspace .git gitfile is agent-writable. A worktree's .git is always a
	// regular file (the gitfile); refuse if it is a symlink (or a directory, or
	// missing), so the agent cannot repoint it at a sibling's gitfile and have the
	// match resolve through it. Fail closed.
	if fi, err := os.Lstat(wantGit); err != nil || !fi.Mode().IsRegular() {
		return "", false
	}
	// Canonicalise by resolving the directory only and naming ".git" lexically, so
	// a symlink the agent controls at the final component is never followed.
	want := canonicalGitfilePath(wantGit)
	worktreesDir := filepath.Join(gitDir, "worktrees")
	entries, err := os.ReadDir(worktreesDir)
	if err != nil {
		return "", false
	}
	match := ""
	for _, e := range entries {
		name := e.Name()
		// A single, non-traversing path element only.
		if name == "" || name == "." || name == ".." || strings.ContainsAny(name, "/\\") {
			continue
		}
		adminDir := filepath.Join(worktreesDir, name)
		if !e.IsDir() {
			continue
		}
		gitdirFile := filepath.Join(adminDir, "gitdir")
		ptr, ok := readSmallTextFile(gitdirFile)
		if !ok {
			continue
		}
		ptr = strings.TrimSpace(ptr)
		if ptr == "" {
			continue
		}
		// git worktree add --relative-paths writes a path relative to the admin
		// dir; an absolute path is also valid. Resolve both the same way.
		resolved := ptr
		if !filepath.IsAbs(resolved) {
			resolved = filepath.Join(adminDir, resolved)
		}
		if canonicalGitfilePath(resolved) != want {
			continue
		}
		if match != "" {
			// Two admin dirs claim this workspace — ambiguous; fail closed.
			return "", false
		}
		match = name
	}
	if match == "" {
		return "", false
	}
	return match, true
}

// canonicalGitfilePath returns a comparable absolute path for a worktree .git
// gitfile. It resolves symlinks in the parent directory (broker-controlled path)
// but names the final component lexically, so a symlink at the gitfile itself —
// which the agent can write at <workspace>/.git — is never followed. If the
// parent cannot be resolved (it may not exist yet) it falls back to the
// absolute, cleaned form.
func canonicalGitfilePath(path string) string {
	abs := path
	if a, err := filepath.Abs(path); err == nil {
		abs = a
	}
	abs = filepath.Clean(abs)
	dir, base := filepath.Split(abs)
	if r, err := filepath.EvalSymlinks(filepath.Clean(dir)); err == nil {
		return filepath.Join(r, base)
	}
	return abs
}

// readSmallTextFile reads a small file, refusing symlinks and non-regular
// files (a FIFO/device/symlink in a shared worktrees dir must not be
// followed or block). It returns ok=false on any such refusal or error.
func readSmallTextFile(path string) (string, bool) {
	fi, err := os.Lstat(path)
	if err != nil || fi.Mode()&os.ModeSymlink != 0 || !fi.Mode().IsRegular() {
		return "", false
	}
	const max = 4096
	if fi.Size() > max {
		return "", false
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	return string(data), true
}
