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
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
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

// identityTestHome points HOME at a fresh directory so workspaces and
// project config directories are created under it.
func identityTestHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	return home
}

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
	workspacePath, err := hubManagedProjectPath(slug)
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

func sharedWorkspaceProject(slug string) *store.Project {
	return &store.Project{
		ID:        api.NewUUID(),
		Name:      slug,
		Slug:      slug,
		GitRemote: "github.com/example/" + slug,
		Labels: map[string]string{
			store.LabelWorkspaceMode: store.WorkspaceModeShared,
		},
	}
}

func notInUse() (bool, error) { return false, nil }

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

		project := sharedWorkspaceProject("clone-identity")
		project.Labels[store.LabelCloneURL] = sourceDir
		project.Labels[store.LabelDefaultBranch] = "master"

		require.NoError(t, srv.cloneSharedWorkspaceProject(context.Background(), project))

		workspacePath, err := hubManagedProjectPath(project.Slug)
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

	project := sharedWorkspaceProject("clone-repo-identity")
	project.Labels[store.LabelCloneURL] = sourceDir
	project.Labels[store.LabelDefaultBranch] = "master"

	require.NoError(t, srv.cloneSharedWorkspaceProject(context.Background(), project))

	workspacePath, err := hubManagedProjectPath(project.Slug)
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
		res, err := alignWorkspaceProjectIdentity(workspacePath, project.Slug, project.ID, notInUse)
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

	res, err := alignWorkspaceProjectIdentity(workspacePath, project.Slug, project.ID, notInUse)
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

			workspacePath, err := hubManagedProjectPath(project.Slug)
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

			res, err := alignWorkspaceProjectIdentity(workspacePath, project.Slug, project.ID, notInUse)
			require.NoError(t, err)
			assert.Equal(t, alignSkippedUnexpectedIdentity, res.Outcome)

			after := snapshotTree(t, home)
			recordsDir := filepath.Join(home, config.GlobalDir, config.HubWorkspacesDir)
			for path := range after {
				if path == recordsDir || strings.HasPrefix(path, recordsDir+string(filepath.Separator)) {
					delete(after, path)
				}
			}
			assert.Equal(t, before, after, "no file or directory changed apart from the hub workspace record")
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

	res, err := alignWorkspaceProjectIdentity(workspacePath, project.Slug, project.ID, notInUse)
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

	res, err := alignWorkspaceProjectIdentity(workspacePath, project.Slug, project.ID, notInUse)
	require.NoError(t, err)
	assert.True(t, res.Changed())
	assert.False(t, res.Relocated)
	assert.Equal(t, want, workspaceConfigRoot(t, workspacePath))
	assert.Equal(t, "shared", readFile(t, filepath.Join(want, config.SharedDirsSubdir, "data", "file.txt")))
}

// stubHubWorkspaceDownload replaces the GCS download with one that writes
// a file and a different project identity into the hub workspace, in the
// form the workspace already has.
func stubHubWorkspaceDownload(t *testing.T) {
	t.Helper()
	orig := syncFromGCSIntoHubWorkspace
	t.Cleanup(func() { syncFromGCSIntoHubWorkspace = orig })
	syncFromGCSIntoHubWorkspace = func(_ context.Context, _, _, workspacePath string) error {
		if err := os.WriteFile(filepath.Join(workspacePath, "synced.txt"), []byte("synced"), 0644); err != nil {
			return err
		}
		scionPath := filepath.Join(workspacePath, config.DotScion)
		info, err := os.Lstat(scionPath)
		switch {
		case os.IsNotExist(err):
			return config.WriteProjectMarker(scionPath, &config.ProjectMarker{
				ProjectID: "copied-id", ProjectName: "copied", ProjectSlug: "copied",
			})
		case err != nil:
			return err
		case info.IsDir():
			return config.WriteProjectID(scionPath, "copied-id")
		default:
			return config.WriteProjectMarker(scionPath, &config.ProjectMarker{
				ProjectID: "copied-id", ProjectName: "copied", ProjectSlug: "copied",
			})
		}
	}
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

func TestSyncHubWorkspaceFromGCS_KeepsHubWorkspaceIdentity(t *testing.T) {
	for _, tc := range []struct {
		name string
		seed func(t *testing.T, scionPath string)
	}{
		{"project-id file", func(t *testing.T, p string) {
			require.NoError(t, os.MkdirAll(p, 0755))
			require.NoError(t, config.WriteProjectID(p, api.NewUUID()))
		}},
		{"directory without project-id", func(t *testing.T, p string) {
			require.NoError(t, os.MkdirAll(p, 0755))
		}},
		{"marker file", func(t *testing.T, p string) {
			require.NoError(t, config.WriteProjectMarker(p, &config.ProjectMarker{
				ProjectID: api.NewUUID(), ProjectName: "hub", ProjectSlug: "hub",
			}))
		}},
		{"no entry", func(t *testing.T, p string) {}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			identityTestHome(t)
			srv, _ := testServer(t)
			stubHubWorkspaceDownload(t)
			workspacePath := t.TempDir()
			tc.seed(t, filepath.Join(workspacePath, config.DotScion))
			before := identitySnapshot(t, workspacePath)

			require.NoError(t, srv.syncHubWorkspaceFromGCS(context.Background(), "bucket", "prefix", workspacePath))

			assert.Equal(t, before, identitySnapshot(t, workspacePath))
			assert.Equal(t, "synced", readFile(t, filepath.Join(workspacePath, "synced.txt")), "other files are downloaded")
		})
	}
}

