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

//go:build unix

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
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/homeprep"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	k8sTestProjectUUID = "11111111-2222-3333-4444-555555555555"
	k8sTestAgentUUID   = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
)

func setupK8sNFSFixture(t *testing.T) (mountRoot, hostBase, projectPath string) {
	t.Helper()
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("SCION_CONFIG_DIR", filepath.Join(tmp, ".scion"))

	mountRoot = filepath.Join(tmp, "nfs-mounts")
	hostBase = filepath.Join(mountRoot, "share-1")
	require.NoError(t, os.MkdirAll(hostBase, 0o755))

	globalDir := filepath.Join(tmp, ".scion")
	require.NoError(t, os.MkdirAll(globalDir, 0o755))
	settingsYAML := `schema_version: "1"
active_profile: gke
runtimes:
  kubernetes:
    type: kubernetes
profiles:
  gke:
    runtime: kubernetes
server:
  project_storage:
    backend: nfs
    homes: nfs
    homes_options:
      leaf: pod
      allow_incomplete_phases: true
    nfs:
      mount_root: ` + mountRoot + `
      subpath_root: projects
      shares:
        - id: share-1
          server: 10.0.0.10
          export: /export
          pv_name: pv-1
`
	require.NoError(t, os.WriteFile(filepath.Join(globalDir, "settings.yaml"), []byte(settingsYAML), 0o644))

	projectPath = filepath.Join(globalDir, "projects", "demo", ".scion")
	require.NoError(t, os.MkdirAll(projectPath, 0o755))
	require.NoError(t, config.InitGlobal(getTestHarnesses()))
	// Re-write global settings after InitGlobal so our K8s NFS settings stay intact.
	require.NoError(t, os.WriteFile(filepath.Join(globalDir, "settings.yaml"), []byte(settingsYAML), 0o644))
	return mountRoot, hostBase, projectPath
}

func TestStart_KubernetesNFSWritesMoveBundleHomeRecord(t *testing.T) {
	f := newSharedDirStorageRunFixture(t)
	f.writeRawGlobalSettings(t, homeStorageStartSettings(f.tmpDir))

	var c sdsCapture
	rt := newSDSMockRuntime("kubernetes", &c)
	opts := homeStorageStartOpts(f, "k8s-rec-agent", "gke")
	opts.ResolvedSecrets = []api.ResolvedSecret{
		{Name: "api_key", Type: "file", Target: "~/.config/app/secret.key", Value: "top-secret"},
		{Name: "ws_env", Type: "file", Target: "/workspace/.env", Value: "SECRET=1"},
	}

	_, err := NewManager(rt).Start(withNFSHomeExperiment(context.Background()), opts)
	require.NoError(t, err)
	assert.Equal(t, runtime.HomeStorageNFS, c.cfg.HomeStorageBackend)

	agentDir, err := provisionAgentStateDir(contextWithAgentRuntime(context.Background(), "kubernetes"), opts)
	require.NoError(t, err)
	rec, err := ReadMoveBundleHomeRecord(agentDir)
	require.NoError(t, err, "Kubernetes NFS Start must persist .scion-move-bundle-home.json")
	assert.NotEmpty(t, rec.UnixUsername)
	assert.Equal(t, "/home/"+rec.UnixUsername, rec.HomeAbs)
	assert.Equal(t, "/workspace", rec.WorkspaceAbs)
	assert.Contains(t, rec.HomeExcludes, ".config/app/secret.key")
	assert.Contains(t, rec.WorkspaceExcludes, ".env")
}

