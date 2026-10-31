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

//go:build !no_sqlite

package hub

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/agent"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/projectkeys"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
	"github.com/GoogleCloudPlatform/scion/pkg/runtimebroker"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// identityForms runs fn for both on-disk forms of a workspace identity: a
// .scion directory holding a project-id file, and a .scion marker file.
func identityForms(t *testing.T, fn func(t *testing.T, markerForm bool)) {
	t.Helper()
	for _, tc := range []struct {
		name       string
		markerForm bool
	}{
		{"project-id file", false},
		{"marker file", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			restore := config.OverrideIsGitRepo(func() bool { return !tc.markerForm })
			t.Cleanup(restore)
			fn(t, tc.markerForm)
		})
	}
}

// seedWorkspaceIdentity creates the workspace for slug under HOME with the
// given identity, and a populated project config directory for it. It
// returns the workspace path and the project config directory.
func seedWorkspaceIdentity(t *testing.T, slug, id string, markerForm bool) (string, string) {
	t.Helper()
	workspacePath, err := localProjectPath(slug)
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(workspacePath, 0755))
	scionPath := filepath.Join(workspacePath, config.DotScion)
	if markerForm {
		require.NoError(t, config.WriteProjectMarker(scionPath, &config.ProjectMarker{
			ProjectID: id, ProjectName: slug, ProjectSlug: slug,
		}))
	} else {
		require.NoError(t, os.MkdirAll(scionPath, 0755))
		require.NoError(t, config.WriteProjectID(scionPath, id))
	}

	root, err := projectConfigRoot(slug, id)
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(filepath.Join(root, config.DotScion, "agents", "agent-a", "home"), 0755))
	require.NoError(t, os.WriteFile(filepath.Join(root, config.DotScion, "settings.yaml"), []byte("schema_version: \"1\"\n"), 0644))
	require.NoError(t, os.WriteFile(filepath.Join(root, config.DotScion, "agents", "agent-a", "home", "notes.txt"), []byte("agent-a"), 0644))
	require.NoError(t, os.MkdirAll(filepath.Join(root, config.SharedDirsSubdir, "data"), 0755))
	require.NoError(t, os.WriteFile(filepath.Join(root, config.SharedDirsSubdir, "data", "file.txt"), []byte("shared"), 0644))
	return workspacePath, root
}

// workspaceConfigRoot resolves the project config directory of a workspace
// the way local project resolution does, from its .scion entry.
func workspaceConfigRoot(t *testing.T, workspacePath string) string {
	t.Helper()
	scionPath := filepath.Join(workspacePath, config.DotScion)
	var ext string
	var err error
	if config.IsProjectMarkerFile(scionPath) {
		ext, err = config.ResolveProjectMarker(scionPath)
	} else {
		ext, err = config.GetGitProjectExternalConfigDir(scionPath)
	}
	require.NoError(t, err)
	require.NotEmpty(t, ext)
	return filepath.Dir(ext)
}

func notInUse() (bool, error) { return false, nil }

func isFormerID(string, string) (recordedRelation, error) { return recordedSameProject, nil }

// projectConfigRoot returns ~/.scion/project-configs/<slug>__<id8>.
func projectConfigRoot(slug, projectID string) (string, error) {
	ext, err := config.ProjectMarker{ProjectID: projectID, ProjectSlug: slug}.ExternalProjectPath()
	if err != nil {
		return "", err
	}
	return filepath.Dir(ext), nil
}

// snapshotTree records every path under root with its size and, for files,
// content.
func snapshotTree(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	require.NoError(t, filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			out[path] = "dir"
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		out[path] = string(data)
		return nil
	}))
	return out
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	return string(data)
}

func TestCloneSharedWorkspaceProject_HubProjectIDIsWorkspaceIdentity(t *testing.T) {
	identityForms(t, func(t *testing.T, markerForm bool) {
		identityTestHome(t)
		srv, _ := testServer(t)

		sourceDir := t.TempDir()
		for _, args := range [][]string{
			{"init"},
			{"config", "user.email", "test@test.com"},
			{"config", "user.name", "Test"},
			{"commit", "--allow-empty", "-m", "init"},
		} {
			cmd := exec.Command("git", args...)
			cmd.Dir = sourceDir
			require.NoError(t, cmd.Run(), "git %v", args)
		}

		allowLocalCloneFixture(t, sourceDir)

		project := sharedWorkspaceProject("clone-identity")
		project.Labels[store.LabelCloneURL] = sourceDir
		project.Labels[store.LabelDefaultBranch] = "master"

		require.NoError(t, srv.cloneSharedWorkspaceProject(context.Background(), project))

		workspacePath, err := localProjectPath(project.Slug)
		require.NoError(t, err)
		ident, err := config.ReadWorkspaceIdentity(workspacePath)
		require.NoError(t, err)
		require.NotNil(t, ident)
		assert.Equal(t, project.ID, ident.ID)
		assert.Equal(t, markerForm, ident.Marker != nil)

		want, err := projectConfigRoot(project.Slug, project.ID)
		require.NoError(t, err)
		assert.Equal(t, want, workspaceConfigRoot(t, workspacePath))
		assert.DirExists(t, want)

		entries, err := os.ReadDir(filepath.Dir(want))
		require.NoError(t, err)
		assert.Len(t, entries, 1, "only the project config directory named after the hub project ID is created")
	})
}

func TestCloneSharedWorkspaceProject_RepositoryIdentityReplacedByHubProjectID(t *testing.T) {
	identityTestHome(t)
	restore := config.OverrideIsGitRepo(func() bool { return true })
	t.Cleanup(restore)
	srv, _ := testServer(t)

	repoID := api.NewUUID()
	sourceDir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(sourceDir, config.DotScion), 0755))
	require.NoError(t, config.WriteProjectID(filepath.Join(sourceDir, config.DotScion), repoID))
	for _, args := range [][]string{
		{"init"},
		{"config", "user.email", "test@test.com"},
		{"config", "user.name", "Test"},
		{"add", "."},
		{"commit", "-m", "init"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = sourceDir
		require.NoError(t, cmd.Run(), "git %v", args)
	}

	allowLocalCloneFixture(t, sourceDir)

	project := sharedWorkspaceProject("clone-repo-identity")
	project.Labels[store.LabelCloneURL] = sourceDir
	project.Labels[store.LabelDefaultBranch] = "master"

	require.NoError(t, srv.cloneSharedWorkspaceProject(context.Background(), project))

	workspacePath, err := localProjectPath(project.Slug)
	require.NoError(t, err)
	want, err := projectConfigRoot(project.Slug, project.ID)
	require.NoError(t, err)
	assert.Equal(t, want, workspaceConfigRoot(t, workspacePath))
	assert.DirExists(t, filepath.Join(want, config.DotScion))
}

