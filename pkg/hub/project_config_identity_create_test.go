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
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/projectkeys"
	"github.com/GoogleCloudPlatform/scion/pkg/provision"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// createProjectOverHTTP creates a project through POST /api/v1/projects (the
// hub is already running, so the hub-start identity pass has passed) and
// returns the stored project.
func createProjectOverHTTP(t *testing.T, srv *Server, body CreateProjectRequest) store.Project {
	t.Helper()
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/projects", body)
	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())
	var project store.Project
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&project))
	return project
}

func readIdentityFile(t *testing.T, cfgDir, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(cfgDir, name))
	require.NoError(t, err, name)
	return string(data)
}

// TestCreateProject_PerAgentGit_SharedBackend_SeedsProjectIdentity: a git
// project created AFTER hub start whose create prepares no workspace
// (clone-per-agent, the git default with no workspace mode, and
// worktree-per-agent) gets <ProjectRoot>/.scion/project-id and project-slug
// on a shared backend, so a broker's shared project configuration check finds
// them without waiting for the next hub start. Only the identity files are
// written: no workspace and no settings are seeded for these shapes.
func TestCreateProject_PerAgentGit_SharedBackend_SeedsProjectIdentity(t *testing.T) {
	for name, mode := range map[string]string{
		"clone-per-agent":    "per-agent",
		"git default":        "",
		"worktree-per-agent": "worktree-per-agent",
	} {
		t.Run(name, func(t *testing.T) {
			f := newWorkspaceLayoutFixture(t)
			f.useNFS()
			project := createProjectOverHTTP(t, f.srv, CreateProjectRequest{
				Name:          "Per Agent " + name,
				GitRemote:     "github.com/test/per-agent-seed",
				WorkspaceMode: mode,
			})
			require.False(t, project.IsSharedWorkspace(), "fixture check: not a shared workspace")

			projectRoot := filepath.Join(f.shareBase, "projects", project.ID)
			cfgDir := filepath.Join(projectRoot, ".scion")
			assert.Equal(t, project.ID+"\n", readIdentityFile(t, cfgDir, projectkeys.ProjectIDFile))
			assert.Equal(t, project.Slug+"\n", readIdentityFile(t, cfgDir, projectkeys.ProjectSlugFile))
			for _, n := range []string{projectkeys.ProjectIDFile, projectkeys.ProjectSlugFile} {
				info, err := os.Lstat(filepath.Join(cfgDir, n))
				require.NoError(t, err)
				assert.Equal(t, os.FileMode(0o644), info.Mode().Perm(), n)
			}
			info, err := os.Lstat(cfgDir)
			require.NoError(t, err)
			assert.True(t, info.IsDir())
			assert.Zero(t, info.Mode().Perm()&0o022, "the configuration directory is not group/other-writable")

			_, err = os.Stat(filepath.Join(projectRoot, "workspace"))
			assert.True(t, os.IsNotExist(err), "no workspace is prepared for a per-agent git project at create")
			_, err = os.Stat(filepath.Join(cfgDir, "settings.yaml"))
			assert.True(t, os.IsNotExist(err), "only the identity files are seeded for this shape")
			_, err = os.Stat(filepath.Join(f.home, ".scion", "projects", project.Slug))
			assert.True(t, os.IsNotExist(err), "nothing is seeded on the hub's local disk")
		})
	}
}

// TestCreateProject_PerAgentGit_LocalBackend_NoIdentityFiles: on the local
// backend (no shared workspace storage) a per-agent git create seeds nothing.
func TestCreateProject_PerAgentGit_LocalBackend_NoIdentityFiles(t *testing.T) {
	f := newWorkspaceLayoutFixture(t)
	project := createProjectOverHTTP(t, f.srv, CreateProjectRequest{
		Name:          "Local Per Agent",
		GitRemote:     "github.com/test/local-per-agent",
		WorkspaceMode: "per-agent",
	})
	_, err := os.Stat(filepath.Join(f.home, ".scion", "projects", project.Slug))
	assert.True(t, os.IsNotExist(err), "no project directory is created on the local backend")
	_, err = os.Stat(filepath.Join(f.shareBase, "projects"))
	assert.True(t, os.IsNotExist(err), "nothing is written to the (unconfigured) share")
}

