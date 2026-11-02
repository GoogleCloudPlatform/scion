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
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// projectDirStorageCase describes a workspace storage configuration.
// shared reports whether it is a shared backend: there the hub-managed
// workspace is the ID-keyed uuid tree, and project delete removes no
// slug-keyed directory (the uuid tree is removed by the ID-keyed tree
// cleanup; see project_delete_nfs_cleanup_test.go).
type projectDirStorageCase struct {
	name      string
	shared    bool
	configure func(t *testing.T, srv *Server, base string)
}

func projectDirStorageCases() []projectDirStorageCase {
	return []projectDirStorageCase{
		{
			name:      "default",
			configure: func(t *testing.T, srv *Server, base string) {},
		},
		{
			name:   "nfs",
			shared: true,
			configure: func(t *testing.T, srv *Server, base string) {
				srv.config.ProjectStorageConfig = &config.V1ProjectStorageConfig{
					Backend: "nfs",
					NFS: &config.V1NFSConfig{
						MountRoot: filepath.Join(base, "nfs"),
						Shares:    []config.V1NFSShare{{ID: "share1", Server: "10.0.0.2", Export: "/scion"}},
					},
				}
			},
		},
		{
			name:   "cloudrun-volume",
			shared: true,
			configure: func(t *testing.T, srv *Server, base string) {
				setVolumeMountBase(t, filepath.Join(base, "mnt"))
				srv.config.ProjectStorageConfig = &config.V1ProjectStorageConfig{
					Backend:        "cloudrun-volume",
					CloudRunVolume: &config.V1CloudRunVolumeConfig{VolumeName: "workspace-vol"},
				}
			},
		},
		{
			name:   "gke-shared-volume",
			shared: true,
			configure: func(t *testing.T, srv *Server, base string) {
				setVolumeMountBase(t, filepath.Join(base, "mnt"))
				srv.config.ProjectStorageConfig = &config.V1ProjectStorageConfig{
					Backend: "gke-shared-volume",
					GKESharedVolume: &config.V1GKESharedVolumeConfig{
						VolumeName:  "workspace-vol",
						PVClaimName: "scion-workspaces",
					},
				}
			},
		},
	}
}

// storeHubManagedProject inserts a hub-managed project record directly into
// the store, so the slug is exactly the stored value.
func storeHubManagedProject(t *testing.T, s store.Store, id, slug string) {
	t.Helper()
	now := time.Now()
	require.NoError(t, s.CreateProject(context.Background(), &store.Project{
		ID: id, Name: "stored " + id, Slug: slug, Created: now, Updated: now,
	}))
}

// TestDeleteProject_StoredSlugNotDirectChild_RemovesNoDirectory checks that
// deleting a stored record whose slug does not resolve to a direct child of
// the local projects root removes the record and leaves the projects root
// and every sibling project directory in place.
func TestDeleteProject_StoredSlugNotDirectChild_RemovesNoDirectory(t *testing.T) {
	for _, tc := range projectDirStorageCases() {
		t.Run(tc.name, func(t *testing.T) {
			tmpHome := t.TempDir()
			t.Setenv("HOME", tmpHome)
			srv, s := testServer(t)
			tc.configure(t, srv, tmpHome)
			localRoot := filepath.Join(tmpHome, ".scion", "projects")
			kept := writeProjectDirFile(t, filepath.Join(localRoot, "sibling-project"))

			const id = "aaaaaaaa-0000-4000-8000-000000000001"
			storeHubManagedProject(t, s, id, ".")

			rec := doRequest(t, srv, http.MethodGet, "/api/v1/projects/"+id, nil)
			assert.Equal(t, http.StatusOK, rec.Code, "the stored record stays readable: %s", rec.Body.String())
			rec = doRequest(t, srv, http.MethodGet, "/api/v1/projects", nil)
			assert.Equal(t, http.StatusOK, rec.Code)
			assert.Contains(t, rec.Body.String(), id, "the stored record stays listed")

			logs := captureProjectsLog(t, srv)
			rec = doRequest(t, srv, http.MethodDelete, "/api/v1/projects/"+id, nil)
			requireProjectDeleteResponse(t, rec)
			srv.storageCleanupWG.Wait()

			logged := logs.String()
			if !tc.shared {
				assert.Contains(t, logged, "hub-managed project directory not removed: it is not a direct child of a projects root", "the skipped removal is logged")
				assert.Contains(t, logged, "project_id="+id, "the log line names the project")
			}
			assert.NotContains(t, logged, tmpHome, "the log carries no filesystem path")

			_, err := s.GetProject(context.Background(), id)
			assert.ErrorIs(t, err, store.ErrNotFound, "the record is deleted")
			_, err = os.Stat(kept)
			assert.NoError(t, err, "sibling project content %s must remain", kept)
		})
	}
}

