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
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/projectfs"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Phase 1 of the project storage cleanup (ptone/scion#5806): the hub's
// shared-dir browser resolves through pkg/projectfs on a Layout built from
// its in-memory server config (server.project_storage since phase 3), so on
// a hub with an nfs backend (the Cloud Run hub + GKE + Filestore topology)
// a web upload lands in the uuid tree agents mount, and on a workstation
// (local) hub the slug-keyed paths are unchanged.

// TestSharedDirLayout_ProjectStorage: the hub builds its shared-dir Layout
// from server.project_storage alone (phase 3, ptone/scion#5821): one
// backend places the workspace and the shared dirs, the volume backends put
// shared dirs on the volume's UUID tree (durable), and an unusable block
// fails closed.
func TestSharedDirLayout_ProjectStorage(t *testing.T) {
	saved := volumeMountBase
	volumeMountBase = "/vol"
	t.Cleanup(func() { volumeMountBase = saved })
	ws := projectStorageFromWorkspace(nfsConfig("/mnt/ws")) // share1, default subpath root
	tests := []struct {
		name        string
		ps          *config.V1ProjectStorageConfig
		wantSource  sharedDirLayoutSource
		wantBackend projectfs.Backend
		wantBase    string
		wantErr     string
	}{
		{"nothing configured", nil, sharedDirLayoutLocal, projectfs.BackendLocal, "", ""},
		{"nfs (T4)", ws, sharedDirLayoutFromProjectStorage, projectfs.BackendNFS, "/mnt/ws/share1", ""},
		{"cloudrun-volume", &config.V1ProjectStorageConfig{Backend: "cloudrun-volume", CloudRunVolume: &config.V1CloudRunVolumeConfig{VolumeName: "v"}}, sharedDirLayoutFromProjectStorage, projectfs.BackendCloudRunVolume, "/vol/v", ""},
		{"gke-shared-volume", &config.V1ProjectStorageConfig{Backend: "gke-shared-volume", GKESharedVolume: &config.V1GKESharedVolumeConfig{VolumeName: "v"}}, sharedDirLayoutFromProjectStorage, projectfs.BackendGKESharedVolume, "/vol/v", ""},
		{"local", &config.V1ProjectStorageConfig{Backend: "local"}, sharedDirLayoutLocal, projectfs.BackendLocal, "", ""},
		{"invalid block fails closed", invalidNFSSharedDirStorageTestConfig(), sharedDirLayoutFromProjectStorage, "", "", "absolute path on the hub"},
		{"nfs without shares", &config.V1ProjectStorageConfig{Backend: "nfs", NFS: &config.V1NFSConfig{MountRoot: "/mnt"}}, sharedDirLayoutFromProjectStorage, "", "", "no NFS shares"},
		{"volume without block", &config.V1ProjectStorageConfig{Backend: "cloudrun-volume"}, sharedDirLayoutFromProjectStorage, "", "", "volume_name is not set"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := &Server{config: ServerConfig{ProjectStorageConfig: tt.ps}}
			layout, source, err := srv.sharedDirLayout()
			assert.Equal(t, tt.wantSource, source)
			if tt.wantErr != "" {
				require.ErrorContains(t, err, tt.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.wantBackend, layout.Backend)
			if tt.wantBase == "" {
				return
			}
			assert.Equal(t, tt.wantBase, layout.HostBase)
			assert.Equal(t, "projects", layout.SubPathRoot)
			assert.True(t, layout.Backend.Durable(), "a shared backend is durable, so the Cloud Run write gate opens")
		})
	}
}

// declareSharedDir declares shared dir name on the project unless it is
// already declared (new projects get the default scratchpad).
func declareSharedDir(t *testing.T, srv *Server, projectID, name string) {
	t.Helper()
	p, err := srv.store.GetProject(context.Background(), projectID)
	require.NoError(t, err)
	for _, sd := range p.SharedDirs {
		if sd.Name == name {
			return
		}
	}
	addSharedDirToProject(t, srv, projectID, name)
}

// sharedDirLayoutFixture is a hub with a fresh HOME, a hub-managed project
// declaring shared dirs scratchpad and custom, and an NFS mount root.
type sharedDirLayoutFixture struct {
	srv       *Server
	s         store.Store
	project   *store.Project
	mountRoot string
}

