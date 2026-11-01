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

package runtimebroker

import (
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

// TestWorkspaceLocks_ProjectDeleteInUseRefused: while another Runtime
// Broker instance of the host still has agents in the project, a project
// removal is refused and nothing is removed; once that instance's agents
// are deleted, the removal proceeds. The tree on the export is the hub's
// to remove (ptone/scion#5835): the broker leaves it either way.
func TestWorkspaceLocks_ProjectDeleteInUseRefused(t *testing.T) {
	setupTestScionEnv(t)
	d := &sharedDaemon{}
	locks := NewWorkspaceLocks()
	a := newPartitionInstanceWithLocks(t, d, "docker-a", t.TempDir(), locks)
	b := newPartitionInstanceWithLocks(t, d, "docker-b", t.TempDir(), locks)
	mountRoot := filepath.Join(t.TempDir(), "mnt")
	a.srv.config.NFSConfig = &config.V1NFSConfig{
		MountRoot:   mountRoot,
		SubPathRoot: "projects",
		Shares:      []config.V1NFSShare{{ID: "share1", Server: "10.0.0.2", Export: "/ws"}},
	}
	subRoot := filepath.Join(mountRoot, "share1", "projects")
	require.NoError(t, os.MkdirAll(subRoot, 0o755))
	dir := hubProjectDir(t, "shared-proj", scopeProjA)
	tree := seedNFSProjectTree(t, subRoot, scopeProjA)
	b.own(t, d, scopeProjA, "agent-b1", "worker", "cid-b1")

	w := serveFlat(a.srv, http.MethodDelete, "/api/v1/projects/shared-proj?project_id="+scopeProjA, "")
	require.Equal(t, http.StatusConflict, w.Code, w.Body.String())
	assertPresent(t, filepath.Join(tree, "workspace", "README.md"))
	require.DirExists(t, dir)

	require.NoError(t, b.srv.ownership.SetRecordState(scopeProjA, "agent-b1", OwnershipStateDeleting))
	require.NoError(t, b.srv.ownership.SetRecordState(scopeProjA, "agent-b1", OwnershipStateDeleted))
	w = serveFlat(a.srv, http.MethodDelete, "/api/v1/projects/shared-proj?project_id="+scopeProjA, "")
	require.Equal(t, http.StatusNoContent, w.Code, w.Body.String())
	require.NoDirExists(t, dir)
	assertPresent(t, filepath.Join(tree, "workspace", "README.md"))
}

// TestWorkspaceLocks_ProjectDeleteInUseLocalDirGoneRefused: the in-use
// refusal runs before the local-directory check, so a removal whose local
// project directory is already gone is still refused while another
// instance of the host has agents in the project.
func TestWorkspaceLocks_ProjectDeleteInUseLocalDirGoneRefused(t *testing.T) {
	setupTestScionEnv(t)
	d := &sharedDaemon{}
	locks := NewWorkspaceLocks()
	a := newPartitionInstanceWithLocks(t, d, "docker-a", t.TempDir(), locks)
	b := newPartitionInstanceWithLocks(t, d, "docker-b", t.TempDir(), locks)
	mountRoot := filepath.Join(t.TempDir(), "mnt")
	a.srv.config.NFSConfig = &config.V1NFSConfig{
		MountRoot:   mountRoot,
		SubPathRoot: "projects",
		Shares:      []config.V1NFSShare{{ID: "share1", Server: "10.0.0.2", Export: "/ws"}},
	}
	subRoot := filepath.Join(mountRoot, "share1", "projects")
	require.NoError(t, os.MkdirAll(subRoot, 0o755))
	dir := hubProjectDir(t, "shared-proj", scopeProjA)
	require.NoError(t, os.RemoveAll(dir))
	tree := seedNFSProjectTree(t, subRoot, scopeProjA)
	b.own(t, d, scopeProjA, "agent-b1", "worker", "cid-b1")

	w := serveFlat(a.srv, http.MethodDelete, "/api/v1/projects/shared-proj?project_id="+scopeProjA, "")
	require.Equal(t, http.StatusConflict, w.Code, w.Body.String())
	assertPresent(t, filepath.Join(tree, "workspace", "README.md"))
}
