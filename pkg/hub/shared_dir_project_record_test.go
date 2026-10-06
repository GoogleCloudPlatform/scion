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
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// setWorkspaceMarker replaces a hub-managed workspace's .scion entry with a
// marker file recording the given project identity.
func setWorkspaceMarker(t *testing.T, workspacePath, projectID, projectSlug string) {
	t.Helper()
	scionPath := filepath.Join(workspacePath, config.DotScion)
	require.NoError(t, os.RemoveAll(scionPath))
	require.NoError(t, config.WriteProjectMarker(scionPath, &config.ProjectMarker{
		ProjectID:   projectID,
		ProjectName: projectSlug,
		ProjectSlug: projectSlug,
	}))
}

// setWorkspaceProjectIDFile records projectID in the workspace's .scion
// directory.
func setWorkspaceProjectIDFile(t *testing.T, workspacePath, projectID string) {
	t.Helper()
	scionPath := filepath.Join(workspacePath, config.DotScion)
	require.NoError(t, os.MkdirAll(scionPath, 0o755))
	require.NoError(t, config.WriteProjectID(scionPath, projectID))
}

// dirEntryNames lists the names in dir, or nil if it does not exist.
func dirEntryNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil
	}
	require.NoError(t, err)
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

// TestSharedDirFiles_WorkspaceIdentityMustMatchProjectRecord: when a project's
// workspace records the identity of a different project, every shared-dir file
// operation on the first project fails closed with a response that names no
// host path, and the other project's shared dir is neither read nor written.
func TestSharedDirFiles_WorkspaceIdentityMustMatchProjectRecord(t *testing.T) {
	cases := []struct {
		name     string
		identify func(t *testing.T, workspacePath string, other *store.Project)
	}{
		{
			name: "marker file names the other project",
			identify: func(t *testing.T, workspacePath string, other *store.Project) {
				setWorkspaceMarker(t, workspacePath, other.ID, other.Slug)
			},
		},
		{
			name: "project-id file names the other project",
			identify: func(t *testing.T, workspacePath string, other *store.Project) {
				setWorkspaceProjectIDFile(t, workspacePath, other.ID)
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, _ := testServer(t)
			project, workspacePath := createTestHubManagedProject(t, srv, "Record Owner")
			other, _ := createTestHubManagedProject(t, srv, "Record Other")
			addSharedDirToProject(t, srv, project.ID, "data")
			addSharedDirToProject(t, srv, other.ID, "data")

			otherDir := resolveTestSharedDirPath(t, other, "data")
			require.NoError(t, os.MkdirAll(otherDir, 0o755))
			require.NoError(t, os.WriteFile(filepath.Join(otherDir, "other.txt"), []byte("OTHER CONTENT"), 0o644))

			tc.identify(t, workspacePath, other)

			home, err := os.UserHomeDir()
			require.NoError(t, err)
			base := fmt.Sprintf("/api/v1/projects/%s/shared-dirs/data", project.ID)

			requests := []struct {
				name string
				do   func() (int, string)
			}{
				{"list", func() (int, string) {
					rec := doRequest(t, srv, http.MethodGet, base+"/files", nil)
					return rec.Code, rec.Body.String()
				}},
				{"download", func() (int, string) {
					rec := doRequest(t, srv, http.MethodGet, base+"/files/other.txt", nil)
					return rec.Code, rec.Body.String()
				}},
				{"archive", func() (int, string) {
					rec := doRequest(t, srv, http.MethodGet, base+"/archive", nil)
					return rec.Code, rec.Body.String()
				}},
				{"write", func() (int, string) {
					rec := doRequest(t, srv, http.MethodPut, base+"/files/written.txt",
						ProjectWorkspaceWriteRequest{Content: "NEW"})
					return rec.Code, rec.Body.String()
				}},
				{"upload", func() (int, string) {
					rec := doMultipartRequest(t, srv, http.MethodPost, base+"/files",
						map[string][]byte{"uploaded.txt": []byte("NEW")})
					return rec.Code, rec.Body.String()
				}},
				{"delete", func() (int, string) {
					rec := doRequest(t, srv, http.MethodDelete, base+"/files/other.txt", nil)
					return rec.Code, rec.Body.String()
				}},
			}
			for _, r := range requests {
				code, body := r.do()
				assert.Equal(t, http.StatusConflict, code, "%s: body: %s", r.name, body)
				assert.NotContains(t, body, "OTHER CONTENT", r.name)
				assert.NotContains(t, body, home, r.name)
				assert.NotContains(t, body, config.ProjectConfigsDir, r.name)
			}

			assert.ElementsMatch(t, []string{"other.txt"}, dirEntryNames(t, otherDir),
				"the other project's shared dir must be unchanged")
			content, err := os.ReadFile(filepath.Join(otherDir, "other.txt"))
			require.NoError(t, err)
			assert.Equal(t, "OTHER CONTENT", string(content))
			assert.Nil(t, dirEntryNames(t, resolveTestSharedDirPath(t, project, "data")),
				"nothing is created for the project either")

			_, resolveErr := resolveHubProjectSharedDirPath(project, "data")
			require.Error(t, resolveErr)
			assert.True(t, errors.Is(resolveErr, errSharedDirProjectRecordMismatch))
			assert.False(t, strings.Contains(resolveErr.Error(), string(filepath.Separator)),
				"error must not contain a path: %q", resolveErr.Error())
		})
	}
}

