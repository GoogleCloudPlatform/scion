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
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// repoState is what a test compares to show a repository was not changed.
func repoState(t *testing.T, repo string) string {
	t.Helper()
	refs, err := exec.Command("git", "-C", repo, "for-each-ref", "--format=%(refname) %(objectname)").Output()
	if err != nil {
		t.Fatalf("git for-each-ref in %s: %v", repo, err)
	}
	return string(refs) + "\n--\n" + worktreeEntries(t, repo)
}

func gitIn(t *testing.T, repo string, args ...string) {
	t.Helper()
	if out, err := exec.Command("git", append([]string{"-C", repo}, args...)...).CombinedOutput(); err != nil {
		t.Fatalf("git %v in %s: %v\n%s", args, repo, err, out)
	}
}

// worktreeFixture creates repository A with a worktree on branch "agent"
// under A/worktrees/agent, and an unrelated repository B with its own
// worktree and a branch also named "agent".
func worktreeFixture(t *testing.T) (repoA, wt, repoB, wtB string) {
	t.Helper()
	t.Setenv("SCION_HOST_UID", "")
	repoA = setupGitRepo(t)
	wt = filepath.Join(repoA, "worktrees", "agent")
	if err := os.MkdirAll(filepath.Dir(wt), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := CreateWorktree(wt, "agent"); err != nil {
		t.Fatalf("CreateWorktree: %v", err)
	}
	repoB = setupGitRepo(t)
	wtB = filepath.Join(repoB, "worktrees", "other")
	if err := os.MkdirAll(filepath.Dir(wtB), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := CreateWorktree(wtB, "other"); err != nil {
		t.Fatalf("CreateWorktree B: %v", err)
	}
	gitIn(t, repoB, "branch", "agent")
	gitIn(t, repoB, "branch", "unrelated")
	return repoA, wt, repoB, wtB
}

// adminDirOf returns the worktree's admin directory as its .git file names it.
func adminDirOf(t *testing.T, wt string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(wt, ".git"))
	if err != nil {
		t.Fatal(err)
	}
	gitdir := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(string(data)), "gitdir:"))
	if !filepath.IsAbs(gitdir) {
		gitdir = filepath.Join(wt, gitdir)
	}
	return filepath.Clean(gitdir)
}

// The repository changed and the branch deleted come from the caller. A
// worktree whose .git file or commondir names another repository changes
// nothing in that repository, and its own branch is not deleted (the
// metadata no longer shows it on the expected branch of the caller's
// repository).
func TestRemoveWorktree_EditedMetadataChangesOnlyCallerRepo(t *testing.T) {
	cases := []struct {
		name string
		edit func(t *testing.T, wt, repoB, wtB string)
	}{
		{"pointer names another repository's worktree", func(t *testing.T, wt, repoB, wtB string) {
			mustWriteFile(t, filepath.Join(wt, ".git"), "gitdir: "+adminDirOf(t, wtB)+"\n")
		}},
		{"pointer names another repository", func(t *testing.T, wt, repoB, wtB string) {
			mustWriteFile(t, filepath.Join(wt, ".git"), "gitdir: "+filepath.Join(repoB, ".git")+"\n")
		}},
		{"commondir names another repository", func(t *testing.T, wt, repoB, wtB string) {
			mustWriteFile(t, filepath.Join(adminDirOf(t, wt), "commondir"), filepath.Join(repoB, ".git")+"\n")
		}},
		{"pointer names a made-up admin directory on an unrelated branch of another repository", func(t *testing.T, wt, repoB, wtB string) {
			// Git in the worktree would report repository B and branch
			// "unrelated", which is not checked out anywhere, so a
			// derived "branch -D unrelated" in B would succeed.
			fake := filepath.Join(t.TempDir(), "admin")
			if err := os.MkdirAll(fake, 0o755); err != nil {
				t.Fatal(err)
			}
			mustWriteFile(t, filepath.Join(fake, "HEAD"), "ref: refs/heads/unrelated\n")
			mustWriteFile(t, filepath.Join(fake, "commondir"), filepath.Join(repoB, ".git")+"\n")
			mustWriteFile(t, filepath.Join(fake, "gitdir"), filepath.Join(wt, ".git")+"\n")
			mustWriteFile(t, filepath.Join(wt, ".git"), "gitdir: "+fake+"\n")
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repoA, wt, repoB, wtB := worktreeFixture(t)
			tc.edit(t, wt, repoB, wtB)
			before := repoState(t, repoB)

			deleted, err := RemoveWorktree(repoA, wt, repoA, "agent")
			if err != nil {
				t.Fatalf("RemoveWorktree: %v", err)
			}
			if deleted {
				t.Error("branch reported deleted for a worktree whose metadata names another repository")
			}
			if after := repoState(t, repoB); after != before {
				t.Errorf("the other repository changed:\nbefore:\n%s\nafter:\n%s", before, after)
			}
			if !BranchExistsIn(repoB, "unrelated") {
				t.Error("the other repository's branch \"unrelated\" was deleted")
			}
			if _, err := os.Stat(wt); !os.IsNotExist(err) {
				t.Errorf("worktree directory still present: %v", err)
			}
			if strings.Contains(worktreeEntries(t, repoA), "refs/heads/agent") {
				t.Errorf("caller repository still lists the worktree:\n%s", worktreeEntries(t, repoA))
			}
		})
	}
}

