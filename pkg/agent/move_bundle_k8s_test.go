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

	t.Run("K8s NFS source with unreadable 0000 home file fails closed with class-only reason", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root bypasses 0000 read permissions")
		}
		_, hostBase, projectPath := setupK8sNFSFixture(t)
		slug := "k8s-unreadable"
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

		realHostBase, err := filepath.EvalSymlinks(hostBase)
		require.NoError(t, err)
		homeDir := filepath.Join(realHostBase, "projects", k8sTestProjectUUID, "agents", slug, "home-"+k8sTestAgentUUID)
		require.NoError(t, os.MkdirAll(homeDir, 0o755))
		unreadableFile := filepath.Join(homeDir, "pod-private.txt")
		require.NoError(t, os.WriteFile(unreadableFile, []byte("secret"), 0o000))
		t.Cleanup(func() { _ = os.Chmod(unreadableFile, 0o600) })

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
		assert.Equal(t, "broker lacks read permission on source NFS home", dur.Reason)
		assert.NotContains(t, dur.Reason, homeDir, "reason must be class-only and never leak host paths")
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

	type chownCall struct {
		uid int
		gid int
	}
	chowned := map[string]chownCall{}
	origLchown := moveBundleTargetLchown
	moveBundleTargetLchown = func(name string, uid, gid int) error {
		chowned[name] = chownCall{uid: uid, gid: gid}
		return nil
	}
	t.Cleanup(func() { moveBundleTargetLchown = origLchown })

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

	// Move marker exists on export agent dir AND on home leaf, workspace is strictly empty, home leaf is rendered with sentinel.
	assertMarker(t, exportAgentDir, rid)
	assertMarker(t, exportHome, rid)
	wsEntries, err := os.ReadDir(exportWS)
	require.NoError(t, err)
	assert.Empty(t, wsEntries, "K8s move-bundle target workspace must be empty")

	// Canonical leaf modes: 2775 for export agent dir & workspace, 2771 for home leaf.
	agentDirInfo, err := os.Stat(exportAgentDir)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o775), agentDirInfo.Mode().Perm(), "exportAgentDir perms")
	assert.NotZero(t, agentDirInfo.Mode()&os.ModeSetgid, "exportAgentDir must have setgid bit")

	wsInfo, err := os.Stat(exportWS)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o775), wsInfo.Mode().Perm(), "exportWS perms")
	assert.NotZero(t, wsInfo.Mode()&os.ModeSetgid, "exportWS must have setgid bit")

	homeInfo, err := os.Stat(exportHome)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o771), homeInfo.Mode().Perm(), "exportHome perms")
	assert.NotZero(t, homeInfo.Mode()&os.ModeSetgid, "exportHome must have setgid bit")

	// Chown applied to home leaf and sentinel with K8sContainerUID:homeStorageGID.
	wantOwner := chownCall{uid: int(runtime.K8sContainerUID), gid: homeStorageGID}
	assert.Equal(t, wantOwner, chowned[exportHome], "exportHome must be chowned to pod UID:GID")
	assert.Equal(t, wantOwner, chowned[filepath.Join(exportHome, homeprep.SentinelName)], "sentinel must be chowned to pod UID:GID")

	sentinelBytes, err := os.ReadFile(filepath.Join(exportHome, homeprep.SentinelName))
	require.NoError(t, err, "K8s move-bundle target must write .scion-home-seed.json sentinel")
	var sentinel homeprep.Sentinel
	require.NoError(t, json.Unmarshal(sentinelBytes, &sentinel))
	assert.Equal(t, k8sTestAgentUUID, sentinel.AgentID)
	assert.Equal(t, homeprep.StateSeeded, sentinel.State)
	assert.Equal(t, int(runtime.K8sContainerUID), sentinel.UID)

	// CheckMoveMarkers validates both broker-local and export agent dirs.
	checkCtx := WithDeleteHubAgentID(context.Background(), k8sTestAgentUUID)
	_, err = mgr.CheckMoveMarkers(checkCtx, projectPath, k8sTestProjectUUID, k8sTestProjectUUID, slug, slug, rid)
	require.NoError(t, err)
	_, err = mgr.CheckMoveMarkers(checkCtx, projectPath, k8sTestProjectUUID, k8sTestProjectUUID, slug, slug, "wrong-rid")
	assert.ErrorIs(t, err, ErrMoveMarkerMismatch)

	// Simulate leftover .bundle-* staging siblings from an interrupted import.
	wsStage := filepath.Join(exportAgentDir, ".workspace.bundle-0123456789abcdef0123456789abcdef")
	homeStage := filepath.Join(exportAgentDir, ".home-"+k8sTestAgentUUID+".bundle-fedcba9876543210fedcba9876543210")
	require.NoError(t, os.MkdirAll(wsStage, 0o755))
	require.NoError(t, os.MkdirAll(homeStage, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(wsStage, "partial.txt"), []byte("x"), 0o644))

	// Normal delete (without WithDeleteMoveReincarnationID) must NOT touch NFS home leaf or .bundle-* staging.
	_, err = mgr.RemoveNFSAgentFiles(WithDeleteHubAgentID(context.Background(), k8sTestAgentUUID), projectPath, k8sTestProjectUUID, slug)
	require.NoError(t, err)
	assert.DirExists(t, exportHome, "normal delete must leave NFS home leaf intact (Hub owns RemoveAgentStorage)")
	assert.DirExists(t, homeStage, "normal delete must leave .bundle-* staging untouched")

	// Recreate workspace so rollback delete exercises full rollback cleanup.
	require.NoError(t, os.MkdirAll(exportWS, 0o755))

	// Rollback delete (with WithDeleteMoveReincarnationID) removes workspace, home leaf, and .bundle-* staging siblings.
	rbCtx := WithDeleteMoveReincarnationID(WithDeleteHubAgentID(context.Background(), k8sTestAgentUUID), rid)
	ds, err := mgr.RemoveNFSAgentFiles(rbCtx, projectPath, k8sTestProjectUUID, slug)
	require.NoError(t, err)
	assert.NotEmpty(t, ds)

	assert.NoDirExists(t, exportWS, "NFS workspace must be removed on rollback")
	assert.NoDirExists(t, exportHome, "NFS home leaf must be removed on rollback")
	assert.NoDirExists(t, wsStage, "workspace .bundle-* staging sibling must be removed on rollback")
	assert.NoDirExists(t, homeStage, "home .bundle-* staging sibling must be removed on rollback")
}

