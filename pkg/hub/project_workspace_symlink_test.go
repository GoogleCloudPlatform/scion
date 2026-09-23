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

// Symlink confinement for the project file browser.
//
// The two directory trees these handlers serve — a project workspace and a
// project shared directory — hold content the hub does not control. A
// workspace is a git checkout, so a symlink can be committed to the repo; a
// shared directory is mounted read-write into every agent in the project, so
// an agent can create one. A link stored in either of them is therefore an
// expected condition, not an anomaly.
//
// Every test below plants links inside the served directory and drives the
// real HTTP handlers against them. The whole matrix runs twice, once per base,
// because the two dispatch through different resolution paths into the same
// handlers and a fix that reached only one of them would be invisible here
// otherwise.
//
// Two things are asserted about the tree outside the base, on every case:
//
//   - nothing out there was created, modified or removed. assertOutsideIntact
//     compares a full content snapshot taken before the request.
//   - nothing out there was read. A handler that followed a link would put the
//     bytes in its response, so each case checks the response body for the
//     secret's contents rather than trusting the status code alone.
//
// A status-code assertion on its own would not be worth much: a handler can
// return 400 and still have already written the file.

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// outsideSecret is the content the handlers must never read out of the tree
// outside the base, and never overwrite.
const outsideSecret = "TOP-SECRET-HUB-TOKEN-do-not-serve-this"

// outsideTree is the stand-in for everything the hub process can reach but the
// file browser has no business touching — ~/.scion, hub.db, authorized_keys.
type outsideTree struct {
	dir string
}

// newOutsideTree builds the tree and returns it. It deliberately lives in its
// own TempDir rather than a sibling of the base, so that a path that reaches
// it cannot have got there by any route except following a link.
func newOutsideTree(t *testing.T) *outsideTree {
	t.Helper()
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "secret.txt"), []byte(outsideSecret), 0600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "victim.txt"), []byte("delete me and the test fails"), 0600))
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "landing"), 0755))
	return &outsideTree{dir: dir}
}

// snapshot records every path and file content under the tree.
func (o *outsideTree) snapshot(t *testing.T) map[string]string {
	t.Helper()
	got := map[string]string{}
	err := filepath.WalkDir(o.dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, relErr := filepath.Rel(o.dir, path)
		if relErr != nil {
			return relErr
		}
		if d.IsDir() {
			got[rel+"/"] = ""
			return nil
		}
		b, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		got[rel] = string(b)
		return nil
	})
	require.NoError(t, err)
	return got
}

// assertIntact fails if anything outside was created, modified or removed.
func (o *outsideTree) assertIntact(t *testing.T, before map[string]string) {
	t.Helper()
	assert.Equal(t, before, o.snapshot(t),
		"the tree outside the served directory changed; a handler followed a symlink out of it")
}

// assertNotLeaked fails if a response body carries content from outside.
func assertNotLeaked(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	assert.NotContains(t, rec.Body.String(), outsideSecret,
		"response leaked content from outside the served directory")
}

// plantSymlinks lays the four link shapes of the matrix into base, alongside
// one genuinely inside file for the allowed cases to resolve to.
//
// The two dangling forms are kept separate on purpose. os.Root refuses every
// absolute link outright, whether or not its target exists, while a relative
// link to a missing name inside the base is an ordinary not-found — the two
// arrive at the handlers as different errors and must both be shown to be safe.
func plantSymlinks(t *testing.T, base string, outside *outsideTree) {
	t.Helper()

	require.NoError(t, os.MkdirAll(filepath.Join(base, "real"), 0755))
	require.NoError(t, os.WriteFile(filepath.Join(base, "real", "inside.txt"), []byte("inside content"), 0644))

	// Escaping leaf: a link whose target is a file outside the base.
	require.NoError(t, os.Symlink(filepath.Join(outside.dir, "secret.txt"), filepath.Join(base, "esc_leaf")))
	// Escaping intermediate directory: a link used as a path component, so
	// that everything addressed beneath it resolves outside the base.
	require.NoError(t, os.Symlink(outside.dir, filepath.Join(base, "esc_dir")))
	// Inside link: resolves back into the base, and is allowed.
	require.NoError(t, os.Symlink("real/inside.txt", filepath.Join(base, "in_link")))
	require.NoError(t, os.Symlink("real", filepath.Join(base, "in_dir")))
	// Dangling, absolute and outside.
	require.NoError(t, os.Symlink(filepath.Join(outside.dir, "no-such-file"), filepath.Join(base, "dangling")))
	// Dangling, relative and inside.
	require.NoError(t, os.Symlink("no-such-file", filepath.Join(base, "dangling_rel")))
}

