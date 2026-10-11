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
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

// TestStart_RejectsPreExistingGlobalAgentWorkspaceOutsideProjectDir covers
// the intended edge case in the global project's containment root: a global
// agent provisioned before <projectDir>/workspace existed (or otherwise
// persisted with a workspace outside it) is refused on resume, now that the
// global project's root is <projectDir>/workspace itself. This is a
// deliberate behavior change, not a bug -- the error must name both ways to
// recover (recreate the agent, or restart with an explicit --workspace),
// since neither is obvious from the base rejection alone, whichever of
// ValidateWorkspaceSource's two base messages ("outside the permitted
// workspace root" for plain containment, "not an allowed workspace path" for
// the ~/.scion floor) a given case happens to surface.
func TestStart_RejectsPreExistingGlobalAgentWorkspaceOutsideProjectDir(t *testing.T) {
	for _, tt := range []struct {
		name string
		// workspace, relative to tmpDir (== $HOME for this test), is the
		// persisted volume source to simulate. "$HOME/wherever-cli-was-run-from"
		// is outside ~/.scion entirely, so it would be refused under either
		// root and does not by itself prove which root is actually being
		// enforced. "~/.scion/templates" is the discriminating case: a
		// ~/.scion root would accept it; the ~/.scion/workspace root
		// refuses it, so only this case tells the two apart.
		workspace string
		// wantBaseError is the base-rejection substring each case surfaces.
		// The two cases fail closed for different reasons: the first is
		// outside ~/.scion entirely, so containment against the global
		// project's own root (~/.scion/workspace) is what refuses it; the
		// second is under ~/.scion but not in the named allow list, so the
		// unconditional ~/.scion floor (ValidateWorkspaceSource) refuses it
		// before containment is even checked.
		wantBaseError string
	}{
		{name: "workspace outside $HOME/.scion entirely", workspace: "wherever-cli-was-run-from", wantBaseError: "is outside the permitted workspace root"},
		{name: "workspace under $HOME/.scion but not .scion/workspace", workspace: filepath.Join(".scion", "templates"), wantBaseError: "is not an allowed workspace path"},
	} {
		t.Run(tt.name, func(t *testing.T) {
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

			// Simulate a workspace persisted by a global agent from before the
			// current containment root (<projectDir>/workspace) took effect.
			persistedWorkspace := filepath.Join(tmpDir, tt.workspace)
			_ = os.MkdirAll(persistedWorkspace, 0755)

			agentName := "pre-existing-global-agent"
			agentDir := filepath.Join(globalScionDir, "agents", agentName)
			_ = os.MkdirAll(filepath.Join(agentDir, "home"), 0755)
			agentJSON := fmt.Sprintf(`{"harness": "generic", "volumes": [{"source": %q, "target": "/workspace"}]}`, persistedWorkspace)
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
				Name:        agentName,
				ProjectPath: globalScionDir,
				NoAuth:      true,
			})
			if err == nil {
				t.Fatal("expected Start to reject a pre-existing global agent's workspace outside <projectDir>/workspace")
			}
			if !strings.Contains(err.Error(), tt.wantBaseError) {
				t.Errorf("expected the base rejection to contain %q, got: %v", tt.wantBaseError, err)
			}
			if !strings.Contains(err.Error(), "recreate") || !strings.Contains(err.Error(), "--workspace") {
				t.Errorf("expected an actionable error naming both recovery paths (recreate the agent, or an explicit --workspace), got: %v", err)
			}
			if runCalled {
				t.Error("expected the runtime's Run to never be invoked (fail closed), but it was called")
			}
		})
	}
}

