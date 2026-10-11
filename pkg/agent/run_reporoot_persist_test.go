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
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
	"github.com/GoogleCloudPlatform/scion/pkg/util"
)

// TestStartUserWorkspaceOverrideYieldsEmptyRepoRoot is the required
// counterpart to the broker-provisioned-worktree RepoRoot stitching fix: a
// user-supplied --workspace (opts.Workspace set with no
// api.ContextWithProvisionedWorktreeRepoRoot signal on ctx) must still
// produce an empty RunConfig.RepoRoot, exactly like before the fix — even
// when the workspace happens to sit inside a git repo, the case
// detectRepoRoot's explicit-workspace skip exists for in the first place.
func TestStartUserWorkspaceOverrideYieldsEmptyRepoRoot(t *testing.T) {
	tmpDir := t.TempDir()
	projectScionDir := startRepoRootProjectScaffold(t, tmpDir)

	// The operator's own workspace: a real git repo, so the "explicit
	// workspace skips git detection" guarantee is actually exercised, not
	// vacuously true because there was no repo to detect.
	userWorkspace := filepath.Join(tmpDir, "operators-own-repo")
	if err := os.MkdirAll(userWorkspace, 0755); err != nil {
		t.Fatalf("failed to create user workspace dir: %v", err)
	}
	setupGitRepo(t, userWorkspace)

	var capturedConfig runtime.RunConfig
	mockRT := &runtime.MockRuntime{
		ListFunc: func(ctx context.Context, labelFilter map[string]string) ([]api.AgentInfo, error) {
			return []api.AgentInfo{}, nil
		},
		RunFunc: func(ctx context.Context, config runtime.RunConfig) (string, error) {
			capturedConfig = config
			return "mock-id", nil
		},
	}

	mgr := NewManager(mockRT)
	// context.Background(): no api.ContextWithProvisionedWorktreeRepoRoot
	// signal — this is the plain CLI/local dispatch shape for a user
	// --workspace flag.
	if _, err := mgr.Start(context.Background(), api.StartOptions{
		Name:        "test-agent",
		ProjectPath: projectScionDir,
		NoAuth:      true,
		Workspace:   userWorkspace,
		Env: map[string]string{
			"SCION_AGENT_ID":   "agent-456",
			"SCION_PROJECT_ID": "proj-123",
		},
	}); err != nil {
		t.Fatalf("Start failed: %v", err)
	}

	if capturedConfig.RepoRoot != "" {
		t.Fatalf("RunConfig.RepoRoot = %q, want \"\" for a user --workspace override", capturedConfig.RepoRoot)
	}
	if capturedConfig.Workspace != userWorkspace {
		t.Fatalf("RunConfig.Workspace = %q, want %q", capturedConfig.Workspace, userWorkspace)
	}
}

// TestStartPersistsFreshProvisionedWorktreeRepoRootWhenProvisionAgentIsSkipped
// covers GetAgent skipping ProvisionAgent entirely once an agent directory
// already exists on disk (e.g. a leftover from a deleted hub agent recreated
// under the same name), so a fresh ctx signal on that dispatch would
// otherwise never be persisted — stranding RepoRoot on the very next resume,
// which has no ctx signal of its own. Start must persist the fresh value
// itself whenever it validates and differs from what's already on disk,
// independent of whether ProvisionAgent ran.
func TestStartPersistsFreshProvisionedWorktreeRepoRootWhenProvisionAgentIsSkipped(t *testing.T) {
	t.Setenv("SCION_HOST_UID", "")
	tmpDir := t.TempDir()
	projectScionDir := startRepoRootProjectScaffold(t, tmpDir)

	// The workspace must be a REAL worktree of sharedBase (not just a plain
	// directory) — Start only persists a ctx signal that actually validates,
	// and validation requires a genuine git worktree relationship, not just
	// a matching directory shape.
	sharedBase := t.TempDir()
	setupGitRepo(t, sharedBase)
	worktreesDir := filepath.Join(sharedBase, "worktrees")
	if err := os.MkdirAll(worktreesDir, 0755); err != nil {
		t.Fatalf("failed to create worktrees dir: %v", err)
	}
	userWorkspace := filepath.Join(worktreesDir, "agent-a")
	if err := util.CreateWorktree(userWorkspace, "agent-a"); err != nil {
		t.Fatalf("failed to create real worktree: %v", err)
	}

	mockRT := &runtime.MockRuntime{
		ListFunc: func(ctx context.Context, labelFilter map[string]string) ([]api.AgentInfo, error) {
			return []api.AgentInfo{}, nil
		},
		RunFunc: func(ctx context.Context, config runtime.RunConfig) (string, error) {
			return "mock-id", nil
		},
	}
	mgr := NewManager(mockRT)
	env := map[string]string{"SCION_AGENT_ID": "agent-a", "SCION_PROJECT_ID": "proj-123"}

	// Step 1: create the agent normally as a plain --workspace agent (no ctx
	// signal) — the exact provisioning shape doesn't matter here, only that
	// the agent directory now exists on disk with no repo root persisted.
	if _, err := mgr.Start(context.Background(), api.StartOptions{
		Name: "agent-a", ProjectPath: projectScionDir, NoAuth: true, Workspace: userWorkspace, Env: env,
	}); err != nil {
		t.Fatalf("initial Start failed: %v", err)
	}

	// Step 2: a later dispatch for the SAME agent name whose ctx carries a
	// fresh broker-provisioned-worktree signal (the real relationship: user
	// Workspace is genuinely sharedBase's worktree). Because the agent
	// directory already exists, GetAgent's "agent dir exists" branch skips
	// ProvisionAgent entirely — the only way this value can reach disk is the
	// persistence added to Start for exactly this case.
	ctx := api.ContextWithProvisionedWorktreeRepoRoot(context.Background(), sharedBase)
	if _, err := mgr.Start(ctx, api.StartOptions{
		Name: "agent-a", ProjectPath: projectScionDir, NoAuth: true, Workspace: userWorkspace, Env: env,
	}); err != nil {
		t.Fatalf("second Start failed: %v", err)
	}

	agentDir := config.GetAgentDir(projectScionDir, "agent-a", false)
	gotRoot, err := filepath.EvalSymlinks(readProvisionedWorktreeRepoRoot(agentDir))
	if err != nil {
		t.Fatalf("EvalSymlinks(persisted repo root): %v", err)
	}
	wantRoot, err := filepath.EvalSymlinks(sharedBase)
	if err != nil {
		t.Fatalf("EvalSymlinks(sharedBase): %v", err)
	}
	if gotRoot != wantRoot {
		t.Fatalf("persisted repo root = %q, want %q — the fresh ctx signal was not persisted when ProvisionAgent was skipped", gotRoot, wantRoot)
	}
}

