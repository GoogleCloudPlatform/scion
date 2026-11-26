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
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/projectfs"
	"github.com/GoogleCloudPlatform/scion/pkg/provision"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/homeprep"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	crTestProjectUUID = "22222222-3333-4444-5555-666666666666"
	crTestAgentUUID   = "bbbbbbbb-cccc-dddd-eeee-ffffffffffff"
)

func setupCloudRunNFSFixture(t *testing.T, uid, gid int) (mountRoot, hostBase, projectPath string) {
	t.Helper()
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("SCION_CONFIG_DIR", filepath.Join(tmp, ".scion"))

	mountRoot = filepath.Join(tmp, "nfs-mounts")
	hostBase = filepath.Join(mountRoot, "cr-share")
	require.NoError(t, os.MkdirAll(hostBase, 0o755))

	globalDir := filepath.Join(tmp, ".scion")
	require.NoError(t, os.MkdirAll(globalDir, 0o755))

	uidLine, gidLine := "", ""
	if uid > 0 {
		uidLine = "\n      uid: " + itoa(uid)
	}
	if gid > 0 {
		gidLine = "\n      gid: " + itoa(gid)
	}
	// Note: intentionally no project_storage.homes and no pv_name, proving
	// Cloud Run does not depend on K8s NFS-home settings or PV claims.
	settingsYAML := `schema_version: "1"
active_profile: cr
runtimes:
  cloudrun:
    type: cloudrun
profiles:
  cr:
    runtime: cloudrun
server:
  project_storage:
    backend: nfs
    nfs:
      mount_root: ` + mountRoot + `
      subpath_root: projects` + uidLine + gidLine + `
      shares:
        - id: cr-share
          server: 10.1.0.10
          export: /filestore
`
	require.NoError(t, os.WriteFile(filepath.Join(globalDir, "settings.yaml"), []byte(settingsYAML), 0o644))

	projectPath = filepath.Join(globalDir, "projects", "demo", ".scion")
	require.NoError(t, os.MkdirAll(projectPath, 0o755))
	require.NoError(t, config.InitGlobal(getTestHarnesses()))
	require.NoError(t, os.WriteFile(filepath.Join(globalDir, "settings.yaml"), []byte(settingsYAML), 0o644))
	return mountRoot, hostBase, projectPath
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var digits []byte
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	return string(digits)
}

func TestStart_CloudRunNFSWritesMoveBundleHomeRecord(t *testing.T) {
	_, _, projectPath := setupCloudRunNFSFixture(t, 2000, 3000)

	var captured runtime.RunConfig
	rt := &runtime.MockRuntime{
		NameFunc: func() string { return "cloudrun" },
		RunFunc: func(_ context.Context, cfg runtime.RunConfig) (string, error) {
			captured = cfg
			return "cr-inst-1", nil
		},
	}
	opts := api.StartOptions{
		Name:         "cr-rec-agent",
		ProjectPath:  projectPath,
		HubProjectID: crTestProjectUUID,
		Profile:      "cr",
		BrokerMode:   true,
		NoAuth:       true,
		Env: map[string]string{
			"SCION_PROJECT_ID":     crTestProjectUUID,
			"SCION_AGENT_ID":       crTestAgentUUID,
			"SCION_WORKSPACE_MODE": "clone-per-agent",
		},
		ResolvedSecrets: []api.ResolvedSecret{
			{Name: "api_key", Type: "file", Target: "~/.config/app/secret.key", Value: "top-secret"},
			{Name: "ws_env", Type: "file", Target: "/workspace/.env", Value: "SECRET=1"},
		},
	}

	_, err := NewManager(rt).Start(context.Background(), opts)
	require.NoError(t, err)
	assert.Equal(t, "nfs", captured.WorkspaceBackendName)

	agentDir, err := provisionAgentStateDir(contextWithAgentRuntime(context.Background(), "cloudrun"), opts)
	require.NoError(t, err)
	rec, err := ReadMoveBundleHomeRecord(agentDir)
	require.NoError(t, err, "Cloud Run NFS Start must persist .scion-move-bundle-home.json")
	assert.NotEmpty(t, rec.UnixUsername)
	assert.Equal(t, "/home/"+rec.UnixUsername, rec.HomeAbs)
	assert.Equal(t, "/workspace", rec.WorkspaceAbs)
	assert.Contains(t, rec.HomeExcludes, ".config/app/secret.key")
	assert.Contains(t, rec.WorkspaceExcludes, ".env")
}