// An inherited GIT_DIR does not redirect the commands to another repository.
func TestRemoveWorktree_InheritedGitDirIgnored(t *testing.T) {
	repoA, wt, repoB, _ := worktreeFixture(t)
	before := repoState(t, repoB)
	t.Setenv("GIT_DIR", filepath.Join(repoB, ".git"))
	t.Setenv("GIT_WORK_TREE", repoB)

	deleted, err := RemoveWorktree(repoA, wt, repoA, "agent")
	if err != nil {
		t.Fatalf("RemoveWorktree: %v", err)
	}
	if !deleted {
		t.Error("expected the caller's branch to be deleted")
	}
	// Read the repositories back without the inherited values (an empty
	// GIT_DIR is not the same as an unset one); t.Setenv above restores the
	// original environment at cleanup.
	_ = os.Unsetenv("GIT_DIR")
	_ = os.Unsetenv("GIT_WORK_TREE")
	if after := repoState(t, repoB); after != before {
		t.Errorf("the other repository changed:\nbefore:\n%s\nafter:\n%s", before, after)
	}
	if BranchExistsIn(repoA, "agent") {
		t.Error("caller repository's branch still exists")
	}
}

// A hook file in the repository does not run when the branch is deleted
// (core.hooksPath points at an empty location), and the legitimate case
// deletes the branch and the worktree entry.
func TestRemoveWorktree_RunsNoRepositoryHookFiles(t *testing.T) {
	repoA, wt, _, _ := worktreeFixture(t)
	sentinel := filepath.Join(t.TempDir(), "hook-ran")
	hooks := filepath.Join(repoA, ".git", "hooks")
	if err := os.MkdirAll(hooks, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, h := range []string{"reference-transaction", "post-checkout"} {
		if err := os.WriteFile(filepath.Join(hooks, h), []byte("#!/bin/sh\ntouch '"+sentinel+"'\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	deleted, err := RemoveWorktree(repoA, wt, repoA, "agent")
	if err != nil {
		t.Fatalf("RemoveWorktree: %v", err)
	}
	if !deleted || BranchExistsIn(repoA, "agent") {
		t.Errorf("expected the branch to be deleted (deleted=%v)", deleted)
	}
	if strings.Contains(worktreeEntries(t, repoA), "worktrees/agent") {
		t.Errorf("worktree entry still listed:\n%s", worktreeEntries(t, repoA))
	}
	if _, err := os.Stat(sentinel); err == nil {
		t.Error("a repository hook file ran during RemoveWorktree")
	}

	// The hook is live: plain git runs it, so the check above is not vacuous.
	gitIn(t, repoA, "branch", "hook-check")
	if _, err := os.Stat(sentinel); err != nil {
		t.Fatalf("fixture hook did not run under plain git: %v", err)
	}
}

// A worktree that is checked out on a different branch than the caller
// expects keeps both branches.
func TestRemoveWorktree_BranchMismatchDeletesNothing(t *testing.T) {
	repoA, wt, _, _ := worktreeFixture(t)
	gitIn(t, wt, "checkout", "-q", "-b", "switched")

	deleted, err := RemoveWorktree(repoA, wt, repoA, "agent")
	if err != nil {
		t.Fatalf("RemoveWorktree: %v", err)
	}
	if deleted {
		t.Error("branch reported deleted although the worktree was on another branch")
	}
	for _, b := range []string{"agent", "switched"} {
		if !BranchExistsIn(repoA, b) {
			t.Errorf("branch %q was deleted", b)
		}
	}
}

// With no repository root, the worktree directory is removed and nothing
// else: no entry removal, no branch deletion, and no prune of the
// repository the process runs in.
func TestRemoveWorktree_EmptyRepoRootSkipsGit(t *testing.T) {
	repoA, wt, _, _ := worktreeFixture(t)
	oldWd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldWd) })
	if err := os.Chdir(repoA); err != nil {
		t.Fatal(err)
	}

	deleted, err := RemoveWorktree(repoA, wt, "", "agent")
	if err != nil {
		t.Fatalf("RemoveWorktree: %v", err)
	}
	if deleted || !BranchExistsIn(repoA, "agent") {
		t.Errorf("branch deleted with no repository root (deleted=%v)", deleted)
	}
	if _, err := os.Stat(wt); !os.IsNotExist(err) {
		t.Errorf("worktree directory still present: %v", err)
	}
	if !strings.Contains(worktreeEntries(t, repoA), "refs/heads/agent") {
		t.Errorf("worktree entry was removed with no repository root:\n%s", worktreeEntries(t, repoA))
	}
}

// A repository root that is not a main repository is not used for git.
func TestRemoveWorktree_RepoRootNotMainRepository(t *testing.T) {
	repoA, wt, _, _ := worktreeFixture(t)
	// The worktree itself has a .git file, not a directory.
	if _, err := RemoveWorktree(repoA, wt, wt, "agent"); err != nil {
		t.Fatalf("RemoveWorktree: %v", err)
	}
	if !BranchExistsIn(repoA, "agent") {
		t.Error("branch deleted through a repository root that is not a main repository")
	}
}

// Branch names starting with "-" are refused, so they are never read as
// options; "--" keeps any other name an operand.
func TestDeleteBranchIn_RefusesOptionLikeNames(t *testing.T) {
	repo := setupGitRepo(t)
	gitIn(t, repo, "branch", "keep-a")
	gitIn(t, repo, "branch", "keep-b")
	before := repoState(t, repo)
	for _, name := range []string{"-D", "--all", "-r", "--delete", "-", ""} {
		if DeleteBranchIn(repo, name) {
			t.Errorf("DeleteBranchIn(%q) = true", name)
		}
	}
	if after := repoState(t, repo); after != before {
		t.Errorf("repository changed:\nbefore:\n%s\nafter:\n%s", before, after)
	}
	if !DeleteBranchIn(repo, "keep-a") || BranchExistsIn(repo, "keep-a") {
		t.Error("DeleteBranchIn did not delete a normal branch")
	}
}

func mustWriteFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// The admin files are read only as small regular files: a symlinked or
// oversized .git file, or a FIFO HEAD, keeps the branch, and the directory
// and the worktree entry are still removed. The FIFO case must not block.
func TestRemoveWorktree_AdminFilesMustBeSmallRegularFiles(t *testing.T) {
	orig := baseRepoGitTimeout
	baseRepoGitTimeout = 5 * time.Second
	t.Cleanup(func() { baseRepoGitTimeout = orig })

	cases := []struct {
		name string
		edit func(t *testing.T, wt string)
		// entryRemoved: git itself can remove the entry. With a FIFO HEAD
		// in the admin directory git blocks reading it, so the entry
		// removal ends at baseRepoGitTimeout and the entry stays.
		entryRemoved bool
	}{
		{"symlinked .git file", func(t *testing.T, wt string) {
			target := filepath.Join(t.TempDir(), "pointer")
			mustWriteFile(t, target, "gitdir: "+adminDirOf(t, wt)+"\n")
			if err := os.Remove(filepath.Join(wt, ".git")); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, filepath.Join(wt, ".git")); err != nil {
				t.Fatal(err)
			}
		}, true},
		{"oversized .git file", func(t *testing.T, wt string) {
			line := "gitdir: " + adminDirOf(t, wt) + "\n"
			mustWriteFile(t, filepath.Join(wt, ".git"), line+strings.Repeat(" ", smallGitFileMax))
		}, true},
		{"FIFO HEAD", func(t *testing.T, wt string) {
			head := filepath.Join(adminDirOf(t, wt), "HEAD")
			if err := os.Remove(head); err != nil {
				t.Fatal(err)
			}
			if err := exec.Command("mkfifo", head).Run(); err != nil {
				t.Skipf("mkfifo unavailable: %v", err)
			}
		}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repoA, wt, _, _ := worktreeFixture(t)
			admin := adminDirOf(t, wt)
			tc.edit(t, wt)
			deleted, err := RemoveWorktree(repoA, wt, repoA, "agent")
			if err != nil {
				t.Fatalf("RemoveWorktree: %v", err)
			}
			if !tc.entryRemoved {
				// Put a regular HEAD back so the git checks below can run.
				_ = os.Remove(filepath.Join(admin, "HEAD"))
				mustWriteFile(t, filepath.Join(admin, "HEAD"), "ref: refs/heads/agent\n")
			}
			if deleted || !BranchExistsIn(repoA, "agent") {
				t.Errorf("branch deleted (deleted=%v)", deleted)
			}
			if _, err := os.Stat(wt); !os.IsNotExist(err) {
				t.Errorf("worktree directory still present: %v", err)
			}
			if tc.entryRemoved && strings.Contains(worktreeEntries(t, repoA), "worktrees/agent") {
				t.Errorf("worktree entry still listed:\n%s", worktreeEntries(t, repoA))
			}
		})
	}
}

