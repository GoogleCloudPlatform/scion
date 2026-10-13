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
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/transfer"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ptone/scion#3890: the other synchronous paths that wait on a broker
// dispatch (lifecycle start and restart, env submit, workspace-bootstrap
// finalize, a start or resume through create) and the create-time workspace
// upload extend the write deadline as create does (ptone/scion#3850). A wait
// that ends after the listener's WriteTimeout, but within the path's own
// budget, must get its real response, not a dropped connection. Timeouts are
// scaled down: WriteTimeout 200ms, each broker call 400ms, dispatch wait 3s.

// setHubWorkspaceUploadTimeout sets hubWorkspaceUploadTimeout for one test.
func setHubWorkspaceUploadTimeout(t *testing.T, d time.Duration) {
	t.Helper()
	prev := hubWorkspaceUploadTimeout
	hubWorkspaceUploadTimeout = d
	t.Cleanup(func() { hubWorkspaceUploadTimeout = prev })
}

func requireAgentRunning(t *testing.T, s store.Store, id string) {
	t.Helper()
	got, err := s.GetAgent(context.Background(), id)
	require.NoError(t, err)
	assert.Equal(t, string(state.PhaseRunning), got.Phase)
}

func TestSlowLifecycleStart_AfterWriteTimeout_GetsResponse(t *testing.T) {
	srv, s, project := setupSlowLaunchServer(t)
	setAgentQuotaLimits(t, s)
	agent := createSiteAgent(t, s, project, "slow-start", state.PhaseStopped, store.RunIntentStopped)

	code, body := serveThroughSlowListener(t, srv, http.MethodPost, "/api/v1/agents/"+agent.ID+"/start", nil, slowPathDelay)
	assert.Equal(t, http.StatusOK, code, string(body))
	requireAgentRunning(t, s, agent.ID)
}

// The restart waits on two legs, each slower than the WriteTimeout. The
// timeouts are set so the two legs together outlast every other path's
// budget but not the restart's own: the restart site must pass
// restartWriteBudget.
func TestSlowLifecycleRestart_AfterWriteTimeout_GetsResponse(t *testing.T) {
	const legDelay = 300 * time.Millisecond
	srv, s, project := setupSlowLaunchServerWithDelay(t, legDelay)
	shortenSyncDispatchTimeout(t, 400*time.Millisecond)
	setSyncDispatchWriteSlack(t, 50*time.Millisecond)
	setHubWorkspaceUploadTimeout(t, 10*time.Millisecond)
	require.Less(t, syncDispatchWriteBudget(), 2*legDelay, "fixture check: the one-dispatch budget must not cover both legs")
	require.Less(t, hubWorkspaceUploadWriteBudget(), 2*legDelay, "fixture check: the upload budget must not cover both legs")
	require.Greater(t, restartWriteBudget(), 2*legDelay+time.Second, "fixture check: the restart budget must cover both legs")
	setAgentQuotaLimits(t, s)
	agent := createSiteAgent(t, s, project, "slow-restart", state.PhaseRunning, store.RunIntentRunning)

	code, body := serveThroughSlowListener(t, srv, http.MethodPost, "/api/v1/agents/"+agent.ID+"/restart", nil, 2*legDelay)
	assert.Equal(t, http.StatusOK, code, string(body))
	requireAgentRunning(t, s, agent.ID)
}

func TestSlowSubmitEnv_AfterWriteTimeout_GetsResponse(t *testing.T) {
	srv, s, project := setupSlowLaunchServer(t)
	agent := createSiteAgent(t, s, project, "slow-env", state.PhaseProvisioning, store.RunIntentRunning)

	code, body := serveThroughSlowListener(t, srv, http.MethodPost, "/api/v1/agents/"+agent.ID+"/env", SubmitEnvRequest{
		Env: map[string]string{"SOME_REQUIRED_KEY": "v"},
	}, slowPathDelay)
	assert.Equal(t, http.StatusOK, code, string(body))
	requireAgentRunning(t, s, agent.ID)
}

func TestSlowWorkspaceBootstrapFinalize_AfterWriteTimeout_GetsResponse(t *testing.T) {
	srv, s, project := setupSlowLaunchServer(t)
	srv.SetStorage(newContentMockStorage("test-bucket"))
	agent := createSiteAgent(t, s, project, "slow-bootstrap", state.PhaseProvisioning, store.RunIntentRunning)

	code, body := serveThroughSlowListener(t, srv, http.MethodPost, "/api/v1/agents/"+agent.ID+"/workspace/sync-to/finalize", SyncToFinalizeRequest{
		Manifest: &transfer.Manifest{Version: "1.0"},
	}, slowPathDelay)
	assert.Equal(t, http.StatusOK, code, string(body))
	requireAgentRunning(t, s, agent.ID)
}

