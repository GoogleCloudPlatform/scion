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
// its in-memory server config, so on a hub configured with only
// server.workspace_storage: nfs (the Cloud Run hub + GKE + Filestore
// topology) a web upload lands in the uuid tree agents mount, and on a
// workstation (local) hub the slug-keyed paths are unchanged.

func sharedDirNFSStorage(mountRoot, shareID string) *config.V1SharedDirStorageConfig {
	return &config.V1SharedDirStorageConfig{
		Backend: "nfs",
		NFS: &config.V1NFSConfig{
			MountRoot: mountRoot,
			Shares:    []config.V1NFSShare{{ID: shareID, PVName: shareID + "-pv"}},
		},
	}
}

func TestSharedDirLayout_Precedence(t *testing.T) {
	ws := nfsConfig("/mnt/ws")                     // share1, default subpath root
	sd := sharedDirNFSStorage("/mnt/sd", "shared") // shared_dir_storage nfs
	tests := []struct {
		name       string
		ws         *config.V1WorkspaceStorageConfig
		sd         *config.V1SharedDirStorageConfig
		wantSource sharedDirLayoutSource
		wantBase   string
		wantErr    string
	}{
		{"nothing configured", nil, nil, sharedDirLayoutLocal, "", ""},
		{"workspace nfs only (T4)", ws, nil, sharedDirLayoutFromWorkspaceStorage, "/mnt/ws/share1", ""},
		{"shared_dir_storage nfs only (T3)", nil, sd, sharedDirLayoutFromSharedDirStorage, "/mnt/sd/shared", ""},
		{"both nfs: shared_dir_storage wins", ws, sd, sharedDirLayoutFromSharedDirStorage, "/mnt/sd/shared", ""},
		{"shared_dir_storage local, workspace nfs", ws, &config.V1SharedDirStorageConfig{Backend: "local"}, sharedDirLayoutFromWorkspaceStorage, "/mnt/ws/share1", ""},
		{"shared_dir_storage empty backend, workspace nfs", ws, &config.V1SharedDirStorageConfig{NFS: sd.NFS}, sharedDirLayoutFromWorkspaceStorage, "/mnt/ws/share1", ""},
		{"workspace cloudrun-volume stays local", &config.V1WorkspaceStorageConfig{Backend: "cloudrun-volume", CloudRunVolume: &config.V1CloudRunVolumeConfig{VolumeName: "v"}}, nil, sharedDirLayoutLocal, "", ""},
		{"workspace gke-shared-volume stays local", &config.V1WorkspaceStorageConfig{Backend: "gke-shared-volume", GKESharedVolume: &config.V1GKESharedVolumeConfig{VolumeName: "v"}}, nil, sharedDirLayoutLocal, "", ""},
		{"workspace local", &config.V1WorkspaceStorageConfig{Backend: "local"}, nil, sharedDirLayoutLocal, "", ""},
		{"invalid shared_dir_storage fails closed", ws, invalidNFSSharedDirStorageTestConfig(), sharedDirLayoutFromSharedDirStorage, "", "server.shared_dir_storage"},
		{"unloadable settings file fails closed", ws, nil, sharedDirLayoutFromSharedDirStorage, "", "global settings unreadable"},
		{"workspace nfs without shares", &config.V1WorkspaceStorageConfig{Backend: "nfs", NFS: &config.V1NFSConfig{MountRoot: "/mnt"}}, nil, sharedDirLayoutFromWorkspaceStorage, "", "no NFS shares"},
		{"workspace nfs without mount_root", &config.V1WorkspaceStorageConfig{Backend: "nfs", NFS: &config.V1NFSConfig{Shares: []config.V1NFSShare{{ID: "s"}}}}, nil, sharedDirLayoutFromWorkspaceStorage, "", "mount_root must be an absolute path"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := &Server{config: ServerConfig{WorkspaceStorageConfig: tt.ws, SharedDirStorageConfig: tt.sd}}
			if tt.name == "unloadable settings file fails closed" {
				srv.config.SharedDirStorageError = errors.New("server.shared_dir_storage: global settings unreadable")
			}
			layout, source, err := srv.sharedDirLayout()
			assert.Equal(t, tt.wantSource, source)
			if tt.wantErr != "" {
				require.ErrorContains(t, err, tt.wantErr)
				return
			}
			require.NoError(t, err)
			if tt.wantBase == "" {
				assert.Equal(t, projectfs.BackendLocal, layout.Backend)
				return
			}
			assert.Equal(t, projectfs.BackendNFS, layout.Backend)
			assert.Equal(t, tt.wantBase, layout.HostBase)
			assert.Equal(t, "projects", layout.SubPathRoot)
		})
	}
}

