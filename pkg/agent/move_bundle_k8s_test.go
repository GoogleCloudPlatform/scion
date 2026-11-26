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
	"github.com/GoogleCloudPlatform/scion/pkg/projectfs"
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

		dirs, err := ResolveMoveBundleDirsForRuntime(context.Background(), in)
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

	t.Run("unregistered runtime fails closed in ResolveMoveBundleDirsForRuntime and Provision (R1)", func(t *testing.T) {
		_, _, projectPath := setupK8sNFSFixture(t)
		in := MoveBundleRuntimeInput{
			RuntimeName: "substrate",
			AgentSlug:   "sub-agent",
			AgentID:     k8sTestAgentUUID,
			ProjectPath: projectPath,
			ProjectID:   k8sTestProjectUUID,
			BrokerMode:  true,
		}
		_, err := ResolveMoveBundleDirsForRuntime(context.Background(), in)
		require.Error(t, err, "unregistered runtime must not fall back to broker-local dirs")
		assert.Contains(t, err.Error(), "not supported")

		subMgr := NewManager(&runtime.MockRuntime{
			NameFunc: func() string { return "substrate" },
		}).(*AgentManager)
		_, err = subMgr.Provision(context.Background(), api.StartOptions{
			Name:                "sub-agent",
			ProjectPath:         projectPath,
			HubProjectID:        k8sTestProjectUUID,
			BrokerMode:          true,
			NoAuth:              true,
			FreshProvision:      true,
			MoveReincarnationID: "rid-sub-1",
			WorkspaceSource:     api.WorkspaceSourceMoveBundle,
		})
		require.Error(t, err, "move-bundle provision must fail closed on unsupported runtime")
		assert.Contains(t, err.Error(), "not supported")
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
	assert.NoDirExists(t, exportAgentDir, "empty marked export agent dir must be removed in the single final cleanup pass (N3)")
}

func TestProvision_K8sNFSMoveBundle_DistinctHomeShareUnmarkedLeafSurvivesRollback(t *testing.T) {
	_, hostBase, projectPath := setupK8sNFSFixture(t)
	realHostBase, err := filepath.EvalSymlinks(hostBase)
	require.NoError(t, err)
	slug := "k8s-distinct"
	rid := "rid-k8s-distinct-1"

	// Pre-populate an UNMARKED home-<agentID> and a staging sibling on a
	// distinct home subpath_root ("legacy-homes") while workspace uses
	// "projects" (a valid config where homeParent != wsExportAgentDir).
	preExistingHomeParent := filepath.Join(realHostBase, "legacy-homes", k8sTestProjectUUID, "agents", slug)
	preExistingHome := filepath.Join(preExistingHomeParent, "home-"+k8sTestAgentUUID)
	require.NoError(t, os.MkdirAll(preExistingHome, 0o755))
	preciousFile := filepath.Join(preExistingHome, "precious.txt")
	require.NoError(t, os.WriteFile(preciousFile, []byte("do not delete"), 0o600))
	unmarkedStage := filepath.Join(preExistingHomeParent, ".home-"+k8sTestAgentUUID+".bundle-0123456789abcdef0123456789abcdef")
	require.NoError(t, os.MkdirAll(unmarkedStage, 0o755))

	// Also mark the workspace export agent dir with rid so that ONLY the
	// unmarked distinct home leaf triggers the marker guards in CheckMoveMarkers
	// and removeK8sNFSAgentHome.
	wsExportAgentDir := filepath.Join(realHostBase, "projects", k8sTestProjectUUID, "agents", slug)
	require.NoError(t, os.MkdirAll(wsExportAgentDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(wsExportAgentDir, MoveMarkerFile), []byte(rid+"\n"), 0o600))

	mgr := NewManager(&runtime.MockRuntime{
		NameFunc: func() string { return "kubernetes" },
	}).(*AgentManager)

	// 1. Provision must fail with ErrMoveWorkspacePresent when a pre-existing
	//    home-<agentID> or agent dir is present on the export.
	_, err = mgr.Provision(withNFSHomeExperiment(context.Background()), api.StartOptions{
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
	})
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrMoveWorkspacePresent, "provision must fail with ErrMoveWorkspacePresent: %v", err)

	// 2. CheckMoveMarkers must inspect the distinct home leaf (homeParent != wsExportAgentDir)
	//    and return ErrMoveMarkerMismatch naming preExistingHome.
	checkCtx := WithDeleteHomeSubPathRoot(WithDeleteHubAgentID(context.Background(), k8sTestAgentUUID), "legacy-homes")
	checkedPaths, err := mgr.CheckMoveMarkers(checkCtx, projectPath, k8sTestProjectUUID, k8sTestProjectUUID, slug, slug, rid)
	assert.ErrorIs(t, err, ErrMoveMarkerMismatch)
	assert.Contains(t, checkedPaths, preExistingHome, "CheckMoveMarkers must have checked the distinct home leaf")

	// 3. RemoveNFSAgentFiles invoked for rollback MUST enter removeK8sNFSAgentHome,
	//    hit the marker guard, return a TreeAgentHome deletion with ErrMoveMarkerMismatch,
	//    and leave pre-existing unmarked home-<agentID> intact.
	rbCtx := WithDeleteMoveReincarnationID(checkCtx, rid)
	ds, rmErr := mgr.RemoveNFSAgentFiles(rbCtx, projectPath, k8sTestProjectUUID, slug)
	assert.ErrorIs(t, rmErr, ErrMoveMarkerMismatch, "rollback must fail on unmarked home leaf")

	var homeDel *projectfs.Deletion
	for i := range ds {
		if ds[i].Tree == projectfs.TreeAgentHome {
			homeDel = &ds[i]
			break
		}
	}
	require.NotNil(t, homeDel, "RemoveNFSAgentFiles must return a TreeAgentHome deletion record")
	assert.Equal(t, preExistingHome, homeDel.Path)
	assert.Equal(t, projectfs.OutcomeFailed, homeDel.Outcome)
	assert.ErrorIs(t, homeDel.Err, ErrMoveMarkerMismatch)

	data, err := os.ReadFile(preciousFile)
	require.NoError(t, err, "pre-existing unmarked home leaf on distinct share must survive rollback")
	assert.Equal(t, "do not delete", string(data))
	assert.DirExists(t, unmarkedStage, "staging sibling in unmarked home parent must survive because staging purge runs after the marker guard (N1)")
}