// TestStartPersistsFreshProvisionedWorktreeRepoRootOnSymlinkedBrokerPath is
// TestStartPersistsFreshProvisionedWorktreeRepoRootWhenProvisionAgentIsSkipped's
// sibling for a broker project path that runs through a symlinked ancestor.
// ValidateWorkspaceSource always returns effectiveWorkspace fully resolved,
// so the persistence gate's ctxRepoRoot argument (passed through unresolved,
// exactly as the broker constructs it) and effectiveWorkspace (resolved) can
// name the same real worktree while disagreeing lexically. Before the fix,
// persistProvisionedWorktreeRepoRootIfValid compared them as given and
// silently discarded a value that was actually correct, reproducing the
// original empty-RepoRoot-on-resume bug on any host where the project path
// runs through a symlink.
func TestStartPersistsFreshProvisionedWorktreeRepoRootOnSymlinkedBrokerPath(t *testing.T) {
	t.Setenv("SCION_HOST_UID", "")
	tmpDir := t.TempDir()
	projectScionDir := startRepoRootProjectScaffold(t, tmpDir)

	// The broker-provisioned base sits behind a symlinked ancestor, exactly
	// like a broker project path whose parent directory is itself a symlink.
	realParent := t.TempDir()
	linkParent := t.TempDir()
	linkDir := filepath.Join(linkParent, "link")
	if err := os.Symlink(realParent, linkDir); err != nil {
		t.Fatal(err)
	}
	sharedBase := filepath.Join(linkDir, "shared-base")
	if err := os.MkdirAll(sharedBase, 0755); err != nil {
		t.Fatalf("failed to create shared base dir: %v", err)
	}
	setupGitRepo(t, sharedBase)
	worktreesDir := filepath.Join(sharedBase, "worktrees")
	if err := os.MkdirAll(worktreesDir, 0755); err != nil {
		t.Fatalf("failed to create worktrees dir: %v", err)
	}
	userWorkspace := filepath.Join(worktreesDir, "agent-a")
	if err := util.CreateWorktree(userWorkspace, "agent-a"); err != nil {
		t.Fatalf("failed to create real worktree: %v", err)
	}

	mockRT := &runtime.MockRuntime{
		ListFunc: func(ctx context.Context, labelFilter map[string]string) ([]api.AgentInfo, error) {
			return []api.AgentInfo{}, nil
		},
		RunFunc: func(ctx context.Context, config runtime.RunConfig) (string, error) {
			return "mock-id", nil
		},
	}
	mgr := NewManager(mockRT)
	env := map[string]string{"SCION_AGENT_ID": "agent-a", "SCION_PROJECT_ID": "proj-123"}

	// Step 1: create the agent normally as a plain --workspace agent (no ctx
	// signal) — the agent directory now exists on disk with no repo root
	// persisted.
	if _, err := mgr.Start(context.Background(), api.StartOptions{
		Name: "agent-a", ProjectPath: projectScionDir, NoAuth: true, Workspace: userWorkspace, Env: env,
	}); err != nil {
		t.Fatalf("initial Start failed: %v", err)
	}

	// Step 2: a later dispatch for the SAME agent whose ctx carries a fresh
	// broker-provisioned-worktree signal — the symlinked, unresolved
	// sharedBase, exactly as the broker would pass it. Because the agent
	// directory already exists, GetAgent's "agent dir exists" branch skips
	// ProvisionAgent entirely, so Start's own persistence call is the only
	// way this value can reach disk.
	ctx := api.ContextWithProvisionedWorktreeRepoRoot(context.Background(), sharedBase)
	if _, err := mgr.Start(ctx, api.StartOptions{
		Name: "agent-a", ProjectPath: projectScionDir, NoAuth: true, Workspace: userWorkspace, Env: env,
	}); err != nil {
		t.Fatalf("second Start failed: %v", err)
	}

	agentDir := config.GetAgentDir(projectScionDir, "agent-a", false)
	persisted := readProvisionedWorktreeRepoRoot(agentDir)
	if persisted == "" {
		t.Fatal("persisted repo root is empty — the fresh ctx signal was not persisted on a symlinked broker path")
	}
	gotRoot, err := filepath.EvalSymlinks(persisted)
	if err != nil {
		t.Fatalf("EvalSymlinks(persisted repo root): %v", err)
	}
	wantRoot, err := filepath.EvalSymlinks(sharedBase)
	if err != nil {
		t.Fatalf("EvalSymlinks(sharedBase): %v", err)
	}
	if gotRoot != wantRoot {
		t.Fatalf("persisted repo root = %q, want %q — the fresh ctx signal was not persisted on a symlinked broker path", gotRoot, wantRoot)
	}

	// Step 3: a plain resume dispatch (no ctx signal, no Workspace — exactly
	// what the hub sends on restart) must recover RepoRoot from the
	// persisted state alone, proving the persisted value is not just
	// non-empty but actually usable.
	var capturedConfig runtime.RunConfig
	mockRT.RunFunc = func(ctx context.Context, config runtime.RunConfig) (string, error) {
		capturedConfig = config
		return "mock-id", nil
	}
	if _, err := mgr.Start(context.Background(), api.StartOptions{
		Name: "agent-a", ProjectPath: projectScionDir, NoAuth: true, Workspace: "", Resume: true, Env: env,
	}); err != nil {
		t.Fatalf("resume Start failed: %v", err)
	}
	if capturedConfig.RepoRoot == "" {
		t.Fatal("resume RunConfig.RepoRoot is empty — the persisted repo root from a symlinked broker path did not survive resume")
	}
}

// TestStartRejectsSymlinkedWorktreeLeafPointingAtSiblingWorktree is the
// permanent regression test for why run.go's post-ValidateWorkspaceSource
// repo-root re-validation block was removed entirely, rather than kept or
// patched: that block fed an ALREADY-RESOLVED effectiveWorkspace into
// validatedWorktreeRepoRoot, which
// erases exactly the lexical-vs-resolved name mismatch
// provision.ValidateWorktreeForBase depends on to catch a worktree leaf that
// is itself a symlink to a sibling worktree. Fed the ORIGINAL (unresolved)
// pair, as Start's first validation pass and detectRepoRoot's fallback both
// do, the mismatch is caught and the dispatch falls back to a plain
// /workspace mount instead of trusting a candidate repo root it must not —
// mounting that root's .git read-write would give the container the shared
// base, not just the one sibling worktree it was ever entitled to.
func TestStartRejectsSymlinkedWorktreeLeafPointingAtSiblingWorktree(t *testing.T) {
	t.Setenv("SCION_HOST_UID", "")
	tmpDir := t.TempDir()
	projectScionDir := startRepoRootProjectScaffold(t, tmpDir)

	sharedBase := t.TempDir()
	setupGitRepo(t, sharedBase)
	worktreesDir := filepath.Join(sharedBase, "worktrees")
	if err := os.MkdirAll(worktreesDir, 0755); err != nil {
		t.Fatalf("failed to create worktrees dir: %v", err)
	}

	// agent-b's worktree is genuine.
	siblingWorktree := filepath.Join(worktreesDir, "agent-b")
	if err := util.CreateWorktree(siblingWorktree, "agent-b"); err != nil {
		t.Fatalf("failed to create sibling worktree: %v", err)
	}

	// agent-a has no real worktree of its own: the path scion would expect
	// to find it at is instead a symlink to agent-b's — a leaf that
	// lexically looks like this agent's own worktree but resolves to a
	// different agent's.
	leafPath := filepath.Join(worktreesDir, "agent-a")
	if err := os.Symlink(siblingWorktree, leafPath); err != nil {
		t.Fatal(err)
	}

	var capturedConfig runtime.RunConfig
	mockRT := &runtime.MockRuntime{
		ListFunc: func(ctx context.Context, labelFilter map[string]string) ([]api.AgentInfo, error) {
			return []api.AgentInfo{}, nil
		},
		RunFunc: func(ctx context.Context, config runtime.RunConfig) (string, error) {
			capturedConfig = config
			return "mock-id", nil
		},
	}
	mgr := NewManager(mockRT)
	env := map[string]string{"SCION_AGENT_ID": "agent-a", "SCION_PROJECT_ID": "proj-123"}

	ctx := api.ContextWithProvisionedWorktreeRepoRoot(context.Background(), sharedBase)
	if _, err := mgr.Start(ctx, api.StartOptions{
		Name: "agent-a", ProjectPath: projectScionDir, NoAuth: true, Workspace: leafPath, Env: env,
	}); err != nil {
		t.Fatalf("Start failed: %v", err)
	}

	if capturedConfig.RepoRoot != "" {
		t.Fatalf("RunConfig.RepoRoot = %q, want empty — a worktree leaf symlinked to a sibling worktree must never validate sharedBase as this agent's repo root", capturedConfig.RepoRoot)
	}
	if capturedConfig.ContainerWorkspace != "/workspace" {
		t.Fatalf("RunConfig.ContainerWorkspace = %q, want %q (the plain mount fallback, not the worktree dual-mount branch that would expose sharedBase's .git)", capturedConfig.ContainerWorkspace, "/workspace")
	}
}

