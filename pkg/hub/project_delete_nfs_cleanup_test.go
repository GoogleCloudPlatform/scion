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
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Tests for ptone/scion#2569: project delete cleans up the NFS workspace tree
// and the broker-side project directory.

// nfsDeleteTestEnv is a hub test server with HOME in a temp dir, a git
// project, and NFS workspace storage on a share at <home>/mnt/share1.
type nfsDeleteTestEnv struct {
	srv     *Server
	s       store.Store
	home    string
	subRoot string
	project *store.Project
}

func newNFSDeleteTestEnv(t *testing.T, name string) *nfsDeleteTestEnv {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	require.NoError(t, os.MkdirAll(filepath.Join(home, ".scion"), 0o755))
	srv, s := testServer(t)
	project := &store.Project{
		ID:        tid("project-" + name),
		Slug:      name,
		Name:      name,
		GitRemote: "github.com/test/" + name,
	}
	require.NoError(t, s.CreateProject(context.Background(), project))
	mountRoot := filepath.Join(home, "mnt")
	srv.config.WorkspaceStorageConfig = &config.V1WorkspaceStorageConfig{
		Backend: "nfs",
		NFS: &config.V1NFSConfig{
			MountRoot:   mountRoot,
			SubPathRoot: "projects",
			Shares:      []config.V1NFSShare{{ID: "share1", Server: "10.0.0.2", Export: "/ws", PVName: "pv"}},
		},
	}
	subRoot := filepath.Join(mountRoot, "share1", "projects")
	require.NoError(t, os.MkdirAll(subRoot, 0o755))
	return &nfsDeleteTestEnv{srv: srv, s: s, home: home, subRoot: subRoot, project: project}
}

// seedTree creates an NFS project tree (workspace, provision, worktrees)
// for projectID and returns its directory.
func (e *nfsDeleteTestEnv) seedTree(t *testing.T, projectID string) string {
	t.Helper()
	dir := filepath.Join(e.subRoot, projectID)
	for _, d := range []string{"workspace/.git", "provision", "worktrees/dev"} {
		require.NoError(t, os.MkdirAll(filepath.Join(dir, d), 0o755))
	}
	require.NoError(t, os.WriteFile(filepath.Join(dir, "provision", "lock"), []byte("x"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "workspace", "README.md"), []byte("x"), 0o644))
	return dir
}

func (e *nfsDeleteTestEnv) addBroker(t *testing.T, name, localPath string) *store.RuntimeBroker {
	t.Helper()
	ctx := context.Background()
	b := &store.RuntimeBroker{
		ID:       tid("broker-" + e.project.Slug + "-" + name),
		Slug:     e.project.Slug + "-" + name,
		Name:     name,
		Status:   store.BrokerStatusOnline,
		Endpoint: "http://" + name + ":9800",
	}
	require.NoError(t, e.s.CreateRuntimeBroker(ctx, b))
	require.NoError(t, e.s.AddProjectProvider(ctx, &store.ProjectProvider{
		ProjectID:  e.project.ID,
		BrokerID:   b.ID,
		BrokerName: b.Name,
		LocalPath:  localPath,
		LinkedBy:   "test",
	}))
	return b
}

func (e *nfsDeleteTestEnv) deleteProject(t *testing.T) {
	t.Helper()
	rec := doRequest(t, e.srv, http.MethodDelete, "/api/v1/projects/"+e.project.ID, nil)
	require.Equal(t, http.StatusNoContent, rec.Code, "body: %s", rec.Body.String())
	// Wait for the background NFS tree removal the delete started.
	e.srv.nfsCleanupWG.Wait()
	_, err := e.s.GetProject(context.Background(), e.project.ID)
	assert.ErrorIs(t, err, store.ErrNotFound)
}

// A git project dispatched by slug (no local path on the broker) lives in
// ~/.scion/projects/<slug> there and may have an NFS tree, so the broker is
// asked to clean up. A broker where the project is linked is not.
func TestDeleteProject_GitBacked_SlugProvider_DispatchesCleanup(t *testing.T) {
	e := newNFSDeleteTestEnv(t, "git-slug-cleanup")
	slugBroker := e.addBroker(t, "slug", "")
	e.addBroker(t, "linked", "/srv/checkouts/git-slug-cleanup")
	mockClient := &mockRuntimeBrokerClient{}
	e.srv.SetDispatcher(NewHTTPAgentDispatcherWithClient(e.s, mockClient, false, slog.Default()))

	e.deleteProject(t)

	assert.Equal(t, 1, mockClient.cleanupCalls, "only the broker without a local path is asked to clean up")
	assert.Equal(t, slugBroker.ID, mockClient.lastBrokerID)
	assert.Equal(t, []string{"git-slug-cleanup"}, mockClient.cleanupSlugs)
	assert.Equal(t, []string{e.project.ID}, mockClient.cleanupProjectIDs,
		"the broker needs the project ID to find the NFS tree")
}