func newSharedDirLayoutFixture(t *testing.T) sharedDirLayoutFixture {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("K_SERVICE", "")
	t.Setenv("CLOUD_RUN_INSTANCE", "")
	srv, s := testServer(t)
	project, _ := createTestHubManagedProject(t, srv, "Layout Project")
	declareSharedDir(t, srv, project.ID, "scratchpad")
	declareSharedDir(t, srv, project.ID, "custom")
	mountRoot := t.TempDir()
	return sharedDirLayoutFixture{srv: srv, s: s, project: project, mountRoot: mountRoot}
}

func (f sharedDirLayoutFixture) filesURL(projectID, dir string) string {
	return fmt.Sprintf("/api/v1/projects/%s/shared-dirs/%s/files", projectID, dir)
}

// TestSharedDirFiles_WorkspaceStorageNFS_UUIDTree pins the T4 fix: with only
// server.workspace_storage: nfs, a web upload to scratchpad or a custom
// shared dir lands at <mount_root>/<share>/projects/<project UUID>/
// shared-dirs/<name> (where Kubernetes pods mount it), not on the hub's
// local project-configs tree, and a file an agent writes there is listed.
func TestSharedDirFiles_WorkspaceStorageNFS_UUIDTree(t *testing.T) {
	f := newSharedDirLayoutFixture(t)
	t.Setenv("K_SERVICE", "scion-hub") // a Cloud Run Service: the write gate must pass on NFS
	f.srv.config.ProjectStorageConfig = projectStorageFromWorkspace(nfsConfig(f.mountRoot))
	require.NoError(t, os.MkdirAll(filepath.Join(f.mountRoot, "share1"), 0o755))

	for _, dir := range []string{"scratchpad", "custom"} {
		t.Run(dir, func(t *testing.T) {
			rec := doMultipartRequest(t, f.srv, http.MethodPost, f.filesURL(f.project.ID, dir),
				map[string][]byte{"from-web.txt": []byte("web")})
			require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

			leaf := filepath.Join(f.mountRoot, "share1", "projects", f.project.ID, "shared-dirs", dir)
			got, err := os.ReadFile(filepath.Join(leaf, "from-web.txt"))
			require.NoError(t, err, "the upload must land in the uuid tree agents mount")
			assert.Equal(t, "web", string(got))

			_, err = os.Stat(resolveTestSharedDirPath(t, f.project, dir))
			assert.True(t, os.IsNotExist(err), "nothing may be written to the hub's local slug-keyed tree")

			// What an agent writes at /scion-volumes/<dir> shows in the browser.
			require.NoError(t, os.WriteFile(filepath.Join(leaf, "from-agent.txt"), []byte("agent"), 0o644))
			rec = doRequest(t, f.srv, http.MethodGet, f.filesURL(f.project.ID, dir), nil)
			require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
			var resp SharedDirListResponse
			require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
			var names []string
			for _, file := range resp.Files {
				names = append(names, file.Path)
			}
			assert.ElementsMatch(t, []string{"from-web.txt", "from-agent.txt"}, names)
		})
	}
}

// TestSharedDirFiles_WorkspaceStorageNFS_RemoteLinkedProject: a git project
// whose only provider is a remote broker was refused (409, "requires a
// co-located runtime broker") because the hub had no path to it; on a
// shared Layout it resolves to the uuid tree like any other project.
func TestSharedDirFiles_WorkspaceStorageNFS_RemoteLinkedProject(t *testing.T) {
	f := newSharedDirLayoutFixture(t)
	project := createTestGitProject(t, f.srv, "Remote Linked", "github.com/test/remote-linked")
	declareSharedDir(t, f.srv, project.ID, "scratchpad")
	addProviderBroker(t, f.s, project.ID, "remote")

	rec := doRequest(t, f.srv, http.MethodGet, f.filesURL(project.ID, "scratchpad"), nil)
	require.Equal(t, http.StatusConflict, rec.Code, "sanity: the local layout cannot serve a remote-only project")

	f.srv.config.ProjectStorageConfig = projectStorageFromWorkspace(nfsConfig(f.mountRoot))
	require.NoError(t, os.MkdirAll(filepath.Join(f.mountRoot, "share1"), 0o755))
	rec = doRequest(t, f.srv, http.MethodPut, f.filesURL(project.ID, "scratchpad")+"/note.txt",
		ProjectWorkspaceWriteRequest{Content: "hello"})
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	_, err := os.Stat(filepath.Join(f.mountRoot, "share1", "projects", project.ID, "shared-dirs", "scratchpad", "note.txt"))
	assert.NoError(t, err)
}

