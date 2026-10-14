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

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/provision"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/util"
)

// deleteWithHiddenSibling sets up a project repository with an agent
// worktree at .scion/agents/<agent>/workspace and a sibling worktree outside
// the repository whose directory is moved away (missing from this process's
// view). prepare runs before DeleteAgentFiles. It returns the repository,
// the sibling path, and a function that restores the sibling.
func deleteWithHiddenSibling(t *testing.T, agentName string, prepare func(projectDir, agentWorkspace string)) (string, string, func()) {
	t.Helper()
	t.Setenv("SCION_HOST_UID", "")
	tmpDir := t.TempDir()
	oldWd, _ := os.Getwd()
	t.Cleanup(func() { _ = os.Chdir(oldWd) })
	if err := os.Chdir(tmpDir); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", tmpDir)

	projectDir := filepath.Join(tmpDir, "project")
	if err := os.MkdirAll(projectDir, 0o755); err != nil {
		t.Fatal(err)
	}
	setupGitRepo(t, projectDir)
	scionDir := filepath.Join(projectDir, ".scion")
	agentWorkspace := filepath.Join(scionDir, "agents", agentName, "workspace")
	if err := os.MkdirAll(filepath.Dir(agentWorkspace), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := util.CreateWorktree(agentWorkspace, agentName); err != nil {
		t.Fatalf("CreateWorktree: %v", err)
	}

	volume := filepath.Join(tmpDir, "volume")
	sibling := filepath.Join(volume, "sibling")
	if err := os.MkdirAll(volume, 0o755); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("git", "-C", projectDir, "worktree", "add", "-b", "sibling", sibling).CombinedOutput(); err != nil {
		t.Fatalf("git worktree add sibling: %v\n%s", err, out)
	}
	hidden := filepath.Join(tmpDir, "volume-hidden")
	if err := os.Rename(volume, hidden); err != nil {
		t.Fatal(err)
	}
	restored := false
	restore := func() {
		if !restored {
			restored = true
			if err := os.Rename(hidden, volume); err != nil {
				t.Fatal(err)
			}
		}
	}
	t.Cleanup(restore)

	if prepare != nil {
		prepare(projectDir, agentWorkspace)
	}
	if _, err := DeleteAgentFiles(agentName, scionDir, true); err != nil {
		t.Fatalf("DeleteAgentFiles: %v", err)
	}
	return projectDir, sibling, restore
}

func assertSiblingWorktreeIntact(t *testing.T, projectDir, sibling string, restore func()) {
	t.Helper()
	restore()
	if list := listWorktrees(t, projectDir); !strings.Contains(list, "branch refs/heads/sibling") {
		t.Errorf("sibling worktree entry was removed; worktree list:\n%s", list)
	}
	if out, err := exec.Command("git", "-C", sibling, "status", "--short").CombinedOutput(); err != nil {
		t.Errorf("git status in sibling worktree failed: %v\n%s", err, out)
	}
}

func TestDeleteAgentFiles_KeepsHiddenSiblingWorktreeEntry(t *testing.T) {
	projectDir, sibling, restore := deleteWithHiddenSibling(t, "own-agent", nil)
	if list := listWorktrees(t, projectDir); strings.Contains(list, "own-agent") {
		t.Errorf("the deleted agent's worktree entry should be removed; worktree list:\n%s", list)
	}
	assertSiblingWorktreeIntact(t, projectDir, sibling, restore)
}

// The agent's workspace directory is already gone (an earlier incomplete
// cleanup): its stale entry is removed, and only that entry.
func TestDeleteAgentFiles_StaleOwnEntry_KeepsHiddenSibling(t *testing.T) {
	projectDir, sibling, restore := deleteWithHiddenSibling(t, "stale-agent", func(_, agentWorkspace string) {
		if err := os.RemoveAll(agentWorkspace); err != nil {
			t.Fatal(err)
		}
	})
	if list := listWorktrees(t, projectDir); strings.Contains(list, "stale-agent") {
		t.Errorf("the stale worktree entry should be removed; worktree list:\n%s", list)
	}
	assertSiblingWorktreeIntact(t, projectDir, sibling, restore)
}

// The whole agent directory is already gone (an earlier incomplete
// cleanup): the agent's stale entry is still removed, and only that entry.
func TestDeleteAgentFiles_AgentDirGone_RemovesStaleEntry(t *testing.T) {
	projectDir, sibling, restore := deleteWithHiddenSibling(t, "gone-agent", func(_, agentWorkspace string) {
		if err := os.RemoveAll(filepath.Dir(agentWorkspace)); err != nil {
			t.Fatal(err)
		}
	})
	if list := listWorktrees(t, projectDir); strings.Contains(list, "gone-agent") {
		t.Errorf("the stale worktree entry should be removed; worktree list:\n%s", list)
	}
	assertSiblingWorktreeIntact(t, projectDir, sibling, restore)
}

// When the agent's own entry cannot be removed (here: it is locked), the
// delete logs it and does not fall back to a prune.
func TestDeleteAgentFiles_OwnEntryRemovalFails_DoesNotPrune(t *testing.T) {
	projectDir, sibling, restore := deleteWithHiddenSibling(t, "locked-agent", func(projectDir, agentWorkspace string) {
		if out, err := exec.Command("git", "-C", projectDir, "worktree", "lock", agentWorkspace).CombinedOutput(); err != nil {
			t.Fatalf("git worktree lock: %v\n%s", err, out)
		}
	})
	assertSiblingWorktreeIntact(t, projectDir, sibling, restore)
}

// Deleting the creator of a shared worktree while another agent still
// shares it must leave that worktree's entry alone, even when its directory
// is missing from this process's view.
func TestDeleteAgentFiles_SharedWorktreeHidden_JoinerKeepsEntry(t *testing.T) {
	t.Setenv("SCION_HOST_UID", "")
	tmpDir := t.TempDir()
	oldWd, _ := os.Getwd()
	t.Cleanup(func() { _ = os.Chdir(oldWd) })
	if err := os.Chdir(tmpDir); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", tmpDir)

	bare := initBareRepo(t)
	gc := &api.GitCloneConfig{URL: bare, Branch: "main", Depth: intPtr(0)}
	projectPath := filepath.Join(tmpDir, "proj")
	scionDir := filepath.Join(projectPath, config.DotScion)
	if err := os.MkdirAll(filepath.Join(scionDir, "agents"), 0o755); err != nil {
		t.Fatal(err)
	}
	base := filepath.Join(projectPath, "workspace")
	resolved := provision.ResolvedWorkspace{HostPath: base, Backend: "local"}
	for _, id := range []string{"agent-a", "agent-b"} {
		if err := provision.ProvisionShared(provision.ProvisionInput{
			Resolved: resolved, Mode: store.SharingModeWorktreePerAgent,
			ProjectID: "p1", AgentID: id, AgentName: "shared-branch",
			GitClone: gc,
		}); err != nil {
			t.Fatalf("provision %s: %v", id, err)
		}
		if err := os.MkdirAll(filepath.Join(scionDir, "agents", id), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	wtA := provision.WorktreePath(base, "agent-a")
	if sharers, _, err := provision.ListSharers(base, "", "shared-branch"); err != nil || len(sharers) != 2 {
		t.Fatalf("setup: expected 2 sharers, got %v (err=%v)", sharers, err)
	}

	// Hide the shared worktree's directory.
	hidden := wtA + "-hidden"
	if err := os.Rename(wtA, hidden); err != nil {
		t.Fatal(err)
	}
	restored := false
	restore := func() {
		if !restored {
			restored = true
			if err := os.Rename(hidden, wtA); err != nil {
				t.Fatal(err)
			}
		}
	}
	t.Cleanup(restore)

	if _, err := DeleteAgentFiles("agent-a", projectPath, true); err != nil {
		t.Fatalf("DeleteAgentFiles(agent-a): %v", err)
	}
	restore()

	if list := listWorktrees(t, base); !strings.Contains(list, "branch refs/heads/shared-branch") {
		t.Errorf("the shared worktree entry was removed while agent-b still shares it; worktree list:\n%s", list)
	}
	if out, err := exec.Command("git", "-C", wtA, "status", "--short").CombinedOutput(); err != nil {
		t.Errorf("git status in the shared worktree failed: %v\n%s", err, out)
	}
}
