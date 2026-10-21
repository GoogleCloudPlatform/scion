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
	"os"
	"path/filepath"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/provision"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testMoveID = "11111111-2222-4333-8444-555555555555"

func assertMarker(t *testing.T, dir, want string) {
	t.Helper()
	got, err := readMoveMarker(dir)
	require.NoError(t, err, "move marker in %s", dir)
	assert.Equal(t, want, got)
}

// A forced move's provision marks the agent directory it creates with the
// move's reincarnation ID, before anything else is written there; the
// marker sits beside the home and workspace, never inside the workspace.
func TestProvision_MoveMarkerWrittenFirst(t *testing.T) {
	for _, empty := range []bool{false, true} {
		fx := newMoveAbsentFixture(t, "docker", false)
		opts := moveAbsentOpts(fx.projectPath, "test-agent", empty)
		opts.MoveReincarnationID = testMoveID
		_, err := fx.mgr.Provision(context.Background(), opts)
		require.NoError(t, err)

		agentDir := filepath.Join(fx.projectPath, "agents", "test-agent")
		assertMarker(t, agentDir, testMoveID)
		assert.NoFileExists(t, filepath.Join(agentDir, "workspace", MoveMarkerFile), "never inside the workspace")
		marker, err := os.Stat(filepath.Join(agentDir, MoveMarkerFile))
		require.NoError(t, err)
		entries, err := os.ReadDir(agentDir)
		require.NoError(t, err)
		require.Greater(t, len(entries), 1, "the provision wrote more than the marker")
		for _, e := range entries {
			info, err := e.Info()
			require.NoError(t, err)
			assert.False(t, info.ModTime().Before(marker.ModTime()), "%s was written before the move marker", e.Name())
		}
	}
}

// A forced move's provision never adopts an agent directory it did not
// create: it fails and writes no marker there.
func TestProvision_MoveMarkerRefusesExistingDir(t *testing.T) {
	fx := newMoveAbsentFixture(t, "docker", false)
	agentDir := filepath.Join(fx.projectPath, "agents", "test-agent")
	require.NoError(t, os.MkdirAll(agentDir, 0o755))
	opts := moveAbsentOpts(fx.projectPath, "test-agent", false)
	opts.MoveReincarnationID = testMoveID
	_, err := fx.mgr.Provision(context.Background(), opts)
	require.True(t, errors.Is(err, ErrMoveWorkspacePresent), "want ErrMoveWorkspacePresent, got %v", err)
	assert.NoFileExists(t, filepath.Join(agentDir, MoveMarkerFile))
	assert.DirExists(t, agentDir, "the existing directory is left in place")
}

// Without a reincarnation ID a provision writes no move marker.
func TestProvision_NoMoveMarkerWithoutMove(t *testing.T) {
	fx := newMoveAbsentFixture(t, "docker", false)
	_, err := fx.mgr.Provision(context.Background(), moveAbsentOpts(fx.projectPath, "test-agent", false))
	require.NoError(t, err)
	assert.NoFileExists(t, filepath.Join(fx.projectPath, "agents", "test-agent", MoveMarkerFile))
}

// The guarded delete's check passes for absent directories and for
// directories marked by this move, and refuses unmarked ones and markers
// that are not regular files.
func TestCheckMoveMarkers_LocalDisk(t *testing.T) {
	setup := func(t *testing.T) (moveAbsentFixture, string) {
		fx := newMoveAbsentFixture(t, "docker", false)
		return fx, filepath.Join(fx.projectPath, "agents", "test-agent")
	}
	check := func(fx moveAbsentFixture) error {
		_, err := fx.mgr.CheckMoveMarkers(context.Background(), fx.projectPath, testNFSWorkspaceProjectID, "test-agent", testMoveID)
		return err
	}
	t.Run("absent directory", func(t *testing.T) {
		fx, _ := setup(t)
		require.NoError(t, check(fx))
	})
	t.Run("marked by this move", func(t *testing.T) {
		fx, dir := setup(t)
		require.NoError(t, os.MkdirAll(dir, 0o755))
		require.NoError(t, writeMoveMarker(dir, testMoveID))
		require.NoError(t, check(fx))
	})
	t.Run("unmarked", func(t *testing.T) {
		fx, dir := setup(t)
		require.NoError(t, os.MkdirAll(dir, 0o755))
		err := check(fx)
		require.True(t, errors.Is(err, ErrMoveMarkerMismatch), "want ErrMoveMarkerMismatch, got %v", err)
	})
	t.Run("marker is a symlink", func(t *testing.T) {
		fx, dir := setup(t)
		require.NoError(t, os.MkdirAll(dir, 0o755))
		real := filepath.Join(t.TempDir(), "marker")
		require.NoError(t, os.WriteFile(real, []byte(testMoveID+"\n"), 0o600))
		require.NoError(t, os.Symlink(real, filepath.Join(dir, MoveMarkerFile)))
		err := check(fx)
		require.True(t, errors.Is(err, ErrMoveMarkerMismatch), "want ErrMoveMarkerMismatch, got %v", err)
	})
	t.Run("no reincarnation ID", func(t *testing.T) {
		fx, _ := setup(t)
		_, err := fx.mgr.CheckMoveMarkers(context.Background(), fx.projectPath, testNFSWorkspaceProjectID, "test-agent", "")
		require.True(t, errors.Is(err, ErrMoveMarkerMismatch), "want ErrMoveMarkerMismatch, got %v", err)
	})
}