// TestSharedDirFiles_WorkspaceStorageNFS_EmbeddedProvidedProjects: Q1 is
// applied exactly (fs-cleanup-arch R1): with the Layout from
// server.workspace_storage, projects the co-located broker provides, a git
// project linked with a LocalPath and a hub-native one, resolve to the uuid
// tree like every other project, not to the broker's local slug tree.
func TestSharedDirFiles_WorkspaceStorageNFS_EmbeddedProvidedProjects(t *testing.T) {
	f := newSharedDirLayoutFixture(t)
	ctx := context.Background()
	b := &store.RuntimeBroker{
		ID: tid("embedded-provider-broker"), Name: "embedded", Slug: "embedded-provider-broker",
		Endpoint: "http://localhost:9800", Status: store.BrokerStatusOnline,
	}
	require.NoError(t, f.s.CreateRuntimeBroker(ctx, b))
	f.srv.SetEmbeddedBrokerID(b.ID)

	linked := createTestGitProject(t, f.srv, "Embedded Linked", "github.com/test/embedded-linked")
	declareSharedDir(t, f.srv, linked.ID, "scratchpad")
	localScion := filepath.Join(t.TempDir(), "proj", ".scion")
	require.NoError(t, os.MkdirAll(localScion, 0o755))
	require.NoError(t, f.s.AddProjectProvider(ctx, &store.ProjectProvider{
		ProjectID: linked.ID, BrokerID: b.ID, BrokerName: b.Name, Status: b.Status, LocalPath: localScion,
	}))
	require.NoError(t, f.s.AddProjectProvider(ctx, &store.ProjectProvider{
		ProjectID: f.project.ID, BrokerID: b.ID, BrokerName: b.Name, Status: b.Status,
	}))

	f.srv.config.ProjectStorageConfig = projectStorageFromWorkspace(nfsConfig(f.mountRoot))
	require.NoError(t, os.MkdirAll(filepath.Join(f.mountRoot, "share1"), 0o755))

	for name, project := range map[string]*store.Project{"git linked": linked, "hub-native": f.project} {
		t.Run(name, func(t *testing.T) {
			rec := doRequest(t, f.srv, http.MethodPut, f.filesURL(project.ID, "scratchpad")+"/note.txt",
				ProjectWorkspaceWriteRequest{Content: "hello"})
			require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
			_, err := os.Stat(filepath.Join(f.mountRoot, "share1", "projects", project.ID, "shared-dirs", "scratchpad", "note.txt"))
			require.NoError(t, err, "the write lands in the uuid tree")
		})
	}
	_, err := os.Stat(filepath.Join(filepath.Dir(localScion), "shared-dirs"))
	assert.True(t, os.IsNotExist(err), "the co-located broker's local tree is not used")
	_, err = os.Stat(resolveTestSharedDirPath(t, f.project, "scratchpad"))
	assert.True(t, os.IsNotExist(err), "the hub's local slug tree is not used")
}

