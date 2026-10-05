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
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/provision"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

var emptyPerAgentEnv = map[string]string{"SCION_WORKSPACE_MODE": "empty-per-agent"}

func TestNFSEmptyAgentDirSelection(t *testing.T) {
	nfs := &config.V1WorkspaceStorageConfig{Backend: "nfs"}
	for _, tc := range []struct {
		name    string
		runtime string
		cfg     *config.V1WorkspaceStorageConfig
		agent   string
		want    string
		wantErr error
	}{
		{name: "no storage config", runtime: "kubernetes", agent: "worker"},
		{name: "local storage", runtime: "kubernetes", cfg: &config.V1WorkspaceStorageConfig{Backend: "local"}, agent: "worker"},
		{name: "gke-shared-volume", runtime: "kubernetes", cfg: &config.V1WorkspaceStorageConfig{Backend: "gke-shared-volume"}, agent: "worker"},
		{name: "nfs on kubernetes", runtime: "kubernetes", cfg: nfs, agent: "worker", want: "worker"},
		{name: "nfs on docker", runtime: "docker", cfg: nfs, agent: "worker", wantErr: errEmptyPerAgentNFSRuntime},
		{name: "nfs with a non-slug name", runtime: "kubernetes", cfg: nfs, agent: "My_Agent"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := nfsEmptyAgentDirSelection(tc.runtime, tc.cfg, tc.agent)
			switch {
			case tc.wantErr != nil:
				require.ErrorIs(t, err, tc.wantErr)
			case tc.cfg != nil && tc.cfg.Backend == "nfs" && tc.want == "":
				require.Error(t, err, "a non-slug name must not fall back to the project's workspace")
			default:
				require.NoError(t, err)
			}
			assert.Equal(t, tc.want, got)
		})
	}
}

// First start of an empty-per-agent agent on Kubernetes with NFS storage:
// only the agent's directory and its workspace are created on the export
// (with the leaf modes), the project's shared workspace is not, the pod is
// told to use the agent directory with an empty workspace and no branch,
// and the workspace source handed to the runtime is never on the project's
// shared path.
func TestStartNFSEmptyAgentDir_FirstStart(t *testing.T) {
	mountRoot := filepath.Join(t.TempDir(), "nfs")
	require.NoError(t, os.MkdirAll(filepath.Join(mountRoot, "share-1"), 0o755))

	cfg, emptyAtRun, ran, err := startNFSAgentDirAgentNamed(t, "test-agent", "kubernetes", mountRoot, emptyPerAgentEnv, nil)
	require.NoError(t, err)
	require.True(t, ran)
	assert.True(t, emptyAtRun, "the agent's workspace must exist, empty, when the pod is created")
	assert.Equal(t, "test-agent", cfg.NFSAgentDirName)
	assert.True(t, cfg.NFSAgentDirEmpty)
	assert.Empty(t, cfg.NFSAgentBranch)
	assert.Empty(t, cfg.NFSWorktreeName)
	assert.Nil(t, cfg.GitCloneForInit)
	assert.Equal(t, "nfs", cfg.WorkspaceBackendName)
	assert.Equal(t, "ws-pv", cfg.NFSPVClaimName)
	assert.Equal(t, "/workspace", cfg.ContainerWorkspace)
	assert.True(t, cfg.NFSWorkspacePreCreated)
	assert.NotEqual(t, nfsTestWorkspaceDir(mountRoot), cfg.Workspace)
	assert.False(t, strings.HasPrefix(cfg.Workspace, mountRoot), "workspace source %q is on the export", cfg.Workspace)
	assert.Equal(t, "agents", filepath.Base(filepath.Dir(filepath.Dir(cfg.Workspace))))

	assert.NoDirExists(t, nfsTestWorkspaceDir(mountRoot), "the project's shared workspace is not created")
	agentDir := nfsTestAgentDir(mountRoot, "test-agent")
	assert.Equal(t, []string{provision.AgentWorkspaceDir}, nfsDirNames(t, agentDir), "only workspace is created in the agent directory")
	for _, dir := range []string{agentDir, filepath.Join(agentDir, provision.AgentWorkspaceDir)} {
		assert.Equal(t, uint32(unix.S_ISGID|0o775), statMode(t, dir).Mode&0o7777, dir)
	}
}

// A restart keeps the files in the agent's workspace on the export.
func TestStartNFSEmptyAgentDir_RestartKeepsFiles(t *testing.T) {
	mountRoot := filepath.Join(t.TempDir(), "nfs")
	ws := filepath.Join(nfsTestAgentDir(mountRoot, "test-agent"), provision.AgentWorkspaceDir)
	require.NoError(t, os.MkdirAll(ws, 0o770))
	require.NoError(t, os.WriteFile(filepath.Join(ws, "notes.txt"), []byte("kept"), 0o644))

	cfg, _, _, err := startNFSAgentDirAgentNamed(t, "test-agent", "kubernetes", mountRoot, emptyPerAgentEnv, nil)
	require.NoError(t, err)
	assert.Equal(t, "test-agent", cfg.NFSAgentDirName)
	assert.FileExists(t, filepath.Join(ws, "notes.txt"))
}

// The export not mounted on the broker: the pod still uses the agent
// directory, and the node creates it.
func TestStartNFSEmptyAgentDir_ExportNotMounted(t *testing.T) {
	mountRoot := filepath.Join(t.TempDir(), "nfs")
	cfg, _, _, err := startNFSAgentDirAgentNamed(t, "test-agent", "kubernetes", mountRoot, emptyPerAgentEnv, nil)
	require.NoError(t, err)
	assert.Equal(t, "test-agent", cfg.NFSAgentDirName)
	assert.True(t, cfg.NFSAgentDirEmpty)
	assert.False(t, cfg.NFSWorkspacePreCreated)
	assert.NoDirExists(t, mountRoot)
}

