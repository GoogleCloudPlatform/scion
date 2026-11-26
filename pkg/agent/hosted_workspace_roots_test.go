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
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHostedExplicitWorkspaceRoots_LocalAndShared(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	globalDir := filepath.Join(tmpHome, ".scion")
	projWS := filepath.Join(globalDir, "projects", "demo-proj")
	require.NoError(t, os.MkdirAll(projWS, 0o755))

	mountRoot := t.TempDir()
	sharedWS := filepath.Join(mountRoot, "share1", "projects", "proj-uuid-1", "workspace")
	require.NoError(t, os.MkdirAll(sharedWS, 0o755))

	linkedRoot := filepath.Join(t.TempDir(), "linked-repo")
	linkedSubdir := filepath.Join(linkedRoot, "sub", "dir")
	linkedProjectDir := filepath.Join(linkedRoot, ".scion")
	require.NoError(t, os.MkdirAll(linkedSubdir, 0o755))
	require.NoError(t, os.MkdirAll(linkedProjectDir, 0o755))

	outsideDir := filepath.Join(t.TempDir(), "outside-dir")
	require.NoError(t, os.MkdirAll(outsideDir, 0o755))

	globalSettings := &config.VersionedSettings{
		Server: &config.V1ServerConfig{
			ProjectStorage: &config.V1ProjectStorageConfig{
				Backend: config.WorkspaceStorageBackendNFS,
				NFS: &config.V1NFSConfig{
					MountRoot:   mountRoot,
					SubPathRoot: "projects",
					Shares: []config.V1NFSShare{
						{ID: "share1", Server: "10.0.0.1", Export: "/export"},
					},
				},
			},
		},
	}

	roots, err := hostedExplicitWorkspaceRootsFromGlobal(globalSettings, linkedRoot, linkedProjectDir)
	require.NoError(t, err)
	require.NotEmpty(t, roots)

	// Hub-managed local project path passes.
	_, err = runtime.ValidateWorkspaceSource(projWS, roots...)
	assert.NoError(t, err)

	// Shared storage backend workspace path passes.
	_, err = runtime.ValidateWorkspaceSource(sharedWS, roots...)
	assert.NoError(t, err)

	// Subdirectory under the project's own root (resolved relative workspace) passes.
	_, err = runtime.ValidateWorkspaceSource(linkedSubdir, roots...)
	assert.NoError(t, err)

	// Absolute path outside permitted roots is refused.
	_, err = runtime.ValidateWorkspaceSource(outsideDir, roots...)
	assert.Error(t, err)

	// Non-workspace path inside ~/.scion is still refused.
	_, err = runtime.ValidateWorkspaceSource(filepath.Join(globalDir, "settings.yaml"), roots...)
	assert.Error(t, err)

	// Workstation mode keeps explicit workspaces unconstrained by hosted roots.
	wsRoots, err := workspaceSourceRootsForMode(context.Background(), true, false, outsideDir, globalSettings, linkedRoot, linkedProjectDir)
	require.NoError(t, err)
	assert.Nil(t, wsRoots)
	_, err = runtime.ValidateWorkspaceSource(outsideDir, wsRoots...)
	assert.NoError(t, err)
}