func TestMoveBundle_CloudRunDurabilityAndResolveDirs(t *testing.T) {
	t.Run("Cloud Run NFS durable source resolves workspace, home leaf, and configured uid/gid", func(t *testing.T) {
		_, hostBase, projectPath := setupCloudRunNFSFixture(t, 2000, 3000)
		slug := "cr-worker"
		opts := api.StartOptions{
			Name:         slug,
			ProjectPath:  projectPath,
			HubProjectID: crTestProjectUUID,
			BrokerMode:   true,
		}
		agentDir, err := provisionAgentStateDir(contextWithAgentRuntime(context.Background(), "cloudrun"), opts)
		require.NoError(t, err)
		require.NoError(t, os.MkdirAll(agentDir, 0o755))
		recordCloudRunAgentWorkspace(agentDir, slug)

		resolvedBase, err := filepath.EvalSymlinks(hostBase)
		require.NoError(t, err)
		wsDir := filepath.Join(resolvedBase, "projects", crTestProjectUUID, "agents", slug, "workspace")
		homeDir := filepath.Join(resolvedBase, "projects", crTestProjectUUID, "agents", slug, "home-"+crTestAgentUUID)
		require.NoError(t, os.MkdirAll(wsDir, 0o755))
		require.NoError(t, os.MkdirAll(homeDir, 0o755))

		for _, rtName := range []string{"cloudrun", "cloudrun-instances"} {
			t.Run(rtName, func(t *testing.T) {
				in := MoveBundleRuntimeInput{
					RuntimeName: rtName,
					AgentSlug:   slug,
					AgentID:     crTestAgentUUID,
					ProjectPath: projectPath,
					ProjectID:   crTestProjectUUID,
					BrokerMode:  true,
				}
				dur, err := CheckMoveBundleSourceDurability(context.Background(), in)
				require.NoError(t, err)
				assert.True(t, dur.Durable, "Cloud Run NFS source must be durable (reason=%q)", dur.Reason)

				dirs, err := ResolveMoveBundleDirsForRuntime(context.Background(), in)
				require.NoError(t, err)
				assert.Equal(t, agentDir, dirs.AgentDir)
				assert.Equal(t, wsDir, dirs.WorkspaceDir)
				assert.Equal(t, homeDir, dirs.HomeDir)
				assert.Equal(t, filepath.Dir(wsDir), dirs.ExportAgentDir)
				// Cloud Run runs as nfs.uid:nfs.gid (2000:3000), NOT K8s's 1000:1000.
				assert.Equal(t, 2000, dirs.WorkspaceOwner.UID)
				assert.Equal(t, 3000, dirs.WorkspaceOwner.GID)
				assert.Equal(t, 2000, dirs.HomeOwner.UID)
				assert.Equal(t, 3000, dirs.HomeOwner.GID)
			})
		}
	})

	t.Run("Cloud Run without NFS workspace storage is refused", func(t *testing.T) {
		tmp := t.TempDir()
		t.Setenv("HOME", tmp)
		t.Setenv("SCION_CONFIG_DIR", filepath.Join(tmp, ".scion"))
		projectPath := filepath.Join(tmp, "proj", ".scion")
		require.NoError(t, os.MkdirAll(projectPath, 0o755))

		in := MoveBundleRuntimeInput{
			RuntimeName: "cloudrun",
			AgentSlug:   "cr-plain",
			AgentID:     crTestAgentUUID,
			ProjectPath: projectPath,
			ProjectID:   crTestProjectUUID,
			BrokerMode:  true,
		}
		dur, err := CheckMoveBundleSourceDurability(context.Background(), in)
		require.NoError(t, err)
		assert.False(t, dur.Durable)
		assert.Equal(t, cloudRunNonDurableReason, dur.Reason)
	})

	t.Run("Cloud Run with unmounted NFS share is refused", func(t *testing.T) {
		_, hostBase, projectPath := setupCloudRunNFSFixture(t, 0, 0)
		require.NoError(t, os.RemoveAll(hostBase))

		in := MoveBundleRuntimeInput{
			RuntimeName: "cloudrun",
			AgentSlug:   "cr-unmounted",
			AgentID:     crTestAgentUUID,
			ProjectPath: projectPath,
			ProjectID:   crTestProjectUUID,
			BrokerMode:  true,
		}
		dur, err := CheckMoveBundleSourceDurability(context.Background(), in)
		require.NoError(t, err)
		assert.False(t, dur.Durable)
		assert.Equal(t, k8sNFSNotMountedReason, dur.Reason)
	})

	t.Run("Cloud Run with missing agentId is refused", func(t *testing.T) {
		_, _, projectPath := setupCloudRunNFSFixture(t, 0, 0)
		in := MoveBundleRuntimeInput{
			RuntimeName: "cloudrun",
			AgentSlug:   "cr-noid",
			AgentID:     "",
			ProjectPath: projectPath,
			ProjectID:   crTestProjectUUID,
			BrokerMode:  true,
		}
		dur, err := CheckMoveBundleSourceDurability(context.Background(), in)
		require.NoError(t, err)
		assert.False(t, dur.Durable)
		assert.Equal(t, cloudRunMissingIdentityReason, dur.Reason)
	})

	t.Run("Cloud Run legacy shared-checkout agent is refused", func(t *testing.T) {
		_, _, projectPath := setupCloudRunNFSFixture(t, 0, 0)
		slug := "cr-legacy"
		agentDir, err := provisionAgentStateDir(contextWithAgentRuntime(context.Background(), "cloudrun"), api.StartOptions{
			Name:         slug,
			ProjectPath:  projectPath,
			HubProjectID: crTestProjectUUID,
			BrokerMode:   true,
		})
		require.NoError(t, err)
		require.NoError(t, os.MkdirAll(agentDir, 0o755))
		// No cloudrun-agent-workspace.json and no agents/<slug>/workspace on export.

		in := MoveBundleRuntimeInput{
			RuntimeName: "cloudrun",
			AgentSlug:   slug,
			AgentID:     crTestAgentUUID,
			ProjectPath: projectPath,
			ProjectID:   crTestProjectUUID,
			BrokerMode:  true,
		}
		dur, err := CheckMoveBundleSourceDurability(context.Background(), in)
		require.NoError(t, err)
		assert.False(t, dur.Durable)
		assert.Equal(t, cloudRunSharedCheckoutReason, dur.Reason)
	})

	t.Run("Cloud Run Sandbox is refused fail-closed", func(t *testing.T) {
		_, _, projectPath := setupCloudRunNFSFixture(t, 0, 0)
		assert.False(t, MoveBundleRuntimeSupported("cloudrun-sandbox"))
		assert.False(t, MoveBundleRuntimeConfigured("cloudrun-sandbox"))

		in := MoveBundleRuntimeInput{
			RuntimeName: "cloudrun-sandbox",
			AgentSlug:   "sbx-agent",
			AgentID:     crTestAgentUUID,
			ProjectPath: projectPath,
			ProjectID:   crTestProjectUUID,
			BrokerMode:  true,
		}
		dur, err := CheckMoveBundleSourceDurability(context.Background(), in)
		require.NoError(t, err)
		assert.False(t, dur.Durable)
		assert.Equal(t, cloudRunSandboxNonDurableReason, dur.Reason)

		_, err = ResolveMoveBundleDirsForRuntime(context.Background(), in)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "not supported")

		sbxMgr := NewManager(&runtime.MockRuntime{
			NameFunc: func() string { return "cloudrun-sandbox" },
		}).(*AgentManager)
		_, err = sbxMgr.Provision(context.Background(), api.StartOptions{
			Name:                "sbx-agent",
			ProjectPath:         projectPath,
			HubProjectID:        crTestProjectUUID,
			BrokerMode:          true,
			NoAuth:              true,
			FreshProvision:      true,
			MoveReincarnationID: "rid-sbx-1",
			WorkspaceSource:     api.WorkspaceSourceMoveBundle,
		})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "not supported")
	})
}