// TestCreateProject_SharedBackend_IdentitySeedIdempotent: the per-agent
// seeding helper and the hub-native path agree. A hub-native project seeded
// at create (initHubManagedProject) is unchanged by the per-agent helper and
// by the hub-start pass; a per-agent git project seeded at create is
// unchanged by the hub-start pass.
func TestCreateProject_SharedBackend_IdentitySeedIdempotent(t *testing.T) {
	f := newWorkspaceLayoutFixture(t)
	f.useNFS()
	native, _ := createTestHubManagedProject(t, f.srv, "Native Seeded")
	perAgent := createProjectOverHTTP(t, f.srv, CreateProjectRequest{
		Name:          "Per Agent Seeded",
		GitRemote:     "github.com/test/per-agent-idem",
		WorkspaceMode: "per-agent",
	})

	nativeDir := filepath.Join(f.shareBase, "projects", native.ID, ".scion")
	perAgentDir := filepath.Join(f.shareBase, "projects", perAgent.ID, ".scion")
	before := map[string]string{}
	for _, d := range []string{nativeDir, perAgentDir} {
		for _, n := range []string{projectkeys.ProjectIDFile, projectkeys.ProjectSlugFile} {
			before[filepath.Join(d, n)] = readIdentityFile(t, d, n)
		}
	}

	f.srv.ensureSharedProjectIdentityBestEffort(native)
	counts := f.srv.ensureProjectConfigIdentities(context.Background())
	assert.Equal(t, 2, counts.ensured)
	assert.Zero(t, counts.mismatched+counts.failed+counts.notReached)
	for path, want := range before {
		got, err := os.ReadFile(path)
		require.NoError(t, err)
		assert.Equal(t, want, string(got), path)
	}
	assert.Equal(t, native.ID+"\n", before[filepath.Join(nativeDir, projectkeys.ProjectIDFile)])
	assert.Equal(t, perAgent.ID+"\n", before[filepath.Join(perAgentDir, projectkeys.ProjectIDFile)])
}

// TestCreateProject_PerAgentGit_SharedBackend_MismatchLeftAndCreateSucceeds:
// the per-agent seed keeps the existing mismatch handling. A pre-existing
// project-id naming another project is not overwritten, the mismatch is
// reported by the health check, and the create still succeeds (best-effort).
func TestCreateProject_PerAgentGit_SharedBackend_MismatchLeftAndCreateSucceeds(t *testing.T) {
	f := newWorkspaceLayoutFixture(t)
	f.useNFS()
	const id = "aaaaaaaa-0000-4000-8000-0000000000e1"
	const other = "bbbbbbbb-0000-4000-8000-0000000000e2"
	cfgDir := filepath.Join(f.shareBase, "projects", id, ".scion")
	require.NoError(t, os.MkdirAll(cfgDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(cfgDir, projectkeys.ProjectIDFile), []byte(other+"\n"), 0o644))

	project := createProjectOverHTTP(t, f.srv, CreateProjectRequest{
		ID:            id,
		Name:          "Per Agent Mismatch",
		GitRemote:     "github.com/test/per-agent-mismatch",
		WorkspaceMode: "per-agent",
	})
	require.Equal(t, id, project.ID, "fixture check: the client-supplied ID is used")
	assert.Equal(t, other+"\n", readIdentityFile(t, cfgDir, projectkeys.ProjectIDFile), "a differing project-id is never overwritten")
	_, err := os.Stat(filepath.Join(cfgDir, projectkeys.ProjectSlugFile))
	assert.True(t, os.IsNotExist(err), "project-slug is not written next to another project's project-id")

	checks := map[string]string{}
	f.srv.checkProjectConfigIdentityHealth(checks)
	assert.Contains(t, checks[projectConfigIdentityHealthKey], HealthStatusDegraded)
}

// TestCloneProject_PerAgentGit_SharedBackend_SeedsProjectIdentity covers the
// clone path (handleProjectClone): cloning a per-agent git project on a
// shared backend seeds the clone's <ProjectRoot>/.scion/project-id and
// project-slug at clone time, as createProject does, with no workspace.
func TestCloneProject_PerAgentGit_SharedBackend_SeedsProjectIdentity(t *testing.T) {
	for name, mode := range map[string]string{
		"clone-per-agent":    "per-agent",
		"worktree-per-agent": "worktree-per-agent",
	} {
		t.Run(name, func(t *testing.T) {
			f := newWorkspaceLayoutFixture(t)
			f.useNFS()
			src := createProjectOverHTTP(t, f.srv, CreateProjectRequest{
				Name:          "Clone Source " + name,
				GitRemote:     "github.com/test/per-agent-clone-src",
				WorkspaceMode: mode,
			})
			rec := doRequest(t, f.srv, http.MethodPost, "/api/v1/projects/"+src.ID+"/clone",
				map[string]string{"name": "Cloned " + name})
			require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())
			var clone store.Project
			require.NoError(t, json.NewDecoder(rec.Body).Decode(&clone))
			require.NotEqual(t, src.ID, clone.ID)
			require.False(t, clone.IsSharedWorkspace(), "fixture check: the clone is not a shared workspace")
			require.NotEmpty(t, clone.GitRemote, "fixture check: the clone is a git project")

			cfgDir := filepath.Join(f.shareBase, "projects", clone.ID, ".scion")
			assert.Equal(t, clone.ID+"\n", readIdentityFile(t, cfgDir, projectkeys.ProjectIDFile))
			assert.Equal(t, clone.Slug+"\n", readIdentityFile(t, cfgDir, projectkeys.ProjectSlugFile))
			_, err := os.Stat(filepath.Join(f.shareBase, "projects", clone.ID, "workspace"))
			assert.True(t, os.IsNotExist(err), "no workspace is prepared for a per-agent git clone")
		})
	}
}