func TestAlignClonedProjectIdentities_ExistingCloneUsesRecordDir(t *testing.T) {
	identityForms(t, func(t *testing.T, markerForm bool) {
		identityTestHome(t)
		srv, st := testServer(t)
		ctx := context.Background()

		project := sharedWorkspaceProject("existing-clone")
		require.NoError(t, st.CreateProject(ctx, project))
		localID := api.NewUUID()
		workspacePath, previous := seedWorkspaceIdentity(t, project.Slug, localID, markerForm)

		counts := srv.alignClonedProjectIdentities(ctx)
		assert.Equal(t, 1, counts.aligned)

		want, err := projectConfigRoot(project.Slug, project.ID)
		require.NoError(t, err)
		assert.Equal(t, want, workspaceConfigRoot(t, workspacePath))

		ident, err := config.ReadWorkspaceIdentity(workspacePath)
		require.NoError(t, err)
		assert.Equal(t, project.ID, ident.ID)
		assert.Equal(t, markerForm, ident.Marker != nil, "the identity keeps its on-disk form")

		assert.NoDirExists(t, previous)
		assert.Equal(t, "schema_version: \"1\"\n", readFile(t, filepath.Join(want, config.DotScion, "settings.yaml")))
		assert.Equal(t, "agent-a", readFile(t, filepath.Join(want, config.DotScion, "agents", "agent-a", "home", "notes.txt")))
		assert.Equal(t, "shared", readFile(t, filepath.Join(want, config.SharedDirsSubdir, "data", "file.txt")))
	})
}

func TestAlignClonedProjectIdentities_SecondRunIsNoOp(t *testing.T) {
	identityForms(t, func(t *testing.T, markerForm bool) {
		identityTestHome(t)
		srv, st := testServer(t)
		ctx := context.Background()

		project := sharedWorkspaceProject("repeat-clone")
		require.NoError(t, st.CreateProject(ctx, project))
		workspacePath, _ := seedWorkspaceIdentity(t, project.Slug, api.NewUUID(), markerForm)

		srv.alignClonedProjectIdentities(ctx)

		scionPath := filepath.Join(workspacePath, config.DotScion)
		identityFile := scionPath
		if !markerForm {
			identityFile = filepath.Join(scionPath, projectkeys.ProjectIDFile)
		}
		before, err := os.Stat(identityFile)
		require.NoError(t, err)
		configsDir := filepath.Dir(workspaceConfigRoot(t, workspacePath))
		entriesBefore, err := os.ReadDir(configsDir)
		require.NoError(t, err)

		counts := srv.alignClonedProjectIdentities(ctx)
		assert.Equal(t, 1, counts.alreadyMatching)
		assert.Zero(t, counts.aligned)
		res, err := alignWorkspaceProjectIdentity(workspacePath, project.Slug, project.ID, isFormerID, notInUse)
		require.NoError(t, err)
		assert.False(t, res.Changed())
		assert.Equal(t, alignAlreadyMatching, res.Outcome)

		after, err := os.Stat(identityFile)
		require.NoError(t, err)
		assert.Equal(t, before.ModTime(), after.ModTime(), "identity is not rewritten")
		entriesAfter, err := os.ReadDir(configsDir)
		require.NoError(t, err)
		assert.Equal(t, len(entriesBefore), len(entriesAfter))
	})
}

func TestAlignClonedProjectIdentities_OtherProjectsUnchanged(t *testing.T) {
	identityTestHome(t)
	srv, st := testServer(t)
	ctx := context.Background()

	// A hub-cloned project whose identity already is its hub project ID.
	matching := sharedWorkspaceProject("matching-clone")
	require.NoError(t, st.CreateProject(ctx, matching))
	matchingWS, matchingRoot := seedWorkspaceIdentity(t, matching.Slug, matching.ID, false)

	// A per-agent worktree git project and a hub-native project hold their
	// own identities; they are not hub-cloned shared workspaces.
	worktree := &store.Project{
		ID: api.NewUUID(), Name: "worktree-project", Slug: "worktree-project",
		GitRemote: "github.com/example/worktree-project",
		Labels:    map[string]string{store.LabelWorkspaceMode: store.WorkspaceModeWorktreePerAgent},
	}
	require.NoError(t, st.CreateProject(ctx, worktree))
	worktreeID := api.NewUUID()
	worktreeWS, worktreeRoot := seedWorkspaceIdentity(t, worktree.Slug, worktreeID, false)

	native := &store.Project{ID: api.NewUUID(), Name: "native-project", Slug: "native-project"}
	require.NoError(t, st.CreateProject(ctx, native))
	nativeID := api.NewUUID()
	nativeWS, nativeRoot := seedWorkspaceIdentity(t, native.Slug, nativeID, true)

	srv.alignClonedProjectIdentities(ctx)

	for _, tc := range []struct {
		name, ws, root, id string
	}{
		{"matching", matchingWS, matchingRoot, matching.ID},
		{"worktree", worktreeWS, worktreeRoot, worktreeID},
		{"native", nativeWS, nativeRoot, nativeID},
	} {
		ident, err := config.ReadWorkspaceIdentity(tc.ws)
		require.NoError(t, err, tc.name)
		assert.Equal(t, tc.id, ident.ID, tc.name)
		assert.Equal(t, tc.root, workspaceConfigRoot(t, tc.ws), tc.name)
		assert.Equal(t, "shared", readFile(t, filepath.Join(tc.root, config.SharedDirsSubdir, "data", "file.txt")), tc.name)
	}
}

func TestAlignClonedProjectIdentities_WaitsForAgentsToStop(t *testing.T) {
	identityTestHome(t)
	srv, st := testServer(t)
	ctx := context.Background()

	project := sharedWorkspaceProject("busy-clone")
	require.NoError(t, st.CreateProject(ctx, project))
	localID := api.NewUUID()
	workspacePath, previous := seedWorkspaceIdentity(t, project.Slug, localID, false)

	agent := &store.Agent{
		ID: api.NewUUID(), Name: "busy-agent", Slug: "busy-agent",
		ProjectID: project.ID, Phase: "created",
	}
	require.NoError(t, st.CreateAgent(ctx, agent))

	for _, phase := range []string{"created", "running", "suspended"} {
		agent.Phase = phase
		require.NoError(t, st.UpdateAgent(ctx, agent))

		counts := srv.alignClonedProjectIdentities(ctx)
		assert.Equal(t, 1, counts.skippedInUse, phase)

		ident, err := config.ReadWorkspaceIdentity(workspacePath)
		require.NoError(t, err)
		assert.Equal(t, localID, ident.ID, "identity is kept while an agent is %s", phase)
		assert.DirExists(t, previous)
	}

	agent.Phase = "stopped"
	require.NoError(t, st.UpdateAgent(ctx, agent))

	counts := srv.alignClonedProjectIdentities(ctx)
	assert.Equal(t, 1, counts.aligned)

	want, err := projectConfigRoot(project.Slug, project.ID)
	require.NoError(t, err)
	assert.Equal(t, want, workspaceConfigRoot(t, workspacePath))
	assert.NoDirExists(t, previous)
}