// TestSharedDirFiles_WorkspaceWithoutIdentityUsesProjectRecord: a workspace
// that records no project identity yet resolves to the directory named by the
// project record, not to a directory inside the workspace.
func TestSharedDirFiles_WorkspaceWithoutIdentityUsesProjectRecord(t *testing.T) {
	srv, _ := testServer(t)
	project, workspacePath := createTestHubManagedProject(t, srv, "Record No Identity")
	addSharedDirToProject(t, srv, project.ID, "data")

	hasIdentity, err := workspaceRecordsProjectIdentity(filepath.Join(workspacePath, config.DotScion))
	require.NoError(t, err)
	require.False(t, hasIdentity)

	rec := doRequest(t, srv, http.MethodPut,
		fmt.Sprintf("/api/v1/projects/%s/shared-dirs/data/files/note.txt", project.ID),
		ProjectWorkspaceWriteRequest{Content: "hello"})
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

	content, err := os.ReadFile(filepath.Join(resolveTestSharedDirPath(t, project, "data"), "note.txt"))
	require.NoError(t, err)
	assert.Equal(t, "hello", string(content))

	_, err = os.Stat(filepath.Join(workspacePath, config.SharedDirsSubdir))
	assert.True(t, os.IsNotExist(err), "nothing is written under the workspace")
}

// TestSharedDirFiles_WorkspaceIdentityMatchingProjectRecord: a workspace that
// records the project's own identity resolves to the record-derived directory
// and serves it normally.
func TestSharedDirFiles_WorkspaceIdentityMatchingProjectRecord(t *testing.T) {
	cases := []struct {
		name     string
		identify func(t *testing.T, workspacePath string, project *store.Project)
	}{
		{
			name: "project-id file names the project",
			identify: func(t *testing.T, workspacePath string, project *store.Project) {
				setWorkspaceProjectIDFile(t, workspacePath, project.ID)
			},
		},
		{
			name: "marker file names the project",
			identify: func(t *testing.T, workspacePath string, project *store.Project) {
				setWorkspaceMarker(t, workspacePath, project.ID, project.Slug)
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, _ := testServer(t)
			project, workspacePath := createTestHubManagedProject(t, srv, "Record Match")
			addSharedDirToProject(t, srv, project.ID, "data")
			tc.identify(t, workspacePath, project)

			sdPath := resolveTestSharedDirPath(t, project, "data")
			got, err := resolveHubProjectSharedDirPath(project, "data")
			require.NoError(t, err)
			assert.Equal(t, sdPath, got)

			require.NoError(t, os.MkdirAll(sdPath, 0o755))
			require.NoError(t, os.WriteFile(filepath.Join(sdPath, "existing.txt"), []byte("existing"), 0o644))

			base := fmt.Sprintf("/api/v1/projects/%s/shared-dirs/data", project.ID)
			rec := doRequest(t, srv, http.MethodGet, base+"/files/existing.txt", nil)
			require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
			assert.Equal(t, "existing", rec.Body.String())

			rec = doRequest(t, srv, http.MethodPut, base+"/files/new.txt",
				ProjectWorkspaceWriteRequest{Content: "new"})
			require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
			content, err := os.ReadFile(filepath.Join(sdPath, "new.txt"))
			require.NoError(t, err)
			assert.Equal(t, "new", string(content))
		})
	}
}