// TestRegisterProject_SharedBackend_SeedsProjectIdentity: a project created
// by POST /api/v1/projects/register is dispatched by slug to every provider
// without a registered local path, so on a shared backend its identity files
// are written at register time (git and hub-native shapes alike).
func TestRegisterProject_SharedBackend_SeedsProjectIdentity(t *testing.T) {
	for name, remote := range map[string]string{
		"git":        "github.com/test/registered-seed",
		"hub-native": "",
	} {
		t.Run(name, func(t *testing.T) {
			f := newWorkspaceLayoutFixture(t)
			f.useNFS()
			rec := doRequest(t, f.srv, http.MethodPost, "/api/v1/projects/register", RegisterProjectRequest{
				Name:      "Registered " + name,
				GitRemote: remote,
			})
			require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
			var resp RegisterProjectResponse
			require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
			require.True(t, resp.Created, "fixture check: a new project")
			require.NotNil(t, resp.Project)

			cfgDir := filepath.Join(f.shareBase, "projects", resp.Project.ID, ".scion")
			assert.Equal(t, resp.Project.ID+"\n", readIdentityFile(t, cfgDir, projectkeys.ProjectIDFile))
			assert.Equal(t, resp.Project.Slug+"\n", readIdentityFile(t, cfgDir, projectkeys.ProjectSlugFile))
		})
	}
}

// TestRegisterProject_ReRegisterExisting_LeavesIdentityUnchanged: only the
// created branch of handleProjectRegister seeds. Re-registering an existing
// project changes nothing, even when its project-id file was left naming
// another project (that mismatch stays for the reporting paths).
func TestRegisterProject_ReRegisterExisting_LeavesIdentityUnchanged(t *testing.T) {
	f := newWorkspaceLayoutFixture(t)
	f.useNFS()
	const id = "aaaaaaaa-0000-4000-8000-0000000000f1"
	const other = "bbbbbbbb-0000-4000-8000-0000000000f2"
	req := RegisterProjectRequest{ID: id, Name: "Registered Again", GitRemote: "github.com/test/registered-again"}
	rec := doRequest(t, f.srv, http.MethodPost, "/api/v1/projects/register", req)
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	var first RegisterProjectResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&first))
	require.True(t, first.Created)
	require.Equal(t, id, first.Project.ID, "fixture check: the client-supplied ID is used")

	cfgDir := filepath.Join(f.shareBase, "projects", id, ".scion")
	require.Equal(t, id+"\n", readIdentityFile(t, cfgDir, projectkeys.ProjectIDFile), "fixture check: seeded at register")
	require.NoError(t, os.WriteFile(filepath.Join(cfgDir, projectkeys.ProjectIDFile), []byte(other+"\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(cfgDir, projectkeys.ProjectSlugFile), []byte("stale\n"), 0o644))

	rec = doRequest(t, f.srv, http.MethodPost, "/api/v1/projects/register", req)
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	var again RegisterProjectResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&again))
	require.False(t, again.Created, "fixture check: the existing project is re-registered")
	assert.Equal(t, other+"\n", readIdentityFile(t, cfgDir, projectkeys.ProjectIDFile), "a re-register never touches project-id")
	assert.Equal(t, "stale\n", readIdentityFile(t, cfgDir, projectkeys.ProjectSlugFile), "a re-register writes nothing")
}

// useNFSOwnedBySelf configures the NFS backend with this process's own
// uid:gid as the Layout owner, so the provisioning chown succeeds unprivileged
// and the owner can be asserted.
func (f workspaceLayoutFixture) useNFSOwnedBySelf() {
	cfg := nfsConfig(f.mountRoot)
	cfg.NFS.UID = os.Getuid()
	cfg.NFS.GID = os.Getgid()
	f.srv.config.ProjectStorageConfig = projectStorageFromWorkspace(cfg)
}

// assertHubNativeWorkspaceProvisioned checks the provisioned hub-native
// workspace of projectID: the leaf at mode 2775 owned by this process (the
// Layout uid:gid of useNFSOwnedBySelf), empty, and the provisioning sentinel
// in <ProjectRoot>/provision.
func assertHubNativeWorkspaceProvisioned(t *testing.T, f workspaceLayoutFixture, projectID string) {
	t.Helper()
	projectRoot := filepath.Join(f.shareBase, "projects", projectID)
	ws := filepath.Join(projectRoot, "workspace")
	info, err := os.Lstat(ws)
	require.NoError(t, err, "the workspace leaf exists")
	require.True(t, info.IsDir())
	assert.Equal(t, os.FileMode(0o775), info.Mode().Perm(), "leaf mode 2775 (permission bits)")
	assert.NotZero(t, info.Mode()&os.ModeSetgid, "leaf mode 2775 (setgid)")
	st, ok := info.Sys().(*syscall.Stat_t)
	require.True(t, ok)
	assert.Equal(t, uint32(os.Getuid()), st.Uid, "the leaf is owned by the Layout uid")
	assert.Equal(t, uint32(os.Getgid()), st.Gid, "the leaf is owned by the Layout gid")
	entries, err := os.ReadDir(ws)
	require.NoError(t, err)
	assert.Empty(t, entries, "the hub-native workspace is empty: no lock file, no sentinel inside")
	assert.FileExists(t, filepath.Join(projectRoot, provision.ProvisionStateDirName, provision.ProvisionSentinelFile),
		"the provisioning sentinel is in <ProjectRoot>/provision")
}

