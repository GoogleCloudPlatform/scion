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
	"path/filepath"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/agent"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// TestTryProvisionWorktree_Start_StitchesRepoRoot is the HARD-required
// regression guard for the hub-native worktree-per-agent RepoRoot bug
// (ptone/scion#2062): it exercises the REAL sequence end to end —
// tryProvisionWorktree (this package) followed by the REAL pkg/agent.Start —
// and asserts RunConfig.RepoRoot comes out non-empty and equal to the shared
// base, WITHOUT ever supplying RunConfig.RepoRoot directly.
//
// Every pre-existing worktree test (TestTryProvisionWorktree_JoinResolvesSharedPath,
// TestWorktreeWorkspace_RepoRootDerivesToBase, pkg/provision's and
// pkg/runtime/common_test.go's) exercises tryProvisionWorktree and the
// mount-building logic in isolation with RunConfig.RepoRoot supplied
// directly (or hand-derives it without calling Start) — exactly why none of
// them caught the defect: the gap was in Start's own stitching of the
// broker's signal into repoRoot, not in either side alone.
//
// Before the fix (Option A — a broker-local ctx signal from
// tryProvisionWorktree, consumed directly by run.go's Start), this test
// fails with RunConfig.RepoRoot == "": Start had no way to know
// opts.Workspace was the broker's OWN worktree rather than a user
// --workspace override, so detectRepoRoot's explicit-workspace skip (#642)
// swallowed the repo root and the real /repo-root/.git mount never fired.
func TestTryProvisionWorktree_Start_StitchesRepoRoot(t *testing.T) {
	t.Setenv("SCION_HOST_UID", "")

	// --- Phase 1: broker side — the REAL tryProvisionWorktree call. ---
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

	// --- Phase 2: mirror the broker handler, then call the REAL pkg/agent.Start. ---
	// createAgent (handlers.go) threads sc.ProvisionedWorktreeRoot onto ctx
	// exactly like this, right after buildStartContext returns and before
	// Manager.Start is called.
	ctx := api.ContextWithProvisionedWorktree(context.Background(), repoRoot)

	tmpDir := t.TempDir()
	oldWd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(tmpDir); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chdir(oldWd) }()

	origHome := os.Getenv("HOME")
	defer func() { _ = os.Setenv("HOME", origHome) }()
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
	// RunConfig.RepoRoot is NEVER supplied directly anywhere in this test.
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
}
