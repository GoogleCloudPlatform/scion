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
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/projectfs"
	"github.com/GoogleCloudPlatform/scion/pkg/projectkeys"
	"github.com/GoogleCloudPlatform/scion/pkg/provision"
	"github.com/GoogleCloudPlatform/scion/pkg/secret"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Project storage (ptone/scion#5813, ptone/scion#5814): the hub's
// workspace Files tab, archive and pull resolve through the layout
// authority. On a shared backend the hub reads and writes the same
// projects/<uuid>/workspace tree agents mount (no legacy slug-keyed
// copy, no content probe); on the local backend (workstation server) the
// path is unchanged, ~/.scion/projects/<slug>. Project configuration is
// seeded at ProjectRoot(uuid)/.scion on shared backends.

// workspaceLayoutFixture is a hub with a fresh HOME and an NFS mount root
// whose share directory exists (the share is "mounted").
type workspaceLayoutFixture struct {
	srv       *Server
	s         store.Store
	home      string
	mountRoot string
	shareBase string
}

func newWorkspaceLayoutFixture(t *testing.T) workspaceLayoutFixture {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("K_SERVICE", "")
	t.Setenv("CLOUD_RUN_INSTANCE", "")
	srv, s := testServer(t)
	mountRoot := t.TempDir()
	shareBase := filepath.Join(mountRoot, "share1")
	require.NoError(t, os.MkdirAll(shareBase, 0o755))
	return workspaceLayoutFixture{srv: srv, s: s, home: home, mountRoot: mountRoot, shareBase: shareBase}
}

func (f workspaceLayoutFixture) useNFS() {
	f.srv.config.ProjectStorageConfig = projectStorageFromWorkspace(nfsConfig(f.mountRoot))
}

func workspaceFilesURL(projectID string) string {
	return fmt.Sprintf("/api/v1/projects/%s/workspace/files", projectID)
}

func listWorkspaceFiles(t *testing.T, srv *Server, projectID string) []string {
	t.Helper()
	rec := doRequest(t, srv, http.MethodGet, workspaceFilesURL(projectID), nil)
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	var resp ProjectWorkspaceListResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	var names []string
	for _, f := range resp.Files {
		names = append(names, f.Path)
	}
	return names
}

// TestProjectWorkspaceFiles_NFS_UUIDTree pins the Cloud Run/GKE fix for the workspace:
// with server.workspace_storage: nfs, a web upload lands in
// <mount_root>/<share>/projects/<uuid>/workspace (where agents mount it),
// a file an agent writes there is listed, and nothing is written under a
// legacy slug-keyed tree on the share or the hub's local slug path.
func TestProjectWorkspaceFiles_NFS_UUIDTree(t *testing.T) {
	f := newWorkspaceLayoutFixture(t)
	f.useNFS()
	t.Setenv("K_SERVICE", "scion-hub") // a Cloud Run Service: the write gate passes on NFS
	project, _ := createTestHubManagedProject(t, f.srv, "UUID Workspace")

	rec := doMultipartRequest(t, f.srv, http.MethodPost, workspaceFilesURL(project.ID),
		map[string][]byte{"from-web.txt": []byte("web")})
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

	ws := filepath.Join(f.shareBase, "projects", project.ID, "workspace")
	got, err := os.ReadFile(filepath.Join(ws, "from-web.txt"))
	require.NoError(t, err, "the upload lands in the uuid tree agents mount")
	assert.Equal(t, "web", string(got))

	require.NoError(t, os.WriteFile(filepath.Join(ws, "from-agent.txt"), []byte("agent"), 0o644))
	assert.ElementsMatch(t, []string{"from-web.txt", "from-agent.txt"}, listWorkspaceFiles(t, f.srv, project.ID))

	entries, err := os.ReadDir(f.shareBase)
	require.NoError(t, err)
	for _, e := range entries {
		assert.Equal(t, "projects", e.Name(), "the share holds only the uuid tree (no legacy slug-keyed copy)")
	}
	_, err = os.Stat(filepath.Join(f.home, ".scion", "projects", project.Slug, "from-web.txt"))
	assert.True(t, os.IsNotExist(err), "nothing is written to the hub's local slug path")
}

// TestProjectWorkspaceFiles_Local_SlugPathUnchanged is the workstation regression
// check: with no workspace storage configured the Files tab serves
// ~/.scion/projects/<slug>, exactly as before.
func TestProjectWorkspaceFiles_Local_SlugPathUnchanged(t *testing.T) {
	f := newWorkspaceLayoutFixture(t)
	project, workspacePath := createTestHubManagedProject(t, f.srv, "Local Workspace")
	require.Equal(t, filepath.Join(f.home, ".scion", "projects", project.Slug), workspacePath)

	rec := doMultipartRequest(t, f.srv, http.MethodPost, workspaceFilesURL(project.ID),
		map[string][]byte{"local.txt": []byte("local")})
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	got, err := os.ReadFile(filepath.Join(workspacePath, "local.txt"))
	require.NoError(t, err)
	assert.Equal(t, "local", string(got))
	_, err = os.Stat(filepath.Join(workspacePath, ".scion", "settings.yaml"))
	assert.NoError(t, err, "the local backend seeds .scion inside the workspace, as before")
}

// TestProjectWorkspaceFiles_VolumeBackends_UUIDTree: the platform volume
// backends resolve the same uuid tree under /mnt/<volume>/<subpath_root>.
func TestProjectWorkspaceFiles_VolumeBackends_UUIDTree(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  *config.V1WorkspaceStorageConfig
	}{
		{"cloudrun-volume", &config.V1WorkspaceStorageConfig{Backend: "cloudrun-volume", CloudRunVolume: &config.V1CloudRunVolumeConfig{VolumeName: "ws"}}},
		{"gke-shared-volume", &config.V1WorkspaceStorageConfig{Backend: "gke-shared-volume", GKESharedVolume: &config.V1GKESharedVolumeConfig{VolumeName: "ws", SubPathRoot: "tree"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newWorkspaceLayoutFixture(t)
			base := t.TempDir()
			setVolumeMountBase(t, base)
			require.NoError(t, os.MkdirAll(filepath.Join(base, "ws"), 0o755))
			f.srv.config.ProjectStorageConfig = projectStorageFromWorkspace(tc.cfg)
			project, _ := createTestHubManagedProject(t, f.srv, "Volume "+tc.name)

			res, err := f.srv.projectWorkspaceResolved(project)
			require.NoError(t, err)
			root := config.SubPathRootOrDefault("")
			if tc.cfg.GKESharedVolume != nil {
				root = "tree"
			}
			assert.Equal(t, filepath.Join(base, "ws", root, project.ID, "workspace"), res.HostPath)
			assert.True(t, res.Backend.Durable())

			rec := doRequest(t, f.srv, http.MethodPut, workspaceFilesURL(project.ID)+"/v.txt", ProjectWorkspaceWriteRequest{Content: "v"})
			require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
			_, err = os.Stat(filepath.Join(res.HostPath, "v.txt"))
			assert.NoError(t, err)
		})
	}
}

// TestProjectWorkspaceFiles_VolumeBackendWithoutName_FailsClosed: a volume
// backend with no volume name has no mount point; the hub never falls back
// to instance-local disk (ptone/scion#1073): the Files tab answers 503 and
// nothing is created locally.
func TestProjectWorkspaceFiles_VolumeBackendWithoutName_FailsClosed(t *testing.T) {
	f := newWorkspaceLayoutFixture(t)
	project, workspacePath := createTestHubManagedProject(t, f.srv, "Unnamed Volume")
	require.NoError(t, os.RemoveAll(workspacePath))
	f.srv.config.ProjectStorageConfig = &config.V1ProjectStorageConfig{Backend: "cloudrun-volume"}

	for _, method := range []string{http.MethodGet, http.MethodPost} {
		var code int
		if method == http.MethodGet {
			code = doRequest(t, f.srv, method, workspaceFilesURL(project.ID), nil).Code
		} else {
			code = doMultipartRequest(t, f.srv, method, workspaceFilesURL(project.ID), map[string][]byte{"a.txt": []byte("a")}).Code
		}
		assert.Equal(t, http.StatusServiceUnavailable, code, method)
	}
	_, err := os.Stat(workspacePath)
	assert.True(t, os.IsNotExist(err), "nothing is created on instance-local disk")
}

// TestProjectWorkspaceFiles_EmptyPerAgent_Conflict: an empty-per-agent
// project has no project-scoped workspace (projectfs.ErrNoMount), so the
// Files tab answers 409 server-side instead of serving a directory no agent
// mounts.
func TestProjectWorkspaceFiles_EmptyPerAgent_Conflict(t *testing.T) {
	f := newWorkspaceLayoutFixture(t)
	project, _ := createTestHubManagedProject(t, f.srv, "Empty Per Agent")
	project.Labels = map[string]string{store.LabelWorkspaceMode: string(store.SharingModeEmptyPerAgent)}
	require.NoError(t, f.s.UpdateProject(context.Background(), project))
	require.True(t, project.IsEmptyPerAgent(), "fixture check")

	_, err := f.srv.projectWorkspaceResolved(project)
	require.ErrorIs(t, err, projectfs.ErrNoMount)

	for _, url := range []string{workspaceFilesURL(project.ID), fmt.Sprintf("/api/v1/projects/%s/workspace/archive", project.ID)} {
		rec := doRequest(t, f.srv, http.MethodGet, url, nil)
		assert.Equal(t, http.StatusConflict, rec.Code, "%s: %s", url, rec.Body.String())
		assert.Contains(t, rec.Body.String(), "each agent gets its own private workspace directory")
	}
}

// TestProjectWorkspaceFiles_RemoteLinkedProject_ConflictNoCache: a linked
// project served only by a remote broker has no workspace the hub can
// reach; the Files tab answers 409 and the hub keeps no cached copy.
func TestProjectWorkspaceFiles_RemoteLinkedProject_ConflictNoCache(t *testing.T) {
	f := newWorkspaceLayoutFixture(t)
	project := createTestGitProject(t, f.srv, "Remote Linked WS", "github.com/test/remote-linked-ws")
	addProviderBroker(t, f.s, project.ID, "remote")

	for _, shared := range []bool{false, true} {
		if shared {
			f.useNFS()
		}
		rec := doRequest(t, f.srv, http.MethodGet, workspaceFilesURL(project.ID), nil)
		require.Equal(t, http.StatusConflict, rec.Code, "shared=%v body: %s", shared, rec.Body.String())
		assert.Contains(t, rec.Body.String(), "remote runtime brokers without shared storage")
	}
	_, err := os.Stat(filepath.Join(f.home, ".scion", "projects", project.Slug))
	assert.True(t, os.IsNotExist(err), "no hub-side cache directory is created")
	for _, rt := range []struct{ method, route string }{
		{http.MethodPost, "workspace/cache/refresh"},
		{http.MethodGet, "workspace/cache/status"},
		{http.MethodPost, "workspace/cache/notify"},
		{http.MethodGet, "sync/status"},
	} {
		rec := doRequest(t, f.srv, rt.method, fmt.Sprintf("/api/v1/projects/%s/%s", project.ID, rt.route), nil)
		assert.Equal(t, http.StatusNotFound, rec.Code, "the linked-project cache route %s %s is gone: %s", rt.method, rt.route, rec.Body.String())
	}
}

// TestProjectWorkspaceFiles_EmbeddedLinkedProject_ProviderPath: a linked
// project with a co-located (embedded) broker keeps serving the broker's
// local path.
func TestProjectWorkspaceFiles_EmbeddedLinkedProject_ProviderPath(t *testing.T) {
	for _, scionDir := range []bool{false, true} {
		t.Run(fmt.Sprintf("scionDir=%t", scionDir), func(t *testing.T) {
			f := newWorkspaceLayoutFixture(t)
			project := createTestGitProject(t, f.srv, "Embedded Linked WS", "github.com/test/embedded-linked-ws")
			b := addProviderBroker(t, f.s, project.ID, "embedded")
			f.srv.SetEmbeddedBrokerID(b.ID)
			checkout := t.TempDir()
			localPath := checkout
			if scionDir {
				localPath = filepath.Join(checkout, config.DotScion) + string(os.PathSeparator)
			}
			require.NoError(t, f.s.AddProjectProvider(context.Background(), &store.ProjectProvider{
				ProjectID: project.ID, BrokerID: b.ID, BrokerName: b.Name, Status: b.Status, LocalPath: localPath,
			}))
			require.NoError(t, os.WriteFile(filepath.Join(checkout, "main.go"), []byte("package main"), 0o644))

			assert.Equal(t, []string{"main.go"}, listWorkspaceFiles(t, f.srv, project.ID))
		})
	}
}

func TestProjectWorkspaceFiles_GlobalProject_ProviderPath(t *testing.T) {
	f := newWorkspaceLayoutFixture(t)
	project := createTestGitProject(t, f.srv, "Global WS", "github.com/test/global-ws")
	project.Slug = config.GlobalProjectSlug
	b := addProviderBroker(t, f.s, project.ID, "embedded")
	f.srv.SetEmbeddedBrokerID(b.ID)
	globalDir := filepath.Join(f.home, config.DotScion)
	require.NoError(t, f.s.AddProjectProvider(context.Background(), &store.ProjectProvider{
		ProjectID: project.ID, BrokerID: b.ID, BrokerName: b.Name, Status: b.Status, LocalPath: globalDir,
	}))
	resolved, err := f.srv.resolveProjectWorkspace(context.Background(), project)
	require.NoError(t, err)
	assert.Equal(t, globalDir, resolved.HostPath)
}

// TestInitHubManagedProject_SharedBackend_SeedsProjectRoot pins the hub
// side of the project configuration: on a shared backend the hub seeds the project
// configuration (settings with the hub connection and project ID) at
// projects/<uuid>/.scion beside workspace/, inside no Mount, and writes no
// .scion into the workspace agents mount.
func TestInitHubManagedProject_SharedBackend_SeedsProjectRoot(t *testing.T) {
	f := newWorkspaceLayoutFixture(t)
	f.useNFS()
	project, _ := createTestHubManagedProject(t, f.srv, "Seeded Root")

	cfgDir := filepath.Join(f.shareBase, "projects", project.ID, ".scion")
	data, err := os.ReadFile(filepath.Join(cfgDir, "settings.yaml"))
	require.NoError(t, err, "the project configuration is seeded at the project root")
	assert.Contains(t, string(data), project.ID, "the seeded settings carry the hub project ID")
	info, err := os.Lstat(cfgDir)
	require.NoError(t, err)
	assert.True(t, info.IsDir())
	assert.Zero(t, info.Mode().Perm()&0o022, "the configuration directory is not group/other-writable")

	_, err = os.Stat(filepath.Join(f.shareBase, "projects", project.ID, "workspace", ".scion"))
	assert.True(t, os.IsNotExist(err), "no .scion is created inside the workspace agents mount")
	_, err = os.Stat(filepath.Join(f.home, ".scion", "projects", project.Slug))
	assert.True(t, os.IsNotExist(err), "nothing is seeded on the hub's local disk")
	_, err = os.Stat(filepath.Join(f.home, ".scion", config.ProjectConfigsDir))
	assert.True(t, os.IsNotExist(err), "the hub connection settings are not redirected to an external project config directory on the hub's disk")

	// The identity files beside settings.yaml: project-id and project-slug,
	// one line each, mode 0644.
	for name, want := range map[string]string{projectkeys.ProjectIDFile: project.ID, projectkeys.ProjectSlugFile: project.Slug} {
		data, err := os.ReadFile(filepath.Join(cfgDir, name))
		require.NoError(t, err, name)
		assert.Equal(t, want+"\n", string(data), name)
		info, err := os.Lstat(filepath.Join(cfgDir, name))
		require.NoError(t, err)
		assert.Equal(t, os.FileMode(0o644), info.Mode().Perm(), name)
	}
}

// TestProjectSlugRename_SharedBackend_UpdatesProjectSlugFile: renaming a
// project's slug on a shared backend rewrites <ProjectRoot>/.scion/
// project-slug; nothing slug-keyed moves.
func TestProjectSlugRename_SharedBackend_UpdatesProjectSlugFile(t *testing.T) {
	f := newWorkspaceLayoutFixture(t)
	f.useNFS()
	project, _ := createTestHubManagedProject(t, f.srv, "Slug Before")
	slugFile := filepath.Join(f.shareBase, "projects", project.ID, ".scion", projectkeys.ProjectSlugFile)
	data, err := os.ReadFile(slugFile)
	require.NoError(t, err)
	require.Equal(t, project.Slug+"\n", string(data), "fixture check")

	oldSlug := project.Slug
	project.Slug = "slug-after"
	f.srv.migrateProjectSlug(context.Background(), project, oldSlug)

	data, err = os.ReadFile(slugFile)
	require.NoError(t, err)
	assert.Equal(t, "slug-after\n", string(data))
	_, err = os.Stat(filepath.Join(f.home, ".scion", "projects", "slug-after"))
	assert.True(t, os.IsNotExist(err), "nothing slug-keyed is created on the hub's local disk")

	// A project without a project configuration directory yet (created
	// before the upgrade, not migrated) gets none from a rename.
	other := &store.Project{ID: "aaaaaaaa-0000-4000-8000-0000000000c1", Name: "Legacy", Slug: "legacy-old"}
	other.Slug = "legacy-new"
	f.srv.migrateProjectSlug(context.Background(), other, "legacy-old")
	_, err = os.Stat(filepath.Join(f.shareBase, "projects", other.ID))
	assert.True(t, os.IsNotExist(err), "a rename never creates the project configuration")
}

// TestProjectSlugFile_LocalBackendUnaffected: on the local backend the seed
// keeps its existing form (no project-id / project-slug files are added).
func TestProjectSlugFile_LocalBackendUnaffected(t *testing.T) {
	f := newWorkspaceLayoutFixture(t)
	_, workspacePath := createTestHubManagedProject(t, f.srv, "Local Slug")
	for _, name := range []string{projectkeys.ProjectIDFile, projectkeys.ProjectSlugFile} {
		_, err := os.Stat(filepath.Join(workspacePath, ".scion", name))
		assert.True(t, os.IsNotExist(err), "%s is not written on the local backend", name)
	}
}

// TestCreateProject_SharedBackendUnmounted_RollsBack: a shared backend whose
// share is not mounted cannot hold the project configuration; the create is
// rolled back with 503 rather than seeding instance-local disk.
func TestCreateProject_SharedBackendUnmounted_RollsBack(t *testing.T) {
	f := newWorkspaceLayoutFixture(t)
	f.srv.config.ProjectStorageConfig = projectStorageFromWorkspace(nfsConfig(filepath.Join(f.home, "not-mounted")))

	rec := doRequest(t, f.srv, http.MethodPost, "/api/v1/projects", CreateProjectRequest{Name: "Unmounted Share"})
	require.Equal(t, http.StatusServiceUnavailable, rec.Code, "body: %s", rec.Body.String())
	result, err := f.s.ListProjects(context.Background(), store.ProjectFilter{}, store.ListOptions{})
	require.NoError(t, err)
	for _, p := range result.Items {
		assert.NotEqual(t, "Unmounted Share", p.Name, "the project is rolled back")
	}
}

// TestAlignClonedProjectIdentities_SharedBackendLeavesWorkspaceScion: on a
// shared backend a .scion in the workspace is repository content;
// the hub's cloned-identity alignment does not run there and
// never rewrites it.
func TestAlignClonedProjectIdentities_SharedBackendLeavesWorkspaceScion(t *testing.T) {
	f := newWorkspaceLayoutFixture(t)
	f.useNFS()
	project := &store.Project{
		ID: "aaaaaaaa-0000-4000-8000-0000000000b1", Name: "Repo Scion", Slug: "repo-scion",
		GitRemote: "github.com/test/repo-scion", Labels: map[string]string{store.LabelWorkspaceMode: store.WorkspaceModeShared},
	}
	require.NoError(t, f.s.CreateProject(context.Background(), project))
	wsScion := filepath.Join(f.shareBase, "projects", project.ID, "workspace", ".scion")
	require.NoError(t, os.MkdirAll(wsScion, 0o755))
	repoID := "bbbbbbbb-0000-4000-8000-0000000000b2"
	require.NoError(t, config.WriteProjectID(wsScion, repoID))

	counts := f.srv.alignClonedProjectIdentities(context.Background())
	assert.Zero(t, counts.aligned)
	got, err := config.ReadProjectID(wsScion)
	require.NoError(t, err)
	assert.Equal(t, repoID, got, "a repository .scion never takes the hub's identity on a shared backend")
}

// TestProjectWorkspaceFiles_MigrationNoticeAbsentByDefault: the list
// response carries no migrationNotice when there is nothing to migrate.
func TestProjectWorkspaceFiles_MigrationNoticeAbsentByDefault(t *testing.T) {
	f := newWorkspaceLayoutFixture(t)
	project, _ := createTestHubManagedProject(t, f.srv, "No Notice")
	rec := doRequest(t, f.srv, http.MethodGet, workspaceFilesURL(project.ID), nil)
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	assert.NotContains(t, rec.Body.String(), "migrationNotice")
}

// TestEnsureProjectConfigIdentities_StartupPass pins the hub-start pass:
// for an existing shared-backend project with no
// <ProjectRoot>/.scion it creates project-id and project-slug; a second
// pass changes nothing; a stale project-slug is rewritten to the current
// slug.
func TestEnsureProjectConfigIdentities_StartupPass(t *testing.T) {
	f := newWorkspaceLayoutFixture(t)
	f.useNFS()
	project := &store.Project{ID: "aaaaaaaa-0000-4000-8000-0000000000d1", Name: "Existing", Slug: "existing"}
	require.NoError(t, f.s.CreateProject(context.Background(), project))
	cfgDir := filepath.Join(f.shareBase, "projects", project.ID, ".scion")
	_, err := os.Stat(cfgDir)
	require.True(t, os.IsNotExist(err), "fixture check: no project configuration yet")

	counts := f.srv.ensureProjectConfigIdentities(context.Background())
	assert.Equal(t, 1, counts.ensured)
	assert.Zero(t, counts.mismatched+counts.failed+counts.notReached)
	read := func(name string) string {
		data, err := os.ReadFile(filepath.Join(cfgDir, name))
		require.NoError(t, err, name)
		return string(data)
	}
	assert.Equal(t, project.ID+"\n", read(projectkeys.ProjectIDFile))
	assert.Equal(t, "existing\n", read(projectkeys.ProjectSlugFile))

	counts = f.srv.ensureProjectConfigIdentities(context.Background())
	assert.Equal(t, 1, counts.ensured, "idempotent")
	assert.Equal(t, project.ID+"\n", read(projectkeys.ProjectIDFile))

	require.NoError(t, os.WriteFile(filepath.Join(cfgDir, projectkeys.ProjectSlugFile), []byte("stale\n"), 0o644))
	f.srv.ensureProjectConfigIdentities(context.Background())
	assert.Equal(t, "existing\n", read(projectkeys.ProjectSlugFile), "project-slug is content: rewritten to the current slug")

	checks := map[string]string{}
	f.srv.checkProjectConfigIdentityHealth(checks)
	assert.Equal(t, HealthStatusHealthy, checks[projectConfigIdentityHealthKey])
}

// TestEnsureProjectConfigIdentities_MismatchLeftAndReported: a project-id
// naming another project is never overwritten; it is reported (counts,
// degraded health check naming the project), and project-slug is left
// alone too.
func TestEnsureProjectConfigIdentities_MismatchLeftAndReported(t *testing.T) {
	f := newWorkspaceLayoutFixture(t)
	f.useNFS()
	project := &store.Project{ID: "aaaaaaaa-0000-4000-8000-0000000000d2", Name: "Mismatch", Slug: "mismatch"}
	require.NoError(t, f.s.CreateProject(context.Background(), project))
	cfgDir := filepath.Join(f.shareBase, "projects", project.ID, ".scion")
	require.NoError(t, os.MkdirAll(cfgDir, 0o755))
	other := "bbbbbbbb-0000-4000-8000-0000000000d3"
	require.NoError(t, os.WriteFile(filepath.Join(cfgDir, projectkeys.ProjectIDFile), []byte(other+"\n"), 0o644))

	counts := f.srv.ensureProjectConfigIdentities(context.Background())
	assert.Equal(t, 1, counts.mismatched)
	assert.Zero(t, counts.ensured)
	data, err := os.ReadFile(filepath.Join(cfgDir, projectkeys.ProjectIDFile))
	require.NoError(t, err)
	assert.Equal(t, other+"\n", string(data), "a differing project-id is never overwritten")
	_, err = os.Stat(filepath.Join(cfgDir, projectkeys.ProjectSlugFile))
	assert.True(t, os.IsNotExist(err), "nothing else is written for a mismatched project")

	checks := map[string]string{}
	f.srv.checkProjectConfigIdentityHealth(checks)
	assert.Equal(t, HealthStatusDegraded+": project-id mismatch in 1 project configuration(s)", checks[projectConfigIdentityHealthKey],
		"the unauthenticated health check carries a count only")
	assert.NotContains(t, checks[projectConfigIdentityHealthKey], project.ID, "no project ID on the health endpoint")
	assert.NotContains(t, checks[projectConfigIdentityHealthKey], f.shareBase, "no host path")
	assert.Equal(t, HealthStatusDegraded, deriveHealthStatus(checks), "a mismatch degrades, never fails, health")
}

// TestEnsureProjectConfigIdentities_LocalBackendUnaffected: on the local
// backend the pass does nothing and adds no health check.
func TestEnsureProjectConfigIdentities_LocalBackendUnaffected(t *testing.T) {
	f := newWorkspaceLayoutFixture(t)
	_, workspacePath := createTestHubManagedProject(t, f.srv, "Local Identity")
	counts := f.srv.ensureProjectConfigIdentities(context.Background())
	assert.Equal(t, projectConfigIdentityCounts{}, counts)
	_, err := os.Stat(filepath.Join(workspacePath, ".scion", projectkeys.ProjectSlugFile))
	assert.True(t, os.IsNotExist(err))
	checks := map[string]string{}
	f.srv.checkProjectConfigIdentityHealth(checks)
	_, ok := checks[projectConfigIdentityHealthKey]
	assert.False(t, ok, "no identity check on the local backend")
}

// newSourceRepo makes a local git repository with one commit (README.md)
// on branch main, usable as a project's clone-url label.
func newSourceRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1",
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, "git %v: %s", args, out)
	}
	run("init", "-q", "-b", "main")
	require.NoError(t, os.WriteFile(filepath.Join(dir, "README.md"), []byte("hello\n"), 0o644))
	run("add", "README.md")
	run("-c", "commit.gpgsign=false", "commit", "-q", "-m", "init")
	return dir
}