// TestHostedExplicitWorkspaceRoots_IgnoresProjectSettings verifies that
// project-level settings.yaml values (server.workspace_storage,
// server.project_storage, workspace_path) cannot widen the broker's hosted
// explicit-workspace containment roots.
func TestHostedExplicitWorkspaceRoots_IgnoresProjectSettings(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	globalDir := filepath.Join(tmpHome, ".scion")
	require.NoError(t, os.MkdirAll(globalDir, 0o755))

	outsideMountRoot := t.TempDir()
	outsideCandidate := filepath.Join(outsideMountRoot, "share1", "projects", "proj-1", "workspace")
	require.NoError(t, os.MkdirAll(outsideCandidate, 0o755))

	outsideWorkspacePath := filepath.Join(t.TempDir(), "custom-ws-root")
	require.NoError(t, os.MkdirAll(outsideWorkspacePath, 0o755))

	projectDir := filepath.Join(globalDir, "projects", "demo-proj", ".scion")
	require.NoError(t, os.MkdirAll(projectDir, 0o755))
	projectSettingsYAML := fmt.Sprintf(`schema_version: "1"
workspace_path: %q
server:
  workspace_storage:
    backend: nfs
    nfs:
      mount_root: %q
      subpath_root: projects
      shares:
        - id: share1
          server: 10.0.0.1
          export: /export
`, outsideWorkspacePath, outsideMountRoot)
	require.NoError(t, os.WriteFile(filepath.Join(projectDir, "settings.yaml"), []byte(projectSettingsYAML), 0o644))

	roots, err := hostedExplicitWorkspaceRoots(context.Background(), projectDir, projectDir)
	require.NoError(t, err)
	require.NotEmpty(t, roots)

	// Neither the project-level server.workspace_storage mount_root nor
	// project-level workspace_path is admitted.
	_, err = runtime.ValidateWorkspaceSource(outsideCandidate, roots...)
	assert.Error(t, err, "project-level server.workspace_storage must not widen hosted L3 roots")

	_, err = runtime.ValidateWorkspaceSource(outsideWorkspacePath, roots...)
	assert.Error(t, err, "project-level workspace_path must not widen hosted L3 roots")
}

