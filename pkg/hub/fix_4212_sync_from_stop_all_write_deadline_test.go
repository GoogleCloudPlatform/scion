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
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ptone/scion#4212: workspace sync-from and stop-all wait on the broker
// past the listener's WriteTimeout and extend the write deadline as the
// other synchronous lifecycle routes do (ptone/scion#3890,
// ptone/scion#4178). Each test serves through a listener whose
// WriteTimeout (200ms) is shorter than the route's wait and requires the
// hub's real answer, not a dropped connection.

// The upload tunneled to the broker answers after slowPathDelay.
func TestSlowWorkspaceSyncFrom_AfterWriteTimeout_GetsResponse(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	srv, s, project := setupSlowLaunchServer(t)
	srv.SetStorage(newContentMockStorage("test-bucket"))
	var downloaded bool
	srv.setHubWorkspaceDownloader(func(context.Context, string, string, string) error {
		downloaded = true
		return nil
	})
	agent := createSiteAgent(t, s, project, "slow-sync-from", state.PhaseRunning, store.RunIntentRunning)
	broker := connectFakeBroker(t, srv, agent.RuntimeBrokerID)
	uploaded := answerBrokerUploads(t, broker, slowPathDelay, 1)

	code, body := serveThroughSlowListener(t, srv, http.MethodPost, "/api/v1/agents/"+agent.ID+"/workspace/sync-from", nil, slowPathDelay)
	select {
	case path := <-uploaded:
		assert.Equal(t, "/api/v1/workspace/upload", path, "fixture check: the upload is tunneled")
	default:
		t.Fatal("fixture check: the upload never reached the broker")
	}
	require.Equal(t, http.StatusOK, code, string(body))
	assert.True(t, downloaded, "fixture check: the hub-managed workspace is synced back")
	var resp SyncFromResponse
	require.NoError(t, json.Unmarshal(body, &resp))
	require.NotNil(t, resp.Manifest)
	assert.Equal(t, "1.0", resp.Manifest.Version)
}

// The download into the hub workspace after the upload is bounded with it,
// so the response still lands within the extended write deadline.
func TestWorkspaceSyncFrom_HubDownloadBounded_GetsResponse(t *testing.T) {
	const syncBound = 400 * time.Millisecond
	t.Setenv("HOME", t.TempDir())
	srv, s, project := setupCreateAgentServer(t, &slowLaunchDispatcher{})
	shortenSyncDispatchTimeout(t, syncBound)
	srv.SetStorage(newContentMockStorage("test-bucket"))
	downloadErr := make(chan error, 1)
	srv.setHubWorkspaceDownloader(func(ctx context.Context, _, _, _ string) error {
		err := errDownloadNeverCut
		if awaitCanceled(ctx) {
			err = ctx.Err()
		}
		downloadErr <- err
		return err
	})
	agent := createSiteAgent(t, s, project, "sync-from-bound", state.PhaseRunning, store.RunIntentRunning)
	broker := connectFakeBroker(t, srv, agent.RuntimeBrokerID)
	answerBrokerUploads(t, broker, 50*time.Millisecond, 1)

	code, body := serveThroughSlowListener(t, srv, http.MethodPost, "/api/v1/agents/"+agent.ID+"/workspace/sync-from", nil, syncBound)
	select {
	case err := <-downloadErr:
		require.ErrorIs(t, err, context.DeadlineExceeded, "the hub download must be cut at the sync-from bound")
	default:
		t.Fatal("fixture check: the hub download never ran")
	}
	assert.Equal(t, http.StatusOK, code, string(body))
}

// Each agent's stop dispatch answers after slowPathDelay; the agents stop
// in parallel.
func TestSlowStopAll_AfterWriteTimeout_GetsResponse(t *testing.T) {
	srv, s, project := setupSlowLaunchServer(t)
	setStopAllAgentOpTimeout(t, 3*time.Second)
	setAgentQuotaLimits(t, s)
	a1 := createSiteAgent(t, s, project, "slow-stop-all-1", state.PhaseRunning, store.RunIntentRunning)
	a2 := createSiteAgent(t, s, project, "slow-stop-all-2", state.PhaseRunning, store.RunIntentRunning)

	code, body := serveThroughSlowListener(t, srv, http.MethodPost, "/api/v1/projects/"+project.ID+"/agents/stop-all", nil, slowPathDelay)
	require.Equal(t, http.StatusOK, code, string(body))
	var resp StopAllAgentsResponse
	require.NoError(t, json.Unmarshal(body, &resp))
	assert.Equal(t, 2, resp.Stopped, string(body))
	assert.Equal(t, 0, resp.Failed, string(body))
	for _, id := range []string{a1.ID, a2.ID} {
		got, err := s.GetAgent(context.Background(), id)
		require.NoError(t, err)
		assert.Equal(t, string(state.PhaseStopped), got.Phase)
	}
}

// A broker that never answers the upload: the tunnel is cut at the
// sync-from bound and the hub answers 504, as a tunnel timeout does.
func TestWorkspaceSyncFrom_BrokerNeverAnswers_GatewayTimeout(t *testing.T) {
	const syncBound = 300 * time.Millisecond
	t.Setenv("HOME", t.TempDir())
	srv, s, project := setupCreateAgentServer(t, &slowLaunchDispatcher{})
	shortenSyncDispatchTimeout(t, syncBound)
	srv.SetStorage(newContentMockStorage("test-bucket"))
	agent := createSiteAgent(t, s, project, "sync-from-silent", state.PhaseRunning, store.RunIntentRunning)
	connectFakeBroker(t, srv, agent.RuntimeBrokerID) // connected, never answers.

	start := time.Now()
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+agent.ID+"/workspace/sync-from", nil)
	elapsed := time.Since(start)
	assert.Equal(t, http.StatusGatewayTimeout, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), ErrCodeBrokerTimeout)
	assert.GreaterOrEqual(t, elapsed, syncBound, "fixture check: the tunnel waits for the bound")
	assert.Less(t, elapsed, 5*time.Second, "the tunnel is cut at the sync-from bound")
}