// cloneLocalFixture makes the hub's shared-workspace provisioning clone the
// local repository src in place of the project's https clone URL, which the
// URL check requires (and a hardened clone refuses local paths). It records
// every provisioning input the hub passed and returns them.
func cloneLocalFixture(t *testing.T, src string) *[]provision.ProvisionInput {
	t.Helper()
	var seen []provision.ProvisionInput
	orig := provisionSharedClone
	provisionSharedClone = func(in provision.ProvisionInput) error {
		seen = append(seen, in)
		if in.GitClone != nil {
			gc := *in.GitClone
			gc.URL = src
			in.GitClone = &gc
		}
		in.HostGitHardened = false
		return orig(in)
	}
	t.Cleanup(func() { provisionSharedClone = orig })
	return &seen
}

// TestCreateSharedWorkspaceProject_SharedBackend_HubProvisionsUUIDWorkspace
// pins hub-side provisioning: on a shared backend the hub
// provisions the uuid workspace at project create through
// provision.ProvisionShared with the init container's layout (sentinel in
// <ProjectRoot>/provision), so a Docker or co-located broker's first agent
// finds a cloned workspace; nothing is seeded into the workspace; and an
// existing sentinel means no second clone.
func TestCreateSharedWorkspaceProject_SharedBackend_HubProvisionsUUIDWorkspace(t *testing.T) {
	f := newWorkspaceLayoutFixture(t)
	f.useNFS()
	src := newSourceRepo(t)
	seen := cloneLocalFixture(t, src)
	const cloneURL = "https://git.example.invalid/test/shared-git-nfs.git"

	rec := doRequest(t, f.srv, http.MethodPost, "/api/v1/projects", CreateProjectRequest{
		Name: "Shared Git NFS", GitRemote: "github.com/test/shared-git-nfs", WorkspaceMode: store.WorkspaceModeShared,
		Labels: map[string]string{store.LabelCloneURL: cloneURL, store.LabelDefaultBranch: "main"},
	})
	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())
	var project store.Project
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&project))
	require.True(t, project.IsSharedWorkspace(), "fixture check")
	require.Len(t, *seen, 1)
	assert.True(t, (*seen)[0].HostGitHardened, "the hub clones with host-git hardening")
	require.NotNil(t, (*seen)[0].GitClone)
	assert.Equal(t, cloneURL, (*seen)[0].GitClone.URL, "the checked URL is the one cloned")

	projectRoot := filepath.Join(f.shareBase, "projects", project.ID)
	ws := filepath.Join(projectRoot, "workspace")
	assert.FileExists(t, filepath.Join(ws, "README.md"), "the hub cloned into the uuid workspace agents mount")
	assert.DirExists(t, filepath.Join(ws, ".git"))
	assert.FileExists(t, filepath.Join(projectRoot, provision.ProvisionStateDirName, provision.ProvisionSentinelFile),
		"the sentinel is where the init container and brokers look, so they take the reuse path")
	_, err := os.Stat(filepath.Join(ws, ".scion"))
	assert.True(t, os.IsNotExist(err), "nothing is seeded into the workspace")
	assert.FileExists(t, filepath.Join(projectRoot, ".scion", "settings.yaml"), "the project configuration is at the project root")

	// The co-located (embedded) broker's first agent is given the
	// provisioned workspace.
	b := addProviderBroker(t, f.s, project.ID, "embedded")
	f.srv.SetEmbeddedBrokerID(b.ID)
	agent := &store.Agent{ID: "aaaaaaaa-0000-4000-8000-0000000000e1", ProjectID: project.ID, RuntimeBrokerID: b.ID, AppliedConfig: &store.AgentAppliedConfig{}}
	require.NoError(t, f.srv.populateAgentConfig(context.Background(), agent, &project, nil))
	assert.Equal(t, ws, agent.AppliedConfig.Workspace)

	// An existing sentinel: provisioning again changes nothing (no re-clone).
	require.NoError(t, os.Remove(filepath.Join(ws, "README.md")))
	require.NoError(t, f.srv.provisionSharedWorkspace(context.Background(), &project))
	_, err = os.Stat(filepath.Join(ws, "README.md"))
	assert.True(t, os.IsNotExist(err), "a provisioned workspace is never cloned again")
}

