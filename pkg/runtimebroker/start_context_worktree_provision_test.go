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
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/provision"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/util"
)

// --- resolveWorktreeProvision tests ---

func TestResolveWorktreeProvision_Eligible(t *testing.T) {
	projectDir := t.TempDir()

	result := resolveWorktreeProvision(worktreeProvisionInput{
		WorkspaceMode: store.WorkspaceModeWorktreePerAgent,
		GitClone: &api.GitCloneConfig{
			URL:    "https://github.com/org/repo.git",
			Branch: "main",
			Depth:  intPtr(1),
		},
		ProjectPath: projectDir,
		ProjectID:   "proj-1",
		ProjectSlug: "my-project",
		AgentID:     "agent-1",
		AgentName:   "test-agent",
	})

	eligible, _ := runtime.WorktreeModeEligible()
	if !eligible {
		if result.ShouldProvision {
			t.Fatal("expected ShouldProvision=false when git is too old")
		}
		t.Skip("git < 2.48, worktree mode not eligible on this host")
	}

	if !result.ShouldProvision {
		t.Fatalf("expected ShouldProvision=true, got false (reason: %s)", result.Reason)
	}

	expectedPath := filepath.Join(projectDir, "workspace", "worktrees", "agent-1")
	if result.WorktreePath != expectedPath {
		t.Errorf("expected WorktreePath=%q, got %q", expectedPath, result.WorktreePath)
	}

	expectedRoot := filepath.Join(projectDir, "workspace")
	if result.ProjectRoot != expectedRoot {
		t.Errorf("expected ProjectRoot=%q, got %q", expectedRoot, result.ProjectRoot)
	}

	pi := result.ProvisionInput
	if pi.Mode != store.SharingModeWorktreePerAgent {
		t.Errorf("expected Mode=worktree-per-agent, got %v", pi.Mode)
	}
	if pi.ProjectID != "proj-1" {
		t.Errorf("expected ProjectID='proj-1', got %q", pi.ProjectID)
	}
	if pi.AgentID != "agent-1" {
		t.Errorf("expected AgentID='agent-1', got %q", pi.AgentID)
	}
	if pi.GitClone == nil || pi.GitClone.URL != "https://github.com/org/repo.git" {
		t.Errorf("expected GitClone.URL set, got %v", pi.GitClone)
	}
}

func TestResolveWorktreeProvision_BranchOverridesAgentName(t *testing.T) {
	projectDir := t.TempDir()
	eligible, _ := runtime.WorktreeModeEligible()
	if !eligible {
		t.Skip("git < 2.48, worktree mode not eligible on this host")
	}

	result := resolveWorktreeProvision(worktreeProvisionInput{
		WorkspaceMode: store.WorkspaceModeWorktreePerAgent,
		GitClone:      &api.GitCloneConfig{URL: "https://example.com/repo.git"},
		ProjectPath:   projectDir,
		ProjectID:     "proj-1",
		AgentID:       "agent-1",
		AgentName:     "test-agent",
		Branch:        "feature-branch",
	})

	if !result.ShouldProvision {
		t.Fatalf("expected ShouldProvision=true, reason: %s", result.Reason)
	}
	if result.ProvisionInput.AgentName != "feature-branch" {
		t.Errorf("expected AgentName='feature-branch' (from Branch), got %q", result.ProvisionInput.AgentName)
	}
}

func TestResolveWorktreeProvision_WrongMode(t *testing.T) {
	result := resolveWorktreeProvision(worktreeProvisionInput{
		WorkspaceMode: store.WorkspaceModePerAgent,
		GitClone:      &api.GitCloneConfig{URL: "https://example.com/repo.git"},
		ProjectPath:   "/some/path",
		ProjectID:     "proj-1",
		AgentID:       "agent-1",
	})

	if result.ShouldProvision {
		t.Fatal("expected ShouldProvision=false for non-worktree mode")
	}
	if !strings.Contains(result.Reason, "not worktree-per-agent") {
		t.Errorf("expected reason to mention mode mismatch, got %q", result.Reason)
	}
}

func TestResolveWorktreeProvision_NoGitClone(t *testing.T) {
	result := resolveWorktreeProvision(worktreeProvisionInput{
		WorkspaceMode: store.WorkspaceModeWorktreePerAgent,
		GitClone:      nil,
		ProjectPath:   "/some/path",
		ProjectID:     "proj-1",
		AgentID:       "agent-1",
	})

	if result.ShouldProvision {
		t.Fatal("expected ShouldProvision=false when GitClone is nil")
	}
	if !strings.Contains(result.Reason, "not git-backed") {
		t.Errorf("expected reason to mention non-git, got %q", result.Reason)
	}
}

// TestResolveWorktreeProvision_MissingIDs is the defense-in-depth guard for
// GoogleCloudPlatform/scion#1931's worktree-per-agent start path: without
// both AgentID and ProjectID, provision.WorktreePath(base, "") resolves to
// the shared "worktrees" parent directory every agent's worktree lives
// under, not a per-agent path. Provisioning must be skipped entirely (fall
// back to clone-per-agent) rather than ever touching that shared path.
func TestResolveWorktreeProvision_MissingIDs(t *testing.T) {
	base := worktreeProvisionInput{
		WorkspaceMode: store.WorkspaceModeWorktreePerAgent,
		GitClone:      &api.GitCloneConfig{URL: "https://example.com/repo.git"},
		ProjectPath:   "/some/path",
		ProjectID:     "proj-1",
		AgentID:       "agent-1",
	}

	missingAgentID := base
	missingAgentID.AgentID = ""
	if result := resolveWorktreeProvision(missingAgentID); result.ShouldProvision {
		t.Fatal("expected ShouldProvision=false when AgentID is empty")
	} else if !strings.Contains(result.Reason, "AgentID") {
		t.Errorf("expected reason to mention AgentID, got %q", result.Reason)
	}

	missingProjectID := base
	missingProjectID.ProjectID = ""
	if result := resolveWorktreeProvision(missingProjectID); result.ShouldProvision {
		t.Fatal("expected ShouldProvision=false when ProjectID is empty")
	} else if !strings.Contains(result.Reason, "ProjectID") {
		t.Errorf("expected reason to mention ProjectID, got %q", result.Reason)
	}

	if result := resolveWorktreeProvision(base); !result.ShouldProvision {
		eligible, _ := runtime.WorktreeModeEligible()
		if eligible {
			t.Errorf("expected ShouldProvision=true when both IDs are set, reason: %s", result.Reason)
		}
	}
}