// A failing broker cleanup is logged and does not fail the delete.
func TestDeleteProject_GitBacked_BrokerCleanupErrorDoesNotFailDelete(t *testing.T) {
	e := newNFSDeleteTestEnv(t, "git-cleanup-err")
	e.addBroker(t, "slug", "")
	mockClient := &mockRuntimeBrokerClient{cleanupErr: assert.AnError}
	e.srv.SetDispatcher(NewHTTPAgentDispatcherWithClient(e.s, mockClient, false, slog.Default()))

	e.deleteProject(t)
	assert.Equal(t, 1, mockClient.cleanupCalls)
}

func TestDeleteProject_EmbeddedBroker_NFS_RemovesExportTree(t *testing.T) {
	e := newNFSDeleteTestEnv(t, "embedded-nfs")
	embedded := e.addBroker(t, "embedded", "")
	e.srv.SetEmbeddedBrokerID(embedded.ID)
	tree := e.seedTree(t, e.project.ID)
	sibling := e.seedTree(t, tid("project-sibling"))

	e.deleteProject(t)

	_, err := os.Lstat(tree)
	assert.True(t, os.IsNotExist(err), "deleted project's NFS tree must be removed (err=%v)", err)
	_, err = os.Stat(filepath.Join(sibling, "provision", "lock"))
	assert.NoError(t, err, "sibling project's tree must be untouched")
	_, err = os.Stat(e.subRoot)
	assert.NoError(t, err, "subpath root must be kept")
}

func TestDeleteProject_EmbeddedBroker_NFS_MissingTreeIsSuccess(t *testing.T) {
	e := newNFSDeleteTestEnv(t, "embedded-nfs-missing")
	e.srv.SetEmbeddedBrokerID(e.addBroker(t, "embedded", "").ID)
	e.deleteProject(t)
}

// A guard refusal (the project directory is a symlink) is logged and does
// not fail the delete; nothing behind the symlink is removed.
func TestDeleteProject_EmbeddedBroker_NFS_CleanupErrorDoesNotFailDelete(t *testing.T) {
	e := newNFSDeleteTestEnv(t, "embedded-nfs-err")
	e.srv.SetEmbeddedBrokerID(e.addBroker(t, "embedded", "").ID)
	sibling := e.seedTree(t, tid("project-sibling-err"))
	require.NoError(t, os.Symlink(sibling, filepath.Join(e.subRoot, e.project.ID)))

	e.deleteProject(t)

	_, err := os.Stat(filepath.Join(sibling, "workspace", "README.md"))
	assert.NoError(t, err, "symlink target must be untouched")
}

// Without an embedded broker the hub still removes the ID-keyed tree from
// its own mount of the export, whatever provider rows exist: here the only
// provider has the project linked at a local path, so no broker is asked.
func TestDeleteProject_NoEmbeddedBroker_NFS_HubRemovesExportTree(t *testing.T) {
	e := newNFSDeleteTestEnv(t, "remote-nfs")
	e.addBroker(t, "linked", "/srv/checkouts/remote-nfs")
	mockClient := &mockRuntimeBrokerClient{}
	e.srv.SetDispatcher(NewHTTPAgentDispatcherWithClient(e.s, mockClient, false, slog.Default()))
	tree := e.seedTree(t, e.project.ID)

	e.deleteProject(t)

	assert.Equal(t, 0, mockClient.cleanupCalls)
	_, err := os.Lstat(tree)
	assert.True(t, os.IsNotExist(err), "deleted project's NFS tree must be removed (err=%v)", err)
}

// A share that is not mounted on the hub is nothing to remove.
func TestDeleteProject_NFS_ShareNotMountedOnHubIsSuccess(t *testing.T) {
	e := newNFSDeleteTestEnv(t, "unmounted-nfs")
	e.srv.config.WorkspaceStorageConfig.NFS.MountRoot = filepath.Join(e.home, "absent")
	e.deleteProject(t)
}

// The hub's workspace storage config is not validated like a broker's, so
// the guard must keep the share host base strictly under the mount root: a
// share ID of ".." must not reach a tree beside the mount root.
func TestDeleteProject_NFS_HostBaseOutsideMountRootRefused(t *testing.T) {
	e := newNFSDeleteTestEnv(t, "dotdot-share")
	nfs := e.srv.config.WorkspaceStorageConfig.NFS
	nfs.Shares[0].ID = ".."
	victim := filepath.Join(filepath.Dir(nfs.MountRoot), "projects", e.project.ID)
	require.NoError(t, os.MkdirAll(victim, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(victim, "keep.txt"), []byte("x"), 0o644))

	e.deleteProject(t)

	_, err := os.Stat(filepath.Join(victim, "keep.txt"))
	assert.NoError(t, err, "a tree outside the mount root must be untouched")
}