// TestCreateSharedWorkspaceProject_SharedBackend_CloneFailureRollsBack: a
// clone that fails rolls the create back with a fixed-text error (no host
// path) and removes the project tree the create made.
func TestCreateSharedWorkspaceProject_SharedBackend_CloneFailureRollsBack(t *testing.T) {
	f := newWorkspaceLayoutFixture(t)
	f.useNFS()
	missing := filepath.Join(t.TempDir(), "no-such-repo")
	cloneLocalFixture(t, missing)

	rec := doRequest(t, f.srv, http.MethodPost, "/api/v1/projects", CreateProjectRequest{
		Name: "Bad Clone NFS", GitRemote: "github.com/test/bad-clone-nfs", WorkspaceMode: store.WorkspaceModeShared,
		Labels: map[string]string{store.LabelCloneURL: "https://git.example.invalid/test/bad-clone-nfs.git"},
	})
	require.Equal(t, http.StatusUnprocessableEntity, rec.Code,
		"a missing clone source is a not-found git error (422), body: %s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), "clone_failed")
	assert.NotContains(t, rec.Body.String(), f.shareBase, "no host path in the error")
	assert.NotContains(t, rec.Body.String(), missing, "no clone source path in the error")
	result, err := f.s.ListProjects(context.Background(), store.ProjectFilter{}, store.ListOptions{})
	require.NoError(t, err)
	for _, p := range result.Items {
		assert.NotEqual(t, "Bad Clone NFS", p.Name, "the project row is rolled back")
	}
	// Provisioning created projects/<id> before the clone ran; the rollback
	// removes exactly that tree and leaves projects/ itself in place.
	entries, err := os.ReadDir(filepath.Join(f.shareBase, "projects"))
	require.NoError(t, err, "projects/ was created by provisioning")
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	assert.Empty(t, names, "the failed create leaves no projects/<id> tree")
}

// TestImportFromWorkspace_SymlinksAreNotFollowed:
// resource import reads the workspace through a confined root: a templates
// directory that is a symlink leading out of the workspace is refused, and a
// symlinked file inside a template is not followed (here the template's only
// scion-agent.yaml is a symlink to a file outside, so nothing is found).
func TestImportFromWorkspace_SymlinksAreNotFollowed(t *testing.T) {
	srv, _, project, wsRoot := setupWorkspaceProject(t, "ws-symlink")
	ctx := context.Background()
	outside := t.TempDir()
	outTmpl := filepath.Join(outside, "templates", "leaked")
	require.NoError(t, os.MkdirAll(outTmpl, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(outTmpl, "scion-agent.yaml"), []byte("harness: claude\n"), 0o644))

	require.NoError(t, os.MkdirAll(filepath.Join(wsRoot, ".scion"), 0o755))
	require.NoError(t, os.Symlink(filepath.Join(outside, "templates"), filepath.Join(wsRoot, ".scion", "templates")))
	_, err := srv.importTemplatesFromWorkspace(ctx, project, "/.scion/templates")
	require.Error(t, err, "a symlinked templates directory leading out of the workspace is refused")
	assert.NotContains(t, err.Error(), outside, "no host path in the error")
	assert.NotContains(t, err.Error(), wsRoot, "no host path in the error")
	_, _, err = srv.discoverFromWorkspace(ctx, project, "/.scion/templates", srv.templateImportKind())
	require.Error(t, err)

	inner := filepath.Join(wsRoot, "templates", "linked")
	require.NoError(t, os.MkdirAll(inner, 0o755))
	require.NoError(t, os.Symlink(filepath.Join(outTmpl, "scion-agent.yaml"), filepath.Join(inner, "scion-agent.yaml")))
	_, err = srv.importTemplatesFromWorkspace(ctx, project, "/templates")
	require.Error(t, err, "a symlinked file inside the workspace is not followed")
	assert.Contains(t, err.Error(), "no scion templates found")
}

// TestProjectWorkspacePull_ConfinedAndNoHostPaths:
// pull confirms the workspace through the confined open before git runs (a
// workspace replaced by a symlink is refused), and a failing pull answers
// fixed text without host paths.
func TestProjectWorkspacePull_ConfinedAndNoHostPaths(t *testing.T) {
	f := newWorkspaceLayoutFixture(t)
	f.useNFS()
	src := newSourceRepo(t)
	cloneLocalFixture(t, src)
	rec := doRequest(t, f.srv, http.MethodPost, "/api/v1/projects", CreateProjectRequest{
		Name: "Pull NFS", GitRemote: "github.com/test/pull-nfs", WorkspaceMode: store.WorkspaceModeShared,
		Labels: map[string]string{store.LabelCloneURL: "https://git.example.invalid/test/pull-nfs.git", store.LabelDefaultBranch: "main"},
	})
	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())
	var project store.Project
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&project))
	pullURL := fmt.Sprintf("/api/v1/projects/%s/workspace/pull", project.ID)
	ws := filepath.Join(f.shareBase, "projects", project.ID, "workspace")

	// The source repository is gone: the pull fails with fixed text.
	require.NoError(t, os.RemoveAll(src))
	rec = doRequest(t, f.srv, http.MethodPost, pullURL, nil)
	require.GreaterOrEqual(t, rec.Code, 400, "body: %s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), "git pull failed")
	for _, p := range []string{ws, src, f.shareBase} {
		assert.NotContains(t, rec.Body.String(), p, "no host path in the pull error")
	}

	// The workspace replaced by a symlink leading elsewhere is refused.
	elsewhere := t.TempDir()
	require.NoError(t, os.RemoveAll(ws))
	require.NoError(t, os.Symlink(elsewhere, ws))
	rec = doRequest(t, f.srv, http.MethodPost, pullURL, nil)
	assert.Equal(t, http.StatusBadRequest, rec.Code, "body: %s", rec.Body.String())
	assert.NotContains(t, rec.Body.String(), elsewhere)
	assert.NotContains(t, rec.Body.String(), f.shareBase)
}