func TestSyncHubManagedWorkspaceBack_KeepsHubWorkspaceIdentity(t *testing.T) {
	identityForms(t, func(t *testing.T, markerForm bool) {
		identityTestHome(t)
		srv, st := testServer(t)
		ctx := context.Background()
		stubHubWorkspaceDownload(t)
		srv.SetStorage(newMockStorage("bucket"))

		project := sharedWorkspaceProject("sync-back")
		require.NoError(t, st.CreateProject(ctx, project))
		workspacePath, _ := seedWorkspaceIdentity(t, project.Slug, project.ID, markerForm)
		before := identitySnapshot(t, workspacePath)

		srv.syncHubManagedWorkspaceBack(ctx, &store.Agent{ID: "agent-1", ProjectID: project.ID}, "unused")

		assert.Equal(t, "synced", readFile(t, filepath.Join(workspacePath, "synced.txt")))
		assert.Equal(t, before, identitySnapshot(t, workspacePath))
	})
}

func TestHandleProjectCacheNotify_KeepsHubWorkspaceIdentity(t *testing.T) {
	identityTestHome(t)
	srv, st := testServer(t)
	ctx := context.Background()
	stubHubWorkspaceDownload(t)
	srv.SetStorage(newMockStorage("bucket"))

	project := sharedWorkspaceProject("cache-notify")
	require.NoError(t, st.CreateProject(ctx, project))
	workspacePath, _ := seedWorkspaceIdentity(t, project.Slug, project.ID, false)
	before := identitySnapshot(t, workspacePath)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/projects/"+project.ID+"/workspace/cache/notify", nil)
	rec := httptest.NewRecorder()
	srv.handleProjectCacheNotify(rec, req, project)

	assert.Equal(t, "synced", readFile(t, filepath.Join(workspacePath, "synced.txt")))
	assert.Equal(t, before, identitySnapshot(t, workspacePath))
}

// TestHubWorkspaceDownloads_UseIdentityKeepingHelper checks that every hub
// download of a workspace upload goes through syncHubWorkspaceFromGCS:
//   - the only reference to gcp.SyncFromGCS in the hub package is the
//     declaration of syncFromGCSIntoHubWorkspace;
//   - syncFromGCSIntoHubWorkspace is used only inside
//     syncHubWorkspaceFromGCS;
//   - syncHubWorkspaceFromGCS is called once from each of the four
//     functions that download a broker workspace upload into a hub
//     workspace, and from nowhere else.
func TestHubWorkspaceDownloads_UseIdentityKeepingHelper(t *testing.T) {
	files, err := filepath.Glob("*.go")
	require.NoError(t, err)
	fset := token.NewFileSet()
	var gcpRefs []string
	varUses := map[string]int{}
	helperCalls := map[string]int{}
	for _, file := range files {
		if strings.HasSuffix(file, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, file, nil, 0)
		require.NoError(t, err, file)

		// Package-level declarations outside functions.
		for _, decl := range f.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok {
				continue
			}
			ast.Inspect(gen, func(n ast.Node) bool {
				if sel, ok := n.(*ast.SelectorExpr); ok && sel.Sel.Name == "SyncFromGCS" {
					if pkg, ok := sel.X.(*ast.Ident); ok && pkg.Name == "gcp" {
						gcpRefs = append(gcpRefs, "decl")
					}
				}
				return true
			})
		}

		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				switch x := n.(type) {
				case *ast.SelectorExpr:
					if pkg, ok := x.X.(*ast.Ident); ok && pkg.Name == "gcp" && x.Sel.Name == "SyncFromGCS" {
						gcpRefs = append(gcpRefs, fn.Name.Name)
					}
					if x.Sel.Name == "syncHubWorkspaceFromGCS" {
						helperCalls[fn.Name.Name]++
					}
				case *ast.Ident:
					if x.Name == "syncFromGCSIntoHubWorkspace" {
						varUses[fn.Name.Name]++
					}
				}
				return true
			})
		}
	}
	assert.Equal(t, []string{"decl"}, gcpRefs, "gcp.SyncFromGCS references")
	assert.Equal(t, map[string]int{"syncHubWorkspaceFromGCS": 1}, varUses, "syncFromGCSIntoHubWorkspace uses")
	assert.Equal(t, map[string]int{
		"syncWorkspaceOnStop":           1,
		"syncHubManagedWorkspaceBack":   1,
		"refreshProjectCacheFromBroker": 1,
		"handleProjectCacheNotify":      1,
	}, helperCalls, "syncHubWorkspaceFromGCS callers")
}

