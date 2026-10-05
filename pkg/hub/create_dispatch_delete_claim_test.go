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
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The create dispatch's full-row write (updateAgentAfterDispatch) must not
// change the phase of a row a delete has claimed (ptone/scion#3055, design
// ptone/scion#2483 §2.1: phase writers outside UpdateAgentStatus respect the
// deletion predicate).

// newRaceSyncCreateServer is newRaceAsyncCreateServer with async launch off,
// so createAgent takes the synchronous broker create path through the real
// HTTPAgentDispatcher.
func newRaceSyncCreateServer(t *testing.T) (*Server, store.Store, *store.Project, *raceAsyncClient) {
	t.Helper()
	srv, s, project, client := newRaceAsyncCreateServer(t)
	d := NewHTTPAgentDispatcherWithClient(s, client, false, slog.Default())
	d.SetAsyncLaunchSettingsProvider(func() AsyncLaunchSettings { return AsyncLaunchSettings{} })
	srv.SetDispatcher(d)
	return srv, s, project, client
}

// syncRunningAnswer is the broker's answer to a synchronous create that
// started the container.
func syncRunningAnswer(req *RemoteCreateAgentRequest) *RemoteAgentResponse {
	return &RemoteAgentResponse{
		Agent: &RemoteAgentInfo{
			ID: "container-" + req.Slug, Slug: req.Slug, Name: req.Name,
			Phase: string(state.PhaseRunning), ContainerStatus: "Up 1 second",
			RunID: req.RunID,
		},
		Created: true,
	}
}

// A synchronous create dispatch returns while a DELETE is blocked in its
// broker call (the N3 probe). The dispatch write leaves the claimed row's
// phase alone, no created is published, and the delete completes with
// exactly one deleted.
func TestCreateDispatchWrite_DeleteClaimed_KeepsPhase(t *testing.T) {
	for i, tc := range []struct {
		name      string
		retention time.Duration
	}{
		{"hard", 0},
		{"retention-on", time.Hour},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, s, project, client := newRaceSyncCreateServer(t)
			srv.config.SoftDeleteRetention = tc.retention
			pub := recordCreatedEvents(t, srv)

			var sent *RemoteCreateAgentRequest
			var atClaim *store.Agent
			var delCh <-chan deleteResult
			var compensating atomic.Int32
			release := make(chan struct{})
			client.answer = func(req *RemoteCreateAgentRequest) (*RemoteAgentResponse, *RemoteEnvRequirementsResponse, error) {
				sent = req
				require.False(t, req.AsyncLaunch, "the create is dispatched synchronously")
				entered := make(chan struct{})
				var once sync.Once
				client.setDeleteFn(func(ctx context.Context) error {
					first := false
					once.Do(func() { first = true; close(entered) })
					if !first {
						// The create's compensating delete (the engine's
						// own is the blocked first call).
						compensating.Add(1)
						return nil
					}
					select {
					case <-release:
						return nil
					case <-ctx.Done():
						return ctx.Err()
					}
				})
				delCh = deleteAsync(t, srv, "/api/v1/agents/"+req.ID, nil)
				waitClosed(t, entered, 5*time.Second, "broker delete dispatch")
				atClaim = mustGetAgent(t, s, req.ID)
				require.Equal(t, store.DeletionStateDeleting, atClaim.DeletionState)
				return syncRunningAnswer(req), nil, nil
			}

			rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", map[string]interface{}{
				"name": "claim-sync-" + string(rune('a'+i)), "projectId": project.ID, "task": "do it",
			})
			require.NotNil(t, sent, "dispatch ran: %d %s", rec.Code, rec.Body.String())

			after := mustGetAgent(t, s, sent.ID)
			assert.Equal(t, atClaim.Phase, after.Phase,
				"the dispatch write must not change the phase of a delete-claimed row")
			assert.Equal(t, atClaim.Activity, after.Activity)
			assert.Equal(t, store.DeletionStateDeleting, after.DeletionState, "the claim is untouched")
			assert.Equal(t, atClaim.RuntimeBrokerID, after.RuntimeBrokerID, "the broker the engine targets is kept")
			assert.Equal(t, atClaim.RunID, after.RunID, "the run the engine deletes is kept")
			assert.Zero(t, pub.count("created"), "no created while the delete holds the row: %v", pub.kinds())
			assert.Equal(t, int32(1), compensating.Load(),
				"the run that landed under a delete claim is deleted again (ptone/scion#3055)")

			close(release)
			r := waitDelete(t, delCh, 10*time.Second)
			require.Equal(t, http.StatusNoContent, r.rec.Code, r.rec.Body.String())
			assert.Zero(t, pub.count("created"), "no created at all: %v", pub.kinds())
			assert.Equal(t, 1, pub.count("deleted"), "exactly one deleted: %v", pub.kinds())
			assertNoStoppedStatus(t, pub)
			assertDeleteLanded(t, s, sent.ID, tc.retention)
		})
	}
}