// TestStartDoesNotPersistUnvalidatedCtxRepoRoot covers the persistence path:
// it must only ever write a repo root that actually validated (repoRoot ==
// ctxRepoRoot), never a bare ctx value that the validator rejected and
// detectRepoRoot then fell back past. Otherwise a bad value could reach disk
// even though it never reached RunConfig on the dispatch that produced it.
func TestStartDoesNotPersistUnvalidatedCtxRepoRoot(t *testing.T) {
	tmpDir := t.TempDir()
	projectScionDir := startRepoRootProjectScaffold(t, tmpDir)

	// ctxRoot is a real git repo, but it has no relationship at all to
	// userWorkspace (a plain, unrelated directory) — the validator must
	// reject this pairing.
	ctxRoot := t.TempDir()
	setupGitRepo(t, ctxRoot)
	userWorkspace := filepath.Join(tmpDir, "operators-own-dir")
	if err := os.MkdirAll(userWorkspace, 0755); err != nil {
		t.Fatalf("failed to create user workspace dir: %v", err)
	}

	mockRT := &runtime.MockRuntime{
		ListFunc: func(ctx context.Context, labelFilter map[string]string) ([]api.AgentInfo, error) {
			return []api.AgentInfo{}, nil
		},
		RunFunc: func(ctx context.Context, config runtime.RunConfig) (string, error) {
			return "mock-id", nil
		},
	}
	mgr := NewManager(mockRT)

	ctx := api.ContextWithProvisionedWorktreeRepoRoot(context.Background(), ctxRoot)
	if _, err := mgr.Start(ctx, api.StartOptions{
		Name:        "agent-a",
		ProjectPath: projectScionDir,
		NoAuth:      true,
		Workspace:   userWorkspace,
		Env:         map[string]string{"SCION_AGENT_ID": "agent-a", "SCION_PROJECT_ID": "proj-123"},
	}); err != nil {
		t.Fatalf("Start failed: %v", err)
	}

	agentDir := config.GetAgentDir(projectScionDir, "agent-a", false)
	if got := readProvisionedWorktreeRepoRoot(agentDir); got != "" {
		t.Fatalf("persisted repo root = %q, want \"\" — an unvalidated ctx value must never be persisted", got)
	}
}

// TestStartPersistsNothingWhenWorkspaceLeafResolvesOutsideItsOwnWorktree
// covers persistProvisionedWorktreeRepoRootIfValid's own comparison
// directly: the workspace value it receives must be compared in its
// original, as-given form, not a form already resolved ahead of the call.
// Resolving ahead of time would make this comparison accept a workspace leaf
// whose own final path element resolves to a different worktree name than
// the one it lexically presents — the same shape covered for
// RunConfig.RepoRoot's own, separate comparison.
func TestStartPersistsNothingWhenWorkspaceLeafResolvesOutsideItsOwnWorktree(t *testing.T) {
	t.Setenv("SCION_HOST_UID", "")
	tmpDir := t.TempDir()
	projectScionDir := startRepoRootProjectScaffold(t, tmpDir)

	sharedBase := t.TempDir()
	setupGitRepo(t, sharedBase)
	worktreesDir := filepath.Join(sharedBase, "worktrees")
	if err := os.MkdirAll(worktreesDir, 0755); err != nil {
		t.Fatalf("failed to create worktrees dir: %v", err)
	}
	otherWorktree := filepath.Join(worktreesDir, "agent-b")
	if err := util.CreateWorktree(otherWorktree, "agent-b"); err != nil {
		t.Fatalf("failed to create other worktree: %v", err)
	}
	// agent-a's own expected path is a symlink to agent-b's worktree rather
	// than a worktree of its own.
	leafPath := filepath.Join(worktreesDir, "agent-a")
	if err := os.Symlink(otherWorktree, leafPath); err != nil {
		t.Fatal(err)
	}

	mockRT := &runtime.MockRuntime{
		ListFunc: func(ctx context.Context, labelFilter map[string]string) ([]api.AgentInfo, error) {
			return []api.AgentInfo{}, nil
		},
		RunFunc: func(ctx context.Context, config runtime.RunConfig) (string, error) {
			return "mock-id", nil
		},
	}
	mgr := NewManager(mockRT)

	ctx := api.ContextWithProvisionedWorktreeRepoRoot(context.Background(), sharedBase)
	if _, err := mgr.Start(ctx, api.StartOptions{
		Name:        "agent-a",
		ProjectPath: projectScionDir,
		NoAuth:      true,
		Workspace:   leafPath,
		Env:         map[string]string{"SCION_AGENT_ID": "agent-a", "SCION_PROJECT_ID": "proj-123"},
	}); err != nil {
		t.Fatalf("Start failed: %v", err)
	}

	agentDir := config.GetAgentDir(projectScionDir, "agent-a", false)
	if got := readProvisionedWorktreeRepoRoot(agentDir); got != "" {
		t.Fatalf("persisted repo root = %q, want \"\"", got)
	}
	if _, err := os.Stat(filepath.Join(agentDir, provisionedWorktreeStateFile)); !os.IsNotExist(err) {
		t.Fatalf("expected no persisted-worktree state file to exist, stat error = %v", err)
	}
}

// TestStartPersistsTheAliasNotTheResolvedBase covers the other direction of
// the same comparison: when the ctx-provided repo root itself is reached
// through an ancestor symlink, and the workspace value given to this same
// dispatch is reached through the identical, as-given ancestor spelling, the
// comparison accepts the pair and persists the value exactly as given — not
// a form resolved ahead of persisting it.
func TestStartPersistsTheAliasNotTheResolvedBase(t *testing.T) {
	t.Setenv("SCION_HOST_UID", "")
	tmpDir := t.TempDir()
	projectScionDir := startRepoRootProjectScaffold(t, tmpDir)

	realParent := t.TempDir()
	linkParent := t.TempDir()
	linkDir := filepath.Join(linkParent, "link")
	if err := os.Symlink(realParent, linkDir); err != nil {
		t.Fatal(err)
	}
	sharedBase := filepath.Join(linkDir, "shared-base")
	if err := os.MkdirAll(sharedBase, 0755); err != nil {
		t.Fatalf("failed to create shared base dir: %v", err)
	}
	setupGitRepo(t, sharedBase)
	worktreesDir := filepath.Join(sharedBase, "worktrees")
	if err := os.MkdirAll(worktreesDir, 0755); err != nil {
		t.Fatalf("failed to create worktrees dir: %v", err)
	}
	userWorkspace := filepath.Join(worktreesDir, "agent-a")
	if err := util.CreateWorktree(userWorkspace, "agent-a"); err != nil {
		t.Fatalf("failed to create real worktree: %v", err)
	}

	mockRT := &runtime.MockRuntime{
		ListFunc: func(ctx context.Context, labelFilter map[string]string) ([]api.AgentInfo, error) {
			return []api.AgentInfo{}, nil
		},
		RunFunc: func(ctx context.Context, config runtime.RunConfig) (string, error) {
			return "mock-id", nil
		},
	}
	mgr := NewManager(mockRT)

	ctx := api.ContextWithProvisionedWorktreeRepoRoot(context.Background(), sharedBase)
	if _, err := mgr.Start(ctx, api.StartOptions{
		Name:        "agent-a",
		ProjectPath: projectScionDir,
		NoAuth:      true,
		Workspace:   userWorkspace,
		Env:         map[string]string{"SCION_AGENT_ID": "agent-a", "SCION_PROJECT_ID": "proj-123"},
	}); err != nil {
		t.Fatalf("Start failed: %v", err)
	}

	agentDir := config.GetAgentDir(projectScionDir, "agent-a", false)
	got := readProvisionedWorktreeRepoRoot(agentDir)
	if got != sharedBase {
		t.Fatalf("persisted repo root = %q, want %q (the value as given, not its resolved form)", got, sharedBase)
	}
	if resolvedBase, err := filepath.EvalSymlinks(sharedBase); err == nil && got == resolvedBase {
		t.Fatalf("persisted repo root = %q, must not be the resolved form %q", got, resolvedBase)
	}
}

