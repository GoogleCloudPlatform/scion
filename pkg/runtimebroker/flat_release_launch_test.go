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
	"context"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// Async launch side of releasing a failed create's reservation
// (ptone/scion#5768): an async create's run is the launch's from its
// admission, so a failure before Manager.Start (claim, marker, workspace
// download, keepalive abort) finishes it, and a cleanup that proved the
// agent's files removed retires the record.

// flatProjectRoot creates the fixture's hub-managed project directory,
// identifying as flatTestProjectID, and returns it.
// The fixture's HOME is the test's own (setupTestScionEnv sets it with
// t.Setenv), so nothing written here outlives the test.
func flatProjectRoot(t *testing.T) string {
	t.Helper()
	globalDir, err := config.GetGlobalDir()
	require.NoError(t, err)
	root := filepath.Join(globalDir, "projects", "flat-project")
	scionDir := filepath.Join(root, config.DotScion)
	require.NoError(t, os.MkdirAll(filepath.Join(scionDir, "agents"), 0o755))
	require.NoError(t, config.WriteProjectID(scionDir, flatTestProjectID))
	return root
}

// TestCleanupAbortedLaunch_RetiresFlatCreate: an async create launch that
// failed before Manager.Start (its run recorded no object) and whose
// cleanup removed its files retires its record and releases the slug, on a
// Docker instance only, for a create launch only, and only when the
// removal is proven.
func TestCleanupAbortedLaunch_RetiresFlatCreate(t *testing.T) {
	cases := []struct {
		name        string
		kind        string
		target      string
		markerOther bool // a newer launch's marker: the files are not this launch's
		noProject   bool // the project path does not exist
		wantRetired bool
	}{
		{name: "create launch", kind: store.LaunchKindCreate, target: "docker", wantRetired: true},
		{name: "kubernetes instance", kind: store.LaunchKindCreate, target: "kubernetes"},
		{name: "start launch", kind: store.LaunchKindStart, target: "docker"},
		{name: "files not removed", kind: store.LaunchKindCreate, target: "docker", markerOther: true},
		{name: "project unresolved", kind: store.LaunchKindCreate, target: "docker", noProject: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFlatInstanceTestServer(t, flatInstanceOpts{hubInProcess: true})
			f.srv.config.FlatInstance.Instance.RuntimeTarget = &config.V1RuntimeTargetConfig{Type: tc.target}
			root := flatProjectRoot(t)
			const agentID, slug, runID = "agent-id-async", "async-agent", "run-async"
			require.NoError(t, f.srv.ownership.BeginRun(flatTestProjectID, agentID, slug, runID))
			require.NoError(t, os.MkdirAll(filepath.Join(root, config.DotScion, "agents", slug, "workspace"), 0o755))
			marker := "L1"
			if tc.markerOther {
				marker = "L-newer"
			}
			require.NoError(t, writeLaunchMarker(root, false, slug, marker))
			projectPath := root
			if tc.noProject {
				projectPath = filepath.Join(t.TempDir(), "gone")
			}
			key := ownedRunKey{projectID: flatTestProjectID, agentID: agentID, runID: runID}
			rec := newLaunchRecord("L1", agentID, tc.kind, "", time.Now().Add(time.Minute), func() {})
			rec.ownedRun = key // as acceptAsyncLaunch records it for a create
			lc := launchCtx{
				opts:  api.StartOptions{Name: slug, ProjectPath: projectPath, HubProjectID: flatTestProjectID, RunID: runID},
				mgr:   f.mgr,
				key:   launchKey{ProjectID: flatTestProjectID, Slug: slug},
				owned: &ownedStart{hold: f.srv.holdOwnedRun(key)},
			}
			_ = f.srv.cleanupAbortedLaunch(f.mgr, rec, lc)

			got, ok, err := f.srv.ownership.Get(flatTestProjectID, agentID)
			require.NoError(t, err)
			holder := flatSlugHolder(t, f, slug)
			if tc.wantRetired {
				assert.False(t, ok, "retired")
				assert.Empty(t, holder, "slug released")
				return
			}
			require.True(t, ok, "kept")
			assert.Equal(t, OwnershipStateActive, got.State)
			assert.Equal(t, agentID, holder)
		})
	}
}

// TestCleanupAbortedLaunch_PreStartRunFinished: the run of an async create
// that failed before Manager.Start is finished by the cleanup (no longer
// live), so the Hub's or the user's later delete can retire the record even
// where the files cannot be proven absent.
func TestCleanupAbortedLaunch_PreStartRunFinished(t *testing.T) {
	f := newFlatInstanceTestServer(t, flatInstanceOpts{hubInProcess: true})
	f.srv.config.FlatInstance.Instance.RuntimeTarget = &config.V1RuntimeTargetConfig{Type: "kubernetes"}
	const agentID, slug, runID = "agent-id-async", "async-agent", "run-async"
	require.NoError(t, f.srv.ownership.BeginRun(flatTestProjectID, agentID, slug, runID))
	key := ownedRunKey{projectID: flatTestProjectID, agentID: agentID, runID: runID}
	rec := newLaunchRecord("L1", agentID, store.LaunchKindCreate, "", time.Now().Add(time.Minute), func() {})
	rec.ownedRun = key
	lc := launchCtx{opts: api.StartOptions{Name: slug, RunID: runID}, mgr: f.mgr,
		key: launchKey{ProjectID: flatTestProjectID, Slug: slug}, owned: &ownedStart{hold: f.srv.holdOwnedRun(key)}}
	_ = f.srv.cleanupAbortedLaunch(f.mgr, rec, lc)
	got, ok, err := f.srv.ownership.Get(flatTestProjectID, agentID)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, OwnershipStateDeleted, got.Run(runID).State, "the pre-Start run is finished")

	w := serveFlat(f.srv, http.MethodDelete, "/api/v1/agents/"+slug+"?projectId="+flatTestProjectID+"&deleteFiles=true", "")
	require.Equal(t, http.StatusNotFound, w.Code, w.Body.String())
	assert.Empty(t, flatSlugHolder(t, f, slug), "the later delete releases the slug")
}