func TestAlignWorkspaceProjectIdentity_PopulatedRecordDirSkipsProject(t *testing.T) {
	identityTestHome(t)
	srv, st := testServer(t)
	ctx := context.Background()

	project := sharedWorkspaceProject("populated-target")
	require.NoError(t, st.CreateProject(ctx, project))
	localID := api.NewUUID()
	workspacePath, previous := seedWorkspaceIdentity(t, project.Slug, localID, false)

	want, err := projectConfigRoot(project.Slug, project.ID)
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(filepath.Join(want, config.DotScion), 0755))
	require.NoError(t, os.WriteFile(filepath.Join(want, config.DotScion, "settings.yaml"), []byte("kept\n"), 0644))

	res, err := alignWorkspaceProjectIdentity(workspacePath, project.Slug, project.ID, isFormerID, notInUse)
	require.NoError(t, err)
	assert.Equal(t, alignSkippedTargetExists, res.Outcome)
	assert.False(t, res.Changed())
	assert.False(t, res.Relocated)

	// Reported on every pass until reconciled by hand.
	for i := 0; i < 2; i++ {
		counts := srv.alignClonedProjectIdentities(ctx)
		assert.Equal(t, 1, counts.skippedTargetExists)
	}

	ident, err := config.ReadWorkspaceIdentity(workspacePath)
	require.NoError(t, err)
	assert.Equal(t, localID, ident.ID, "identity is unchanged")
	assert.Equal(t, previous, workspaceConfigRoot(t, workspacePath))
	assert.Equal(t, "kept\n", readFile(t, filepath.Join(want, config.DotScion, "settings.yaml")))
	assert.Equal(t, "shared", readFile(t, filepath.Join(previous, config.SharedDirsSubdir, "data", "file.txt")))
	assert.Equal(t, "schema_version: \"1\"\n", readFile(t, filepath.Join(previous, config.DotScion, "settings.yaml")))
}

func TestAlignClonedProjectIdentities_IdentityOutsideExpectedFormChangesNothing(t *testing.T) {
	for _, tc := range []struct {
		name       string
		markerForm bool
		id, slug   string
	}{
		{name: "marker slug with parent segments", markerForm: true, id: "5b0e6f3a-2c4d-4e8f-9a1b-3c5d7e9f1a2b", slug: "../../other"},
		{name: "marker slug with separator", markerForm: true, id: "5b0e6f3a-2c4d-4e8f-9a1b-3c5d7e9f1a2b", slug: "a/b"},
		{name: "marker id with parent segments", markerForm: true, id: "../../other", slug: "other"},
		{name: "project-id file with parent segments", markerForm: false, id: "../../other"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := identityTestHome(t)
			srv, st := testServer(t)
			ctx := context.Background()

			project := sharedWorkspaceProject("form-check")
			require.NoError(t, st.CreateProject(ctx, project))

			workspacePath, err := localProjectPath(project.Slug)
			require.NoError(t, err)
			require.NoError(t, os.MkdirAll(workspacePath, 0755))
			scionPath := filepath.Join(workspacePath, config.DotScion)
			slug := tc.slug
			if tc.markerForm {
				require.NoError(t, config.WriteProjectMarker(scionPath, &config.ProjectMarker{
					ProjectID: tc.id, ProjectName: "other", ProjectSlug: tc.slug,
				}))
			} else {
				require.NoError(t, os.MkdirAll(scionPath, 0755))
				require.NoError(t, config.WriteProjectID(scionPath, tc.id))
				slug = project.Slug
			}

			// The project-config path accepts only IDs in the project ID format.
			if config.ValidateProjectID(tc.id) != nil {
				_, err := config.ProjectMarker{ProjectID: tc.id, ProjectSlug: slug}.ExternalProjectPath()
				require.ErrorIs(t, err, config.ErrInvalidProjectID)
			}

			// A directory where an unchecked join of the recorded identity
			// would point.
			short := strings.ReplaceAll(tc.id, "-", "")
			if len(short) > 8 {
				short = short[:8]
			}
			named := filepath.Join(home, config.GlobalDir, config.ProjectConfigsDir, slug+"__"+short)
			require.NoError(t, os.MkdirAll(named, 0755))
			require.NoError(t, os.WriteFile(filepath.Join(named, "file.txt"), []byte("other"), 0644))

			before := snapshotTree(t, home)

			counts := srv.alignClonedProjectIdentities(ctx)
			assert.Equal(t, 1, counts.skippedUnexpectedIdentity)
			assert.Zero(t, counts.failed)
			assert.Zero(t, counts.aligned)

			res, err := alignWorkspaceProjectIdentity(workspacePath, project.Slug, project.ID, isFormerID, notInUse)
			require.NoError(t, err)
			assert.Equal(t, alignSkippedUnexpectedIdentity, res.Outcome)

			after := snapshotTree(t, home)
			assert.Equal(t, before, after, "no file or directory changed")
		})
	}
}

func TestAlignWorkspaceProjectIdentity_EmptyRecordDirIsReplaced(t *testing.T) {
	identityTestHome(t)
	project := sharedWorkspaceProject("empty-target")
	workspacePath, previous := seedWorkspaceIdentity(t, project.Slug, api.NewUUID(), false)

	want, err := projectConfigRoot(project.Slug, project.ID)
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(filepath.Join(want, config.DotScion, "agents"), 0755))

	res, err := alignWorkspaceProjectIdentity(workspacePath, project.Slug, project.ID, isFormerID, notInUse)
	require.NoError(t, err)
	assert.True(t, res.Relocated)
	assert.Equal(t, alignAligned, res.Outcome)
	assert.NoDirExists(t, previous)
	assert.Equal(t, "shared", readFile(t, filepath.Join(want, config.SharedDirsSubdir, "data", "file.txt")))
}

func TestAlignWorkspaceProjectIdentity_CompletesAfterMovedDir(t *testing.T) {
	identityTestHome(t)
	project := sharedWorkspaceProject("moved-dir")
	localID := api.NewUUID()
	workspacePath, previous := seedWorkspaceIdentity(t, project.Slug, localID, false)

	// The directory was moved by an earlier run that did not record the
	// identity.
	want, err := projectConfigRoot(project.Slug, project.ID)
	require.NoError(t, err)
	require.NoError(t, os.Rename(previous, want))

	res, err := alignWorkspaceProjectIdentity(workspacePath, project.Slug, project.ID, isFormerID, notInUse)
	require.NoError(t, err)
	assert.True(t, res.Changed())
	assert.False(t, res.Relocated)
	assert.Equal(t, want, workspaceConfigRoot(t, workspacePath))
	assert.Equal(t, "shared", readFile(t, filepath.Join(want, config.SharedDirsSubdir, "data", "file.txt")))
}