// A directory marked by another move is refused: the files are not this
// move's.
func TestCheckMoveMarkers_OtherMoveRefused(t *testing.T) {
	fx := newMoveAbsentFixture(t, "docker", false)
	dir := filepath.Join(fx.projectPath, "agents", "test-agent")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	require.NoError(t, writeMoveMarker(dir, "99999999-2222-4333-8444-555555555555"))
	_, err := fx.mgr.CheckMoveMarkers(context.Background(), fx.projectPath, testNFSWorkspaceProjectID, "test-agent", testMoveID)
	require.True(t, errors.Is(err, ErrMoveMarkerMismatch), "want ErrMoveMarkerMismatch, got %v", err)
}

// On Kubernetes with NFS workspace storage the agent's own directory on
// the export is checked too, all or nothing.
func TestCheckMoveMarkers_KubernetesNFS(t *testing.T) {
	setup := func(t *testing.T) (moveAbsentFixture, string, string) {
		fx := newMoveAbsentFixture(t, "kubernetes", true)
		local := filepath.Join(fx.projectPath, "agents", "test-agent")
		require.NoError(t, os.MkdirAll(local, 0o755))
		require.NoError(t, writeMoveMarker(local, testMoveID))
		export := nfsTestAgentDir(fx.mountRoot, "test-agent")
		require.NoError(t, os.MkdirAll(export, 0o755))
		return fx, local, export
	}
	t.Run("both marked", func(t *testing.T) {
		fx, _, export := setup(t)
		require.NoError(t, writeMoveMarker(export, testMoveID))
		paths, err := fx.mgr.CheckMoveMarkers(context.Background(), fx.projectPath, testNFSWorkspaceProjectID, "test-agent", testMoveID)
		require.NoError(t, err)
		assert.Contains(t, paths, export)
	})
	t.Run("export unmarked", func(t *testing.T) {
		fx, _, _ := setup(t)
		_, err := fx.mgr.CheckMoveMarkers(context.Background(), fx.projectPath, testNFSWorkspaceProjectID, "test-agent", testMoveID)
		require.True(t, errors.Is(err, ErrMoveMarkerMismatch), "want ErrMoveMarkerMismatch, got %v", err)
	})
}

// A forced move's start marks the agent's own directory on the export when
// it creates it, with the local agent directory's marker; a directory that
// already existed is left unmarked.
func TestStart_MarksMoveExportAgentDir(t *testing.T) {
	run := func(t *testing.T, preexisting, moved bool) (moveAbsentFixture, string) {
		fx := newMoveAbsentFixture(t, "kubernetes", true)
		export := nfsTestAgentDir(fx.mountRoot, "test-agent")
		if preexisting {
			require.NoError(t, os.MkdirAll(export, 0o770))
		}
		opts := moveAbsentOpts(fx.projectPath, "test-agent", false)
		if moved {
			opts.MoveReincarnationID = testMoveID
		}
		_, err := fx.mgr.Provision(context.Background(), opts)
		require.NoError(t, err)
		startOpts := opts
		startOpts.FreshProvision = false
		startOpts.MoveReincarnationID = ""
		startOpts.Env = moveAbsentOpts(fx.projectPath, "test-agent", false).Env
		_, err = fx.mgr.Start(context.Background(), startOpts)
		require.NoError(t, err)
		require.DirExists(t, export)
		return fx, export
	}
	t.Run("created by the move's start", func(t *testing.T) {
		_, export := run(t, false, true)
		assertMarker(t, export, testMoveID)
		assert.NoFileExists(t, filepath.Join(export, "workspace", MoveMarkerFile), "never inside the workspace")
	})
	t.Run("already existed", func(t *testing.T) {
		_, export := run(t, true, true)
		assert.NoFileExists(t, filepath.Join(export, MoveMarkerFile))
	})
	t.Run("no move: unchanged", func(t *testing.T) {
		fx, export := run(t, false, false)
		assert.NoFileExists(t, filepath.Join(export, MoveMarkerFile))
		assert.NoFileExists(t, filepath.Join(fx.projectPath, "agents", "test-agent", MoveMarkerFile))
	})
}