// TestStartDoesNotPersistWhenRepoRootStaysEmpty ties persistProvisionedWorktreeRepoRootIfValid's
// decision to Start's own: whenever Start's own comparison for
// RunConfig.RepoRoot rejects a pair (leaving RepoRoot empty for this
// dispatch), the persistence gate must reject the identical pair too, rather
// than saving a value this same dispatch did not trust enough to use.
func TestStartDoesNotPersistWhenRepoRootStaysEmpty(t *testing.T) {
	t.Setenv("SCION_HOST_UID", "")
	tmpDir := t.TempDir()
	projectScionDir := startRepoRootProjectScaffold(t, tmpDir)

	sharedBase := t.TempDir()
	setupGitRepo(t, sharedBase)
	worktreesDir := filepath.Join(sharedBase, "worktrees")
	if err := os.MkdirAll(worktreesDir, 0755); err != nil {
		t.Fatalf("failed to create worktrees dir: %v", err)
	}
	otherWorktree := filepath.Join(worktreesDir, "agent-b")
	if err := util.CreateWorktree(otherWorktree, "agent-b"); err != nil {
		t.Fatalf("failed to create other worktree: %v", err)
	}
	leafPath := filepath.Join(worktreesDir, "agent-a")
	if err := os.Symlink(otherWorktree, leafPath); err != nil {
		t.Fatal(err)
	}

	var capturedConfig runtime.RunConfig
	mockRT := &runtime.MockRuntime{
		ListFunc: func(ctx context.Context, labelFilter map[string]string) ([]api.AgentInfo, error) {
			return []api.AgentInfo{}, nil
		},
		RunFunc: func(ctx context.Context, config runtime.RunConfig) (string, error) {
			capturedConfig = config
			return "mock-id", nil
		},
	}
	mgr := NewManager(mockRT)

	ctx := api.ContextWithProvisionedWorktreeRepoRoot(context.Background(), sharedBase)
	if _, err := mgr.Start(ctx, api.StartOptions{
		Name:        "agent-a",
		ProjectPath: projectScionDir,
		NoAuth:      true,
		Workspace:   leafPath,
		Env:         map[string]string{"SCION_AGENT_ID": "agent-a", "SCION_PROJECT_ID": "proj-123"},
	}); err != nil {
		t.Fatalf("Start failed: %v", err)
	}

	if capturedConfig.RepoRoot != "" {
		t.Fatalf("RunConfig.RepoRoot = %q, want empty for this pair", capturedConfig.RepoRoot)
	}
	agentDir := config.GetAgentDir(projectScionDir, "agent-a", false)
	if got := readProvisionedWorktreeRepoRoot(agentDir); got != "" {
		t.Fatalf("persisted repo root = %q, want \"\" — Start did not set RepoRoot for this pair, so it must not be persisted either", got)
	}
}

// TestStartResolvesRepoRootWhenPersistedRootIsSymlinkedAndWorkspaceIsResolved
// and the test following it cover run.go's own RunConfig.RepoRoot comparison
// (distinct from persistProvisionedWorktreeRepoRootIfValid's, covered above):
// the persisted repo root and the dispatch's own workspace value can each
// independently arrive already resolved or not, and a resolve-parent-only
// comparison must accept the pair either way while still telling apart a
// workspace leaf whose own final path element names something else.
// setPersistedWorkspaceVolumeSource rewrites the Source of agentDir's
// persisted /workspace volume mount in scion-agent.json, so a later Start
// dispatch's extractWorkspaceFromVolumes recovers exactly newSource rather
// than whatever spelling an earlier dispatch recorded.
func setPersistedWorkspaceVolumeSource(t *testing.T, agentDir, newSource string) {
	t.Helper()
	cfgPath := filepath.Join(agentDir, "scion-agent.json")
	data, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatalf("read %s: %v", cfgPath, err)
	}
	var cfg api.ScionConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("unmarshal %s: %v", cfgPath, err)
	}
	found := false
	for i := range cfg.Volumes {
		if cfg.Volumes[i].Target == "/workspace" {
			cfg.Volumes[i].Source = newSource
			found = true
		}
	}
	if !found {
		t.Fatalf("no /workspace volume found in %s to rewrite", cfgPath)
	}
	newData, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		t.Fatalf("marshal updated config: %v", err)
	}
	if err := os.WriteFile(cfgPath, newData, 0644); err != nil {
		t.Fatalf("write %s: %v", cfgPath, err)
	}
}

func TestStartResolvesRepoRootWhenPersistedRootIsSymlinkedAndWorkspaceIsResolved(t *testing.T) {
	t.Setenv("SCION_HOST_UID", "")
	tmpDir := t.TempDir()
	projectScionDir := startRepoRootProjectScaffold(t, tmpDir)

	realParent := t.TempDir()
	linkParent := t.TempDir()
	linkDir := filepath.Join(linkParent, "link")
	if err := os.Symlink(realParent, linkDir); err != nil {
		t.Fatal(err)
	}
	sharedBase := filepath.Join(linkDir, "shared-base")
	if err := os.MkdirAll(sharedBase, 0755); err != nil {
		t.Fatalf("failed to create shared base dir: %v", err)
	}
	setupGitRepo(t, sharedBase)
	worktreesDir := filepath.Join(sharedBase, "worktrees")
	if err := os.MkdirAll(worktreesDir, 0755); err != nil {
		t.Fatalf("failed to create worktrees dir: %v", err)
	}
	userWorkspace := filepath.Join(worktreesDir, "agent-a")
	if err := util.CreateWorktree(userWorkspace, "agent-a"); err != nil {
		t.Fatalf("failed to create real worktree: %v", err)
	}
	resolvedWorkspace, err := filepath.EvalSymlinks(userWorkspace)
	if err != nil {
		t.Fatalf("EvalSymlinks(userWorkspace): %v", err)
	}

	var capturedConfig runtime.RunConfig
	mockRT := &runtime.MockRuntime{
		ListFunc: func(ctx context.Context, labelFilter map[string]string) ([]api.AgentInfo, error) {
			return []api.AgentInfo{}, nil
		},
		RunFunc: func(ctx context.Context, config runtime.RunConfig) (string, error) {
			capturedConfig = config
			return "mock-id", nil
		},
	}
	mgr := NewManager(mockRT)
	env := map[string]string{"SCION_AGENT_ID": "agent-a", "SCION_PROJECT_ID": "proj-123"}

	// Materialize the agent directory, then set the persisted repo root
	// directly in its symlinked, as-given form, independent of whichever
	// path a prior dispatch would have used to get it there.
	if _, err := mgr.Start(context.Background(), api.StartOptions{
		Name: "agent-a", ProjectPath: projectScionDir, NoAuth: true, Workspace: userWorkspace, Env: env,
	}); err != nil {
		t.Fatalf("initial Start failed: %v", err)
	}
	agentDir := config.GetAgentDir(projectScionDir, "agent-a", false)
	if err := writeProvisionedWorktreeRepoRoot(agentDir, sharedBase); err != nil {
		t.Fatalf("writeProvisionedWorktreeRepoRoot: %v", err)
	}
	// An explicit-workspace agent recovers its workspace on every later
	// dispatch from the persisted /workspace volume, not from opts.Workspace
	// again — rewrite that persisted volume's source directly to the
	// resolved form, to arrange the "workspace already resolved" side of
	// this test independent of the initial dispatch's own spelling.
	setPersistedWorkspaceVolumeSource(t, agentDir, resolvedWorkspace)

	// This dispatch recovers the persisted, symlinked repo root with no
	// fresh ctx signal, and its own recovered workspace value is already
	// resolved.
	if _, err := mgr.Start(context.Background(), api.StartOptions{
		Name: "agent-a", ProjectPath: projectScionDir, NoAuth: true, Env: env,
	}); err != nil {
		t.Fatalf("second Start failed: %v", err)
	}

	if capturedConfig.RepoRoot == "" {
		t.Fatal("RunConfig.RepoRoot is empty")
	}
	gotRoot, err := filepath.EvalSymlinks(capturedConfig.RepoRoot)
	if err != nil {
		t.Fatalf("EvalSymlinks(RepoRoot): %v", err)
	}
	wantRoot, err := filepath.EvalSymlinks(sharedBase)
	if err != nil {
		t.Fatalf("EvalSymlinks(sharedBase): %v", err)
	}
	if gotRoot != wantRoot {
		t.Fatalf("RunConfig.RepoRoot resolves to %q, want %q", gotRoot, wantRoot)
	}
}