func TestMoveBundle_K8sDurabilityAndResolveDirs(t *testing.T) {
	t.Run("K8s NFS durable source resolves export workspace and NFS home leaf", func(t *testing.T) {
		_, hostBase, projectPath := setupK8sNFSFixture(t)
		slug := "k8s-worker"
		opts := api.StartOptions{
			Name:         slug,
			ProjectPath:  projectPath,
			HubProjectID: k8sTestProjectUUID,
			BrokerMode:   true,
		}
		agentDir, err := provisionAgentStateDir(contextWithAgentRuntime(context.Background(), "kubernetes"), opts)
		require.NoError(t, err)
		require.NoError(t, os.MkdirAll(agentDir, 0o755))
		require.NoError(t, writeHomeStorageRecord(agentDir, homeStorageRecord{
			Backend:     config.HomeStorageBackendNFS,
			Leaf:        config.HomeStorageLeafPod,
			ShareID:     "share-1",
			PVClaimName: "pv-1",
			SubPathRoot: "projects",
		}))

		in := MoveBundleRuntimeInput{
			RuntimeName: "kubernetes",
			AgentSlug:   slug,
			AgentID:     k8sTestAgentUUID,
			ProjectPath: projectPath,
			ProjectID:   k8sTestProjectUUID,
			BrokerMode:  true,
		}
		dur, err := CheckMoveBundleSourceDurability(context.Background(), in)
		require.NoError(t, err)
		assert.True(t, dur.Durable)
		assert.Empty(t, dur.Reason)

		dirs, err := ResolveMoveBundleDirsForRuntime(context.Background(), in, false)
		require.NoError(t, err)
		realHostBase, err := filepath.EvalSymlinks(hostBase)
		require.NoError(t, err)
		wantExportAgent := filepath.Join(realHostBase, "projects", k8sTestProjectUUID, "agents", slug)
		assert.Equal(t, agentDir, dirs.AgentDir)
		assert.Equal(t, wantExportAgent, dirs.ExportAgentDir)
		assert.Equal(t, filepath.Join(wantExportAgent, "workspace"), dirs.WorkspaceDir)
		assert.Equal(t, filepath.Join(wantExportAgent, "home-"+k8sTestAgentUUID), dirs.HomeDir)
		assert.False(t, dirs.SharedWorkspace)
	})

	t.Run("K8s without NFS home is refused", func(t *testing.T) {
		_, _, projectPath := setupK8sNFSFixture(t)
		slug := "k8s-local-home"
		opts := api.StartOptions{
			Name:         slug,
			ProjectPath:  projectPath,
			HubProjectID: k8sTestProjectUUID,
			BrokerMode:   true,
		}
		agentDir, err := provisionAgentStateDir(contextWithAgentRuntime(context.Background(), "kubernetes"), opts)
		require.NoError(t, err)
		require.NoError(t, os.MkdirAll(agentDir, 0o755))
		require.NoError(t, writeHomeStorageRecord(agentDir, homeStorageRecord{
			Backend: config.HomeStorageBackendLocal,
		}))

		in := MoveBundleRuntimeInput{
			RuntimeName: "kubernetes",
			AgentSlug:   slug,
			AgentID:     k8sTestAgentUUID,
			ProjectPath: projectPath,
			ProjectID:   k8sTestProjectUUID,
			BrokerMode:  true,
		}
		dur, err := CheckMoveBundleSourceDurability(context.Background(), in)
		require.NoError(t, err)
		assert.False(t, dur.Durable)
		assert.Equal(t, k8sNonDurableHomeOnlyReason, dur.Reason)
	})

	t.Run("plain K8s without NFS workspace is refused", func(t *testing.T) {
		tmp := t.TempDir()
		t.Setenv("HOME", tmp)
		t.Setenv("SCION_CONFIG_DIR", filepath.Join(tmp, ".scion"))
		projectPath := filepath.Join(tmp, "proj", ".scion")
		require.NoError(t, os.MkdirAll(projectPath, 0o755))

		in := MoveBundleRuntimeInput{
			RuntimeName: "kubernetes",
			AgentSlug:   "plain-pod",
			AgentID:     k8sTestAgentUUID,
			ProjectPath: projectPath,
			ProjectID:   k8sTestProjectUUID,
			BrokerMode:  true,
		}
		dur, err := CheckMoveBundleSourceDurability(context.Background(), in)
		require.NoError(t, err)
		assert.False(t, dur.Durable)
		assert.Equal(t, k8sNonDurablePodReason, dur.Reason)
	})
}