// A failed marker write fails the forced move's provision, and the
// provision removes only the agent directory it claimed: a sibling agent's
// directory and the project are untouched.
func TestProvision_MoveMarkerWriteFailureRemovesOnlyClaimedDir(t *testing.T) {
	fx := newMoveAbsentFixture(t, "docker", false)
	sibling := filepath.Join(fx.projectPath, "agents", "other-agent")
	require.NoError(t, os.MkdirAll(sibling, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(sibling, "keep.txt"), []byte("keep"), 0o644))
	opts := moveAbsentOpts(fx.projectPath, "test-agent", false)
	opts.MoveReincarnationID = "bad\nid" // writeMoveMarker refuses it
	_, err := fx.mgr.Provision(context.Background(), opts)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "move marker")
	assert.NoDirExists(t, filepath.Join(fx.projectPath, "agents", "test-agent"), "the claimed directory is removed")
	assert.FileExists(t, filepath.Join(sibling, "keep.txt"), "a sibling agent's directory is untouched")
}

// The guarded delete also refuses when a worktree the same delete would
// remove exists, though every agent directory is marked by this move: a
// forced move never creates a worktree. Nothing is changed.
func TestCheckMoveMarkers_RefusesWorktreesTheMoveNeverCreates(t *testing.T) {
	t.Run("NFS worktree on the export", func(t *testing.T) {
		fx := newMoveAbsentFixture(t, "kubernetes", true)
		local := filepath.Join(fx.projectPath, "agents", "test-agent")
		require.NoError(t, os.MkdirAll(local, 0o755))
		require.NoError(t, writeMoveMarker(local, testMoveID))
		export := nfsTestAgentDir(fx.mountRoot, "test-agent")
		require.NoError(t, os.MkdirAll(export, 0o755))
		require.NoError(t, writeMoveMarker(export, testMoveID))
		wt := filepath.Join(fx.mountRoot, "share-1", nfsWorktreeSubPath(filepath.Join("projects", testNFSWorkspaceProjectID, "workspace"), "test-agent"))
		require.NoError(t, os.MkdirAll(wt, 0o755))
		paths, err := fx.mgr.CheckMoveMarkers(context.Background(), fx.projectPath, testNFSWorkspaceProjectID, "test-agent", testMoveID)
		require.True(t, errors.Is(err, ErrMoveMarkerMismatch), "want ErrMoveMarkerMismatch, got %v", err)
		assert.Contains(t, err.Error(), "a worktree exists")
		assert.Contains(t, paths, wt)
		assert.DirExists(t, wt)
	})
	t.Run("worktree under the project's shared base", func(t *testing.T) {
		fx := newMoveAbsentFixture(t, "docker", false)
		local := filepath.Join(fx.projectPath, "agents", "test-agent")
		require.NoError(t, os.MkdirAll(local, 0o755))
		require.NoError(t, writeMoveMarker(local, testMoveID))
		base := filepath.Join(filepath.Dir(fx.projectPath), "workspace")
		require.NoError(t, os.MkdirAll(filepath.Join(base, ".git"), 0o755))
		wt := filepath.Join(base, "worktrees", "test-agent")
		require.NoError(t, os.MkdirAll(wt, 0o755))
		_, err := fx.mgr.CheckMoveMarkers(context.Background(), fx.projectPath, testNFSWorkspaceProjectID, "test-agent", testMoveID)
		require.True(t, errors.Is(err, ErrMoveMarkerMismatch), "want ErrMoveMarkerMismatch, got %v", err)
		assert.Contains(t, err.Error(), "a worktree exists")
		assert.DirExists(t, wt)
	})
	t.Run("sharer registration", func(t *testing.T) {
		root := t.TempDir()
		t.Chdir(root)
		t.Setenv("HOME", root)
		proj := filepath.Join(root, "proj")
		mustMkdir(t, proj)
		setupGitRepo(t, proj)
		scion := filepath.Join(proj, config.DotScion)
		local := filepath.Join(scion, "agents", "test-agent")
		mustMkdir(t, local)
		require.NoError(t, writeMoveMarker(local, testMoveID))
		require.NoError(t, provision.RegisterSharer(proj, scion, "b1", "", "test-agent"))
		mgr := NewManager(&runtime.MockRuntime{NameFunc: func() string { return "docker" }}).(*AgentManager)
		_, err := mgr.CheckMoveMarkers(context.Background(), scion, testNFSWorkspaceProjectID, "test-agent", testMoveID)
		require.True(t, errors.Is(err, ErrMoveMarkerMismatch), "want ErrMoveMarkerMismatch, got %v", err)
		assert.Contains(t, err.Error(), "registered on a shared worktree")
		assert.DirExists(t, local)
	})
}

