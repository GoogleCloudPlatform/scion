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
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// embeddedCleanupFixture is a combined hub+broker server with HOME pointed
// at a temporary directory and one hub-native project in the store.
type embeddedCleanupFixture struct {
	srv       *Server
	store     store.Store
	home      string
	project   *store.Project
	localPath string // ~/.scion/projects/<slug>
}

func newEmbeddedCleanupFixture(t *testing.T, slug string, wsCfg func(home string) *config.V1WorkspaceStorageConfig, embedded bool) *embeddedCleanupFixture {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)

	srv, s := testServer(t)
	if wsCfg != nil {
		srv.config.WorkspaceStorageConfig = wsCfg(home)
	}

	ctx := context.Background()
	project := &store.Project{
		ID:   tid("project-" + slug),
		Slug: slug,
		Name: "Project " + slug,
	}
	require.NoError(t, s.CreateProject(ctx, project))

	if embedded {
		broker := &store.RuntimeBroker{
			ID:       tid("broker-" + slug),
			Slug:     "broker-" + slug,
			Name:     "Broker " + slug,
			Status:   store.BrokerStatusOnline,
			Endpoint: "http://localhost:9800",
		}
		require.NoError(t, s.CreateRuntimeBroker(ctx, broker))
		require.NoError(t, s.AddProjectProvider(ctx, &store.ProjectProvider{
			ProjectID:  project.ID,
			BrokerID:   broker.ID,
			BrokerName: broker.Name,
			LinkedBy:   "auto-provide",
		}))
		srv.SetEmbeddedBrokerID(broker.ID)
	}

	localPath, err := localProjectPath(slug)
	require.NoError(t, err)
	require.Equal(t, filepath.Join(home, ".scion", "projects", slug), localPath)

	return &embeddedCleanupFixture{srv: srv, store: s, home: home, project: project, localPath: localPath}
}

// writeTree creates each relative file under root with fixed content.
func writeTree(t *testing.T, root string, files ...string) {
	t.Helper()
	for _, f := range files {
		p := filepath.Join(root, f)
		require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o755))
		require.NoError(t, os.WriteFile(p, []byte("content"), 0o644))
	}
}

func assertAbsent(t *testing.T, path, msg string) {
	t.Helper()
	_, err := os.Stat(path)
	assert.True(t, os.IsNotExist(err), "%s: %s (stat err: %v)", msg, path, err)
}

func (f *embeddedCleanupFixture) deleteProject(t *testing.T) {
	t.Helper()
	rec := doRequest(t, f.srv, http.MethodDelete, "/api/v1/projects/"+f.project.ID, nil)
	require.Equal(t, http.StatusNoContent, rec.Code, "body: %s", rec.Body.String())
	_, err := f.store.GetProject(context.Background(), f.project.ID)
	assert.ErrorIs(t, err, store.ErrNotFound)
}

func nfsWorkspaceStorage(home string) *config.V1WorkspaceStorageConfig {
	return &config.V1WorkspaceStorageConfig{
		Backend: "nfs",
		NFS: &config.V1NFSConfig{
			MountRoot: filepath.Join(home, "nfs"),
			Shares:    []config.V1NFSShare{{ID: "share1", Server: "10.0.0.2", Export: "/scion"}},
		},
	}
}

func cloudRunVolumeWorkspaceStorage(string) *config.V1WorkspaceStorageConfig {
	return &config.V1WorkspaceStorageConfig{
		Backend:        "cloudrun-volume",
		CloudRunVolume: &config.V1CloudRunVolumeConfig{VolumeName: "workspace-vol"},
	}
}

func gkeSharedVolumeWorkspaceStorage(string) *config.V1WorkspaceStorageConfig {
	return &config.V1WorkspaceStorageConfig{
		Backend: "gke-shared-volume",
		GKESharedVolume: &config.V1GKESharedVolumeConfig{
			VolumeName:  "workspace-vol",
			PVClaimName: "scion-workspaces",
		},
	}
}