// TestStartResolvesRepoRootWhenPersistedRootIsResolvedAndWorkspaceIsSymlinked
// is the other order: the persisted repo root already arrives resolved,
// while this dispatch's own workspace value is symlinked.
func TestStartResolvesRepoRootWhenPersistedRootIsResolvedAndWorkspaceIsSymlinked(t *testing.T) {
	t.Setenv("SCION_HOST_UID", "")
	tmpDir := t.TempDir()
	projectScionDir := startRepoRootProjectScaffold(t, tmpDir)

	realParent := t.TempDir()
	linkParent := t.TempDir()
	linkDir := filepath.Join(linkParent, "link")
	if err := os.Symlink(realParent, linkDir); err != nil {
		t.Fatal(err)
	}
	sharedBase := filepath.Join(linkDir, "shared-base")
	if err := os.MkdirAll(sharedBase, 0755); err != nil {
		t.Fatalf("failed to create shared base dir: %v", err)
	}
	setupGitRepo(t, sharedBase)
	worktreesDir := filepath.Join(sharedBase, "worktrees")
	if err := os.MkdirAll(worktreesDir, 0755); err != nil {
		t.Fatalf("failed to create worktrees dir: %v", err)
	}
	userWorkspace := filepath.Join(worktreesDir, "agent-a")
	if err := util.CreateWorktree(userWorkspace, "agent-a"); err != nil {
		t.Fatalf("failed to create real worktree: %v", err)
	}
	resolvedBase, err := filepath.EvalSymlinks(sharedBase)
	if err != nil {
		t.Fatalf("EvalSymlinks(sharedBase): %v", err)
	}

	var capturedConfig runtime.RunConfig
	mockRT := &runtime.MockRuntime{
		ListFunc: func(ctx context.Context, labelFilter map[string]string) ([]api.AgentInfo, error) {
			return []api.AgentInfo{}, nil
		},
		RunFunc: func(ctx context.Context, config runtime.RunConfig) (string, error) {
			capturedConfig = config
			return "mock-id", nil
		},
	}
	mgr := NewManager(mockRT)
	env := map[string]string{"SCION_AGENT_ID": "agent-a", "SCION_PROJECT_ID": "proj-123"}

	if _, err := mgr.Start(context.Background(), api.StartOptions{
		Name: "agent-a", ProjectPath: projectScionDir, NoAuth: true, Workspace: userWorkspace, Env: env,
	}); err != nil {
		t.Fatalf("initial Start failed: %v", err)
	}
	agentDir := config.GetAgentDir(projectScionDir, "agent-a", false)
	// Set the persisted repo root directly in its resolved form, independent
	// of whichever path a prior dispatch would have used to get it there.
	if err := writeProvisionedWorktreeRepoRoot(agentDir, resolvedBase); err != nil {
		t.Fatalf("writeProvisionedWorktreeRepoRoot: %v", err)
	}

	// This dispatch recovers the persisted, resolved repo root with no fresh
	// ctx signal, but its own workspace value is symlinked.
	if _, err := mgr.Start(context.Background(), api.StartOptions{
		Name: "agent-a", ProjectPath: projectScionDir, NoAuth: true, Workspace: userWorkspace, Env: env,
	}); err != nil {
		t.Fatalf("second Start failed: %v", err)
	}

	if capturedConfig.RepoRoot == "" {
		t.Fatal("RunConfig.RepoRoot is empty")
	}
	gotRoot, err := filepath.EvalSymlinks(capturedConfig.RepoRoot)
	if err != nil {
		t.Fatalf("EvalSymlinks(RepoRoot): %v", err)
	}
	if gotRoot != resolvedBase {
		t.Fatalf("RunConfig.RepoRoot resolves to %q, want %q", gotRoot, resolvedBase)
	}
}

// repoRootMatrixShape builds one of three root spellings for the matrix
// test below: "plain" (no symlink anywhere), "ancestor-alias" (reached
// through a symlinked ancestor directory, with its own final path element a
// real directory), and "leaf-alias" (its own final path element is itself a
// symlink). Returns the root path to use and the worktrees directory under
// it (a real directory in all three cases).
func repoRootMatrixShape(t *testing.T, shape string) (root, worktreesDir string) {
	t.Helper()
	switch shape {
	case "plain":
		root = t.TempDir()
		setupGitRepo(t, root)
	case "ancestor-alias":
		realParent := t.TempDir()
		linkParent := t.TempDir()
		linkDir := filepath.Join(linkParent, "link")
		if err := os.Symlink(realParent, linkDir); err != nil {
			t.Fatal(err)
		}
		root = filepath.Join(linkDir, "base")
		if err := os.MkdirAll(root, 0755); err != nil {
			t.Fatal(err)
		}
		setupGitRepo(t, root)
	case "leaf-alias":
		realDisk := t.TempDir()
		projectDir := t.TempDir()
		root = filepath.Join(projectDir, "workspace")
		if err := os.Symlink(realDisk, root); err != nil {
			t.Fatal(err)
		}
		setupGitRepo(t, root)
	default:
		t.Fatalf("unknown root shape %q", shape)
	}
	worktreesDir = filepath.Join(root, "worktrees")
	if err := os.MkdirAll(worktreesDir, 0755); err != nil {
		t.Fatal(err)
	}
	return root, worktreesDir
}

// repoRootMatrixWorkspace builds one of three workspace spellings under
// worktreesDir for the matrix test below: "as-given" (a genuine worktree,
// spelled exactly as root's own shape implies), "resolved" (the identical
// genuine worktree, but with every symlink on the path to it already
// resolved), and "negative" (the target agent's own path is a symlink to a
// different, independently genuine worktree, rather than being a genuine
// worktree itself).
func repoRootMatrixWorkspace(t *testing.T, worktreesDir, shape string) string {
	t.Helper()
	asGiven := filepath.Join(worktreesDir, "agent-a")
	switch shape {
	case "as-given":
		if err := util.CreateWorktree(asGiven, "agent-a"); err != nil {
			t.Fatalf("failed to create worktree: %v", err)
		}
		return asGiven
	case "resolved":
		if err := util.CreateWorktree(asGiven, "agent-a"); err != nil {
			t.Fatalf("failed to create worktree: %v", err)
		}
		resolved, err := filepath.EvalSymlinks(asGiven)
		if err != nil {
			t.Fatalf("EvalSymlinks(%q): %v", asGiven, err)
		}
		return resolved
	case "negative":
		other := filepath.Join(worktreesDir, "agent-b")
		if err := util.CreateWorktree(other, "agent-b"); err != nil {
			t.Fatalf("failed to create worktree: %v", err)
		}
		if err := os.Symlink(other, asGiven); err != nil {
			t.Fatal(err)
		}
		return asGiven
	default:
		t.Fatalf("unknown workspace shape %q", shape)
		return ""
	}
}