// In a split-storage layout (a project-id file) the agent's home lives
// under the external agents root's <name>, a second directory the forced
// move creates and its rollback delete removes: it is marked too, so the
// guarded delete's check passes for the move's own files.
func TestProvision_MoveMarkerOnSplitStorageHomeParent(t *testing.T) {
	agentName := "split-move"
	scionDir, agentDir, agentHome := setupSplitStorageFailedProvision(t, agentName)
	ctx := withMoveReincarnationID(api.ContextWithGitClone(context.Background(), &api.GitCloneConfig{URL: "https://example.com/o/r.git"}), testMoveID)
	_, _, _, err := ProvisionAgent(ctx, agentName, "default", "", "", scionDir, "", "", "", "")
	require.NoError(t, err)
	assertMarker(t, agentDir, testMoveID)
	assertMarker(t, filepath.Dir(agentHome), testMoveID)

	mgr := NewManager(&runtime.MockRuntime{NameFunc: func() string { return "docker" }}).(*AgentManager)
	paths, err := mgr.CheckMoveMarkers(context.Background(), scionDir, "", agentName, testMoveID)
	require.NoError(t, err, "the move's own files pass the guarded delete's check")
	assert.Contains(t, paths, filepath.Dir(agentHome))
}

// The leaf helper still reports that it created the agent's directory on
// the export when a later directory then fails, so the start can mark it.
func TestEnsureNFSAgentWorkspaceLeafClaim_ReportsCreatedOnLaterError(t *testing.T) {
	mountRoot := filepath.Join(t.TempDir(), "nfs")
	require.NoError(t, os.MkdirAll(filepath.Join(mountRoot, "share-1"), 0o755))
	res := resolveTestNFSWorkspace(t, mountRoot, "scratchpad")
	blocker := filepath.Join(mountRoot, "share-1", "projects", testNFSWorkspaceProjectID, "shared-dirs", "scratchpad")
	require.NoError(t, os.MkdirAll(filepath.Dir(blocker), 0o755))
	require.NoError(t, os.WriteFile(blocker, []byte("not a directory"), 0o644))

	_, created, err := ensureNFSAgentWorkspaceLeafClaim("kubernetes", testNFSWorkspaceProjectID, res, "ws-pv", []string{"scratchpad"}, "test-agent")
	require.Error(t, err)
	assert.True(t, created, "the agent's directory was created before the later failure")
	assert.DirExists(t, nfsTestAgentDir(mountRoot, "test-agent"))
}