// identitySnapshot returns the raw identity entry of a workspace: the marker
// content, the project-id content, or a fixed value when absent.
func identitySnapshot(t *testing.T, workspacePath string) string {
	t.Helper()
	scionPath := filepath.Join(workspacePath, config.DotScion)
	info, err := os.Lstat(scionPath)
	if os.IsNotExist(err) {
		return "<none>"
	}
	require.NoError(t, err)
	if !info.IsDir() {
		return "marker:" + readFile(t, scionPath)
	}
	data, err := os.ReadFile(filepath.Join(scionPath, projectkeys.ProjectIDFile))
	if os.IsNotExist(err) {
		return "dir:<no project-id>"
	}
	require.NoError(t, err)
	return "dir:" + string(data)
}

// sameHomeManager is the smallest agent.Manager a broker create needs.
type sameHomeManager struct {
	agent.Manager
}

func (m *sameHomeManager) List(context.Context, map[string]string) ([]api.AgentInfo, error) {
	return nil, nil
}

func (m *sameHomeManager) Preflight(context.Context, api.StartOptions) error { return nil }

func (m *sameHomeManager) Provision(context.Context, api.StartOptions) (*api.ScionConfig, error) {
	return &api.ScionConfig{}, nil
}

func (m *sameHomeManager) Start(_ context.Context, opts api.StartOptions) (*api.AgentInfo, error) {
	return &api.AgentInfo{ID: "container-1", Name: opts.Name, Phase: "running"}, nil
}

// TestHubWorkspaceOnSameHome_BrokerKeepsHubIdentity runs the hub's record
// writer and a broker against one HOME. All coordination between them is on
// disk, so this covers a broker in the hub process and a broker in a
// separate process with the same HOME alike. The hub workspace carries an
// identity other than the hub project ID (an older clone the hub step has
// not aligned yet); the broker's create (with and without a workspace
// upload) must leave it, and its project config directory, unchanged.
func TestHubWorkspaceOnSameHome_BrokerKeepsHubIdentity(t *testing.T) {
	for _, tc := range []struct {
		name          string
		markerForm    bool
		brokerRecord  bool
		withWorkspace bool
	}{
		{"project-id file, upload", false, false, true},
		{"marker file, upload", true, false, true},
		// No "broker record present, upload" case: without the retired hub
		// workspace record the broker cannot tell the hub's workspace from
		// its own on a shared HOME, and the hub no longer sends a workspace
		// upload for its own hub-managed workspace (a co-located broker gets
		// the workspace path; a remote broker without a path is refused).
		{"project-id file, no upload", false, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			identityTestHome(t)
			_, st := testServer(t)
			ctx := context.Background()

			project := sharedWorkspaceProject("same-home")
			require.NoError(t, st.CreateProject(ctx, project))
			localID := api.NewUUID()
			workspacePath, previous := seedWorkspaceIdentity(t, project.Slug, localID, tc.markerForm)

			brokerRecord, err := config.BrokerWorkspaceRecordPath(project.Slug)
			require.NoError(t, err)
			if tc.brokerRecord {
				require.NoError(t, config.WriteWorkspaceRecord(brokerRecord, project.ID))
			}
			before := identitySnapshot(t, workspacePath)

			cfg := runtimebroker.DefaultServerConfig()
			cfg.BrokerAuthEnabled = false // no hub keys here: serve the broker API without auth
			cfg.BrokerID = "same-home-broker"
			cfg.BrokerName = "same-home-broker"
			cfg.StorageBucket = "bucket"
			brokerSrv := runtimebroker.New(cfg, &sameHomeManager{}, &runtime.MockRuntime{NameFunc: func() string { return "docker" }}, config.RuntimeBuildRecord{})
			brokerSrv.SetWorkspaceDownloader(func(_ context.Context, _, _, localPath string) error {
				return os.WriteFile(filepath.Join(localPath, "downloaded.txt"), []byte("downloaded"), 0644)
			})

			createReq := runtimebroker.CreateAgentRequest{
				ID:          "agent-same-home",
				Name:        "agent-same-home",
				ProjectID:   project.ID,
				ProjectSlug: project.Slug,
			}
			if tc.withWorkspace {
				// A broker the hub treats as remote: workspace upload.
				createReq.WorkspaceStoragePath = "workspaces/" + project.ID
			} else {
				// A broker the hub treats as local: the hub workspace path.
				createReq.WorkspaceMode = store.WorkspaceModeShared
				createReq.Config = &runtimebroker.CreateAgentConfig{Workspace: workspacePath}
			}
			body, err := json.Marshal(createReq)
			require.NoError(t, err)
			req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(string(body)))
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			brokerSrv.Handler().ServeHTTP(rec, req)
			require.Less(t, rec.Code, 300, rec.Body.String())

			assert.Equal(t, before, identitySnapshot(t, workspacePath), "hub workspace identity unchanged")
			assert.Equal(t, previous, workspaceConfigRoot(t, workspacePath), "project config directory unchanged")
			if !tc.brokerRecord {
				assert.NoFileExists(t, brokerRecord, "no broker record for the hub's own workspace")
			}
		})
	}
}

func TestAlignWorkspaceProjectIdentity_EntryAddedToEmptyRecordDirIsKept(t *testing.T) {
	identityTestHome(t)
	project := sharedWorkspaceProject("entry-added")
	localID := api.NewUUID()
	workspacePath, previous := seedWorkspaceIdentity(t, project.Slug, localID, false)

	want, err := projectConfigRoot(project.Slug, project.ID)
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(filepath.Join(want, config.DotScion, "agents"), 0755))

	// A file appears in the destination after it was inspected and found
	// to hold only empty directories.
	added := filepath.Join(want, config.DotScion, "agents", "added.txt")
	inUse := func() (bool, error) {
		return false, os.WriteFile(added, []byte("added"), 0644)
	}

	res, err := alignWorkspaceProjectIdentity(workspacePath, project.Slug, project.ID, isFormerID, inUse)
	require.NoError(t, err)
	assert.Equal(t, alignSkippedTargetExists, res.Outcome)
	assert.False(t, res.Changed())
	assert.Equal(t, "added", readFile(t, added), "the file is kept")
	ident, err := config.ReadWorkspaceIdentity(workspacePath)
	require.NoError(t, err)
	assert.Equal(t, localID, ident.ID, "identity is unchanged")
	assert.Equal(t, "shared", readFile(t, filepath.Join(previous, config.SharedDirsSubdir, "data", "file.txt")))
}