// TestStart_HostedExplicitWorkspaceContainment exercises the Start-path L3
// guard directly:
//   - a pre-existing agent with a persisted explicit workspace outside the
//     hosted roots is refused on Start (without reaching ProvisionAgent or
//     runtime.Run);
//   - a marker-style linked project with a project-relative workspace succeeds
//     through both Provision and Start;
//   - the same marker-style project whose external settings.yaml points
//     workspace_path outside the marker's directory is refused on Start;
//   - an in-repository .scion project with a relative workspace succeeds.
func TestStart_HostedExplicitWorkspaceContainment(t *testing.T) {
	writeMinimalHarness := func(t *testing.T, dir string) {
		t.Helper()
		hcDir := filepath.Join(dir, "harness-configs", "claude")
		require.NoError(t, os.MkdirAll(hcDir, 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(hcDir, "config.yaml"), []byte("harness: claude\nimage: dummy:latest\n"), 0o644))
	}

	setupMarkerProject := func(t *testing.T, tmpHome, workspacePath string) (repoDir, externalScionDir string) {
		t.Helper()
		repoDir = filepath.Join(t.TempDir(), "linked-repo")
		require.NoError(t, os.MkdirAll(filepath.Join(repoDir, "sub", "dir"), 0o755))

		marker := &config.ProjectMarker{
			ProjectID:   "12345678-1234-1234-1234-1234567890ab",
			ProjectSlug: "linked-repo",
		}
		markerPath := filepath.Join(repoDir, config.DotScion)
		require.NoError(t, config.WriteProjectMarker(markerPath, marker))

		var err error
		externalScionDir, err = config.ResolveProjectMarker(markerPath)
		require.NoError(t, err)
		require.NoError(t, os.MkdirAll(externalScionDir, 0o755))
		writeMinimalHarness(t, externalScionDir)

		if workspacePath == "" {
			workspacePath = repoDir
		}
		settingsYAML := fmt.Sprintf("schema_version: \"1\"\nworkspace_path: %q\n", workspacePath)
		require.NoError(t, os.WriteFile(filepath.Join(externalScionDir, "settings.yaml"), []byte(settingsYAML), 0o644))
		return repoDir, externalScionDir
	}

	t.Run("persisted external explicit workspace refused on Start without ProvisionAgent", func(t *testing.T) {
		tmpHome := t.TempDir()
		t.Setenv("HOME", tmpHome)

		projectDir := filepath.Join(tmpHome, ".scion", "projects", "demo", ".scion")
		require.NoError(t, os.MkdirAll(projectDir, 0o755))
		writeMinimalHarness(t, projectDir)

		outsideDir := filepath.Join(t.TempDir(), "external-persisted-ws")
		require.NoError(t, os.MkdirAll(outsideDir, 0o755))

		// Pre-create the agent directory and scion-agent.json so GetAgent
		// returns the existing agent without invoking ProvisionAgent.
		agentDir := filepath.Join(projectDir, "agents", "existing-agent")
		require.NoError(t, os.MkdirAll(filepath.Join(agentDir, "home"), 0o755))
		cfgBytes, err := json.Marshal(&api.ScionConfig{
			Harness:           "claude",
			HarnessConfig:     "claude",
			Image:             "dummy:latest",
			ExplicitWorkspace: true,
			Volumes: []api.VolumeMount{
				{Source: outsideDir, Target: "/workspace"},
			},
		})
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(filepath.Join(agentDir, "scion-agent.json"), cfgBytes, 0o644))

		runCalled := false
		rt := &runtime.MockRuntime{
			NameFunc: func() string { return "docker" },
			RunFunc: func(_ context.Context, _ runtime.RunConfig) (string, error) {
				runCalled = true
				return "unexpected-container", nil
			},
		}
		mgr := NewManager(rt)
		_, err = mgr.Start(context.Background(), api.StartOptions{
			Name:        "existing-agent",
			ProjectPath: projectDir,
			HostedMode:  true,
		})
		require.Error(t, err)
		var lf *runtime.LaunchFailure
		require.True(t, errors.As(err, &lf), "expected LaunchFailure, got %v", err)
		assert.Equal(t, api.LaunchCodeWorkspaceNotPermitted, lf.Code)
		assert.False(t, runCalled, "runtime.Run must not be called when Start-path guard refuses")
	})

	t.Run("marker-style linked project relative workspace succeeds on hosted Start", func(t *testing.T) {
		tmpHome := t.TempDir()
		t.Setenv("HOME", tmpHome)
		repoDir, _ := setupMarkerProject(t, tmpHome, "")

		runCalled := false
		var capturedWS string
		rt := &runtime.MockRuntime{
			NameFunc: func() string { return "docker" },
			RunFunc: func(_ context.Context, rc runtime.RunConfig) (string, error) {
				runCalled = true
				capturedWS = rc.Workspace
				return "marker-container", nil
			},
		}
		mgr := NewManager(rt)
		_, err := mgr.Start(context.Background(), api.StartOptions{
			Name:          "marker-rel-agent",
			ProjectPath:   repoDir,
			Workspace:     "sub/dir",
			HarnessConfig: "claude",
			HostedMode:    true,
			NoAuth:        true,
		})
		require.NoError(t, err)
		assert.True(t, runCalled, "runtime.Run should be called for marker-style relative workspace")
		assert.True(t, samePath(capturedWS, filepath.Join(repoDir, "sub", "dir")),
			"workspace = %q, want %q", capturedWS, filepath.Join(repoDir, "sub", "dir"))
	})

	t.Run("marker-style linked project with workspace_path outside marker dir is refused on Start", func(t *testing.T) {
		tmpHome := t.TempDir()
		t.Setenv("HOME", tmpHome)

		outsideRoot := filepath.Join(t.TempDir(), "outside-root")
		require.NoError(t, os.MkdirAll(filepath.Join(outsideRoot, "sub", "dir"), 0o755))
		repoDir, _ := setupMarkerProject(t, tmpHome, outsideRoot)

		runCalled := false
		rt := &runtime.MockRuntime{
			NameFunc: func() string { return "docker" },
			RunFunc: func(_ context.Context, _ runtime.RunConfig) (string, error) {
				runCalled = true
				return "unexpected-container", nil
			},
		}
		mgr := NewManager(rt)
		_, err := mgr.Start(context.Background(), api.StartOptions{
			Name:          "marker-widened-agent",
			ProjectPath:   repoDir,
			Workspace:     "sub/dir",
			HarnessConfig: "claude",
			HostedMode:    true,
			NoAuth:        true,
		})
		require.Error(t, err)
		var lf *runtime.LaunchFailure
		require.True(t, errors.As(err, &lf), "expected LaunchFailure, got %v", err)
		assert.Equal(t, api.LaunchCodeWorkspaceNotPermitted, lf.Code)
		assert.False(t, runCalled, "runtime.Run must not be called when workspace_path points outside marker dir")
	})

	t.Run("in-repository .scion project relative workspace succeeds on hosted Start", func(t *testing.T) {
		tmpHome := t.TempDir()
		t.Setenv("HOME", tmpHome)

		repoDir := filepath.Join(t.TempDir(), "inrepo")
		scionDir := filepath.Join(repoDir, config.DotScion)
		require.NoError(t, os.MkdirAll(filepath.Join(repoDir, "sub", "dir"), 0o755))
		require.NoError(t, os.MkdirAll(scionDir, 0o755))
		writeMinimalHarness(t, scionDir)

		runCalled := false
		rt := &runtime.MockRuntime{
			NameFunc: func() string { return "docker" },
			RunFunc: func(_ context.Context, _ runtime.RunConfig) (string, error) {
				runCalled = true
				return "inrepo-container", nil
			},
		}
		mgr := NewManager(rt)
		_, err := mgr.Start(context.Background(), api.StartOptions{
			Name:          "inrepo-rel-agent",
			ProjectPath:   repoDir,
			Workspace:     "sub/dir",
			HarnessConfig: "claude",
			HostedMode:    true,
			NoAuth:        true,
		})
		require.NoError(t, err)
		assert.True(t, runCalled)
	})
}