// unusableWorkspaceStorage is a shared workspace backend the hub cannot
// build a Layout for (a volume backend with no volume name,
// ptone/scion#1073): every resolution fails closed.
func unusableWorkspaceStorage() *config.V1WorkspaceStorageConfig {
	return &config.V1WorkspaceStorageConfig{Backend: "cloudrun-volume"}
}

// TestCreateAgent_UnusableSharedStorage_503NoRowNoDispatch: an agent
// create that cannot resolve the project workspace answers 503 (no host path), creates no row and dispatches nothing.
func TestCreateAgent_UnusableSharedStorage_503NoRowNoDispatch(t *testing.T) {
	f := newWorkspaceLayoutFixture(t)
	ctx := context.Background()
	disp := &mockDispatcher{}
	f.srv.SetDispatcher(disp)
	broker := &store.RuntimeBroker{ID: tid("broker-unusable"), Slug: "unusable-broker", Name: "Unusable Broker", Status: store.BrokerStatusOnline}
	require.NoError(t, f.s.CreateRuntimeBroker(ctx, broker))
	project := &store.Project{ID: tid("project-unusable"), Slug: "unusable", Name: "Unusable"}
	require.NoError(t, f.s.CreateProject(ctx, project))
	f.srv.config.ProjectStorageConfig = projectStorageFromWorkspace(unusableWorkspaceStorage())

	rec := doRequest(t, f.srv, http.MethodPost, "/api/v1/agents", map[string]interface{}{
		"name": "unusable-agent", "projectId": project.ID, "runtimeBrokerId": broker.ID,
	})
	require.Equal(t, http.StatusServiceUnavailable, rec.Code, "body: %s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), "Workspace storage is not available")
	agents, err := f.s.ListAgents(ctx, store.AgentFilter{ProjectID: project.ID, IncludeDeleted: true}, store.ListOptions{})
	require.NoError(t, err)
	assert.Empty(t, agents.Items, "no agent row may be created")
	assert.Empty(t, disp.dispatchedAgents, "nothing may be dispatched")
}

// TestCreateAgent_EmbeddedBroker_WorkspaceLeafFailure_503: the co-located
// broker's workspace leaf cannot be created (the share is not mounted):
// 503, no row, nothing dispatched.
func TestCreateAgent_EmbeddedBroker_WorkspaceLeafFailure_503(t *testing.T) {
	f := newWorkspaceLayoutFixture(t)
	ctx := context.Background()
	disp := &mockDispatcher{}
	f.srv.SetDispatcher(disp)
	broker := &store.RuntimeBroker{ID: tid("broker-leaf-fail"), Slug: "leaf-fail-broker", Name: "Leaf Fail Broker", Status: store.BrokerStatusOnline}
	require.NoError(t, f.s.CreateRuntimeBroker(ctx, broker))
	f.srv.SetEmbeddedBrokerID(broker.ID)
	project := &store.Project{ID: tid("project-leaf-fail"), Slug: "leaf-fail", Name: "Leaf Fail"}
	require.NoError(t, f.s.CreateProject(ctx, project))
	f.srv.config.ProjectStorageConfig = projectStorageFromWorkspace(nfsConfig(filepath.Join(f.home, "not-mounted")))

	rec := doRequest(t, f.srv, http.MethodPost, "/api/v1/agents", map[string]interface{}{
		"name": "leaf-fail-agent", "projectId": project.ID, "runtimeBrokerId": broker.ID,
	})
	require.Equal(t, http.StatusServiceUnavailable, rec.Code, "body: %s", rec.Body.String())
	assert.NotContains(t, rec.Body.String(), f.home, "no host path")
	agents, err := f.s.ListAgents(ctx, store.AgentFilter{ProjectID: project.ID, IncludeDeleted: true}, store.ListOptions{})
	require.NoError(t, err)
	assert.Empty(t, agents.Items)
	assert.Empty(t, disp.dispatchedAgents)
}

// TestReincarnateAgent_UnusableSharedStorage_503: reincarnate re-derives the
// agent's config and fails closed the same way.
func TestReincarnateAgent_UnusableSharedStorage_503(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	disp := newReincarnateTestDispatcher()
	srv, s, project, broker := setupReincarnateTestServer(t, disp)
	srv.config.ProjectStorageConfig = projectStorageFromWorkspace(unusableWorkspaceStorage())
	agent := newReincarnateTestAgent(t, s, project, broker, func(a *store.Agent) {
		a.AppliedConfig.Workspace = ""
	})

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+agent.ID+"/reincarnate", ReincarnateAgentRequest{DryRun: true})
	require.Equal(t, http.StatusServiceUnavailable, rec.Code, "body: %s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), "Workspace storage is not available")
}

// TestCreateProject_UnusableSharedStorage_ReleasesProjectQuota: the rolled
// back create releases its project quota reservation.
func TestCreateProject_UnusableSharedStorage_ReleasesProjectQuota(t *testing.T) {
	f := newWorkspaceLayoutFixture(t)
	setUserProjectQuotaCeiling(t, f.s, 1)
	f.srv.config.ProjectStorageConfig = projectStorageFromWorkspace(unusableWorkspaceStorage())
	for i := 0; i < 2; i++ {
		rec := doRequest(t, f.srv, http.MethodPost, "/api/v1/projects", CreateProjectRequest{Name: fmt.Sprintf("Unusable Quota %d", i)})
		require.Equal(t, http.StatusServiceUnavailable, rec.Code, "body: %s", rec.Body.String())
		assert.Zero(t, countProjectQuotaReservations(t, f.s, DevUserID), "rollback must release the reservation")
	}
	f.srv.config.ProjectStorageConfig = nil
	rec := doRequest(t, f.srv, http.MethodPost, "/api/v1/projects", CreateProjectRequest{Name: "Usable Quota"})
	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())
}

