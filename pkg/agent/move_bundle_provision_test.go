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
	"os"
	"path/filepath"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestProvision_MoveBundleWorkspaceSourceLeavesWorkspaceEmpty(t *testing.T) {
	fx := newMoveAbsentFixture(t, "docker", false)
	opts := moveAbsentOpts(fx.projectPath, "bundle-target", false)
	opts.MoveReincarnationID = "rid-bundle-1"
	opts.WorkspaceSource = api.WorkspaceSourceMoveBundle
	opts.GitClone = &api.GitCloneConfig{
		URL:    "https://example.com/org/repo.git",
		Branch: "main",
	}

	cfg, err := fx.mgr.Provision(context.Background(), opts)
	require.NoError(t, err)
	require.NotNil(t, cfg)

	agentDir := filepath.Join(fx.projectPath, "agents", "bundle-target")
	wsDir := filepath.Join(agentDir, "workspace")
	homeDir := config.GetAgentHomePath(fx.projectPath, "bundle-target")

	// Move marker was written in agentDir (not inside workspace).
	assertMarker(t, agentDir, "rid-bundle-1")
	assert.NoFileExists(t, filepath.Join(wsDir, MoveMarkerFile))

	// Workspace directory exists and is strictly empty (no clone, no .git, no empty-repo init).
	wsEntries, err := os.ReadDir(wsDir)
	require.NoError(t, err)
	assert.Empty(t, wsEntries, "move-bundle provision must leave workspace directory empty")

	// Home directory was rendered normally.
	homeEntries, err := os.ReadDir(homeDir)
	require.NoError(t, err)
	assert.NotEmpty(t, homeEntries, "move-bundle provision must render home normally")
}

func TestProvision_MoveBundleRefusesSharedLayout(t *testing.T) {
	sl := newSharedLayoutStarter(t, "docker")
	mgr := NewManager(&runtime.MockRuntime{
		NameFunc: func() string { return "docker" },
	}).(*AgentManager)
	opts := api.StartOptions{
		Name:                "test-agent",
		ProjectPath:         sl.root,
		HubProjectID:        sharedRootUUID,
		BrokerMode:          true,
		NoAuth:              true,
		FreshProvision:      true,
		MoveReincarnationID: "rid-shared-target",
		WorkspaceSource:     api.WorkspaceSourceMoveBundle,
		Env: map[string]string{
			"SCION_PROJECT_ID": sharedRootUUID,
		},
	}
	_, err := mgr.Provision(context.Background(), opts)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "shared project storage layout")
}

