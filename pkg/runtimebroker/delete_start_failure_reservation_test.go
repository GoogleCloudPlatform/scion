// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package runtimebroker

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/provision"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// A failed flat create on a worktree-per-agent project keeps its slug
// reserved while a branch named after the agent remains: the start-failure
// cleanup deletes no branch, and the files proof counts that branch as one
// of the agent's files. The record stays active and the slug held until the
// agent's delete removes its files. A worktree the start joined stays in
// place with its work, and so does the branch.
func TestCleanupAbortedLaunch_StartFailureKeepsReservationWhileAgentBranchRemains(t *testing.T) {
	for _, tc := range []struct {
		name   string
		joined bool // the start joined an unregistered worktree on the agent's branch
	}{
		{name: "joined worktree", joined: true},
		{name: "fresh branch", joined: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requireWorktreeGit(t)
			t.Setenv("SCION_HOST_UID", "")
			f := newFlatInstanceTestServer(t, flatInstanceOpts{hubInProcess: true})
			f.srv.config.FlatInstance.Instance.RuntimeTarget = &config.V1RuntimeTargetConfig{Type: config.RuntimeTargetTypeDocker}
			root := flatProjectRoot(t)
			const agentID, slug, runID = "agent-id-sf", "sf-agent", "run-sf"
			require.NoError(t, f.srv.ownership.BeginRun(flatTestProjectID, agentID, slug, runID))
			require.NoError(t, os.MkdirAll(filepath.Join(root, config.DotScion, "agents", slug), 0o755))

			base := filepath.Join(root, "workspace")
			gc := &api.GitCloneConfig{URL: initBareRepoWithCommit(t), Branch: "main"}
			var joined, dirty string
			if tc.joined {
				// Clone the shared base through an unrelated agent, then
				// leave a worktree on the agent's branch that no registry
				// entry names, with work in it.
				require.NoError(t, provision.ProvisionShared(provision.ProvisionInput{
					Resolved: provision.ResolvedWorkspace{HostPath: base, Backend: "local"},
					Mode:     store.SharingModeWorktreePerAgent, ProjectID: flatTestProjectID,
					AgentID: "seed-id", AgentName: "seed", GitClone: gc,
				}))
				joined = filepath.Join(base, "worktrees", "leftover")
				startFailureGitOut(t, base, "worktree", "add", "-q", "-b", slug, joined)
				dirty = filepath.Join(joined, "uncommitted.txt")
				require.NoError(t, os.WriteFile(dirty, []byte("work\n"), 0o644))
			}
			require.NoError(t, provision.ProvisionShared(provision.ProvisionInput{
				Resolved: provision.ResolvedWorkspace{HostPath: base, Backend: "local"},
				Mode:     store.SharingModeWorktreePerAgent, ProjectID: flatTestProjectID,
				AgentID: agentID, AgentName: slug, GitClone: gc,
			}))
			if tc.joined {
				_, at, found, err := provision.FindBranchForAgent(base, "", agentID)
				require.NoError(t, err)
				require.True(t, found)
				require.Equal(t, joined, at, "the start joined the leftover worktree")
			}
			require.NotEmpty(t, startFailureBranchTip(t, base, slug), "setup: the agent's branch exists")
			require.NoError(t, writeLaunchMarker(root, false, slug, "L1"))

			key := ownedRunKey{projectID: flatTestProjectID, agentID: agentID, runID: runID}
			rec := newLaunchRecord("L1", agentID, store.LaunchKindCreate, "", time.Now().Add(time.Minute), func() {})
			rec.ownedRun = key // as acceptAsyncLaunch records it for a create
			lc := launchCtx{
				opts:  api.StartOptions{Name: slug, ProjectPath: root, HubProjectID: flatTestProjectID, RunID: runID},
				mgr:   f.mgr,
				key:   launchKey{ProjectID: flatTestProjectID, Slug: slug},
				owned: &ownedStart{hold: f.srv.holdOwnedRun(key)},
			}
			_ = f.srv.cleanupAbortedLaunch(f.mgr, rec, lc)

			// The cleanup ran: the agent's directory and its registration
			// under the hub agent ID are gone.
			_, err := os.Stat(filepath.Join(root, config.DotScion, "agents", slug))
			assert.True(t, os.IsNotExist(err), "agent directory removed")
			_, _, found, err := provision.FindBranchForAgent(base, "", agentID)
			require.NoError(t, err)
			assert.False(t, found, "the hub agent ID is unregistered")

			// The branch named after the agent remains, so the files are
			// not proven absent: the record is kept and the slug stays
			// reserved.
			assert.NotEmpty(t, startFailureBranchTip(t, base, slug), "the agent's branch is kept")
			assert.False(t, rec.filesRemoved.Load(), "files not proven removed while the agent's branch remains")
			got, ok, err := f.srv.ownership.Get(flatTestProjectID, agentID)
			require.NoError(t, err)
			require.True(t, ok, "the ownership record is kept")
			assert.Equal(t, OwnershipStateActive, got.State)
			assert.Equal(t, agentID, flatSlugHolder(t, f, slug), "the slug stays reserved")

			if tc.joined {
				_, err := os.Stat(dirty)
				assert.NoError(t, err, "the joined worktree keeps its work")
			} else {
				_, err := os.Lstat(provision.WorktreePath(base, agentID))
				assert.True(t, os.IsNotExist(err), "the worktree the start created is removed")
			}
		})
	}
}