// TestSafetyGate_WorkspaceAbsent_NothingCreated: on a
// Cloud Run Service with local workspace storage, refused writes to an
// absent workspace create nothing.
func TestSafetyGate_WorkspaceAbsent_NothingCreated(t *testing.T) {
	f := newWorkspaceLayoutFixture(t)
	project, workspacePath := createTestHubManagedProject(t, f.srv, "Gate Absent")
	require.NoError(t, os.RemoveAll(workspacePath))
	t.Setenv("K_SERVICE", "hub-service")

	url := workspaceFilesURL(project.ID)
	assert.Equal(t, http.StatusServiceUnavailable,
		doMultipartRequest(t, f.srv, http.MethodPost, url, map[string][]byte{"a.txt": []byte("a")}).Code)
	assert.Equal(t, http.StatusServiceUnavailable,
		doRequest(t, f.srv, http.MethodPut, url+"/a.txt", ProjectWorkspaceWriteRequest{Content: "a"}).Code)
	_, err := os.Stat(workspacePath)
	assert.True(t, os.IsNotExist(err), "a refused write creates nothing")
}

// TestProjectWorkspaceFiles_ShareRootMissing_503: on a
// shared backend whose share root is missing (not mounted), the Files tab
// answers 503, not an empty listing.
func TestProjectWorkspaceFiles_ShareRootMissing_503(t *testing.T) {
	f := newWorkspaceLayoutFixture(t)
	project, _ := createTestHubManagedProject(t, f.srv, "Share Missing")
	f.srv.config.ProjectStorageConfig = projectStorageFromWorkspace(nfsConfig(filepath.Join(f.home, "not-mounted")))
	rec := doRequest(t, f.srv, http.MethodGet, workspaceFilesURL(project.ID), nil)
	assert.Equal(t, http.StatusServiceUnavailable, rec.Code, "body: %s", rec.Body.String())
	assert.NotContains(t, rec.Body.String(), f.home)
}