// TestTryProvisionWorktree_MissingIdentityOnStart_FailsClosed proves a
// start dispatch (never a create) with no valid agent identity fails the
// request instead of silently falling back to an in-container clone, which
// would mount a fresh, empty workspace over whatever this agent's real
// worktree holds — a create with the same missing identity still falls
// back, since a fresh create has no existing worktree to protect.
// TestResolveWorktreeProvision_InvalidIDsRejected is the table test for an
// AgentID or ProjectID that is any of the listed invalid or malformed
// values: each must be rejected, never reaching path construction.
func TestResolveWorktreeProvision_InvalidIDsRejected(t *testing.T) {
	projectDir := t.TempDir()
	invalidValues := []string{
		"..",
		".",
		"../../x",
		"a/b",
		"/tmp/abs",
		"agent-1/",
		"",
		"a\\b",
		"a\x00b",
	}
	for _, id := range invalidValues {
		t.Run("agentID="+id, func(t *testing.T) {
			result := resolveWorktreeProvision(worktreeProvisionInput{
				WorkspaceMode: store.WorkspaceModeWorktreePerAgent,
				GitClone:      &api.GitCloneConfig{URL: "https://example.com/repo.git"},
				ProjectPath:   projectDir,
				ProjectID:     "proj-1",
				AgentID:       id,
			})
			if result.ShouldProvision {
				t.Fatalf("expected ShouldProvision=false for AgentID=%q", id)
			}
			if !result.MissingIdentity {
				t.Errorf("expected MissingIdentity=true for AgentID=%q, reason: %s", id, result.Reason)
			}
		})
		t.Run("projectID="+id, func(t *testing.T) {
			result := resolveWorktreeProvision(worktreeProvisionInput{
				WorkspaceMode: store.WorkspaceModeWorktreePerAgent,
				GitClone:      &api.GitCloneConfig{URL: "https://example.com/repo.git"},
				ProjectPath:   projectDir,
				ProjectID:     id,
				AgentID:       "agent-1",
			})
			if result.ShouldProvision {
				t.Fatalf("expected ShouldProvision=false for ProjectID=%q", id)
			}
			if !result.MissingIdentity {
				t.Errorf("expected MissingIdentity=true for ProjectID=%q, reason: %s", id, result.Reason)
			}
		})
	}

	// A literal-looking "%2e%2e" is an ordinary, if unusual, directory
	// name, since nothing decodes it, and must be accepted like any other
	// opaque ID.
	if !isSingleCleanPathElement("%2e%2e") {
		t.Error(`expected "%2e%2e" to be a single clean path element (a literal name, not interpreted)`)
	}
}

func TestTryProvisionWorktree_MissingIdentityOnStart_FailsClosed(t *testing.T) {
	requireWorktreeGit(t)
	cfg := DefaultServerConfig()
	cfg.StateDir = t.TempDir()
	srv := newTestServerForStartContext(t, cfg)

	bare := initBareRepoWithCommit(t)
	gc := &api.GitCloneConfig{URL: bare, Branch: "main"}
	projectPath := filepath.Join(t.TempDir(), "proj")
	if err := os.MkdirAll(projectPath, 0o755); err != nil {
		t.Fatal(err)
	}

	opts := &api.StartOptions{}
	ok, _, err := srv.tryProvisionWorktree(context.Background(), startContextInputs{
		Name:          "some-agent",
		AgentID:       "", // missing
		ProjectID:     "p1",
		ProjectPath:   projectPath,
		WorkspaceMode: store.WorkspaceModeWorktreePerAgent,
		Config:        &CreateAgentConfig{GitClone: gc},
		Operation:     opHTTPStart,
	}, opts, map[string]string{}, srv.runtime.Name())

	if err == nil {
		t.Fatal("expected tryProvisionWorktree to fail closed when AgentID is missing on a start dispatch")
	}
	if ok {
		t.Error("expected ok=false alongside the error")
	}
	if opts.GitClone != nil {
		t.Error("expected no fallback to an in-container clone")
	}

	// The same missing-identity case on a create dispatch still falls back.
	opts2 := &api.StartOptions{}
	ok2, _, err2 := srv.tryProvisionWorktree(context.Background(), startContextInputs{
		Name:          "some-agent",
		AgentID:       "",
		ProjectID:     "p1",
		ProjectPath:   projectPath,
		WorkspaceMode: store.WorkspaceModeWorktreePerAgent,
		Config:        &CreateAgentConfig{GitClone: gc},
		Operation:     opCreate,
	}, opts2, map[string]string{}, srv.runtime.Name())
	if err2 != nil {
		t.Fatalf("expected create to fall back cleanly, got error: %v", err2)
	}
	if ok2 {
		t.Error("expected ok=false (fallback), got true")
	}
}

func TestResolveWorktreeProvision_GitTooOld_Fallback(t *testing.T) {
	projectDir := t.TempDir()

	result := resolveWorktreeProvision(worktreeProvisionInput{
		WorkspaceMode: store.WorkspaceModeWorktreePerAgent,
		GitClone: &api.GitCloneConfig{
			URL:    "https://github.com/org/repo.git",
			Branch: "main",
			Depth:  intPtr(1),
		},
		ProjectPath: projectDir,
		ProjectID:   "proj-1",
		ProjectSlug: "my-project",
		AgentID:     "agent-1",
		AgentName:   "test-agent",
		eligibilityOverride: func() (bool, string) {
			return false, "git >= 2.48.0 required for worktree-per-agent mode (--relative-paths), found 2.39.0"
		},
	})

	if result.ShouldProvision {
		t.Fatal("expected ShouldProvision=false when git is too old")
	}
	if !strings.Contains(result.Reason, "2.48") {
		t.Errorf("expected reason to mention git 2.48 requirement, got %q", result.Reason)
	}
	if result.ProvisionInput.ProjectID != "" {
		t.Error("expected empty ProvisionInput when ineligible")
	}
}

func TestResolveWorktreeProvision_KubernetesNodeLocal_Rejected(t *testing.T) {
	projectDir := t.TempDir()

	result := resolveWorktreeProvision(worktreeProvisionInput{
		WorkspaceMode: store.WorkspaceModeWorktreePerAgent,
		GitClone: &api.GitCloneConfig{
			URL:    "https://github.com/org/repo.git",
			Branch: "main",
		},
		ProjectPath: projectDir,
		ProjectID:   "proj-1",
		ProjectSlug: "my-project",
		AgentID:     "agent-1",
		AgentName:   "test-agent",
		RuntimeName: "kubernetes",
		eligibilityOverride: func() (bool, string) {
			return true, ""
		},
	})

	if result.ShouldProvision {
		t.Fatal("expected ShouldProvision=false for Kubernetes without NFS backend")
	}
	if !strings.Contains(result.Reason, "Kubernetes") {
		t.Errorf("expected reason to mention Kubernetes, got %q", result.Reason)
	}
	if !strings.Contains(result.Reason, "NFS") {
		t.Errorf("expected reason to mention NFS requirement, got %q", result.Reason)
	}
	if result.ProvisionInput.ProjectID != "" {
		t.Error("expected empty ProvisionInput when rejected")
	}
}