// TestStart_RejectsGlobalProjectInsideGitWorkTree covers the global project
// whose own directory (~/.scion) is itself a git work tree -- a dotfiles
// repository, for example. isGitWorkspaceProject then routes it through
// workspaceSourceRoots' git branch, so ProvisionAgent creates a per-agent
// worktree/workspace at ~/.scion/agents/<name>/workspace, the ordinary git-
// project shape, not <projectDir>/workspace. That source can never pass the
// ~/.scion floor (only ~/.scion/workspace and ~/.scion/projects/<slug> are
// admitted), and root == ~/.scion itself is refused outright as a
// misconfigured root, so the agent -- freshly created by this same call --
// must fail closed with its own actionable message, not the generic
// "delete and recreate" hint that cannot help a shape this fixed.
func TestStart_RejectsGlobalProjectInsideGitWorkTree(t *testing.T) {
	t.Setenv("SCION_HOST_UID", "")

	tmpDir := t.TempDir()

	oldWd, _ := os.Getwd()
	_ = os.Chdir(tmpDir)
	defer func() { _ = os.Chdir(oldWd) }()

	originalHome := os.Getenv("HOME")
	defer func() { _ = os.Setenv("HOME", originalHome) }()
	_ = os.Setenv("HOME", tmpDir)

	if oldAutoExpose, ok := os.LookupEnv("SCION_AUTO_EXPOSE_PORTS"); ok {
		_ = os.Unsetenv("SCION_AUTO_EXPOSE_PORTS")
		defer func() { _ = os.Setenv("SCION_AUTO_EXPOSE_PORTS", oldAutoExpose) }()
	}

	globalScionDir := filepath.Join(tmpDir, ".scion")
	// ~/.scion is itself a git work tree -- the dotfiles-repository shape.
	// agents/ must be gitignored, the same requirement provision.go
	// enforces for any project-local git project, checked before
	// workspaceSourceRoots's own global-project handling runs.
	if err := os.MkdirAll(globalScionDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(globalScionDir, ".gitignore"), []byte("agents/\n"), 0644); err != nil {
		t.Fatal(err)
	}
	setupTestGitRepoWithBranch(t, globalScionDir, "unused-branch")

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
		Name:        "git-tracked-global-agent",
		ProjectPath: globalScionDir,
		NoAuth:      true,
	})
	if err == nil {
		t.Fatal("expected Start to refuse a fresh global agent when ~/.scion is itself a git work tree")
	}
	if !strings.Contains(err.Error(), "is itself inside a git work tree") {
		t.Errorf("expected an error naming the actual shape (global project inside a git work tree), got: %v", err)
	}
	if !strings.Contains(err.Error(), "--workspace") {
		t.Errorf("expected the error to name the actual recovery path (an explicit --workspace), got: %v", err)
	}
	if strings.Contains(err.Error(), "delete and recreate") {
		t.Errorf("expected the generic recreate hint to be absent -- recreating cannot fix this shape, got: %v", err)
	}
	if runCalled {
		t.Error("expected the runtime's Run to never be invoked (fail closed), but it was called")
	}
}

// TestStart_AcceptsOrdinaryGitRepoNamedGlobal covers an ordinary git
// project whose own repository root happens to be named "global" -- not
// the real global project, which is identified by its resolved directory,
// not by name. Start must treat it like any other git project: create a
// worktree and reach Run, rather than refusing it as if it were the global
// project's own directory misconfigured as a git work tree.
func TestStart_AcceptsOrdinaryGitRepoNamedGlobal(t *testing.T) {
	t.Setenv("SCION_HOST_UID", "")

	tmpDir := t.TempDir()

	oldWd, _ := os.Getwd()
	_ = os.Chdir(tmpDir)
	defer func() { _ = os.Chdir(oldWd) }()

	// HOME is a separate directory from the project below, so the real
	// global directory (HOME/.scion) and this project's own repository
	// root are unambiguously different paths.
	originalHome := os.Getenv("HOME")
	defer func() { _ = os.Setenv("HOME", originalHome) }()
	home := filepath.Join(tmpDir, "home")
	_ = os.Setenv("HOME", home)

	globalScionDir := filepath.Join(home, ".scion")
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

	agentName := "ordinary-agent"
	projectDir := filepath.Join(tmpDir, "global")
	setupTestGitRepoWithBranch(t, projectDir, api.Slugify(agentName))
	projectScionDir := filepath.Join(projectDir, ".scion")
	if err := os.MkdirAll(projectScionDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projectDir, ".gitignore"), []byte("agents/\n"), 0644); err != nil {
		t.Fatal(err)
	}

	if err := os.Chdir(projectDir); err != nil {
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
	mgr := NewManager(mockRT)

	_, err := mgr.Start(context.Background(), api.StartOptions{
		Name:        agentName,
		ProjectPath: projectScionDir,
		NoAuth:      true,
	})
	if err != nil {
		t.Fatalf("expected Start to accept an ordinary git repo named %q, got error: %v", filepath.Base(projectDir), err)
	}
	if !runCalled {
		t.Error("expected the runtime's Run to be invoked")
	}
}