// Known limit, kept as a test so the follow-up that makes the repository
// configuration read-only flips it: a hook defined in the repository's
// configuration (hook.<name>.command and .event) is not covered by
// core.hooksPath and still runs when the branch is deleted.
func TestRemoveWorktree_ConfigDefinedHookStillRuns(t *testing.T) {
	repoA, wt, _, _ := worktreeFixture(t)
	sentinel := filepath.Join(t.TempDir(), "config-hook-ran")
	gitIn(t, repoA, "config", "hook.p4611check.event", "reference-transaction")
	gitIn(t, repoA, "config", "hook.p4611check.command", "touch '"+sentinel+"'")
	out, err := exec.Command("git", "-C", repoA, "hook", "list", "reference-transaction").CombinedOutput()
	if err != nil || !strings.Contains(string(out), "p4611check") {
		t.Skipf("this git does not support configuration-defined hooks: %v %s", err, out)
	}

	deleted, err := RemoveWorktree(repoA, wt, repoA, "agent")
	if err != nil {
		t.Fatalf("RemoveWorktree: %v", err)
	}
	if !deleted {
		t.Fatal("expected the branch to be deleted")
	}
	if _, err := os.Stat(sentinel); err != nil {
		t.Errorf("configuration-defined hook did not run (%v); if repository configuration is now read-only or ignored, update this test and BaseRepoGitCommand's doc", err)
	}
}

// BaseRepoGitCommand applies HostGitCommand's first-argument rule: no
// arguments, an empty first argument, or a first argument that is an option
// returns errHostGitArgsOption and no command; a subcommand first builds one.
// Mutant this catches: removing the guard (a command is returned for "-C").
func TestBaseRepoGitCommand_RefusesLeadingOption(t *testing.T) {
	repo := setupGitRepo(t)
	for _, args := range [][]string{{}, {""}, {"-C", "x", "branch"}, {"--git-dir=x", "branch"}} {
		cmd, err := BaseRepoGitCommand(context.Background(), repo, args...)
		if !errors.Is(err, errHostGitArgsOption) || cmd != nil {
			t.Errorf("%q: cmd %v, err %v; want no command and the leading-option refusal", args, cmd, err)
		}
	}
	cmd, err := BaseRepoGitCommand(context.Background(), repo, "branch", "--list")
	if err != nil || cmd == nil {
		t.Errorf("branch --list: cmd %v, err %v; want a command", cmd, err)
	}
}