// TestResolveWorktreeProvision_KubernetesAliases_Rejected: every recognized
// spelling of the Kubernetes runtime skips host-side provisioning.
func TestResolveWorktreeProvision_KubernetesAliases_Rejected(t *testing.T) {
	for _, name := range []string{"k8s", "remote"} {
		t.Run(name, func(t *testing.T) {
			result := resolveWorktreeProvision(worktreeProvisionInput{
				WorkspaceMode: store.WorkspaceModeWorktreePerAgent,
				GitClone:      &api.GitCloneConfig{URL: "https://github.com/org/repo.git", Branch: "main"},
				ProjectPath:   t.TempDir(),
				ProjectID:     "proj-1",
				AgentID:       "agent-1",
				AgentName:     "test-agent",
				RuntimeName:   name,
				eligibilityOverride: func() (bool, string) {
					return true, ""
				},
			})
			if result.ShouldProvision {
				t.Fatalf("expected ShouldProvision=false for runtime %q", name)
			}
			if !strings.Contains(result.Reason, "Kubernetes") {
				t.Errorf("expected reason to mention Kubernetes, got %q", result.Reason)
			}
		})
	}
}

func TestResolveWorktreeProvision_DockerRuntime_NotRejected(t *testing.T) {
	eligible, _ := runtime.WorktreeModeEligible()
	if !eligible {
		t.Skip("git < 2.48, worktree mode not eligible on this host")
	}

	projectDir := t.TempDir()

	result := resolveWorktreeProvision(worktreeProvisionInput{
		WorkspaceMode: store.WorkspaceModeWorktreePerAgent,
		GitClone: &api.GitCloneConfig{
			URL:    "https://github.com/org/repo.git",
			Branch: "main",
		},
		ProjectPath: projectDir,
		ProjectID:   "proj-1",
		ProjectSlug: "my-project",
		AgentID:     "agent-1",
		AgentName:   "test-agent",
		RuntimeName: "docker",
	})

	if !result.ShouldProvision {
		t.Fatalf("expected ShouldProvision=true for Docker runtime, got false (reason: %s)", result.Reason)
	}
}

func TestResolveWorktreeProvision_EmptyRuntime_NotRejected(t *testing.T) {
	eligible, _ := runtime.WorktreeModeEligible()
	if !eligible {
		t.Skip("git < 2.48, worktree mode not eligible on this host")
	}

	projectDir := t.TempDir()

	result := resolveWorktreeProvision(worktreeProvisionInput{
		WorkspaceMode: store.WorkspaceModeWorktreePerAgent,
		GitClone: &api.GitCloneConfig{
			URL:    "https://github.com/org/repo.git",
			Branch: "main",
		},
		ProjectPath: projectDir,
		ProjectID:   "proj-1",
		AgentID:     "agent-1",
		RuntimeName: "",
	})

	if !result.ShouldProvision {
		t.Fatalf("expected ShouldProvision=true for empty RuntimeName, got false (reason: %s)", result.Reason)
	}
}

func TestResolveWorktreeProvision_FullCloneDepth(t *testing.T) {
	eligible, _ := runtime.WorktreeModeEligible()
	if !eligible {
		t.Skip("git < 2.48, worktree mode not eligible on this host")
	}

	projectDir := t.TempDir()
	originalGC := &api.GitCloneConfig{
		URL:    "https://github.com/org/repo.git",
		Branch: "main",
		Depth:  intPtr(1),
	}

	result := resolveWorktreeProvision(worktreeProvisionInput{
		WorkspaceMode: store.WorkspaceModeWorktreePerAgent,
		GitClone:      originalGC,
		ProjectPath:   projectDir,
		ProjectID:     "proj-1",
		ProjectSlug:   "my-project",
		AgentID:       "agent-1",
	})

	if !result.ShouldProvision {
		t.Fatalf("expected ShouldProvision=true, reason: %s", result.Reason)
	}

	if result.ProvisionInput.GitClone.Depth == nil || *result.ProvisionInput.GitClone.Depth != 0 {
		t.Errorf("expected GitClone.Depth=0 (full clone), got %v", result.ProvisionInput.GitClone.Depth)
	}

	if originalGC.Depth == nil || *originalGC.Depth != 1 {
		t.Errorf("original GitClone.Depth was mutated: got %v, want 1", originalGC.Depth)
	}
}

// TestTryProvisionWorktree_JoinResolvesSharedPath verifies that when agent-b
// is provisioned with --branch pointing to an already-checked-out branch
// (agent-a's), provisioning succeeds as a JOIN and opts.Workspace is set to
// agent-a's worktree path (not WorktreePath(base, agent-b)).
func TestTryProvisionWorktree_JoinResolvesSharedPath(t *testing.T) {
	t.Setenv("SCION_HOST_UID", "")

	cfg := DefaultServerConfig()
	cfg.StateDir = t.TempDir()
	srv := newTestServerForStartContext(t, cfg)

	bare := initBareRepoWithCommit(t)
	gc := &api.GitCloneConfig{URL: bare, Branch: "main"}

	projectPath := filepath.Join(t.TempDir(), "proj")
	if err := os.MkdirAll(projectPath, 0o755); err != nil {
		t.Fatal(err)
	}

	// Set up the shared base + agent-a's worktree on branch "agent-a".
	resolved, err := runtime.NewLocalBackend().Resolve(runtime.ResolveInput{
		ProjectDir: projectPath, ProjectID: "p1", AgentID: "agent-a",
		Mode: store.SharingModeWorktreePerAgent,
	})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if err := provision.ProvisionShared(provision.ProvisionInput{
		Resolved: resolved, Mode: store.SharingModeWorktreePerAgent,
		ProjectID: "p1", AgentID: "agent-a", AgentName: "agent-a", GitClone: gc,
	}); err != nil {
		t.Fatalf("setup agent-a: %v", err)
	}
	base := resolved.HostPath
	agentAWt := provision.WorktreePath(base, "agent-a")
	if _, err := os.Stat(agentAWt); err != nil {
		t.Fatalf("agent-a worktree missing after setup: %v", err)
	}

	// Provision agent-b with --branch "agent-a" → should JOIN, not fail.
	opts := &api.StartOptions{}
	ok, repoRoot, err := srv.tryProvisionWorktree(context.Background(), startContextInputs{
		Name: "agent-b", AgentID: "agent-b",
		ProjectID: "p1", ProjectSlug: "proj", ProjectPath: projectPath,
		WorkspaceMode: store.WorkspaceModeWorktreePerAgent,
		Config:        &CreateAgentConfig{GitClone: gc, Branch: "agent-a"},
	}, opts, map[string]string{}, srv.runtime.Name())
	if err != nil {
		t.Fatalf("tryProvisionWorktree returned an error: %v", err)
	}

	if !ok {
		t.Fatal("expected JOIN to succeed, got ok=false (fell back to clone-per-agent)")
	}
	if repoRoot != base {
		t.Errorf("repoRoot = %q, want %q (the shared base)", repoRoot, base)
	}

	// opts.Workspace must point to agent-a's worktree (the shared path).
	if opts.Workspace != agentAWt {
		t.Errorf("opts.Workspace = %q, want %q (agent-a's worktree)", opts.Workspace, agentAWt)
	}

	// No separate worktree created for agent-b.
	agentBWt := provision.WorktreePath(base, "agent-b")
	if _, err := os.Stat(agentBWt); !os.IsNotExist(err) {
		t.Errorf("agent-b should NOT have its own worktree, stat err=%v", err)
	}

	// Both agents registered as sharers.
	sharers, wtPath, err := provision.ListSharers(base, "", "agent-a")
	if err != nil {
		t.Fatalf("ListSharers: %v", err)
	}
	if wtPath != agentAWt {
		t.Errorf("registry worktreePath = %q, want %q", wtPath, agentAWt)
	}
	if len(sharers) != 2 {
		t.Fatalf("expected 2 sharers, got %d: %v", len(sharers), sharers)
	}

	// Shared base and agent-a worktree are intact.
	if _, err := os.Stat(filepath.Join(base, ".git")); err != nil {
		t.Errorf("shared base .git was destroyed: %v", err)
	}
	if _, err := os.Stat(agentAWt); err != nil {
		t.Errorf("agent-a worktree was destroyed: %v", err)
	}
}