// TestAcceptAsyncLaunch_CreateRunRecordedAtAdmission: an accepted async
// create records its run on the launch before its first journal write; an
// async start does not (it records it when it reaches Manager.Start).
func TestAcceptAsyncLaunch_CreateRunRecordedAtAdmission(t *testing.T) {
	observe := func(t *testing.T) (seen *ownedRunKey, called *bool) {
		seen, called = &ownedRunKey{}, new(bool)
		testHookAsyncLaunchBeforeHandOff = func(_ context.Context, _ launchKey, rec *launchRecord, _ launchCtx) {
			rec.ownedRunMu.Lock()
			*seen = rec.ownedRun
			rec.ownedRunMu.Unlock()
			*called = true
			rec.cancel() // the launch ends as for a local stop; only admission matters here
		}
		t.Cleanup(func() { testHookAsyncLaunchBeforeHandOff = nil })
		return seen, called
	}
	t.Run("create", func(t *testing.T) {
		f := newFlatInstanceTestServer(t, flatInstanceOpts{hubInProcess: true})
		seen, called := observe(t)
		w := serveFlat(f.srv, http.MethodPost, "/api/v1/agents", flatAsyncCreateBody(f, "async-admit", map[string]interface{}{"runId": "run-admit"}))
		require.Less(t, w.Code, 300, w.Body.String())
		require.True(t, *called)
		assert.Equal(t, ownedRunKey{projectID: flatTestProjectID, agentID: "agent-id-async-admit", runID: "run-admit"}, *seen)
	})
	t.Run("start", func(t *testing.T) {
		f := newFlatInstanceTestServer(t, flatInstanceOpts{hubInProcess: true})
		f.seedOwnedAgent(t)
		f.mgr.mu.Lock()
		f.mgr.agents[0].Phase, f.mgr.agents[0].ContainerStatus = "stopped", "Exited (0) 1 minute ago"
		f.mgr.mu.Unlock()
		seen, called := observe(t)
		w := postAsyncStart(t, f.srv, "test-agent-1",
			"projectId="+flatTestProjectID+"&"+api.AgentIDQueryParam+"="+flatTestAgentID+"&runId=run-start",
			map[string]any{"asyncLaunch": true, "launchId": "L-start", "launchTimeoutSeconds": 300, "runId": "run-start",
				"expectedRuntimeTargetId": f.identity.RuntimeTarget.ID,
				"resolvedEnv":             map[string]any{"SCION_AGENT_ID": flatTestAgentID}})
		require.Less(t, w.Code, 300, w.Body.String())
		require.True(t, *called, "the async start reached admission")
		assert.Equal(t, ownedRunKey{}, *seen, "a start records its run only when it reaches Manager.Start")
	})
}

// TestRecoverLaunches_RetiresFlatCreate: boot recovery of a create launch
// that failed before Manager.Start (journalled with its run from
// admission) finishes the run, removes the agent's files and retires the
// record, releasing the slug.
func TestRecoverLaunches_RetiresFlatCreate(t *testing.T) {
	mgr := newAsyncManager()
	srv, rtb := newAsyncTestServer(t, mgr)
	rtb.launchReportFunc = appliedReports
	srv.ownership = NewOwnershipStore(t.TempDir(), "broker-a")
	srv.config.FlatInstance = &FlatInstanceConfig{Instance: config.V1RuntimeBrokerInstanceConfig{
		RuntimeTarget: &config.V1RuntimeTargetConfig{Type: "docker"}}}
	globalDir, err := config.GetGlobalDir()
	require.NoError(t, err)
	root := filepath.Join(globalDir, "projects", "abandon")
	scionDir := filepath.Join(root, config.DotScion)
	require.NoError(t, os.MkdirAll(filepath.Join(scionDir, "agents", "agent-L9", "workspace"), 0o755))
	require.NoError(t, config.WriteProjectID(scionDir, abandonProjectID))
	require.NoError(t, writeLaunchMarker(root, false, "agent-L9", "L9"))
	require.NoError(t, srv.ownership.BeginRun(abandonProjectID, "agent-id-L9", "agent-L9", "run-L9"))

	jr := launchingRecord("L9")
	jr.ProjectID, jr.ProjectPath, jr.HubProjectID, jr.Handles = abandonProjectID, root, abandonProjectID, nil
	jr.OwnedRun = &journalOwnedRun{ProjectID: abandonProjectID, AgentID: "agent-id-L9", RunID: "run-L9"}
	writeDeadRecord(t, srv, jr)
	runRecovery(t, srv)

	_, ok, err := srv.ownership.Get(abandonProjectID, "agent-id-L9")
	require.NoError(t, err)
	assert.False(t, ok, "retired")
	holder, err := srv.ownership.SlugHolder(abandonProjectID, "agent-L9")
	require.NoError(t, err)
	assert.Empty(t, holder, "slug released")
}
