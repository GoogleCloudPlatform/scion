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
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

// TestStart_RejectsPersistedWorkspaceSourceEqualToFilesystemRoot is the fail-closed
// regression test for a workspace source that was resolved and persisted to
// scion-agent.json by an older version of the resolution logic, before the
// per-project workspace source validation existed. Start must still refuse
// it on resume, and — critically — must not reach the point of calling the
// runtime to set up the container: RunFunc must never be invoked.
func TestStart_RejectsPersistedWorkspaceSourceEqualToFilesystemRoot(t *testing.T) {
	tmpDir := t.TempDir()

	oldWd, _ := os.Getwd()
	_ = os.Chdir(tmpDir)
	defer func() { _ = os.Chdir(oldWd) }()

	originalHome := os.Getenv("HOME")
	defer func() { _ = os.Setenv("HOME", originalHome) }()
	_ = os.Setenv("HOME", tmpDir)

	globalScionDir := filepath.Join(tmpDir, ".scion")

	hcDir := filepath.Join(globalScionDir, "harness-configs", "test-harness")
	_ = os.MkdirAll(hcDir, 0755)
	_ = os.WriteFile(filepath.Join(hcDir, "config.yaml"), []byte("harness: gemini\nuser: scion\nimage: test-image:latest\n"), 0644)

	tplDir := filepath.Join(globalScionDir, "templates", "default")
	_ = os.MkdirAll(tplDir, 0755)
	_ = os.WriteFile(filepath.Join(tplDir, "scion-agent.json"), []byte(`{"default_harness_config": "test-harness"}`), 0644)

	_ = os.WriteFile(filepath.Join(globalScionDir, "settings.yaml"), []byte(`schema_version: "1"
active_profile: local
profiles:
  local:
    runtime: docker
`), 0644)

	projectDir := filepath.Join(tmpDir, "project")
	projectScionDir := filepath.Join(projectDir, ".scion")
	_ = os.MkdirAll(projectScionDir, 0755)

	runCalled := false
	mockRT := &runtime.MockRuntime{
		ListFunc: func(ctx context.Context, labelFilter map[string]string) ([]api.AgentInfo, error) {
			return []api.AgentInfo{}, nil
		},
		RunFunc: func(ctx context.Context, config runtime.RunConfig) (string, error) {
			runCalled = true
			return "mock-id", nil
		},
	}

	agentDir := filepath.Join(projectScionDir, "agents", "local-test")
	_ = os.MkdirAll(filepath.Join(agentDir, "home"), 0755)
	_ = os.WriteFile(filepath.Join(agentDir, "scion-agent.json"), []byte(`{
		"harness": "generic",
		"explicit_workspace": true,
		"volumes": [{"source": "/", "target": "/workspace"}]
	}`), 0644)

	mgr := NewManager(mockRT)

	_, err := mgr.Start(context.Background(), api.StartOptions{
		Name:        "local-test",
		ProjectPath: projectScionDir,
		NoAuth:      true,
	})
	if err == nil {
		t.Fatal("expected Start to fail for a persisted workspace source of '/'")
	}
	if !strings.Contains(err.Error(), "is not an allowed workspace path") {
		t.Errorf("expected the rejection to come from workspace source validation, got: %v", err)
	}
	if runCalled {
		t.Error("expected the runtime's Run to never be invoked (fail closed), but it was called")
	}
}

// TestStart_RejectsPersistedWorkspaceSourceEqualToHome is the $HOME sibling
// of TestStart_RejectsPersistedWorkspaceSourceEqualToFilesystemRoot.
func TestStart_RejectsPersistedWorkspaceSourceEqualToHome(t *testing.T) {
	tmpDir := t.TempDir()

	oldWd, _ := os.Getwd()
	_ = os.Chdir(tmpDir)
	defer func() { _ = os.Chdir(oldWd) }()

	originalHome := os.Getenv("HOME")
	defer func() { _ = os.Setenv("HOME", originalHome) }()
	_ = os.Setenv("HOME", tmpDir)

	globalScionDir := filepath.Join(tmpDir, ".scion")

	hcDir := filepath.Join(globalScionDir, "harness-configs", "test-harness")
	_ = os.MkdirAll(hcDir, 0755)
	_ = os.WriteFile(filepath.Join(hcDir, "config.yaml"), []byte("harness: gemini\nuser: scion\nimage: test-image:latest\n"), 0644)

	tplDir := filepath.Join(globalScionDir, "templates", "default")
	_ = os.MkdirAll(tplDir, 0755)
	_ = os.WriteFile(filepath.Join(tplDir, "scion-agent.json"), []byte(`{"default_harness_config": "test-harness"}`), 0644)

	_ = os.WriteFile(filepath.Join(globalScionDir, "settings.yaml"), []byte(`schema_version: "1"
active_profile: local
profiles:
  local:
    runtime: docker
`), 0644)

	projectDir := filepath.Join(tmpDir, "project")
	projectScionDir := filepath.Join(projectDir, ".scion")
	_ = os.MkdirAll(projectScionDir, 0755)

	runCalled := false
	mockRT := &runtime.MockRuntime{
		ListFunc: func(ctx context.Context, labelFilter map[string]string) ([]api.AgentInfo, error) {
			return []api.AgentInfo{}, nil
		},
		RunFunc: func(ctx context.Context, config runtime.RunConfig) (string, error) {
			runCalled = true
			return "mock-id", nil
		},
	}

	agentDir := filepath.Join(projectScionDir, "agents", "local-test")
	_ = os.MkdirAll(filepath.Join(agentDir, "home"), 0755)
	agentJSON := fmt.Sprintf(`{
		"harness": "generic",
		"explicit_workspace": true,
		"volumes": [{"source": %q, "target": "/workspace"}]
	}`, tmpDir)
	_ = os.WriteFile(filepath.Join(agentDir, "scion-agent.json"), []byte(agentJSON), 0644)

	mgr := NewManager(mockRT)

	_, err := mgr.Start(context.Background(), api.StartOptions{
		Name:        "local-test",
		ProjectPath: projectScionDir,
		NoAuth:      true,
	})
	if err == nil {
		t.Fatal("expected Start to fail for a persisted workspace source equal to $HOME")
	}
	if !strings.Contains(err.Error(), "is not an allowed workspace path") {
		t.Errorf("expected the rejection to come from the deny-set floor, got: %v", err)
	}
	if runCalled {
		t.Error("expected the runtime's Run to never be invoked (fail closed), but it was called")
	}
}