// symlinkBase is one of the two trees the browser serves.
type symlinkBase struct {
	name string
	// setup creates a project, plants the link matrix in the served directory,
	// and returns that directory plus the endpoints that address it.
	setup func(t *testing.T, srv *Server, outside *outsideTree) (baseDir, filesURL, archiveURL string)
}

func symlinkBases() []symlinkBase {
	return []symlinkBase{
		{
			name: "workspace",
			setup: func(t *testing.T, srv *Server, outside *outsideTree) (string, string, string) {
				project, workspacePath := createTestHubManagedProject(t, srv, "Symlink WS")
				plantSymlinks(t, workspacePath, outside)
				return workspacePath,
					fmt.Sprintf("/api/v1/projects/%s/workspace/files", project.ID),
					fmt.Sprintf("/api/v1/projects/%s/workspace/archive", project.ID)
			},
		},
		{
			name: "shareddir",
			setup: func(t *testing.T, srv *Server, outside *outsideTree) (string, string, string) {
				project, workspacePath := createTestHubManagedProject(t, srv, "Symlink SD")
				addSharedDirToProject(t, srv, project.ID, "scratch")
				sharedDirPath := resolveTestSharedDirPath(t, workspacePath, "scratch")
				// The handlers no longer create this directory on a read, so
				// the test creates it the way agent provisioning would.
				require.NoError(t, os.MkdirAll(sharedDirPath, 0755))
				plantSymlinks(t, sharedDirPath, outside)
				return sharedDirPath,
					fmt.Sprintf("/api/v1/projects/%s/shared-dirs/scratch/files", project.ID),
					fmt.Sprintf("/api/v1/projects/%s/shared-dirs/scratch/archive", project.ID)
			},
		},
	}
}

// runMatrix runs fn against both bases.
func runMatrix(t *testing.T, fn func(t *testing.T, srv *Server, outside *outsideTree, baseDir, filesURL, archiveURL string)) {
	t.Helper()
	for _, base := range symlinkBases() {
		t.Run(base.name, func(t *testing.T) {
			srv, _ := testServer(t)
			outside := newOutsideTree(t)
			baseDir, filesURL, archiveURL := base.setup(t, srv, outside)
			fn(t, srv, outside, baseDir, filesURL, archiveURL)
		})
	}
}

// ============================================================================
// GET — download a single file
// ============================================================================

func TestSymlink_Download(t *testing.T) {
	runMatrix(t, func(t *testing.T, srv *Server, outside *outsideTree, baseDir, filesURL, _ string) {
		cases := []struct {
			name     string
			path     string
			wantCode int
			wantBody string // exact body expected on success
		}{
			{"escaping leaf", "esc_leaf", http.StatusBadRequest, ""},
			{"escaping intermediate dir", "esc_dir/secret.txt", http.StatusBadRequest, ""},
			{"inside link is followed", "in_link", http.StatusOK, "inside content"},
			{"inside dir link is followed", "in_dir/inside.txt", http.StatusOK, "inside content"},
			{"dangling absolute", "dangling", http.StatusBadRequest, ""},
			{"dangling relative", "dangling_rel", http.StatusNotFound, ""},
		}

		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				before := outside.snapshot(t)
				rec := doRequest(t, srv, http.MethodGet, filesURL+"/"+tc.path, nil)

				assert.Equal(t, tc.wantCode, rec.Code, "body: %s", rec.Body.String())
				assertNotLeaked(t, rec)
				outside.assertIntact(t, before)
				if tc.wantBody != "" {
					assert.Equal(t, tc.wantBody, rec.Body.String())
				}
			})
		}
	})
}