// TestProjectRecordSharedDirPath_RequiresValidRecord: the record-derived path
// is produced only from a valid slug, project ID and shared dir name.
func TestProjectRecordSharedDirPath_RequiresValidRecord(t *testing.T) {
	const validID = "0b1c2d3e-4f50-4617-8293-a4b5c6d7e8f9"
	cases := []struct {
		name    string
		project store.Project
		dirName string
	}{
		{"rejects an empty slug", store.Project{ID: validID, Slug: ""}, "data"},
		{"rejects an empty project ID", store.Project{ID: "", Slug: "proj"}, "data"},
		{"rejects project IDs outside the ID format (parent segment)", store.Project{ID: "../x", Slug: "proj"}, "data"},
		{"rejects project IDs outside the ID format (separator)", store.Project{ID: "a/b", Slug: "proj"}, "data"},
		{"rejects an empty dir name", store.Project{ID: validID, Slug: "proj"}, ""},
		{"rejects dir names outside the name format (parent segment)", store.Project{ID: validID, Slug: "proj"}, "../x"},
		{"rejects dir names outside the name format (separator)", store.Project{ID: validID, Slug: "proj"}, "a/b"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			project := tc.project
			got, err := projectRecordSharedDirPath(&project, tc.dirName)
			require.Error(t, err)
			assert.Empty(t, got)
		})
	}

	t.Run("accepts a valid record", func(t *testing.T) {
		project := store.Project{ID: validID, Slug: "proj"}
		got, err := projectRecordSharedDirPath(&project, "data")
		require.NoError(t, err)
		assert.Equal(t, resolveTestSharedDirPath(t, &project, "data"), got)
	})
}

// TestSharedDirFiles_UnreadableWorkspaceIdentityFailsClosed: a workspace
// project-id that exists but cannot be read as an identity fails closed with
// the same pathless error, and nothing is created at the record path.
func TestSharedDirFiles_UnreadableWorkspaceIdentityFailsClosed(t *testing.T) {
	cases := []struct {
		name  string
		setup func(t *testing.T, scionPath string)
	}{
		{
			name: "project-id is a directory",
			setup: func(t *testing.T, scionPath string) {
				require.NoError(t, os.MkdirAll(filepath.Join(scionPath, "project-id"), 0o755))
			},
		},
		{
			name: "project-id is empty",
			setup: func(t *testing.T, scionPath string) {
				require.NoError(t, os.WriteFile(filepath.Join(scionPath, "project-id"), nil, 0o644))
			},
		},
	}
	if os.Geteuid() != 0 {
		cases = append(cases, struct {
			name  string
			setup func(t *testing.T, scionPath string)
		}{
			name: "project-id is unreadable",
			setup: func(t *testing.T, scionPath string) {
				p := filepath.Join(scionPath, "project-id")
				require.NoError(t, os.WriteFile(p, []byte("some-id\n"), 0o644))
				require.NoError(t, os.Chmod(p, 0))
				t.Cleanup(func() { _ = os.Chmod(p, 0o644) })
			},
		})
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, _ := testServer(t)
			project, workspacePath := createTestHubManagedProject(t, srv, "Record Unreadable")
			addSharedDirToProject(t, srv, project.ID, "data")
			scionPath := filepath.Join(workspacePath, config.DotScion)
			require.NoError(t, os.MkdirAll(scionPath, 0o755))
			tc.setup(t, scionPath)

			_, identityErr := workspaceRecordsProjectIdentity(scionPath)
			require.Error(t, identityErr)

			_, resolveErr := resolveHubProjectSharedDirPath(project, "data")
			require.Error(t, resolveErr)
			assert.True(t, errors.Is(resolveErr, errSharedDirProjectRecordMismatch))
			assert.False(t, strings.Contains(resolveErr.Error(), string(filepath.Separator)),
				"error must not contain a path: %q", resolveErr.Error())

			home, err := os.UserHomeDir()
			require.NoError(t, err)
			rec := doRequest(t, srv, http.MethodPut,
				fmt.Sprintf("/api/v1/projects/%s/shared-dirs/data/files/note.txt", project.ID),
				ProjectWorkspaceWriteRequest{Content: "hello"})
			assert.Equal(t, http.StatusConflict, rec.Code, "body: %s", rec.Body.String())
			assert.NotContains(t, rec.Body.String(), home)
			assert.Nil(t, dirEntryNames(t, resolveTestSharedDirPath(t, project, "data")),
				"nothing is created at the record path")
		})
	}
}