func TestProvision_CloudRunNFSMoveBundleTargetAndRollback(t *testing.T) {
	_, hostBase, projectPath := setupCloudRunNFSFixture(t, 2000, 3000)

	origPurge := purgeRemovedAgentWorkspaces
	purgeRemovedAgentWorkspaces = func(agentDir, projectID, agentName string, _ *slog.Logger) {
		_ = provision.PurgeRemovedAgentWorkspaces(agentDir)
	}
	t.Cleanup(func() { purgeRemovedAgentWorkspaces = origPurge })

	type chownRec struct {
		path string
		uid  int
		gid  int
	}
	var chowns []chownRec
	origLchown := moveBundleTargetLchown
	moveBundleTargetLchown = func(name string, uid, gid int) error {
		chowns = append(chowns, chownRec{path: name, uid: uid, gid: gid})
		return nil
	}
	t.Cleanup(func() { moveBundleTargetLchown = origLchown })

	rt := &runtime.MockRuntime{NameFunc: func() string { return "cloudrun" }}
	mgr := NewManager(rt).(*AgentManager)

	slug := "cr-target"
	rid := "rid-cr-prov-1"
	opts := api.StartOptions{
		Name:                slug,
		ProjectPath:         projectPath,
		HubProjectID:        crTestProjectUUID,
		Profile:             "cr",
		BrokerMode:          true,
		NoAuth:              true,
		FreshProvision:      true,
		MoveReincarnationID: rid,
		WorkspaceSource:     api.WorkspaceSourceMoveBundle,
		Env: map[string]string{
			"SCION_PROJECT_ID":     crTestProjectUUID,
			"SCION_AGENT_ID":       crTestAgentUUID,
			"SCION_WORKSPACE_MODE": "clone-per-agent",
		},
	}

	_, err := mgr.Provision(context.Background(), opts)
	require.NoError(t, err)

	resolvedBase, err := filepath.EvalSymlinks(hostBase)
	require.NoError(t, err)
	exportAgentDir := filepath.Join(resolvedBase, "projects", crTestProjectUUID, "agents", slug)
	wsDir := filepath.Join(exportAgentDir, "workspace")
	homeDir := filepath.Join(exportAgentDir, "home-"+crTestAgentUUID)

	assert.DirExists(t, exportAgentDir)
	assert.DirExists(t, wsDir)
	assert.DirExists(t, homeDir)

	// Workspace must be empty.
	wsEntries, err := os.ReadDir(wsDir)
	require.NoError(t, err)
	assert.Empty(t, wsEntries)

	// Move markers written on both exportAgentDir and homeDir.
	gotMarker, err := ReadMoveMarker(exportAgentDir)
	require.NoError(t, err)
	assert.Equal(t, rid, gotMarker)
	gotHomeMarker, err := ReadMoveMarker(homeDir)
	require.NoError(t, err)
	assert.Equal(t, rid, gotHomeMarker)

	// Cloud Run does NOT write K8s's .scion-home-seed.json sentinel.
	assert.NoFileExists(t, filepath.Join(homeDir, homeprep.SentinelName))

	// Cloud Run workspace and home leaf were chowned to configured 2000:3000.
	assert.Contains(t, chowns, chownRec{path: wsDir, uid: 2000, gid: 3000})
	assert.Contains(t, chowns, chownRec{path: homeDir, uid: 2000, gid: 3000})

	// CheckMoveMarkers succeeds for this moveID.
	ctx := WithDeleteHubAgentID(context.Background(), crTestAgentUUID)
	_, err = mgr.CheckMoveMarkers(ctx, projectPath, crTestProjectUUID, crTestProjectUUID, slug, "", rid)
	require.NoError(t, err)

	// Normal delete (without moveReincarnationID) preserves home-<agentID> (Hub owns RemoveAgentStorage).
	_, err = mgr.RemoveNFSAgentFiles(ctx, projectPath, crTestProjectUUID, slug)
	require.NoError(t, err)
	assert.NoDirExists(t, wsDir)
	assert.DirExists(t, homeDir, "normal delete must not remove Cloud Run NFS home leaf")

	// Recreate workspace + staging sibling, then run rollback delete with moveReincarnationID.
	require.NoError(t, os.MkdirAll(wsDir, 0o755))
	stageDir := filepath.Join(exportAgentDir, ".workspace.bundle-0123456789abcdef0123456789abcdef")
	require.NoError(t, os.MkdirAll(stageDir, 0o755))

	rbCtx := WithDeleteMoveReincarnationID(ctx, rid)
	ds, err := mgr.RemoveNFSAgentFiles(rbCtx, projectPath, crTestProjectUUID, slug)
	require.NoError(t, err)
	var sawHomeDeleted bool
	for _, d := range ds {
		if d.Tree == projectfs.TreeAgentHome && d.Outcome == projectfs.OutcomeDeleted {
			sawHomeDeleted = true
		}
	}
	assert.True(t, sawHomeDeleted, "rollback delete must audit TreeAgentHome OutcomeDeleted")
	assert.NoDirExists(t, wsDir)
	assert.NoDirExists(t, homeDir)
	assert.NoDirExists(t, stageDir)
	assert.NoDirExists(t, exportAgentDir, "rollback delete must remove empty marked export agent dir")
}

