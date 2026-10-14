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
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

// TestWorkspaceLocks_ProjectDeleteInUseStartsNoNFSCleanup: while another
// Runtime Broker instance of the host still has agents in the project, a
// project removal is refused before the NFS workspace tree cleanup starts,
// so the tree on the export is left alone; once that instance's agents are
// deleted, the removal proceeds and the cleanup runs.
func TestWorkspaceLocks_ProjectDeleteInUseStartsNoNFSCleanup(t *testing.T) {
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
	attempts := setBrokerAttemptHook(t, nil)
	b.own(t, d, scopeProjA, "agent-b1", "worker", "cid-b1")

	w := serveFlat(a.srv, http.MethodDelete, "/api/v1/projects/shared-proj?project_id="+scopeProjA, "")
	require.Equal(t, http.StatusConflict, w.Code, w.Body.String())
	a.srv.nfsCleanupWG.Wait()
	require.Empty(t, *attempts, "no NFS tree cleanup started")
	assertPresent(t, filepath.Join(tree, "workspace", "README.md"))
	require.DirExists(t, dir)

	require.NoError(t, b.srv.ownership.SetRecordState(scopeProjA, "agent-b1", OwnershipStateDeleting))
	require.NoError(t, b.srv.ownership.SetRecordState(scopeProjA, "agent-b1", OwnershipStateDeleted))
	w = serveFlat(a.srv, http.MethodDelete, "/api/v1/projects/shared-proj?project_id="+scopeProjA, "")
	require.Equal(t, http.StatusNoContent, w.Code, w.Body.String())
	a.srv.nfsCleanupWG.Wait()
	require.NotEmpty(t, *attempts, "the NFS tree cleanup runs once the project is free")
	assertGone(t, tree)
	require.NoDirExists(t, dir)
}

// TestWorkspaceLocks_ProjectDeleteInUseLocalDirGoneStartsNoNFSCleanup: the
// in-use refusal runs before the local-directory check, so a removal whose
// local project directory is already gone is still refused while another
// instance of the host has agents in the project, and the NFS workspace
// tree cleanup does not start.
func TestWorkspaceLocks_ProjectDeleteInUseLocalDirGoneStartsNoNFSCleanup(t *testing.T) {
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
	attempts := setBrokerAttemptHook(t, nil)
	b.own(t, d, scopeProjA, "agent-b1", "worker", "cid-b1")

	w := serveFlat(a.srv, http.MethodDelete, "/api/v1/projects/shared-proj?project_id="+scopeProjA, "")
	require.Equal(t, http.StatusConflict, w.Code, w.Body.String())
	a.srv.nfsCleanupWG.Wait()
	require.Empty(t, *attempts, "no NFS tree cleanup started")
	assertPresent(t, filepath.Join(tree, "workspace", "README.md"))
}

// nfsDeleteSiblings sets up two flat instances of one host sharing a
// workspace lock service, instance A with NFS workspace storage holding the
// project's tree. The tree's workspace cannot be removed until restore is
// called, so A's first cleanup attempt fails and the retry runs.
func nfsDeleteSiblings(t *testing.T) (a, b *partitionInstance, d *sharedDaemon, tree string, restore func()) {
	t.Helper()
	setupTestScionEnv(t)
	d = &sharedDaemon{}
	locks := NewWorkspaceLocks()
	a = newPartitionInstanceWithLocks(t, d, "docker-a", t.TempDir(), locks)
	b = newPartitionInstanceWithLocks(t, d, "docker-b", t.TempDir(), locks)
	mountRoot := filepath.Join(t.TempDir(), "mnt")
	a.srv.config.NFSConfig = &config.V1NFSConfig{
		MountRoot:   mountRoot,
		SubPathRoot: "projects",
		Shares:      []config.V1NFSShare{{ID: "share1", Server: "10.0.0.2", Export: "/ws"}},
	}
	subRoot := filepath.Join(mountRoot, "share1", "projects")
	require.NoError(t, os.MkdirAll(subRoot, 0o755))
	hubProjectDir(t, "shared-proj", scopeProjA)
	tree = seedNFSProjectTree(t, subRoot, scopeProjA)
	restore = makeWorkspaceUnremovable(t, tree)
	return a, b, d, tree, restore
}