// TestSharedDirConfigDelete_ProjectStorageNFS_LeafRemoved: with one storage
// block, removing a shared dir declaration removes exactly that leaf from
// the project's shared tree, as on the local backend and as
// server.shared_dir_storage nfs did; sibling shared dirs and the slug tree
// are untouched.
func TestSharedDirConfigDelete_ProjectStorageNFS_LeafRemoved(t *testing.T) {
	f := newSharedDirLayoutFixture(t)
	f.srv.config.ProjectStorageConfig = projectStorageFromWorkspace(nfsConfig(f.mountRoot))
	sharedDirs := filepath.Join(f.mountRoot, "share1", "projects", f.project.ID, "shared-dirs")
	for _, dir := range []string{"scratchpad", "custom"} {
		require.NoError(t, os.MkdirAll(filepath.Join(sharedDirs, dir), 0o2775))
		require.NoError(t, os.WriteFile(filepath.Join(sharedDirs, dir, "data.txt"), []byte(dir), 0o644))
	}

	rec := doRequest(t, f.srv, http.MethodDelete, fmt.Sprintf("/api/v1/projects/%s/shared-dirs/custom", f.project.ID), nil)
	require.Equal(t, http.StatusNoContent, rec.Code, "body: %s", rec.Body.String())

	got, err := f.s.GetProject(context.Background(), f.project.ID)
	require.NoError(t, err)
	for _, d := range got.SharedDirs {
		assert.NotEqual(t, "custom", d.Name, "the declaration is removed")
	}
	_, err = os.Stat(filepath.Join(sharedDirs, "custom"))
	assert.True(t, os.IsNotExist(err), "the removed declaration's leaf is deleted")
	b, err := os.ReadFile(filepath.Join(sharedDirs, "scratchpad", "data.txt"))
	require.NoError(t, err, "sibling shared dirs are untouched")
	assert.Equal(t, "scratchpad", string(b))
	_, err = os.Stat(resolveTestSharedDirPath(t, f.project, "custom"))
	assert.True(t, os.IsNotExist(err), "nothing is created or removed under the slug tree")
}

// TestSharedDirFiles_CloudRun_NonDurableSharedDirRefused pins the write gate
// on the resolved Mount (ptone/scion#5811): on a Cloud Run Service whose
// shared dirs resolve to instance-local disk (no shared project_storage
// backend), shared-dir writes (upload, write, delete) answer 503 with a
// shared-dir specific error naming server.project_storage and create
// nothing; reads still work.
func TestSharedDirFiles_CloudRun_NonDurableSharedDirRefused(t *testing.T) {
	f := newSharedDirLayoutFixture(t)
	t.Setenv("K_SERVICE", "scion-hub")
	f.srv.config.ProjectStorageConfig = &config.V1ProjectStorageConfig{Backend: "local"}

	url := f.filesURL(f.project.ID, "scratchpad")
	for name, do := range map[string]func() int{
		"upload": func() int {
			return doMultipartRequest(t, f.srv, http.MethodPost, url, map[string][]byte{"a.txt": []byte("a")}).Code
		},
		"write": func() int {
			return doRequest(t, f.srv, http.MethodPut, url+"/a.txt", ProjectWorkspaceWriteRequest{Content: "a"}).Code
		},
		"delete": func() int { return doRequest(t, f.srv, http.MethodDelete, url+"/a.txt", nil).Code },
	} {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, http.StatusServiceUnavailable, do())
		})
	}
	rec := doRequest(t, f.srv, http.MethodPut, url+"/a.txt", ProjectWorkspaceWriteRequest{Content: "a"})
	assert.Contains(t, rec.Body.String(), "shared_dir_writes_unavailable")
	assert.Contains(t, rec.Body.String(), "not on durable storage")
	assert.Contains(t, rec.Body.String(), "server.project_storage")
	_, err := os.Stat(resolveTestSharedDirPath(t, f.project, "scratchpad"))
	assert.True(t, os.IsNotExist(err), "refused writes must not create the local directory")

	rec = doRequest(t, f.srv, http.MethodGet, url, nil)
	assert.Equal(t, http.StatusOK, rec.Code, "reads are not gated")

	require.NoError(t, os.MkdirAll(resolveTestSharedDirPath(t, f.project, "scratchpad"), 0o755))
	rec = doRequest(t, f.srv, http.MethodDelete, url, nil)
	assert.Equal(t, http.StatusMethodNotAllowed, rec.Code, "an unrouted DELETE still answers 405, not the write gate's 503")
}

