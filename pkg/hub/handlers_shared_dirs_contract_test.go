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
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestSharedDirCreate_BackingDirCreatedOnFirstUse pins the backing-directory
// contract documented on handleProjectSharedDirs (ptone/scion#2879): POST
// records the declaration only; a list of the declared-but-uncreated dir is
// a 200 empty list, a read of a file in it is a 404, neither creates it, and
// the first write creates it.
// Creation at agent start is pinned on the broker side by
// pkg/agent TestResolveSharedDirs_Unset_MatchesLegacyBehavior.
func TestSharedDirCreate_BackingDirCreatedOnFirstUse(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	srv, _ := testServer(t)
	project, _ := createTestHubManagedProject(t, srv, "SD First Use Contract")

	rec := doRequest(t, srv, http.MethodPost, fmt.Sprintf("/api/v1/projects/%s/shared-dirs", project.ID),
		map[string]interface{}{"name": "results"})
	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())

	sdPath := resolveTestSharedDirPath(t, project, "results")
	_, err := os.Lstat(sdPath)
	assert.True(t, os.IsNotExist(err), "POST must record the declaration only, not create %s", sdPath)

	rec = doRequest(t, srv, http.MethodGet, fmt.Sprintf("/api/v1/projects/%s/shared-dirs", project.ID), nil)
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	var listed struct {
		SharedDirs []struct {
			Name string `json:"name"`
		} `json:"sharedDirs"`
	}
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&listed))
	var names []string
	for _, d := range listed.SharedDirs {
		names = append(names, d.Name)
	}
	assert.Contains(t, names, "results", "the declaration must be recorded")

	filesURL := fmt.Sprintf("/api/v1/projects/%s/shared-dirs/results/files", project.ID)
	rec = doRequest(t, srv, http.MethodGet, filesURL, nil)
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	var files ProjectWorkspaceListResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&files))
	assert.Equal(t, 0, files.TotalCount)
	_, err = os.Lstat(sdPath)
	assert.True(t, os.IsNotExist(err), "listing must not create the backing directory")

	rec = doRequest(t, srv, http.MethodGet, filesURL+"/missing.txt", nil)
	assert.Equal(t, http.StatusNotFound, rec.Code, "body: %s", rec.Body.String())
	_, err = os.Lstat(sdPath)
	assert.True(t, os.IsNotExist(err), "reading a file must not create the backing directory")

	rec = doRequest(t, srv, http.MethodPut, filesURL+"/first.txt", ProjectWorkspaceWriteRequest{Content: "hello"})
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	got, err := os.ReadFile(filepath.Join(sdPath, "first.txt"))
	require.NoError(t, err, "the first write must create the backing directory")
	assert.Equal(t, "hello", string(got))
}
