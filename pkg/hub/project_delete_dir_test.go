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

// projectDirStorageCase describes a workspace storage configuration. configure
// applies it to srv and returns the backend projects root hub-managed
// directories resolve under ("" for the default local backend).
type projectDirStorageCase struct {
	name      string
	configure func(t *testing.T, srv *Server, base string) string
}

func projectDirStorageCases() []projectDirStorageCase {
	return []projectDirStorageCase{
		{
			name:      "default",
			configure: func(t *testing.T, srv *Server, base string) string { return "" },
		},
		{
			name: "nfs",
			configure: func(t *testing.T, srv *Server, base string) string {
				srv.config.WorkspaceStorageConfig = &config.V1WorkspaceStorageConfig{
					Backend: "nfs",
					NFS: &config.V1NFSConfig{
						MountRoot: filepath.Join(base, "nfs"),
						Shares:    []config.V1NFSShare{{ID: "share1", Server: "10.0.0.2", Export: "/scion"}},
					},
				}
				return filepath.Join(base, "nfs", "share1", "hub-projects")
			},
		},
		{
			name: "cloudrun-volume",
			configure: func(t *testing.T, srv *Server, base string) string {
				setVolumeMountBase(t, filepath.Join(base, "mnt"))
				srv.config.WorkspaceStorageConfig = &config.V1WorkspaceStorageConfig{
					Backend:        "cloudrun-volume",
					CloudRunVolume: &config.V1CloudRunVolumeConfig{VolumeName: "workspace-vol"},
				}
				return filepath.Join(base, "mnt", "workspace-vol", config.SubPathRootOrDefault(""), "hub-projects")
			},
		},
		{
			name: "gke-shared-volume",
			configure: func(t *testing.T, srv *Server, base string) string {
				setVolumeMountBase(t, filepath.Join(base, "mnt"))
				srv.config.WorkspaceStorageConfig = &config.V1WorkspaceStorageConfig{
					Backend: "gke-shared-volume",
					GKESharedVolume: &config.V1GKESharedVolumeConfig{
						VolumeName:  "workspace-vol",
						PVClaimName: "scion-workspaces",
					},
				}
				return filepath.Join(base, "mnt", "workspace-vol", config.SubPathRootOrDefault(""), "hub-projects")
			},
		},
	}
}