func TestMoveBundleHomeRecord_WriteReadAndExpand(t *testing.T) {
	dir := t.TempDir()

	// Missing record fails closed.
	_, err := ReadMoveBundleHomeRecord(dir)
	assert.ErrorIs(t, err, ErrMoveBundleHomeRecordMissing)

	// Empty UnixUsername cannot build a record.
	_, err = BuildMoveBundleHomeRecord(runtime.RunConfig{})
	assert.ErrorIs(t, err, ErrMoveBundleHomeRecordInvalid)

	// Valid record persists resolved UnixUsername, HomeAbs, WorkspaceAbs, file secrets, and auth files.
	cfg := runtime.RunConfig{
		UnixUsername:       "scion",
		ContainerWorkspace: "/workspace",
		ResolvedSecrets: []api.ResolvedSecret{
			{Name: "k1", Type: "file", Target: "~/.config/app/key.pem"},
			{Name: "k2", Type: "file", Target: "/home/scion/.ssh/id_ed25519"},
			{Name: "k3", Type: "file", Target: "/etc/outside.pem"},
			{Name: "k4", Type: "file", Target: "/workspace/.env"},
		},
		ResolvedAuth: &api.ResolvedAuth{
			Files: []api.FileMapping{
				{SourcePath: "/host/a", ContainerPath: "~/.claude/.credentials.json"},
				{SourcePath: "/host/b", ContainerPath: "/workspace/config/auth.json"},
			},
		},
	}
	require.NoError(t, WriteMoveBundleHomeRecord(dir, cfg))

	rec, err := ReadMoveBundleHomeRecord(dir)
	require.NoError(t, err)
	assert.Equal(t, "scion", rec.UnixUsername)
	assert.Equal(t, "/home/scion", rec.HomeAbs)
	assert.Equal(t, "/workspace", rec.WorkspaceAbs)
	assert.Contains(t, rec.HomeExcludes, ".config/app/key.pem")
	assert.Contains(t, rec.HomeExcludes, ".ssh/id_ed25519")
	assert.Contains(t, rec.HomeExcludes, ".claude/.credentials.json")
	assert.NotContains(t, rec.HomeExcludes, "etc/outside.pem")
	assert.Contains(t, rec.WorkspaceExcludes, ".env")
	assert.Contains(t, rec.WorkspaceExcludes, "config/auth.json")
	assert.NotContains(t, rec.WorkspaceExcludes, "etc/outside.pem")

	// ExpandHomeRelTarget works directly against homeAbs (including /root) and rejects /home.
	rel, ok := ExpandHomeRelTarget("~/.gemini/antigravity-cli/token", "/home/scion")
	assert.True(t, ok)
	assert.Equal(t, ".gemini/antigravity-cli/token", rel)
	rel, ok = ExpandHomeRelTarget("~/.copilot/config.json", "/root")
	assert.True(t, ok)
	assert.Equal(t, ".copilot/config.json", rel)
	_, ok = ExpandHomeRelTarget("~/.kube/config", "/home")
	assert.False(t, ok, "/home is not a user home")

	// ExpandAbsRelTarget resolves workspace-relative targets and rejects paths outside rootAbs.
	rel, ok = ExpandAbsRelTarget("/workspace/.env", "/workspace")
	assert.True(t, ok)
	assert.Equal(t, ".env", rel)
	rel, ok = ExpandAbsRelTarget("/src/sub/sa.json", "/src")
	assert.True(t, ok)
	assert.Equal(t, "sub/sa.json", rel)
	_, ok = ExpandAbsRelTarget("/workspace2/.env", "/workspace")
	assert.False(t, ok)
	_, ok = ExpandAbsRelTarget("~/.env", "/workspace")
	assert.False(t, ok)
}

func TestMoveBundleHomeRecord_FailedWriteRemovesStaleRecord(t *testing.T) {
	dir := t.TempDir()

	// Seed a valid record from an earlier start.
	require.NoError(t, WriteMoveBundleHomeRecord(dir, runtime.RunConfig{
		UnixUsername: "scion",
		ResolvedSecrets: []api.ResolvedSecret{
			{Name: "old", Type: "file", Target: "~/.config/old/secret.key"},
		},
	}))
	_, err := ReadMoveBundleHomeRecord(dir)
	require.NoError(t, err)

	// A subsequent start whose RunConfig cannot produce a valid record must fail
	// AND remove the stale record so export fails closed instead of using stale excludes.
	err = WriteMoveBundleHomeRecord(dir, runtime.RunConfig{})
	assert.ErrorIs(t, err, ErrMoveBundleHomeRecordInvalid)

	_, err = ReadMoveBundleHomeRecord(dir)
	assert.ErrorIs(t, err, ErrMoveBundleHomeRecordMissing, "stale record must be removed when a later write fails")
}

func TestStart_WritesMoveBundleHomeRecord(t *testing.T) {
	fx := newMoveAbsentFixture(t, "docker", false)
	opts := moveAbsentOpts(fx.projectPath, "start-rec-agent", true)
	opts.ResolvedSecrets = []api.ResolvedSecret{
		{Name: "my_file_secret", Type: "file", Target: "~/.config/app/secret.key", Value: "top-secret-value"},
		{Name: "my_ws_secret", Type: "file", Target: "/workspace/.env", Value: "API_KEY=secret"},
	}

	_, err := fx.mgr.Start(context.Background(), opts)
	require.NoError(t, err)

	agentDir := filepath.Join(fx.projectPath, "agents", "start-rec-agent")
	rec, err := ReadMoveBundleHomeRecord(agentDir)
	require.NoError(t, err, "AgentManager.Start must persist the move-bundle home record")
	assert.NotEmpty(t, rec.UnixUsername)
	assert.NotEmpty(t, rec.HomeAbs)
	assert.Equal(t, "/workspace", rec.WorkspaceAbs)
	assert.Contains(t, rec.HomeExcludes, ".config/app/secret.key")
	assert.Contains(t, rec.WorkspaceExcludes, ".env")
}