// The same through the synchronous create (createAgent): Manager.Start
// fails, the start-failure cleanup runs, and the files proof keeps the
// record and the slug while the agent's branch remains.
func TestSyncCreateFailure_StartFailureKeepsReservationWhileAgentBranchRemains(t *testing.T) {
	for _, tc := range []struct {
		name   string
		joined bool
	}{
		{name: "joined worktree", joined: true},
		{name: "fresh branch", joined: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requireWorktreeGit(t)
			t.Setenv("SCION_HOST_UID", "")
			f := newFlatInstanceTestServer(t, flatInstanceOpts{hubInProcess: true})
			f.srv.config.FlatInstance.Instance.RuntimeTarget = &config.V1RuntimeTargetConfig{Type: config.RuntimeTargetTypeDocker}
			root := flatProjectRoot(t)
			const slug = "sf-sync"
			agentID := "agent-id-" + slug // flatCreateBody's ID for the name
			base := filepath.Join(root, "workspace")
			gc := &api.GitCloneConfig{URL: initBareRepoWithCommit(t), Branch: "main"}
			var joined, dirty string
			if tc.joined {
				require.NoError(t, provision.ProvisionShared(provision.ProvisionInput{
					Resolved: provision.ResolvedWorkspace{HostPath: base, Backend: "local"},
					Mode:     store.SharingModeWorktreePerAgent, ProjectID: flatTestProjectID,
					AgentID: "seed-id", AgentName: "seed", GitClone: gc,
				}))
				joined = filepath.Join(base, "worktrees", "leftover")
				startFailureGitOut(t, base, "worktree", "add", "-q", "-b", slug, joined)
				dirty = filepath.Join(joined, "uncommitted.txt")
				require.NoError(t, os.WriteFile(dirty, []byte("work\n"), 0o644))
			}
			// The start's provisioning, as ensureWorktree leaves it under the
			// hub agent ID.
			require.NoError(t, provision.ProvisionShared(provision.ProvisionInput{
				Resolved: provision.ResolvedWorkspace{HostPath: base, Backend: "local"},
				Mode:     store.SharingModeWorktreePerAgent, ProjectID: flatTestProjectID,
				AgentID: agentID, AgentName: slug, GitClone: gc,
			}))
			require.NotEmpty(t, startFailureBranchTip(t, base, slug), "setup: the agent's branch exists")

			f.mgr.startErr = errors.New("start failed")
			w := flatCreateAccepted(f, slug)
			require.GreaterOrEqual(t, w.Code, 400, w.Body.String())

			// The cleanup ran under the hub agent ID.
			_, _, found, err := provision.FindBranchForAgent(base, "", agentID)
			require.NoError(t, err)
			assert.False(t, found, "the hub agent ID is unregistered")
			// The agent's branch remains: the record is kept and the slug
			// stays reserved.
			assert.NotEmpty(t, startFailureBranchTip(t, base, slug), "the agent's branch is kept")
			got, ok, err := f.srv.ownership.Get(flatTestProjectID, agentID)
			require.NoError(t, err)
			require.True(t, ok, "the ownership record is kept")
			assert.Equal(t, OwnershipStateActive, got.State)
			assert.Equal(t, agentID, flatSlugHolder(t, f, slug), "the slug stays reserved")
			if tc.joined {
				_, err := os.Stat(dirty)
				assert.NoError(t, err, "the joined worktree keeps its work")
			} else {
				_, err := os.Lstat(provision.WorktreePath(base, agentID))
				assert.True(t, os.IsNotExist(err), "the worktree the start created is removed")
			}
		})
	}
}