func TestDeleteProject_EmbeddedBroker_GitSlugProvider_RemovesLocalDir(t *testing.T) {
	e := newNFSDeleteTestEnv(t, "embedded-git-local")
	e.srv.SetEmbeddedBrokerID(e.addBroker(t, "embedded", "").ID)
	local := filepath.Join(e.home, ".scion", "projects", e.project.Slug)
	require.NoError(t, os.MkdirAll(filepath.Join(local, ".scion", "agents", "dev"), 0o755))
	other := filepath.Join(e.home, ".scion", "projects", "other-project")
	require.NoError(t, os.MkdirAll(other, 0o755))

	e.deleteProject(t)

	_, err := os.Lstat(local)
	assert.True(t, os.IsNotExist(err), "embedded broker's project directory must be removed (err=%v)", err)
	_, err = os.Stat(other)
	assert.NoError(t, err, "other project directories must be untouched")
}

func TestDeleteProject_EmbeddedBroker_GitLinkedProvider_KeepsLocalDir(t *testing.T) {
	e := newNFSDeleteTestEnv(t, "embedded-git-linked")
	e.srv.SetEmbeddedBrokerID(e.addBroker(t, "embedded", "/srv/checkouts/embedded-git-linked").ID)
	local := filepath.Join(e.home, ".scion", "projects", e.project.Slug)
	require.NoError(t, os.MkdirAll(local, 0o755))

	e.deleteProject(t)

	_, err := os.Stat(local)
	assert.NoError(t, err, "a linked project's slug directory is not the hub's to remove")
}

// setHubAttemptHook installs hubNFSCleanupAttemptHook (with a short retry
// delay) for one test and returns the attempts seen.
func setHubAttemptHook(t *testing.T, fn func(attempt int)) *[]int {
	t.Helper()
	var seen []int
	origDelay := hubNFSProjectCleanupRetryDelay
	hubNFSProjectCleanupRetryDelay = time.Millisecond
	hubNFSCleanupAttemptHook = func(attempt int) {
		seen = append(seen, attempt)
		if fn != nil {
			fn(attempt)
		}
	}
	t.Cleanup(func() { hubNFSCleanupAttemptHook, hubNFSProjectCleanupRetryDelay = nil, origDelay })
	return &seen
}

// makeWorkspaceUnremovable makes the tree's workspace dir read-only, so a
// removal attempt fails, and returns a func that undoes it.
func makeWorkspaceUnremovable(t *testing.T, tree string) func() {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	ws := filepath.Join(tree, "workspace")
	require.NoError(t, os.Chmod(ws, 0o555))
	restore := func() { _ = os.Chmod(ws, 0o755) }
	t.Cleanup(restore)
	return restore
}

// A failed removal is retried once; attempt 2 runs and removes the tree.
func TestDeleteProject_NFS_HubRetriesFailedRemoval(t *testing.T) {
	e := newNFSDeleteTestEnv(t, "hub-retry")
	tree := e.seedTree(t, e.project.ID)
	restore := makeWorkspaceUnremovable(t, tree)
	seen := setHubAttemptHook(t, func(attempt int) {
		if attempt == 2 {
			restore()
		}
	})

	e.deleteProject(t)

	assert.Equal(t, []int{1, 2}, *seen)
	_, err := os.Lstat(tree)
	assert.True(t, os.IsNotExist(err), "tree must be removed by the retry (err=%v)", err)
}

// ptone/scion#2569 review: a project re-registered with the same ID while the
// retry is pending keeps its tree.
func TestDeleteProject_NFS_ProjectRecreatedBeforeRetryKeepsTree(t *testing.T) {
	e := newNFSDeleteTestEnv(t, "hub-recreated")
	tree := e.seedTree(t, e.project.ID)
	restore := makeWorkspaceUnremovable(t, tree)
	seen := setHubAttemptHook(t, func(attempt int) {
		if attempt == 2 {
			restore()
			again := *e.project
			assert.NoError(t, e.s.CreateProject(context.Background(), &again))
		}
	})

	e.deleteProject(t)

	assert.Equal(t, []int{1, 2}, *seen)
	_, err := os.Stat(filepath.Join(tree, "workspace", "README.md"))
	assert.NoError(t, err, "the re-created project's tree must be kept")
}

// The check runs before the first attempt too.
func TestDeleteProject_NFS_ProjectRecreatedBeforeFirstAttemptKeepsTree(t *testing.T) {
	e := newNFSDeleteTestEnv(t, "hub-recreated-first")
	tree := e.seedTree(t, e.project.ID)
	seen := setHubAttemptHook(t, func(attempt int) {
		if attempt == 1 {
			again := *e.project
			assert.NoError(t, e.s.CreateProject(context.Background(), &again))
		}
	})

	rec := doRequest(t, e.srv, http.MethodDelete, "/api/v1/projects/"+e.project.ID, nil)
	require.Equal(t, http.StatusNoContent, rec.Code, "body: %s", rec.Body.String())
	e.srv.nfsCleanupWG.Wait()

	assert.Equal(t, []int{1}, *seen)
	_, err := os.Stat(filepath.Join(tree, "workspace", "README.md"))
	assert.NoError(t, err, "the re-created project's tree must be kept")
}