func TestRemoveEmptyDirs_RemovesOnlyDirectories(t *testing.T) {
	t.Run("empty tree", func(t *testing.T) {
		root := filepath.Join(t.TempDir(), "root")
		require.NoError(t, os.MkdirAll(filepath.Join(root, "a", "b"), 0755))
		require.NoError(t, removeEmptyDirs(root))
		assert.NoDirExists(t, root)
	})
	t.Run("tree with a file", func(t *testing.T) {
		root := filepath.Join(t.TempDir(), "root")
		require.NoError(t, os.MkdirAll(filepath.Join(root, "a", "b"), 0755))
		file := filepath.Join(root, "a", "keep.txt")
		require.NoError(t, os.WriteFile(file, []byte("keep"), 0644))
		require.ErrorIs(t, removeEmptyDirs(root), errConfigRootNotEmpty)
		assert.Equal(t, "keep", readFile(t, file))
	})
}

func TestAlignWorkspaceProjectIdentity_UnreadableIdentityIsFailure(t *testing.T) {
	identityTestHome(t)
	srv, st := testServer(t)
	ctx := context.Background()
	project := sharedWorkspaceProject("unreadable-identity")
	require.NoError(t, st.CreateProject(ctx, project))
	workspacePath, previous := seedWorkspaceIdentity(t, project.Slug, api.NewUUID(), false)

	// The project-id entry cannot be read as a file.
	scionPath := filepath.Join(workspacePath, config.DotScion)
	require.NoError(t, os.Remove(filepath.Join(scionPath, projectkeys.ProjectIDFile)))
	require.NoError(t, os.MkdirAll(filepath.Join(scionPath, projectkeys.ProjectIDFile, "x"), 0755))

	_, err := alignWorkspaceProjectIdentity(workspacePath, project.Slug, project.ID, isFormerID, notInUse)
	require.Error(t, err)
	assert.NotErrorIs(t, err, config.ErrInvalidProjectID)

	counts := srv.alignClonedProjectIdentities(ctx)
	assert.Equal(t, 1, counts.failed)
	assert.Zero(t, counts.skippedUnexpectedIdentity)
	assert.DirExists(t, previous)
}

// madeUpIDWithSamePrefix returns a project ID other than id whose first
// eight characters (the <id8> of a project config directory name) are id's.
func madeUpIDWithSamePrefix(t *testing.T, id string) string {
	t.Helper()
	tail := "0"
	if strings.HasSuffix(id, "0") {
		tail = "1"
	}
	other := id[:len(id)-1] + tail
	require.NotEqual(t, id, other)
	return other
}

func TestAlignClonedProjectIdentities_FormerIDOfSameProjectIsMoved(t *testing.T) {
	identityForms(t, func(t *testing.T, markerForm bool) {
		identityTestHome(t)
		srv, st := testServer(t)
		ctx := context.Background()

		// An unrelated live project on the same hub.
		other := sharedWorkspaceProject("bystander")
		require.NoError(t, st.CreateProject(ctx, other))
		otherWS, otherRoot := seedWorkspaceIdentity(t, other.Slug, other.ID, markerForm)
		otherBefore := snapshotTree(t, otherRoot)

		project := sharedWorkspaceProject("former-id")
		require.NoError(t, st.CreateProject(ctx, project))
		formerID := api.NewUUID()
		workspacePath, previous := seedWorkspaceIdentity(t, project.Slug, formerID, markerForm)

		rel, err := srv.recordedProjectRelation(ctx, project, project.Slug, formerID)
		require.NoError(t, err)
		assert.Equal(t, recordedSameProject, rel, "an ID under the project's slug that no live project has is a former ID")

		counts := srv.alignClonedProjectIdentities(ctx)
		assert.Equal(t, 1, counts.aligned)
		assert.Equal(t, 1, counts.alreadyMatching)
		assert.Zero(t, counts.skippedNotFormerID)

		want, err := projectConfigRoot(project.Slug, project.ID)
		require.NoError(t, err)
		assert.Equal(t, want, workspaceConfigRoot(t, workspacePath))
		assert.NoDirExists(t, previous)
		assert.Equal(t, "shared", readFile(t, filepath.Join(want, config.SharedDirsSubdir, "data", "file.txt")))

		assert.Equal(t, otherRoot, workspaceConfigRoot(t, otherWS))
		assert.Equal(t, otherBefore, snapshotTree(t, otherRoot))
	})
}

// TestAlignClonedProjectIdentities_NotFormerIDChangesNothing covers a
// hub-cloned workspace whose recorded identity is not a former ID of the
// project: another live project's ID (with that project's slug or the
// workspace's own), a made-up ID naming another project's config directory,
// or a made-up ID no project has under a slug other than the project's.
func TestAlignClonedProjectIdentities_NotFormerIDChangesNothing(t *testing.T) {
	for _, tc := range []struct {
		name       string
		markerForm bool
		// recorded returns the slug and ID the workspace records.
		recorded func(t *testing.T, project, owner *store.Project) (string, string)
	}{
		{"marker naming the other project", true, func(_ *testing.T, _, owner *store.Project) (string, string) {
			return owner.Slug, owner.ID
		}},
		{"marker with own slug and the other project's ID", true, func(_ *testing.T, project, owner *store.Project) (string, string) {
			return project.Slug, owner.ID
		}},
		{"project-id file with the other project's ID", false, func(_ *testing.T, project, owner *store.Project) (string, string) {
			return project.Slug, owner.ID
		}},
		{"marker naming the other project's config directory", true, func(t *testing.T, _, owner *store.Project) (string, string) {
			return owner.Slug, madeUpIDWithSamePrefix(t, owner.ID)
		}},
		{"marker with an unowned made-up ID under another slug", true, func(_ *testing.T, _, _ *store.Project) (string, string) {
			return "elsewhere", api.NewUUID()
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			identityTestHome(t)
			restore := config.OverrideIsGitRepo(func() bool { return !tc.markerForm })
			t.Cleanup(restore)
			srv, st := testServer(t)
			ctx := context.Background()
			var logs strings.Builder
			srv.projectsLog = slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))

			owner := sharedWorkspaceProject("owner-project")
			require.NoError(t, st.CreateProject(ctx, owner))
			ownerWS, ownerRoot := seedWorkspaceIdentity(t, owner.Slug, owner.ID, tc.markerForm)

			project := sharedWorkspaceProject("mismatched-clone")
			require.NoError(t, st.CreateProject(ctx, project))
			recordedSlug, recordedID := tc.recorded(t, project, owner)
			workspacePath, recordedRoot := seedWorkspaceIdentity(t, project.Slug, recordedID, tc.markerForm)
			if tc.markerForm {
				require.NoError(t, config.WriteProjectMarker(filepath.Join(workspacePath, config.DotScion), &config.ProjectMarker{
					ProjectID: recordedID, ProjectName: recordedSlug, ProjectSlug: recordedSlug,
				}))
				recordedRoot = workspaceConfigRoot(t, workspacePath)
				require.NoError(t, os.MkdirAll(filepath.Join(recordedRoot, config.SharedDirsSubdir, "data"), 0755))
				recordedFile := filepath.Join(recordedRoot, config.SharedDirsSubdir, "data", "recorded.txt")
				require.NoError(t, os.WriteFile(recordedFile, []byte("recorded"), 0644))
			}

			configsDir := filepath.Dir(ownerRoot)
			configsBefore := snapshotTree(t, configsDir)
			ownerIdentity := identitySnapshot(t, ownerWS)
			projectIdentity := identitySnapshot(t, workspacePath)

			res, err := alignWorkspaceProjectIdentity(workspacePath, project.Slug, project.ID, func(slug, id string) (recordedRelation, error) {
				return srv.recordedProjectRelation(ctx, project, slug, id)
			}, func() (bool, error) {
				t.Fatal("in-use check reached for an identity that is not a former ID")
				return false, nil
			})
			require.NoError(t, err)
			assert.Equal(t, alignSkippedNotFormerID, res.Outcome)
			assert.Equal(t, recordedID, res.RecordedID)
			assert.False(t, res.Changed())
			assert.False(t, res.Relocated)

			counts := srv.alignClonedProjectIdentities(ctx)
			assert.Equal(t, 1, counts.skippedNotFormerID)
			assert.Equal(t, 1, counts.alreadyMatching)
			assert.Zero(t, counts.aligned)
			assert.Zero(t, counts.failed)

			assert.Equal(t, configsBefore, snapshotTree(t, configsDir), "no project config directory is touched")
			assert.Equal(t, ownerIdentity, identitySnapshot(t, ownerWS), "the other project's identity is unchanged")
			assert.Equal(t, projectIdentity, identitySnapshot(t, workspacePath), "the workspace identity is unchanged")
			assert.Equal(t, ownerRoot, workspaceConfigRoot(t, ownerWS))
			assert.Equal(t, recordedRoot, workspaceConfigRoot(t, workspacePath))
			want, err := projectConfigRoot(project.Slug, project.ID)
			require.NoError(t, err)
			assert.NoDirExists(t, want)

			var mismatch []string
			for _, line := range strings.Split(logs.String(), "\n") {
				if strings.Contains(line, "is not a former ID of the project") {
					mismatch = append(mismatch, line)
				}
			}
			require.Len(t, mismatch, 1, logs.String())
			assert.Contains(t, mismatch[0], "project_id="+project.ID)
			assert.Contains(t, mismatch[0], "recorded_project_id="+recordedID)
			assert.NotContains(t, mismatch[0], configsDir, "the log line carries IDs only")
			assert.NotContains(t, mismatch[0], workspacePath, "the log line carries IDs only")
		})
	}
}