// TestCreateSharedWorkspaceProject_SharedBackend_RefusesNonNetworkCloneURL:
// on a shared backend the hub clones the project-supplied URL on its own
// host, so a local path, file://, ext:: or any scheme other than https, ssh
// and git is refused with a fixed 422 before anything is created: no
// project row, no project tree, no clone attempted.
func TestCreateSharedWorkspaceProject_SharedBackend_RefusesNonNetworkCloneURL(t *testing.T) {
	f := newWorkspaceLayoutFixture(t)
	f.useNFS()
	src := newSourceRepo(t)
	seen := cloneLocalFixture(t, src)

	for i, cloneURL := range []string{
		src,
		"file://" + src,
		"ext::git-remote-probe",
		"http://git.example.invalid/test/repo.git",
		"ftp://git.example.invalid/test/repo.git",
	} {
		name := fmt.Sprintf("Refused Clone %d", i)
		rec := doRequest(t, f.srv, http.MethodPost, "/api/v1/projects", CreateProjectRequest{
			Name: name, GitRemote: fmt.Sprintf("github.com/test/refused-clone-%d", i), WorkspaceMode: store.WorkspaceModeShared,
			Labels: map[string]string{store.LabelCloneURL: cloneURL},
		})
		require.Equal(t, http.StatusUnprocessableEntity, rec.Code, "%s: body: %s", cloneURL, rec.Body.String())
		assert.Contains(t, rec.Body.String(), provision.ErrHostCloneURLRefused.Error())
		assert.NotContains(t, rec.Body.String(), src, "the refused URL is not echoed")
		result, err := f.s.ListProjects(context.Background(), store.ProjectFilter{}, store.ListOptions{})
		require.NoError(t, err)
		for _, p := range result.Items {
			assert.NotEqual(t, name, p.Name, "the project row is rolled back")
		}
	}
	assert.Empty(t, *seen, "no clone was attempted")
	entries, err := os.ReadDir(filepath.Join(f.shareBase, "projects"))
	if err == nil {
		assert.Empty(t, entries, "no project tree was created")
	} else {
		assert.True(t, os.IsNotExist(err), "projects/: %v", err)
	}

	// The accepted schemes reach the clone.
	for _, cloneURL := range []string{
		"https://git.example.invalid/test/ok.git",
		"ssh://git@git.example.invalid/test/ok.git",
		"git://git.example.invalid/test/ok.git",
		"git@git.example.invalid:test/ok.git",
	} {
		require.NoError(t, provision.CheckHostCloneURL(cloneURL), cloneURL)
	}
}

