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

// cleanupPartialWorktree removes only the failed agent's own worktree and
// its git entry, never every entry whose directory is missing: another
// worktree that this process cannot see keeps its registration
// (ptone/scion#5071).

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func gitT(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return string(out)
}

// partialWorktreeBase creates a shared base checkout with a worktree for
// agent-1 at base/worktrees/agent-1 and a sibling worktree outside the base
// whose parent is moved away (missing from this process's view). It
// returns the base, agent-1's worktree, the sibling and a function that
// puts the sibling back.
func partialWorktreeBase(t *testing.T) (base, own, sibling string, restore func()) {
	t.Helper()
	t.Setenv("SCION_HOST_UID", "")
	base = filepath.Join(t.TempDir(), "workspace")
	if err := os.MkdirAll(base, 0o755); err != nil {
		t.Fatal(err)
	}
	gitT(t, base, "init", "-q", "-b", "main")
	gitT(t, base, "-c", "user.email=t@example.com", "-c", "user.name=t", "commit", "-q", "--allow-empty", "-m", "init")
	own = filepath.Join(base, "worktrees", "agent-1")
	if err := os.MkdirAll(filepath.Dir(own), 0o755); err != nil {
		t.Fatal(err)
	}
	gitT(t, base, "worktree", "add", "-q", "--relative-paths", "-b", "agent-1", own)

	other := t.TempDir()
	volume := filepath.Join(other, "volume")
	sibling = filepath.Join(volume, "sibling")
	if err := os.MkdirAll(volume, 0o755); err != nil {
		t.Fatal(err)
	}
	gitT(t, base, "worktree", "add", "-q", "-b", "sibling", sibling)
	hidden := filepath.Join(other, "volume-hidden")
	if err := os.Rename(volume, hidden); err != nil {
		t.Fatal(err)
	}
	restored := false
	restore = func() {
		if restored {
			return
		}
		restored = true
		if err := os.Rename(hidden, volume); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(restore)
	return base, own, sibling, restore
}

// captureLog sends the default slog output to a buffer for the rest of the
// test.
func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	orig := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(orig) })
	return &buf
}

func requireSiblingEntryIntact(t *testing.T, base, sibling string, restore func()) {
	t.Helper()
	restore()
	if list := gitT(t, base, "worktree", "list", "--porcelain"); !strings.Contains(list, "branch refs/heads/sibling\n") {
		t.Errorf("sibling worktree entry was removed; worktree list:\n%s", list)
	}
	if out, err := exec.Command("git", "-C", sibling, "status", "--short").CombinedOutput(); err != nil {
		t.Errorf("git status in sibling worktree failed: %v\n%s", err, out)
	}
}

func ownAdminDir(base string) string {
	return filepath.Join(base, ".git", "worktrees", "agent-1")
}

// The normal path: git worktree remove --force removes the partial worktree
// and its entry, and nothing else.
func TestCleanupPartialWorktree_RemovesOwnWorktreeOnly(t *testing.T) {
	base, own, sibling, restore := partialWorktreeBase(t)

	cleanupPartialWorktree(context.Background(), base, own, "agent-1")

	if _, err := os.Lstat(own); !os.IsNotExist(err) {
		t.Errorf("expected the partial worktree to be removed (lstat err=%v)", err)
	}
	if _, err := os.Lstat(ownAdminDir(base)); !os.IsNotExist(err) {
		t.Errorf("expected the partial worktree's entry to be removed (lstat err=%v)", err)
	}
	requireSiblingEntryIntact(t, base, sibling, restore)
}

// When git worktree remove --force fails (here: the entry is locked), the
// directory is removed and only its own entry is targeted; the targeted
// removal fails too (the entry is still locked) and is logged. There is no
// repository-wide prune.
func TestCleanupPartialWorktree_RemoveFails_NoPrune(t *testing.T) {
	base, own, sibling, restore := partialWorktreeBase(t)
	gitT(t, base, "worktree", "lock", own)
	logs := captureLog(t)

	cleanupPartialWorktree(context.Background(), base, own, "agent-1")

	if _, err := os.Lstat(own); !os.IsNotExist(err) {
		t.Errorf("expected the partial worktree's directory to be removed (lstat err=%v)", err)
	}
	if !strings.Contains(logs.String(), "could not remove the partial worktree's git entry") {
		t.Errorf("expected the failed entry removal to be logged; log:\n%s", logs.String())
	}
	requireSiblingEntryIntact(t, base, sibling, restore)
}

// The fallback runs git through SafeGitCommand like the rest of the
// function: with a commondir file in the base's .git, both the remove and
// the entry removal are refused, and nothing else is touched.
func TestCleanupPartialWorktree_UsesSafeGitCommand(t *testing.T) {
	base, own, sibling, restore := partialWorktreeBase(t)
	commondir := filepath.Join(base, ".git", "commondir")
	if err := os.WriteFile(commondir, []byte(".\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(commondir) })
	logs := captureLog(t)

	cleanupPartialWorktree(context.Background(), base, own, "agent-1")

	if err := os.Remove(commondir); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(logs.String(), "could not remove the partial worktree's git entry") {
		t.Errorf("expected the refused entry removal to be logged; log:\n%s", logs.String())
	}
	if _, err := os.Lstat(ownAdminDir(base)); err != nil {
		t.Errorf("expected the entry to be kept when git is refused (lstat err=%v)", err)
	}
	requireSiblingEntryIntact(t, base, sibling, restore)
}