// A forced move's start marks the export directory it created even when a
// later directory then fails the start.
func TestStart_MarksMoveExportAgentDirBeforeLaterFailure(t *testing.T) {
	fx := newMoveAbsentFixture(t, "kubernetes", true)
	opts := moveAbsentOpts(fx.projectPath, "test-agent", false)
	opts.MoveReincarnationID = testMoveID
	_, err := fx.mgr.Provision(context.Background(), opts)
	require.NoError(t, err)
	blocker := filepath.Join(fx.mountRoot, "share-1", "projects", testNFSWorkspaceProjectID, "shared-dirs", "scratchpad")
	require.NoError(t, os.MkdirAll(filepath.Dir(blocker), 0o755))
	require.NoError(t, os.WriteFile(blocker, []byte("not a directory"), 0o644))

	startOpts := opts
	startOpts.FreshProvision = false
	startOpts.MoveReincarnationID = ""
	startOpts.Env = moveAbsentOpts(fx.projectPath, "test-agent", false).Env
	startOpts.SharedDirs = []api.SharedDir{{Name: "scratchpad"}}
	_, err = fx.mgr.Start(context.Background(), startOpts)
	require.Error(t, err)
	export := nfsTestAgentDir(fx.mountRoot, "test-agent")
	require.DirExists(t, export)
	assertMarker(t, export, testMoveID)
}

// When the project directory cannot be resolved, the guarded delete's check
// refuses (fail closed) without naming a path, and changes nothing.
func TestCheckMoveMarkers_ProjectDirUnresolvedRefuses(t *testing.T) {
	fx := newMoveAbsentFixture(t, "docker", false)
	dir := filepath.Join(fx.projectPath, "agents", "test-agent")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	require.NoError(t, writeMoveMarker(dir, testMoveID))
	prev := moveMarkerProjectDir
	t.Cleanup(func() { moveMarkerProjectDir = prev })
	moveMarkerProjectDir = func(string) (string, error) { return "", errors.New("injected resolver failure") }

	_, err := fx.mgr.CheckMoveMarkers(context.Background(), fx.projectPath, testNFSWorkspaceProjectID, "test-agent", testMoveID)
	require.True(t, errors.Is(err, ErrMoveMarkerMismatch), "want ErrMoveMarkerMismatch, got %v", err)
	assert.Contains(t, err.Error(), "resolve the project directory")
	assert.NotContains(t, err.Error(), fx.projectPath, "the refusal names no path")
	assertMarker(t, dir, testMoveID)
}

// In a split-storage layout, a failed home claim after the move claimed (and
// marked) the home's parent removes that parent and its marker with the rest
// of what the provision created; a parent that existed before is untouched.
func TestProvision_HomeClaimFailureRemovesClaimedHomeParent(t *testing.T) {
	failHomeClaim := func(t *testing.T) {
		prev := claimAgentHome
		t.Cleanup(func() { claimAgentHome = prev })
		claimAgentHome = func(string) (bool, error) { return false, errors.New("injected home claim failure") }
	}
	t.Run("parent claimed by the move", func(t *testing.T) {
		agentName := "split-claim-fail"
		scionDir, agentDir, agentHome := setupSplitStorageFailedProvision(t, agentName)
		failHomeClaim(t)
		ctx := withMoveReincarnationID(api.ContextWithGitClone(context.Background(), &api.GitCloneConfig{URL: "https://example.com/o/r.git"}), testMoveID)
		_, _, _, err := ProvisionAgent(ctx, agentName, "default", "", "", scionDir, "", "", "", "")
		require.Error(t, err)
		assert.NoFileExists(t, filepath.Join(filepath.Dir(agentHome), MoveMarkerFile), "the parent's marker is removed")
		assert.NoDirExists(t, filepath.Dir(agentHome), "the parent this provision claimed is removed")
		assert.NoDirExists(t, agentDir)
	})
	t.Run("pre-existing parent", func(t *testing.T) {
		agentName := "split-claim-keep"
		scionDir, _, agentHome := setupSplitStorageFailedProvision(t, agentName)
		parent := filepath.Dir(agentHome)
		require.NoError(t, os.MkdirAll(parent, 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(parent, "keep.txt"), []byte("keep"), 0o644))
		failHomeClaim(t)
		ctx := withMoveReincarnationID(api.ContextWithGitClone(context.Background(), &api.GitCloneConfig{URL: "https://example.com/o/r.git"}), testMoveID)
		_, _, _, err := ProvisionAgent(ctx, agentName, "default", "", "", scionDir, "", "", "", "")
		require.Error(t, err)
		assert.FileExists(t, filepath.Join(parent, "keep.txt"), "a parent that existed before is untouched")
		assert.NoFileExists(t, filepath.Join(parent, MoveMarkerFile), "a parent the move did not create is never marked")
	})
}