// TestRepoRootComparisonMatrix is the required matrix covering every root
// spelling against every workspace spelling, for a first dispatch and for
// both resume orderings. Every case asserts both Start's RunConfig.RepoRoot
// and the value persistProvisionedWorktreeRepoRootIfValid writes, since the
// two share the same comparison and are required to reach the same outcome
// for it, whichever source supplies the pair (a fresh ctx signal or
// recovered state).
//
// Expected-value rule, the same across every root shape: when the workspace
// is a genuine worktree (as-given or resolved), RepoRoot resolves to the
// real root regardless of root's own spelling, and the persisted value is
// root exactly as it was given to this dispatch, never a resolved form.
// When the workspace is the negative shape (its own final path element
// resolves to a different worktree), RepoRoot is empty and nothing is
// persisted, regardless of root's own spelling.
//
// Resume orderings: order A recovers the workspace in the same spelling the
// root was persisted in; order B recovers the workspace fully resolved
// regardless of root's persisted spelling; order C is the reverse of B — the
// persisted root is fully resolved while the recovered workspace keeps its
// own as-given spelling. A further resume case, for every root shape, covers
// a value that validated and was persisted, but whose workspace path has
// since come to name a different, independently genuine worktree by the
// time of the resume dispatch: RepoRoot must be empty on that resume, not
// the stale, no-longer-current persisted value. Only the two
// worktree-genuine workspace shapes apply to the three orderings (nothing is
// ever persisted for the negative shape, so there is nothing to resume
// from).
func TestRepoRootComparisonMatrix(t *testing.T) {
	t.Setenv("SCION_HOST_UID", "")

	type matrixCase struct {
		name          string
		rootShape     string
		workspace     string // "first", "resumeA", "resumeB"
		workspaceKind string // "as-given", "resolved", "negative"
	}

	var cases []matrixCase
	for _, rootShape := range []string{"plain", "ancestor-alias", "leaf-alias"} {
		for _, wsKind := range []string{"as-given", "resolved", "negative"} {
			cases = append(cases, matrixCase{
				name:          rootShape + "/first-dispatch/" + wsKind,
				rootShape:     rootShape,
				workspace:     "first",
				workspaceKind: wsKind,
			})
		}
		cases = append(cases,
			matrixCase{name: rootShape + "/resume-order-A", rootShape: rootShape, workspace: "resumeA", workspaceKind: "as-given"},
			matrixCase{name: rootShape + "/resume-order-B", rootShape: rootShape, workspace: "resumeB", workspaceKind: "resolved"},
			matrixCase{name: rootShape + "/resume-leaf-replaced", rootShape: rootShape, workspace: "resumeLeafReplaced"},
			matrixCase{name: rootShape + "/resume-order-C", rootShape: rootShape, workspace: "resumeC", workspaceKind: "as-given"},
		)
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tmpDir := t.TempDir()
			projectScionDir := startRepoRootProjectScaffold(t, tmpDir)
			root, worktreesDir := repoRootMatrixShape(t, tc.rootShape)
			resolvedRoot, err := filepath.EvalSymlinks(root)
			if err != nil {
				t.Fatalf("EvalSymlinks(root): %v", err)
			}

			var capturedConfig runtime.RunConfig
			mockRT := &runtime.MockRuntime{
				ListFunc: func(ctx context.Context, labelFilter map[string]string) ([]api.AgentInfo, error) {
					return []api.AgentInfo{}, nil
				},
				RunFunc: func(ctx context.Context, config runtime.RunConfig) (string, error) {
					capturedConfig = config
					return "mock-id", nil
				},
			}
			mgr := NewManager(mockRT)
			env := map[string]string{"SCION_AGENT_ID": "agent-a", "SCION_PROJECT_ID": "proj-123"}
			agentDir := config.GetAgentDir(projectScionDir, "agent-a", false)

			var wantRepoRootEmpty bool
			var wantPersisted string

			switch tc.workspace {
			case "first":
				workspace := repoRootMatrixWorkspace(t, worktreesDir, tc.workspaceKind)
				ctx := api.ContextWithProvisionedWorktreeRepoRoot(context.Background(), root)
				if _, err := mgr.Start(ctx, api.StartOptions{
					Name: "agent-a", ProjectPath: projectScionDir, NoAuth: true, Workspace: workspace, Env: env,
				}); err != nil {
					t.Fatalf("Start failed: %v", err)
				}
				wantRepoRootEmpty = tc.workspaceKind == "negative"
				if !wantRepoRootEmpty {
					wantPersisted = root
				}

			case "resumeA", "resumeB", "resumeC":
				// A genuine worktree must exist to resume onto regardless of
				// ordering; which spelling recovery uses is set directly
				// below.
				asGivenWorkspace := repoRootMatrixWorkspace(t, worktreesDir, "as-given")
				resolvedWorkspace, err := filepath.EvalSymlinks(asGivenWorkspace)
				if err != nil {
					t.Fatalf("EvalSymlinks(workspace): %v", err)
				}

				// Materialize the agent directory, then set the persisted
				// values directly, independent of whichever path a prior
				// dispatch would have used to get them there.
				if _, err := mgr.Start(context.Background(), api.StartOptions{
					Name: "agent-a", ProjectPath: projectScionDir, NoAuth: true, Workspace: asGivenWorkspace, Env: env,
				}); err != nil {
					t.Fatalf("initial Start failed: %v", err)
				}
				persistedRoot := root
				if tc.workspace == "resumeC" {
					persistedRoot = resolvedRoot
				}
				if err := writeProvisionedWorktreeRepoRoot(agentDir, persistedRoot); err != nil {
					t.Fatalf("writeProvisionedWorktreeRepoRoot: %v", err)
				}
				if tc.workspace == "resumeB" {
					setPersistedWorkspaceVolumeSource(t, agentDir, resolvedWorkspace)
				}
				// resumeA/resumeC need no workspace rewrite: the initial
				// Start above already persisted the as-given spelling.

				if _, err := mgr.Start(context.Background(), api.StartOptions{
					Name: "agent-a", ProjectPath: projectScionDir, NoAuth: true, Env: env,
				}); err != nil {
					t.Fatalf("resume Start failed: %v", err)
				}
				wantRepoRootEmpty = false
				wantPersisted = persistedRoot

			case "resumeLeafReplaced":
				// A genuine worktree for this agent, and a second,
				// independently genuine worktree to replace it with after a
				// valid persist.
				asGivenWorkspace := repoRootMatrixWorkspace(t, worktreesDir, "as-given")
				other := filepath.Join(worktreesDir, "agent-b")
				if err := util.CreateWorktree(other, "agent-b"); err != nil {
					t.Fatalf("failed to create second worktree: %v", err)
				}

				if _, err := mgr.Start(context.Background(), api.StartOptions{
					Name: "agent-a", ProjectPath: projectScionDir, NoAuth: true, Workspace: asGivenWorkspace, Env: env,
				}); err != nil {
					t.Fatalf("initial Start failed: %v", err)
				}
				if err := writeProvisionedWorktreeRepoRoot(agentDir, root); err != nil {
					t.Fatalf("writeProvisionedWorktreeRepoRoot: %v", err)
				}

				// Only after a valid value is already persisted: this
				// agent's own path now names a different worktree than the
				// one it was persisted against.
				if err := os.RemoveAll(asGivenWorkspace); err != nil {
					t.Fatalf("remove original worktree: %v", err)
				}
				if err := os.Symlink(other, asGivenWorkspace); err != nil {
					t.Fatal(err)
				}

				if _, err := mgr.Start(context.Background(), api.StartOptions{
					Name: "agent-a", ProjectPath: projectScionDir, NoAuth: true, Env: env,
				}); err != nil {
					t.Fatalf("resume Start failed: %v", err)
				}
				wantRepoRootEmpty = true
				wantPersisted = root

			default:
				t.Fatalf("unknown dispatch shape %q", tc.workspace)
			}

			if wantRepoRootEmpty {
				if capturedConfig.RepoRoot != "" {
					t.Errorf("RunConfig.RepoRoot = %q, want empty", capturedConfig.RepoRoot)
				}
			} else {
				if capturedConfig.RepoRoot == "" {
					t.Error("RunConfig.RepoRoot is empty, want the resolved root")
				} else if gotRoot, err := filepath.EvalSymlinks(capturedConfig.RepoRoot); err != nil {
					t.Errorf("EvalSymlinks(RunConfig.RepoRoot): %v", err)
				} else if gotRoot != resolvedRoot {
					t.Errorf("RunConfig.RepoRoot resolves to %q, want %q", gotRoot, resolvedRoot)
				}
			}

			gotPersisted := readProvisionedWorktreeRepoRoot(agentDir)
			if gotPersisted != wantPersisted {
				t.Errorf("persisted repo root = %q, want %q", gotPersisted, wantPersisted)
			}
		})
	}
}