func TestRollback_K8sNFSHomeResolutionFailureAuditsOutcomeFailed(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("SCION_CONFIG_DIR", filepath.Join(tmp, ".scion"))

	mountRoot := filepath.Join(tmp, "nfs-mounts")
	wsHostBase := filepath.Join(mountRoot, "ws-share")
	require.NoError(t, os.MkdirAll(wsHostBase, 0o755))

	globalDir := filepath.Join(tmp, ".scion")
	require.NoError(t, os.MkdirAll(globalDir, 0o755))
	// Workspace storage is mounted and healthy on ws-share, while
	// home_storage=nfs + a broken/divergent shared_dir_storage makes
	// homeShareLayout fail during rollback.
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
      subpath_root: projects
      shares:
        - id: ws-share
          server: 10.0.0.10
          export: /ws-export
          pv_name: pv-ws
  shared_dir_storage:
    backend: nfs
    nfs:
      mount_root: ` + mountRoot + `
      subpath_root: homes
      shares:
        - id: home-share
          server: 10.0.0.11
          export: /home-export
          pv_name: pv-home
  home_storage:
    backend: nfs
    leaf: pod
    allow_incomplete_phases: true
`
	require.NoError(t, os.WriteFile(filepath.Join(globalDir, "settings.yaml"), []byte(settingsYAML), 0o644))
	projectPath := filepath.Join(globalDir, "projects", "demo", ".scion")
	require.NoError(t, os.MkdirAll(projectPath, 0o755))
	require.NoError(t, config.InitGlobal(getTestHarnesses()))
	require.NoError(t, os.WriteFile(filepath.Join(globalDir, "settings.yaml"), []byte(settingsYAML), 0o644))

	realWSBase, err := filepath.EvalSymlinks(wsHostBase)
	require.NoError(t, err)
	slug := "k8s-unmounted"
	rid := "rid-k8s-unmounted-1"

	// Create a marked workspace export agent dir + workspace (just like Provision does).
	exportAgentDir := filepath.Join(realWSBase, "projects", k8sTestProjectUUID, "agents", slug)
	exportWS := filepath.Join(exportAgentDir, "workspace")
	require.NoError(t, os.MkdirAll(exportWS, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(exportAgentDir, MoveMarkerFile), []byte(rid+"\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(exportWS, "work.txt"), []byte("data"), 0o644))

	mgr := NewManager(&runtime.MockRuntime{
		NameFunc: func() string { return "kubernetes" },
	}).(*AgentManager)

	// Drive CheckMoveMarkers followed by RemoveNFSAgentFiles, exactly as the
	// broker delete handler sequences rollback deletes (V2).
	checkCtx := WithDeleteHubAgentID(context.Background(), k8sTestAgentUUID)
	_, chkErr := mgr.CheckMoveMarkers(checkCtx, projectPath, k8sTestProjectUUID, k8sTestProjectUUID, slug, slug, rid)
	require.NoError(t, chkErr, "home-share resolution failure must not become ErrMoveMarkerMismatch (409)")

	rbCtx := WithDeleteMoveReincarnationID(checkCtx, rid)
	ds, rmErr := mgr.RemoveNFSAgentFiles(rbCtx, projectPath, k8sTestProjectUUID, slug)
	require.Error(t, rmErr, "rollback must report an error when NFS home share cannot be resolved")

	var wsDel, homeDel *projectfs.Deletion
	for i := range ds {
		switch ds[i].Tree {
		case projectfs.TreeAgentWorkspace:
			if ds[i].Path == exportWS {
				wsDel = &ds[i]
			}
		case projectfs.TreeAgentHome:
			homeDel = &ds[i]
		}
	}
	require.NotNil(t, wsDel, "workspace deletion must be audited")
	assert.Equal(t, projectfs.OutcomeDeleted, wsDel.Outcome, "marked workspace must still be removed during home-share outage")
	assert.NoDirExists(t, exportWS, "marked workspace dir must be removed")

	require.NotNil(t, homeDel, "RemoveNFSAgentFiles must audit an OutcomeFailed TreeAgentHome deletion (C3)")
	assert.Equal(t, projectfs.OutcomeFailed, homeDel.Outcome)
	require.Error(t, homeDel.Err)
}

func TestRollback_K8sNFSHomePrefersRecordedSubPathRoot(t *testing.T) {
	_, hostBase, projectPath := setupK8sNFSFixture(t)
	slug := "k8s-subpath"
	rid := "rid-k8s-subpath-1"

	realHostBase, err := filepath.EvalSymlinks(hostBase)
	require.NoError(t, err)

	// Agent's home-storage.json recorded legacy subpath_root "legacy-homes"
	// before global settings changed to "projects".
	agentDir, err := provisionAgentStateDir(contextWithAgentRuntime(context.Background(), "kubernetes"), api.StartOptions{
		Name:         slug,
		ProjectPath:  projectPath,
		HubProjectID: k8sTestProjectUUID,
		BrokerMode:   true,
	})
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(agentDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(agentDir, MoveMarkerFile), []byte(rid+"\n"), 0o600))
	require.NoError(t, writeHomeStorageRecord(agentDir, homeStorageRecord{
		Backend:     config.HomeStorageBackendNFS,
		Leaf:        config.HomeStorageLeafPod,
		ShareID:     "share-1",
		PVClaimName: "pv-1",
		SubPathRoot: "legacy-homes",
	}))

	// Create the marked home leaf under "legacy-homes" AND an unmarked decoy
	// under the current default "projects".
	legacyHomeParent := filepath.Join(realHostBase, "legacy-homes", k8sTestProjectUUID, "agents", slug)
	legacyHome := filepath.Join(legacyHomeParent, "home-"+k8sTestAgentUUID)
	require.NoError(t, os.MkdirAll(legacyHome, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(legacyHome, MoveMarkerFile), []byte(rid+"\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(legacyHomeParent, MoveMarkerFile), []byte(rid+"\n"), 0o600))

	decoyParent := filepath.Join(realHostBase, "projects", k8sTestProjectUUID, "agents", slug)
	decoyHome := filepath.Join(decoyParent, "home-"+k8sTestAgentUUID)
	require.NoError(t, os.MkdirAll(decoyHome, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(decoyParent, MoveMarkerFile), []byte(rid+"\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(decoyHome, "keep.txt"), []byte("keep"), 0o600))

	// ReadHomeStorageSubPathRoot recovers "legacy-homes" from home-storage.json.
	recordedRoot := ReadHomeStorageSubPathRoot(context.Background(), projectPath, k8sTestProjectUUID, slug)
	assert.Equal(t, "legacy-homes", recordedRoot)

	mgr := NewManager(&runtime.MockRuntime{
		NameFunc: func() string { return "kubernetes" },
	}).(*AgentManager)

	rbCtx := WithDeleteMoveReincarnationID(
		WithDeleteHomeSubPathRoot(
			WithDeleteHubAgentID(context.Background(), k8sTestAgentUUID),
			recordedRoot,
		),
		rid,
	)

	// CheckMoveMarkers succeeds against the marked legacy-homes leaf (not failing on the projects decoy).
	_, err = mgr.CheckMoveMarkers(rbCtx, projectPath, k8sTestProjectUUID, k8sTestProjectUUID, slug, slug, rid)
	require.NoError(t, err)

	// RemoveNFSAgentFiles removes the recorded legacy-homes leaf and its empty parent, leaving the decoy untouched.
	ds, err := mgr.RemoveNFSAgentFiles(rbCtx, projectPath, k8sTestProjectUUID, slug)
	require.NoError(t, err)
	var homeDel *projectfs.Deletion
	for i := range ds {
		if ds[i].Tree == projectfs.TreeAgentHome {
			homeDel = &ds[i]
			break
		}
	}
	require.NotNil(t, homeDel)
	assert.Equal(t, legacyHome, homeDel.Path)
	assert.Equal(t, projectfs.OutcomeDeleted, homeDel.Outcome)
	assert.NoDirExists(t, legacyHome)
	assert.NoDirExists(t, legacyHomeParent)
	assert.FileExists(t, filepath.Join(decoyHome, "keep.txt"), "decoy under current default subpath_root must remain untouched")
}