// linkedCheckoutStart makes the flat project root a git checkout (no
// worktree-per-agent shared base) and leaves what a ProvisionAgent start
// on a reused branch leaves there: a worktree on branch slug at
// .scion/agents/<slug>/workspace, registered by name, with a commit of its
// own on the branch. It returns the checkout and the branch tip.
func linkedCheckoutStart(t *testing.T, root, slug string) (string, string) {
	t.Helper()
	startFailureGitOut(t, root, "init", "-q", "-b", "main")
	require.NoError(t, os.WriteFile(filepath.Join(root, "README.md"), []byte("project\n"), 0o644))
	startFailureGitOut(t, root, "add", "README.md")
	startFailureGitOut(t, root, "commit", "-q", "-m", "initial")
	scionDir := filepath.Join(root, config.DotScion)
	wt := filepath.Join(scionDir, "agents", slug, "workspace")
	startFailureGitOut(t, root, "worktree", "add", "-q", "-b", slug, wt)
	require.NoError(t, os.WriteFile(filepath.Join(wt, "work.txt"), []byte("work\n"), 0o644))
	startFailureGitOut(t, wt, "add", "work.txt")
	startFailureGitOut(t, wt, "commit", "-q", "-m", "work")
	require.NoError(t, provision.RegisterSharer(root, scionDir, slug, wt, slug))
	tip := startFailureBranchTip(t, root, slug)
	require.NotEmpty(t, tip)
	return wt, tip
}

// In a linked checkout (ProvisionAgent layout, registered by name), the
// start-failure cleanup removes the agent's own worktree and keeps the
// branch and its commit; the files proof then keeps the record and the
// slug reserved until the agent's delete.
func TestCleanupAbortedLaunch_StartFailureKeepsReservationLinkedCheckout(t *testing.T) {
	requireWorktreeGit(t)
	t.Setenv("SCION_HOST_UID", "")
	f := newFlatInstanceTestServer(t, flatInstanceOpts{hubInProcess: true})
	f.srv.config.FlatInstance.Instance.RuntimeTarget = &config.V1RuntimeTargetConfig{Type: config.RuntimeTargetTypeDocker}
	root := flatProjectRoot(t)
	const agentID, slug, runID = "agent-id-lc", "lc-agent", "run-lc"
	require.NoError(t, f.srv.ownership.BeginRun(flatTestProjectID, agentID, slug, runID))
	wt, tip := linkedCheckoutStart(t, root, slug)
	require.NoError(t, writeLaunchMarker(root, false, slug, "L1"))

	key := ownedRunKey{projectID: flatTestProjectID, agentID: agentID, runID: runID}
	rec := newLaunchRecord("L1", agentID, store.LaunchKindCreate, "", time.Now().Add(time.Minute), func() {})
	rec.ownedRun = key
	lc := launchCtx{
		opts:  api.StartOptions{Name: slug, ProjectPath: root, HubProjectID: flatTestProjectID, RunID: runID},
		mgr:   f.mgr,
		key:   launchKey{ProjectID: flatTestProjectID, Slug: slug},
		owned: &ownedStart{hold: f.srv.holdOwnedRun(key)},
	}
	_ = f.srv.cleanupAbortedLaunch(f.mgr, rec, lc)

	_, err := os.Lstat(wt)
	assert.True(t, os.IsNotExist(err), "the agent's own worktree is removed")
	assert.Equal(t, tip, startFailureBranchTip(t, root, slug), "the branch and its commit are kept")
	assert.False(t, rec.filesRemoved.Load(), "files not proven removed while the agent's branch remains")
	got, ok, err := f.srv.ownership.Get(flatTestProjectID, agentID)
	require.NoError(t, err)
	require.True(t, ok, "the ownership record is kept")
	assert.Equal(t, OwnershipStateActive, got.State)
	assert.Equal(t, agentID, flatSlugHolder(t, f, slug), "the slug stays reserved")
}

// The same linked-checkout case through the synchronous create.
func TestSyncCreateFailure_StartFailureKeepsReservationLinkedCheckout(t *testing.T) {
	requireWorktreeGit(t)
	t.Setenv("SCION_HOST_UID", "")
	f := newFlatInstanceTestServer(t, flatInstanceOpts{hubInProcess: true})
	f.srv.config.FlatInstance.Instance.RuntimeTarget = &config.V1RuntimeTargetConfig{Type: config.RuntimeTargetTypeDocker}
	root := flatProjectRoot(t)
	const slug = "lc-sync"
	agentID := "agent-id-" + slug
	wt, tip := linkedCheckoutStart(t, root, slug)

	f.mgr.startErr = errors.New("start failed")
	w := flatCreateAccepted(f, slug)
	require.GreaterOrEqual(t, w.Code, 400, w.Body.String())

	_, err := os.Lstat(wt)
	assert.True(t, os.IsNotExist(err), "the agent's own worktree is removed")
	assert.Equal(t, tip, startFailureBranchTip(t, root, slug), "the branch and its commit are kept")
	got, ok, err := f.srv.ownership.Get(flatTestProjectID, agentID)
	require.NoError(t, err)
	require.True(t, ok, "the ownership record is kept")
	assert.Equal(t, OwnershipStateActive, got.State)
	assert.Equal(t, agentID, flatSlugHolder(t, f, slug), "the slug stays reserved")
}