// TestWriteRootFile_RefusesSymlinkAndSpecialFiles: os.Root follows a
// symbolic link that stays inside the root even with O_NOFOLLOW, so
// writeRootFile checks the leaf itself: a symlink (to a sibling file, or
// dangling) and a FIFO are refused without blocking and left unchanged,
// nothing is created through a dangling link, a regular file is replaced
// and an absent one is created.
func TestWriteRootFile_RefusesSymlinkAndSpecialFiles(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "settings.yaml"), []byte("keep: me\n"), 0o644))
	require.NoError(t, os.Symlink("settings.yaml", filepath.Join(dir, "project-slug")))
	require.NoError(t, os.Symlink("absent-target", filepath.Join(dir, "dangling")))
	require.NoError(t, syscall.Mkfifo(filepath.Join(dir, "project-id"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "existing"), []byte("a much longer old value\n"), 0o644))
	root, err := os.OpenRoot(dir)
	require.NoError(t, err)
	defer func() { _ = root.Close() }()

	require.Error(t, writeRootFile(root, "project-slug", "new-slug\n"), "a symlink is refused")
	got, err := os.ReadFile(filepath.Join(dir, "settings.yaml"))
	require.NoError(t, err)
	assert.Equal(t, "keep: me\n", string(got), "the link target is not truncated or written")
	fi, err := os.Lstat(filepath.Join(dir, "project-slug"))
	require.NoError(t, err)
	assert.NotZero(t, fi.Mode()&os.ModeSymlink, "the link is left as is")

	require.Error(t, writeRootFile(root, "dangling", "x\n"), "a dangling symlink is refused")
	_, err = os.Lstat(filepath.Join(dir, "absent-target"))
	assert.True(t, os.IsNotExist(err), "nothing is created through a dangling symlink")

	require.Error(t, writeRootFile(root, "project-id", "x\n"), "a FIFO is refused without blocking")

	require.NoError(t, writeRootFile(root, "existing", "v\n"))
	got, err = os.ReadFile(filepath.Join(dir, "existing"))
	require.NoError(t, err)
	assert.Equal(t, "v\n", string(got), "a regular file is replaced")
	require.NoError(t, writeRootFile(root, "fresh", "f\n"))
	got, err = os.ReadFile(filepath.Join(dir, "fresh"))
	require.NoError(t, err)
	assert.Equal(t, "f\n", string(got))
}