// TestAlignClonedProjectIdentities_FirstLinkCloneUnaffected covers the
// ordinary path: a fresh hub clone records the hub project ID, and the
// hub-start pass finds it matching and changes nothing.
func TestAlignClonedProjectIdentities_FirstLinkCloneUnaffected(t *testing.T) {
	identityForms(t, func(t *testing.T, markerForm bool) {
		identityTestHome(t)
		srv, st := testServer(t)
		ctx := context.Background()

		sourceDir := t.TempDir()
		for _, args := range [][]string{
			{"init"},
			{"config", "user.email", "test@test.com"},
			{"config", "user.name", "Test"},
			{"commit", "--allow-empty", "-m", "init"},
		} {
			cmd := exec.Command("git", args...)
			cmd.Dir = sourceDir
			require.NoError(t, cmd.Run(), "git %v", args)
		}
		allowLocalCloneFixture(t, sourceDir)

		project := sharedWorkspaceProject("first-link")
		project.Labels[store.LabelCloneURL] = sourceDir
		project.Labels[store.LabelDefaultBranch] = "master"
		require.NoError(t, st.CreateProject(ctx, project))
		require.NoError(t, srv.cloneSharedWorkspaceProject(ctx, project))

		workspacePath, err := localProjectPath(project.Slug)
		require.NoError(t, err)
		want, err := projectConfigRoot(project.Slug, project.ID)
		require.NoError(t, err)
		configsBefore := snapshotTree(t, filepath.Dir(want))
		identityBefore := identitySnapshot(t, workspacePath)

		counts := srv.alignClonedProjectIdentities(ctx)
		assert.Equal(t, 1, counts.alreadyMatching)
		assert.Zero(t, counts.aligned)
		assert.Zero(t, counts.skippedNotFormerID)

		assert.Equal(t, identityBefore, identitySnapshot(t, workspacePath))
		assert.Equal(t, configsBefore, snapshotTree(t, filepath.Dir(want)))
		assert.Equal(t, want, workspaceConfigRoot(t, workspacePath))
	})
}

// TestBrokerRecordedOtherProjectIdentity_HubAlignmentChangesNothing runs a
// broker and the hub-start pass against one HOME. A broker create
// dispatched for one project with another hub-cloned project's workspace
// as its project path records the dispatched project's identity in that
// workspace; the hub-start pass then leaves both projects unchanged.
func TestBrokerRecordedOtherProjectIdentity_HubAlignmentChangesNothing(t *testing.T) {
	identityTestHome(t)
	restore := config.OverrideIsGitRepo(func() bool { return false })
	t.Cleanup(restore)
	srv, st := testServer(t)
	ctx := context.Background()
	var logs strings.Builder
	srv.projectsLog = slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))

	owner := sharedWorkspaceProject("dispatched-project")
	require.NoError(t, st.CreateProject(ctx, owner))
	ownerWS, ownerRoot := seedWorkspaceIdentity(t, owner.Slug, owner.ID, true)

	project := sharedWorkspaceProject("receiving-clone")
	require.NoError(t, st.CreateProject(ctx, project))
	workspacePath, err := localProjectPath(project.Slug)
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(workspacePath, 0755))

	cfg := runtimebroker.DefaultServerConfig()
	cfg.BrokerAuthEnabled = false // no hub keys here: serve the broker API without auth
	cfg.BrokerID = "same-home-broker"
	cfg.BrokerName = "same-home-broker"
	cfg.StorageBucket = "bucket"
	brokerSrv := runtimebroker.New(cfg, &sameHomeManager{}, &runtime.MockRuntime{NameFunc: func() string { return "docker" }}, config.RuntimeBuildRecord{})

	body, err := json.Marshal(runtimebroker.CreateAgentRequest{
		ID:          "agent-dispatched",
		Name:        "agent-dispatched",
		ProjectID:   owner.ID,
		ProjectSlug: owner.Slug,
		ProjectPath: workspacePath,
	})
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	brokerSrv.Handler().ServeHTTP(rec, req)
	require.Less(t, rec.Code, 300, rec.Body.String())

	ident, err := config.ReadWorkspaceIdentity(workspacePath)
	require.NoError(t, err)
	require.NotNil(t, ident, "the broker records the dispatched identity")
	require.Equal(t, owner.ID, ident.ID)

	configsDir := filepath.Dir(ownerRoot)
	configsBefore := snapshotTree(t, configsDir)
	ownerIdentity := identitySnapshot(t, ownerWS)
	projectIdentity := identitySnapshot(t, workspacePath)

	counts := srv.alignClonedProjectIdentities(ctx)
	assert.Equal(t, 1, counts.skippedNotFormerID)
	assert.Zero(t, counts.aligned)

	assert.Equal(t, configsBefore, snapshotTree(t, configsDir), "no project config directory is touched")
	assert.Equal(t, ownerIdentity, identitySnapshot(t, ownerWS))
	assert.Equal(t, projectIdentity, identitySnapshot(t, workspacePath))
	assert.Equal(t, "shared", readFile(t, filepath.Join(ownerRoot, config.SharedDirsSubdir, "data", "file.txt")))
	assert.Contains(t, logs.String(), "recorded_project_id="+owner.ID)
}