// The JSON editor view reads the file through a second code path (ReadFile
// rather than Open), so it gets its own case.
func TestSymlink_DownloadFormatJSON(t *testing.T) {
	runMatrix(t, func(t *testing.T, srv *Server, outside *outsideTree, baseDir, filesURL, _ string) {
		for _, path := range []string{"esc_leaf", "esc_dir/secret.txt"} {
			t.Run(path, func(t *testing.T) {
				before := outside.snapshot(t)
				rec := doRequest(t, srv, http.MethodGet, filesURL+"/"+path+"?format=json", nil)

				assert.Equal(t, http.StatusBadRequest, rec.Code, "body: %s", rec.Body.String())
				assertNotLeaked(t, rec)
				outside.assertIntact(t, before)
			})
		}
	})
}

// ============================================================================
// GET — list
// ============================================================================

func TestSymlink_List(t *testing.T) {
	runMatrix(t, func(t *testing.T, srv *Server, outside *outsideTree, baseDir, filesURL, _ string) {
		before := outside.snapshot(t)
		rec := doRequest(t, srv, http.MethodGet, filesURL, nil)
		require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
		assertNotLeaked(t, rec)
		outside.assertIntact(t, before)

		var resp ProjectWorkspaceListResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))

		listed := map[string]bool{}
		for _, f := range resp.Files {
			listed[f.Path] = true
		}

		// The listing walks past a symlinked directory instead of descending
		// into it, so nothing beyond one shows up.
		for path := range listed {
			assert.False(t, strings.HasPrefix(path, "esc_dir/"),
				"listing descended through a symlinked directory: %s", path)
			assert.False(t, strings.HasPrefix(path, "in_dir/"),
				"listing descended through a symlinked directory: %s", path)
		}

		// The real file behind the in-base directory link is still reachable
		// by its real name.
		assert.True(t, listed[filepath.Join("real", "inside.txt")],
			"the genuine file went missing from the listing: %v", listed)

		// Links themselves remain visible — the browser should show what is
		// there, it just must not read through it. Their reported mode is the
		// link's own, from an lstat, not the target's.
		assert.True(t, listed["esc_leaf"], "the symlink itself should still be listed: %v", listed)
		for _, f := range resp.Files {
			if f.Path == "esc_leaf" {
				assert.True(t, strings.HasPrefix(f.Mode, "L"),
					"esc_leaf should be reported as a symlink, got mode %q", f.Mode)
			}
		}
	})
}

// ============================================================================
// GET — archive
// ============================================================================

func TestSymlink_Archive(t *testing.T) {
	runMatrix(t, func(t *testing.T, srv *Server, outside *outsideTree, baseDir, _, archiveURL string) {
		before := outside.snapshot(t)
		rec := doRequest(t, srv, http.MethodGet, archiveURL, nil)
		require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
		outside.assertIntact(t, before)

		// The zip is binary, so check the decoded entries rather than the raw
		// body — deflate would hide the secret from a substring search.
		zr, err := zip.NewReader(bytes.NewReader(rec.Body.Bytes()), int64(rec.Body.Len()))
		require.NoError(t, err)

		names := map[string]string{}
		for _, f := range zr.File {
			rc, err := f.Open()
			require.NoError(t, err)
			content, err := io.ReadAll(rc)
			require.NoError(t, rc.Close())
			require.NoError(t, err)
			names[f.Name] = string(content)
		}

		for name, content := range names {
			assert.NotContains(t, content, outsideSecret,
				"archive entry %q carries content from outside the served directory", name)
			assert.False(t, strings.HasPrefix(name, "esc_dir/"),
				"archive descended through a symlinked directory: %s", name)
		}

		// Links are left out of the archive entirely rather than followed.
		assert.NotContains(t, names, "esc_leaf")
		assert.NotContains(t, names, "in_link")
		assert.NotContains(t, names, "dangling")
		assert.NotContains(t, names, "dangling_rel")

		// The real content is still archived.
		assert.Equal(t, "inside content", names[filepath.Join("real", "inside.txt")])
	})
}