// TestCreateHubNativeProject_SharedBackend_ProvisionsWorkspaceLeaf: a new
// hub-native project on a shared backend gets projects/<uuid>/workspace
// provisioned at create (mode 2775, the Layout owner, the sentinel), so the
// first agent finds a provisioned workspace.
func TestCreateHubNativeProject_SharedBackend_ProvisionsWorkspaceLeaf(t *testing.T) {
	f := newWorkspaceLayoutFixture(t)
	f.useNFSOwnedBySelf()
	project, _ := createTestHubManagedProject(t, f.srv, "Native Leaf")
	assertHubNativeWorkspaceProvisioned(t, f, project.ID)
	assert.FileExists(t, filepath.Join(f.shareBase, "projects", project.ID, ".scion", projectkeys.ProjectIDFile),
		"the project configuration is seeded as before")
}

// TestEnsureProjectConfigIdentities_BackfillsHubNativeWorkspace: at hub start
// an existing hub-native project without the workspace leaf gets it
// provisioned; a second pass changes nothing.
func TestEnsureProjectConfigIdentities_BackfillsHubNativeWorkspace(t *testing.T) {
	f := newWorkspaceLayoutFixture(t)
	f.useNFSOwnedBySelf()
	project := &store.Project{ID: "aaaaaaaa-0000-4000-8000-0000000000a7", Name: "Native Existing", Slug: "native-existing"}
	require.NoError(t, f.s.CreateProject(context.Background(), project))
	_, err := os.Stat(filepath.Join(f.shareBase, "projects", project.ID, "workspace"))
	require.True(t, os.IsNotExist(err), "fixture check: no workspace leaf yet")

	counts := f.srv.ensureProjectConfigIdentities(context.Background())
	assert.Equal(t, 1, counts.workspacesProvisioned)
	assert.Zero(t, counts.workspacesFailed+counts.failed)
	assertHubNativeWorkspaceProvisioned(t, f, project.ID)

	counts = f.srv.ensureProjectConfigIdentities(context.Background())
	assert.Zero(t, counts.workspacesProvisioned, "a present leaf is left alone")
	assert.Zero(t, counts.workspacesFailed)
}

// TestEnsureProjectConfigIdentities_GitProjectsGetNoWorkspaceLeaf: the
// startup pass never creates a workspace leaf for a git project
// (clone-per-agent, worktree-per-agent or shared-workspace), nor for an
// empty-per-agent project, and a per-agent git create gets none either: a
// hub-created leaf must not change a git project's provisioning decision.
func TestEnsureProjectConfigIdentities_GitProjectsGetNoWorkspaceLeaf(t *testing.T) {
	f := newWorkspaceLayoutFixture(t)
	f.useNFSOwnedBySelf()
	projects := []*store.Project{
		{ID: "aaaaaaaa-0000-4000-8000-0000000000b7", Name: "Git Per Agent", Slug: "git-per-agent", GitRemote: "github.com/test/git-per-agent",
			Labels: map[string]string{store.LabelWorkspaceMode: store.WorkspaceModePerAgent}},
		{ID: "aaaaaaaa-0000-4000-8000-0000000000b8", Name: "Git Worktree", Slug: "git-worktree", GitRemote: "github.com/test/git-worktree",
			Labels: map[string]string{store.LabelWorkspaceMode: store.WorkspaceModeWorktreePerAgent}},
		{ID: "aaaaaaaa-0000-4000-8000-0000000000b9", Name: "Git Shared", Slug: "git-shared", GitRemote: "github.com/test/git-shared",
			Labels: map[string]string{store.LabelWorkspaceMode: store.WorkspaceModeShared}},
		{ID: "aaaaaaaa-0000-4000-8000-0000000000ba", Name: "Empty Per Agent", Slug: "empty-per-agent-x",
			Labels: map[string]string{store.LabelWorkspaceMode: string(store.SharingModeEmptyPerAgent)}},
	}
	for _, p := range projects {
		require.NoError(t, f.s.CreateProject(context.Background(), p))
	}
	require.True(t, projects[3].IsEmptyPerAgent(), "fixture check: empty-per-agent")
	counts := f.srv.ensureProjectConfigIdentities(context.Background())
	assert.Zero(t, counts.workspacesProvisioned)
	assert.Zero(t, counts.workspacesFailed)
	for _, p := range projects {
		_, err := os.Stat(filepath.Join(f.shareBase, "projects", p.ID, "workspace"))
		assert.True(t, os.IsNotExist(err), "%s gets no workspace leaf from the startup pass", p.Name)
		_, err = os.Stat(filepath.Join(f.shareBase, "projects", p.ID, provision.ProvisionStateDirName))
		assert.True(t, os.IsNotExist(err), "%s gets no provisioning state from the startup pass", p.Name)
	}

	created := createProjectOverHTTP(t, f.srv, CreateProjectRequest{
		Name: "Git Per Agent Created", GitRemote: "github.com/test/git-per-agent-created", WorkspaceMode: "per-agent",
	})
	_, err := os.Stat(filepath.Join(f.shareBase, "projects", created.ID, "workspace"))
	assert.True(t, os.IsNotExist(err), "a per-agent git create gets no workspace leaf")
}