// TestStart_GlobalProjectResumeUsesConsistentWorkspace covers restart/resume
// for a global-project agent: ProvisionAgent's Case 3 creates a per-agent
// subdirectory under ~/.scion/workspace (not a directory shared by every
// global agent), and a second Start() call for the same agent -- simulating
// a restart/resume, not a fresh provision -- must resolve to that exact
// same directory again, not recreate a new one or fall back to the bare
// ~/.scion/workspace root.
func TestStart_GlobalProjectResumeUsesConsistentWorkspace(t *testing.T) {
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

	agentName := "resumable-global-agent"
	var capturedWorkspaces []string
	mockRT := &runtime.MockRuntime{
		ListFunc: func(ctx context.Context, labelFilter map[string]string) ([]api.AgentInfo, error) {
			return []api.AgentInfo{}, nil
		},
		RunFunc: func(ctx context.Context, config runtime.RunConfig) (string, error) {
			capturedWorkspaces = append(capturedWorkspaces, config.Workspace)
			return "mock-id", nil
		},
	}
	mgr := NewManager(mockRT)

	for i := 0; i < 2; i++ {
		if _, err := mgr.Start(context.Background(), api.StartOptions{
			Name:        agentName,
			ProjectPath: globalScionDir,
			NoAuth:      true,
		}); err != nil {
			t.Fatalf("Start call %d failed: %v", i+1, err)
		}
	}

	if len(capturedWorkspaces) != 2 {
		t.Fatalf("expected 2 captured RunConfig.Workspace values, got %d: %v", len(capturedWorkspaces), capturedWorkspaces)
	}

	wantWorkspace, err := filepath.EvalSymlinks(filepath.Join(globalScionDir, "workspace", agentName))
	if err != nil {
		t.Fatalf("EvalSymlinks(want): %v", err)
	}
	for i, got := range capturedWorkspaces {
		evalGot, err := filepath.EvalSymlinks(got)
		if err != nil {
			t.Fatalf("EvalSymlinks(call %d): %v", i+1, err)
		}
		if evalGot != wantWorkspace {
			t.Errorf("Start call %d: RunConfig.Workspace = %q, want %q", i+1, evalGot, wantWorkspace)
		}
	}
	if capturedWorkspaces[0] != capturedWorkspaces[1] {
		t.Errorf("expected the first and second Start calls to resolve to the identical workspace value, got %q then %q", capturedWorkspaces[0], capturedWorkspaces[1])
	}
}

// setupHubMarkerProjectConfigsDir creates the externalized .scion directory
// a hub-dispatched project's marker file resolves to
// (config.ResolveProjectMarker's output: ~/.scion/project-configs/<dir>/.scion,
// pkg/config/project_marker.go's ExternalProjectPath), along with the
// harness-config, template and settings.yaml a fresh provision needs. It
// returns the resolved .scion directory to pass as StartOptions.ProjectPath.
// The marker file itself (~/.scion/projects/<slug>/.scion) is not created:
// Start() never reads it -- resolution happens upstream, in whichever
// dispatcher set ProjectPath -- so passing the already-resolved directory
// directly reproduces the shape Start() actually sees.
func setupHubMarkerProjectConfigsDir(t *testing.T, tmpHome, dirName string) string {
	t.Helper()
	projectConfigsDir := filepath.Join(tmpHome, ".scion", "project-configs", dirName)
	projectScionDir := filepath.Join(projectConfigsDir, ".scion")

	hcDir := filepath.Join(projectScionDir, "harness-configs", "test-harness")
	if err := os.MkdirAll(hcDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(hcDir, "config.yaml"), []byte("harness: gemini\nuser: scion\nimage: test-image:latest\n"), 0644); err != nil {
		t.Fatal(err)
	}
	tplDir := filepath.Join(projectScionDir, "templates", "default")
	if err := os.MkdirAll(tplDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tplDir, "scion-agent.json"), []byte(`{"default_harness_config": "test-harness"}`), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projectScionDir, "settings.yaml"), []byte("schema_version: \"1\"\nactive_profile: local\nprofiles:\n  local:\n    runtime: docker\n"), 0644); err != nil {
		t.Fatal(err)
	}
	return projectScionDir
}