func TestProvision_CloudRunNFSMoveBundle_UnmarkedPreExistingLeafSurvivesRollback(t *testing.T) {
	_, hostBase, projectPath := setupCloudRunNFSFixture(t, 2000, 3000)

	rt := &runtime.MockRuntime{NameFunc: func() string { return "cloudrun" }}
	mgr := NewManager(rt).(*AgentManager)

	slug := "cr-preexist"
	rid := "rid-cr-guard-1"
	resolvedBase, err := filepath.EvalSymlinks(hostBase)
	require.NoError(t, err)

	exportAgentDir := filepath.Join(resolvedBase, "projects", crTestProjectUUID, "agents", slug)
	preExistingHome := filepath.Join(exportAgentDir, "home-"+crTestAgentUUID)
	require.NoError(t, os.MkdirAll(preExistingHome, 0o755))
	precious := filepath.Join(preExistingHome, "precious.txt")
	require.NoError(t, os.WriteFile(precious, []byte("keep-me"), 0o644))
	stagingSibling := filepath.Join(exportAgentDir, ".home-"+crTestAgentUUID+".bundle-0123456789abcdef0123456789abcdef")
	require.NoError(t, os.MkdirAll(stagingSibling, 0o755))

	opts := api.StartOptions{
		Name:                slug,
		ProjectPath:         projectPath,
		HubProjectID:        crTestProjectUUID,
		Profile:             "cr",
		BrokerMode:          true,
		NoAuth:              true,
		FreshProvision:      true,
		MoveReincarnationID: rid,
		WorkspaceSource:     api.WorkspaceSourceMoveBundle,
		Env: map[string]string{
			"SCION_PROJECT_ID":     crTestProjectUUID,
			"SCION_AGENT_ID":       crTestAgentUUID,
			"SCION_WORKSPACE_MODE": "clone-per-agent",
		},
	}

	// 1. CheckMoveWorkspaceAbsent and Provision must refuse over pre-existing exportAgentDir.
	_, err = mgr.CheckMoveWorkspaceAbsent(context.Background(), opts)
	require.ErrorIs(t, err, ErrMoveWorkspacePresent)

	_, err = mgr.Provision(context.Background(), opts)
	require.ErrorIs(t, err, ErrMoveWorkspacePresent)

	// 2. CheckMoveMarkers must fail closed with ErrMoveMarkerMismatch.
	ctx := WithDeleteMoveReincarnationID(WithDeleteHubAgentID(context.Background(), crTestAgentUUID), rid)
	_, err = mgr.CheckMoveMarkers(ctx, projectPath, crTestProjectUUID, crTestProjectUUID, slug, "", rid)
	require.ErrorIs(t, err, ErrMoveMarkerMismatch)

	// 3. RemoveNFSAgentFiles invoked for rollback MUST refuse to remove the unmarked
	//    home leaf or staging sibling, auditing TreeAgentHome OutcomeFailed with
	//    ErrMoveMarkerMismatch.
	ds, err := mgr.RemoveNFSAgentFiles(ctx, projectPath, crTestProjectUUID, slug)
	require.ErrorIs(t, err, ErrMoveMarkerMismatch)
	var homeDel *projectfs.Deletion
	for i := range ds {
		if ds[i].Tree == projectfs.TreeAgentHome {
			homeDel = &ds[i]
			break
		}
	}
	require.NotNil(t, homeDel, "expected TreeAgentHome deletion audit entry")
	assert.Equal(t, projectfs.OutcomeFailed, homeDel.Outcome)
	require.ErrorIs(t, homeDel.Err, ErrMoveMarkerMismatch)

	got, readErr := os.ReadFile(precious)
	require.NoError(t, readErr, "unmarked pre-existing Cloud Run home leaf must survive rollback")
	assert.Equal(t, "keep-me", string(got))
	assert.DirExists(t, stagingSibling, "staging sibling in unmarked dir must survive rollback")
}