// TestCreateHubNativeProject_LocalBackend_NoShareWrites: on the local
// backend a hub-native create provisions nothing on a share and keeps its
// existing ~/.scion/projects/<slug> workspace.
func TestCreateHubNativeProject_LocalBackend_NoShareWrites(t *testing.T) {
	f := newWorkspaceLayoutFixture(t)
	_, workspacePath := createTestHubManagedProject(t, f.srv, "Native Local")
	assert.DirExists(t, workspacePath, "the local workspace is created as before")
	_, err := os.Stat(filepath.Join(f.shareBase, "projects"))
	assert.True(t, os.IsNotExist(err), "nothing is written to the (unconfigured) share")
	_, err = os.Stat(filepath.Join(workspacePath, provision.ProvisionSentinelFile))
	assert.True(t, os.IsNotExist(err), "no provisioning sentinel on the local backend")
}

// TestCreateHubNativeProject_SharedBackend_ProvisioningFailureRollsBack: a
// failed hub-native workspace provisioning rolls the create back with 503
// (no host path, no clone wording) and removes the project tree it created.
func TestCreateHubNativeProject_SharedBackend_ProvisioningFailureRollsBack(t *testing.T) {
	f := newWorkspaceLayoutFixture(t)
	f.useNFSOwnedBySelf()
	orig := provisionSharedClone
	var seen []provision.ProvisionInput
	provisionSharedClone = func(in provision.ProvisionInput) error {
		seen = append(seen, in)
		return errors.New("simulated provisioning failure at " + in.Resolved.HostPath)
	}
	t.Cleanup(func() { provisionSharedClone = orig })

	rec := doRequest(t, f.srv, http.MethodPost, "/api/v1/projects", CreateProjectRequest{Name: "Native Fails"})
	require.Equal(t, http.StatusServiceUnavailable, rec.Code, "body: %s", rec.Body.String())
	assert.NotContains(t, rec.Body.String(), f.mountRoot, "no host path in the response")
	assert.NotContains(t, rec.Body.String(), "clone", "no clone wording for a hub-native workspace")
	require.Len(t, seen, 1, "ProvisionShared was called once")
	assert.Nil(t, seen[0].GitClone, "a hub-native workspace is never cloned")
	assert.Empty(t, seen[0].LegacyDir, "no workspace lock for a hub-native workspace")
	assert.Equal(t, store.SharingModeSharedPlain, seen[0].Mode)

	result, err := f.s.ListProjects(context.Background(), store.ProjectFilter{}, store.ListOptions{})
	require.NoError(t, err)
	for _, p := range result.Items {
		assert.NotEqual(t, "Native Fails", p.Name, "the project is rolled back")
	}
	entries, err := os.ReadDir(filepath.Join(f.shareBase, "projects"))
	if err == nil {
		assert.Empty(t, entries, "the project tree the failed create made is removed")
	} else {
		assert.True(t, os.IsNotExist(err))
	}
}

// TestWriteRootFile_AtomicReplaceLeavesNoTempFile: an identity
// file is replaced by a temp file plus rename (a new inode, so a concurrent
// reader never sees a truncated file), a new file is published whole, and
// neither a rewrite, a create nor a refused write leaves a temporary file.
func TestWriteRootFile_AtomicReplaceLeavesNoTempFile(t *testing.T) {
	dir := t.TempDir()
	root, err := os.OpenRoot(dir)
	require.NoError(t, err)
	defer func() { _ = root.Close() }()

	require.NoError(t, writeRootFile(root, projectkeys.ProjectSlugFile, "first-slug-that-is-long\n"))
	before, err := os.Lstat(filepath.Join(dir, projectkeys.ProjectSlugFile))
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o644), before.Mode().Perm())

	require.NoError(t, writeRootFile(root, projectkeys.ProjectSlugFile, "second\n"))
	after, err := os.Lstat(filepath.Join(dir, projectkeys.ProjectSlugFile))
	require.NoError(t, err)
	assert.False(t, os.SameFile(before, after), "the rewrite replaces the file (rename), it does not truncate in place")
	assert.Equal(t, os.FileMode(0o644), after.Mode().Perm())
	got, err := os.ReadFile(filepath.Join(dir, projectkeys.ProjectSlugFile))
	require.NoError(t, err)
	assert.Equal(t, "second\n", string(got))

	require.NoError(t, os.Symlink(projectkeys.ProjectSlugFile, filepath.Join(dir, "linked")))
	require.Error(t, writeRootFile(root, "linked", "x\n"), "a symlink is still refused")

	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	assert.ElementsMatch(t, []string{projectkeys.ProjectSlugFile, "linked"}, names, "no temporary file is left behind")
}