// TestTryProvisionWorktree_JoinTargetPreExisting_FailsInsteadOfFallback
// covers a JOIN agent whose own WorktreePath(base, "agent-b") never exists
// (it shares agent-a's worktree instead): the preExisted check also
// consults the sharer registry, so it still recognizes that a real, live
// worktree it is about to attach to already exists. A provisioning failure
// on the JOIN fails the start — no removal (there is nothing at agent-b's
// own path to remove anyway), and no silent fallback to an in-container
// clone, which would abandon agent-a's live worktree without ever mounting
// it for agent-b.
func TestTryProvisionWorktree_JoinTargetPreExisting_FailsInsteadOfFallback(t *testing.T) {
	requireWorktreeGit(t)
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory permissions, so the read-only sharer dir fault injection below never fails")
	}
	t.Setenv("SCION_HOST_UID", "")

	cfg := DefaultServerConfig()
	cfg.StateDir = t.TempDir()
	srv := newTestServerForStartContext(t, cfg)

	bare := initBareRepoWithCommit(t)
	gc := &api.GitCloneConfig{URL: bare, Branch: "main"}

	projectPath := filepath.Join(t.TempDir(), "proj")
	if err := os.MkdirAll(projectPath, 0o755); err != nil {
		t.Fatal(err)
	}

	// Set up the shared base + agent-a's worktree on branch "agent-a", as
	// the JOIN target.
	resolved, err := runtime.NewLocalBackend().Resolve(runtime.ResolveInput{
		ProjectDir: projectPath, ProjectID: "p1", AgentID: "agent-a",
		Mode: store.SharingModeWorktreePerAgent,
	})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if err := provision.ProvisionShared(provision.ProvisionInput{
		Resolved: resolved, Mode: store.SharingModeWorktreePerAgent,
		ProjectID: "p1", AgentID: "agent-a", AgentName: "agent-a", GitClone: gc,
	}); err != nil {
		t.Fatalf("setup agent-a: %v", err)
	}
	base := resolved.HostPath
	agentAWt := provision.WorktreePath(base, "agent-a")
	if _, err := os.Stat(agentAWt); err != nil {
		t.Fatalf("agent-a worktree missing after setup: %v", err)
	}

	// Make the sharer registry directory read-only: ListSharers (a read of
	// an existing, valid marker) still succeeds and reports agent-a's
	// worktree, but RegisterSharer's write to add agent-b as a sharer fails
	// — a failure that happens only because a real JOIN target exists.
	sharerDir := filepath.Join(base, ".git", "scion-sharers")
	if err := os.Chmod(sharerDir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(sharerDir, 0o755) })

	// Simulate un-pushed work in agent-a's live worktree.
	const unpushedContent = "package main // un-pushed change\n"
	unpushedFile := filepath.Join(agentAWt, "unpushed.go")
	if err := os.WriteFile(unpushedFile, []byte(unpushedContent), 0644); err != nil {
		t.Fatal(err)
	}

	// Agent-b attempts to JOIN branch "agent-a": must fail outright.
	opts := &api.StartOptions{}
	ok, _, err := srv.tryProvisionWorktree(context.Background(), startContextInputs{
		Name: "agent-b", AgentID: "agent-b",
		ProjectID: "p1", ProjectSlug: "proj", ProjectPath: projectPath,
		WorkspaceMode: store.WorkspaceModeWorktreePerAgent,
		Config:        &CreateAgentConfig{GitClone: gc, Branch: "agent-a"},
	}, opts, map[string]string{}, srv.runtime.Name())

	if err == nil {
		t.Fatalf("expected tryProvisionWorktree to fail when the JOIN target's registration write fails, got ok=%v", ok)
	}
	if ok {
		t.Error("expected ok=false alongside the error")
	}

	// Agent-a's worktree and its un-pushed file must survive untouched.
	if _, statErr := os.Stat(agentAWt); statErr != nil {
		t.Errorf("agent-a's worktree must survive a failed JOIN, stat error: %v", statErr)
	}
	got, readErr := os.ReadFile(unpushedFile)
	if readErr != nil {
		t.Fatalf("un-pushed file must survive a failed JOIN, but reading it failed: %v", readErr)
	}
	if string(got) != unpushedContent {
		t.Errorf("un-pushed file content = %q, want %q", got, unpushedContent)
	}
}

