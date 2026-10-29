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

// These tests create symlinks.
//go:build unix

package util

import (
	"os"
	"path/filepath"
	"testing"
)

// worktreeAdminFixture makes <root>/repo/.git/worktrees/<name>/gitdir pointing
// at <root>/<ws>/.git (relative when rel is set) and a regular <ws>/.git gitfile.
func worktreeAdminFixture(t *testing.T, root, name, ws string, rel bool) (gitDir, workspace string) {
	t.Helper()
	gitDir = filepath.Join(root, "repo", ".git")
	admin := filepath.Join(gitDir, "worktrees", name)
	workspace = filepath.Join(root, ws)
	if err := os.MkdirAll(admin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, ".git"), []byte("gitdir: "+admin+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ptr := filepath.Join(workspace, ".git")
	if rel {
		r, err := filepath.Rel(admin, ptr)
		if err != nil {
			t.Fatal(err)
		}
		ptr = r
	}
	if err := os.WriteFile(filepath.Join(admin, "gitdir"), []byte(ptr+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return gitDir, workspace
}

func TestTrustedWorktreeAdminName(t *testing.T) {
	t.Run("absolute back-link", func(t *testing.T) {
		gitDir, ws := worktreeAdminFixture(t, t.TempDir(), "wt-a", "ws", false)
		if name, ok := TrustedWorktreeAdminName(gitDir, ws); !ok || name != "wt-a" {
			t.Errorf("got %q, %v; want wt-a, true", name, ok)
		}
	})
	t.Run("relative back-link", func(t *testing.T) {
		gitDir, ws := worktreeAdminFixture(t, t.TempDir(), "wt-b", "ws", true)
		if name, ok := TrustedWorktreeAdminName(gitDir, ws); !ok || name != "wt-b" {
			t.Errorf("got %q, %v; want wt-b, true", name, ok)
		}
	})
	t.Run("two admin dirs claim the workspace", func(t *testing.T) {
		root := t.TempDir()
		gitDir, ws := worktreeAdminFixture(t, root, "wt-c", "ws", false)
		dup := filepath.Join(gitDir, "worktrees", "wt-d")
		if err := os.MkdirAll(dup, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dup, "gitdir"), []byte(filepath.Join(ws, ".git")+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if name, ok := TrustedWorktreeAdminName(gitDir, ws); ok || name != "" {
			t.Errorf("ambiguous match must fail closed, got %q, %v", name, ok)
		}
	})
	t.Run("symlinked gitfile", func(t *testing.T) {
		root := t.TempDir()
		gitDir, ws := worktreeAdminFixture(t, root, "wt-e", "ws", false)
		real := filepath.Join(root, "real-gitfile")
		if err := os.Rename(filepath.Join(ws, ".git"), real); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(real, filepath.Join(ws, ".git")); err != nil {
			t.Fatal(err)
		}
		if name, ok := TrustedWorktreeAdminName(gitDir, ws); ok || name != "" {
			t.Errorf("a symlinked <workspace>/.git must fail closed, got %q, %v", name, ok)
		}
	})
	t.Run("no worktrees dir", func(t *testing.T) {
		root := t.TempDir()
		ws := filepath.Join(root, "ws")
		if err := os.MkdirAll(ws, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(ws, ".git"), []byte("gitdir: x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if name, ok := TrustedWorktreeAdminName(filepath.Join(root, "repo", ".git"), ws); ok || name != "" {
			t.Errorf("got %q, %v; want no match", name, ok)
		}
	})
}