func TestAlignWorkspaceProjectIdentity_FormerIDLookupFailureChangesNothing(t *testing.T) {
	identityTestHome(t)
	project := sharedWorkspaceProject("lookup-failure")
	workspacePath, previous := seedWorkspaceIdentity(t, project.Slug, api.NewUUID(), false)
	before := identitySnapshot(t, workspacePath)
	configsBefore := snapshotTree(t, filepath.Dir(previous))

	lookupErr := errors.New("store unavailable")
	_, err := alignWorkspaceProjectIdentity(workspacePath, project.Slug, project.ID, func(string, string) (recordedRelation, error) {
		return recordedNotFormer, lookupErr
	}, func() (bool, error) {
		t.Fatal("in-use check reached after a failed former ID lookup")
		return false, nil
	})
	require.ErrorIs(t, err, lookupErr)
	var ae *alignError
	require.ErrorAs(t, err, &ae)
	assert.Equal(t, "look up recorded project", ae.step)

	assert.Equal(t, before, identitySnapshot(t, workspacePath))
	assert.Equal(t, configsBefore, snapshotTree(t, filepath.Dir(previous)))
}

// writeMarker records slug and id in the workspace's .scion marker file.
func writeMarker(t *testing.T, workspacePath, slug, id string) {
	t.Helper()
	require.NoError(t, config.WriteProjectMarker(filepath.Join(workspacePath, config.DotScion), &config.ProjectMarker{
		ProjectID: id, ProjectName: slug, ProjectSlug: slug,
	}))
}

// TestAlignClonedProjectIdentities_OwnIDUnderOldSlugIsRepaired covers a slug
// rename: the workspace marker records the project's own ID under the
// earlier slug. The marker is rewritten to the current slug, and the project
// config directory under the earlier slug is moved when it is still there.
func TestAlignClonedProjectIdentities_OwnIDUnderOldSlugIsRepaired(t *testing.T) {
	for _, tc := range []struct {
		name string
		// oldDirLeft keeps the project config directory under the earlier
		// slug (the rename did not move it) instead of under the current one.
		oldDirLeft bool
	}{
		{"config directory already renamed", false},
		{"config directory left under the earlier slug", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			identityTestHome(t)
			restore := config.OverrideIsGitRepo(func() bool { return false })
			t.Cleanup(restore)
			srv, st := testServer(t)
			ctx := context.Background()

			project := sharedWorkspaceProject("renamed-project")
			require.NoError(t, st.CreateProject(ctx, project))
			workspacePath, currentRoot := seedWorkspaceIdentity(t, project.Slug, project.ID, true)
			oldRoot, err := projectConfigRoot("earlier-slug", project.ID)
			require.NoError(t, err)
			if tc.oldDirLeft {
				require.NoError(t, os.Rename(currentRoot, oldRoot))
			}
			writeMarker(t, workspacePath, "earlier-slug", project.ID)
			moved := snapshotTree(t, map[bool]string{true: oldRoot, false: currentRoot}[tc.oldDirLeft])

			counts := srv.alignClonedProjectIdentities(ctx)
			assert.Equal(t, 1, counts.aligned)
			assert.Zero(t, counts.skippedNotFormerID)
			assert.Zero(t, counts.skippedOtherProjectDir)

			ident, err := config.ReadWorkspaceIdentity(workspacePath)
			require.NoError(t, err)
			require.NotNil(t, ident.Marker)
			assert.Equal(t, project.ID, ident.ID)
			assert.Equal(t, project.Slug, ident.Slug, "the marker records the current slug")
			assert.Equal(t, currentRoot, workspaceConfigRoot(t, workspacePath))
			assert.NoDirExists(t, oldRoot)
			got := snapshotTree(t, currentRoot)
			assert.Len(t, got, len(moved), "the project config directory is moved whole")
			assert.Equal(t, "shared", readFile(t, filepath.Join(currentRoot, config.SharedDirsSubdir, "data", "file.txt")))
			entries, err := os.ReadDir(filepath.Dir(currentRoot))
			require.NoError(t, err)
			assert.Len(t, entries, 1, "no other project config directory is created or left")
		})
	}
}

// TestAlignClonedProjectIdentities_OwnIDUnderOtherProjectsDirChangesNothing
// covers the project's own ID recorded under a slug that another live
// project holds, whose ID shares the project's first eight characters: the
// directory <slug>__<id8> is that project's, and nothing is moved.
func TestAlignClonedProjectIdentities_OwnIDUnderOtherProjectsDirChangesNothing(t *testing.T) {
	identityTestHome(t)
	restore := config.OverrideIsGitRepo(func() bool { return false })
	t.Cleanup(restore)
	srv, st := testServer(t)
	ctx := context.Background()
	var logs strings.Builder
	srv.projectsLog = slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))

	project := sharedWorkspaceProject("renamed-project")
	require.NoError(t, st.CreateProject(ctx, project))
	workspacePath, _ := seedWorkspaceIdentity(t, project.Slug, project.ID, true)

	holder := sharedWorkspaceProject("earlier-slug")
	holder.ID = madeUpIDWithSamePrefix(t, project.ID)
	require.NoError(t, st.CreateProject(ctx, holder))
	holderWS, holderRoot := seedWorkspaceIdentity(t, holder.Slug, holder.ID, true)
	collided, err := projectConfigRoot(holder.Slug, project.ID)
	require.NoError(t, err)
	require.Equal(t, holderRoot, collided, "the recorded identity names the other project's directory")

	writeMarker(t, workspacePath, holder.Slug, project.ID)
	configsDir := filepath.Dir(holderRoot)
	configsBefore := snapshotTree(t, configsDir)
	projectIdentity := identitySnapshot(t, workspacePath)
	holderIdentity := identitySnapshot(t, holderWS)

	rel, err := srv.recordedProjectRelation(ctx, project, holder.Slug, project.ID)
	require.NoError(t, err)
	assert.Equal(t, recordedOtherProjectDir, rel)

	counts := srv.alignClonedProjectIdentities(ctx)
	assert.Equal(t, 1, counts.skippedOtherProjectDir)
	assert.Equal(t, 1, counts.alreadyMatching)
	assert.Zero(t, counts.aligned)

	assert.Equal(t, configsBefore, snapshotTree(t, configsDir), "no project config directory is touched")
	assert.Equal(t, projectIdentity, identitySnapshot(t, workspacePath))
	assert.Equal(t, holderIdentity, identitySnapshot(t, holderWS))

	var lines []string
	for _, line := range strings.Split(logs.String(), "\n") {
		if strings.Contains(line, "belongs to another project") {
			lines = append(lines, line)
		}
	}
	require.Len(t, lines, 1, logs.String())
	assert.Contains(t, lines[0], "project_id="+project.ID)
	assert.Contains(t, lines[0], "recorded_project_id="+project.ID)
	assert.NotContains(t, lines[0], configsDir, "the log line carries IDs only")
}