// TestBuildStartContext_WorktreePerAgentOnStart_TakesWorktreePathAndReusesIt
// proves a start dispatch (Operation opHTTPStart) with WorkspaceMode
// worktree-per-agent and GitClone set takes create's worktree-per-agent
// path, not the in-container clone path — and that re-running start against
// the same agent reuses the existing worktree instead of recreating it
// (GoogleCloudPlatform/scion#1931).
func TestBuildStartContext_WorktreePerAgentOnStart_TakesWorktreePathAndReusesIt(t *testing.T) {
	requireWorktreeGit(t)
	cfg := DefaultServerConfig()
	cfg.StateDir = t.TempDir()
	srv := newTestServerForStartContext(t, cfg)

	bare := initBareRepoWithCommit(t)
	gc := &api.GitCloneConfig{URL: bare, Branch: "main"}

	projectPath := filepath.Join(t.TempDir(), "proj")
	if err := os.MkdirAll(projectPath, 0o755); err != nil {
		t.Fatal(err)
	}

	in := startContextInputs{
		Name:          "agent-a",
		AgentID:       "agent-a",
		ProjectID:     "p1",
		ProjectSlug:   "proj",
		ProjectPath:   projectPath,
		WorkspaceMode: store.WorkspaceModeWorktreePerAgent,
		Config:        &CreateAgentConfig{GitClone: gc},
		Operation:     opHTTPStart,
	}

	sc1, err := srv.buildStartContext(context.Background(), in)
	if err != nil {
		t.Fatalf("first buildStartContext failed: %v", err)
	}
	if sc1.Opts.GitClone != nil {
		t.Errorf("expected GitClone to be suppressed by the worktree path, got %+v", sc1.Opts.GitClone)
	}
	firstWorkspace := sc1.Opts.Workspace
	if firstWorkspace == "" {
		t.Fatal("expected a worktree Workspace path to be set")
	}
	if _, err := os.Stat(firstWorkspace); err != nil {
		t.Fatalf("expected worktree to exist on disk: %v", err)
	}

	// Simulate un-pushed work in the worktree between the two starts.
	const unpushedContent = "package main // un-pushed change\n"
	unpushedFile := filepath.Join(firstWorkspace, "unpushed.go")
	if err := os.WriteFile(unpushedFile, []byte(unpushedContent), 0644); err != nil {
		t.Fatal(err)
	}

	// Re-run start for the same agent: must reuse the existing worktree, not
	// recreate it or fall back to the in-container clone path.
	sc2, err := srv.buildStartContext(context.Background(), in)
	if err != nil {
		t.Fatalf("second buildStartContext (reuse) failed: %v", err)
	}
	if sc2.Opts.GitClone != nil {
		t.Errorf("expected GitClone to remain suppressed on reuse, got %+v", sc2.Opts.GitClone)
	}
	if sc2.Opts.Workspace != firstWorkspace {
		t.Errorf("expected start to reuse the same worktree path %q, got %q", firstWorkspace, sc2.Opts.Workspace)
	}

	// Reuse must not run any destructive git operation (e.g. git clean -fdx)
	// against the existing worktree: the un-pushed file must still be there.
	got, err := os.ReadFile(unpushedFile)
	if err != nil {
		t.Fatalf("un-pushed file must survive a worktree-reuse start, but reading it failed: %v", err)
	}
	if string(got) != unpushedContent {
		t.Errorf("un-pushed file content = %q, want %q", got, unpushedContent)
	}
}

// TestBuildStartContext_WorktreePerAgentEnvParity extends
// TestStartAndCreate_GitWorkspaceEnvParity (clone-per-agent) to
// worktree-per-agent: create and start, each provisioning a fresh agent's
// own worktree for the first time, must produce identical
// SCION_WORKSPACE_MODE/SCION_WORKSPACE_GIT env, and both must omit
// SCION_GIT_CLONE_URL since the worktree path suppresses the in-container
// clone on both operations.
func TestBuildStartContext_WorktreePerAgentEnvParity(t *testing.T) {
	cfg := DefaultServerConfig()
	cfg.StateDir = t.TempDir()
	srv := newTestServerForStartContext(t, cfg)

	bare := initBareRepoWithCommit(t)
	gc := &api.GitCloneConfig{URL: bare, Branch: "main"}

	createProjectPath := filepath.Join(t.TempDir(), "proj-create")
	if err := os.MkdirAll(createProjectPath, 0o755); err != nil {
		t.Fatal(err)
	}
	createSC, err := srv.buildStartContext(context.Background(), startContextInputs{
		Name:          "agent-create",
		AgentID:       "agent-create",
		ProjectID:     "p1",
		ProjectSlug:   "proj-create",
		ProjectPath:   createProjectPath,
		WorkspaceMode: store.WorkspaceModeWorktreePerAgent,
		Config:        &CreateAgentConfig{GitClone: gc},
		Operation:     opCreate,
	})
	if err != nil {
		t.Fatalf("create buildStartContext failed: %v", err)
	}

	startProjectPath := filepath.Join(t.TempDir(), "proj-start")
	if err := os.MkdirAll(startProjectPath, 0o755); err != nil {
		t.Fatal(err)
	}
	startSC, err := srv.buildStartContext(context.Background(), startContextInputs{
		Name:          "agent-start",
		AgentID:       "agent-start",
		ProjectID:     "p2",
		ProjectSlug:   "proj-start",
		ProjectPath:   startProjectPath,
		WorkspaceMode: store.WorkspaceModeWorktreePerAgent,
		Config:        &CreateAgentConfig{GitClone: gc},
		Operation:     opHTTPStart,
	})
	if err != nil {
		t.Fatalf("start buildStartContext failed: %v", err)
	}

	for _, key := range []string{"SCION_WORKSPACE_MODE", "SCION_WORKSPACE_GIT", "SCION_GIT_CLONE_URL"} {
		createVal, createOK := createSC.Opts.Env[key]
		startVal, startOK := startSC.Opts.Env[key]
		if createOK != startOK || createVal != startVal {
			t.Errorf("%s: create=%q(present=%v) start=%q(present=%v), want identical", key, createVal, createOK, startVal, startOK)
		}
	}
	if _, ok := createSC.Opts.Env["SCION_GIT_CLONE_URL"]; ok {
		t.Errorf("expected SCION_GIT_CLONE_URL absent for create worktree-per-agent, got %q", createSC.Opts.Env["SCION_GIT_CLONE_URL"])
	}
	if createSC.Opts.GitClone != nil {
		t.Errorf("expected create's in-container GitClone to be suppressed by the worktree path, got %+v", createSC.Opts.GitClone)
	}
	if startSC.Opts.GitClone != nil {
		t.Errorf("expected start's in-container GitClone to be suppressed by the worktree path, got %+v", startSC.Opts.GitClone)
	}
}

