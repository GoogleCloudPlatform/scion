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

package agent

// Provisioning and agent-dir cleanup remove only the agent's own worktree
// entry, never every entry whose directory is missing: another agent's
// worktree that this process cannot see keeps its registration
// (ptone/scion#5071).

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/util"
)

// addHiddenSiblingWorktree adds a worktree on branch "sibling" in a
// directory outside repo, then moves its parent away so its path is missing
// from this process's view. It returns the sibling path and a function that
// puts it back (also run at cleanup).
func addHiddenSiblingWorktree(t *testing.T, repo string) (string, func()) {
	t.Helper()
	other := t.TempDir()
	volume := filepath.Join(other, "volume")
	sibling := filepath.Join(volume, "sibling")
	if err := os.MkdirAll(volume, 0o755); err != nil {
		t.Fatal(err)
	}
	runGitT(t, repo, "worktree", "add", "-q", "-b", "sibling", sibling)
	hidden := filepath.Join(other, "volume-hidden")
	if err := os.Rename(volume, hidden); err != nil {
		t.Fatal(err)
	}
	restored := false
	restore := func() {
		if restored {
			return
		}
		restored = true
		if err := os.Rename(hidden, volume); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(restore)
	return sibling, restore
}

// requireNoEntryForBranch fails when repo still has a worktree entry for
// branch.
func requireNoEntryForBranch(t *testing.T, repo, branch string) {
	t.Helper()
	if list := listWorktrees(t, repo); strings.Contains(list, "branch refs/heads/"+branch+"\n") {
		t.Errorf("the worktree entry for branch %q should be removed; worktree list:\n%s", branch, list)
	}
}

// A first provision that fails removes the agent dir it created without
// pruning the repository, also when the project is reached through a
// symlinked directory.
func TestProvisionAgent_FailedFirstProvision_KeepsHiddenSibling(t *testing.T) {
	for _, symlinked := range []bool{false, true} {
		name := "plain"
		if symlinked {
			name = "symlinked parent"
		}
		t.Run(name, func(t *testing.T) {
			tmpDir := t.TempDir()
			if symlinked {
				realDir := filepath.Join(tmpDir, "real")
				if err := os.MkdirAll(realDir, 0o755); err != nil {
					t.Fatal(err)
				}
				link := filepath.Join(tmpDir, "link")
				if err := os.Symlink(realDir, link); err != nil {
					t.Fatal(err)
				}
				tmpDir = link
			}
			agentName := "failed-first-agent"
			scionDir, agentDir, _ := setupNoConfigAgentProjectIn(t, tmpDir, agentName)
			projectDir := filepath.Dir(scionDir)
			// Provisioning requires the agents directory to be ignored.
			if err := os.WriteFile(filepath.Join(projectDir, ".gitignore"), []byte(".scion/agents/\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			setupGitRepo(t, projectDir)
			sibling, restore := addHiddenSiblingWorktree(t, projectDir)
			if err := os.RemoveAll(agentDir); err != nil {
				t.Fatal(err)
			}

			ctx := api.ContextWithGitClone(context.Background(), &api.GitCloneConfig{URL: "https://example.com/o/r.git"})
			if _, _, _, err := ProvisionAgent(ctx, agentName, "default", "", "", scionDir, "", "", "", "relative/ws"); err == nil {
				t.Fatal("expected ProvisionAgent to fail for a relative workspace with git clone")
			}
			if _, err := os.Stat(agentDir); !os.IsNotExist(err) {
				t.Fatalf("expected the agent dir created by the failed provision to be removed (stat err=%v)", err)
			}
			assertSiblingWorktreeIntact(t, projectDir, sibling, restore)
		})
	}
}

// A stale agent dir (no config, no home state, clean workspace) is removed
// with its own worktree entry, and only that entry.
func TestHandleAgentDirWithoutConfig_StaleDir_KeepsHiddenSibling(t *testing.T) {
	_, repo, agentDir, agentHome, ws := staleWorktreeLayout(t)
	sibling, restore := addHiddenSiblingWorktree(t, repo)

	if err := handleAgentDirWithoutConfig("a", agentDir, agentHome, ws); err != nil {
		t.Fatalf("handleAgentDirWithoutConfig: %v", err)
	}
	if _, err := os.Stat(agentDir); !os.IsNotExist(err) {
		t.Fatalf("expected the agent dir to be removed (stat err=%v)", err)
	}
	requireNoEntryForBranch(t, repo, "a")
	assertSiblingWorktreeIntact(t, repo, sibling, restore)
}

// An expired kept dir is purged after being renamed to a tomb. git's entry
// names the original agentDir/workspace, so that entry is removed, also
// when the project is reached through a symlinked directory, and no other.
func TestPurgeExpiredKeptAgentDir_SymlinkedParent_RemovesOwnEntryOnly(t *testing.T) {
	keptTestRetention(t, 72*time.Hour)
	now := keptTestClock(t)
	base := isolateGitTestEnv(t)
	realRoot := filepath.Join(base, "real")
	if err := os.MkdirAll(realRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "link")
	if err := os.Symlink(realRoot, link); err != nil {
		t.Fatal(err)
	}
	repo := filepath.Join(link, "project")
	initTestRepo(t, repo)
	agentDir := filepath.Join(repo, ".scion", "agents", "a")
	agentHome := filepath.Join(base, "external", "a", "home")
	for _, d := range []string{agentDir, agentHome} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	ws := filepath.Join(agentDir, "workspace")
	runGitT(t, repo, "worktree", "add", "-q", "--relative-paths", "-b", "a", ws)
	if err := os.WriteFile(filepath.Join(ws, "notes.txt"), []byte("keep me\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(base)

	if err := handleAgentDirWithoutConfig("a", agentDir, agentHome, ws); err == nil {
		t.Fatal("expected the dir with work to be kept on the first call")
	}
	if _, err := os.Stat(agentDir); err != nil {
		t.Fatalf("expected the agent dir to be kept: %v", err)
	}
	sibling, restore := addHiddenSiblingWorktree(t, repo)

	*now = now.Add(72 * time.Hour)
	if err := handleAgentDirWithoutConfig("a", agentDir, agentHome, ws); err != nil {
		t.Fatalf("handleAgentDirWithoutConfig after the window: %v", err)
	}
	requireRemoved(t, agentDir)
	requireNoEntryForBranch(t, repo, "a")
	assertSiblingWorktreeIntact(t, repo, sibling, restore)
}

// A new agent whose branch's worktree directory was removed out of band
// (its entry left behind at the agent's own path) is provisioned again on
// that branch. Only the agent's own stale entry is removed; there is no
// prune before the lookup or before the create.
func TestProvisionAgent_OwnStaleEntry_ReprovisionsAndKeepsHiddenSibling(t *testing.T) {
	scionDir, _ := reprovisionSetup(t)
	resolvedScionDir, err := config.GetResolvedProjectDir(scionDir)
	if err != nil {
		t.Fatalf("GetResolvedProjectDir: %v", err)
	}
	root, err := util.RepoRootDir(filepath.Dir(scionDir))
	if err != nil {
		t.Fatalf("RepoRootDir: %v", err)
	}
	agentDir := filepath.Join(resolvedScionDir, "agents", "b")
	ws := filepath.Join(agentDir, "workspace")
	if err := os.MkdirAll(agentDir, 0o755); err != nil {
		t.Fatal(err)
	}
	runGitT(t, root, "worktree", "add", "-q", "--relative-paths", "-b", "b", ws)
	if err := os.RemoveAll(agentDir); err != nil {
		t.Fatal(err)
	}
	sibling, restore := addHiddenSiblingWorktree(t, root)

	if _, _, _, err := ProvisionAgent(context.Background(), "b", "default", "", "", scionDir, "", "created", "b", ""); err != nil {
		t.Fatalf("ProvisionAgent: %v", err)
	}
	if _, err := os.Stat(filepath.Join(ws, ".git")); err != nil {
		t.Fatalf("expected a worktree at %s: %v", ws, err)
	}
	assertSiblingWorktreeIntact(t, root, sibling, restore)
}

// A branch held by another worktree at a path that is not the agent's own
// is not taken over, even when that worktree's directory is missing from
// this process's view: provisioning fails with an error that names the
// holder and how to clear it, and the holder keeps its registration.
func TestProvisionAgent_BranchHeldByHiddenWorktree_ClearErrorAndKeepsEntry(t *testing.T) {
	scionDir, _ := reprovisionSetup(t)
	root, err := util.RepoRootDir(filepath.Dir(scionDir))
	if err != nil {
		t.Fatalf("RepoRootDir: %v", err)
	}
	sibling, restore := addHiddenSiblingWorktree(t, root)

	_, _, _, err = ProvisionAgent(context.Background(), "c", "default", "", "", scionDir, "", "created", "sibling", "")
	if err == nil {
		t.Fatal("expected ProvisionAgent to fail for a branch held by another worktree")
	}
	if !strings.Contains(err.Error(), sibling) || !strings.Contains(err.Error(), fmt.Sprintf("git worktree remove %q", sibling)) {
		t.Errorf("error should name the holder and how to clear it; got: %v", err)
	}
	assertSiblingWorktreeIntact(t, root, sibling, restore)
}

// GetAgent recreating an agent's missing worktree removes only the agent's
// own stale entry.
func TestGetAgent_RecreateMissingWorktree_KeepsHiddenSibling(t *testing.T) {
	scionDir, _ := reprovisionSetup(t)
	resolvedScionDir, err := config.GetResolvedProjectDir(scionDir)
	if err != nil {
		t.Fatalf("GetResolvedProjectDir: %v", err)
	}
	root, err := util.RepoRootDir(filepath.Dir(scionDir))
	if err != nil {
		t.Fatalf("RepoRootDir: %v", err)
	}
	if _, _, _, err := ProvisionAgent(context.Background(), "d", "default", "", "", scionDir, "", "created", "d", ""); err != nil {
		t.Fatalf("ProvisionAgent: %v", err)
	}
	ws := filepath.Join(resolvedScionDir, "agents", "d", "workspace")
	if err := os.RemoveAll(ws); err != nil {
		t.Fatal(err)
	}
	sibling, restore := addHiddenSiblingWorktree(t, root)

	if _, _, _, _, err := GetAgent(context.Background(), "d", "", "", "", scionDir, "", "", "d", ""); err != nil {
		t.Fatalf("GetAgent: %v", err)
	}
	if _, err := os.Stat(filepath.Join(ws, ".git")); err != nil {
		t.Fatalf("expected the worktree to be recreated at %s: %v", ws, err)
	}
	assertSiblingWorktreeIntact(t, root, sibling, restore)
}