// TestStart_AcceptsGitCloneHubMarkerWorkspace covers a hub-dispatched git
// project whose marker file resolves projectDir to its externalized
// ~/.scion/project-configs/<dir>/.scion (a split-storage layout: the
// project's own directory holds only a .scion marker file, not a full
// .scion directory). ProvisionAgent's git-clone branch creates the
// workspace at agentDir/workspace under that externalized directory
// (pkg/agent/provision.go), i.e.
// ~/.scion/project-configs/<dir>/.scion/agents/<agent>/workspace. Start()
// must accept that shape, not just the ~/.scion/projects/<slug> family.
func TestStart_AcceptsGitCloneHubMarkerWorkspace(t *testing.T) {
	t.Setenv("SCION_HOST_UID", "")

	tmpDir := t.TempDir()
	originalHome := os.Getenv("HOME")
	defer func() { _ = os.Setenv("HOME", originalHome) }()
	_ = os.Setenv("HOME", tmpDir)

	projectScionDir := setupHubMarkerProjectConfigsDir(t, tmpDir, "hub-slug__11111111")

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

	_, err := mgr.Start(context.Background(), api.StartOptions{
		Name:        "gc-agent",
		ProjectPath: projectScionDir,
		NoAuth:      true,
		GitClone:    &api.GitCloneConfig{URL: "https://example.com/repo.git"},
	})
	if err != nil {
		t.Fatalf("expected Start to accept a git-clone hub-marker workspace, got error: %v", err)
	}

	wantWorkspace := filepath.Join(projectScionDir, "agents", "gc-agent", "workspace")
	resolvedWant, evalErr := filepath.EvalSymlinks(wantWorkspace)
	if evalErr != nil {
		t.Fatalf("EvalSymlinks(%q): %v", wantWorkspace, evalErr)
	}
	if capturedConfig.Workspace != resolvedWant {
		t.Errorf("RunConfig.Workspace = %q, want %q", capturedConfig.Workspace, resolvedWant)
	}
}

// TestStart_AcceptsNonGitHubMarkerWorkspace covers the non-git counterpart:
// a hub-dispatched, non-git project whose marker file resolves projectDir to
// the same externalized ~/.scion/project-configs/<dir>/.scion shape, with no
// GitClone and no settings.WorkspacePath. ProvisionAgent resolves this to
// its own agentDir/workspace, the same per-agent shape the git-clone branch
// uses, rather than mounting the bare project-configs directory (which held
// only configuration, never the project's actual files).
func TestStart_AcceptsNonGitHubMarkerWorkspace(t *testing.T) {
	t.Setenv("SCION_HOST_UID", "")

	tmpDir := t.TempDir()
	originalHome := os.Getenv("HOME")
	defer func() { _ = os.Setenv("HOME", originalHome) }()
	_ = os.Setenv("HOME", tmpDir)

	projectScionDir := setupHubMarkerProjectConfigsDir(t, tmpDir, "hub-slug2__21111111")

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

	_, err := mgr.Start(context.Background(), api.StartOptions{
		Name:        "non-git-agent",
		ProjectPath: projectScionDir,
		NoAuth:      true,
	})
	if err != nil {
		t.Fatalf("expected Start to accept a non-git hub-marker workspace, got error: %v", err)
	}

	wantWorkspace := filepath.Join(projectScionDir, "agents", "non-git-agent", "workspace")
	resolvedWant, evalErr := filepath.EvalSymlinks(wantWorkspace)
	if evalErr != nil {
		t.Fatalf("EvalSymlinks(%q): %v", wantWorkspace, evalErr)
	}
	if capturedConfig.Workspace != resolvedWant {
		t.Errorf("RunConfig.Workspace = %q, want %q", capturedConfig.Workspace, resolvedWant)
	}
}