// TestBuildStartContext_WorktreePerAgentStart_ProvisioningFailureNeverRemovesExistingWorktree
// covers GoogleCloudPlatform/scion#1931's worktree-reuse path: on a second
// start (or restart) for an agent whose worktree already exists, a
// provisioning failure must fail the request and must never touch the
// existing worktree, since it may hold un-pushed work.
func TestBuildStartContext_WorktreePerAgentStart_ProvisioningFailureNeverRemovesExistingWorktree(t *testing.T) {
	requireWorktreeGit(t)
	cfg := DefaultServerConfig()
	cfg.StateDir = t.TempDir()
	srv := newTestServerForStartContext(t, cfg)

	bare := initBareRepoWithCommit(t)
	gc := &api.GitCloneConfig{URL: bare, Branch: "main"}

	projectPath := filepath.Join(t.TempDir(), "proj")
	if err := os.MkdirAll(projectPath, 0o755); err != nil {
		t.Fatal(err)
	}

	in := startContextInputs{
		Name:          "agent-a",
		AgentID:       "agent-a",
		ProjectID:     "p1",
		ProjectSlug:   "proj",
		ProjectPath:   projectPath,
		WorkspaceMode: store.WorkspaceModeWorktreePerAgent,
		Config:        &CreateAgentConfig{GitClone: gc},
		Operation:     opHTTPStart,
	}

	sc1, err := srv.buildStartContext(context.Background(), in)
	if err != nil {
		t.Fatalf("first buildStartContext failed: %v", err)
	}
	worktreePath := sc1.Opts.Workspace
	if worktreePath == "" {
		t.Fatal("expected a worktree Workspace path to be set")
	}

	// Simulate un-pushed work in the agent's live worktree.
	const unpushedContent = "package main // un-pushed change\n"
	unpushedFile := filepath.Join(worktreePath, "unpushed.go")
	if err := os.WriteFile(unpushedFile, []byte(unpushedContent), 0644); err != nil {
		t.Fatal(err)
	}

	// Corrupt the sharer marker so RegisterSharer's re-registration on the
	// next call fails: base is two levels above the per-agent worktree
	// (<base>/worktrees/<agentID>).
	base := filepath.Dir(filepath.Dir(worktreePath))
	markerPath := filepath.Join(base, ".git", "scion-sharers", "agent-a.json")
	if err := os.MkdirAll(filepath.Dir(markerPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(markerPath, []byte("{not valid json"), 0644); err != nil {
		t.Fatal(err)
	}

	// Start again: provisioning must fail (corrupt marker), and the request
	// itself must fail — no removal, no fallback to an in-container clone.
	_, err = srv.buildStartContext(context.Background(), in)
	if err == nil {
		t.Fatal("expected buildStartContext to fail when provisioning the existing worktree errors")
	}

	// The client-facing error must be generic: the underlying ProvisionShared
	// error (which can embed a raw clone URL, e.g. from git's own error text)
	// is logged server-side only, never returned to the caller.
	wantErr := `worktree-per-agent: provisioning failed for the existing worktree of agent "agent-a"; the existing workspace was left untouched`
	if err.Error() != wantErr {
		t.Errorf("error = %q, want %q", err.Error(), wantErr)
	}

	// The worktree and its un-pushed file must be untouched.
	if _, statErr := os.Stat(worktreePath); statErr != nil {
		t.Errorf("expected the existing worktree to survive the provisioning failure, stat error: %v", statErr)
	}
	got, readErr := os.ReadFile(unpushedFile)
	if readErr != nil {
		t.Fatalf("un-pushed file must survive a failed re-provision, but reading it failed: %v", readErr)
	}
	if string(got) != unpushedContent {
		t.Errorf("un-pushed file content = %q, want %q", got, unpushedContent)
	}
}

// TestBuildStartContext_WorktreePerAgentStart_MissingMarkersFailsInsteadOfSelfHeal
// is the fault-injection guard for GoogleCloudPlatform/scion#1931: when the
// provisioning sentinel and the shared base's .git are both missing (as
// provision.ProvisionShared's own self-heal expects for a first-time
// provision), but this agent's worktree already exists on disk with
// un-pushed work, the start must fail with a clear error instead of letting
// ProvisionShared re-clone over the shared
// base — and everything under it, including this worktree — while still
// returning success.
func TestBuildStartContext_WorktreePerAgentStart_MissingMarkersFailsInsteadOfSelfHeal(t *testing.T) {
	requireWorktreeGit(t)
	cfg := DefaultServerConfig()
	cfg.StateDir = t.TempDir()
	srv := newTestServerForStartContext(t, cfg)

	bare := initBareRepoWithCommit(t)
	gc := &api.GitCloneConfig{URL: bare, Branch: "main"}

	projectPath := filepath.Join(t.TempDir(), "proj")
	if err := os.MkdirAll(projectPath, 0o755); err != nil {
		t.Fatal(err)
	}

	in := startContextInputs{
		Name:          "agent-a",
		AgentID:       "agent-a",
		ProjectID:     "p1",
		ProjectSlug:   "proj",
		ProjectPath:   projectPath,
		WorkspaceMode: store.WorkspaceModeWorktreePerAgent,
		Config:        &CreateAgentConfig{GitClone: gc},
		Operation:     opHTTPStart,
	}

	sc1, err := srv.buildStartContext(context.Background(), in)
	if err != nil {
		t.Fatalf("first buildStartContext failed: %v", err)
	}
	worktreePath := sc1.Opts.Workspace
	if worktreePath == "" {
		t.Fatal("expected a worktree Workspace path to be set")
	}

	const unpushedContent = "package main // un-pushed change\n"
	unpushedFile := filepath.Join(worktreePath, "unpushed.go")
	if err := os.WriteFile(unpushedFile, []byte(unpushedContent), 0644); err != nil {
		t.Fatal(err)
	}

	// Remove both markers ProvisionShared checks for "already provisioned":
	// the sentinel file (in the project root, the parent of the shared
	// "workspace" dir) and the shared base's own .git.
	base := filepath.Dir(filepath.Dir(worktreePath)) // .../workspace
	sentinelPath := filepath.Join(filepath.Dir(base), provision.ProvisionSentinelFile)
	if err := os.Remove(sentinelPath); err != nil {
		t.Fatalf("failed to remove sentinel for fault injection: %v", err)
	}
	if err := os.RemoveAll(filepath.Join(base, ".git")); err != nil {
		t.Fatalf("failed to remove base .git for fault injection: %v", err)
	}

	// Start again: must fail closed, not self-heal by wiping the base.
	_, err = srv.buildStartContext(context.Background(), in)
	if err == nil {
		t.Fatal("expected buildStartContext to fail when the sentinel and base .git are both missing")
	}

	// The worktree and its un-pushed file must survive: ProvisionShared's
	// clone step on the shared base must never have run.
	if _, statErr := os.Stat(worktreePath); statErr != nil {
		t.Errorf("expected the existing worktree to survive, stat error: %v", statErr)
	}
	got, readErr := os.ReadFile(unpushedFile)
	if readErr != nil {
		t.Fatalf("un-pushed file must survive, but reading it failed: %v", readErr)
	}
	if string(got) != unpushedContent {
		t.Errorf("un-pushed file content = %q, want %q", got, unpushedContent)
	}
}

// TestBuildStartContext_WorktreePerAgentStart_SingleMissingMarkerFailsClosed
// pins worktreeBaseIsProvisioned's contract for each marker independently:
// removing only the sentinel, or only the base's .git, must each alone
// still fail the start closed. The joint test above (removing both) does
// not distinguish which check did the work.
func TestBuildStartContext_WorktreePerAgentStart_SingleMissingMarkerFailsClosed(t *testing.T) {
	requireWorktreeGit(t)

	for _, tc := range []struct {
		name           string
		removeSentinel bool
		removeGit      bool
	}{
		{name: "sentinel only missing", removeSentinel: true},
		{name: "base .git only missing", removeGit: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := DefaultServerConfig()
			cfg.StateDir = t.TempDir()
			srv := newTestServerForStartContext(t, cfg)

			bare := initBareRepoWithCommit(t)
			gc := &api.GitCloneConfig{URL: bare, Branch: "main"}

			projectPath := filepath.Join(t.TempDir(), "proj")
			if err := os.MkdirAll(projectPath, 0o755); err != nil {
				t.Fatal(err)
			}

			in := startContextInputs{
				Name:          "agent-a",
				AgentID:       "agent-a",
				ProjectID:     "p1",
				ProjectSlug:   "proj",
				ProjectPath:   projectPath,
				WorkspaceMode: store.WorkspaceModeWorktreePerAgent,
				Config:        &CreateAgentConfig{GitClone: gc},
				Operation:     opHTTPStart,
			}

			sc1, err := srv.buildStartContext(context.Background(), in)
			if err != nil {
				t.Fatalf("first buildStartContext failed: %v", err)
			}
			worktreePath := sc1.Opts.Workspace
			if worktreePath == "" {
				t.Fatal("expected a worktree Workspace path to be set")
			}

			const unpushedContent = "package main // un-pushed change\n"
			unpushedFile := filepath.Join(worktreePath, "unpushed.go")
			if err := os.WriteFile(unpushedFile, []byte(unpushedContent), 0644); err != nil {
				t.Fatal(err)
			}

			base := filepath.Dir(filepath.Dir(worktreePath)) // .../workspace
			if tc.removeSentinel {
				sentinelPath := filepath.Join(filepath.Dir(base), provision.ProvisionSentinelFile)
				if err := os.Remove(sentinelPath); err != nil {
					t.Fatalf("failed to remove sentinel for fault injection: %v", err)
				}
			}
			if tc.removeGit {
				if err := os.RemoveAll(filepath.Join(base, ".git")); err != nil {
					t.Fatalf("failed to remove base .git for fault injection: %v", err)
				}
			}

			_, err = srv.buildStartContext(context.Background(), in)
			if err == nil {
				t.Fatalf("expected buildStartContext to fail when %s", tc.name)
			}

			if _, statErr := os.Stat(worktreePath); statErr != nil {
				t.Errorf("expected the existing worktree to survive, stat error: %v", statErr)
			}
			got, readErr := os.ReadFile(unpushedFile)
			if readErr != nil {
				t.Fatalf("un-pushed file must survive, but reading it failed: %v", readErr)
			}
			if string(got) != unpushedContent {
				t.Errorf("un-pushed file content = %q, want %q", got, unpushedContent)
			}
		})
	}
}

// TestBuildStartContext_WorktreePerAgentCreate_ProvisioningFailureCleansUpPartial
// proves create's own partial-worktree cleanup on a provisioning failure is
// unchanged: when the agent's worktree does not exist yet (a fresh create),
// a failure still removes only what this call created.
func TestBuildStartContext_WorktreePerAgentCreate_ProvisioningFailureCleansUpPartial(t *testing.T) {
	requireWorktreeGit(t)
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory permissions, so the read-only sharer dir fault injection below never fails")
	}

	cfg := DefaultServerConfig()
	cfg.StateDir = t.TempDir()
	srv := newTestServerForStartContext(t, cfg)

	bare := initBareRepoWithCommit(t)
	gc := &api.GitCloneConfig{URL: bare, Branch: "main"}

	projectPath := filepath.Join(t.TempDir(), "proj")
	if err := os.MkdirAll(projectPath, 0o755); err != nil {
		t.Fatal(err)
	}

	// Force RegisterSharer to fail on this agent's very first provision, by
	// pre-creating a plain file where the scion-sharers directory must go.
	// The shared base clone doesn't exist yet, so create it via the resolve
	// path first: run the same resolution buildStartContext uses.
	resolved, err := runtime.NewLocalBackend().Resolve(runtime.ResolveInput{
		ProjectDir: projectPath, ProjectID: "p1", AgentID: "agent-a",
		Mode: store.SharingModeWorktreePerAgent,
	})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	base := resolved.HostPath
	if err := os.MkdirAll(filepath.Dir(filepath.Join(base, ".git")), 0o755); err != nil {
		t.Fatal(err)
	}
	// Clone the shared base ourselves so we control it before provisioning.
	cloneCmd := exec.Command("git", "clone", bare, base)
	if out, cloneErr := cloneCmd.CombinedOutput(); cloneErr != nil {
		t.Fatalf("git clone: %v: %s", cloneErr, out)
	}
	// Pre-create the marker directory read-only, so ListSharers (a read of a
	// not-yet-existing file, which is not an error) still lets git worktree
	// add succeed, but the subsequent RegisterSharer's write into this same
	// directory fails — reproducing a failure that happens only after this
	// call has already created the worktree on disk.
	sharerDir := filepath.Join(base, ".git", "scion-sharers")
	if err := os.MkdirAll(sharerDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(sharerDir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(sharerDir, 0o755) })

	in := startContextInputs{
		Name:          "agent-a",
		AgentID:       "agent-a",
		ProjectID:     "p1",
		ProjectSlug:   "proj",
		ProjectPath:   projectPath,
		WorkspaceMode: store.WorkspaceModeWorktreePerAgent,
		Config:        &CreateAgentConfig{GitClone: gc},
		Operation:     opCreate,
	}

	sc, err := srv.buildStartContext(context.Background(), in)
	// The worktree provisioning failure falls back to clone-per-agent (a
	// fresh agent has no existing worktree to protect), so the overall
	// request still succeeds — it is not the same failure mode as the
	// existing-worktree case above.
	if err != nil {
		t.Fatalf("buildStartContext should fall back to clone-per-agent, not fail outright: %v", err)
	}
	if sc.Opts.GitClone == nil {
		t.Error("expected fallback to in-container clone mode (GitClone set) when worktree provisioning fails on a fresh create")
	}

	// This call's own partial worktree must have been cleaned up.
	worktreePath := filepath.Join(base, "worktrees", "agent-a")
	if _, statErr := os.Stat(worktreePath); !os.IsNotExist(statErr) {
		t.Errorf("expected the partial worktree created by this call to be cleaned up, stat error: %v", statErr)
	}
}