// Control: with no delete, the synchronous create's dispatch write still
// advances the phase to the broker's answer.
func TestCreateDispatchWrite_NoDelete_UpdatesPhase(t *testing.T) {
	srv, s, project, client := newRaceSyncCreateServer(t)
	pub := recordCreatedEvents(t, srv)

	var sent *RemoteCreateAgentRequest
	client.answer = func(req *RemoteCreateAgentRequest) (*RemoteAgentResponse, *RemoteEnvRequirementsResponse, error) {
		sent = req
		return syncRunningAnswer(req), nil, nil
	}

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", map[string]interface{}{
		"name": "claim-sync-control", "projectId": project.ID, "task": "do it",
	})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	require.NotNil(t, sent)

	after := mustGetAgent(t, s, sent.ID)
	assert.Equal(t, string(state.PhaseRunning), after.Phase, "the broker's phase is persisted")
	assert.Equal(t, "container:container-"+sent.Slug, after.RuntimeState)
	assert.Equal(t, 1, pub.count("created"), "created is published: %v", pub.kinds())
}

// dispatchRetryFixture is a running agent on an online broker, with a copy
// read before any concurrent write, as a dispatch holds it.
func dispatchRetryFixture(t *testing.T, suffix string) (*Server, store.Store, *engineStubDispatcher, *store.Agent) {
	t.Helper()
	srv, s := testServer(t)
	disp := &engineStubDispatcher{}
	srv.SetDispatcher(disp)
	_, _, agent := setupOnlineBrokerAgent(t, s, suffix)
	current := mustGetAgent(t, s, agent.ID)
	current.Phase = string(state.PhaseProvisioning)
	require.NoError(t, s.UpdateAgent(context.Background(), current))
	return srv, s, disp, mustGetAgent(t, s, agent.ID)
}

// The conflict-retry path: a delete claimed a provisioning row (phase
// stopping) after the dispatch read it. The retry re-reads the row and must
// keep stopping, not copy the dispatch's running over it.
func TestUpdateAgentAfterDispatch_DeleteClaimedStopping_KeepsPhase(t *testing.T) {
	srv, s, disp, stale := dispatchRetryFixture(t, "retry-stopping")

	entered := make(chan struct{})
	release := make(chan struct{})
	disp.setFn(blockingDelete(entered, release, nil))
	delCh := deleteAsync(t, srv, "/api/v1/agents/"+stale.ID, nil)
	waitClosed(t, entered, 5*time.Second, "broker delete dispatch")
	claimed := mustGetAgent(t, s, stale.ID)
	require.Equal(t, store.DeletionStateDeleting, claimed.DeletionState)
	require.Equal(t, string(state.PhaseStopping), claimed.Phase, "the claim stops an active row")

	stale.Phase = string(state.PhaseRunning)
	stale.Activity = "working"
	stale.ContainerStatus = "Up 1 second"
	stale.Template = "tmpl-from-broker"
	require.NoError(t, srv.updateAgentAfterDispatch(context.Background(), stale))

	after := mustGetAgent(t, s, stale.ID)
	assert.Equal(t, string(state.PhaseStopping), after.Phase, "the retry must not overwrite stopping")
	assert.Equal(t, claimed.Activity, after.Activity)
	assert.Equal(t, store.DeletionStateDeleting, after.DeletionState)
	assert.Equal(t, "tmpl-from-broker", after.Template, "non-status fields are still merged")

	close(release)
	r := waitDelete(t, delCh, 10*time.Second)
	require.Equal(t, http.StatusNoContent, r.rec.Code, r.rec.Body.String())
	assert.True(t, agentGone(t, s, stale.ID))
}

// Control for the retry path: a concurrent write that is not a delete claim
// still lets the dispatch's phase through on the retry.
func TestUpdateAgentAfterDispatch_ConflictWithoutDelete_UpdatesPhase(t *testing.T) {
	srv, s, _, stale := dispatchRetryFixture(t, "retry-control")

	concurrent := mustGetAgent(t, s, stale.ID)
	concurrent.TaskSummary = "concurrent write"
	require.NoError(t, s.UpdateAgent(context.Background(), concurrent))

	stale.Phase = string(state.PhaseRunning)
	stale.Activity = "working"
	require.NoError(t, srv.updateAgentAfterDispatch(context.Background(), stale))

	after := mustGetAgent(t, s, stale.ID)
	assert.Equal(t, string(state.PhaseRunning), after.Phase, "the retry applies the dispatch's phase")
	assert.Equal(t, "working", after.Activity)
	assert.Equal(t, "concurrent write", after.TaskSummary, "the retry merges onto the re-read row")
}