// TestWorkspaceLocks_SiblingAgentBeforeNFSRetryKeepsTree: a project ID can
// come back after a delete. When another instance of the host starts an
// agent of that project between instance A's failed first NFS cleanup
// attempt and its retry, the retry keeps the host-shared tree.
func TestWorkspaceLocks_SiblingAgentBeforeNFSRetryKeepsTree(t *testing.T) {
	a, b, d, tree, restore := nfsDeleteSiblings(t)
	var beginErr error
	attempts := setBrokerAttemptHook(t, func(attempt int) {
		if attempt == 2 {
			restore()
			// What b.own does, without t.Fatal off the test goroutine.
			d.add(api.AgentInfo{ID: "cid-b1", ContainerID: "cid-b1", Name: "worker", ProjectID: scopeProjA, Phase: "running",
				Labels: map[string]string{"scion.agent": "true", "scion.name": "worker", "scion.project_id": scopeProjA, "agent_id": "agent-b1",
					api.LabelRunID: "run-cid-b1", api.LabelRuntimeBrokerID: b.identity.RuntimeBrokerID}})
			beginErr = b.srv.ownership.BeginRun(scopeProjA, "agent-b1", "worker", "run-cid-b1")
		}
	})

	w := serveFlat(a.srv, http.MethodDelete, "/api/v1/projects/shared-proj?project_id="+scopeProjA, "")
	require.Equal(t, http.StatusNoContent, w.Code, w.Body.String())
	a.srv.nfsCleanupWG.Wait()
	require.NoError(t, beginErr)
	require.Equal(t, []int{1, 2}, *attempts)
	assertPresent(t, filepath.Join(tree, "workspace", "README.md"))
}

// TestWorkspaceLocks_NFSRetryWithoutSiblingAgentRemovesTree: the control:
// with no agent of the project on any instance, A's retry removes the tree.
func TestWorkspaceLocks_NFSRetryWithoutSiblingAgentRemovesTree(t *testing.T) {
	a, _, _, tree, restore := nfsDeleteSiblings(t)
	attempts := setBrokerAttemptHook(t, func(attempt int) {
		if attempt == 2 {
			restore()
		}
	})

	w := serveFlat(a.srv, http.MethodDelete, "/api/v1/projects/shared-proj?project_id="+scopeProjA, "")
	require.Equal(t, http.StatusNoContent, w.Code, w.Body.String())
	a.srv.nfsCleanupWG.Wait()
	require.Equal(t, []int{1, 2}, *attempts)
	assertGone(t, tree)
}

// nfsKeepReason runs a project delete on instance A whose first NFS
// cleanup attempt fails; atRetry runs just before the retry's check. It
// returns the reason A logged for keeping the tree.
func nfsKeepReason(t *testing.T, atRetry func(a, b *partitionInstance, d *sharedDaemon) error) string {
	t.Helper()
	a, b, d, tree, restore := nfsDeleteSiblings(t)
	logs := &syncBuffer{}
	a.srv.agentLifecycleLog = slog.New(slog.NewJSONHandler(logs, nil))
	var hookErr error
	attempts := setBrokerAttemptHook(t, func(attempt int) {
		if attempt == 2 {
			restore()
			hookErr = atRetry(a, b, d)
		}
	})

	w := serveFlat(a.srv, http.MethodDelete, "/api/v1/projects/shared-proj?project_id="+scopeProjA, "")
	require.Equal(t, http.StatusNoContent, w.Code, w.Body.String())
	a.srv.nfsCleanupWG.Wait()
	require.NoError(t, hookErr)
	require.Equal(t, []int{1, 2}, *attempts)
	assertPresent(t, filepath.Join(tree, "workspace", "README.md"))
	for _, line := range strings.Split(logs.String(), "\n") {
		var rec map[string]any
		if json.Unmarshal([]byte(line), &rec) == nil && rec["msg"] == "NFS workspace tree kept: the project is in use" {
			reason, _ := rec["reason"].(string)
			return reason
		}
	}
	t.Fatalf("no NFS keep log line in: %s", logs.String())
	return ""
}

// TestWorkspaceLocks_NFSKeepLogNamesTheSiblingInstance: when another
// instance of the host started an agent of the project before the retry,
// the keep log says so rather than blaming this instance.
func TestWorkspaceLocks_NFSKeepLogNamesTheSiblingInstance(t *testing.T) {
	reason := nfsKeepReason(t, func(_, b *partitionInstance, d *sharedDaemon) error {
		d.add(api.AgentInfo{ID: "cid-b1", ContainerID: "cid-b1", Name: "worker", ProjectID: scopeProjA, Phase: "running",
			Labels: map[string]string{"scion.agent": "true", "scion.name": "worker", "scion.project_id": scopeProjA, "agent_id": "agent-b1",
				api.LabelRunID: "run-cid-b1", api.LabelRuntimeBrokerID: b.identity.RuntimeBrokerID}})
		return b.srv.ownership.BeginRun(scopeProjA, "agent-b1", "worker", "run-cid-b1")
	})
	require.Equal(t, projectInUseSibling, reason)
}

// TestWorkspaceLocks_NFSKeepLogNamesAnUnreadableSibling: when another
// instance's records cannot be read at the retry, the tree is kept and the
// keep log says that it cannot tell.
func TestWorkspaceLocks_NFSKeepLogNamesAnUnreadableSibling(t *testing.T) {
	reason := nfsKeepReason(t, func(a, _ *partitionInstance, _ *sharedDaemon) error {
		a.srv.locks().registerWorkspaceUser(&Server{}, &workspaceUser{instance: "unreadable-sibling",
			projectInUse: func(string) (bool, error) { return false, errors.New("ownership records cannot be read") }})
		return nil
	})
	require.Equal(t, projectInUseSiblingUnknown, reason)
}