// TestCreateAndRegisterProject_RejectNonCanonicalClientID: a
// client-supplied project ID must be a canonical lowercase UUID on create and
// on register; anything else is a 400 before any lookup or write.
func TestCreateAndRegisterProject_RejectNonCanonicalClientID(t *testing.T) {
	f := newWorkspaceLayoutFixture(t)
	f.useNFS()
	const valid = "aaaaaaaa-0000-4000-8000-0000000000c9"
	for _, bad := range []string{
		"not-a-uuid",
		"../escape",
		"AAAAAAAA-0000-4000-8000-0000000000C9",
		"{aaaaaaaa-0000-4000-8000-0000000000c9}",
		"urn:uuid:aaaaaaaa-0000-4000-8000-0000000000c9",
		"aaaaaaaa00004000800000000000000c9",
	} {
		rec := doRequest(t, f.srv, http.MethodPost, "/api/v1/projects", CreateProjectRequest{ID: bad, Name: "Bad ID create"})
		assert.Equal(t, http.StatusBadRequest, rec.Code, "create with id %q: %s", bad, rec.Body.String())
		rec = doRequest(t, f.srv, http.MethodPost, "/api/v1/projects/register", RegisterProjectRequest{ID: bad, Name: "Bad ID register"})
		assert.Equal(t, http.StatusBadRequest, rec.Code, "register with id %q: %s", bad, rec.Body.String())
	}
	entries, err := os.ReadDir(filepath.Join(f.shareBase, "projects"))
	if err == nil {
		assert.Empty(t, entries, "nothing is written for a refused ID")
	}
	project := createProjectOverHTTP(t, f.srv, CreateProjectRequest{ID: valid, Name: "Good ID"})
	assert.Equal(t, valid, project.ID, "a canonical UUID is accepted")
}

// TestSanitizedStorageError_NoPath: the operator-log rendering of a
// storage error keeps the operation and errno and never a path.
func TestSanitizedStorageError_NoPath(t *testing.T) {
	hostPath := "/mnt/nfs/share1/projects/aaaaaaaa-0000-4000-8000-0000000000d9/.scion/project-id"
	pe := &os.PathError{Op: "open", Path: hostPath, Err: syscall.EACCES}
	got := sanitizedStorageError(fmt.Errorf("write project-id: %w", pe))
	assert.Equal(t, "write project-id: open: permission denied", got)
	assert.NotContains(t, got, "/mnt")

	got = sanitizedStorageError(fmt.Errorf("create project configuration directory %q: %w", hostPath, syscall.EIO))
	assert.NotContains(t, got, "/mnt", "a path formatted into the message is withheld")
	assert.NotContains(t, got, "projects/")

	le := &os.LinkError{Op: "rename", Old: hostPath + ".tmp", New: hostPath, Err: syscall.EXDEV}
	got = sanitizedStorageError(le)
	assert.NotContains(t, got, "/mnt")
	assert.Contains(t, got, "rename")
	assert.Empty(t, sanitizedStorageError(nil))
}

// TestCreateHubNativeProject_SharedBackend_ExistingRootNotProvisioned: a
// hub-native create whose project root already exists (a tree another hub on
// the same share made, or one left behind) provisions nothing: no sentinel,
// no state directory, the existing workspace untouched, and a project-id
// naming another project left unchanged. The leaf is left to the
// identity-confirmed startup backfill.
func TestCreateHubNativeProject_SharedBackend_ExistingRootNotProvisioned(t *testing.T) {
	f := newWorkspaceLayoutFixture(t)
	f.useNFSOwnedBySelf()
	const id = "aaaaaaaa-0000-4000-8000-0000000000aa"
	const other = "bbbbbbbb-0000-4000-8000-0000000000ab"
	projectRoot := filepath.Join(f.shareBase, "projects", id)
	ws := filepath.Join(projectRoot, "workspace")
	require.NoError(t, os.MkdirAll(ws, 0o700))
	require.NoError(t, os.Chmod(ws, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(ws, "existing.txt"), []byte("keep\n"), 0o600))
	cfgDir := filepath.Join(projectRoot, ".scion")
	require.NoError(t, os.MkdirAll(cfgDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(cfgDir, projectkeys.ProjectIDFile), []byte(other+"\n"), 0o644))

	rec := doRequest(t, f.srv, http.MethodPost, "/api/v1/projects", CreateProjectRequest{ID: id, Name: "Native Existing Root"})
	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())

	_, err := os.Stat(filepath.Join(projectRoot, provision.ProvisionStateDirName))
	assert.True(t, os.IsNotExist(err), "no provisioning state or sentinel is written into an existing tree at create")
	info, err := os.Lstat(ws)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o700), info.Mode().Perm(), "the existing workspace is not re-moded or re-owned")
	got, err := os.ReadFile(filepath.Join(ws, "existing.txt"))
	require.NoError(t, err)
	assert.Equal(t, "keep\n", string(got))
	assert.Equal(t, other+"\n", readIdentityFile(t, cfgDir, projectkeys.ProjectIDFile), "a foreign project-id is never overwritten")
}

