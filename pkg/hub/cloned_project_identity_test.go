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

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/projectkeys"
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
		ident, err := readWorkspaceIdentity(workspacePath)
		require.NoError(t, err)
		require.NotNil(t, ident)
		assert.Equal(t, project.ID, ident.id)
		assert.Equal(t, markerForm, ident.marker != nil)

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

		ident, err := readWorkspaceIdentity(workspacePath)
		require.NoError(t, err)
		assert.Equal(t, project.ID, ident.id)
		assert.Equal(t, markerForm, ident.marker != nil, "the identity keeps its on-disk form")

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
		ident, err := readWorkspaceIdentity(tc.ws)
		require.NoError(t, err, tc.name)
		assert.Equal(t, tc.id, ident.id, tc.name)
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

		ident, err := readWorkspaceIdentity(workspacePath)
		require.NoError(t, err)
		assert.Equal(t, localID, ident.id, "identity is kept while an agent is %s", phase)
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

	ident, err := readWorkspaceIdentity(workspacePath)
	require.NoError(t, err)
	assert.Equal(t, localID, ident.id, "identity is unchanged")
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

			// A directory where the recorded identity would point.
			named, err := config.ProjectMarker{ProjectID: tc.id, ProjectSlug: slug}.ExternalProjectPath()
			require.NoError(t, err)
			named = filepath.Dir(named)
			require.NoError(t, os.MkdirAll(named, 0755))
			require.NoError(t, os.WriteFile(filepath.Join(named, "file.txt"), []byte("other"), 0644))

			before := snapshotTree(t, home)

			counts := srv.alignClonedProjectIdentities(ctx)
			assert.Equal(t, 1, counts.skippedUnexpectedIdentity)
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
// download of a workspace upload goes through syncHubWorkspaceFromGCS: the
// only reference to gcp.SyncFromGCS in the hub package is the variable that
// helper calls.
func TestHubWorkspaceDownloads_UseIdentityKeepingHelper(t *testing.T) {
	files, err := filepath.Glob("*.go")
	require.NoError(t, err)
	fset := token.NewFileSet()
	var refs []string
	for _, file := range files {
		if strings.HasSuffix(file, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, file, nil, 0)
		require.NoError(t, err, file)
		ast.Inspect(f, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "SyncFromGCS" {
				return true
			}
			if pkg, ok := sel.X.(*ast.Ident); ok && pkg.Name == "gcp" {
				refs = append(refs, fset.Position(sel.Pos()).String())
			}
			return true
		})
	}
	require.Len(t, refs, 1, "gcp.SyncFromGCS references: %v", refs)
	assert.Contains(t, refs[0], "cloned_project_identity.go")
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