// TestStart_RejectsUnresolvableGitRepoRoot is the regression test for a
// fail-open gap: a project directory can satisfy git's own
// is-inside-work-tree check while having no top-level work tree to resolve
// at all -- a bare repository is the standard example (`git rev-parse
// --is-inside-work-tree` succeeds and prints "false" there, but `git
// rev-parse --show-toplevel` fails). workspaceSourceRoots must refuse this
// case outright rather than returning no roots, which the caller would
// otherwise treat as "no root available, check the rootless floor only" and
// accept any absolute path that isn't '/', $HOME, or under ~/.scion.
func TestStart_RejectsUnresolvableGitRepoRoot(t *testing.T) {
	t.Setenv("SCION_HOST_UID", "")

	tmpDir := t.TempDir()

	oldWd, _ := os.Getwd()
	_ = os.Chdir(tmpDir)
	defer func() { _ = os.Chdir(oldWd) }()

	originalHome := os.Getenv("HOME")
	defer func() { _ = os.Setenv("HOME", originalHome) }()
	_ = os.Setenv("HOME", tmpDir)

	globalScionDir := filepath.Join(tmpDir, ".scion")
	hcDir := filepath.Join(globalScionDir, "harness-configs", "test-harness")
	_ = os.MkdirAll(hcDir, 0755)
	_ = os.WriteFile(filepath.Join(hcDir, "config.yaml"), []byte("harness: gemini\nuser: scion\nimage: test-image:latest\n"), 0644)
	tplDir := filepath.Join(globalScionDir, "templates", "default")
	_ = os.MkdirAll(tplDir, 0755)
	_ = os.WriteFile(filepath.Join(tplDir, "scion-agent.json"), []byte(`{"default_harness_config": "test-harness"}`), 0644)
	_ = os.WriteFile(filepath.Join(globalScionDir, "settings.yaml"), []byte(`schema_version: "1"
active_profile: local
profiles:
  local:
    runtime: docker
`), 0644)

	// A bare repository: git considers a path inside it "inside a work
	// tree" (is-inside-work-tree succeeds, printing "false") without there
	// being any top-level work tree to resolve (show-toplevel fails). This
	// is the real, reproducible way util.IsGitRepoDir can report true while
	// util.RepoRootDir has nothing to return.
	bareRepo := filepath.Join(tmpDir, "bare-project.git")
	initCmd := exec.Command("git", "init", "--bare", bareRepo)
	if out, err := initCmd.CombinedOutput(); err != nil {
		t.Fatalf("git init --bare: %v: %s", err, out)
	}
	projectScionDir := filepath.Join(bareRepo, ".scion")
	_ = os.MkdirAll(projectScionDir, 0755)

	// A persisted, non-explicit workspace pointing somewhere unrelated --
	// with the bug, this would be accepted by the rootless deny-set-only
	// floor since it is neither '/', $HOME, nor under ~/.scion.
	unrelatedWorkspace := filepath.Join(tmpDir, "unrelated-workspace")
	_ = os.MkdirAll(unrelatedWorkspace, 0755)

	agentDir := filepath.Join(projectScionDir, "agents", "bare-repo-agent")
	_ = os.MkdirAll(filepath.Join(agentDir, "home"), 0755)
	agentJSON := fmt.Sprintf(`{"harness": "generic", "volumes": [{"source": %q, "target": "/workspace"}]}`, unrelatedWorkspace)
	_ = os.WriteFile(filepath.Join(agentDir, "scion-agent.json"), []byte(agentJSON), 0644)

	runCalled := false
	mockRT := &runtime.MockRuntime{
		ListFunc: func(ctx context.Context, labelFilter map[string]string) ([]api.AgentInfo, error) {
			return []api.AgentInfo{}, nil
		},
		RunFunc: func(ctx context.Context, config runtime.RunConfig) (string, error) {
			runCalled = true
			return "mock-id", nil
		},
	}

	mgr := NewManager(mockRT)
	_, err := mgr.Start(context.Background(), api.StartOptions{
		Name:        "bare-repo-agent",
		ProjectPath: projectScionDir,
		NoAuth:      true,
	})
	if err == nil {
		t.Fatal("expected Start to refuse a workspace source when the project's own repo root cannot be resolved")
	}
	if !strings.Contains(err.Error(), "does not resolve to its own git work tree") {
		t.Errorf("expected the rejection to name the unresolvable repo root, got: %v", err)
	}
	if !strings.Contains(err.Error(), projectScionDir) {
		t.Errorf("expected the rejection to include the project directory path, got: %v", err)
	}
	if runCalled {
		t.Error("expected the runtime's Run to never be invoked (fail closed), but it was called")
	}
}