// TestCreateHubNativeProject_SharedBackend_LaterFailureRemovesCreatedRoot:
// when this create made the project root and a later storage step fails
// (here the project configuration directory cannot be created), the create
// is rolled back with 503 and the tree it made is removed.
func TestCreateHubNativeProject_SharedBackend_LaterFailureRemovesCreatedRoot(t *testing.T) {
	f := newWorkspaceLayoutFixture(t)
	f.useNFSOwnedBySelf()
	const id = "aaaaaaaa-0000-4000-8000-0000000000ac"
	projectRoot := filepath.Join(f.shareBase, "projects", id)
	orig := provisionSharedClone
	provisionSharedClone = func(in provision.ProvisionInput) error {
		if err := orig(in); err != nil {
			return err
		}
		// A regular file where the project configuration directory goes:
		// the next storage step (EnsureProjectConfigDir) fails.
		return os.WriteFile(filepath.Join(projectRoot, ".scion"), []byte("not a directory\n"), 0o644)
	}
	t.Cleanup(func() { provisionSharedClone = orig })

	rec := doRequest(t, f.srv, http.MethodPost, "/api/v1/projects", CreateProjectRequest{ID: id, Name: "Native Seed Fails"})
	require.Equal(t, http.StatusServiceUnavailable, rec.Code, "body: %s", rec.Body.String())
	_, err := os.Lstat(projectRoot)
	assert.True(t, os.IsNotExist(err), "the project tree this create made is removed")
	_, err = f.s.GetProject(context.Background(), id)
	assert.ErrorIs(t, err, store.ErrNotFound, "the project row is rolled back")
}

// TestEnsureProjectConfigIdentities_LinkedProjectsGetNoWorkspaceLeaf: the
// startup backfill skips a project linked from an existing checkout (origin
// label linked) and an unlabeled project with a provider-registered local
// path, and provisions an unlabeled project with no local path (the default
// for projects older than the origin label).
func TestEnsureProjectConfigIdentities_LinkedProjectsGetNoWorkspaceLeaf(t *testing.T) {
	f := newWorkspaceLayoutFixture(t)
	f.useNFSOwnedBySelf()
	ctx := context.Background()
	linked := &store.Project{ID: "aaaaaaaa-0000-4000-8000-0000000000ad", Name: "Linked Label", Slug: "linked-label",
		Labels: map[string]string{store.LabelProjectOrigin: store.ProjectOriginLinked}}
	withPath := &store.Project{ID: "aaaaaaaa-0000-4000-8000-0000000000ae", Name: "Unlabeled With Path", Slug: "unlabeled-with-path"}
	plain := &store.Project{ID: "aaaaaaaa-0000-4000-8000-0000000000af", Name: "Unlabeled Plain", Slug: "unlabeled-plain"}
	for _, p := range []*store.Project{linked, withPath, plain} {
		require.NoError(t, f.s.CreateProject(ctx, p))
	}
	b := addProviderBroker(t, f.s, withPath.ID, "linked-path")
	providers, err := f.s.GetProjectProviders(ctx, withPath.ID)
	require.NoError(t, err)
	require.Len(t, providers, 1)
	providers[0].LocalPath = "/home/dev/checkout"
	require.NoError(t, f.s.AddProjectProvider(ctx, &providers[0]), "fixture: register the provider's local path (broker %s)", b.ID)
	providers, err = f.s.GetProjectProviders(ctx, withPath.ID)
	require.NoError(t, err)
	require.Equal(t, "/home/dev/checkout", providers[0].LocalPath, "fixture check: the local path is stored")

	counts := f.srv.ensureProjectConfigIdentities(ctx)
	assert.Equal(t, 1, counts.workspacesProvisioned)
	for _, p := range []*store.Project{linked, withPath} {
		_, err := os.Stat(filepath.Join(f.shareBase, "projects", p.ID, "workspace"))
		assert.True(t, os.IsNotExist(err), "%s gets no workspace leaf", p.Name)
	}
	assertHubNativeWorkspaceProvisioned(t, f, plain.ID)
}

// TestCreateHubNativeProject_SharedBackend_LaterFailureKeepsExistingRoot: the
// rollback cleanup removes only a project root the failed create made. A
// pre-existing root whose identity cannot be read (here project-id is a
// symlink leading out of the configuration directory, which os.Root refuses)
// fails the create with 503, and the tree, its content and the link are kept.
func TestCreateHubNativeProject_SharedBackend_LaterFailureKeepsExistingRoot(t *testing.T) {
	f := newWorkspaceLayoutFixture(t)
	f.useNFSOwnedBySelf()
	const id = "aaaaaaaa-0000-4000-8000-0000000000b0"
	projectRoot := filepath.Join(f.shareBase, "projects", id)
	ws := filepath.Join(projectRoot, "workspace")
	require.NoError(t, os.MkdirAll(ws, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(ws, "existing.txt"), []byte("keep\n"), 0o644))
	cfgDir := filepath.Join(projectRoot, ".scion")
	require.NoError(t, os.MkdirAll(cfgDir, 0o755))
	outside := filepath.Join(t.TempDir(), "foreign-project-id")
	require.NoError(t, os.WriteFile(outside, []byte("bbbbbbbb-0000-4000-8000-0000000000b1\n"), 0o644))
	require.NoError(t, os.Symlink(outside, filepath.Join(cfgDir, projectkeys.ProjectIDFile)))

	rec := doRequest(t, f.srv, http.MethodPost, "/api/v1/projects", CreateProjectRequest{ID: id, Name: "Native Existing Root Fails"})
	require.Equal(t, http.StatusServiceUnavailable, rec.Code, "body: %s", rec.Body.String())
	_, err := f.s.GetProject(context.Background(), id)
	assert.ErrorIs(t, err, store.ErrNotFound, "the project row is rolled back")

	got, err := os.ReadFile(filepath.Join(ws, "existing.txt"))
	require.NoError(t, err, "a pre-existing tree is never removed by the rollback")
	assert.Equal(t, "keep\n", string(got))
	fi, err := os.Lstat(filepath.Join(cfgDir, projectkeys.ProjectIDFile))
	require.NoError(t, err)
	assert.NotZero(t, fi.Mode()&os.ModeSymlink, "the foreign project-id link is kept")
	got, err = os.ReadFile(outside)
	require.NoError(t, err)
	assert.Equal(t, "bbbbbbbb-0000-4000-8000-0000000000b1\n", string(got), "the link target is untouched")
	_, err = os.Stat(filepath.Join(projectRoot, provision.ProvisionStateDirName))
	assert.True(t, os.IsNotExist(err), "nothing was provisioned into the existing tree")
}