func TestRecordHubWorkspace(t *testing.T) {
	identityTestHome(t)
	srv, st := testServer(t)
	ctx := context.Background()

	clone := sharedWorkspaceProject("own-clone")
	require.NoError(t, st.CreateProject(ctx, clone))
	_, _ = seedWorkspaceIdentity(t, clone.Slug, clone.ID, false)

	native := &store.Project{ID: api.NewUUID(), Name: "own-native", Slug: "own-native"}
	require.NoError(t, st.CreateProject(ctx, native))
	_, _ = seedWorkspaceIdentity(t, native.Slug, native.ID, true)

	worktree := &store.Project{
		ID: api.NewUUID(), Name: "own-worktree", Slug: "own-worktree",
		GitRemote: "github.com/example/own-worktree",
		Labels:    map[string]string{store.LabelWorkspaceMode: store.WorkspaceModeWorktreePerAgent},
	}
	require.NoError(t, st.CreateProject(ctx, worktree))
	_, _ = seedWorkspaceIdentity(t, worktree.Slug, worktree.ID, false)

	noWorkspace := sharedWorkspaceProject("no-workspace")
	require.NoError(t, st.CreateProject(ctx, noWorkspace))

	counts := srv.alignClonedProjectIdentities(ctx)
	assert.Equal(t, 2, counts.hubWorkspaceRecorded)
	assert.Zero(t, counts.hubWorkspaceRecordFailed)

	for _, p := range []*store.Project{clone, native} {
		path, err := config.HubWorkspaceRecordPath(p.Slug)
		require.NoError(t, err)
		got, err := config.ReadWorkspaceRecord(path)
		require.NoError(t, err, p.Slug)
		assert.Equal(t, p.ID, got, "record holds exactly the hub project ID")
	}
	for _, p := range []*store.Project{worktree, noWorkspace} {
		path, err := config.HubWorkspaceRecordPath(p.Slug)
		require.NoError(t, err)
		assert.NoFileExists(t, path, p.Slug)
	}
}

func TestCloneSharedWorkspaceProject_RecordsHubWorkspace(t *testing.T) {
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
	project := sharedWorkspaceProject("clone-record")
	project.Labels[store.LabelCloneURL] = sourceDir
	project.Labels[store.LabelDefaultBranch] = "master"

	require.NoError(t, srv.cloneSharedWorkspaceProject(context.Background(), project))

	path, err := config.HubWorkspaceRecordPath(project.Slug)
	require.NoError(t, err)
	got, err := config.ReadWorkspaceRecord(path)
	require.NoError(t, err)
	assert.Equal(t, project.ID, got)
}

// replaceScionEntry replaces the workspace .scion entry with one of the
// given form ("dir" with a project-id and another file, or "marker"), as a
// download carrying the other form would.
func replaceScionEntry(workspacePath, form string) error {
	scionPath := filepath.Join(workspacePath, config.DotScion)
	if err := os.RemoveAll(scionPath); err != nil {
		return err
	}
	if form == "marker" {
		return config.WriteProjectMarker(scionPath, &config.ProjectMarker{
			ProjectID: "copied-id", ProjectName: "copied", ProjectSlug: "copied",
		})
	}
	if err := os.MkdirAll(filepath.Join(scionPath, "templates"), 0755); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(scionPath, "templates", "t.yaml"), []byte("t"), 0644); err != nil {
		return err
	}
	return config.WriteProjectID(scionPath, "copied-id")
}

