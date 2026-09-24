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

// A STATIC (never-changing, non-racing) symlink at an NFS structural component --
// either <pid>/shared-dirs or <pid> itself -- with an EXISTING leaf already
// present on the victim side.
//
// TestSharedDirConfigDelete_NFSBackend_SymlinkedSharedDirsRefused (in
// handlers_shared_dirs_nfs_test.go) already plants a symlinked shared-dirs/
// component, but the victim it points at has no "artifacts" leaf inside it
// at all -- so shareddirs.DeleteSharedDir's own "nothing to delete" no-op
// path produces the exact same passing result regardless of whether the
// symlink-refusal logic is doing anything: a regression that deleted that
// logic entirely would still pass. Planting a real, populated leaf on the
// victim side closes that gap: if refusal ever regressed, the assertions below
// would see the victim's existing content actually get read, overwritten,
// or removed.

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// setupNFSStaticStructuralSymlink plants a static symlink at one NFS
// structural component and pre-populates the victim side with an existing
// "artifacts" leaf (containing a file), so the leaf a test then targets via
// the "artifacts" shared dir genuinely exists on both sides of the symlink.
//
// symlinkPid selects which structural component is the symlink:
//   - false: <pid> is a real directory; <pid>/shared-dirs is the symlink to
//     outside.dir, which itself holds the "artifacts" leaf directly.
//   - true: <pid> itself is the symlink to outside.dir, which holds
//     "shared-dirs/artifacts" (since the whole pid level is swapped out).
func setupNFSStaticStructuralSymlink(t *testing.T, symlinkPid bool) (srv *Server, outside *outsideTree, filesURL, deleteConfigURL, victimExistingFile string) {
	t.Helper()
	hostBase := setNFSSharedDirStorageGlobalSettings(t)

	srv, _ = testServer(t)
	outside = newOutsideTree(t)
	project := createTestGitProject(t, srv, "Static Structural Symlink", "github.com/test/static-structural-symlink")
	addSharedDirToProject(t, srv, project.ID, "artifacts")

	projectsDir := filepath.Join(hostBase, "projects")
	require.NoError(t, os.MkdirAll(projectsDir, 0o755))
	pidPath := filepath.Join(projectsDir, project.ID)

	var victimArtifacts string
	if symlinkPid {
		victimArtifacts = filepath.Join(outside.dir, "shared-dirs", "artifacts")
		require.NoError(t, os.MkdirAll(victimArtifacts, 0o2775))
		require.NoError(t, os.WriteFile(filepath.Join(victimArtifacts, "existing.txt"), []byte("victim content"), 0o644))
		require.NoError(t, os.Symlink(outside.dir, pidPath))
	} else {
		require.NoError(t, os.MkdirAll(pidPath, 0o2755))
		victimArtifacts = filepath.Join(outside.dir, "artifacts")
		require.NoError(t, os.MkdirAll(victimArtifacts, 0o2775))
		require.NoError(t, os.WriteFile(filepath.Join(victimArtifacts, "existing.txt"), []byte("victim content"), 0o644))
		require.NoError(t, os.Symlink(outside.dir, filepath.Join(pidPath, "shared-dirs")))
	}

	filesURL = fmt.Sprintf("/api/v1/projects/%s/shared-dirs/artifacts/files", project.ID)
	deleteConfigURL = fmt.Sprintf("/api/v1/projects/%s/shared-dirs/artifacts", project.ID)
	victimExistingFile = filepath.Join(victimArtifacts, "existing.txt")
	return srv, outside, filesURL, deleteConfigURL, victimExistingFile
}

// assertVictimLeafIntact fails unless the victim's pre-existing file still
// has its original content, unchanged.
func assertVictimLeafIntact(t *testing.T, victimExistingFile string) {
	t.Helper()
	got, err := os.ReadFile(victimExistingFile)
	require.NoError(t, err, "the victim's pre-existing leaf file must still exist")
	assert.Equal(t, "victim content", string(got), "the victim's pre-existing leaf file must be unchanged")
}

func TestStaticStructuralSymlinkNFS_Get(t *testing.T) {
	for _, tc := range []struct {
		name       string
		symlinkPid bool
	}{
		{"shared-dirs component symlinked", false},
		{"pid component symlinked", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, outside, filesURL, _, victimExistingFile := setupNFSStaticStructuralSymlink(t, tc.symlinkPid)
			before := outside.snapshot(t)

			rec := doRequest(t, srv, http.MethodGet, filesURL+"/existing.txt", nil)
			assert.Equal(t, http.StatusConflict, rec.Code, "body: %s", rec.Body.String())
			assertNotLeaked(t, rec)

			rec = doRequest(t, srv, http.MethodGet, filesURL, nil)
			assert.NotContains(t, rec.Body.String(), "existing.txt", "a listing through a symlinked structural component must not surface the victim's content")

			outside.assertIntact(t, before)
			assertVictimLeafIntact(t, victimExistingFile)
		})
	}
}

func TestStaticStructuralSymlinkNFS_Put(t *testing.T) {
	for _, tc := range []struct {
		name       string
		symlinkPid bool
	}{
		{"shared-dirs component symlinked", false},
		{"pid component symlinked", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, outside, filesURL, _, victimExistingFile := setupNFSStaticStructuralSymlink(t, tc.symlinkPid)
			before := outside.snapshot(t)

			rec := doRequest(t, srv, http.MethodPut, filesURL+"/planted.txt",
				ProjectWorkspaceWriteRequest{Content: "written through a symlinked structural component"})
			assert.Equal(t, http.StatusConflict, rec.Code, "body: %s", rec.Body.String())
			assertNotLeaked(t, rec)

			outside.assertIntact(t, before)
			assertVictimLeafIntact(t, victimExistingFile)
		})
	}
}

func TestStaticStructuralSymlinkNFS_FileDelete(t *testing.T) {
	for _, tc := range []struct {
		name       string
		symlinkPid bool
	}{
		{"shared-dirs component symlinked", false},
		{"pid component symlinked", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, outside, filesURL, _, victimExistingFile := setupNFSStaticStructuralSymlink(t, tc.symlinkPid)
			before := outside.snapshot(t)

			rec := doRequest(t, srv, http.MethodDelete, filesURL+"/existing.txt", nil)
			assert.Equal(t, http.StatusConflict, rec.Code, "body: %s", rec.Body.String())
			assertNotLeaked(t, rec)

			outside.assertIntact(t, before)
			assertVictimLeafIntact(t, victimExistingFile)
		})
	}
}

func TestStaticStructuralSymlinkNFS_ConfigDelete(t *testing.T) {
	for _, tc := range []struct {
		name       string
		symlinkPid bool
	}{
		{"shared-dirs component symlinked", false},
		{"pid component symlinked", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, outside, _, deleteConfigURL, victimExistingFile := setupNFSStaticStructuralSymlink(t, tc.symlinkPid)
			before := outside.snapshot(t)

			// The declaration is removed regardless of host cleanup outcome
			// (best-effort, logged on failure -- see
			// TestSharedDirConfigDelete_NFSBackend_SymlinkedSharedDirsRefused),
			// so the decisive assertion is that the victim's existing leaf
			// survives untouched, not the response code.
			rec := doRequest(t, srv, http.MethodDelete, deleteConfigURL, nil)
			assert.Equal(t, http.StatusNoContent, rec.Code, "body: %s", rec.Body.String())

			outside.assertIntact(t, before)
			assertVictimLeafIntact(t, victimExistingFile)
		})
	}
}