func TestSharedDirLayoutWarnings(t *testing.T) {
	ws := nfsConfig("/mnt/ws")
	assert.Empty(t, sharedDirLayoutWarnings(ws, nil))
	assert.Empty(t, sharedDirLayoutWarnings(nil, sharedDirNFSStorage("/mnt/sd", "shared")))
	same := &config.V1SharedDirStorageConfig{Backend: "nfs", NFS: ws.NFS}
	assert.Empty(t, sharedDirLayoutWarnings(ws, same), "identical blocks do not disagree")

	got := sharedDirLayoutWarnings(ws, sharedDirNFSStorage("/mnt/sd", "shared"))
	require.Len(t, got, 1)
	assert.Contains(t, got[0], "the hub serves shared dirs from server.shared_dir_storage and workspaces from server.workspace_storage")
	assert.Contains(t, got[0], "/mnt/sd/shared")
	assert.Contains(t, got[0], "/mnt/ws/share1")
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
	f.srv.config.WorkspaceStorageConfig = nfsConfig(f.mountRoot)
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

	f.srv.config.WorkspaceStorageConfig = nfsConfig(f.mountRoot)
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

	f.srv.config.WorkspaceStorageConfig = nfsConfig(f.mountRoot)
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

// TestSharedDirConfigDelete_WorkspaceStorageNFS_LeafKept: removing a shared
// dir declaration on a hub whose shared dirs are on the workspace export
// (only server.workspace_storage nfs) removes the declaration but leaves the
// leaf, which agents mount, in place until the lifecycle phase owns that
// deletion (ptone/scion#5835). Sibling shared dirs and the slug tree are
// untouched.
func TestSharedDirConfigDelete_WorkspaceStorageNFS_LeafKept(t *testing.T) {
	f := newSharedDirLayoutFixture(t)
	f.srv.config.WorkspaceStorageConfig = nfsConfig(f.mountRoot)
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
	for _, dir := range []string{"scratchpad", "custom"} {
		b, err := os.ReadFile(filepath.Join(sharedDirs, dir, "data.txt"))
		require.NoError(t, err, "%s: agent data on the workspace export must survive", dir)
		assert.Equal(t, dir, string(b))
	}
	_, err = os.Stat(resolveTestSharedDirPath(t, f.project, "custom"))
	assert.True(t, os.IsNotExist(err), "nothing is created or removed under the slug tree")
}

// TestSharedDirConfigDelete_SharedDirStorageNFS_LeafRemoved: with
// server.shared_dir_storage nfs the declaration DELETE still removes exactly
// that leaf (unchanged behaviour), leaving its siblings.
func TestSharedDirConfigDelete_SharedDirStorageNFS_LeafRemoved(t *testing.T) {
	f := newSharedDirLayoutFixture(t)
	f.srv.config.SharedDirStorageConfig = sharedDirNFSStorage(f.mountRoot, "sdshare")
	sharedDirs := filepath.Join(f.mountRoot, "sdshare", "projects", f.project.ID, "shared-dirs")
	for _, dir := range []string{"scratchpad", "custom"} {
		require.NoError(t, os.MkdirAll(filepath.Join(sharedDirs, dir), 0o2775))
		require.NoError(t, os.WriteFile(filepath.Join(sharedDirs, dir, "data.txt"), []byte(dir), 0o644))
	}

	rec := doRequest(t, f.srv, http.MethodDelete, fmt.Sprintf("/api/v1/projects/%s/shared-dirs/custom", f.project.ID), nil)
	require.Equal(t, http.StatusNoContent, rec.Code, "body: %s", rec.Body.String())

	_, err := os.Stat(filepath.Join(sharedDirs, "custom"))
	assert.True(t, os.IsNotExist(err), "the removed declaration's leaf is deleted")
	_, err = os.Stat(filepath.Join(sharedDirs, "scratchpad", "data.txt"))
	assert.NoError(t, err, "sibling shared dirs are untouched")
}

// TestSharedDirFiles_CloudRun_NonDurableSharedDirRefused pins the write gate
// on the resolved Mount (ptone/scion#5811): on a Cloud Run Service whose
// workspace is durable (a cloudrun-volume) but whose shared dirs resolve to
// instance-local disk, shared-dir writes (upload, write, delete) answer 503
// with a shared-dir specific error and create nothing; reads still work.
func TestSharedDirFiles_CloudRun_NonDurableSharedDirRefused(t *testing.T) {
	f := newSharedDirLayoutFixture(t)
	t.Setenv("K_SERVICE", "scion-hub")
	f.srv.config.WorkspaceStorageConfig = &config.V1WorkspaceStorageConfig{
		Backend:        "cloudrun-volume",
		CloudRunVolume: &config.V1CloudRunVolumeConfig{VolumeName: "ws"},
	}
	require.False(t, f.srv.workspaceWriteBlocked(), "sanity: workspace writes pass on the durable volume")

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
	f.srv.config.WorkspaceStorageConfig = nil
	f.srv.config.SharedDirStorageConfig = nil

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

// TestSharedDirFiles_BothNFS_SharedDirStorageWins: when both blocks are nfs,
// shared dirs follow server.shared_dir_storage (as brokers do today).
func TestSharedDirFiles_BothNFS_SharedDirStorageWins(t *testing.T) {
	f := newSharedDirLayoutFixture(t)
	f.srv.config.WorkspaceStorageConfig = nfsConfig(f.mountRoot)
	f.srv.config.SharedDirStorageConfig = sharedDirNFSStorage(f.mountRoot, "sdshare")
	require.NoError(t, os.MkdirAll(filepath.Join(f.mountRoot, "share1"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(f.mountRoot, "sdshare"), 0o755))

	rec := doRequest(t, f.srv, http.MethodPut, f.filesURL(f.project.ID, "scratchpad")+"/note.txt",
		ProjectWorkspaceWriteRequest{Content: "hello"})
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	_, err := os.Stat(filepath.Join(f.mountRoot, "sdshare", "projects", f.project.ID, "shared-dirs", "scratchpad", "note.txt"))
	require.NoError(t, err)
	_, err = os.Stat(filepath.Join(f.mountRoot, "share1", "projects"))
	assert.True(t, os.IsNotExist(err))
}
