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
		{"workspace nfs without shares", &config.V1WorkspaceStorageConfig{Backend: "nfs", NFS: &config.V1NFSConfig{MountRoot: "/mnt"}}, nil, sharedDirLayoutFromWorkspaceStorage, "", "no NFS shares"},
		{"workspace nfs without mount_root", &config.V1WorkspaceStorageConfig{Backend: "nfs", NFS: &config.V1NFSConfig{Shares: []config.V1NFSShare{{ID: "s"}}}}, nil, sharedDirLayoutFromWorkspaceStorage, "", "mount_root must be an absolute path"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := &Server{config: ServerConfig{WorkspaceStorageConfig: tt.ws, SharedDirStorageConfig: tt.sd}}
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
	addSharedDirToProject(t, srv, project.ID, "scratchpad")
	addSharedDirToProject(t, srv, project.ID, "custom")
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
	addSharedDirToProject(t, f.srv, project.ID, "scratchpad")
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

// TestSharedDirFiles_WorkspaceStorageNFS_EmbeddedLinkedKeepsLocal: with the
// Layout from server.workspace_storage, a git project linked through the
// co-located broker keeps that broker's local path (local-container brokers
// keep shared dirs local unless server.shared_dir_storage is nfs). On a
// Cloud Run Service that local path is not durable, so writes are refused
// before anything is created.
func TestSharedDirFiles_WorkspaceStorageNFS_EmbeddedLinkedKeepsLocal(t *testing.T) {
	f := newSharedDirLayoutFixture(t)
	project := createTestGitProject(t, f.srv, "Embedded Linked", "github.com/test/embedded-linked")
	addSharedDirToProject(t, f.srv, project.ID, "scratchpad")
	ctx := context.Background()
	b := &store.RuntimeBroker{
		ID: tid("embedded-linked-broker"), Name: "embedded", Slug: "embedded-linked-broker",
		Endpoint: "http://localhost:9800", Status: store.BrokerStatusOnline,
	}
	require.NoError(t, f.s.CreateRuntimeBroker(ctx, b))
	localScion := filepath.Join(t.TempDir(), "proj", ".scion")
	require.NoError(t, os.MkdirAll(localScion, 0o755))
	require.NoError(t, f.s.AddProjectProvider(ctx, &store.ProjectProvider{
		ProjectID: project.ID, BrokerID: b.ID, BrokerName: b.Name, Status: b.Status, LocalPath: localScion,
	}))
	f.srv.SetEmbeddedBrokerID(b.ID)
	f.srv.config.WorkspaceStorageConfig = nfsConfig(f.mountRoot)

	rec := doRequest(t, f.srv, http.MethodPut, f.filesURL(project.ID, "scratchpad")+"/note.txt",
		ProjectWorkspaceWriteRequest{Content: "hello"})
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	localLeaf := filepath.Join(filepath.Dir(localScion), "shared-dirs", "scratchpad")
	_, err := os.Stat(filepath.Join(localLeaf, "note.txt"))
	require.NoError(t, err, "the co-located broker's local path is kept")
	_, err = os.Stat(filepath.Join(f.mountRoot, "share1", "projects"))
	assert.True(t, os.IsNotExist(err), "the nfs tree is not touched")

	t.Setenv("K_SERVICE", "scion-hub")
	require.NoError(t, os.RemoveAll(localLeaf))
	rec = doRequest(t, f.srv, http.MethodPut, f.filesURL(project.ID, "scratchpad")+"/note.txt",
		ProjectWorkspaceWriteRequest{Content: "hello"})
	require.Equal(t, http.StatusServiceUnavailable, rec.Code, "body: %s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), "shared_dir_writes_unavailable")
	_, err = os.Stat(localLeaf)
	assert.True(t, os.IsNotExist(err), "a refused write must not create the directory")
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