func TestProvision_K8sNFSMoveBundleTargetAndRollback(t *testing.T) {
	_, hostBase, projectPath := setupK8sNFSFixture(t)
	slug := "k8s-target"
	rid := "rid-k8s-target-1"

	mgr := NewManager(&runtime.MockRuntime{
		NameFunc: func() string { return "kubernetes" },
	}).(*AgentManager)

	opts := api.StartOptions{
		Name:                slug,
		ProjectPath:         projectPath,
		HubProjectID:        k8sTestProjectUUID,
		BrokerMode:          true,
		NoAuth:              true,
		FreshProvision:      true,
		MoveReincarnationID: rid,
		WorkspaceSource:     api.WorkspaceSourceMoveBundle,
		Env: map[string]string{
			"SCION_PROJECT_ID": k8sTestProjectUUID,
			"SCION_AGENT_ID":   k8sTestAgentUUID,
		},
	}

	// CheckMoveWorkspaceAbsent succeeds before provision.
	_, err := mgr.CheckMoveWorkspaceAbsent(withNFSHomeExperiment(context.Background()), opts)
	require.NoError(t, err)

	cfg, err := mgr.Provision(withNFSHomeExperiment(context.Background()), opts)
	require.NoError(t, err)
	require.NotNil(t, cfg)

	realHostBase, err := filepath.EvalSymlinks(hostBase)
	require.NoError(t, err)
	exportAgentDir := filepath.Join(realHostBase, "projects", k8sTestProjectUUID, "agents", slug)
	exportWS := filepath.Join(exportAgentDir, "workspace")
	exportHome := filepath.Join(exportAgentDir, "home-"+k8sTestAgentUUID)

	// Move marker exists on export agent dir, workspace is strictly empty, home leaf is rendered with sentinel.
	assertMarker(t, exportAgentDir, rid)
	wsEntries, err := os.ReadDir(exportWS)
	require.NoError(t, err)
	assert.Empty(t, wsEntries, "K8s move-bundle target workspace must be empty")

	sentinelBytes, err := os.ReadFile(filepath.Join(exportHome, homeprep.SentinelName))
	require.NoError(t, err, "K8s move-bundle target must write .scion-home-seed.json sentinel")
	var sentinel homeprep.Sentinel
	require.NoError(t, json.Unmarshal(sentinelBytes, &sentinel))
	assert.Equal(t, k8sTestAgentUUID, sentinel.AgentID)
	assert.Equal(t, homeprep.StateSeeded, sentinel.State)
	assert.Equal(t, int(runtime.K8sContainerUID), sentinel.UID)

	// CheckMoveMarkers validates both broker-local and export agent dirs.
	_, err = mgr.CheckMoveMarkers(context.Background(), projectPath, k8sTestProjectUUID, k8sTestProjectUUID, slug, slug, rid)
	require.NoError(t, err)
	_, err = mgr.CheckMoveMarkers(context.Background(), projectPath, k8sTestProjectUUID, k8sTestProjectUUID, slug, slug, "wrong-rid")
	assert.ErrorIs(t, err, ErrMoveMarkerMismatch)

	// Simulate leftover .bundle-* staging siblings from an interrupted import.
	wsStage := filepath.Join(exportAgentDir, ".workspace.bundle-0123456789abcdef0123456789abcdef")
	homeStage := filepath.Join(exportAgentDir, ".home-"+k8sTestAgentUUID+".bundle-fedcba9876543210fedcba9876543210")
	require.NoError(t, os.MkdirAll(wsStage, 0o755))
	require.NoError(t, os.MkdirAll(homeStage, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(wsStage, "partial.txt"), []byte("x"), 0o644))

	// Run rollback NFS cleanup and assert immediate removal of workspace, home leaf, and .bundle-* staging siblings.
	ds, err := mgr.RemoveNFSAgentFiles(WithDeleteHubAgentID(context.Background(), k8sTestAgentUUID), projectPath, k8sTestProjectUUID, slug)
	require.NoError(t, err)
	assert.NotEmpty(t, ds)

	assert.NoDirExists(t, exportWS, "NFS workspace must be removed on rollback")
	assert.NoDirExists(t, exportHome, "NFS home leaf must be removed on rollback")
	assert.NoDirExists(t, wsStage, "workspace .bundle-* staging sibling must be removed on rollback")
	assert.NoDirExists(t, homeStage, "home .bundle-* staging sibling must be removed on rollback")
}