// TestStartRepoRootSurvivesResumeAfterAMismatchedFirstDispatch covers a
// single dispatch whose root and workspace values arrive in two different
// spellings (root reached through a symlinked ancestor, workspace already
// resolved) still agrees between RunConfig.RepoRoot and the persisted value,
// and a later, ordinary resume recovers the same root from that persisted
// value.
func TestStartRepoRootSurvivesResumeAfterAMismatchedFirstDispatch(t *testing.T) {
	t.Setenv("SCION_HOST_UID", "")
	tmpDir := t.TempDir()
	projectScionDir := startRepoRootProjectScaffold(t, tmpDir)

	root, worktreesDir := repoRootMatrixShape(t, "ancestor-alias")
	asGiven := filepath.Join(worktreesDir, "agent-a")
	if err := util.CreateWorktree(asGiven, "agent-a"); err != nil {
		t.Fatalf("failed to create worktree: %v", err)
	}
	resolvedWorkspace, err := filepath.EvalSymlinks(asGiven)
	if err != nil {
		t.Fatalf("EvalSymlinks(asGiven): %v", err)
	}
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatalf("EvalSymlinks(root): %v", err)
	}

	var capturedConfig runtime.RunConfig
	mockRT := &runtime.MockRuntime{
		ListFunc: func(ctx context.Context, labelFilter map[string]string) ([]api.AgentInfo, error) {
			return []api.AgentInfo{}, nil
		},
		RunFunc: func(ctx context.Context, config runtime.RunConfig) (string, error) {
			capturedConfig = config
			return "mock-id", nil
		},
	}
	mgr := NewManager(mockRT)
	env := map[string]string{"SCION_AGENT_ID": "agent-a", "SCION_PROJECT_ID": "proj-123"}

	// First dispatch: root through its ancestor alias, workspace already
	// resolved — the two values arrive in different spellings.
	ctx := api.ContextWithProvisionedWorktreeRepoRoot(context.Background(), root)
	if _, err := mgr.Start(ctx, api.StartOptions{
		Name: "agent-a", ProjectPath: projectScionDir, NoAuth: true, Workspace: resolvedWorkspace, Env: env,
	}); err != nil {
		t.Fatalf("first Start failed: %v", err)
	}
	if capturedConfig.RepoRoot == "" {
		t.Fatal("first dispatch: RunConfig.RepoRoot is empty")
	}
	agentDir := config.GetAgentDir(projectScionDir, "agent-a", false)
	persisted := readProvisionedWorktreeRepoRoot(agentDir)
	if persisted != root {
		t.Fatalf("first dispatch: persisted repo root = %q, want %q (root as given)", persisted, root)
	}

	// Resume: no ctx signal, no explicit workspace — RepoRoot must come from
	// the persisted state alone.
	capturedConfig = runtime.RunConfig{}
	if _, err := mgr.Start(context.Background(), api.StartOptions{
		Name: "agent-a", ProjectPath: projectScionDir, NoAuth: true, Env: env,
	}); err != nil {
		t.Fatalf("resume Start failed: %v", err)
	}
	if capturedConfig.RepoRoot == "" {
		t.Fatal("resume: RunConfig.RepoRoot is empty")
	}
	gotRoot, err := filepath.EvalSymlinks(capturedConfig.RepoRoot)
	if err != nil {
		t.Fatalf("EvalSymlinks(resume RepoRoot): %v", err)
	}
	if gotRoot != resolvedRoot {
		t.Fatalf("resume: RunConfig.RepoRoot resolves to %q, want %q", gotRoot, resolvedRoot)
	}
}

// TestStartPersistsLeafAliasRootAsGiven is the leaf-alias analogue of
// TestStartPersistsTheAliasNotTheResolvedBase. See that test's own comment
// for why an ancestor-alias root does not discriminate against persisting a
// resolved form (every real version of the persistence gate in this
// package's history has always written its root argument exactly as given,
// so a root reached through an ancestor alias was never actually at risk of
// being persisted resolved). A root whose OWN final path element is a
// symlink exercises the identical property against the fuller resolution
// validatedWorktreeRepoRoot now applies for comparison purposes only: this
// test would fail if the comparison's resolved form were ever written
// instead of the original.
func TestStartPersistsLeafAliasRootAsGiven(t *testing.T) {
	t.Setenv("SCION_HOST_UID", "")
	tmpDir := t.TempDir()
	projectScionDir := startRepoRootProjectScaffold(t, tmpDir)

	root, worktreesDir := repoRootMatrixShape(t, "leaf-alias")
	userWorkspace := filepath.Join(worktreesDir, "agent-a")
	if err := util.CreateWorktree(userWorkspace, "agent-a"); err != nil {
		t.Fatalf("failed to create worktree: %v", err)
	}
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatalf("EvalSymlinks(root): %v", err)
	}

	mockRT := &runtime.MockRuntime{
		ListFunc: func(ctx context.Context, labelFilter map[string]string) ([]api.AgentInfo, error) {
			return []api.AgentInfo{}, nil
		},
		RunFunc: func(ctx context.Context, config runtime.RunConfig) (string, error) {
			return "mock-id", nil
		},
	}
	mgr := NewManager(mockRT)
	env := map[string]string{"SCION_AGENT_ID": "agent-a", "SCION_PROJECT_ID": "proj-123"}

	ctx := api.ContextWithProvisionedWorktreeRepoRoot(context.Background(), root)
	if _, err := mgr.Start(ctx, api.StartOptions{
		Name: "agent-a", ProjectPath: projectScionDir, NoAuth: true, Workspace: userWorkspace, Env: env,
	}); err != nil {
		t.Fatalf("Start failed: %v", err)
	}

	agentDir := config.GetAgentDir(projectScionDir, "agent-a", false)
	got := readProvisionedWorktreeRepoRoot(agentDir)
	if got != root {
		t.Fatalf("persisted repo root = %q, want %q (the value as given, not its resolved form)", got, root)
	}
	if got == resolvedRoot {
		t.Fatalf("test setup broken: root's as-given and resolved forms are identical (%q)", root)
	}
}

// TestValidatedWorktreeRepoRootAcceptsAliasedRootAgainstResolvedWorkspace
// covers the workspace-storage-backend re-validation in Start (triggered
// when a non-local backend replaces effectiveWorkspace with its own mount
// host path, already fully resolved) calls the exact same shared comparison
// as every other call site in this file. This exercises that comparison
// directly for the shape a non-local backend produces: an ancestor-aliased
// root against an already-resolved workspace. The NFS backend's own
// end-to-end test (TestStartInvalidatesProvisionedRepoRootWhenNFSBackendReplacesWorkspace)
// covers the case where the backend's path has no relationship to the prior
// root at all; this covers the case where it is still the same worktree,
// just already resolved.
func TestValidatedWorktreeRepoRootAcceptsAliasedRootAgainstResolvedWorkspace(t *testing.T) {
	t.Setenv("SCION_HOST_UID", "")
	root, worktreesDir := repoRootMatrixShape(t, "ancestor-alias")
	asGiven := filepath.Join(worktreesDir, "agent-a")
	if err := util.CreateWorktree(asGiven, "agent-a"); err != nil {
		t.Fatalf("failed to create worktree: %v", err)
	}
	resolvedWorkspace, err := filepath.EvalSymlinks(asGiven)
	if err != nil {
		t.Fatalf("EvalSymlinks(asGiven): %v", err)
	}

	got := validatedWorktreeRepoRoot(root, resolvedWorkspace)
	if got != root {
		t.Fatalf("validatedWorktreeRepoRoot(%q, %q) = %q, want %q", root, resolvedWorkspace, got, root)
	}
}