// writeProjectDirFile creates dir/keep.txt and returns its path.
func writeProjectDirFile(t *testing.T, dir string) string {
	t.Helper()
	require.NoError(t, os.MkdirAll(dir, 0o755))
	f := filepath.Join(dir, "keep.txt")
	require.NoError(t, os.WriteFile(f, []byte("keep"), 0o644))
	return f
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
// deleting a stored record whose slug does not resolve to a direct child of a
// projects root removes the record and leaves every projects root and every
// sibling project directory in place, local and backend.
//
// With backend content present the slug resolves against the backend root;
// without it, resolution falls back to the local root.
func TestDeleteProject_StoredSlugNotDirectChild_RemovesNoDirectory(t *testing.T) {
	for _, tc := range projectDirStorageCases() {
		for _, backendHasContent := range []bool{true, false} {
			name := tc.name
			if !backendHasContent {
				if tc.name == "default" {
					continue
				}
				name += "/local-fallback"
			}
			t.Run(name, func(t *testing.T) {
				tmpHome := t.TempDir()
				t.Setenv("HOME", tmpHome)
				srv, s := testServer(t)
				backendRoot := tc.configure(t, srv, tmpHome)

				localRoot := filepath.Join(tmpHome, ".scion", "projects")
				kept := []string{writeProjectDirFile(t, filepath.Join(localRoot, "sibling-project"))}
				if backendRoot != "" && backendHasContent {
					kept = append(kept, writeProjectDirFile(t, filepath.Join(backendRoot, "sibling-project")))
				}

				const id = "aaaaaaaa-0000-4000-8000-000000000001"
				storeHubManagedProject(t, s, id, ".")

				rec := doRequest(t, srv, http.MethodGet, "/api/v1/projects/"+id, nil)
				assert.Equal(t, http.StatusOK, rec.Code, "the stored record stays readable: %s", rec.Body.String())
				rec = doRequest(t, srv, http.MethodGet, "/api/v1/projects", nil)
				assert.Equal(t, http.StatusOK, rec.Code)
				assert.Contains(t, rec.Body.String(), id, "the stored record stays listed")

				logs := captureProjectsLog(t, srv)
				rec = doRequest(t, srv, http.MethodDelete, "/api/v1/projects/"+id, nil)
				require.Equal(t, http.StatusNoContent, rec.Code, "body: %s", rec.Body.String())

				logged := logs.String()
				assert.Contains(t, logged, "hub-managed project directory not removed: it is not a direct child of a projects root", "the skipped removal is logged")
				assert.Contains(t, logged, "project_id="+id, "the log line names the project")
				assert.NotContains(t, logged, tmpHome, "the log carries no filesystem path")

				_, err := s.GetProject(context.Background(), id)
				assert.ErrorIs(t, err, store.ErrNotFound, "the record is deleted")
				for _, f := range kept {
					_, err := os.Stat(f)
					assert.NoError(t, err, "sibling project content %s must remain", f)
				}
			})
		}
	}
}

// TestRemoveProjectDirUnderProjectsRoot_RemovesOnlyDirectChild checks the
// removal step on its own, independent of slug validation: given a resolved
// path that is a projects root or lies outside a direct child, it removes
// nothing; given a direct child, it removes exactly that directory.
func TestRemoveProjectDirUnderProjectsRoot_RemovesOnlyDirectChild(t *testing.T) {
	for _, tc := range projectDirStorageCases() {
		t.Run(tc.name, func(t *testing.T) {
			tmpHome := t.TempDir()
			t.Setenv("HOME", tmpHome)
			srv, _ := testServer(t)
			backendRoot := tc.configure(t, srv, tmpHome)

			roots := []string{filepath.Join(tmpHome, ".scion", "projects")}
			if backendRoot != "" {
				roots = append(roots, backendRoot)
			}
			outside := writeProjectDirFile(t, filepath.Join(tmpHome, "outside"))

			for _, root := range roots {
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
				assert.True(t, os.IsNotExist(err), "a direct child of %s is removed", root)
				_, err = os.Stat(sibling)
				assert.NoError(t, err, "sibling must remain after removing a direct child")
			}
		})
	}
}

// TestDeleteProject_RemovesExactlyOwnDirectory checks that a normal delete
// removes the project's own directory and nothing else, on the default and on
// each backend configuration.
func TestDeleteProject_RemovesExactlyOwnDirectory(t *testing.T) {
	for _, tc := range projectDirStorageCases() {
		t.Run(tc.name, func(t *testing.T) {
			tmpHome := t.TempDir()
			t.Setenv("HOME", tmpHome)
			srv, s := testServer(t)
			backendRoot := tc.configure(t, srv, tmpHome)

			root := filepath.Join(tmpHome, ".scion", "projects")
			if backendRoot != "" {
				root = backendRoot
			}
			const slug = "own-project"
			writeProjectDirFile(t, filepath.Join(root, slug))
			sibling := writeProjectDirFile(t, filepath.Join(root, "sibling-project"))

			const id = "aaaaaaaa-0000-4000-8000-000000000002"
			storeHubManagedProject(t, s, id, slug)
			resolved, err := srv.hubManagedProjectPath(slug)
			require.NoError(t, err)
			require.Equal(t, filepath.Join(root, slug), resolved)

			rec := doRequest(t, srv, http.MethodDelete, "/api/v1/projects/"+id, nil)
			require.Equal(t, http.StatusNoContent, rec.Code, "body: %s", rec.Body.String())

			_, err = os.Stat(filepath.Join(root, slug))
			assert.True(t, os.IsNotExist(err), "the project's own directory is removed")
			_, err = os.Stat(sibling)
			assert.NoError(t, err, "the sibling project directory remains")
			_, err = os.Stat(root)
			assert.NoError(t, err, "the projects root remains")
		})
	}
}

// TestDeleteProject_BackendLocalFallback_RemovesExactlyOwnDirectory checks
// that, with a backend configured but holding no content for the project, a
// delete removes the project's own directory under the local projects root
// (where hubManagedProjectPath falls back to) and nothing else.
func TestDeleteProject_BackendLocalFallback_RemovesExactlyOwnDirectory(t *testing.T) {
	for _, tc := range projectDirStorageCases() {
		if tc.name == "default" {
			continue
		}
		t.Run(tc.name, func(t *testing.T) {
			tmpHome := t.TempDir()
			t.Setenv("HOME", tmpHome)
			srv, s := testServer(t)
			backendRoot := tc.configure(t, srv, tmpHome)
			backendSibling := writeProjectDirFile(t, filepath.Join(backendRoot, "sibling-project"))

			localRoot := filepath.Join(tmpHome, ".scion", "projects")
			const slug = "own-project"
			writeProjectDirFile(t, filepath.Join(localRoot, slug))
			localSibling := writeProjectDirFile(t, filepath.Join(localRoot, "sibling-project"))

			const id = "aaaaaaaa-0000-4000-8000-000000000003"
			storeHubManagedProject(t, s, id, slug)
			resolved, err := srv.hubManagedProjectPath(slug)
			require.NoError(t, err)
			require.Equal(t, filepath.Join(localRoot, slug), resolved)

			rec := doRequest(t, srv, http.MethodDelete, "/api/v1/projects/"+id, nil)
			require.Equal(t, http.StatusNoContent, rec.Code, "body: %s", rec.Body.String())

			_, err = os.Stat(filepath.Join(localRoot, slug))
			assert.True(t, os.IsNotExist(err), "the project's own local directory is removed")
			for _, f := range []string{localSibling, backendSibling} {
				_, err = os.Stat(f)
				assert.NoError(t, err, "sibling project content %s must remain", f)
			}
		})
	}
}

// TestRemoveProjectDirUnderProjectsRoot_EmptyNFSMountRootRemovesNothing checks
// that an nfs configuration with an empty mount root contributes no projects
// root, so a relative directory under the working directory is never removed.
func TestRemoveProjectDirUnderProjectsRoot_EmptyNFSMountRootRemovesNothing(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	srv, _ := testServer(t)
	srv.config.WorkspaceStorageConfig = &config.V1WorkspaceStorageConfig{
		Backend: "nfs",
		NFS: &config.V1NFSConfig{
			MountRoot: "",
			Shares:    []config.V1NFSShare{{ID: "", Server: "10.0.0.2", Export: "/scion"}},
		},
	}

	for _, root := range srv.hubManagedProjectsRoots() {
		assert.True(t, filepath.IsAbs(root), "every projects root is absolute")
	}

	workDir := t.TempDir()
	t.Chdir(workDir)
	kept := writeProjectDirFile(t, filepath.Join(workDir, "hub-projects", "own-project"))

	srv.removeProjectDirUnderProjectsRoot("p", filepath.Join("hub-projects", "own-project"))

	_, err := os.Stat(kept)
	assert.NoError(t, err, "a relative directory is never removed")
}

// TestRemoveHubManagedProjectDir_ReturnsResolvedPathOrEmpty checks the value
// the hub-managed removal returns: the resolved path when one was resolved,
// whether or not it was removed, and "" when the slug fails the slug rule.
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

	// A configuration whose resolved path is not under any projects root:
	// the removal is refused and the resolved path is still returned.
	srv.config.WorkspaceStorageConfig = &config.V1WorkspaceStorageConfig{
		Backend: "nfs",
		NFS: &config.V1NFSConfig{
			MountRoot: "",
			Shares:    []config.V1NFSShare{{ID: "", Server: "10.0.0.2", Export: "/scion"}},
		},
	}
	workDir := t.TempDir()
	t.Chdir(workDir)
	kept := writeProjectDirFile(t, filepath.Join(workDir, "hub-projects", "own-project"))
	assert.Equal(t, filepath.Join("hub-projects", "own-project"), srv.removeHubManagedProjectDir("p", "own-project"),
		"a refused removal still returns the resolved path")
	_, err = os.Stat(kept)
	assert.NoError(t, err, "the refused directory remains")

	t.Run("unresolved path returns no path", func(t *testing.T) {
		f := newHungPathFixture(t, "unresolved-project")
		mountRoot := filepath.Join(f.tmpHome, "nfs-mount")
		hangReadDirFor(t, mountRoot)
		srv, _ := testServer(t)
		srv.config.WorkspaceStorageConfig = nfsConfig(mountRoot)

		assert.Equal(t, "", srv.removeHubManagedProjectDir("p", f.slug), "a path that cannot be resolved returns no path")
		assert.DirExists(t, f.localDir, "nothing is removed when the path cannot be resolved")
	})
}