func TestResolveMoveBundleAgentDirs_MatchesProvisionAgentStateDir(t *testing.T) {
	fx := newMoveAbsentFixture(t, "docker", false)
	opts := api.StartOptions{
		Name:        "parity-agent",
		ProjectPath: fx.projectPath,
	}
	wantAgentDir, err := provisionAgentStateDir(context.Background(), opts)
	require.NoError(t, err)
	wantProjectDir, err := config.GetResolvedProjectDir(fx.projectPath)
	require.NoError(t, err)

	got, err := ResolveMoveBundleAgentDirs(context.Background(), "docker", opts)
	require.NoError(t, err)
	assert.Equal(t, wantAgentDir, got.AgentDir)
	assert.Equal(t, filepath.Join(wantAgentDir, "workspace"), got.WorkspaceDir)
	assert.Equal(t, config.GetAgentHomePath(wantProjectDir, "parity-agent"), got.HomeDir)
	assert.False(t, got.SharedWorkspace)
}

func TestResolveMoveBundleAgentDirs_SharedLayoutAndSplitStorage(t *testing.T) {
	// Shared project storage layout (kubernetes + NFS share):
	// agent state lives in the broker-local external agent dir (split storage)
	// and SharedWorkspace is reported true so Phase 1 precheck/export refuse it.
	sl := newSharedLayoutStarter(t, "kubernetes")
	opts := api.StartOptions{
		Name:         "test-agent",
		ProjectPath:  sl.root,
		HubProjectID: sharedRootUUID,
		BrokerMode:   true,
	}
	got, err := ResolveMoveBundleAgentDirs(context.Background(), "kubernetes", opts)
	require.NoError(t, err)
	assert.True(t, got.SharedWorkspace, "shared project storage layout must be flagged SharedWorkspace=true")
	assert.Equal(t, sl.ext, got.AgentDir, "split-storage external agent dir must match provisionAgentStateDir")
	assert.Equal(t, filepath.Join(sl.ext, "home"), got.HomeDir)

	// Local-disk runtime on the same NFS shared project layout:
	// agent state resolves to the broker-local external agent dir (split storage),
	// matching provisionAgentStateDir, and SharedWorkspace is still true because
	// the project workspace lives on the shared layout.
	gotLocal, err := ResolveMoveBundleAgentDirs(context.Background(), "docker", opts)
	require.NoError(t, err)
	wantLocal, err := provisionAgentStateDir(contextWithAgentRuntime(context.Background(), "docker"), opts)
	require.NoError(t, err)
	assert.Equal(t, wantLocal, gotLocal.AgentDir)
	assert.Equal(t, sl.ext, gotLocal.AgentDir)
	assert.Equal(t, filepath.Join(sl.ext, "workspace"), gotLocal.WorkspaceDir)
	assert.Equal(t, filepath.Join(sl.ext, "home"), gotLocal.HomeDir)
	assert.True(t, gotLocal.SharedWorkspace)

	// Non-shared split-storage project (local project dir outside any NFS share
	// with HubProjectID + BrokerMode): resolves to the same agent dir as
	// provisionAgentStateDir with SharedWorkspace=false.
	fx := newMoveAbsentFixture(t, "docker", false)
	splitOpts := api.StartOptions{
		Name:         "split-agent",
		ProjectPath:  fx.projectPath,
		HubProjectID: "11111111-2222-3333-4444-555555555555",
		BrokerMode:   true,
	}
	wantSplit, err := provisionAgentStateDir(contextWithAgentRuntime(context.Background(), "docker"), splitOpts)
	require.NoError(t, err)
	gotSplit, err := ResolveMoveBundleAgentDirs(context.Background(), "docker", splitOpts)
	require.NoError(t, err)
	assert.Equal(t, wantSplit, gotSplit.AgentDir)
	assert.Equal(t, filepath.Join(wantSplit, "workspace"), gotSplit.WorkspaceDir)
	assert.False(t, gotSplit.SharedWorkspace)
}