// TestDeleteProject_BackendConfiguredCombinedServer_RemovesLocalProjectDir
// verifies that on a combined server with a workspace storage backend, project
// delete removes both the backend project directory and the embedded broker's
// local ~/.scion/projects/<slug> directory.
func TestDeleteProject_BackendConfiguredCombinedServer_RemovesLocalProjectDir(t *testing.T) {
	tests := []struct {
		name  string
		wsCfg func(home string) *config.V1WorkspaceStorageConfig
	}{
		{"nfs", nfsWorkspaceStorage},
		{"cloudrun-volume", cloudRunVolumeWorkspaceStorage},
		{"gke-shared-volume", gkeSharedVolumeWorkspaceStorage},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			prevMountBase := volumeMountBase
			volumeMountBase = t.TempDir()
			t.Cleanup(func() { volumeMountBase = prevMountBase })

			f := newEmbeddedCleanupFixture(t, "backend-local-"+tt.name, tt.wsCfg, true)

			backendPath, err := f.srv.hubManagedProjectPath(f.project.Slug)
			require.NoError(t, err)
			require.NotEqual(t, f.localPath, backendPath, "backend path must differ from the local path")

			writeTree(t, backendPath, "README.md")
			writeTree(t, f.localPath,
				"README.md",
				".scion/agents/worker/home/notes.txt",
				".scion/agents/worker/workspace/main.go",
			)
			// A sibling project's local directory must survive.
			siblingPath := filepath.Join(f.home, ".scion", "projects", "sibling-project")
			writeTree(t, siblingPath, "keep.txt")

			f.deleteProject(t)

			assertAbsent(t, backendPath, "backend project directory should be removed")
			assertAbsent(t, f.localPath, "embedded broker local project directory should be removed")
			_, err = os.Stat(filepath.Join(siblingPath, "keep.txt"))
			assert.NoError(t, err, "sibling project directory must be untouched")
		})
	}
}

// TestDeleteProject_LocalBackendCombinedServer_RemovesProjectDir verifies the
// default local backend on a combined server: the hub-managed path is the
// local project directory, and project delete removes it.
func TestDeleteProject_LocalBackendCombinedServer_RemovesProjectDir(t *testing.T) {
	f := newEmbeddedCleanupFixture(t, "local-default", nil, true)

	hubPath, err := f.srv.hubManagedProjectPath(f.project.Slug)
	require.NoError(t, err)
	require.Equal(t, f.localPath, hubPath)

	writeTree(t, f.localPath, "README.md", ".scion/agents/worker/home/notes.txt")
	siblingPath := filepath.Join(f.home, ".scion", "projects", "sibling-project")
	writeTree(t, siblingPath, "keep.txt")

	f.deleteProject(t)

	assertAbsent(t, f.localPath, "local project directory should be removed")
	_, err = os.Stat(filepath.Join(siblingPath, "keep.txt"))
	assert.NoError(t, err, "sibling project directory must be untouched")
}

// TestDeleteProject_BackendConfiguredCombinedServer_LocalProjectDirAbsent
// verifies that project delete succeeds when the embedded broker's local
// project directory does not exist.
func TestDeleteProject_BackendConfiguredCombinedServer_LocalProjectDirAbsent(t *testing.T) {
	f := newEmbeddedCleanupFixture(t, "local-absent", nfsWorkspaceStorage, true)

	backendPath, err := f.srv.hubManagedProjectPath(f.project.Slug)
	require.NoError(t, err)
	writeTree(t, backendPath, "README.md")
	assertAbsent(t, f.localPath, "precondition: local project directory absent")

	f.deleteProject(t)

	assertAbsent(t, backendPath, "backend project directory should be removed")
	assertAbsent(t, f.localPath, "local project directory should remain absent")
}