// providersFaultStore fails GetProjectProviders once its switch is armed,
// for the provider-lookup error case of hubNativeWorkspaceProject. It is
// installed with installStoreFault (never by reassigning srv.store).
type providersFaultStore struct {
	store.Store
	fault *storeFaultSwitch
}

func (w *providersFaultStore) GetProjectProviders(ctx context.Context, projectID string) ([]store.ProjectProvider, error) {
	if !w.fault.Active() {
		return w.Store.GetProjectProviders(ctx, projectID)
	}
	return nil, errors.New("simulated provider lookup failure")
}

// TestHubNativeWorkspaceProject_Predicate pins the one hub-native predicate:
// git, linked, empty-per-agent and global projects are never hub-native; the
// hub origin label is; an unlabeled project is hub-native only without a
// provider-registered local path; a provider lookup error means not hub-native;
// and a non-shared backend means no.
func TestHubNativeWorkspaceProject_Predicate(t *testing.T) {
	f := newWorkspaceLayoutFixture(t)
	// Installed before any setup; transparent until armed below.
	_, fault := installStoreFault(t, f.srv, func(inner store.Store, sw *storeFaultSwitch) *providersFaultStore {
		return &providersFaultStore{Store: inner, fault: sw}
	})
	f.useNFS()
	ctx := context.Background()
	mk := func(id, slug, remote string, labels map[string]string) *store.Project {
		p := &store.Project{ID: id, Name: slug, Slug: slug, GitRemote: remote, Labels: labels}
		require.NoError(t, f.s.CreateProject(ctx, p))
		return p
	}
	origin := func(v string) map[string]string { return map[string]string{store.LabelProjectOrigin: v} }
	gitProject := mk("aaaaaaaa-0000-4000-8000-0000000000c1", "pred-git", "github.com/test/pred-git", origin(store.ProjectOriginHub))
	linked := mk("aaaaaaaa-0000-4000-8000-0000000000c2", "pred-linked", "", origin(store.ProjectOriginLinked))
	emptyPerAgent := mk("aaaaaaaa-0000-4000-8000-0000000000c3", "pred-empty", "",
		map[string]string{store.LabelWorkspaceMode: string(store.SharingModeEmptyPerAgent), store.LabelProjectOrigin: store.ProjectOriginHub})
	hubLabeled := mk("aaaaaaaa-0000-4000-8000-0000000000c4", "pred-hub", "", origin(store.ProjectOriginHub))
	unlabeled := mk("aaaaaaaa-0000-4000-8000-0000000000c5", "pred-unlabeled", "", nil)
	unlabeledPath := mk("aaaaaaaa-0000-4000-8000-0000000000c6", "pred-unlabeled-path", "", nil)
	addProviderBroker(t, f.s, unlabeledPath.ID, "pred-path")
	providers, err := f.s.GetProjectProviders(ctx, unlabeledPath.ID)
	require.NoError(t, err)
	require.Len(t, providers, 1)
	providers[0].LocalPath = "/home/dev/checkout"
	require.NoError(t, f.s.AddProjectProvider(ctx, &providers[0]))
	global := &store.Project{ID: "aaaaaaaa-0000-4000-8000-0000000000c7", Name: "global", Slug: "global"}
	require.True(t, isGlobalHubProject(global.Slug), "fixture check: the global project slug")

	for name, tc := range map[string]struct {
		p    *store.Project
		want bool
	}{
		"git project":                    {gitProject, false},
		"linked origin":                  {linked, false},
		"empty-per-agent":                {emptyPerAgent, false},
		"global project":                 {global, false},
		"hub origin":                     {hubLabeled, true},
		"unlabeled, no local path":       {unlabeled, true},
		"unlabeled, provider local path": {unlabeledPath, false},
		"nil project":                    {nil, false},
	} {
		assert.Equal(t, tc.want, f.srv.hubNativeWorkspaceProject(ctx, tc.p), name)
	}

	local := newWorkspaceLayoutFixture(t)
	assert.False(t, local.srv.hubNativeWorkspaceProject(ctx, hubLabeled), "the local backend is never hub-native-provisioned")

	// Last, because the switch stays armed: a provider lookup error.
	fault.Arm()
	assert.False(t, f.srv.hubNativeWorkspaceProject(ctx, unlabeled), "a provider lookup error means not hub-native")
	assert.True(t, f.srv.hubNativeWorkspaceProject(ctx, hubLabeled), "the hub origin label needs no provider lookup")
}