func TestReprovision_HostedRefusesExternalAbsoluteWorkspace(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)

	projectDir := filepath.Join(tmpHome, ".scion", "projects", "demo", ".scion")
	agentDir := filepath.Join(projectDir, "agents", "test-agent")
	require.NoError(t, os.MkdirAll(agentDir, 0o755))

	outsideDir := filepath.Join(t.TempDir(), "external-workspace")
	require.NoError(t, os.MkdirAll(outsideDir, 0o755))

	mgr := &AgentManager{Runtime: &runtime.MockRuntime{}}
	_, err := mgr.Reprovision(context.Background(), api.StartOptions{
		Name:        "test-agent",
		ProjectPath: projectDir,
		Workspace:   outsideDir,
		HostedMode:  true,
	})
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrReprovisionRefused)

	var lf *runtime.LaunchFailure
	require.True(t, errors.As(err, &lf), "expected LaunchFailure, got %v", err)
	assert.Equal(t, api.LaunchCodeWorkspaceNotPermitted, lf.Code)
}

func TestProvisionAgent_HostedRefusesExternalAbsoluteWorkspace(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)

	projectDir := filepath.Join(tmpHome, ".scion", "projects", "demo", ".scion")
	hcDir := filepath.Join(projectDir, "harness-configs", "claude")
	require.NoError(t, os.MkdirAll(hcDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(hcDir, "config.yaml"), []byte("harness: claude\nimage: dummy:latest\n"), 0o644))

	outsideDir := filepath.Join(t.TempDir(), "external-workspace")
	require.NoError(t, os.MkdirAll(outsideDir, 0o755))

	ctx := api.ContextWithHostedMode(context.Background())
	_, _, _, err := ProvisionAgent(ctx, "test-agent", "", "", "claude", projectDir, "", "created", "", outsideDir, nil)
	require.Error(t, err)

	var lf *runtime.LaunchFailure
	require.True(t, errors.As(err, &lf), "expected LaunchFailure, got %v", err)
	assert.Equal(t, api.LaunchCodeWorkspaceNotPermitted, lf.Code)
}