// TestStart_RejectsNestedBareRepoResolvingToEnclosingRepo is the regression
// test for a residual variant of the unresolvable-repo-root gap: a bare
// repository nested inside another repository's work tree does not error at
// all when util.RepoRootDir resolves it. RepoRootDir retries from the parent
// directory on a --show-toplevel failure, so the walk-up moves past the bare
// repo's own directory entirely and returns the ENCLOSING repository's top
// level -- a real, successfully-resolved root, just not projectDir's own.
// Without workspaceSharesProjectRepo's common-git-dir cross-check, a
// persisted workspace anywhere in the enclosing repo would be accepted.
// SharedWorkspace: true makes Start validate the persisted outer-repo
// source itself rather than a self-healed managed worktree, matching the
// reported shape.
func TestStart_RejectsNestedBareRepoResolvingToEnclosingRepo(t *testing.T) {
	t.Setenv("SCION_HOST_UID", "")

	tmpDir := t.TempDir()

	oldWd, _ := os.Getwd()
	_ = os.Chdir(tmpDir)
	defer func() { _ = os.Chdir(oldWd) }()

	originalHome := os.Getenv("HOME")
	defer func() { _ = os.Setenv("HOME", originalHome) }()
	_ = os.Setenv("HOME", tmpDir)

	globalScionDir := filepath.Join(tmpDir, ".scion")
	hcDir := filepath.Join(globalScionDir, "harness-configs", "test-harness")
	_ = os.MkdirAll(hcDir, 0755)
	_ = os.WriteFile(filepath.Join(hcDir, "config.yaml"), []byte("harness: gemini\nuser: scion\nimage: test-image:latest\n"), 0644)
	tplDir := filepath.Join(globalScionDir, "templates", "default")
	_ = os.MkdirAll(tplDir, 0755)
	_ = os.WriteFile(filepath.Join(tplDir, "scion-agent.json"), []byte(`{"default_harness_config": "test-harness"}`), 0644)
	_ = os.WriteFile(filepath.Join(globalScionDir, "settings.yaml"), []byte(`schema_version: "1"
active_profile: local
profiles:
  local:
    runtime: docker
`), 0644)

	// The enclosing repository: a normal, non-bare work tree.
	outerRepo := filepath.Join(tmpDir, "outer")
	setupTestGitRepoWithBranch(t, outerRepo, "unused-branch")

	// A bare repository nested inside the outer repo's work tree, with
	// .scion inside IT -- the exact shape that makes util.RepoRootDir's
	// parent-retry walk past the bare repo and land on the outer repo.
	nestedBare := filepath.Join(outerRepo, "vendor", "some-bare.git")
	initCmd := exec.Command("git", "init", "--bare", nestedBare)
	if out, err := initCmd.CombinedOutput(); err != nil {
		t.Fatalf("git init --bare: %v: %s", err, out)
	}
	projectScionDir := filepath.Join(nestedBare, ".scion")
	_ = os.MkdirAll(projectScionDir, 0755)

	// A persisted workspace elsewhere in the OUTER repo -- with the bug,
	// this is accepted because RepoRootDir silently resolves to the outer
	// repo's root and containment is checked against that.
	outerWorkspace := filepath.Join(outerRepo, "some-other-dir")
	_ = os.MkdirAll(outerWorkspace, 0755)

	agentDir := filepath.Join(projectScionDir, "agents", "nested-bare-agent")
	_ = os.MkdirAll(filepath.Join(agentDir, "home"), 0755)
	agentJSON := fmt.Sprintf(`{"harness": "generic", "volumes": [{"source": %q, "target": "/workspace"}]}`, outerWorkspace)
	_ = os.WriteFile(filepath.Join(agentDir, "scion-agent.json"), []byte(agentJSON), 0644)

	runCalled := false
	mockRT := &runtime.MockRuntime{
		ListFunc: func(ctx context.Context, labelFilter map[string]string) ([]api.AgentInfo, error) {
			return []api.AgentInfo{}, nil
		},
		RunFunc: func(ctx context.Context, config runtime.RunConfig) (string, error) {
			runCalled = true
			return "mock-id", nil
		},
	}

	mgr := NewManager(mockRT)
	_, err := mgr.Start(context.Background(), api.StartOptions{
		Name:            "nested-bare-agent",
		ProjectPath:     projectScionDir,
		NoAuth:          true,
		SharedWorkspace: true,
	})
	if err == nil {
		t.Fatal("expected Start to refuse a workspace source when the resolved repo root belongs to a different (enclosing) repository")
	}
	if !strings.Contains(err.Error(), "does not resolve to its own git work tree") {
		t.Errorf("expected the rejection to say the project does not resolve to its own git work tree, got: %v", err)
	}
	if !strings.Contains(err.Error(), projectScionDir) {
		t.Errorf("expected the rejection to include the project directory path, got: %v", err)
	}
	if runCalled {
		t.Error("expected the runtime's Run to never be invoked (fail closed), but it was called")
	}
}