// An empty-per-agent agent on NFS storage that cannot get its own
// directory (a non-slug name, or a runtime other than Kubernetes) stops
// the start: the pod is not created and nothing is created on the export,
// so the agent never falls back to the project's shared workspace.
func TestStartNFSEmptyAgentDir_FailsClosed(t *testing.T) {
	for _, tc := range []struct {
		name, agent, runtime string
		wantErr              error
	}{
		{name: "non-slug name", agent: "My_Agent", runtime: "kubernetes"},
		{name: "dotted name", agent: "a.b", runtime: "kubernetes"},
		{name: "docker runtime", agent: "test-agent", runtime: "docker", wantErr: errEmptyPerAgentNFSRuntime},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mountRoot := filepath.Join(t.TempDir(), "nfs")
			require.NoError(t, os.MkdirAll(filepath.Join(mountRoot, "share-1"), 0o755))
			_, _, ran, err := startNFSAgentDirAgentNamed(t, tc.agent, tc.runtime, mountRoot, emptyPerAgentEnv, nil)
			require.Error(t, err)
			if tc.wantErr != nil {
				assert.True(t, errors.Is(err, tc.wantErr), "got %v", err)
			}
			assert.False(t, ran, "the pod must not be created")
			assert.Equal(t, []string{"share-1"}, nfsDirNames(t, mountRoot))
			assert.Empty(t, nfsDirNames(t, filepath.Join(mountRoot, "share-1")), "nothing is created on the export")
		})
	}
}

// NFS storage whose share has no pv_name: the pod could not mount the
// agent's directory, so the start stops instead of using the project's
// workspace path.
func TestStartNFSEmptyAgentDir_NoClaimFailsClosed(t *testing.T) {
	mountRoot := filepath.Join(t.TempDir(), "nfs")
	require.NoError(t, os.MkdirAll(filepath.Join(mountRoot, "share-1"), 0o755))
	f := newSharedDirStorageRunFixture(t)
	f.writeGlobalSettings(t, strings.Replace(fmt.Sprintf(nfsWorkspaceStartYAML, mountRoot), "          pv_name: ws-pv\n", "", 1))
	ran := false
	mockRT := &runtime.MockRuntime{
		NameFunc: func() string { return "kubernetes" },
		RunFunc: func(ctx context.Context, rc runtime.RunConfig) (string, error) {
			ran = true
			return "mock-id", nil
		},
	}
	_, err := NewManager(mockRT).Start(context.Background(), api.StartOptions{
		Name:        "test-agent",
		ProjectPath: f.projectScionDir,
		NoAuth:      true,
		Env:         map[string]string{"SCION_PROJECT_ID": testNFSWorkspaceProjectID, "SCION_WORKSPACE_MODE": "empty-per-agent"},
	})
	require.ErrorIs(t, err, errEmptyPerAgentNFSNoClaim)
	assert.False(t, ran, "the pod must not be created")
	assert.Empty(t, nfsDirNames(t, filepath.Join(mountRoot, "share-1")), "nothing is created on the export")
}

// Delete with files of an empty-per-agent agent on the NFS export, through
// the broker's existing name-keyed remover: the agent's workspace is
// removed, and the agent directory and anything else in it (such as a home
// directory kept next to the workspace) stay. Created again, the agent
// starts from an empty workspace.
func TestRemoveNFSAgentFiles_EmptyPerAgentKeepsSiblings(t *testing.T) {
	f := newSharedDirStorageRunFixture(t)
	mountRoot := filepath.Join(t.TempDir(), "nfs")
	f.writeGlobalSettings(t, fmt.Sprintf(nfsWorkspaceStartYAML, mountRoot))
	agentDir := nfsTestAgentDir(mountRoot, "test-agent")
	require.NoError(t, os.MkdirAll(agentDir, 0o770))
	in := provision.ProvisionInput{
		Resolved:  provision.ResolvedWorkspace{HostPath: agentDir},
		ProjectID: testNFSWorkspaceProjectID,
		AgentID:   "test-agent",
		Mode:      store.SharingModeEmptyPerAgent,
		NFSUID:    os.Getuid(),
		NFSGID:    os.Getgid(),
	}
	require.NoError(t, provision.ProvisionAgentDir(in))
	ws := filepath.Join(agentDir, provision.AgentWorkspaceDir)
	require.NoError(t, os.WriteFile(filepath.Join(ws, "notes.txt"), []byte("x"), 0o644))
	home := filepath.Join(agentDir, "home-agent-id-1")
	require.NoError(t, os.MkdirAll(home, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(home, ".profile"), []byte("x"), 0o644))
	runAgentWorkspacePurgeInline(t)

	paths, err := kubernetesTestManager("kubernetes").RemoveNFSAgentFiles(context.Background(), f.projectScionDir, testNFSWorkspaceProjectID, "test-agent")
	require.NoError(t, err)
	assert.Contains(t, paths, agentDir)
	assert.NoDirExists(t, ws)
	assert.FileExists(t, filepath.Join(home, ".profile"), "a sibling of the workspace is kept")
	assert.DirExists(t, agentDir)

	require.NoError(t, provision.ProvisionAgentDir(in))
	assert.Empty(t, nfsDirNames(t, ws))
}