func TestProvision_K8sNFSMoveBundle_DistinctHomeShareUnmarkedLeafSurvivesRollback(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("SCION_CONFIG_DIR", filepath.Join(tmp, ".scion"))

	mountRoot := filepath.Join(tmp, "nfs-mounts")
	wsHostBase := filepath.Join(mountRoot, "ws-share")
	homeHostBase := filepath.Join(mountRoot, "home-share")
	require.NoError(t, os.MkdirAll(wsHostBase, 0o755))
	require.NoError(t, os.MkdirAll(homeHostBase, 0o755))

	globalDir := filepath.Join(tmp, ".scion")
	require.NoError(t, os.MkdirAll(globalDir, 0o755))
	// Configure workspace storage on ws-share and home storage (project_storage) on distinct home-share.
	settingsYAML := `schema_version: "1"
active_profile: gke
runtimes:
  kubernetes:
    type: kubernetes
profiles:
  gke:
    runtime: kubernetes
server:
  workspace_storage:
    backend: nfs
    nfs:
      mount_root: ` + mountRoot + `
      subpath_root: ws-projects
      shares:
        - id: ws-share
          server: 10.0.0.10
          export: /ws-export
          pv_name: pv-ws
  project_storage:
    backend: nfs
    homes: nfs
    homes_options:
      leaf: pod
      allow_incomplete_phases: true
    nfs:
      mount_root: ` + mountRoot + `
      subpath_root: homes
      shares:
        - id: home-share
          server: 10.0.0.11
          export: /home-export
          pv_name: pv-home
`
	require.NoError(t, os.WriteFile(filepath.Join(globalDir, "settings.yaml"), []byte(settingsYAML), 0o644))
	projectPath := filepath.Join(globalDir, "projects", "demo", ".scion")
	require.NoError(t, os.MkdirAll(projectPath, 0o755))
	require.NoError(t, config.InitGlobal(getTestHarnesses()))
	require.NoError(t, os.WriteFile(filepath.Join(globalDir, "settings.yaml"), []byte(settingsYAML), 0o644))

	realHomeBase, err := filepath.EvalSymlinks(homeHostBase)
	require.NoError(t, err)
	slug := "k8s-distinct"
	rid := "rid-k8s-distinct-1"

	// Pre-populate an UNMARKED home-<agentID> on the distinct home share.
	preExistingHome := filepath.Join(realHomeBase, "homes", k8sTestProjectUUID, "agents", slug, "home-"+k8sTestAgentUUID)
	require.NoError(t, os.MkdirAll(preExistingHome, 0o755))
	preciousFile := filepath.Join(preExistingHome, "precious.txt")
	require.NoError(t, os.WriteFile(preciousFile, []byte("do not delete"), 0o600))

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

	// Provision must fail because pre-existing home-<agentID> is present.
	_, err = mgr.Provision(withNFSHomeExperiment(context.Background()), opts)
	require.Error(t, err)

	// CheckMoveMarkers must detect the unmarked home leaf and return ErrMoveMarkerMismatch.
	checkCtx := WithDeleteHubAgentID(context.Background(), k8sTestAgentUUID)
	_, err = mgr.CheckMoveMarkers(checkCtx, projectPath, k8sTestProjectUUID, k8sTestProjectUUID, slug, slug, rid)
	assert.ErrorIs(t, err, ErrMoveMarkerMismatch)

	// Even if RemoveNFSAgentFiles is invoked directly for rollback, the unmarked home leaf MUST survive.
	rbCtx := WithDeleteMoveReincarnationID(checkCtx, rid)
	_, err = mgr.RemoveNFSAgentFiles(rbCtx, projectPath, k8sTestProjectUUID, slug)
	require.NoError(t, err)
	data, err := os.ReadFile(preciousFile)
	require.NoError(t, err, "pre-existing unmarked home leaf on distinct share must survive rollback")
	assert.Equal(t, "do not delete", string(data))
}

func TestCopyProvisionedHomeToNFSLeaf_AppliesPortableHomeExcludes(t *testing.T) {
	src := t.TempDir()
	dst := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(src, ".scion"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(src, ".scion", "secrets.json"), []byte(`{"k":"v"}`), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(src, ".gitconfig"), []byte("[user]\nname=test\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(src, ".state.tmp-0123456789abcdef"), []byte("tmp"), 0o644))

	origLchown := moveBundleTargetLchown
	moveBundleTargetLchown = func(string, int, int) error { return nil }
	t.Cleanup(func() { moveBundleTargetLchown = origLchown })

	require.NoError(t, copyProvisionedHomeToNFSLeaf(src, dst))

	assert.FileExists(t, filepath.Join(dst, ".gitconfig"))
	assert.NoFileExists(t, filepath.Join(dst, ".scion", "secrets.json"), "portable home exclude (.scion/secrets.json) must not be copied")
	assert.NoFileExists(t, filepath.Join(dst, ".state.tmp-0123456789abcdef"), "temp files matching IsHomeTempName must not be copied")
}
