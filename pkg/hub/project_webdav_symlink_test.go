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

// Symlink confinement for the WebDAV sync endpoint.
//
// This endpoint serves the same project workspace as the file browser, so it
// inherits the same problem: the directory holds content the hub does not
// control, and a symlink in it is ordinary. It has a wider verb set than the
// browser, though — MKCOL, MOVE and COPY have no equivalent there, and COPY in
// particular both reads a source and writes a destination, so it has to be
// checked from both ends.
//
// The invariants are the ones used for the browser: the tree outside the
// workspace is byte-identical afterwards, and no response body carries content
// from it. Helpers are shared with project_workspace_symlink_test.go.

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// doDavRequest issues a WebDAV request with a raw body and optional headers.
// The JSON-encoding doRequest helper cannot express these verbs.
func doDavRequest(t *testing.T, srv *Server, method, urlPath string, body []byte, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, urlPath, bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+testDevToken)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

// assertNotSuccess fails if the status says the operation was carried out.
// The exact refusal code is left open on purpose: it comes out of the WebDAV
// error mapping, and pinning it would make the test about that mapping rather
// than about whether the operation was performed.
func assertNotSuccess(t *testing.T, rec *httptest.ResponseRecorder, what string) {
	t.Helper()
	assert.False(t, rec.Code >= 200 && rec.Code < 300,
		"%s should have been refused, got %d: %s", what, rec.Code, rec.Body.String())
}

// davSetup stands up a project, plants the link matrix in its workspace, and
// returns the workspace path and the endpoint prefix addressing it.
func davSetup(t *testing.T, name string) (*Server, *outsideTree, string, string) {
	t.Helper()
	srv, _ := testServer(t)
	outside := newOutsideTree(t)
	project, workspacePath := createTestHubManagedProject(t, srv, name)
	require.NoError(t, os.MkdirAll(workspacePath, 0755))
	plantSymlinks(t, workspacePath, outside)
	return srv, outside, workspacePath, fmt.Sprintf("/api/v1/projects/%s/dav", project.ID)
}

// ============================================================================
// GET
// ============================================================================

func TestWebDAVSymlink_Get(t *testing.T) {
	srv, outside, _, davURL := davSetup(t, "DAV Get")

	cases := []struct {
		name    string
		path    string
		wantOK  bool
		wantHas string
	}{
		{"escaping leaf", "/esc_leaf", false, ""},
		{"escaping intermediate dir", "/esc_dir/secret.txt", false, ""},
		{"inside link is followed", "/in_link", true, "inside content"},
		{"inside dir link is followed", "/in_dir/inside.txt", true, "inside content"},
		{"dangling absolute", "/dangling", false, ""},
		{"dangling relative", "/dangling_rel", false, ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			before := outside.snapshot(t)
			rec := doDavRequest(t, srv, http.MethodGet, davURL+tc.path, nil, nil)

			assertNotLeaked(t, rec)
			outside.assertIntact(t, before)
			if tc.wantOK {
				require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
				assert.Contains(t, rec.Body.String(), tc.wantHas)
			} else {
				assertNotSuccess(t, rec, "GET "+tc.path)
			}
		})
	}
}

// ============================================================================
// PROPFIND
// ============================================================================

func TestWebDAVSymlink_Propfind(t *testing.T) {
	srv, outside, _, davURL := davSetup(t, "DAV Propfind")

	before := outside.snapshot(t)
	rec := doDavRequest(t, srv, "PROPFIND", davURL+"/", nil, map[string]string{"Depth": "infinity"})

	require.Equal(t, http.StatusMultiStatus, rec.Code, "body: %s", rec.Body.String())
	assertNotLeaked(t, rec)
	outside.assertIntact(t, before)

	// A symlinked directory is reported from an lstat, so the walk sees a
	// non-directory and stops rather than enumerating what it points at.
	body := rec.Body.String()
	assert.NotContains(t, body, "victim.txt",
		"PROPFIND enumerated the contents of a directory outside the workspace")
	assert.NotContains(t, body, "secret.txt",
		"PROPFIND enumerated the contents of a directory outside the workspace")

	// The real tree is still reported.
	assert.Contains(t, body, "inside.txt")
}

// Addressing a symlinked directory directly must not enumerate its target.
func TestWebDAVSymlink_PropfindEscapingDir(t *testing.T) {
	srv, outside, _, davURL := davSetup(t, "DAV Propfind Esc")

	before := outside.snapshot(t)
	rec := doDavRequest(t, srv, "PROPFIND", davURL+"/esc_dir/", nil, map[string]string{"Depth": "1"})

	assertNotLeaked(t, rec)
	assert.NotContains(t, rec.Body.String(), "victim.txt",
		"PROPFIND listed a directory outside the workspace")
	outside.assertIntact(t, before)
}