// TestCheckMoveMarkers_CloudRunUnmarkedWorkspaceRefused pins both the Cloud Run
// export-dir branch in CheckMoveMarkers (R1) and the pre-removal marker check
// in removeNFSAgentWorkspace (S1). The fixture intentionally has NO home-<agentID>
// leaf so the home-leaf check cannot mask the workspace export-dir guard.
func TestCheckMoveMarkers_CloudRunUnmarkedWorkspaceRefused(t *testing.T) {
	_, hostBase, projectPath := setupCloudRunNFSFixture(t, 2000, 3000)

	rt := &runtime.MockRuntime{NameFunc: func() string { return "cloudrun" }}
	mgr := NewManager(rt).(*AgentManager)

	slug := "cr-unmarked-ws"
	rid := "rid-cr-ws-guard-1"
	resolvedBase, err := filepath.EvalSymlinks(hostBase)
	require.NoError(t, err)

	exportAgentDir := filepath.Join(resolvedBase, "projects", crTestProjectUUID, "agents", slug)
	wsDir := filepath.Join(exportAgentDir, "workspace")
	require.NoError(t, os.MkdirAll(wsDir, 0o755))
	precious := filepath.Join(wsDir, "precious.txt")
	require.NoError(t, os.WriteFile(precious, []byte("do-not-delete"), 0o644))

	ctx := WithDeleteMoveReincarnationID(WithDeleteHubAgentID(context.Background(), crTestAgentUUID), rid)

	// 1. CheckMoveMarkers must fail closed with ErrMoveMarkerMismatch on the
	//    unmarked Cloud Run export agent dir (even with no home leaf present).
	_, err = mgr.CheckMoveMarkers(ctx, projectPath, crTestProjectUUID, crTestProjectUUID, slug, "", rid)
	require.ErrorIs(t, err, ErrMoveMarkerMismatch)

	// 2. S1 defense-in-depth: RemoveNFSAgentFiles invoked directly for rollback
	//    must refuse BEFORE moving workspace aside (ErrMoveMarkerMismatch),
	//    leaving precious.txt intact at its original path.
	ds, err := mgr.RemoveNFSAgentFiles(ctx, projectPath, crTestProjectUUID, slug)
	require.ErrorIs(t, err, ErrMoveMarkerMismatch)
	var wsDel *projectfs.Deletion
	for i := range ds {
		if ds[i].Tree == projectfs.TreeAgentWorkspace {
			wsDel = &ds[i]
			break
		}
	}
	require.NotNil(t, wsDel, "expected TreeAgentWorkspace deletion audit entry")
	assert.Equal(t, projectfs.OutcomeFailed, wsDel.Outcome)
	require.ErrorIs(t, wsDel.Err, ErrMoveMarkerMismatch)

	got, readErr := os.ReadFile(precious)
	require.NoError(t, readErr, "unmarked workspace file must survive rollback untouched")
	assert.Equal(t, "do-not-delete", string(got))
}

