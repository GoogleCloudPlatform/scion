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
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

func TestStartPropagatesNFSWorkspaceBackendToRunConfig(t *testing.T) {
	// subpath_root reaches the runtime two ways that must agree: inside
	// Workspace/NFSSubPath (via the nfs backend) and as NFSSubPathRoot, from
	// which the Cloud Run runtime rebuilds its export and host paths. If
	// NFSSubPathRoot fell back to the default while the backend used the
	// configured root, every Cloud Run agent start would fail.
	t.Run("default subpath_root", func(t *testing.T) {
		testStartPropagatesNFSWorkspaceBackendToRunConfig(t, "")
	})
	t.Run("configured subpath_root", func(t *testing.T) {
		testStartPropagatesNFSWorkspaceBackendToRunConfig(t, "team/trees")
	})
}

func testStartPropagatesNFSWorkspaceBackendToRunConfig(t *testing.T, subPathRoot string) {
	tmpDir := t.TempDir()

	oldWd, err := os.Getwd()
	if err != nil {
		t.Fatalf("failed to get working directory: %v", err)
	}
	if err := os.Chdir(tmpDir); err != nil {
		t.Fatalf("failed to chdir to tmpDir: %v", err)
	}
	defer func() { _ = os.Chdir(oldWd) }()

	originalHome := os.Getenv("HOME")
	defer func() { _ = os.Setenv("HOME", originalHome) }()
	if err := os.Setenv("HOME", tmpDir); err != nil {
		t.Fatalf("failed to set HOME: %v", err)
	}

	projectDir := filepath.Join(tmpDir, "project")
	projectScionDir := filepath.Join(projectDir, ".scion")
	if err := os.MkdirAll(projectScionDir, 0755); err != nil {
		t.Fatalf("failed to create project .scion dir: %v", err)
	}

	nfsMountRoot := filepath.Join(tmpDir, "nfs")
	settingsYAML := fmt.Sprintf(`schema_version: "1"
active_profile: local
server:
  workspace_storage:
    backend: nfs
    nfs:
      mount_root: %s
      uid: 2000
      gid: 2001
      storage_class: filestore-sc
      subpath_root: %q
      shares:
        - id: share-1
          server: 10.0.0.2
          export: /scion-workspaces
          pv_name: scion-workspaces-pv
harness_configs:
  test-harness:
    harness: gemini
    user: scion
    image: test-image:latest
profiles:
  local:
    runtime: docker
`, nfsMountRoot, subPathRoot)
	if err := os.WriteFile(filepath.Join(projectScionDir, "settings.yaml"), []byte(settingsYAML), 0644); err != nil {
		t.Fatalf("failed to write settings: %v", err)
	}

	hcDir := filepath.Join(projectScionDir, "harness-configs", "test-harness")
	if err := os.MkdirAll(hcDir, 0755); err != nil {
		t.Fatalf("failed to create harness-config dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(hcDir, "config.yaml"), []byte("harness: gemini\nuser: scion\nimage: test-image:latest\n"), 0644); err != nil {
		t.Fatalf("failed to write harness config: %v", err)
	}

	tplDir := filepath.Join(projectScionDir, "templates", "default")
	if err := os.MkdirAll(tplDir, 0755); err != nil {
		t.Fatalf("failed to create template dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(tplDir, "scion-agent.json"), []byte(`{"default_harness_config": "test-harness"}`), 0644); err != nil {
		t.Fatalf("failed to write template: %v", err)
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
	_, err = mgr.Start(context.Background(), api.StartOptions{
		Name:        "test-agent",
		ProjectPath: projectScionDir,
		NoAuth:      true,
		Env: map[string]string{
			"SCION_AGENT_ID":   "agent-456",
			"SCION_PROJECT_ID": "proj-123",
		},
		GitClone: &api.GitCloneConfig{URL: "https://example.com/repo.git"},
	})
	if err != nil {
		t.Fatalf("Start failed: %v", err)
	}

	if capturedConfig.WorkspaceBackendName != "nfs" {
		t.Fatalf("WorkspaceBackendName = %q, want nfs", capturedConfig.WorkspaceBackendName)
	}
	wantRoot := config.SubPathRootOrDefault(subPathRoot)
	wantWorkspace := filepath.Join(nfsMountRoot, "share-1", filepath.FromSlash(wantRoot), "proj-123", "workspace")
	if capturedConfig.Workspace != wantWorkspace {
		t.Fatalf("Workspace = %q, want %q", capturedConfig.Workspace, wantWorkspace)
	}
	if got := config.SubPathRootOrDefault(capturedConfig.NFSSubPathRoot); got != wantRoot {
		t.Fatalf("NFSSubPathRoot = %q (effective %q), want effective %q", capturedConfig.NFSSubPathRoot, got, wantRoot)
	}
	if capturedConfig.NFSUID != 2000 || capturedConfig.NFSGID != 2001 {
		t.Fatalf("NFS uid/gid = %d/%d, want 2000/2001", capturedConfig.NFSUID, capturedConfig.NFSGID)
	}
	if capturedConfig.NFSPVClaimName != "scion-workspaces-pv" {
		t.Fatalf("NFSPVClaimName = %q", capturedConfig.NFSPVClaimName)
	}
	if capturedConfig.NFSSubPath != filepath.Join(filepath.FromSlash(wantRoot), "proj-123", "workspace") {
		t.Fatalf("NFSSubPath = %q", capturedConfig.NFSSubPath)
	}
	if capturedConfig.NFSStorageClass != "filestore-sc" {
		t.Fatalf("NFSStorageClass = %q", capturedConfig.NFSStorageClass)
	}
	if capturedConfig.Labels["agent_id"] != "agent-456" {
		t.Fatalf("agent_id label = %q", capturedConfig.Labels["agent_id"])
	}
	// F-111 (design §9): GitCloneForInit must carry the same GitClone config
	// passed to Start — this is what the k8s runtime's NFS init container
	// uses to decide clone-vs-plain-provision (nfsProvisionCommand). Before
	// this fix nothing set this field at all, for git or non-git projects,
	// so the init container never ran for anyone.
	if capturedConfig.GitCloneForInit == nil {
		t.Fatal("GitCloneForInit is nil, want the GitClone config passed to Start")
	}
	if capturedConfig.GitCloneForInit.URL != "https://example.com/repo.git" {
		t.Fatalf("GitCloneForInit.URL = %q, want %q", capturedConfig.GitCloneForInit.URL, "https://example.com/repo.git")
	}
}

// TestStartPropagatesNFSWorkspaceBackend_NonGit_StillRequestsProvisioning is
// the F-111 (design §9) regression test: a non-git, shared-plain NFS project
// must still get NFSPVClaimName/NFSSubPath populated (so the k8s runtime
// still injects its provisioning init container) even though there is no
// GitClone config to carry. Before this fix, the k8s runtime's init
// container was gated on GitCloneForInit != nil, so a nil value here — which
// is correct, there's nothing to clone — silently meant "no provisioning at
// all" instead of "provision, but don't clone." The gate is now independent
// of this field (k8s_runtime.go's nfsInitContainerInjected); this test pins
// the pkg/agent half of that fix: RunConfig must still carry everything the
// gate itself needs, whether or not the project is git-backed.
func TestStartPropagatesNFSWorkspaceBackend_NonGit_StillRequestsProvisioning(t *testing.T) {
	tmpDir := t.TempDir()

	oldWd, err := os.Getwd()
	if err != nil {
		t.Fatalf("failed to get working directory: %v", err)
	}
	if err := os.Chdir(tmpDir); err != nil {
		t.Fatalf("failed to chdir to tmpDir: %v", err)
	}
	defer func() { _ = os.Chdir(oldWd) }()

	originalHome := os.Getenv("HOME")
	defer func() { _ = os.Setenv("HOME", originalHome) }()
	if err := os.Setenv("HOME", tmpDir); err != nil {
		t.Fatalf("failed to set HOME: %v", err)
	}

	projectDir := filepath.Join(tmpDir, "project")
	projectScionDir := filepath.Join(projectDir, ".scion")
	if err := os.MkdirAll(projectScionDir, 0755); err != nil {
		t.Fatalf("failed to create project .scion dir: %v", err)
	}

	nfsMountRoot := filepath.Join(tmpDir, "nfs")
	settingsYAML := fmt.Sprintf(`schema_version: "1"
active_profile: local
server:
  workspace_storage:
    backend: nfs
    nfs:
      mount_root: %s
      uid: 2000
      gid: 2001
      storage_class: filestore-sc
      shares:
        - id: share-1
          server: 10.0.0.2
          export: /scion-workspaces
          pv_name: scion-workspaces-pv
harness_configs:
  test-harness:
    harness: gemini
    user: scion
    image: test-image:latest
profiles:
  local:
    runtime: docker
`, nfsMountRoot)
	if err := os.WriteFile(filepath.Join(projectScionDir, "settings.yaml"), []byte(settingsYAML), 0644); err != nil {
		t.Fatalf("failed to write settings: %v", err)
	}

	hcDir := filepath.Join(projectScionDir, "harness-configs", "test-harness")
	if err := os.MkdirAll(hcDir, 0755); err != nil {
		t.Fatalf("failed to create harness-config dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(hcDir, "config.yaml"), []byte("harness: gemini\nuser: scion\nimage: test-image:latest\n"), 0644); err != nil {
		t.Fatalf("failed to write harness config: %v", err)
	}

	tplDir := filepath.Join(projectScionDir, "templates", "default")
	if err := os.MkdirAll(tplDir, 0755); err != nil {
		t.Fatalf("failed to create template dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(tplDir, "scion-agent.json"), []byte(`{"default_harness_config": "test-harness"}`), 0644); err != nil {
		t.Fatalf("failed to write template: %v", err)
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
	_, err = mgr.Start(context.Background(), api.StartOptions{
		Name:            "test-agent",
		ProjectPath:     projectScionDir,
		NoAuth:          true,
		SharedWorkspace: true, // non-git shared-plain: no GitClone at all
		Env: map[string]string{
			"SCION_AGENT_ID":   "agent-789",
			"SCION_PROJECT_ID": "proj-456",
		},
		// GitClone intentionally omitted — this is the non-git case.
	})
	if err != nil {
		t.Fatalf("Start failed: %v", err)
	}

	if capturedConfig.WorkspaceBackendName != "nfs" {
		t.Fatalf("WorkspaceBackendName = %q, want nfs", capturedConfig.WorkspaceBackendName)
	}
	if capturedConfig.NFSPVClaimName != "scion-workspaces-pv" {
		t.Fatalf("NFSPVClaimName = %q, want scion-workspaces-pv (provisioning must still be requested)", capturedConfig.NFSPVClaimName)
	}
	if capturedConfig.NFSSubPath == "" {
		t.Fatal("NFSSubPath is empty, want a resolved subPath (provisioning must still be requested)")
	}
	if capturedConfig.GitCloneForInit != nil {
		t.Fatalf("GitCloneForInit = %+v, want nil (nothing to clone)", capturedConfig.GitCloneForInit)
	}
}

// TestStartInvalidatesProvisionedRepoRootWhenNFSBackendReplacesWorkspace
// covers server.workspace_storage.backend == "nfs", which applies to
// worktree-per-agent mode too (runtime.SelectWorkspaceBackend), and when it
// fires it replaces effectiveWorkspace with the NFS-backed host path — which
// can combine with a ctx-provisioned repo root from tryProvisionWorktree's
// local host-side worktree in the same dispatch. The repo root validated
// against the pre-backend workspace no longer corresponds to the final
// workspace RunConfig actually uses, so it must be re-validated (and here,
// correctly invalidated) rather than left stale.
func TestStartInvalidatesProvisionedRepoRootWhenNFSBackendReplacesWorkspace(t *testing.T) {
	t.Setenv("SCION_HOST_UID", "")
	tmpDir := t.TempDir()
	projectScionDir := startRepoRootProjectScaffold(t, tmpDir)

	nfsMountRoot := filepath.Join(tmpDir, "nfs")
	settingsYAML := fmt.Sprintf(`schema_version: "1"
active_profile: local
server:
  workspace_storage:
    backend: nfs
    nfs:
      mount_root: %s
      shares:
        - id: share-1
          server: 10.0.0.2
          export: /scion-workspaces
          pv_name: scion-workspaces-pv
harness_configs:
  test-harness:
    harness: gemini
    user: scion
    image: test-image:latest
profiles:
  local:
    runtime: docker
`, nfsMountRoot)
	if err := os.WriteFile(filepath.Join(projectScionDir, "settings.yaml"), []byte(settingsYAML), 0644); err != nil {
		t.Fatalf("failed to write settings: %v", err)
	}

	// A REAL local worktree — this is what tryProvisionWorktree would have
	// provisioned on the host before the NFS backend applies. The ctx signal
	// below validates successfully against THIS workspace, on purpose: the
	// point of the test is that the NFS backend then replaces it.
	sharedBase := filepath.Join(tmpDir, "shared-base")
	if err := os.MkdirAll(sharedBase, 0755); err != nil {
		t.Fatalf("failed to create shared base dir: %v", err)
	}
	setupGitRepo(t, sharedBase)
	localWorktree := createRealWorktree(t, sharedBase, "agent-a")

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
		Workspace:   localWorktree,
		Env: map[string]string{
			"SCION_AGENT_ID":   "agent-a",
			"SCION_PROJECT_ID": "proj-123",
		},
	}); err != nil {
		t.Fatalf("Start failed: %v", err)
	}

	// Confirm the NFS backend actually fired and replaced the workspace —
	// otherwise this test would pass vacuously.
	if capturedConfig.WorkspaceBackendName != "nfs" {
		t.Fatalf("WorkspaceBackendName = %q, want nfs (test setup broken)", capturedConfig.WorkspaceBackendName)
	}
	wantWorkspace := filepath.Join(nfsMountRoot, "share-1", "projects", "proj-123", "workspace")
	if capturedConfig.Workspace != wantWorkspace {
		t.Fatalf("Workspace = %q, want %q (test setup broken)", capturedConfig.Workspace, wantWorkspace)
	}

	// The actual assertion: RepoRoot must NOT be the local sharedBase — that
	// value was validated against localWorktree, not the NFS path RunConfig
	// now actually uses. An explicit --workspace-shaped dispatch (opts.Workspace
	// set) with no valid provisioned root falls through to detectRepoRoot,
	// which stays "" for an explicit workspace.
	if capturedConfig.RepoRoot != "" {
		t.Fatalf("RunConfig.RepoRoot = %q, want \"\" — stale repo root from before the NFS backend replaced the workspace", capturedConfig.RepoRoot)
	}
}

// TestStartBackendReValidationAcceptsAliasedRootAgainstNFSWorktreeShapedPath
// drives the real workspace-storage-backend re-validation end to end,
// instead of calling the shared comparison directly: the NFS backend is
// configured with a subpath_root and project ID chosen so its computed host
// path happens to be a genuine worktree under the same base the
// ctx-provisioned, aliased root names, exercising this call site's own
// wiring to the shared comparison rather than just the comparison itself.
func TestStartBackendReValidationAcceptsAliasedRootAgainstNFSWorktreeShapedPath(t *testing.T) {
	t.Setenv("SCION_HOST_UID", "")
	tmpDir := t.TempDir()
	projectScionDir := startRepoRootProjectScaffold(t, tmpDir)
	nfsMountRoot := filepath.Join(tmpDir, "nfs")
	settingsYAML := fmt.Sprintf(`schema_version: "1"
active_profile: local
server:
  workspace_storage:
    backend: nfs
    nfs:
      mount_root: %s
      subpath_root: repo
      shares:
        - id: share-1
          server: 10.0.0.2
          export: /scion-workspaces
harness_configs:
  test-harness:
    harness: gemini
    user: scion
    image: test-image:latest
profiles:
  local:
    runtime: docker
`, nfsMountRoot)
	if err := os.WriteFile(filepath.Join(projectScionDir, "settings.yaml"), []byte(settingsYAML), 0644); err != nil {
		t.Fatal(err)
	}

	// The NFS backend computes <mount_root>/<share>/<subpath_root>/<project
	// ID>/workspace. With subpath_root "repo" and a project ID of
	// "worktrees", that computed path is <base>/worktrees/workspace — the
	// same shape a genuine, scion-created worktree named "workspace" would
	// have under base.
	base := filepath.Join(nfsMountRoot, "share-1", "repo")
	if err := os.MkdirAll(base, 0755); err != nil {
		t.Fatal(err)
	}
	setupGitRepo(t, base)
	localWorktree := createRealWorktree(t, base, "agent-a")
	nfsWorktree := createRealWorktree(t, base, "workspace")
	aliasedRoot := filepath.Join(tmpDir, "alias")
	if err := os.Symlink(base, aliasedRoot); err != nil {
		t.Fatal(err)
	}

	var capturedConfig runtime.RunConfig
	mgr := NewManager(&runtime.MockRuntime{
		ListFunc: func(ctx context.Context, labelFilter map[string]string) ([]api.AgentInfo, error) {
			return nil, nil
		},
		RunFunc: func(ctx context.Context, c runtime.RunConfig) (string, error) {
			capturedConfig = c
			return "mock-id", nil
		},
	})

	ctx := api.ContextWithProvisionedWorktreeRepoRoot(context.Background(), aliasedRoot)
	if _, err := mgr.Start(ctx, api.StartOptions{
		Name:        "agent-a",
		ProjectPath: projectScionDir,
		NoAuth:      true,
		Workspace:   localWorktree,
		Env:         map[string]string{"SCION_AGENT_ID": "agent-a", "SCION_PROJECT_ID": "worktrees"},
	}); err != nil {
		t.Fatalf("Start failed: %v", err)
	}

	// Confirm the backend actually fired and replaced the workspace with the
	// worktree-shaped NFS path — otherwise this test would pass vacuously.
	if capturedConfig.WorkspaceBackendName != "nfs" || capturedConfig.Workspace != nfsWorktree {
		t.Fatalf("test setup broken: WorkspaceBackendName=%q Workspace=%q, want nfs / %q", capturedConfig.WorkspaceBackendName, capturedConfig.Workspace, nfsWorktree)
	}
	if capturedConfig.RepoRoot != aliasedRoot {
		t.Fatalf("RunConfig.RepoRoot = %q, want %q", capturedConfig.RepoRoot, aliasedRoot)
	}
}

// TestStartKubernetesNFSWorktreeDispatch drives a Kubernetes dispatch,
// worktree-per-agent mode, and the nfs workspace-storage backend together
// against a real git base and a real, upstream-layout worktree
// (<base>/worktrees/<agent>) at the path the backend actually resolves to,
// through the real Start path (not the shared comparison function directly).
// It pins the three outcomes this shape produces: effectiveWorkspace stays
// the shared base (never worktree-shaped — see validatedWorktreeRepoRoot's
// own doc comment), RunConfig.RepoRoot still ends up non-empty via the
// detectRepoRoot fallback, and nothing is persisted to the broker-owned
// repo-root file, whether or not a provisioned-worktree ctx signal is
// present on the dispatch.
func TestStartKubernetesNFSWorktreeDispatch(t *testing.T) {
	t.Setenv("SCION_HOST_UID", "")

	// run drives one dispatch and returns the outcomes to check. When
	// withCtxSignal is true, the provisioned-worktree ctx signal is set to a
	// symlink alias of the base path once it is known: a value distinct from
	// base itself, so that RepoRoot would visibly differ (become the alias)
	// if the re-validation wrongly accepted it, rather than coincidentally
	// matching the correct detectRepoRoot fallback value either way. Still
	// not a genuine worktree-to-root pair (the workspace IS the root, not a
	// descendant of it, even resolved through the alias), so this must still
	// fail the re-validation and the persistence gate's own validation.
	run := func(t *testing.T, withCtxSignal bool) (capturedConfig runtime.RunConfig, agentDir, base string) {
		tmpDir := t.TempDir()
		projectScionDir := startRepoRootProjectScaffold(t, tmpDir)

		nfsMountRoot := filepath.Join(tmpDir, "nfs")
		settingsYAML := fmt.Sprintf(`schema_version: "1"
active_profile: local
server:
  workspace_storage:
    backend: nfs
    nfs:
      mount_root: %s
      shares:
        - id: share-1
          server: 10.0.0.2
          export: /scion-workspaces
          pv_name: scion-workspaces-pv
harness_configs:
  test-harness:
    harness: gemini
    user: scion
    image: test-image:latest
profiles:
  local:
    runtime: kubernetes
`, nfsMountRoot)
		if err := os.WriteFile(filepath.Join(projectScionDir, "settings.yaml"), []byte(settingsYAML), 0644); err != nil {
			t.Fatalf("failed to write settings: %v", err)
		}

		// The base the nfs backend resolves to
		// (<mount_root>/<share>/projects/<projectID>/workspace, the default
		// subpath_root) — a real git repo with a real worktree for this
		// agent already in place, the shape the Kubernetes init container
		// produces for a worktree-per-agent project before the agent
		// container starts.
		base = filepath.Join(nfsMountRoot, "share-1", "projects", "proj-k8s", "workspace")
		if err := os.MkdirAll(base, 0755); err != nil {
			t.Fatalf("failed to create base dir: %v", err)
		}
		setupGitRepo(t, base)
		createRealWorktree(t, base, "agent-a")

		mockRT := &runtime.MockRuntime{
			NameFunc: func() string { return "kubernetes" },
			ListFunc: func(ctx context.Context, labelFilter map[string]string) ([]api.AgentInfo, error) {
				return []api.AgentInfo{}, nil
			},
			RunFunc: func(ctx context.Context, config runtime.RunConfig) (string, error) {
				capturedConfig = config
				return "mock-id", nil
			},
		}
		mgr := NewManager(mockRT)

		ctx := context.Background()
		if withCtxSignal {
			aliasBase := filepath.Join(tmpDir, "alias-base")
			if err := os.Symlink(base, aliasBase); err != nil {
				t.Fatalf("failed to create alias symlink: %v", err)
			}
			ctx = api.ContextWithProvisionedWorktreeRepoRoot(ctx, aliasBase)
		}
		if _, err := mgr.Start(ctx, api.StartOptions{
			Name:        "agent-a",
			ProjectPath: projectScionDir,
			NoAuth:      true,
			GitClone:    &api.GitCloneConfig{URL: "https://example.invalid/repo.git"},
			Env: map[string]string{
				"SCION_AGENT_ID":       "agent-a",
				"SCION_PROJECT_ID":     "proj-k8s",
				"SCION_WORKSPACE_MODE": "worktree-per-agent",
			},
		}); err != nil {
			t.Fatalf("Start failed: %v", err)
		}
		agentDir = config.GetAgentDir(projectScionDir, "agent-a", false)
		return capturedConfig, agentDir, base
	}

	assertOutcomes := func(t *testing.T, capturedConfig runtime.RunConfig, agentDir, base string) {
		t.Helper()
		// Confirm the backend and the worktree selection actually fired —
		// otherwise this test would pass vacuously.
		if capturedConfig.WorkspaceBackendName != "nfs" {
			t.Fatalf("WorkspaceBackendName = %q, want nfs (test setup broken)", capturedConfig.WorkspaceBackendName)
		}
		if capturedConfig.Workspace != base {
			t.Fatalf("Workspace = %q, want %q — the nfs backend's host path is always the shared base, never worktree-shaped", capturedConfig.Workspace, base)
		}
		if want := "/repo-root/worktrees/agent-a"; capturedConfig.ContainerWorkspace != want {
			t.Fatalf("ContainerWorkspace = %q, want %q", capturedConfig.ContainerWorkspace, want)
		}
		// The detectRepoRoot fallback resolves effectiveWorkspace (the real
		// git base) to a non-empty RepoRoot. The Kubernetes runtime builds
		// its mounts from ContainerWorkspace and the NFS* fields, not
		// RepoRoot — see the comment on the backend re-validation in run.go.
		if capturedConfig.RepoRoot != base {
			t.Fatalf("RepoRoot = %q, want %q (the detectRepoRoot fallback)", capturedConfig.RepoRoot, base)
		}
		if got := readProvisionedWorktreeRepoRoot(agentDir); got != "" {
			t.Fatalf("persisted repo root = %q, want \"\" — nothing should be written to the broker-owned repo-root file for a Kubernetes dispatch", got)
		}
	}

	t.Run("no ctx signal", func(t *testing.T) {
		capturedConfig, agentDir, base := run(t, false)
		assertOutcomes(t, capturedConfig, agentDir, base)
	})

	t.Run("ctx signal present", func(t *testing.T) {
		capturedConfig, agentDir, base := run(t, true)
		assertOutcomes(t, capturedConfig, agentDir, base)
	})
}
