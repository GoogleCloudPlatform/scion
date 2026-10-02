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
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/agent"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// setupRepoRootProjectScaffold creates a minimal project under tmpDir that
// pkg/agent's Start can resolve harness/template/settings from (docker
// profile, a "test-harness" harness-config, and a "default" template),
// changes the working directory and HOME to tmpDir for the duration of the
// test, and returns the project's .scion directory (the ProjectPath
// pkg/agent.Start expects).
func setupRepoRootProjectScaffold(t *testing.T, tmpDir string) string {
	t.Helper()

	oldWd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(tmpDir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldWd) })

	origHome := os.Getenv("HOME")
	t.Cleanup(func() { _ = os.Setenv("HOME", origHome) })
	if err := os.Setenv("HOME", tmpDir); err != nil {
		t.Fatal(err)
	}

	projectDir := filepath.Join(tmpDir, "project")
	projectScionDir := filepath.Join(projectDir, ".scion")
	if err := os.MkdirAll(projectScionDir, 0o755); err != nil {
		t.Fatal(err)
	}
	settingsYAML := `schema_version: "1"
active_profile: local
harness_configs:
  test-harness:
    harness: gemini
    user: scion
    image: test-image:latest
profiles:
  local:
    runtime: docker
`
	if err := os.WriteFile(filepath.Join(projectScionDir, "settings.yaml"), []byte(settingsYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	hcDir := filepath.Join(projectScionDir, "harness-configs", "test-harness")
	if err := os.MkdirAll(hcDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(hcDir, "config.yaml"), []byte("harness: gemini\nuser: scion\nimage: test-image:latest\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	tplDir := filepath.Join(projectScionDir, "templates", "default")
	if err := os.MkdirAll(tplDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tplDir, "scion-agent.json"), []byte(`{"default_harness_config": "test-harness"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	return projectScionDir
}

// TestTryProvisionWorktree_Start_StitchesRepoRoot is the required regression
// guard for the hub-native worktree-per-agent RepoRoot bug: it exercises the
// real sequence end to end — tryProvisionWorktree (this package) followed by
// the real pkg/agent.Start — and asserts RunConfig.RepoRoot comes out
// non-empty and equal to the shared base, without ever supplying
// RunConfig.RepoRoot directly.
//
// Every pre-existing worktree test (TestTryProvisionWorktree_JoinResolvesSharedPath,
// TestWorktreeWorkspace_RepoRootDerivesToBase, pkg/provision's and
// pkg/runtime/common_test.go's) exercises tryProvisionWorktree and the
// mount-building logic in isolation with RunConfig.RepoRoot supplied
// directly (or hand-derives it without calling Start) — exactly why none of
// them caught the defect: the gap was in Start's own stitching of the
// broker's signal into repoRoot, not in either side alone.
//
// Before the fix (a broker-local ctx signal from tryProvisionWorktree,
// consumed directly by run.go's Start), this test fails with
// RunConfig.RepoRoot == "": Start had no way to know opts.Workspace was the
// broker's own worktree rather than a user --workspace override, so
// detectRepoRoot's explicit-workspace skip (#642) swallowed the repo root and
// the real /repo-root/.git mount never fired.
func TestTryProvisionWorktree_Start_StitchesRepoRoot(t *testing.T) {
	t.Setenv("SCION_HOST_UID", "")
	if eligible, reason := runtime.WorktreeModeEligible(); !eligible {
		t.Skipf("git too old, worktree mode not eligible on this host: %s", reason)
	}

	// --- Phase 1: broker side — the real tryProvisionWorktree call. ---
	bare := initBareRepoWithCommit(t)
	gc := &api.GitCloneConfig{URL: bare, Branch: "main"}

	brokerProjectPath := filepath.Join(t.TempDir(), "broker-project")
	if err := os.MkdirAll(brokerProjectPath, 0o755); err != nil {
		t.Fatal(err)
	}

	srv := &Server{}
	provisionOpts := &api.StartOptions{}
	provisioned, repoRoot := srv.tryProvisionWorktree(context.Background(), startContextInputs{
		Name:          "agent-a",
		AgentID:       "agent-a",
		ProjectID:     "p1",
		ProjectSlug:   "proj",
		ProjectPath:   brokerProjectPath,
		WorkspaceMode: store.WorkspaceModeWorktreePerAgent,
		Config:        &CreateAgentConfig{GitClone: gc},
	}, provisionOpts, map[string]string{})
	if !provisioned {
		t.Fatal("tryProvisionWorktree: expected provisioning to succeed")
	}
	if repoRoot == "" {
		t.Fatal("tryProvisionWorktree: expected a non-empty repo root")
	}
	if provisionOpts.Workspace == "" {
		t.Fatal("tryProvisionWorktree: expected opts.Workspace to be set")
	}

	// --- Phase 2: mirror the broker handler, then call the real pkg/agent.Start. ---
	// createAgent (handlers.go) threads sc.ProvisionedWorktreeRepoRoot onto
	// ctx exactly like this, right after buildStartContext returns and before
	// Manager.Start is called.
	ctx := api.ContextWithProvisionedWorktreeRepoRoot(context.Background(), repoRoot)

	projectScionDir := setupRepoRootProjectScaffold(t, t.TempDir())

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
	mgr := agent.NewManager(mockRT)

	// This mirrors the real broker dispatch shape: opts.Workspace is the
	// broker-provisioned worktree from Phase 1, GitClone is nil (the worktree
	// is already provisioned host-side — no in-container clone needed), and
	// RunConfig.RepoRoot is never supplied directly anywhere in this test.
	startOpts := api.StartOptions{
		Name:        "agent-a",
		ProjectPath: projectScionDir,
		NoAuth:      true,
		Workspace:   provisionOpts.Workspace,
		Env: map[string]string{
			"SCION_AGENT_ID":   "agent-a",
			"SCION_PROJECT_ID": "p1",
		},
	}
	if _, err := mgr.Start(ctx, startOpts); err != nil {
		t.Fatalf("Start failed: %v", err)
	}

	if capturedConfig.RepoRoot == "" {
		t.Fatal("RunConfig.RepoRoot is empty — the broker-provisioned worktree signal did not reach Start's repoRoot resolution")
	}
	gotRoot, err := filepath.EvalSymlinks(capturedConfig.RepoRoot)
	if err != nil {
		t.Fatalf("EvalSymlinks(RepoRoot): %v", err)
	}
	wantRoot, err := filepath.EvalSymlinks(repoRoot)
	if err != nil {
		t.Fatalf("EvalSymlinks(repoRoot): %v", err)
	}
	if gotRoot != wantRoot {
		t.Fatalf("RunConfig.RepoRoot = %q, want %q (the shared base)", gotRoot, wantRoot)
	}
	// ContainerWorkspace is what actually selects the mount branch in
	// pkg/runtime/common.go — asserting only on
	// RepoRoot after EvalSymlinks can hide a lexical mismatch that still
	// misroutes the mount. Confirm it lands under /repo-root, not the
	// full-root-fallback's /workspace.
	if wantContainerWorkspace := "/repo-root/worktrees/agent-a"; capturedConfig.ContainerWorkspace != wantContainerWorkspace {
		t.Fatalf("RunConfig.ContainerWorkspace = %q, want %q", capturedConfig.ContainerWorkspace, wantContainerWorkspace)
	}
}

// TestTryProvisionWorktree_Start_RepoRootSurvivesResume is the required R2
// regression guard: a SECOND Start call for the same agent — with an empty
// Workspace and no ctx signal, exactly like a hub-dispatched restart/resume,
// where the broker does not re-run tryProvisionWorktree — must still resolve
// RunConfig.RepoRoot to the same shared base, via the persisted
// AgentInfo.ProvisionedWorktreeRepoRoot value and run.go's validation, not via
// any value this test supplies directly.
func TestTryProvisionWorktree_Start_RepoRootSurvivesResume(t *testing.T) {
	t.Setenv("SCION_HOST_UID", "")
	if eligible, reason := runtime.WorktreeModeEligible(); !eligible {
		t.Skipf("git too old, worktree mode not eligible on this host: %s", reason)
	}

	bare := initBareRepoWithCommit(t)
	gc := &api.GitCloneConfig{URL: bare, Branch: "main"}

	brokerProjectPath := filepath.Join(t.TempDir(), "broker-project")
	if err := os.MkdirAll(brokerProjectPath, 0o755); err != nil {
		t.Fatal(err)
	}

	srv := &Server{}
	provisionOpts := &api.StartOptions{}
	provisioned, repoRoot := srv.tryProvisionWorktree(context.Background(), startContextInputs{
		Name:          "agent-a",
		AgentID:       "agent-a",
		ProjectID:     "p1",
		ProjectSlug:   "proj",
		ProjectPath:   brokerProjectPath,
		WorkspaceMode: store.WorkspaceModeWorktreePerAgent,
		Config:        &CreateAgentConfig{GitClone: gc},
	}, provisionOpts, map[string]string{})
	if !provisioned || repoRoot == "" || provisionOpts.Workspace == "" {
		t.Fatalf("tryProvisionWorktree setup failed: provisioned=%v repoRoot=%q workspace=%q", provisioned, repoRoot, provisionOpts.Workspace)
	}

	projectScionDir := setupRepoRootProjectScaffold(t, t.TempDir())

	var capturedConfig runtime.RunConfig
	mockRT := &runtime.MockRuntime{
		ListFunc: func(ctx context.Context, labelFilter map[string]string) ([]api.AgentInfo, error) {
			// No running container found — mirrors both create and a
			// resume/restart where the previous container has already exited.
			return []api.AgentInfo{}, nil
		},
		RunFunc: func(ctx context.Context, config runtime.RunConfig) (string, error) {
			capturedConfig = config
			return "mock-id", nil
		},
	}
	mgr := agent.NewManager(mockRT)

	env := map[string]string{"SCION_AGENT_ID": "agent-a", "SCION_PROJECT_ID": "p1"}

	// First Start: the create dispatch, with the ctx signal, exactly like
	// TestTryProvisionWorktree_Start_StitchesRepoRoot.
	createCtx := api.ContextWithProvisionedWorktreeRepoRoot(context.Background(), repoRoot)
	if _, err := mgr.Start(createCtx, api.StartOptions{
		Name:        "agent-a",
		ProjectPath: projectScionDir,
		NoAuth:      true,
		Workspace:   provisionOpts.Workspace,
		Env:         env,
	}); err != nil {
		t.Fatalf("create Start failed: %v", err)
	}
	if capturedConfig.RepoRoot == "" {
		t.Fatal("create Start: RunConfig.RepoRoot is empty (setup broken, not the resume case under test)")
	}

	// Second Start: the resume/restart dispatch. No ctx signal (the broker
	// does not re-run tryProvisionWorktree on start/restart) and an empty
	// Workspace (exactly what the hub sends on restart) — RepoRoot must come
	// from the persisted AgentInfo value alone, validated against the real
	// filesystem.
	capturedConfig = runtime.RunConfig{}
	if _, err := mgr.Start(context.Background(), api.StartOptions{
		Name:        "agent-a",
		ProjectPath: projectScionDir,
		NoAuth:      true,
		Workspace:   "",
		Resume:      true,
		Env:         env,
	}); err != nil {
		t.Fatalf("resume Start failed: %v", err)
	}

	if capturedConfig.RepoRoot == "" {
		t.Fatal("resume Start: RunConfig.RepoRoot is empty — the persisted repo root did not survive resume")
	}
	gotRoot, err := filepath.EvalSymlinks(capturedConfig.RepoRoot)
	if err != nil {
		t.Fatalf("EvalSymlinks(RepoRoot): %v", err)
	}
	wantRoot, err := filepath.EvalSymlinks(repoRoot)
	if err != nil {
		t.Fatalf("EvalSymlinks(repoRoot): %v", err)
	}
	if gotRoot != wantRoot {
		t.Fatalf("resume RunConfig.RepoRoot = %q, want %q (the shared base)", gotRoot, wantRoot)
	}
	gotWorkspace, err := filepath.EvalSymlinks(capturedConfig.Workspace)
	if err != nil {
		t.Fatalf("EvalSymlinks(Workspace): %v", err)
	}
	wantWorkspace, err := filepath.EvalSymlinks(provisionOpts.Workspace)
	if err != nil {
		t.Fatalf("EvalSymlinks(provisionOpts.Workspace): %v", err)
	}
	if gotWorkspace != wantWorkspace {
		t.Fatalf("resume RunConfig.Workspace = %q, want %q (the worktree, recovered via the persisted Volumes mount)", gotWorkspace, wantWorkspace)
	}
	// See the StitchesRepoRoot test for why ContainerWorkspace, not just
	// RepoRoot, must be asserted.
	if wantContainerWorkspace := "/repo-root/worktrees/agent-a"; capturedConfig.ContainerWorkspace != wantContainerWorkspace {
		t.Fatalf("resume RunConfig.ContainerWorkspace = %q, want %q", capturedConfig.ContainerWorkspace, wantContainerWorkspace)
	}
}

// postCreateAgentExpectCreated drives the real HTTP handler, so the request
// travels the production path: decode -> createAgent -> ctx decoration ->
// Manager.Start. Modelled on postCreateAgent in
// handlers_hub_defaults_wiring_test.go.
func postCreateAgentExpectCreated(t *testing.T, srv *Server, body string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("want %d, got %d: %s", http.StatusCreated, w.Code, w.Body.String())
	}
}

// TestCreateAgent_WiresProvisionedWorktreeRepoRootOntoStartContext is the
// required R1 regression guard: it drives the real HTTP createAgent handler
// (not a hand-built ctx) for a worktree-per-agent, git-backed create request,
// with the primary Manager backed by a capturing runtime.MockRuntime, and
// asserts RunConfig.RepoRoot equals the shared base. This is the one seam
// TestTryProvisionWorktree_Start_StitchesRepoRoot cannot cover: that test
// builds ctx by hand, so it never exercises createAgent's
// `ctx = api.ContextWithProvisionedWorktreeRepoRoot(ctx, sc.ProvisionedWorktreeRepoRoot)`
// wrap. Deleting that wrap leaves the rest of the pkg/runtimebroker suite
// green; this test is what catches it.
func TestCreateAgent_WiresProvisionedWorktreeRepoRootOntoStartContext(t *testing.T) {
	t.Setenv("SCION_HOST_UID", "")
	if eligible, reason := runtime.WorktreeModeEligible(); !eligible {
		t.Skipf("git too old, worktree mode not eligible on this host: %s", reason)
	}

	bare := initBareRepoWithCommit(t)
	brokerTmp := t.TempDir()
	projectScionDir := setupRepoRootProjectScaffold(t, brokerTmp)
	// tryProvisionWorktree resolves the shared base under ProjectPath/workspace
	// (pkg/runtime's local backend) — pass the project's .scion dir as
	// ProjectPath, matching the hub-managed-project convention used elsewhere
	// in this package's tests.
	projectPath := projectScionDir

	var capturedConfig runtime.RunConfig
	mockRT := &runtime.MockRuntime{
		NameFunc: func() string { return "docker" },
		ListFunc: func(ctx context.Context, labelFilter map[string]string) ([]api.AgentInfo, error) {
			return []api.AgentInfo{}, nil
		},
		RunFunc: func(ctx context.Context, config runtime.RunConfig) (string, error) {
			capturedConfig = config
			return "mock-id", nil
		},
	}
	cfg := DefaultServerConfig()
	cfg.ForceRuntime = "docker"
	srv := New(cfg, agent.NewManager(mockRT), mockRT)

	body := `{
		"name": "agent-a",
		"id": "agent-a",
		"slug": "agent-a",
		"projectPath": ` + jsonStr(projectPath) + `,
		"projectId": "p1",
		"projectSlug": "proj",
		"workspaceMode": "worktree-per-agent",
		"noAuth": true,
		"config": {
			"gitClone": {"url": ` + jsonStr(bare) + `, "branch": "main"}
		}
	}`

	postCreateAgentExpectCreated(t, srv, body)

	if capturedConfig.RepoRoot == "" {
		t.Fatal("RunConfig.RepoRoot is empty — createAgent's ctx wrap did not thread the broker-provisioned worktree signal into Start")
	}
	expectedBase := filepath.Join(projectPath, "workspace")
	gotRoot, err := filepath.EvalSymlinks(capturedConfig.RepoRoot)
	if err != nil {
		t.Fatalf("EvalSymlinks(RepoRoot): %v", err)
	}
	wantRoot, err := filepath.EvalSymlinks(expectedBase)
	if err != nil {
		t.Fatalf("EvalSymlinks(expectedBase): %v", err)
	}
	if gotRoot != wantRoot {
		t.Fatalf("RunConfig.RepoRoot = %q, want %q (the shared base)", gotRoot, wantRoot)
	}
	// See TestTryProvisionWorktree_Start_StitchesRepoRoot for why
	// ContainerWorkspace, not just RepoRoot, must be asserted.
	if wantContainerWorkspace := "/repo-root/worktrees/agent-a"; capturedConfig.ContainerWorkspace != wantContainerWorkspace {
		t.Fatalf("RunConfig.ContainerWorkspace = %q, want %q", capturedConfig.ContainerWorkspace, wantContainerWorkspace)
	}
}

// jsonStr quotes s as a JSON string literal, for building request bodies
// containing filesystem paths (which may need escaping on some platforms).
func jsonStr(s string) string {
	return `"` + strings.ReplaceAll(strings.ReplaceAll(s, `\`, `\\`), `"`, `\"`) + `"`
}

// TestTryProvisionWorktree_Start_SymlinkedBase_ContainerWorkspaceStaysConsistent
// is a regression guard: when the
// broker's project path runs through a symlink (a symlinked $HOME, a
// symlinked projects dir, or macOS's /var -> /private/var), the repo root
// validateProvisionedWorktreeRepoRoot returns must stay lexically consistent
// with the unresolved RunConfig.Workspace, or pkg/runtime/common.go's
// filepath.Rel(RepoRoot, Workspace) breaks and misroutes the mount into the
// full-root fallback branch (ContainerWorkspace == "/workspace" instead of
// "/repo-root/worktrees/<id>", and in-container git breaks again).
//
// Before the fix, validateProvisionedWorktreeRepoRoot returned
// EvalSymlinks(root) while effectiveWorkspace (and so RunConfig.Workspace)
// stayed lexical, exactly reproducing this break.
func TestTryProvisionWorktree_Start_SymlinkedBase_ContainerWorkspaceStaysConsistent(t *testing.T) {
	t.Setenv("SCION_HOST_UID", "")
	if eligible, reason := runtime.WorktreeModeEligible(); !eligible {
		t.Skipf("git too old, worktree mode not eligible on this host: %s", reason)
	}

	realDir := t.TempDir()
	linkParent := t.TempDir()
	linkDir := filepath.Join(linkParent, "link")
	if err := os.Symlink(realDir, linkDir); err != nil {
		t.Fatalf("os.Symlink: %v", err)
	}

	bare := initBareRepoWithCommit(t)
	gc := &api.GitCloneConfig{URL: bare, Branch: "main"}

	// The broker project path runs through the symlink.
	brokerProjectPath := filepath.Join(linkDir, "broker-project")
	if err := os.MkdirAll(brokerProjectPath, 0o755); err != nil {
		t.Fatal(err)
	}

	srv := &Server{}
	provisionOpts := &api.StartOptions{}
	provisioned, repoRoot := srv.tryProvisionWorktree(context.Background(), startContextInputs{
		Name:          "agent-a",
		AgentID:       "agent-a",
		ProjectID:     "p1",
		ProjectSlug:   "proj",
		ProjectPath:   brokerProjectPath,
		WorkspaceMode: store.WorkspaceModeWorktreePerAgent,
		Config:        &CreateAgentConfig{GitClone: gc},
	}, provisionOpts, map[string]string{})
	if !provisioned || repoRoot == "" || provisionOpts.Workspace == "" {
		t.Fatalf("tryProvisionWorktree setup failed: provisioned=%v repoRoot=%q workspace=%q", provisioned, repoRoot, provisionOpts.Workspace)
	}
	if !strings.Contains(repoRoot, linkDir) {
		t.Fatalf("test setup broken: repoRoot %q does not contain the symlinked component %q", repoRoot, linkDir)
	}

	ctx := api.ContextWithProvisionedWorktreeRepoRoot(context.Background(), repoRoot)
	projectScionDir := setupRepoRootProjectScaffold(t, t.TempDir())

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
	mgr := agent.NewManager(mockRT)

	if _, err := mgr.Start(ctx, api.StartOptions{
		Name:        "agent-a",
		ProjectPath: projectScionDir,
		NoAuth:      true,
		Workspace:   provisionOpts.Workspace,
		Env: map[string]string{
			"SCION_AGENT_ID":   "agent-a",
			"SCION_PROJECT_ID": "p1",
		},
	}); err != nil {
		t.Fatalf("Start failed: %v", err)
	}

	// The core assertion: ContainerWorkspace must land under /repo-root, the
	// worktree dual-mount branch — not /workspace, common.go's full-root
	// fallback that fires when filepath.Rel(RepoRoot, Workspace) doesn't
	// resolve to a clean "worktrees/<id>" subpath.
	if wantContainerWorkspace := "/repo-root/worktrees/agent-a"; capturedConfig.ContainerWorkspace != wantContainerWorkspace {
		t.Fatalf("RunConfig.ContainerWorkspace = %q, want %q — RepoRoot and Workspace are lexically inconsistent on a symlinked broker path", capturedConfig.ContainerWorkspace, wantContainerWorkspace)
	}
	// RepoRoot itself must stay lexically identical to the value
	// tryProvisionWorktree produced (through the symlink), not
	// EvalSymlinks'd — that lexical identity is exactly what keeps
	// filepath.Rel(RepoRoot, Workspace) correct in common.go.
	if capturedConfig.RepoRoot != repoRoot {
		t.Fatalf("RunConfig.RepoRoot = %q, want the original (unresolved) %q", capturedConfig.RepoRoot, repoRoot)
	}
}