// TestResolveActualWorkspace_RejectsPathFailingRelationshipValidation covers
// acceptance criterion 2 (hub read path): resolveActualWorkspace — the
// helper tryProvisionWorktree calls to pick the mounted workspace after
// ProvisionShared has already run — must not take the registry's recorded
// path at face value. A registered path that is lexically in-tree-shaped
// (passes the registry read boundary) but is not a genuine worktree (an
// in-tree decoy directory, no real git admin metadata) must be rejected by
// the full relationship check, falling back to the agent's own freshly-
// provisioned path instead.
func TestResolveActualWorkspace_RejectsPathFailingRelationshipValidation(t *testing.T) {
	t.Setenv("SCION_HOST_UID", "")

	base := t.TempDir()
	if err := os.MkdirAll(filepath.Join(base, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	branch := "shared-branch"

	// Register a decoy directly: lexically in-tree (base/worktrees/decoy,
	// passing the registry read-boundary shape check) but backed by nothing —
	// no real git worktree was ever created there. A brand-new branch with no
	// prior registration is used so RegisterSharer's write-side immutability
	// (see its doc comment) doesn't apply here; that protection is exercised
	// elsewhere (pkg/provision's registry tests) and isn't what this test is about.
	decoy := provision.WorktreePath(base, "decoy")
	if err := provision.RegisterSharer(base, "", branch, decoy, "agent-c"); err != nil {
		t.Fatalf("RegisterSharer: %v", err)
	}

	fallback := provision.WorktreePath(base, "agent-x")
	got := resolveActualWorkspace(base, branch, fallback, "agent-x")
	if got != fallback {
		t.Errorf("resolveActualWorkspace = %q, want fallback %q (decoy must be rejected)", got, fallback)
	}
	if got == decoy {
		t.Error("resolveActualWorkspace must never return the unvalidated decoy path")
	}
}

// TestWorktreeWorkspace_RepoRootDerivesToBase validates that the container
// dual-mount inputs resolve correctly for the worktree layout WITHOUT any
// explicit opts.RepoRoot (api.StartOptions has no such field). pkg/agent/run.go
// derives repoRoot from the workspace itself: IsGitRepoDir(worktree) is true
// (git rev-parse --is-inside-work-tree works through the worktree .git pointer
// file), and GetCommonGitDir(worktree) returns the SHARED base .git, so
// repoRoot = filepath.Dir(commonDir) == the base checkout. The worktree then
// sits at <base>/worktrees/<id>, giving a non-".." relative path that triggers
// common.go's .git + worktree dual-mount. (Regression guard for the #350 review
// claim that opts.RepoRoot must be set explicitly.)
func TestWorktreeWorkspace_RepoRootDerivesToBase(t *testing.T) {
	t.Setenv("SCION_HOST_UID", "")

	bare := initBareRepoWithCommit(t)
	gc := &api.GitCloneConfig{URL: bare, Branch: "main"}

	projectPath := filepath.Join(t.TempDir(), "proj")
	if err := os.MkdirAll(projectPath, 0o755); err != nil {
		t.Fatal(err)
	}

	resolved, err := runtime.NewLocalBackend().Resolve(runtime.ResolveInput{
		ProjectDir: projectPath, ProjectID: "p1", AgentID: "agent-a",
		Mode: store.SharingModeWorktreePerAgent,
	})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if err := provision.ProvisionShared(provision.ProvisionInput{
		Resolved: resolved, Mode: store.SharingModeWorktreePerAgent,
		ProjectID: "p1", AgentID: "agent-a", AgentName: "agent-a", GitClone: gc,
	}); err != nil {
		t.Fatalf("provision: %v", err)
	}

	base := resolved.HostPath // <projectPath>/workspace — the shared base checkout
	worktree := provision.WorktreePath(base, "agent-a")

	// Replicate pkg/agent/run.go's repoRoot derivation from the workspace.
	if !util.IsGitRepoDir(worktree) {
		t.Fatal("IsGitRepoDir(worktree) = false; run.go would not derive repoRoot from the worktree")
	}
	commonDir, err := util.GetCommonGitDir(worktree)
	if err != nil {
		t.Fatalf("GetCommonGitDir(worktree): %v", err)
	}
	repoRoot := filepath.Dir(commonDir)
	if repoRoot != base {
		t.Errorf("derived repoRoot = %q, want base %q", repoRoot, base)
	}

	// The dual-mount in common.go only fires when rel(repoRoot, workspace) is a
	// non-".." subpath — confirm the worktree is nested inside the base.
	rel, err := filepath.Rel(repoRoot, worktree)
	if err != nil {
		t.Fatalf("Rel: %v", err)
	}
	if rel != filepath.Join("worktrees", "agent-a") {
		t.Errorf("rel(repoRoot, worktree) = %q, want %q", rel, filepath.Join("worktrees", "agent-a"))
	}
	if strings.HasPrefix(rel, "..") {
		t.Errorf("rel %q starts with .. — common.go dual-mount would NOT fire", rel)
	}
}

// worktreeBaseIsProvisioned checks exactly the directories ProvisionShared
// checks (ProvisionInput.SentinelDirs): the base's parent by default, plus
// LegacyDir when set. It still requires the base's .git.
func TestWorktreeBaseIsProvisioned_SentinelLocations(t *testing.T) {
	for _, tc := range []struct {
		name      string
		dirOf     func(base string) string
		legacyDir bool
		want      bool
	}{
		{name: "parent", dirOf: filepath.Dir, want: true},
		{name: "state dir not checked by ProvisionShared", dirOf: provision.ProjectStateDir, want: false},
		{name: "base without LegacyDir", dirOf: func(base string) string { return base }, want: false},
		{name: "base as LegacyDir", dirOf: func(base string) string { return base }, legacyDir: true, want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base := filepath.Join(t.TempDir(), "workspace")
			if err := os.MkdirAll(filepath.Join(base, ".git"), 0o755); err != nil {
				t.Fatal(err)
			}
			in := provision.ProvisionInput{Resolved: provision.ResolvedWorkspace{HostPath: base}}
			if tc.legacyDir {
				in.LegacyDir = base
			}
			if worktreeBaseIsProvisioned(in) {
				t.Fatal("provisioned without a sentinel")
			}
			dir := tc.dirOf(base)
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, provision.ProvisionSentinelFile), []byte("x"), 0o644); err != nil {
				t.Fatal(err)
			}
			if got := worktreeBaseIsProvisioned(in); got != tc.want {
				t.Fatalf("sentinel in %s: got %v, want %v", dir, got, tc.want)
			}
			if !tc.want {
				return
			}
			if err := os.RemoveAll(filepath.Join(base, ".git")); err != nil {
				t.Fatal(err)
			}
			if worktreeBaseIsProvisioned(in) {
				t.Error("provisioned without the base's .git")
			}
		})
	}
}