// A create that starts or resumes an existing agent (handleExistingAgent)
// runs before create's own extension, so each of its start branches extends.
func TestSlowCreateExistingAgent_AfterWriteTimeout_GetsResponse(t *testing.T) {
	for _, tc := range []struct {
		name  string
		phase state.Phase
		req   CreateAgentRequest
	}{
		{name: "suspended-resume", phase: state.PhaseSuspended},
		{name: "stopped-resume", phase: state.PhaseStopped, req: CreateAgentRequest{Resume: true}},
		{name: "provisioning-start", phase: state.PhaseProvisioning},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, s, project := setupSlowLaunchServer(t)
			name := "slow-existing-" + tc.name
			agent := createSiteAgent(t, s, project, name, tc.phase, store.RunIntentStopped)

			req := tc.req
			req.Name, req.ProjectID, req.Task = name, project.ID, "work"
			code, body := serveThroughSlowListener(t, srv, http.MethodPost, "/api/v1/agents", req, slowPathDelay)
			assert.Equal(t, http.StatusOK, code, string(body))
			requireAgentRunning(t, s, agent.ID)
		})
	}
}

// A create-time workspace upload that runs out of its own budget, after the
// WriteTimeout, is answered with its failure instead of a dropped
// connection. The timeouts are set so the upload outlasts every other
// path's budget but not its own: the upload site must pass
// hubWorkspaceUploadWriteBudget.
func TestSlowHubWorkspaceUpload_BudgetExpiredAfterWriteTimeout_GetsResponse(t *testing.T) {
	const uploadTimeout = 600 * time.Millisecond
	t.Setenv("HOME", t.TempDir())

	srv, s, project := setupSlowLaunchServer(t) // hub-managed: no GitRemote.
	setHubWorkspaceUploadTimeout(t, uploadTimeout)
	shortenSyncDispatchTimeout(t, 50*time.Millisecond)
	setWorkspaceCheckTimeout(t, 10*time.Millisecond)
	setSyncDispatchWriteSlack(t, 300*time.Millisecond)
	require.Less(t, syncDispatchWriteBudget(), uploadTimeout, "fixture check: the one-dispatch budget must not cover the upload")
	require.Less(t, restartWriteBudget(), uploadTimeout, "fixture check: the restart budget must not cover the upload")
	srv.SetStorage(newGCSContentMockStorage("test-bucket"))
	setAgentQuotaLimits(t, s)
	t.Cleanup(func() {
		if p, err := hubManagedProjectPath(project.Slug); err == nil {
			_ = os.RemoveAll(p)
		}
	})
	var uploadCalled atomic.Bool
	prev := syncToGCSForWorkspaceUpload
	syncToGCSForWorkspaceUpload = func(uctx context.Context, _, _, _ string) error {
		uploadCalled.Store(true)
		<-uctx.Done()
		return uctx.Err()
	}
	t.Cleanup(func() { syncToGCSForWorkspaceUpload = prev })

	code, body := serveThroughSlowListener(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name: "slow-upload", ProjectID: project.ID, Task: "work",
	}, uploadTimeout)
	require.True(t, uploadCalled.Load(), "fixture check: the upload branch must be reached")
	// The hub's own answer (RuntimeError, 502 with a JSON body), not an
	// empty response from a dropped connection.
	assert.Equal(t, http.StatusBadGateway, code, string(body))
	assert.Contains(t, string(body), "Timed out uploading the project workspace")
}

// The per-path budgets cover each path's own wait plus the slack, and all
// outlast the default WriteTimeout. extendWriteDeadline applies the budget
// it is given under the same rule as extendWriteDeadlineForSyncDispatch.
func TestSyncDispatchWriteBudgets(t *testing.T) {
	def := DefaultServerConfig().WriteTimeout
	assert.Equal(t, workspaceCheckTimeout+2*syncDispatchTimeout+syncDispatchWriteSlack, restartWriteBudget())
	assert.Greater(t, restartWriteBudget(), syncDispatchWriteBudget(), "a restart waits on two legs")
	assert.Equal(t, hubWorkspaceUploadTimeout+syncDispatchWriteSlack, hubWorkspaceUploadWriteBudget())
	for _, b := range []time.Duration{syncDispatchWriteBudget(), restartWriteBudget(), hubWorkspaceUploadWriteBudget()} {
		assert.Greater(t, b, def)
	}

	budget := restartWriteBudget()
	for _, tc := range []struct {
		name       string
		configured time.Duration
		want       bool
	}{
		{"default", def, true},
		{"unbounded", 0, false},
		{"already longer", budget + time.Minute, false},
		// Longer than the one-dispatch budget but shorter than this one.
		{"between budgets", syncDispatchWriteBudget() + time.Second, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := &deadlineRecorder{ResponseRecorder: httptest.NewRecorder()}
			start := time.Now()
			extendWriteDeadline(context.Background(), &responseWriter{ResponseWriter: rec}, tc.configured, budget)
			rec.mu.Lock()
			defer rec.mu.Unlock()
			if !tc.want {
				assert.Empty(t, rec.deadlines)
				return
			}
			require.Len(t, rec.deadlines, 1)
			assert.False(t, rec.deadlines[0].Before(start.Add(budget)))
			assert.False(t, rec.deadlines[0].After(time.Now().Add(budget)))
		})
	}
}