// errIdentityLookupFault is the store error the identity lookup wrappers
// return while armed.
var errIdentityLookupFault = errors.New("store unavailable")

// getProjectFailingStore fails GetProject for one project ID with a store
// error other than not found while its switch is armed.
type getProjectFailingStore struct {
	store.Store
	fault  *storeFaultSwitch
	failID string
}

func (s *getProjectFailingStore) GetProject(ctx context.Context, id string) (*store.Project, error) {
	if s.fault.Active() && id == s.failID {
		return nil, errIdentityLookupFault
	}
	return s.Store.GetProject(ctx, id)
}

// TestAlignClonedProjectIdentities_StoreErrorMovesNothing covers the real
// lookup failing for a recorded ID that would otherwise move (an unowned ID
// under the project's slug): the project counts as failed and nothing
// changes.
func TestAlignClonedProjectIdentities_StoreErrorMovesNothing(t *testing.T) {
	identityTestHome(t)
	restore := config.OverrideIsGitRepo(func() bool { return false })
	t.Cleanup(restore)
	recordedID := api.NewUUID()
	srv, st, _, fault := testServerWithStoreFault(t, func(inner store.Store, f *storeFaultSwitch) *getProjectFailingStore {
		return &getProjectFailingStore{Store: inner, fault: f, failID: recordedID}
	})
	ctx := context.Background()
	var logs strings.Builder
	srv.projectsLog = slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))

	project := sharedWorkspaceProject("lookup-outage")
	require.NoError(t, st.CreateProject(ctx, project))
	workspacePath, previous := seedWorkspaceIdentity(t, project.Slug, recordedID, false)
	configsDir := filepath.Dir(previous)
	configsBefore := snapshotTree(t, configsDir)
	identityBefore := identitySnapshot(t, workspacePath)

	fault.Arm()

	counts := srv.alignClonedProjectIdentities(ctx)
	assert.Equal(t, 1, counts.failed)
	assert.Zero(t, counts.aligned)
	assert.Zero(t, counts.skippedNotFormerID)

	assert.Equal(t, configsBefore, snapshotTree(t, configsDir), "no project config directory is touched")
	assert.Equal(t, identityBefore, identitySnapshot(t, workspacePath))
	want, err := projectConfigRoot(project.Slug, project.ID)
	require.NoError(t, err)
	assert.NoDirExists(t, want)

	var lines []string
	for _, line := range strings.Split(logs.String(), "\n") {
		if strings.Contains(line, "failed to align workspace identity") {
			lines = append(lines, line)
		}
	}
	require.Len(t, lines, 1, logs.String())
	assert.Contains(t, lines[0], `step="look up recorded project"`)
	assert.Contains(t, lines[0], "project_id="+project.ID)
	assert.NotContains(t, lines[0], configsDir, "the log line carries no path")
	assert.NotContains(t, lines[0], workspacePath, "the log line carries no path")
}

// getProjectBySlugFailingStore fails GetProjectBySlug for one slug with a
// store error other than not found while its switch is armed.
type getProjectBySlugFailingStore struct {
	store.Store
	fault    *storeFaultSwitch
	failSlug string
}

func (s *getProjectBySlugFailingStore) GetProjectBySlug(ctx context.Context, slug string) (*store.Project, error) {
	if s.fault.Active() && slug == s.failSlug {
		return nil, errIdentityLookupFault
	}
	return s.Store.GetProjectBySlug(ctx, slug)
}

// TestAlignClonedProjectIdentities_OwnIDSlugLookupErrorMovesNothing covers
// the slug lookup of the own-ID case failing: the project's own ID recorded
// under an earlier slug, whose config directory would otherwise move. The
// project counts as failed and nothing changes.
func TestAlignClonedProjectIdentities_OwnIDSlugLookupErrorMovesNothing(t *testing.T) {
	identityTestHome(t)
	restore := config.OverrideIsGitRepo(func() bool { return false })
	t.Cleanup(restore)
	srv, st, _, fault := testServerWithStoreFault(t, func(inner store.Store, f *storeFaultSwitch) *getProjectBySlugFailingStore {
		return &getProjectBySlugFailingStore{Store: inner, fault: f, failSlug: "earlier-slug"}
	})
	ctx := context.Background()
	var logs strings.Builder
	srv.projectsLog = slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))

	project := sharedWorkspaceProject("renamed-project")
	require.NoError(t, st.CreateProject(ctx, project))
	workspacePath, currentRoot := seedWorkspaceIdentity(t, project.Slug, project.ID, true)
	oldRoot, err := projectConfigRoot("earlier-slug", project.ID)
	require.NoError(t, err)
	require.NoError(t, os.Rename(currentRoot, oldRoot))
	writeMarker(t, workspacePath, "earlier-slug", project.ID)

	configsDir := filepath.Dir(oldRoot)
	configsBefore := snapshotTree(t, configsDir)
	identityBefore := identitySnapshot(t, workspacePath)

	fault.Arm()

	counts := srv.alignClonedProjectIdentities(ctx)
	assert.Equal(t, 1, counts.failed)
	assert.Zero(t, counts.aligned)
	assert.Zero(t, counts.skippedOtherProjectDir)
	assert.Zero(t, counts.skippedNotFormerID)

	assert.Equal(t, configsBefore, snapshotTree(t, configsDir), "no project config directory is touched")
	assert.Equal(t, identityBefore, identitySnapshot(t, workspacePath))
	assert.NoDirExists(t, currentRoot)

	var lines []string
	for _, line := range strings.Split(logs.String(), "\n") {
		if strings.Contains(line, "failed to align workspace identity") {
			lines = append(lines, line)
		}
	}
	require.Len(t, lines, 1, logs.String())
	assert.Contains(t, lines[0], `step="look up recorded project"`)
	assert.Contains(t, lines[0], "project_id="+project.ID)
	assert.NotContains(t, lines[0], configsDir, "the log line carries IDs only")
	assert.NotContains(t, lines[0], workspacePath, "the log line carries IDs only")
}