// TestCloudRunRecreateMove_StartFailureRollsBackAndAllowsRetry verifies R2:
// on a Cloud Run recreate move (no WorkspaceSource=move-bundle), Start claims
// and marks agents/<slug> before Runtime.Run creates workspace/ and
// home-<agentID>. If Start then fails definitively, CheckMoveMarkers passes,
// rollback removes both the NFS dirs and broker-local agentDir, and a retry
// to the same broker succeeds.
func TestCloudRunRecreateMove_StartFailureRollsBackAndAllowsRetry(t *testing.T) {
	_, hostBase, projectPath := setupCloudRunNFSFixture(t, 2000, 3000)

	resolvedBase, err := filepath.EvalSymlinks(hostBase)
	require.NoError(t, err)

	slug := "cr-recreate"
	rid := "rid-cr-recreate-1"
	exportAgentDir := filepath.Join(resolvedBase, "projects", crTestProjectUUID, "agents", slug)
	wsDir := filepath.Join(exportAgentDir, "workspace")
	homeDir := filepath.Join(exportAgentDir, "home-"+crTestAgentUUID)

	rt := &runtime.MockRuntime{
		NameFunc: func() string { return "cloudrun" },
		RunFunc: func(_ context.Context, _ runtime.RunConfig) (string, error) {
			// Simulate CloudRunRuntime.Run creating workspace/ and home-<agentID>
			// before failing definitively (e.g. errCloudRunHomeTooLarge or
			// instance-create failure).
			if err := os.MkdirAll(wsDir, 0o770); err != nil {
				return "", err
			}
			if err := os.MkdirAll(homeDir, 0o770); err != nil {
				return "", err
			}
			return "", errors.New("cloudrun: simulated definitive start failure")
		},
	}
	mgr := NewManager(rt).(*AgentManager)

	provOpts := api.StartOptions{
		Name:                slug,
		ProjectPath:         projectPath,
		HubProjectID:        crTestProjectUUID,
		Profile:             "cr",
		BrokerMode:          true,
		NoAuth:              true,
		FreshProvision:      true,
		MoveReincarnationID: rid,
		GitClone:            &api.GitCloneConfig{URL: "https://example.com/repo.git"},
		Env: map[string]string{
			"SCION_PROJECT_ID":     crTestProjectUUID,
			"SCION_AGENT_ID":       crTestAgentUUID,
			"SCION_WORKSPACE_MODE": "clone-per-agent",
		},
	}

	// 1. Pre-provision absence check passes and Provision writes the broker-local move marker.
	_, err = mgr.CheckMoveWorkspaceAbsent(context.Background(), provOpts)
	require.NoError(t, err)
	_, err = mgr.Provision(context.Background(), provOpts)
	require.NoError(t, err)

	agentDir, err := provisionAgentStateDir(contextWithAgentRuntime(context.Background(), "cloudrun"), provOpts)
	require.NoError(t, err)
	gotLocalMarker, err := ReadMoveMarker(agentDir)
	require.NoError(t, err)
	assert.Equal(t, rid, gotLocalMarker)

	// 2. Start claims & marks agents/<slug>, then Runtime.Run creates workspace/
	//    and home-<agentID> and fails definitively.
	startOpts := provOpts
	startOpts.FreshProvision = false
	startOpts.MoveReincarnationID = ""
	_, err = mgr.Start(context.Background(), startOpts)
	require.Error(t, err)
	assert.DirExists(t, wsDir)
	assert.DirExists(t, homeDir)
	gotExportMarker, err := ReadMoveMarker(exportAgentDir)
	require.NoError(t, err, "Start must mark agents/<slug> before Runtime.Run")
	assert.Equal(t, rid, gotExportMarker)

	// 3. Rollback: CheckMoveMarkers succeeds, RemoveNFSAgentFiles removes
	//    workspace/, home-<agentID>, and empty agents/<slug>, and
	//    DeleteAgentFiles removes broker-local agentDir.
	rbCtx := WithDeleteMoveReincarnationID(WithDeleteHubAgentID(context.Background(), crTestAgentUUID), rid)
	_, err = mgr.CheckMoveMarkers(rbCtx, projectPath, crTestProjectUUID, crTestProjectUUID, slug, "", rid)
	require.NoError(t, err, "recreate rollback CheckMoveMarkers must succeed")

	_, err = mgr.RemoveNFSAgentFiles(rbCtx, projectPath, crTestProjectUUID, slug)
	require.NoError(t, err)
	assert.NoDirExists(t, wsDir)
	assert.NoDirExists(t, homeDir)
	assert.NoDirExists(t, exportAgentDir)

	_, err = DeleteHubAgentFiles(slug, "", projectPath, crTestProjectUUID, false)
	require.NoError(t, err)
	assert.NoDirExists(t, agentDir)

	// 4. Retry with a new move ID succeeds cleanly.
	retryOpts := provOpts
	retryOpts.MoveReincarnationID = "rid-cr-recreate-2"
	_, err = mgr.CheckMoveWorkspaceAbsent(context.Background(), retryOpts)
	require.NoError(t, err, "retry CheckMoveWorkspaceAbsent must succeed after rollback")
	_, err = mgr.Provision(context.Background(), retryOpts)
	require.NoError(t, err, "retry Provision must succeed after rollback")
}