// TestValidatedWorktreeRepoRootRejectsRelativeRoot covers a relative root
// value whose own name is a symlink to an absolute target: resolving it
// succeeds and produces an absolute result, so a check performed only after
// resolution would never see that the original value was relative. The
// comparison must still refuse it, and must never return anything other
// than the empty string or the original candidateRoot — never a resolved
// form derived from a value it refused.
func TestValidatedWorktreeRepoRootRejectsRelativeRoot(t *testing.T) {
	t.Setenv("SCION_HOST_UID", "")
	root, worktreesDir := repoRootMatrixShape(t, "plain")
	asGiven := filepath.Join(worktreesDir, "agent-a")
	if err := util.CreateWorktree(asGiven, "agent-a"); err != nil {
		t.Fatalf("failed to create worktree: %v", err)
	}

	oldWd, err := os.Getwd()
	if err != nil {
		t.Fatalf("os.Getwd: %v", err)
	}
	defer func() {
		if err := os.Chdir(oldWd); err != nil {
			t.Fatalf("restore working directory: %v", err)
		}
	}()
	cwd := t.TempDir()
	if err := os.Chdir(cwd); err != nil {
		t.Fatalf("os.Chdir: %v", err)
	}
	const relRoot = "relative-root-link"
	if err := os.Symlink(root, relRoot); err != nil {
		t.Fatal(err)
	}

	if got := validatedWorktreeRepoRoot(relRoot, asGiven); got != "" {
		t.Fatalf("validatedWorktreeRepoRoot(%q, ...) = %q, want empty for a relative root", relRoot, got)
	}
}

// TestValidatedWorktreeRepoRootRejectsRelativeWorkspace is the workspace-side
// analogue of TestValidatedWorktreeRepoRootRejectsRelativeRoot: a relative
// workspace value whose parent directory is a symlink to an absolute
// target.
func TestValidatedWorktreeRepoRootRejectsRelativeWorkspace(t *testing.T) {
	t.Setenv("SCION_HOST_UID", "")
	root, worktreesDir := repoRootMatrixShape(t, "plain")
	asGiven := filepath.Join(worktreesDir, "agent-a")
	if err := util.CreateWorktree(asGiven, "agent-a"); err != nil {
		t.Fatalf("failed to create worktree: %v", err)
	}

	oldWd, err := os.Getwd()
	if err != nil {
		t.Fatalf("os.Getwd: %v", err)
	}
	defer func() {
		if err := os.Chdir(oldWd); err != nil {
			t.Fatalf("restore working directory: %v", err)
		}
	}()
	cwd := t.TempDir()
	if err := os.Chdir(cwd); err != nil {
		t.Fatalf("os.Chdir: %v", err)
	}
	const relWorktreesLink = "relative-worktrees-link"
	if err := os.Symlink(worktreesDir, relWorktreesLink); err != nil {
		t.Fatal(err)
	}
	relWorkspace := filepath.Join(relWorktreesLink, "agent-a")

	if got := validatedWorktreeRepoRoot(root, relWorkspace); got != "" {
		t.Fatalf("validatedWorktreeRepoRoot(..., %q) = %q, want empty for a relative workspace", relWorkspace, got)
	}
}

// TestValidatedWorktreeRepoRootAcceptsTrailingSlashWorkspace covers a
// workspace value with a trailing path separator: filepath.Dir on such a
// value returns the value itself with the separator trimmed, not its true
// parent, so the comparison must clean the value first.
func TestValidatedWorktreeRepoRootAcceptsTrailingSlashWorkspace(t *testing.T) {
	t.Setenv("SCION_HOST_UID", "")
	root, worktreesDir := repoRootMatrixShape(t, "plain")
	asGiven := filepath.Join(worktreesDir, "agent-a")
	if err := util.CreateWorktree(asGiven, "agent-a"); err != nil {
		t.Fatalf("failed to create worktree: %v", err)
	}

	withTrailingSlash := asGiven + string(filepath.Separator)
	got := validatedWorktreeRepoRoot(root, withTrailingSlash)
	if got != root {
		t.Fatalf("validatedWorktreeRepoRoot(%q, %q) = %q, want %q", root, withTrailingSlash, got, root)
	}
}

// TestStartResumeDoesNotAdoptRepoRootFromAgentInfoFile is a regression test
// for the storage boundary that keeps the persisted repo root out of
// container-writable storage: a user --workspace agent on a directory shaped
// like "<repo>/worktrees/<name>" (not a real git worktree). The first Start
// correctly yields an empty RepoRoot. agent-info.json is writable at
// runtime, so this test writes a "provisionedWorktreeRepoRoot" key into it
// directly to prove run.go must never source RepoRoot from agentHome on
// resume; the value lives in a broker-owned file under agentDir that is not
// mounted into the container, and the validator rejects a non-worktree
// directory regardless.
func TestStartResumeDoesNotAdoptRepoRootFromAgentInfoFile(t *testing.T) {
	tmpDir := t.TempDir()
	projectScionDir := startRepoRootProjectScaffold(t, tmpDir)

	// Use a REAL git worktree so the isolation is precise: the written value
	// below would validate successfully if run.go consulted agent-info.json
	// for it. That isolates "is the container-writable file even consulted"
	// (this test) from "does the validator reject a fake worktree shape"
	// (covered separately in pkg/provision's validator tests).
	t.Setenv("SCION_HOST_UID", "")
	userRepo := filepath.Join(tmpDir, "userrepo")
	if err := os.MkdirAll(userRepo, 0755); err != nil {
		t.Fatalf("failed to create user repo dir: %v", err)
	}
	setupGitRepo(t, userRepo)
	userWorkspace := createRealWorktree(t, userRepo, "foo")

	var capturedConfig runtime.RunConfig
	mockRT := &runtime.MockRuntime{
		ListFunc: func(ctx context.Context, labelFilter map[string]string) ([]api.AgentInfo, error) {
			return []api.AgentInfo{}, nil
		},
		RunFunc: func(ctx context.Context, config runtime.RunConfig) (string, error) {
			capturedConfig = config
			return "mock-id", nil
		},
	}
	mgr := NewManager(mockRT)
	env := map[string]string{"SCION_AGENT_ID": "agent-a", "SCION_PROJECT_ID": "proj-123"}

	// First Start: plain user --workspace, no ctx signal. Correct baseline:
	// RepoRoot is empty.
	if _, err := mgr.Start(context.Background(), api.StartOptions{
		Name: "agent-a", ProjectPath: projectScionDir, NoAuth: true, Workspace: userWorkspace, Env: env,
	}); err != nil {
		t.Fatalf("initial Start failed: %v", err)
	}
	if capturedConfig.RepoRoot != "" {
		t.Fatalf("baseline RunConfig.RepoRoot = %q, want \"\" before the state file is written", capturedConfig.RepoRoot)
	}

	// Write a "provisionedWorktreeRepoRoot" key into agent-info.json in
	// agentHome (bind-mounted read-write into the container); RepoRoot must
	// never be read from agentHome.
	agentHome := config.GetAgentHomePath(projectScionDir, "agent-a")
	containerWritten := []byte(`{"provisionedWorktreeRepoRoot":"` + userRepo + `"}`)
	if err := os.WriteFile(filepath.Join(agentHome, "agent-info.json"), containerWritten, 0644); err != nil {
		t.Fatalf("failed to write agent-info.json: %v", err)
	}

	// Resume: no ctx signal, empty Workspace — the shape that would surface
	// a container-writable value as a live RepoRoot if it were still consulted.
	capturedConfig = runtime.RunConfig{}
	if _, err := mgr.Start(context.Background(), api.StartOptions{
		Name: "agent-a", ProjectPath: projectScionDir, NoAuth: true, Resume: true, Env: env,
	}); err != nil {
		t.Fatalf("resume Start failed: %v", err)
	}

	if capturedConfig.RepoRoot != "" {
		t.Fatalf("resume RunConfig.RepoRoot = %q, want \"\" — a container-writable agent-info.json must not be able to set RepoRoot", capturedConfig.RepoRoot)
	}
}