// TestStart_AcceptsExistingWorktreeOutsideRepoRoot is a regression test: a
// git project attaching to an existing worktree that
// lives outside the repo root (provision.go's FindWorktreeByBranchIn path,
// e.g. from a worktree created directly with `git worktree add
// ../repo-feature`, or the legacy `.scion_worktrees` layout) must still be
// accepted at Start() -- it worked before the workspace source validator
// existed, via buildCommonRunArgs's "workspace outside repo root" fallback,
// and must keep working now that Start() validates ahead of that.
func TestStart_AcceptsExistingWorktreeOutsideRepoRoot(t *testing.T) {
	// This test creates a real git worktree and needs isGit detection in
	// ProvisionAgent to actually run: it's forced off when SCION_HOST_UID is
	// set (the "running inside an agent container" guard), which this test
	// binary's own environment may set regardless of the test.
	t.Setenv("SCION_HOST_UID", "")

	tmpDir := t.TempDir()

	oldWd, _ := os.Getwd()
	_ = os.Chdir(tmpDir)
	defer func() { _ = os.Chdir(oldWd) }()

	originalHome := os.Getenv("HOME")
	defer func() { _ = os.Setenv("HOME", originalHome) }()
	_ = os.Setenv("HOME", tmpDir)

	globalScionDir := filepath.Join(tmpDir, ".scion")
	hcDir := filepath.Join(globalScionDir, "harness-configs", "test-harness")
	_ = os.MkdirAll(hcDir, 0755)
	_ = os.WriteFile(filepath.Join(hcDir, "config.yaml"), []byte("harness: gemini\nuser: scion\nimage: test-image:latest\n"), 0644)
	tplDir := filepath.Join(globalScionDir, "templates", "default")
	_ = os.MkdirAll(tplDir, 0755)
	_ = os.WriteFile(filepath.Join(tplDir, "scion-agent.json"), []byte(`{"default_harness_config": "test-harness"}`), 0644)
	_ = os.WriteFile(filepath.Join(globalScionDir, "settings.yaml"), []byte(`schema_version: "1"
active_profile: local
profiles:
  local:
    runtime: docker
`), 0644)

	agentName := "worktree-agent"
	targetBranch := api.Slugify(agentName)

	projectDir := filepath.Join(tmpDir, "project")
	setupTestGitRepoWithBranch(t, projectDir, targetBranch)
	projectScionDir := filepath.Join(projectDir, ".scion")
	_ = os.MkdirAll(projectScionDir, 0755)
	_ = os.WriteFile(filepath.Join(projectDir, ".gitignore"), []byte("agents/\n"), 0644)

	// Attach an existing worktree for targetBranch OUTSIDE the repo root, as
	// a plain `git worktree add` sibling would.
	existingWorktree := filepath.Join(tmpDir, "project-worktree-sibling")
	addCmd := exec.Command("git", "-C", projectDir, "worktree", "add", existingWorktree, targetBranch)
	if out, err := addCmd.CombinedOutput(); err != nil {
		t.Fatalf("git worktree add: %v: %s", err, out)
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

	// The process stays in tmpDir, outside the project repository, as on a
	// runtime broker: provision.go's branch/worktree lookups run against
	// the project repository, not the process's cwd (ptone/scion#4439).

	_, err := mgr.Start(context.Background(), api.StartOptions{
		Name:        agentName,
		ProjectPath: projectScionDir,
		NoAuth:      true,
	})
	if err != nil {
		t.Fatalf("expected Start to accept an existing worktree outside the repo root, got error: %v", err)
	}

	wantWorkspace, evalErr := filepath.EvalSymlinks(existingWorktree)
	if evalErr != nil {
		t.Fatalf("EvalSymlinks(%q): %v", existingWorktree, evalErr)
	}
	if capturedConfig.Workspace != wantWorkspace {
		t.Errorf("RunConfig.Workspace = %q, want %q", capturedConfig.Workspace, wantWorkspace)
	}
}

// TestStart_RejectsNonWorktreeSourceOutsideRepoRoot is the negative sibling
// of TestStart_AcceptsExistingWorktreeOutsideRepoRoot: a source outside the
// repo root that is NOT a registered worktree of that repository (git
// verification correctly returns false) must still be rejected. This is
// also the missing "Start() non-explicit source outside root" containment
// reject case.
//
// StartOptions.SharedWorkspace is set to true below, which keeps
// GetAgent's agentWorkspace empty throughout -- without it, GetAgent's
// resume path self-heals a missing <agentDir>/workspace by recreating a
// managed worktree there (the project directory here is a real git repo,
// and the target branch already exists), regardless of the persisted
// volume's own source, which would silently replace the very path this
// test needs to stay in place: an environment where that recreation isn't
// itself blocked (SCION_HOST_UID unset) would make Start() succeed with
// the freshly recreated worktree, never exercising the containment check
// this test exists to prove. See TestStart_RejectsStaleRecreatedWorktree
// for the same pattern.
func TestStart_RejectsNonWorktreeSourceOutsideRepoRoot(t *testing.T) {
	t.Setenv("SCION_HOST_UID", "")

	tmpDir := t.TempDir()

	oldWd, _ := os.Getwd()
	_ = os.Chdir(tmpDir)
	defer func() { _ = os.Chdir(oldWd) }()

	originalHome := os.Getenv("HOME")
	defer func() { _ = os.Setenv("HOME", originalHome) }()
	_ = os.Setenv("HOME", tmpDir)

	globalScionDir := filepath.Join(tmpDir, ".scion")
	hcDir := filepath.Join(globalScionDir, "harness-configs", "test-harness")
	_ = os.MkdirAll(hcDir, 0755)
	_ = os.WriteFile(filepath.Join(hcDir, "config.yaml"), []byte("harness: gemini\nuser: scion\nimage: test-image:latest\n"), 0644)
	tplDir := filepath.Join(globalScionDir, "templates", "default")
	_ = os.MkdirAll(tplDir, 0755)
	_ = os.WriteFile(filepath.Join(tplDir, "scion-agent.json"), []byte(`{"default_harness_config": "test-harness"}`), 0644)
	_ = os.WriteFile(filepath.Join(globalScionDir, "settings.yaml"), []byte(`schema_version: "1"
active_profile: local
profiles:
  local:
    runtime: docker
`), 0644)

	agentName := "non-worktree-agent"
	projectDir := filepath.Join(tmpDir, "project")
	setupTestGitRepoWithBranch(t, projectDir, api.Slugify(agentName))
	projectScionDir := filepath.Join(projectDir, ".scion")
	_ = os.MkdirAll(projectScionDir, 0755)
	_ = os.WriteFile(filepath.Join(projectDir, ".gitignore"), []byte("agents/\n"), 0644)

	// A directory that merely sits outside the repo -- never registered
	// with git as a worktree of it -- passed as a persisted, non-explicit
	// workspace source (the shape a bad or stale value would take).
	notAWorktree := filepath.Join(tmpDir, "not-a-worktree")
	_ = os.MkdirAll(notAWorktree, 0755)

	runCalled := false
	mockRT := &runtime.MockRuntime{
		ListFunc: func(ctx context.Context, labelFilter map[string]string) ([]api.AgentInfo, error) {
			return []api.AgentInfo{}, nil
		},
		RunFunc: func(ctx context.Context, config runtime.RunConfig) (string, error) {
			runCalled = true
			return "mock-id", nil
		},
	}

	agentDir := filepath.Join(projectScionDir, "agents", agentName)
	_ = os.MkdirAll(filepath.Join(agentDir, "home"), 0755)
	agentJSON := fmt.Sprintf(`{"harness": "generic", "volumes": [{"source": %q, "target": "/workspace"}]}`, notAWorktree)
	_ = os.WriteFile(filepath.Join(agentDir, "scion-agent.json"), []byte(agentJSON), 0644)

	mgr := NewManager(mockRT)
	_, err := mgr.Start(context.Background(), api.StartOptions{
		Name:            agentName,
		ProjectPath:     projectScionDir,
		NoAuth:          true,
		SharedWorkspace: true,
	})
	if err == nil {
		t.Fatal("expected Start to reject a non-worktree source outside the repo root")
	}
	if !strings.Contains(err.Error(), "is outside the permitted workspace root") {
		t.Errorf("expected the rejection to come from containment, got: %v", err)
	}
	if runCalled {
		t.Error("expected the runtime's Run to never be invoked (fail closed), but it was called")
	}
}

// TestStart_RejectsAnotherRepositoryWorktree is the caller-context
// regression test for a worktree-membership bug: containment must be
// verified against THIS project's own repo, derived from projectDir, never
// against whatever repo the source itself happens to belong to. A worktree
// (main or linked) of a completely different repository must be refused at
// Start(), even though util.IsRegisteredWorktree would report it as a real,
// non-prunable registration -- just of the wrong repo.
//
// The other repository's worktree reaches Start() as the agent's persisted
// /workspace source. (It used to reach it through provision.go's branch and
// worktree lookups, which ran against the process's current directory; they
// now run against the project's own repository, ptone/scion#4439 -- see
// TestStart_IgnoresSameNamedBranchInCwdRepository.) SharedWorkspace keeps
// GetAgent's agentWorkspace empty so the persisted source stays in place;
// see TestStart_RejectsNonWorktreeSourceOutsideRepoRoot for the pattern.
func TestStart_RejectsAnotherRepositoryWorktree(t *testing.T) {
	t.Setenv("SCION_HOST_UID", "")

	tmpDir := t.TempDir()

	originalHome := os.Getenv("HOME")
	defer func() { _ = os.Setenv("HOME", originalHome) }()
	_ = os.Setenv("HOME", tmpDir)

	globalScionDir := filepath.Join(tmpDir, ".scion")
	hcDir := filepath.Join(globalScionDir, "harness-configs", "test-harness")
	_ = os.MkdirAll(hcDir, 0755)
	_ = os.WriteFile(filepath.Join(hcDir, "config.yaml"), []byte("harness: gemini\nuser: scion\nimage: test-image:latest\n"), 0644)
	tplDir := filepath.Join(globalScionDir, "templates", "default")
	_ = os.MkdirAll(tplDir, 0755)
	_ = os.WriteFile(filepath.Join(tplDir, "scion-agent.json"), []byte(`{"default_harness_config": "test-harness"}`), 0644)
	_ = os.WriteFile(filepath.Join(globalScionDir, "settings.yaml"), []byte(`schema_version: "1"
active_profile: local
profiles:
  local:
    runtime: docker
`), 0644)

	agentName := "cross-repo-agent"
	branch := api.Slugify(agentName)

	// The agent's own project -- has the branch, but no worktree for it.
	projectDir := filepath.Join(tmpDir, "project")
	setupTestGitRepoWithBranch(t, projectDir, branch)
	projectScionDir := filepath.Join(projectDir, ".scion")
	_ = os.MkdirAll(projectScionDir, 0755)
	_ = os.WriteFile(filepath.Join(projectDir, ".gitignore"), []byte("agents/\n"), 0644)

	// Two completely different, unrelated repositories that happen to use
	// the SAME branch name -- one with that branch checked out in its main
	// worktree, the other with it checked out in a linked worktree instead
	// (git refuses to check the same branch out twice in one repo, so this
	// needs two separate repos to cover both shapes).
	otherRepoMain := filepath.Join(tmpDir, "other-repo-main")
	setupTestGitRepoWithBranch(t, otherRepoMain, branch)
	checkoutCmd := exec.Command("git", "-C", otherRepoMain, "checkout", branch)
	if out, err := checkoutCmd.CombinedOutput(); err != nil {
		t.Fatalf("git checkout: %v: %s", err, out)
	}

	otherRepoLinked := filepath.Join(tmpDir, "other-repo-linked")
	setupTestGitRepoWithBranch(t, otherRepoLinked, branch)
	otherRepoWorktree := filepath.Join(tmpDir, "other-repo-linked-wt")
	addCmd := exec.Command("git", "-C", otherRepoLinked, "worktree", "add", otherRepoWorktree, branch)
	if out, err := addCmd.CombinedOutput(); err != nil {
		t.Fatalf("git worktree add: %v: %s", err, out)
	}

	t.Chdir(tmpDir)

	for _, tt := range []struct {
		name string
		// source is the persisted /workspace source: a worktree of the
		// OTHER repository, not the project being started.
		source string
	}{
		{name: "another repo's main worktree", source: otherRepoMain},
		{name: "another repo's linked worktree", source: otherRepoWorktree},
	} {
		t.Run(tt.name, func(t *testing.T) {
			runCalled := false
			mockRT := &runtime.MockRuntime{
				ListFunc: func(ctx context.Context, labelFilter map[string]string) ([]api.AgentInfo, error) {
					return []api.AgentInfo{}, nil
				},
				RunFunc: func(ctx context.Context, config runtime.RunConfig) (string, error) {
					runCalled = true
					return "mock-id", nil
				},
			}

			agentDir := filepath.Join(projectScionDir, "agents", agentName)
			_ = os.RemoveAll(agentDir)
			_ = os.MkdirAll(filepath.Join(agentDir, "home"), 0755)
			agentJSON := fmt.Sprintf(`{"harness": "generic", "volumes": [{"source": %q, "target": "/workspace"}]}`, tt.source)
			_ = os.WriteFile(filepath.Join(agentDir, "scion-agent.json"), []byte(agentJSON), 0644)

			mgr := NewManager(mockRT)
			_, err := mgr.Start(context.Background(), api.StartOptions{
				Name:            agentName,
				ProjectPath:     projectScionDir,
				NoAuth:          true,
				SharedWorkspace: true,
			})
			if err == nil {
				t.Fatal("expected Start to reject a worktree belonging to a different repository")
			}
			if !strings.Contains(err.Error(), "is outside the permitted workspace root") {
				t.Errorf("expected the rejection to come from containment, got: %v", err)
			}
			if runCalled {
				t.Error("expected the runtime's Run to never be invoked (fail closed), but it was called")
			}
		})
	}
}

// TestStart_IgnoresSameNamedBranchInCwdRepository checks that Start() never
// attaches to a worktree of whatever repository the process happens to be
// sitting in: provision.go's branch and worktree lookups run against the
// project's own repository (ptone/scion#4439). With the process inside an
// unrelated repository that has the same branch checked out in a linked
// worktree, the agent gets its own worktree in its own project.
func TestStart_IgnoresSameNamedBranchInCwdRepository(t *testing.T) {
	t.Setenv("SCION_HOST_UID", "")

	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	globalScionDir := filepath.Join(tmpDir, ".scion")
	hcDir := filepath.Join(globalScionDir, "harness-configs", "test-harness")
	_ = os.MkdirAll(hcDir, 0755)
	_ = os.WriteFile(filepath.Join(hcDir, "config.yaml"), []byte("harness: gemini\nuser: scion\nimage: test-image:latest\n"), 0644)
	tplDir := filepath.Join(globalScionDir, "templates", "default")
	_ = os.MkdirAll(tplDir, 0755)
	_ = os.WriteFile(filepath.Join(tplDir, "scion-agent.json"), []byte(`{"default_harness_config": "test-harness"}`), 0644)
	_ = os.WriteFile(filepath.Join(globalScionDir, "settings.yaml"), []byte(`schema_version: "1"
active_profile: local
profiles:
  local:
    runtime: docker
`), 0644)

	agentName := "cwd-repo-agent"
	branch := api.Slugify(agentName)

	projectDir := filepath.Join(tmpDir, "project")
	setupTestGitRepoWithBranch(t, projectDir, branch)
	projectScionDir := filepath.Join(projectDir, ".scion")
	_ = os.MkdirAll(projectScionDir, 0755)
	_ = os.WriteFile(filepath.Join(projectDir, ".gitignore"), []byte("agents/\n"), 0644)

	otherRepo := filepath.Join(tmpDir, "other-repo")
	setupTestGitRepoWithBranch(t, otherRepo, branch)
	otherWorktree := filepath.Join(tmpDir, "other-repo-wt")
	if out, err := exec.Command("git", "-C", otherRepo, "worktree", "add", otherWorktree, branch).CombinedOutput(); err != nil {
		t.Fatalf("git worktree add: %v: %s", err, out)
	}
	t.Chdir(otherRepo)

	var captured runtime.RunConfig
	mockRT := &runtime.MockRuntime{
		ListFunc: func(ctx context.Context, labelFilter map[string]string) ([]api.AgentInfo, error) {
			return []api.AgentInfo{}, nil
		},
		RunFunc: func(ctx context.Context, config runtime.RunConfig) (string, error) {
			captured = config
			return "mock-id", nil
		},
	}
	if _, err := NewManager(mockRT).Start(context.Background(), api.StartOptions{
		Name:        agentName,
		ProjectPath: projectScionDir,
		NoAuth:      true,
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	ownWorktree := filepath.Join(projectScionDir, "agents", agentName, "workspace")
	if _, err := os.Stat(filepath.Join(ownWorktree, ".git")); err != nil {
		t.Fatalf("expected the agent's own worktree at %s: %v", ownWorktree, err)
	}
	wantWorkspace, _ := filepath.EvalSymlinks(ownWorktree)
	gotWorkspace, _ := filepath.EvalSymlinks(captured.Workspace)
	if gotWorkspace != wantWorkspace {
		t.Errorf("RunConfig.Workspace = %q, want the project's own worktree %q (not the cwd repository's %q)", captured.Workspace, ownWorktree, otherWorktree)
	}
}

// TestStart_RejectsRegisteredWorktreeResolvingToHome is the Start()-level
// complement to TestValidateWorkspaceSource_UnusableSuppliedRootIsRejected
// (the precise unit test, in workspace_source_guard_test.go, for the rule
// that worktree/root verification decides whether repo-root containment
// applies, never whether the universal '/' and $HOME deny-set floor
// applies -- a supplied or verified root of $HOME is refused outright, not
// silently treated as "no root"). This test exercises the real git-aware
// code path in Start(): a git project whose effectiveWorkspace resolves to
// $HOME must still be rejected, regardless of whether git worktree
// verification runs against it.
func TestStart_RejectsRegisteredWorktreeResolvingToHome(t *testing.T) {
	t.Setenv("SCION_HOST_UID", "")

	tmpDir := t.TempDir()

	oldWd, _ := os.Getwd()
	_ = os.Chdir(tmpDir)
	defer func() { _ = os.Chdir(oldWd) }()

	agentName := "home-worktree-agent"
	projectDir := filepath.Join(tmpDir, "project")
	setupTestGitRepoWithBranch(t, projectDir, api.Slugify(agentName))

	// A REAL worktree, registered with the project's own repo, whose path
	// becomes $HOME -- not just a directory that happens to equal $HOME.
	// Even a git-verified worktree of the project's own repo must still be
	// refused if it resolves to $HOME, because the deny-set floor runs
	// unconditionally before worktree membership is ever considered.
	homeWorktree := filepath.Join(tmpDir, "home-wt")
	addCmd := exec.Command("git", "-C", projectDir, "worktree", "add", homeWorktree, api.Slugify(agentName))
	if out, err := addCmd.CombinedOutput(); err != nil {
		t.Fatalf("git worktree add: %v: %s", err, out)
	}

	originalHome := os.Getenv("HOME")
	defer func() { _ = os.Setenv("HOME", originalHome) }()
	_ = os.Setenv("HOME", homeWorktree)

	globalScionDir := filepath.Join(homeWorktree, ".scion")
	hcDir := filepath.Join(globalScionDir, "harness-configs", "test-harness")
	_ = os.MkdirAll(hcDir, 0755)
	_ = os.WriteFile(filepath.Join(hcDir, "config.yaml"), []byte("harness: gemini\nuser: scion\nimage: test-image:latest\n"), 0644)
	tplDir := filepath.Join(globalScionDir, "templates", "default")
	_ = os.MkdirAll(tplDir, 0755)
	_ = os.WriteFile(filepath.Join(tplDir, "scion-agent.json"), []byte(`{"default_harness_config": "test-harness"}`), 0644)
	_ = os.WriteFile(filepath.Join(globalScionDir, "settings.yaml"), []byte(`schema_version: "1"
active_profile: local
profiles:
  local:
    runtime: docker
`), 0644)

	projectScionDir := filepath.Join(projectDir, ".scion")
	_ = os.MkdirAll(projectScionDir, 0755)
	_ = os.WriteFile(filepath.Join(projectDir, ".gitignore"), []byte("agents/\n"), 0644)

	// A persisted config whose workspace volume is $HOME itself (the real
	// registered worktree from above). Membership deciding whether repo-root
	// containment applies must never override the unconditional $HOME
	// refusal that runs first.
	runCalled := false
	mockRT := &runtime.MockRuntime{
		ListFunc: func(ctx context.Context, labelFilter map[string]string) ([]api.AgentInfo, error) {
			return []api.AgentInfo{}, nil
		},
		RunFunc: func(ctx context.Context, config runtime.RunConfig) (string, error) {
			runCalled = true
			return "mock-id", nil
		},
	}

	agentDir := filepath.Join(projectScionDir, "agents", agentName)
	_ = os.MkdirAll(filepath.Join(agentDir, "home"), 0755)
	agentJSON := fmt.Sprintf(`{"harness": "generic", "volumes": [{"source": %q, "target": "/workspace"}]}`, homeWorktree)
	_ = os.WriteFile(filepath.Join(agentDir, "scion-agent.json"), []byte(agentJSON), 0644)

	mgr := NewManager(mockRT)
	_, err := mgr.Start(context.Background(), api.StartOptions{
		Name:        agentName,
		ProjectPath: projectScionDir,
		NoAuth:      true,
	})
	if err == nil {
		t.Fatal("expected Start to reject a registered worktree of the project's own repo that resolves to $HOME")
	}
	if !strings.Contains(err.Error(), "is not an allowed workspace path") {
		t.Errorf("expected the rejection to come from the deny-set floor, got: %v", err)
	}
	if runCalled {
		t.Error("expected the runtime's Run to never be invoked (fail closed), but it was called")
	}
}

// TestStart_RejectsStaleRecreatedWorktree is the Start()-level counterpart
// to the unit-level TestIsRegisteredWorktree_PrunableRecreatedPathRejected:
// it exercises the same prunable-worktree-recreated-as-a-plain-directory
// shape through Start() itself.
// StartOptions.SharedWorkspace is set to true below, which keeps
// GetAgent's agentWorkspace empty throughout -- without it, GetAgent's
// resume path self-heals a missing <agentDir>/workspace by recreating a
// managed worktree there regardless of the persisted volume's own source,
// which would silently replace the very path this test needs to stay stale.
func TestStart_RejectsStaleRecreatedWorktree(t *testing.T) {
	t.Setenv("SCION_HOST_UID", "")

	tmpDir := t.TempDir()

	oldWd, _ := os.Getwd()
	_ = os.Chdir(tmpDir)
	defer func() { _ = os.Chdir(oldWd) }()

	originalHome := os.Getenv("HOME")
	defer func() { _ = os.Setenv("HOME", originalHome) }()
	_ = os.Setenv("HOME", tmpDir)

	globalScionDir := filepath.Join(tmpDir, ".scion")
	hcDir := filepath.Join(globalScionDir, "harness-configs", "test-harness")
	_ = os.MkdirAll(hcDir, 0755)
	_ = os.WriteFile(filepath.Join(hcDir, "config.yaml"), []byte("harness: gemini\nuser: scion\nimage: test-image:latest\n"), 0644)
	tplDir := filepath.Join(globalScionDir, "templates", "default")
	_ = os.MkdirAll(tplDir, 0755)
	_ = os.WriteFile(filepath.Join(tplDir, "scion-agent.json"), []byte(`{"default_harness_config": "test-harness"}`), 0644)
	_ = os.WriteFile(filepath.Join(globalScionDir, "settings.yaml"), []byte(`schema_version: "1"
active_profile: local
profiles:
  local:
    runtime: docker
`), 0644)

	agentName := "stale-worktree-agent"
	projectDir := filepath.Join(tmpDir, "project")
	setupTestGitRepoWithBranch(t, projectDir, api.Slugify(agentName))
	projectScionDir := filepath.Join(projectDir, ".scion")
	_ = os.MkdirAll(projectScionDir, 0755)
	_ = os.WriteFile(filepath.Join(projectDir, ".gitignore"), []byte("agents/\n"), 0644)

	// A real worktree, registered with the project's own repo...
	staleWorktree := filepath.Join(tmpDir, "stale-wt")
	addCmd := exec.Command("git", "-C", projectDir, "worktree", "add", staleWorktree, api.Slugify(agentName))
	if out, err := addCmd.CombinedOutput(); err != nil {
		t.Fatalf("git worktree add: %v: %s", err, out)
	}
	// ...then removed directly (not via `git worktree remove`), leaving
	// git's own registration in place but pointing at a gitdir that no
	// longer resolves -- exactly what makes `git worktree list --porcelain`
	// report the entry as prunable...
	if err := os.RemoveAll(staleWorktree); err != nil {
		t.Fatal(err)
	}
	// ...and recreated as a plain directory at the same path: nothing
	// git-related, just a directory that happens to have the same name.
	if err := os.MkdirAll(staleWorktree, 0755); err != nil {
		t.Fatal(err)
	}

	runCalled := false
	mockRT := &runtime.MockRuntime{
		ListFunc: func(ctx context.Context, labelFilter map[string]string) ([]api.AgentInfo, error) {
			return []api.AgentInfo{}, nil
		},
		RunFunc: func(ctx context.Context, config runtime.RunConfig) (string, error) {
			runCalled = true
			return "mock-id", nil
		},
	}

	agentDir := filepath.Join(projectScionDir, "agents", agentName)
	_ = os.MkdirAll(filepath.Join(agentDir, "home"), 0755)
	agentJSON := fmt.Sprintf(`{"harness": "generic", "volumes": [{"source": %q, "target": "/workspace"}]}`, staleWorktree)
	_ = os.WriteFile(filepath.Join(agentDir, "scion-agent.json"), []byte(agentJSON), 0644)

	mgr := NewManager(mockRT)
	// SharedWorkspace: true keeps GetAgent's agentWorkspace empty throughout,
	// which skips the managed-worktree self-heal (recreating a fresh worktree
	// at <agentDir>/workspace when it's missing). Without this, self-heal
	// would prune the stale registration and recreate a valid worktree
	// there, masking the exact persisted-volume shape this test needs to
	// reach the containment check with.
	_, err := mgr.Start(context.Background(), api.StartOptions{
		Name:            agentName,
		ProjectPath:     projectScionDir,
		NoAuth:          true,
		SharedWorkspace: true,
	})
	if err == nil {
		t.Fatal("expected Start to reject a stale worktree registration recreated as a plain directory")
	}
	if !strings.Contains(err.Error(), "is outside the permitted workspace root") {
		t.Errorf("expected the rejection to come from containment (no longer a verified worktree, so only repo-root containment applies), got: %v", err)
	}
	if runCalled {
		t.Error("expected the runtime's Run to never be invoked (fail closed), but it was called")
	}
}

// TestStart_AcceptsMainWorktreeWhenProjectLivesInLinkedWorktree covers a
// project whose own .scion directory lives in a linked worktree rather than
// the repository's main one: the containment root workspaceSourceRoots
// derives from projectDir is that linked worktree's own path, not the main
// worktree's. A persisted source that is the repository's MAIN worktree
// must still be accepted -- it is the same repository, just reached from a
// project sitting in a different (linked) checkout of it -- which requires
// recognizing the main worktree structurally rather than by repoRoot
// equality (see util.IsRegisteredWorktree's doc comment).
func TestStart_AcceptsMainWorktreeWhenProjectLivesInLinkedWorktree(t *testing.T) {
	t.Setenv("SCION_HOST_UID", "")

	tmpDir := t.TempDir()

	oldWd, _ := os.Getwd()
	_ = os.Chdir(tmpDir)
	defer func() { _ = os.Chdir(oldWd) }()

	originalHome := os.Getenv("HOME")
	defer func() { _ = os.Setenv("HOME", originalHome) }()
	_ = os.Setenv("HOME", tmpDir)

	globalScionDir := filepath.Join(tmpDir, ".scion")
	hcDir := filepath.Join(globalScionDir, "harness-configs", "test-harness")
	_ = os.MkdirAll(hcDir, 0755)
	_ = os.WriteFile(filepath.Join(hcDir, "config.yaml"), []byte("harness: gemini\nuser: scion\nimage: test-image:latest\n"), 0644)
	tplDir := filepath.Join(globalScionDir, "templates", "default")
	_ = os.MkdirAll(tplDir, 0755)
	_ = os.WriteFile(filepath.Join(tplDir, "scion-agent.json"), []byte(`{"default_harness_config": "test-harness"}`), 0644)
	_ = os.WriteFile(filepath.Join(globalScionDir, "settings.yaml"), []byte(`schema_version: "1"
active_profile: local
profiles:
  local:
    runtime: docker
`), 0644)

	agentName := "linked-project-agent"

	// The repository's main worktree.
	mainWorktree := filepath.Join(tmpDir, "main-repo")
	setupTestGitRepoWithBranch(t, mainWorktree, "linked-branch")

	// A linked worktree of the SAME repository -- this is where the
	// project's own .scion directory lives, not the main worktree.
	linkedWorktree := filepath.Join(tmpDir, "linked-repo")
	addCmd := exec.Command("git", "-C", mainWorktree, "worktree", "add", linkedWorktree, "linked-branch")
	if out, err := addCmd.CombinedOutput(); err != nil {
		t.Fatalf("git worktree add: %v: %s", err, out)
	}

	projectScionDir := filepath.Join(linkedWorktree, ".scion")
	_ = os.MkdirAll(projectScionDir, 0755)
	_ = os.WriteFile(filepath.Join(linkedWorktree, ".gitignore"), []byte("agents/\n"), 0644)

	// The persisted source is the repository's MAIN worktree -- a
	// legitimate location in the same repository as the project, reached
	// from a project living in a different (linked) checkout of it.
	runCalled := false
	var capturedConfig runtime.RunConfig
	mockRT := &runtime.MockRuntime{
		ListFunc: func(ctx context.Context, labelFilter map[string]string) ([]api.AgentInfo, error) {
			return []api.AgentInfo{}, nil
		},
		RunFunc: func(ctx context.Context, config runtime.RunConfig) (string, error) {
			runCalled = true
			capturedConfig = config
			return "mock-id", nil
		},
	}

	agentDir := filepath.Join(projectScionDir, "agents", agentName)
	_ = os.MkdirAll(filepath.Join(agentDir, "home"), 0755)
	agentJSON := fmt.Sprintf(`{"harness": "generic", "volumes": [{"source": %q, "target": "/workspace"}]}`, mainWorktree)
	_ = os.WriteFile(filepath.Join(agentDir, "scion-agent.json"), []byte(agentJSON), 0644)

	mgr := NewManager(mockRT)
	_, err := mgr.Start(context.Background(), api.StartOptions{
		Name:            agentName,
		ProjectPath:     projectScionDir,
		NoAuth:          true,
		SharedWorkspace: true,
	})
	if err != nil {
		t.Fatalf("expected Start to accept the repository's main worktree as a source for a project living in a linked worktree, got error: %v", err)
	}
	if !runCalled {
		t.Fatal("expected the runtime's Run to be invoked")
	}
	wantWorkspace, evalErr := filepath.EvalSymlinks(mainWorktree)
	if evalErr != nil {
		t.Fatalf("EvalSymlinks(%q): %v", mainWorktree, evalErr)
	}
	if capturedConfig.Workspace != wantWorkspace {
		t.Errorf("RunConfig.Workspace = %q, want %q", capturedConfig.Workspace, wantWorkspace)
	}
}

// TestStart_FiltersUnsafeWorkspaceVolumeInWorktreeCase is the regression test
// for the gap in the worktree case (agentWorkspace set, not volume-derived):
// buildCommonRunArgs mounts a worktree-subdirectory workspace at
// /repo-root/<rel>, not /workspace, leaving the /workspace target slot free
// for a raw, unvalidated volume from a persisted config to claim. A /workspace
// -target volume must be filtered out of the generic volume list whenever
// there is an effective workspace at all, not only when effectiveWorkspace
// happens to differ from agentWorkspace by value (a comparison that silently
// flips depending on symlink canonicalization -- see the comment at the
// Volumes builder in Start()).
func TestStart_FiltersUnsafeWorkspaceVolumeInWorktreeCase(t *testing.T) {
	t.Setenv("SCION_HOST_UID", "")

	tmpDir := t.TempDir()

	oldWd, _ := os.Getwd()
	_ = os.Chdir(tmpDir)
	defer func() { _ = os.Chdir(oldWd) }()

	originalHome := os.Getenv("HOME")
	defer func() { _ = os.Setenv("HOME", originalHome) }()
	_ = os.Setenv("HOME", tmpDir)

	globalScionDir := filepath.Join(tmpDir, ".scion")
	hcDir := filepath.Join(globalScionDir, "harness-configs", "test-harness")
	_ = os.MkdirAll(hcDir, 0755)
	_ = os.WriteFile(filepath.Join(hcDir, "config.yaml"), []byte("harness: gemini\nuser: scion\nimage: test-image:latest\n"), 0644)
	tplDir := filepath.Join(globalScionDir, "templates", "default")
	_ = os.MkdirAll(tplDir, 0755)
	_ = os.WriteFile(filepath.Join(tplDir, "scion-agent.json"), []byte(`{"default_harness_config": "test-harness"}`), 0644)
	_ = os.WriteFile(filepath.Join(globalScionDir, "settings.yaml"), []byte(`schema_version: "1"
active_profile: local
profiles:
  local:
    runtime: docker
`), 0644)

	agentName := "fresh-worktree-agent"
	projectDir := filepath.Join(tmpDir, "project")
	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = projectDir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	_ = os.MkdirAll(projectDir, 0755)
	run("init")
	run("config", "user.email", "you@example.com")
	run("config", "user.name", "Your Name")
	run("commit", "--allow-empty", "-m", "root commit")
	// Deliberately do NOT pre-create the agent's target branch, so
	// ProvisionAgent creates a fresh managed worktree (agentWorkspace set)
	// rather than attaching to an existing one.
	projectScionDir := filepath.Join(projectDir, ".scion")
	_ = os.MkdirAll(projectScionDir, 0755)
	_ = os.WriteFile(filepath.Join(projectDir, ".gitignore"), []byte("agents/\n"), 0644)

	mockRT := &runtime.MockRuntime{
		ListFunc: func(ctx context.Context, labelFilter map[string]string) ([]api.AgentInfo, error) {
			return []api.AgentInfo{}, nil
		},
		RunFunc: func(ctx context.Context, config runtime.RunConfig) (string, error) {
			return "mock-id", nil
		},
	}
	mgr := NewManager(mockRT)

	if err := os.Chdir(projectDir); err != nil {
		t.Fatal(err)
	}
	if _, err := mgr.Start(context.Background(), api.StartOptions{
		Name:        agentName,
		ProjectPath: projectScionDir,
		NoAuth:      true,
	}); err != nil {
		t.Fatalf("initial Start failed: %v", err)
	}

	agentDir := filepath.Join(projectScionDir, "agents", agentName)
	agentWorkspace := filepath.Join(agentDir, "workspace")
	if _, err := os.Stat(agentWorkspace); err != nil {
		t.Fatalf("expected a managed worktree at %s: %v", agentWorkspace, err)
	}

	// Simulate a template or a buggy persisted config with an extra
	// /workspace-target volume alongside the legitimate worktree-derived
	// workspace.
	agentConfigPath := filepath.Join(agentDir, "scion-agent.json")
	raw, err := os.ReadFile(agentConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	var cfg map[string]interface{}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	cfg["volumes"] = []map[string]interface{}{
		{"source": "/", "target": "/workspace"},
	}
	rewritten, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(agentConfigPath, rewritten, 0644); err != nil {
		t.Fatal(err)
	}

	var capturedConfig runtime.RunConfig
	mockRT.RunFunc = func(ctx context.Context, config runtime.RunConfig) (string, error) {
		capturedConfig = config
		return "mock-id-2", nil
	}

	info, err := mgr.Start(context.Background(), api.StartOptions{
		Name:        agentName,
		ProjectPath: projectScionDir,
		NoAuth:      true,
	})
	if err != nil {
		t.Fatalf("resume Start failed: %v", err)
	}

	for _, v := range capturedConfig.Volumes {
		if v.Target == "/workspace" {
			t.Errorf("expected the extra /workspace-target volume to be filtered out, found: %+v", v)
		}
	}

	foundWarning := false
	for _, w := range info.Warnings {
		if strings.Contains(w, "/workspace") {
			foundWarning = true
		}
	}
	if !foundWarning {
		t.Errorf("expected a warning about the dropped /workspace volume, got warnings: %v", info.Warnings)
	}

	wantWorkspace, evalErr := filepath.EvalSymlinks(agentWorkspace)
	if evalErr != nil {
		t.Fatalf("EvalSymlinks(%q): %v", agentWorkspace, evalErr)
	}
	if capturedConfig.Workspace != wantWorkspace {
		t.Errorf("RunConfig.Workspace = %q, want the legitimate managed worktree %q", capturedConfig.Workspace, wantWorkspace)
	}
}