// ============================================================================
// PUT
// ============================================================================

func TestWebDAVSymlink_Put(t *testing.T) {
	srv, outside, workspacePath, davURL := davSetup(t, "DAV Put")

	cases := []struct {
		name   string
		path   string
		wantOK bool
	}{
		{"escaping leaf", "/esc_leaf", false},
		{"escaping intermediate dir", "/esc_dir/landing/pwned.txt", false},
		{"dangling absolute", "/dangling", false},
		{"inside dir link", "/in_dir/put.txt", true},
		{"dangling relative", "/dangling_rel", true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			before := outside.snapshot(t)
			rec := doDavRequest(t, srv, http.MethodPut, davURL+tc.path, []byte("written by the test"), nil)

			outside.assertIntact(t, before)
			if tc.wantOK {
				assert.True(t, rec.Code >= 200 && rec.Code < 300,
					"PUT %s should have succeeded, got %d: %s", tc.path, rec.Code, rec.Body.String())
			} else {
				assertNotSuccess(t, rec, "PUT "+tc.path)
			}
		})
	}

	// The permitted write landed inside the workspace.
	got, err := os.ReadFile(filepath.Join(workspacePath, "real", "put.txt"))
	require.NoError(t, err)
	assert.Equal(t, "written by the test", string(got))
}

// ============================================================================
// MKCOL
// ============================================================================

func TestWebDAVSymlink_Mkcol(t *testing.T) {
	srv, outside, workspacePath, davURL := davSetup(t, "DAV Mkcol")

	t.Run("escaping intermediate dir", func(t *testing.T) {
		before := outside.snapshot(t)
		rec := doDavRequest(t, srv, "MKCOL", davURL+"/esc_dir/newdir/", nil, nil)
		assertNotSuccess(t, rec, "MKCOL under a symlinked directory")
		outside.assertIntact(t, before)
	})

	t.Run("inside dir link", func(t *testing.T) {
		rec := doDavRequest(t, srv, "MKCOL", davURL+"/in_dir/newdir/", nil, nil)
		assert.True(t, rec.Code >= 200 && rec.Code < 300,
			"MKCOL inside the workspace should have succeeded, got %d: %s", rec.Code, rec.Body.String())
		fi, err := os.Stat(filepath.Join(workspacePath, "real", "newdir"))
		require.NoError(t, err)
		assert.True(t, fi.IsDir())
	})
}

// ============================================================================
// DELETE
// ============================================================================

func TestWebDAVSymlink_Delete(t *testing.T) {
	srv, outside, workspacePath, davURL := davSetup(t, "DAV Delete")

	// DELETE maps to RemoveAll, which is recursive — following a link to a
	// directory here would remove a whole tree outside the workspace.
	t.Run("escaping dir contents", func(t *testing.T) {
		before := outside.snapshot(t)
		rec := doDavRequest(t, srv, http.MethodDelete, davURL+"/esc_dir/victim.txt", nil, nil)
		assertNotSuccess(t, rec, "DELETE under a symlinked directory")
		outside.assertIntact(t, before)
	})

	t.Run("escaping dir itself removes only the link", func(t *testing.T) {
		before := outside.snapshot(t)
		rec := doDavRequest(t, srv, http.MethodDelete, davURL+"/esc_dir", nil, nil)
		assert.True(t, rec.Code >= 200 && rec.Code < 300,
			"deleting the link itself should succeed, got %d: %s", rec.Code, rec.Body.String())

		_, err := os.Lstat(filepath.Join(workspacePath, "esc_dir"))
		assert.True(t, os.IsNotExist(err), "the link should be gone from the workspace")
		outside.assertIntact(t, before)
	})

	t.Run("escaping leaf removes only the link", func(t *testing.T) {
		before := outside.snapshot(t)
		rec := doDavRequest(t, srv, http.MethodDelete, davURL+"/esc_leaf", nil, nil)
		assert.True(t, rec.Code >= 200 && rec.Code < 300,
			"deleting the link itself should succeed, got %d: %s", rec.Code, rec.Body.String())

		_, err := os.Lstat(filepath.Join(workspacePath, "esc_leaf"))
		assert.True(t, os.IsNotExist(err))
		outside.assertIntact(t, before)
	})
}

// ============================================================================
// MOVE and COPY
// ============================================================================

