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
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestSharedDirConfigDelete_NFSBackend_RemovesLeaf, exercised through the
// real HTTP endpoint end to end: DELETE /shared-dirs/{name} removes the
// declaration and the NFS leaf via shareddirs.DeleteSharedDir, leaving a
// sibling shared dir alone.
func TestSharedDirConfigDelete_NFSBackend_RemovesLeaf(t *testing.T) {
	hostBase := setNFSSharedDirStorageGlobalSettings(t)
	srv, _ := testServer(t)
	project := createTestGitProject(t, srv, "Config Delete NFS", "github.com/test/config-delete-nfs")
	addSharedDirToProject(t, srv, project.ID, "artifacts")
	addSharedDirToProject(t, srv, project.ID, "extra")

	target := filepath.Join(hostBase, "projects", project.ID, "shared-dirs", "artifacts")
	require.NoError(t, os.MkdirAll(target, 0o2775))
	require.NoError(t, os.WriteFile(filepath.Join(target, "file.txt"), []byte("hi"), 0o644))

	sibling := filepath.Join(hostBase, "projects", project.ID, "shared-dirs", "extra")
	require.NoError(t, os.MkdirAll(sibling, 0o2775))
	require.NoError(t, os.WriteFile(filepath.Join(sibling, "keep.txt"), []byte("keep"), 0o644))

	rec := doRequest(t, srv, http.MethodDelete,
		fmt.Sprintf("/api/v1/projects/%s/shared-dirs/artifacts", project.ID), nil)
	assert.Equal(t, http.StatusNoContent, rec.Code, "body: %s", rec.Body.String())

	_, err := os.Stat(target)
	assert.True(t, os.IsNotExist(err), "the deleted shared dir's leaf must be removed from the export")

	info, err := os.Stat(filepath.Join(sibling, "keep.txt"))
	require.NoError(t, err, "a sibling shared dir must survive untouched")
	assert.False(t, info.IsDir())
}

// TestSharedDirConfigDelete_NFSBackend_SymlinkedSharedDirsRefused: a
// symlinked shared-dirs/ component must not be traversed by the config
// DELETE's host cleanup. The declaration is still removed from the DB
// (cleanup is best-effort), but the victim outside the export must never be
// touched.
func TestSharedDirConfigDelete_NFSBackend_SymlinkedSharedDirsRefused(t *testing.T) {
	hostBase := setNFSSharedDirStorageGlobalSettings(t)
	srv, _ := testServer(t)
	outside := newOutsideTree(t)
	project := createTestGitProject(t, srv, "Config Delete Symlink NFS", "github.com/test/config-delete-symlink-nfs")
	addSharedDirToProject(t, srv, project.ID, "artifacts")

	pidDir := filepath.Join(hostBase, "projects", project.ID)
	require.NoError(t, os.MkdirAll(pidDir, 0o755))
	require.NoError(t, os.Symlink(outside.dir, filepath.Join(pidDir, "shared-dirs")))

	before := outside.snapshot(t)
	rec := doRequest(t, srv, http.MethodDelete,
		fmt.Sprintf("/api/v1/projects/%s/shared-dirs/artifacts", project.ID), nil)
	// The declaration is removed regardless of host cleanup outcome (the
	// handler treats host cleanup as best-effort and logs on failure), so
	// the decisive assertion here is that the victim was never touched --
	// not the response code.
	assert.Equal(t, http.StatusNoContent, rec.Code, "body: %s", rec.Body.String())
	outside.assertIntact(t, before)

	target, readErr := os.Readlink(filepath.Join(pidDir, "shared-dirs"))
	require.NoError(t, readErr, "the symlink itself must still be exactly what it was")
	assert.Equal(t, outside.dir, target)
}