// userTokenSecretBackend returns value as every user-scope GITHUB_TOKEN (the
// project creator's token, resolveCloneToken's last fallback).
type userTokenSecretBackend struct {
	mockSecretBackend
	value string
}

func (b *userTokenSecretBackend) Get(_ context.Context, name, scope, _ string) (*secret.SecretWithValue, error) {
	if name == "GITHUB_TOKEN" && scope == "user" {
		return &secret.SecretWithValue{Value: b.value}, nil
	}
	return nil, nil
}

// TestCreateSharedWorkspaceProject_SharedBackend_TokenBoundToRecordedRemote:
// the hub binds the project's git token to the project's own recorded
// remote (CloneTokenRemote from the project record, never the clone-url
// label). A non-GitHub remote (here a GitHub Enterprise-style host) gets the
// creator's token through the user GITHUB_TOKEN path.
func TestCreateSharedWorkspaceProject_SharedBackend_TokenBoundToRecordedRemote(t *testing.T) {
	f := newWorkspaceLayoutFixture(t)
	f.useNFS()
	const token = "test-clone-token-bound-to-remote"
	f.srv.SetSecretBackend(&userTokenSecretBackend{value: token})
	seen := cloneLocalFixture(t, newSourceRepo(t))

	create := func(name, remote string, labels map[string]string) provision.ProvisionInput {
		t.Helper()
		before := len(*seen)
		rec := doRequest(t, f.srv, http.MethodPost, "/api/v1/projects", CreateProjectRequest{
			Name: name, GitRemote: remote, WorkspaceMode: store.WorkspaceModeShared, Labels: labels,
		})
		require.Equal(t, http.StatusCreated, rec.Code, "%s: body: %s", name, rec.Body.String())
		require.Len(t, *seen, before+1, "%s: one provisioning call", name)
		return (*seen)[before]
	}

	in := create("Token Own Remote", "ghe.example.invalid/team/app", nil)
	assert.Equal(t, token, in.CloneToken, "the creator's token is resolved for a non-GitHub remote")
	assert.Equal(t, "ghe.example.invalid/team/app", in.CloneTokenRemote, "bound to the recorded remote")
	require.NotNil(t, in.GitClone)
	assert.Equal(t, "https://ghe.example.invalid/team/app.git", in.GitClone.URL)
	assert.True(t, in.HostGitHardened)

	// A clone-url label naming another host: the token stays bound to the
	// recorded remote, so the clone of the label's URL gets none (the
	// binding itself is pinned by pkg/provision's
	// TestRunGitClone_TokenBoundToRecordedRemoteAndNoRedirects).
	in = create("Token Mirror Host", "ghe.example.invalid/team/mirror",
		map[string]string{store.LabelCloneURL: "https://mirror.example.invalid/team/mirror.git"})
	assert.Equal(t, "ghe.example.invalid/team/mirror", in.CloneTokenRemote, "the record, never the label")
	assert.Equal(t, "https://mirror.example.invalid/team/mirror.git", in.GitClone.URL)
}

// TestCreateSharedWorkspaceProject_SharedBackend_CloneFailureKeepsExistingRoot:
// the rollback of a failed provisioning removes only a project root that
// this create made; a root that already existed (content another writer
// put there) is left with its content.
func TestCreateSharedWorkspaceProject_SharedBackend_CloneFailureKeepsExistingRoot(t *testing.T) {
	f := newWorkspaceLayoutFixture(t)
	f.useNFS()
	cloneLocalFixture(t, filepath.Join(t.TempDir(), "no-such-repo"))
	const id = "aaaaaaaa-0000-4000-8000-0000000000c7"
	root := filepath.Join(f.shareBase, "projects", id)
	require.NoError(t, os.MkdirAll(root, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(root, "keep.txt"), []byte("existing"), 0o644))

	rec := doRequest(t, f.srv, http.MethodPost, "/api/v1/projects", CreateProjectRequest{
		ID: id, Name: "Existing Root NFS", GitRemote: "github.com/test/existing-root-nfs", WorkspaceMode: store.WorkspaceModeShared,
		Labels: map[string]string{store.LabelCloneURL: "https://git.example.invalid/test/existing-root-nfs.git"},
	})
	require.Equal(t, http.StatusUnprocessableEntity, rec.Code, "body: %s", rec.Body.String())
	got, err := os.ReadFile(filepath.Join(root, "keep.txt"))
	require.NoError(t, err, "a project root that existed before the create is not removed")
	assert.Equal(t, "existing", string(got))
}