// ============================================================================
// POST — upload
// ============================================================================

func TestSymlink_Upload(t *testing.T) {
	runMatrix(t, func(t *testing.T, srv *Server, outside *outsideTree, baseDir, filesURL, _ string) {
		cases := []struct {
			name     string
			path     string
			wantCode int
		}{
			// Writing through a link to a file outside would overwrite it.
			{"escaping leaf", "esc_leaf", http.StatusBadRequest},
			// Writing beneath a link to a directory outside would create there.
			{"escaping intermediate dir", "esc_dir/landing/pwned.txt", http.StatusBadRequest},
			// The implicit parent-directory creation is its own sink: there is
			// no explicit mkdir verb, so this is the only way the handlers
			// create directories.
			{"escaping dir, implicit mkdir", "esc_dir/landing/new/deep.txt", http.StatusBadRequest},
			// A dangling absolute link would otherwise be created on write.
			{"dangling absolute", "dangling", http.StatusBadRequest},
			// These stay inside and must keep working.
			{"inside dir link", "in_dir/uploaded.txt", http.StatusOK},
			{"dangling relative", "dangling_rel", http.StatusOK},
		}

		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				before := outside.snapshot(t)
				rec := doMultipartRequest(t, srv, http.MethodPost, filesURL,
					map[string][]byte{tc.path: []byte("written by the test")})

				assert.Equal(t, tc.wantCode, rec.Code, "body: %s", rec.Body.String())
				assertNotLeaked(t, rec)
				outside.assertIntact(t, before)
			})
		}

		// The two permitted writes landed inside the base, where they belong.
		got, err := os.ReadFile(filepath.Join(baseDir, "real", "uploaded.txt"))
		require.NoError(t, err, "a write through an in-base directory link should have landed in the real directory")
		assert.Equal(t, "written by the test", string(got))

		got, err = os.ReadFile(filepath.Join(baseDir, "no-such-file"))
		require.NoError(t, err, "a write through an in-base dangling link should have created the target inside the base")
		assert.Equal(t, "written by the test", string(got))
	})
}

// ============================================================================
// PUT — write
// ============================================================================

func TestSymlink_Write(t *testing.T) {
	runMatrix(t, func(t *testing.T, srv *Server, outside *outsideTree, baseDir, filesURL, _ string) {
		cases := []struct {
			name     string
			path     string
			wantCode int
		}{
			{"escaping leaf", "esc_leaf", http.StatusBadRequest},
			{"escaping intermediate dir", "esc_dir/landing/pwned.txt", http.StatusBadRequest},
			{"escaping dir, implicit mkdir", "esc_dir/landing/new/deep.txt", http.StatusBadRequest},
			{"dangling absolute", "dangling", http.StatusBadRequest},
			{"inside dir link", "in_dir/written.txt", http.StatusOK},
			{"inside link is followed", "in_link", http.StatusOK},
			{"dangling relative", "dangling_rel", http.StatusOK},
		}

		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				before := outside.snapshot(t)
				rec := doRequest(t, srv, http.MethodPut, filesURL+"/"+tc.path,
					ProjectWorkspaceWriteRequest{Content: "written by the test"})

				assert.Equal(t, tc.wantCode, rec.Code, "body: %s", rec.Body.String())
				assertNotLeaked(t, rec)
				outside.assertIntact(t, before)
			})
		}

		// Writing through the in-base link followed it to the real file,
		// rather than replacing the link with a regular file.
		fi, err := os.Lstat(filepath.Join(baseDir, "in_link"))
		require.NoError(t, err)
		assert.NotZero(t, fi.Mode()&os.ModeSymlink, "in_link should still be a symlink")
		got, err := os.ReadFile(filepath.Join(baseDir, "real", "inside.txt"))
		require.NoError(t, err)
		assert.Equal(t, "written by the test", string(got))
	})
}