// TestRemoveProjectDirUnderProjectsRoot_RemovesOnlyDirectChild checks the
// removal step on its own, independent of slug validation: given a resolved
// path that is the local projects root or lies outside a direct child, it
// removes nothing; given a direct child, it removes exactly that directory.
func TestRemoveProjectDirUnderProjectsRoot_RemovesOnlyDirectChild(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	srv, _ := testServer(t)

	root := filepath.Join(tmpHome, ".scion", "projects")
	outside := writeProjectDirFile(t, filepath.Join(tmpHome, "outside"))
	sibling := writeProjectDirFile(t, filepath.Join(root, "sibling-project"))
	nested := writeProjectDirFile(t, filepath.Join(root, "sibling-project", "nested"))

	for _, target := range []string{
		root,
		root + "/.",
		root + "/",
		filepath.Join(root, "sibling-project", "nested"),
		root + "/sibling-project/../..",
		filepath.Dir(root),
		filepath.Join(tmpHome, "outside"),
	} {
		srv.removeProjectDirUnderProjectsRoot("p", target)
	}
	for _, f := range []string{sibling, nested, outside} {
		_, err := os.Stat(f)
		assert.NoError(t, err, "%s must remain", f)
	}

	own := filepath.Join(root, "own-project")
	writeProjectDirFile(t, own)
	srv.removeProjectDirUnderProjectsRoot("p", own)
	_, err := os.Stat(own)
	assert.True(t, os.IsNotExist(err), "a direct child of the projects root is removed")
	_, err = os.Stat(sibling)
	assert.NoError(t, err, "sibling must remain after removing a direct child")
}

// TestDeleteProject_RemovesExactlyOwnDirectory checks that a normal delete on
// the local backend removes the project's own directory and nothing else,
// and that on a shared backend it removes no slug-keyed directory at all.
func TestDeleteProject_RemovesExactlyOwnDirectory(t *testing.T) {
	for _, tc := range projectDirStorageCases() {
		t.Run(tc.name, func(t *testing.T) {
			tmpHome := t.TempDir()
			t.Setenv("HOME", tmpHome)
			srv, s := testServer(t)
			tc.configure(t, srv, tmpHome)

			root := filepath.Join(tmpHome, ".scion", "projects")
			const slug = "own-project"
			own := writeProjectDirFile(t, filepath.Join(root, slug))
			sibling := writeProjectDirFile(t, filepath.Join(root, "sibling-project"))

			const id = "aaaaaaaa-0000-4000-8000-000000000002"
			storeHubManagedProject(t, s, id, slug)

			rec := doRequest(t, srv, http.MethodDelete, "/api/v1/projects/"+id, nil)
			requireProjectDeleteResponse(t, rec)
			srv.storageCleanupWG.Wait()

			_, err := os.Stat(own)
			if tc.shared {
				assert.NoError(t, err, "on a shared backend the hub removes no slug-keyed directory")
			} else {
				assert.True(t, os.IsNotExist(err), "the project's own directory is removed")
			}
			_, err = os.Stat(sibling)
			assert.NoError(t, err, "the sibling project directory remains")
			_, err = os.Stat(root)
			assert.NoError(t, err, "the projects root remains")
		})
	}
}

// TestRemoveProjectDirUnderProjectsRoot_RelativePathRemovesNothing checks
// that a relative directory under the working directory is never removed.
func TestRemoveProjectDirUnderProjectsRoot_RelativePathRemovesNothing(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	srv, _ := testServer(t)

	workDir := t.TempDir()
	t.Chdir(workDir)
	kept := writeProjectDirFile(t, filepath.Join(workDir, "projects", "own-project"))

	srv.removeProjectDirUnderProjectsRoot("p", filepath.Join("projects", "own-project"))

	_, err := os.Stat(kept)
	assert.NoError(t, err, "a relative directory is never removed")
}

// TestRemoveHubManagedProjectDir_ReturnsResolvedPathOrEmpty checks the value
// the hub-managed removal returns: on the local backend the resolved path,
// and "" when the slug fails the slug rule; on a shared backend always "",
// with nothing removed.
func TestRemoveHubManagedProjectDir_ReturnsResolvedPathOrEmpty(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	srv, _ := testServer(t)

	own := filepath.Join(tmpHome, ".scion", "projects", "own-project")
	writeProjectDirFile(t, own)
	assert.Equal(t, own, srv.removeHubManagedProjectDir("p", "own-project"), "a removed directory's path is returned")
	_, err := os.Stat(own)
	assert.True(t, os.IsNotExist(err))

	assert.Equal(t, "", srv.removeHubManagedProjectDir("p", "."), "a slug failing the slug rule returns no path")
	assert.Equal(t, "", srv.removeHubManagedProjectDir("p", ""), "an empty slug returns no path")

	srv.config.ProjectStorageConfig = projectStorageFromWorkspace(nfsConfig(filepath.Join(tmpHome, "nfs")))
	writeProjectDirFile(t, own)
	assert.Equal(t, "", srv.removeHubManagedProjectDir("p", "own-project"), "a shared backend resolves no slug-keyed path")
	assert.DirExists(t, own, "nothing slug-keyed is removed on a shared backend")
}
