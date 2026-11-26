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

	roots, err := hostedExplicitWorkspaceRootsFromGlobal(globalSettings, linkedProjectDir)
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
	wsRoots, err := workspaceSourceRootsForMode(context.Background(), true, false, outsideDir, globalSettings, linkedProjectDir)
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

	roots, err := hostedExplicitWorkspaceRoots(context.Background(), projectDir)
	require.NoError(t, err)
	require.NotEmpty(t, roots)

	// Neither the project-level server.workspace_storage mount_root nor
	// project-level workspace_path is admitted.
	_, err = runtime.ValidateWorkspaceSource(outsideCandidate, roots...)
	assert.Error(t, err, "project-level server.workspace_storage must not widen hosted L3 roots")

	_, err = runtime.ValidateWorkspaceSource(outsideWorkspacePath, roots...)
	assert.Error(t, err, "project-level workspace_path must not widen hosted L3 roots")
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