// ============================================================================
// DELETE
// ============================================================================

func TestSymlink_Delete(t *testing.T) {
	runMatrix(t, func(t *testing.T, srv *Server, outside *outsideTree, baseDir, filesURL, _ string) {
		// Deleting beneath a link to a directory outside must be refused
		// outright — that is someone else's file.
		t.Run("escaping intermediate dir", func(t *testing.T) {
			before := outside.snapshot(t)
			rec := doRequest(t, srv, http.MethodDelete, filesURL+"/esc_dir/victim.txt", nil)
			assert.Equal(t, http.StatusBadRequest, rec.Code, "body: %s", rec.Body.String())
			outside.assertIntact(t, before)
		})

		// Deleting the link itself is allowed and unlinks only the link. It is
		// visible in the listing, so refusing would strand it there with no
		// way to remove it.
		t.Run("escaping leaf removes the link, not the target", func(t *testing.T) {
			before := outside.snapshot(t)
			rec := doRequest(t, srv, http.MethodDelete, filesURL+"/esc_leaf", nil)
			assert.Equal(t, http.StatusNoContent, rec.Code, "body: %s", rec.Body.String())

			_, err := os.Lstat(filepath.Join(baseDir, "esc_leaf"))
			assert.True(t, os.IsNotExist(err), "the link should be gone from the base")
			outside.assertIntact(t, before)
		})

		t.Run("inside link removes the link, not the target", func(t *testing.T) {
			rec := doRequest(t, srv, http.MethodDelete, filesURL+"/in_link", nil)
			assert.Equal(t, http.StatusNoContent, rec.Code, "body: %s", rec.Body.String())

			_, err := os.Lstat(filepath.Join(baseDir, "in_link"))
			assert.True(t, os.IsNotExist(err), "the link should be gone")
			_, err = os.Stat(filepath.Join(baseDir, "real", "inside.txt"))
			assert.NoError(t, err, "deleting a link must not delete what it points at")
		})

		t.Run("dangling absolute", func(t *testing.T) {
			before := outside.snapshot(t)
			rec := doRequest(t, srv, http.MethodDelete, filesURL+"/dangling", nil)
			assert.Equal(t, http.StatusNoContent, rec.Code, "body: %s", rec.Body.String())

			_, err := os.Lstat(filepath.Join(baseDir, "dangling"))
			assert.True(t, os.IsNotExist(err), "the dangling link should be gone")
			outside.assertIntact(t, before)
		})

		t.Run("dangling relative", func(t *testing.T) {
			rec := doRequest(t, srv, http.MethodDelete, filesURL+"/dangling_rel", nil)
			assert.Equal(t, http.StatusNoContent, rec.Code, "body: %s", rec.Body.String())
			_, err := os.Lstat(filepath.Join(baseDir, "dangling_rel"))
			assert.True(t, os.IsNotExist(err), "the dangling link should be gone")
		})
	})
}

// ============================================================================
// The base directory itself
// ============================================================================