// TestDeleteProject_BackendConfiguredHubOnly_KeepsLocalPathHandling verifies
// that a hub without an embedded broker keeps its existing delete behaviour:
// only the backend project directory is removed.
func TestDeleteProject_BackendConfiguredHubOnly_KeepsLocalPathHandling(t *testing.T) {
	f := newEmbeddedCleanupFixture(t, "hub-only", nfsWorkspaceStorage, false)

	backendPath, err := f.srv.hubManagedProjectPath(f.project.Slug)
	require.NoError(t, err)
	writeTree(t, backendPath, "README.md")
	writeTree(t, f.localPath, ".scion/settings.yaml")

	f.deleteProject(t)

	assertAbsent(t, backendPath, "backend project directory should be removed")
	_, err = os.Stat(filepath.Join(f.localPath, ".scion", "settings.yaml"))
	assert.NoError(t, err, "without an embedded broker the local path is not removed")
}

// TestDeleteProject_BackendConfiguredCombinedServer_RemovesOnlyDirectChildOfProjectsRoot
// verifies that the embedded broker's local removal is confined to a single
// direct child of ~/.scion/projects: for a stored slug that does not name
// such a child, sibling project directories on the local disk are kept.
func TestDeleteProject_BackendConfiguredCombinedServer_RemovesOnlyDirectChildOfProjectsRoot(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	srv, s := testServer(t)
	srv.config.WorkspaceStorageConfig = nfsWorkspaceStorage(home)

	ctx := context.Background()
	project := &store.Project{
		ID:   tid("project-not-single-child"),
		Slug: ".",
		Name: "Project not single child",
	}
	require.NoError(t, s.CreateProject(ctx, project))

	broker := &store.RuntimeBroker{
		ID:       tid("broker-not-single-child"),
		Slug:     "broker-not-single-child",
		Name:     "Broker not single child",
		Status:   store.BrokerStatusOnline,
		Endpoint: "http://localhost:9800",
	}
	require.NoError(t, s.CreateRuntimeBroker(ctx, broker))
	require.NoError(t, s.AddProjectProvider(ctx, &store.ProjectProvider{
		ProjectID:  project.ID,
		BrokerID:   broker.ID,
		BrokerName: broker.Name,
		LinkedBy:   "auto-provide",
	}))
	srv.SetEmbeddedBrokerID(broker.ID)

	// Backend content so the hub-managed path resolves to the backend mount.
	writeTree(t, filepath.Join(home, "nfs", "share1", "hub-projects", "other-project"), "README.md")
	siblingFile := filepath.Join(home, ".scion", "projects", "other-project", "keep.txt")
	writeTree(t, filepath.Dir(siblingFile), "keep.txt")

	rec := doRequest(t, srv, http.MethodDelete, "/api/v1/projects/"+project.ID, nil)
	require.Equal(t, http.StatusNoContent, rec.Code, "body: %s", rec.Body.String())

	_, err := os.Stat(siblingFile)
	assert.NoError(t, err, "sibling project directory on the embedded broker's local disk must be kept")
}

// TestRemoveEmbeddedBrokerProjectDir_RemovedPathCases verifies how the
// helper treats the hub-managed path already removed: an equal path leaves
// the local directory to that removal, and an empty or different path
// removes the local directory.
func TestRemoveEmbeddedBrokerProjectDir_RemovedPathCases(t *testing.T) {
	tests := []struct {
		name        string
		removedPath func(localPath, home string) string
		wantRemoved bool
	}{
		{"equal path", func(localPath, _ string) string { return localPath }, false},
		{"equal path uncleaned", func(localPath, _ string) string { return localPath + string(filepath.Separator) }, false},
		{"empty path", func(string, string) string { return "" }, true},
		{"backend path", func(_, home string) string {
			return filepath.Join(home, "nfs", "share1", "hub-projects", "removed-path")
		}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newEmbeddedCleanupFixture(t, "removed-path", nil, true)
			writeTree(t, f.localPath, "README.md")

			f.srv.removeEmbeddedBrokerProjectDir(f.project.ID, f.project.Slug, tt.removedPath(f.localPath, f.home))

			_, err := os.Stat(f.localPath)
			if tt.wantRemoved {
				assert.True(t, os.IsNotExist(err), "local project directory should be removed (stat err: %v)", err)
			} else {
				assert.NoError(t, err, "local project directory should be left to the hub-managed removal")
			}
		})
	}
}