func TestSyncHubWorkspaceFromGCS_KeepsIdentityFormWhenDownloadChangesIt(t *testing.T) {
	for _, tc := range []struct {
		name, downloadForm string
		seed               func(t *testing.T, scionPath string)
	}{
		{"marker kept over downloaded directory", "dir", func(t *testing.T, p string) {
			require.NoError(t, config.WriteProjectMarker(p, &config.ProjectMarker{
				ProjectID: api.NewUUID(), ProjectName: "hub", ProjectSlug: "hub",
			}))
		}},
		{"directory kept over downloaded marker", "marker", func(t *testing.T, p string) {
			require.NoError(t, os.MkdirAll(p, 0755))
			require.NoError(t, config.WriteProjectID(p, api.NewUUID()))
		}},
		{"directory without project-id kept over downloaded marker", "marker", func(t *testing.T, p string) {
			require.NoError(t, os.MkdirAll(p, 0755))
		}},
		{"no entry kept over downloaded directory", "dir", func(t *testing.T, p string) {}},
		{"no entry kept over downloaded marker", "marker", func(t *testing.T, p string) {}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			identityTestHome(t)
			srv, _ := testServer(t)
			orig := syncFromGCSIntoHubWorkspace
			t.Cleanup(func() { syncFromGCSIntoHubWorkspace = orig })
			syncFromGCSIntoHubWorkspace = func(_ context.Context, _, _, workspacePath string) error {
				if err := os.WriteFile(filepath.Join(workspacePath, "synced.txt"), []byte("synced"), 0644); err != nil {
					return err
				}
				return replaceScionEntry(workspacePath, tc.downloadForm)
			}
			workspacePath := t.TempDir()
			tc.seed(t, filepath.Join(workspacePath, config.DotScion))
			before := identitySnapshot(t, workspacePath)

			require.NoError(t, srv.syncHubWorkspaceFromGCS(context.Background(), "bucket", "prefix", workspacePath))

			assert.Equal(t, before, identitySnapshot(t, workspacePath))
			assert.Equal(t, "synced", readFile(t, filepath.Join(workspacePath, "synced.txt")))
		})
	}
}

func TestSyncHubWorkspaceFromGCS_ReportsIdentityNotKept(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("a read-only directory does not stop removal when running as root")
	}
	identityTestHome(t)
	srv, _ := testServer(t)
	workspacePath := t.TempDir()
	scionPath := filepath.Join(workspacePath, config.DotScion)
	require.NoError(t, config.WriteProjectMarker(scionPath, &config.ProjectMarker{
		ProjectID: api.NewUUID(), ProjectName: "hub", ProjectSlug: "hub",
	}))

	// The download leaves a .scion directory that cannot be removed.
	locked := filepath.Join(scionPath, "locked")
	orig := syncFromGCSIntoHubWorkspace
	t.Cleanup(func() {
		syncFromGCSIntoHubWorkspace = orig
		_ = os.Chmod(locked, 0755)
	})
	syncFromGCSIntoHubWorkspace = func(_ context.Context, _, _, _ string) error {
		if err := os.Remove(scionPath); err != nil {
			return err
		}
		if err := os.MkdirAll(locked, 0755); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(locked, "f"), []byte("f"), 0644); err != nil {
			return err
		}
		return os.Chmod(locked, 0500)
	}

	err := srv.syncHubWorkspaceFromGCS(context.Background(), "bucket", "prefix", workspacePath)
	require.ErrorIs(t, err, errHubIdentityNotKept)
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
		{"project-id file, broker record present, upload", false, true, true},
		{"marker file, broker record present, no upload", true, true, false},
		{"project-id file, no upload", false, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			identityTestHome(t)
			hubSrv, st := testServer(t)
			ctx := context.Background()

			project := sharedWorkspaceProject("same-home")
			require.NoError(t, st.CreateProject(ctx, project))
			localID := api.NewUUID()
			workspacePath, previous := seedWorkspaceIdentity(t, project.Slug, localID, tc.markerForm)

			recorded, err := hubSrv.recordHubWorkspace(project)
			require.NoError(t, err)
			require.True(t, recorded)

			brokerRecord, err := config.BrokerWorkspaceRecordPath(project.Slug)
			require.NoError(t, err)
			if tc.brokerRecord {
				require.NoError(t, config.WriteWorkspaceRecord(brokerRecord, project.ID))
			}
			before := identitySnapshot(t, workspacePath)

			cfg := runtimebroker.DefaultServerConfig()
			cfg.BrokerID = "same-home-broker"
			cfg.BrokerName = "same-home-broker"
			cfg.StorageBucket = "bucket"
			brokerSrv := runtimebroker.New(cfg, &sameHomeManager{}, &runtime.MockRuntime{NameFunc: func() string { return "docker" }})
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

	res, err := alignWorkspaceProjectIdentity(workspacePath, project.Slug, project.ID, inUse)
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

	_, err := alignWorkspaceProjectIdentity(workspacePath, project.Slug, project.ID, notInUse)
	require.Error(t, err)
	assert.NotErrorIs(t, err, config.ErrInvalidProjectID)

	counts := srv.alignClonedProjectIdentities(ctx)
	assert.Equal(t, 1, counts.failed)
	assert.Zero(t, counts.skippedUnexpectedIdentity)
	assert.DirExists(t, previous)
}