// A Root opened on a base that is itself a symlink confines nothing: os.OpenRoot
// follows the link and every operation below it is then confined to wherever
// the link pointed. The shared-dir leaf is the reachable case — its name is the
// last path component the resolver appends — so the guard is checked there.
func TestSymlink_SharedDirBaseIsSymlink(t *testing.T) {
	srv, _ := testServer(t)
	outside := newOutsideTree(t)

	project, workspacePath := createTestHubManagedProject(t, srv, "Symlink SD Base")
	addSharedDirToProject(t, srv, project.ID, "scratch")
	sharedDirPath := resolveTestSharedDirPath(t, workspacePath, "scratch")

	// Stand the whole shared directory up as a link to somewhere else.
	require.NoError(t, os.MkdirAll(filepath.Dir(sharedDirPath), 0755))
	require.NoError(t, os.RemoveAll(sharedDirPath))
	require.NoError(t, os.Symlink(outside.dir, sharedDirPath))

	filesURL := fmt.Sprintf("/api/v1/projects/%s/shared-dirs/scratch/files", project.ID)
	before := outside.snapshot(t)

	// Reading the listing must not enumerate the link's target.
	rec := doRequest(t, srv, http.MethodGet, filesURL, nil)
	assert.Equal(t, http.StatusInternalServerError, rec.Code, "body: %s", rec.Body.String())
	assertNotLeaked(t, rec)

	// Nor may a single file be read out of it.
	rec = doRequest(t, srv, http.MethodGet, filesURL+"/secret.txt", nil)
	assertNotLeaked(t, rec)
	assert.NotEqual(t, http.StatusOK, rec.Code)

	// Nor written into, nor deleted from.
	rec = doRequest(t, srv, http.MethodPut, filesURL+"/planted.txt",
		ProjectWorkspaceWriteRequest{Content: "should never land"})
	assert.NotEqual(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

	rec = doRequest(t, srv, http.MethodDelete, filesURL+"/victim.txt", nil)
	assert.NotEqual(t, http.StatusNoContent, rec.Code, "body: %s", rec.Body.String())

	outside.assertIntact(t, before)
}

// Reading a shared directory that does not exist yet must not bring it into
// being. Browsing is not a provisioning operation, and creating the directory
// meant the hub reached along a path it had not established was safe.
func TestSymlink_ReadDoesNotCreateSharedDir(t *testing.T) {
	srv, _ := testServer(t)
	project, workspacePath := createTestHubManagedProject(t, srv, "SD No Create On Read")
	addSharedDirToProject(t, srv, project.ID, "scratch")
	sharedDirPath := resolveTestSharedDirPath(t, workspacePath, "scratch")
	require.NoError(t, os.RemoveAll(sharedDirPath))

	filesURL := fmt.Sprintf("/api/v1/projects/%s/shared-dirs/scratch/files", project.ID)

	// A list of a directory that is not there is an empty list.
	rec := doRequest(t, srv, http.MethodGet, filesURL, nil)
	assert.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	_, err := os.Lstat(sharedDirPath)
	assert.True(t, os.IsNotExist(err), "listing created the shared directory")

	// A read inside it is a 404.
	rec = doRequest(t, srv, http.MethodGet, filesURL+"/anything.txt", nil)
	assert.Equal(t, http.StatusNotFound, rec.Code, "body: %s", rec.Body.String())
	_, err = os.Lstat(sharedDirPath)
	assert.True(t, os.IsNotExist(err), "a read created the shared directory")

	// A write still creates it on first use, which is the case that needed it.
	rec = doRequest(t, srv, http.MethodPut, filesURL+"/first.txt",
		ProjectWorkspaceWriteRequest{Content: "hello"})
	assert.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	got, err := os.ReadFile(filepath.Join(sharedDirPath, "first.txt"))
	require.NoError(t, err)
	assert.Equal(t, "hello", string(got))
}

// A workspace that does not exist behaves the same way.
func TestSymlink_ReadDoesNotCreateWorkspace(t *testing.T) {
	srv, _ := testServer(t)
	project, workspacePath := createTestHubManagedProject(t, srv, "WS No Create On Read")
	require.NoError(t, os.RemoveAll(workspacePath))

	filesURL := fmt.Sprintf("/api/v1/projects/%s/workspace/files", project.ID)

	rec := doRequest(t, srv, http.MethodGet, filesURL, nil)
	assert.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	_, err := os.Lstat(workspacePath)
	assert.True(t, os.IsNotExist(err), "listing created the workspace")

	rec = doRequest(t, srv, http.MethodGet, filesURL+"/anything.txt", nil)
	assert.Equal(t, http.StatusNotFound, rec.Code, "body: %s", rec.Body.String())

	rec = doRequest(t, srv, http.MethodDelete, filesURL+"/anything.txt", nil)
	assert.Equal(t, http.StatusNotFound, rec.Code, "body: %s", rec.Body.String())
	_, err = os.Lstat(workspacePath)
	assert.True(t, os.IsNotExist(err), "a delete created the workspace")
}