func TestWebDAVSymlink_MoveAndCopy(t *testing.T) {
	// COPY reads its source and writes its destination, so both ends are a way
	// out of the workspace and both are checked. MOVE is a rename, which the
	// root resolves on both names too.
	cases := []struct {
		name   string
		method string
		src    string
		dst    string
	}{
		{"COPY from outside the workspace", "COPY", "/esc_dir/secret.txt", "/stolen.txt"},
		{"COPY a link whose target is outside", "COPY", "/esc_leaf", "/stolen.txt"},
		{"COPY to outside the workspace", "COPY", "/real/inside.txt", "/esc_dir/landing/planted.txt"},
		{"COPY onto a link pointing outside", "COPY", "/real/inside.txt", "/esc_leaf"},
		{"MOVE from outside the workspace", "MOVE", "/esc_dir/victim.txt", "/taken.txt"},
		{"MOVE to outside the workspace", "MOVE", "/real/inside.txt", "/esc_dir/landing/planted.txt"},
		{"MOVE onto a link pointing outside", "MOVE", "/real/inside.txt", "/esc_leaf"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, outside, _, davURL := davSetup(t, "DAV "+tc.method)
			before := outside.snapshot(t)

			rec := doDavRequest(t, srv, tc.method, davURL+tc.src, nil, map[string]string{
				"Destination": "http://example.com" + davURL + tc.dst,
				"Overwrite":   "T",
				"Depth":       "infinity",
			})

			assertNotLeaked(t, rec)
			outside.assertIntact(t, before)

			// A copy or move that reached outside would have left the secret
			// inside the workspace, where the client can then read it back.
			read := doDavRequest(t, srv, http.MethodGet, davURL+tc.dst, nil, nil)
			assertNotLeaked(t, read)
		})
	}
}

// A move and a copy that stay inside the workspace keep working.
func TestWebDAVSymlink_MoveAndCopyInsideStillWork(t *testing.T) {
	srv, _, workspacePath, davURL := davSetup(t, "DAV Inside Move")

	rec := doDavRequest(t, srv, "COPY", davURL+"/real/inside.txt", nil, map[string]string{
		"Destination": "http://example.com" + davURL + "/copied.txt",
		"Overwrite":   "T",
	})
	assert.True(t, rec.Code >= 200 && rec.Code < 300, "COPY inside: %d %s", rec.Code, rec.Body.String())
	got, err := os.ReadFile(filepath.Join(workspacePath, "copied.txt"))
	require.NoError(t, err)
	assert.Equal(t, "inside content", string(got))

	rec = doDavRequest(t, srv, "MOVE", davURL+"/copied.txt", nil, map[string]string{
		"Destination": "http://example.com" + davURL + "/moved.txt",
		"Overwrite":   "T",
	})
	assert.True(t, rec.Code >= 200 && rec.Code < 300, "MOVE inside: %d %s", rec.Code, rec.Body.String())
	got, err = os.ReadFile(filepath.Join(workspacePath, "moved.txt"))
	require.NoError(t, err)
	assert.Equal(t, "inside content", string(got))
	_, err = os.Lstat(filepath.Join(workspacePath, "copied.txt"))
	assert.True(t, os.IsNotExist(err), "MOVE should have removed the source")
}

// ============================================================================
// The workspace directory itself
// ============================================================================

// The endpoint creates the workspace when it is absent, which is how a client
// syncs into a new project. That must not extend to adopting a symlink left in
// its place: opening a root on a link follows it, and everything underneath
// would then be confined to the wrong directory.
func TestWebDAVSymlink_WorkspaceIsSymlink(t *testing.T) {
	srv, _ := testServer(t)
	outside := newOutsideTree(t)
	project, workspacePath := createTestHubManagedProject(t, srv, "DAV Base Link")

	require.NoError(t, os.MkdirAll(filepath.Dir(workspacePath), 0755))
	require.NoError(t, os.RemoveAll(workspacePath))
	require.NoError(t, os.Symlink(outside.dir, workspacePath))

	davURL := fmt.Sprintf("/api/v1/projects/%s/dav", project.ID)
	before := outside.snapshot(t)

	rec := doDavRequest(t, srv, "PROPFIND", davURL+"/", nil, map[string]string{"Depth": "1"})
	assertNotLeaked(t, rec)
	assert.NotContains(t, rec.Body.String(), "victim.txt", "PROPFIND enumerated the link target")

	rec = doDavRequest(t, srv, http.MethodGet, davURL+"/secret.txt", nil, nil)
	assertNotLeaked(t, rec)
	assertNotSuccess(t, rec, "GET through a symlinked workspace")

	rec = doDavRequest(t, srv, http.MethodPut, davURL+"/planted.txt", []byte("should never land"), nil)
	assertNotSuccess(t, rec, "PUT through a symlinked workspace")

	rec = doDavRequest(t, srv, http.MethodDelete, davURL+"/victim.txt", nil, nil)
	assertNotSuccess(t, rec, "DELETE through a symlinked workspace")

	outside.assertIntact(t, before)
}