// TestSharedDirFiles_CloudRun_NoWorkspaceStorageRefused pins the write gate
// on a Cloud Run Service with no workspace_storage and no
// shared_dir_storage at all: shared dirs resolve to instance-local disk, so upload,
// write and delete answer 503 and create nothing, while reads are allowed.
func TestSharedDirFiles_CloudRun_NoWorkspaceStorageRefused(t *testing.T) {
	f := newSharedDirLayoutFixture(t)
	t.Setenv("K_SERVICE", "scion-hub")
	f.srv.config.ProjectStorageConfig = nil

	url := f.filesURL(f.project.ID, "scratchpad")
	for name, do := range map[string]func() *httptest.ResponseRecorder{
		"upload": func() *httptest.ResponseRecorder {
			return doMultipartRequest(t, f.srv, http.MethodPost, url, map[string][]byte{"a.txt": []byte("a")})
		},
		"write": func() *httptest.ResponseRecorder {
			return doRequest(t, f.srv, http.MethodPut, url+"/a.txt", ProjectWorkspaceWriteRequest{Content: "a"})
		},
		"delete": func() *httptest.ResponseRecorder { return doRequest(t, f.srv, http.MethodDelete, url+"/a.txt", nil) },
	} {
		t.Run(name, func(t *testing.T) {
			rec := do()
			assert.Equal(t, http.StatusServiceUnavailable, rec.Code, "body: %s", rec.Body.String())
			assert.Contains(t, rec.Body.String(), "shared_dir_writes_unavailable")
			_, err := os.Stat(resolveTestSharedDirPath(t, f.project, "scratchpad"))
			assert.True(t, os.IsNotExist(err), "a refused write must not create the local directory")
		})
	}

	rec := doRequest(t, f.srv, http.MethodGet, url, nil)
	assert.Equal(t, http.StatusOK, rec.Code, "reads are not gated: %s", rec.Body.String())
}

// TestSharedDirFiles_LocalLayout_SlugPathUnchanged is the T1 regression
// check: with no storage configured (workstation server), the shared-dir
// browser writes to ~/.scion/project-configs/<slug>__<short id>/shared-dirs/<name>.
func TestSharedDirFiles_LocalLayout_SlugPathUnchanged(t *testing.T) {
	f := newSharedDirLayoutFixture(t)
	for _, dir := range []string{"scratchpad", "custom"} {
		rec := doRequest(t, f.srv, http.MethodPut, f.filesURL(f.project.ID, dir)+"/note.txt",
			ProjectWorkspaceWriteRequest{Content: "hello"})
		require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
		got, err := os.ReadFile(filepath.Join(resolveTestSharedDirPath(t, f.project, dir), "note.txt"))
		require.NoError(t, err)
		assert.Equal(t, "hello", string(got))
	}
}

// TestSharedDirFiles_CloudRunVolume_SharedDirsOnVolume: with
// server.project_storage backend cloudrun-volume the shared dirs are on the
// volume's UUID tree (one backend for workspace and shared dirs), so the
// Cloud Run write gate opens and a web upload lands where agents mount it (ptone/scion#5823 note).
func TestSharedDirFiles_CloudRunVolume_SharedDirsOnVolume(t *testing.T) {
	f := newSharedDirLayoutFixture(t)
	t.Setenv("K_SERVICE", "scion-hub")
	saved := volumeMountBase
	volumeMountBase = f.mountRoot
	t.Cleanup(func() { volumeMountBase = saved })
	require.NoError(t, os.MkdirAll(filepath.Join(f.mountRoot, "ws"), 0o755))
	f.srv.config.ProjectStorageConfig = &config.V1ProjectStorageConfig{
		Backend:        "cloudrun-volume",
		CloudRunVolume: &config.V1CloudRunVolumeConfig{VolumeName: "ws"},
	}

	rec := doRequest(t, f.srv, http.MethodPut, f.filesURL(f.project.ID, "scratchpad")+"/note.txt",
		ProjectWorkspaceWriteRequest{Content: "hello"})
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	got, err := os.ReadFile(filepath.Join(f.mountRoot, "ws", "projects", f.project.ID, "shared-dirs", "scratchpad", "note.txt"))
	require.NoError(t, err, "the write lands in the volume's uuid tree")
	assert.Equal(t, "hello", string(got))
	_, err = os.Stat(resolveTestSharedDirPath(t, f.project, "scratchpad"))
	assert.True(t, os.IsNotExist(err), "nothing is written to the hub's local slug-keyed tree")
}