// TestStart_RejectsProjectConfigsAgentHome covers the refusal side of the
// same project-configs shape: an agent's home directory
// (~/.scion/project-configs/<dir>/.scion/agents/<agent>/home,
// config.GetAgentHomePath, credential-bearing) must stay refused even
// though its sibling .../workspace is now admitted, and even when the
// persisted source somehow ends up pointing at it directly.
func TestStart_RejectsProjectConfigsAgentHome(t *testing.T) {
	t.Setenv("SCION_HOST_UID", "")

	tmpDir := t.TempDir()
	originalHome := os.Getenv("HOME")
	defer func() { _ = os.Setenv("HOME", originalHome) }()
	_ = os.Setenv("HOME", tmpDir)

	projectScionDir := setupHubMarkerProjectConfigsDir(t, tmpDir, "hub-slug3__31111111")

	agentHome := config.GetAgentHomePath(projectScionDir, "bad-agent")
	if err := os.MkdirAll(agentHome, 0755); err != nil {
		t.Fatal(err)
	}
	agentDir := filepath.Join(projectScionDir, "agents", "bad-agent")
	agentJSON := fmt.Sprintf(`{"harness": "generic", "volumes": [{"source": %q, "target": "/workspace"}]}`, agentHome)
	if err := os.WriteFile(filepath.Join(agentDir, "scion-agent.json"), []byte(agentJSON), 0644); err != nil {
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
	mgr := NewManager(mockRT)

	_, err := mgr.Start(context.Background(), api.StartOptions{
		Name:        "bad-agent",
		ProjectPath: projectScionDir,
		NoAuth:      true,
	})
	if err == nil {
		t.Fatal("expected Start to refuse a persisted workspace pointing at an agent's own home directory under project-configs")
	}
	if !strings.Contains(err.Error(), "is not an allowed workspace path") {
		t.Errorf("expected the rejection to come from the workspace-source floor, got: %v", err)
	}
	if runCalled {
		t.Error("expected the runtime's Run to never be invoked (fail closed), but it was called")
	}
}

// TestStart_RejectsPreExistingProjectConfigsAgentBareWorkspace covers the
// project-configs counterpart to
// TestStart_RejectsPreExistingGlobalAgentWorkspaceOutsideProjectDir: an
// agent whose persisted workspace is its project's own bare externalized
// directory (~/.scion/project-configs/<dir>, not the per-agent
// .../agents/<agent-id>/workspace shape underneath it) is refused on
// resume, since isAllowedProjectConfigsSubtree never admits the bare
// directory itself. The error must name both ways to recover, the same as
// the global case, since isProjectConfigsPath(projectDir) now joins
// IsGlobalProjectDir(projectDir) in the recovery-hint wrap.
func TestStart_RejectsPreExistingProjectConfigsAgentBareWorkspace(t *testing.T) {
	t.Setenv("SCION_HOST_UID", "")

	tmpDir := t.TempDir()
	originalHome := os.Getenv("HOME")
	defer func() { _ = os.Setenv("HOME", originalHome) }()
	_ = os.Setenv("HOME", tmpDir)

	projectScionDir := setupHubMarkerProjectConfigsDir(t, tmpDir, "hub-slug4__41111111")

	// The project's own bare externalized directory
	// (~/.scion/project-configs/<dir>), not a per-agent subdirectory under
	// it -- the shape isAllowedProjectConfigsSubtree never admits.
	bareProjectConfigsDir := filepath.Dir(projectScionDir)

	agentName := "pre-existing-pc-agent"
	agentDir := filepath.Join(projectScionDir, "agents", agentName)
	if err := os.MkdirAll(agentDir, 0755); err != nil {
		t.Fatal(err)
	}
	agentJSON := fmt.Sprintf(`{"harness": "generic", "volumes": [{"source": %q, "target": "/workspace"}]}`, bareProjectConfigsDir)
	if err := os.WriteFile(filepath.Join(agentDir, "scion-agent.json"), []byte(agentJSON), 0644); err != nil {
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
	mgr := NewManager(mockRT)

	_, err := mgr.Start(context.Background(), api.StartOptions{
		Name:        agentName,
		ProjectPath: projectScionDir,
		NoAuth:      true,
	})
	if err == nil {
		t.Fatal("expected Start to refuse a pre-existing project-configs agent's bare-directory workspace")
	}
	if !strings.Contains(err.Error(), "is not an allowed workspace path") {
		t.Errorf("expected the base rejection to come from the ~/.scion allow-list floor, got: %v", err)
	}
	if !strings.Contains(err.Error(), "recreate") || !strings.Contains(err.Error(), "--workspace") {
		t.Errorf("expected an actionable error naming both recovery paths (recreate the agent, or an explicit --workspace), got: %v", err)
	}
	if runCalled {
		t.Error("expected the runtime's Run to never be invoked (fail closed), but it was called")
	}
}

// TestStart_AcceptsSettingsWorkspacePath is the missing Start()-level
// positive regression test for the externalized-project branch: a non-git
// project whose settings.yaml sets workspace_path must have that path
// accepted, mounted, and returned as-is (it already equals its own root).
func TestStart_AcceptsSettingsWorkspacePath(t *testing.T) {
	tmpDir := t.TempDir()

	oldWd, _ := os.Getwd()
	_ = os.Chdir(tmpDir)
	defer func() { _ = os.Chdir(oldWd) }()

	originalHome := os.Getenv("HOME")
	defer func() { _ = os.Setenv("HOME", originalHome) }()
	_ = os.Setenv("HOME", tmpDir)

	// A broker/container environment can export the ambient port-publishing
	// setting as a plain boolean string. Koanf's env provider maps it onto a
	// bare key that collides with VersionedSettings' struct-typed field of
	// the same name and fails the whole decode -- not just that one field --
	// taking settings.WorkspacePath down with it. t.Setenv(key, "") is not
	// enough here: the key merely being present still collides, so it must
	// be fully unset for the duration of the test.
	if oldAutoExpose, ok := os.LookupEnv("SCION_AUTO_EXPOSE_PORTS"); ok {
		_ = os.Unsetenv("SCION_AUTO_EXPOSE_PORTS")
		defer func() { _ = os.Setenv("SCION_AUTO_EXPOSE_PORTS", oldAutoExpose) }()
	}

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

	externalWorkspace := filepath.Join(tmpDir, "external-workspace")
	_ = os.MkdirAll(externalWorkspace, 0755)

	projectScionDir := filepath.Join(tmpDir, "project", ".scion")
	_ = os.MkdirAll(projectScionDir, 0755)
	_ = os.WriteFile(filepath.Join(projectScionDir, "settings.yaml"), []byte(fmt.Sprintf(`schema_version: "1"
workspace_path: %q
`, externalWorkspace)), 0644)

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

	_, err := mgr.Start(context.Background(), api.StartOptions{
		Name:        "externalized-agent",
		ProjectPath: projectScionDir,
		NoAuth:      true,
	})
	if err != nil {
		t.Fatalf("expected Start to accept settings.WorkspacePath, got error: %v", err)
	}

	wantWorkspace, evalErr := filepath.EvalSymlinks(externalWorkspace)
	if evalErr != nil {
		t.Fatalf("EvalSymlinks(%q): %v", externalWorkspace, evalErr)
	}
	if capturedConfig.Workspace != wantWorkspace {
		t.Errorf("RunConfig.Workspace = %q, want %q", capturedConfig.Workspace, wantWorkspace)
	}
}

// TestStart_AcceptsSettingsWorkspacePathInGitProjectUnderContainerUID is the
// regression test for a git/non-git detection mismatch between provisioning
// and validation: ProvisionAgent treats a project as non-git whenever
// SCION_HOST_UID is set (isGitWorkspaceProject's container override, to
// avoid creating a worktree whose --relative-paths would be computed against
// the container's mount layout rather than the host's), so a git project
// with settings.WorkspacePath set is provisioned through the externalized,
// non-git branch -- its workspace can legitimately sit outside the repo.
// workspaceSourceRoots must apply the exact same override when validating
// that workspace at Start(), including on every resume, or it re-classifies
// the project as git and checks the externally configured workspace against
// repo-root containment instead -- rejecting a configuration that
// provisioning itself just accepted, purely because SCION_HOST_UID happened
// to be set both times.
func TestStart_AcceptsSettingsWorkspacePathInGitProjectUnderContainerUID(t *testing.T) {
	t.Setenv("SCION_HOST_UID", "1001")

	tmpDir := t.TempDir()

	oldWd, _ := os.Getwd()
	_ = os.Chdir(tmpDir)
	defer func() { _ = os.Chdir(oldWd) }()

	originalHome := os.Getenv("HOME")
	defer func() { _ = os.Setenv("HOME", originalHome) }()
	_ = os.Setenv("HOME", tmpDir)

	if oldAutoExpose, ok := os.LookupEnv("SCION_AUTO_EXPOSE_PORTS"); ok {
		_ = os.Unsetenv("SCION_AUTO_EXPOSE_PORTS")
		defer func() { _ = os.Setenv("SCION_AUTO_EXPOSE_PORTS", oldAutoExpose) }()
	}

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

	// The workspace lives OUTSIDE the git repo entirely -- only valid because
	// this project is provisioned through the non-git, externalized branch.
	externalWorkspace := filepath.Join(tmpDir, "external-workspace")
	_ = os.MkdirAll(externalWorkspace, 0755)

	projectDir := filepath.Join(tmpDir, "project")
	setupTestGitRepoWithBranch(t, projectDir, "unused-branch")
	projectScionDir := filepath.Join(projectDir, ".scion")
	_ = os.MkdirAll(projectScionDir, 0755)
	_ = os.WriteFile(filepath.Join(projectDir, ".gitignore"), []byte("agents/\n"), 0644)
	_ = os.WriteFile(filepath.Join(projectScionDir, "settings.yaml"), []byte(fmt.Sprintf(`schema_version: "1"
workspace_path: %q
`, externalWorkspace)), 0644)

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

	_, err := mgr.Start(context.Background(), api.StartOptions{
		Name:        "container-uid-agent",
		ProjectPath: projectScionDir,
		NoAuth:      true,
	})
	if err != nil {
		t.Fatalf("expected Start to accept settings.WorkspacePath for a git project under SCION_HOST_UID, got error: %v", err)
	}

	wantWorkspace, evalErr := filepath.EvalSymlinks(externalWorkspace)
	if evalErr != nil {
		t.Fatalf("EvalSymlinks(%q): %v", externalWorkspace, evalErr)
	}
	if capturedConfig.Workspace != wantWorkspace {
		t.Errorf("RunConfig.Workspace = %q, want %q", capturedConfig.Workspace, wantWorkspace)
	}
}

// TestStart_AcceptsExplicitHubManagedProjectsWorkspace is a Start()-level
// positive test that was still missing: an explicit
// opts.Workspace under ~/.scion/projects/<slug> (the shape
// pkg/runtimebroker/handlers.go sets for hub-dispatched, non-git projects on
// a local-runtime broker) must be accepted, and RunConfig.Workspace must be
// the resolved path. An explicit workspace skips containment entirely (see
// workspaceSourceRoots), so this exercises the rootless floor's
// ~/.scion/projects/<slug> allowance specifically.
func TestStart_AcceptsExplicitHubManagedProjectsWorkspace(t *testing.T) {
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

	hubWorkspace := filepath.Join(tmpDir, ".scion", "projects", "hub-slug", "workspace")
	_ = os.MkdirAll(hubWorkspace, 0755)

	projectDir := filepath.Join(tmpDir, "hub-project")
	projectScionDir := filepath.Join(projectDir, ".scion")
	_ = os.MkdirAll(projectScionDir, 0755)

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

	_, err := mgr.Start(context.Background(), api.StartOptions{
		Name:        "hub-managed-agent",
		ProjectPath: projectScionDir,
		Workspace:   hubWorkspace,
		NoAuth:      true,
	})
	if err != nil {
		t.Fatalf("expected Start to accept an explicit workspace under ~/.scion/projects/<slug>, got error: %v", err)
	}

	wantWorkspace, evalErr := filepath.EvalSymlinks(hubWorkspace)
	if evalErr != nil {
		t.Fatalf("EvalSymlinks(%q): %v", hubWorkspace, evalErr)
	}
	if capturedConfig.Workspace != wantWorkspace {
		t.Errorf("RunConfig.Workspace = %q, want %q", capturedConfig.Workspace, wantWorkspace)
	}
}

// TestStart_AcceptsNonGitInRepoFallbackWorkspace is the missing Start()-level
// positive regression test for the plain non-git project fallback: no
// settings.WorkspacePath, not the global project -- the workspace is the
// project's own directory (parent of its .scion directory).
func TestStart_AcceptsNonGitInRepoFallbackWorkspace(t *testing.T) {
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

	projectDir := filepath.Join(tmpDir, "plain-project")
	projectScionDir := filepath.Join(projectDir, ".scion")
	_ = os.MkdirAll(projectScionDir, 0755)

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

	_, err := mgr.Start(context.Background(), api.StartOptions{
		Name:        "plain-agent",
		ProjectPath: projectScionDir,
		NoAuth:      true,
	})
	if err != nil {
		t.Fatalf("expected Start to accept the non-git in-repo fallback workspace, got error: %v", err)
	}

	wantWorkspace, evalErr := filepath.EvalSymlinks(projectDir)
	if evalErr != nil {
		t.Fatalf("EvalSymlinks(%q): %v", projectDir, evalErr)
	}
	if capturedConfig.Workspace != wantWorkspace {
		t.Errorf("RunConfig.Workspace = %q, want %q", capturedConfig.Workspace, wantWorkspace)
	}
}

// TestStart_RunConfigWorkspaceIsResolvedPathForSymlinkedProject is the
// missing Start()-level test asserting that RunConfig.Workspace, as handed
// to the runtime, is the resolved path -- not just at the guard and
// buildCommonRunArgs level, but end to end through Start() -- when the
// project directory itself is reached through a symlink.
func TestStart_RunConfigWorkspaceIsResolvedPathForSymlinkedProject(t *testing.T) {
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

	realProjectDir := filepath.Join(tmpDir, "real-project")
	_ = os.MkdirAll(realProjectDir, 0755)
	projectDirLink := filepath.Join(tmpDir, "project-link")
	if err := os.Symlink(realProjectDir, projectDirLink); err != nil {
		t.Fatal(err)
	}
	projectScionDir := filepath.Join(projectDirLink, ".scion")
	_ = os.MkdirAll(projectScionDir, 0755)

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

	_, err := mgr.Start(context.Background(), api.StartOptions{
		Name:        "symlinked-project-agent",
		ProjectPath: projectScionDir,
		NoAuth:      true,
	})
	if err != nil {
		t.Fatalf("expected Start to succeed for a symlinked project directory, got error: %v", err)
	}

	wantRealProjectDir, evalErr := filepath.EvalSymlinks(realProjectDir)
	if evalErr != nil {
		t.Fatalf("EvalSymlinks(%q): %v", realProjectDir, evalErr)
	}
	if capturedConfig.Workspace != wantRealProjectDir {
		t.Errorf("RunConfig.Workspace = %q, want the resolved real path %q (not a path through the symlink)", capturedConfig.Workspace, wantRealProjectDir)
	}
}

// TestStart_RunConfigRepoRootIsResolvedPathForSymlinkedRepo is the
// RepoRoot-side sibling of TestStart_RunConfigWorkspaceIsResolvedPathForSymlinkedProject:
// a git project reached through a symlink must have RunConfig.RepoRoot
// resolved the same way RunConfig.Workspace already is, so the two spellings
// agree by the time buildCommonRunArgs compares them with filepath.Rel.
func TestStart_RunConfigRepoRootIsResolvedPathForSymlinkedRepo(t *testing.T) {
	t.Setenv("SCION_HOST_UID", "")

	tmpDir := t.TempDir()

	oldWd, _ := os.Getwd()
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

	realProjectDir := filepath.Join(tmpDir, "real-project")
	agentName := "symlinked-repo-agent"
	setupTestGitRepoWithBranch(t, realProjectDir, api.Slugify(agentName))
	projectDirLink := filepath.Join(tmpDir, "project-link")
	if err := os.Symlink(realProjectDir, projectDirLink); err != nil {
		t.Fatal(err)
	}
	projectScionDir := filepath.Join(projectDirLink, ".scion")
	_ = os.MkdirAll(projectScionDir, 0755)
	_ = os.WriteFile(filepath.Join(projectDirLink, ".gitignore"), []byte("agents/\n"), 0644)

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

	if err := os.Chdir(projectDirLink); err != nil {
		t.Fatal(err)
	}

	_, err := mgr.Start(context.Background(), api.StartOptions{
		Name:        agentName,
		ProjectPath: projectScionDir,
		NoAuth:      true,
	})
	if err != nil {
		t.Fatalf("expected Start to succeed for a symlinked git project, got error: %v", err)
	}

	realResolvedRepoRoot, evalErr := filepath.EvalSymlinks(realProjectDir)
	if evalErr != nil {
		t.Fatalf("EvalSymlinks(%q): %v", realProjectDir, evalErr)
	}
	if capturedConfig.RepoRoot != realResolvedRepoRoot {
		t.Errorf("RunConfig.RepoRoot = %q, want the resolved real path %q (not a path through the symlink)", capturedConfig.RepoRoot, realResolvedRepoRoot)
	}
}
