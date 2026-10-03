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
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Lease renewal and the routed items of ptone/scion#2483 phase 1a-2.

// The lease renews while the engine works, without bumping updated; a
// renewal that finds the claim taken over stops the engine (lost) with no
// rollback write, and the requester joins the new holder.
func TestAgentDeleteEngine_LeaseRenewal(t *testing.T) {
	setDeleteKnob(t, &deleteLeaseRenewInterval, 30*time.Millisecond)
	setDeleteKnob(t, &deleteSyncWait, 600*time.Millisecond)
	srv, s, _, disp := engineTestServer(t)
	entered, release := make(chan struct{}), make(chan struct{})
	defer close(release)
	disp.setFn(blockingDelete(entered, release, nil))
	agent := setupBrokerAgentInPhase(t, s, "renew", state.PhaseRunning)

	ch := deleteAsync(t, srv, "/api/v1/agents/"+agent.ID, nil)
	waitClosed(t, entered, 5*time.Second, "broker delete dispatch")
	first := mustGetAgent(t, s, agent.ID)
	require.NotNil(t, first.DeletionLeaseAt)

	require.Eventually(t, func() bool {
		got := mustGetAgent(t, s, agent.ID)
		return got.DeletionLeaseAt != nil && got.DeletionLeaseAt.After(*first.DeletionLeaseAt)
	}, 3*time.Second, 10*time.Millisecond, "the lease advances")
	renewed := mustGetAgent(t, s, agent.ID)
	assert.True(t, renewed.Updated.Equal(first.Updated), "a renewal does not bump updated")

	// Another request takes the claim over.
	seedAgentDeletion(t, s, agent.ID, seedLiveDeleting)
	taken := mustGetAgent(t, s, agent.ID)
	require.Greater(t, taken.DeletionClaim, first.DeletionClaim)

	r := waitDelete(t, ch, 5*time.Second)
	assert.Equal(t, http.StatusAccepted, r.rec.Code, "lost → the requester joins the live holder: %s", r.rec.Body.String())
	got := mustGetAgent(t, s, agent.ID)
	assert.Equal(t, taken.DeletionClaim, got.DeletionClaim)
	assert.Equal(t, store.DeletionStateDeleting, got.DeletionState, "no rollback over the new holder")
	assert.True(t, got.DeletedAt.IsZero())
}

// Suspend, like stop, is a 200 no-op while a delete is taking the agent
// down: no phase change and the marker is untouched.
func TestAgentDeleteRouted_SuspendNoopDuringDelete(t *testing.T) {
	for _, seed := range []deleteSeed{seedLiveDeleting, seedExpiredFinalize} {
		t.Run(seed.name, func(t *testing.T) {
			srv, s, _, _ := engineTestServer(t)
			agent := setupBrokerAgentInPhase(t, s, "suspnoop-"+seed.state, state.PhaseRunning)
			seedAgentDeletion(t, s, agent.ID, seed)
			before := mustGetAgent(t, s, agent.ID)

			rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+agent.ID+"/suspend", nil)
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			got := mustGetAgent(t, s, agent.ID)
			assert.Equal(t, string(state.PhaseRunning), got.Phase)
			assert.Equal(t, before.DeletionState, got.DeletionState)
			assert.Equal(t, before.DeletionClaim, got.DeletionClaim)
		})
	}
}

// Stop-all skips rows a delete owns.
func TestAgentDeleteRouted_StopAllSkipsDeletingRows(t *testing.T) {
	srv, s, _, _ := engineTestServer(t)
	ctx := context.Background()
	deleting := setupBrokerAgentInPhase(t, s, "stopall", state.PhaseRunning)
	seedAgentDeletion(t, s, deleting.ID, seedLiveDeleting)
	other := &store.Agent{
		ID: tid("stopall-other"), Slug: "stopall-other", Name: "Stop All Other",
		ProjectID: deleting.ProjectID, RuntimeBrokerID: deleting.RuntimeBrokerID,
		Phase: string(state.PhaseRunning),
	}
	require.NoError(t, s.CreateAgent(ctx, other))

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/projects/"+deleting.ProjectID+"/agents/stop-all", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var resp StopAllAgentsResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Len(t, resp.Results, 1)
	assert.Equal(t, other.ID, resp.Results[0].ID)

	got := mustGetAgent(t, s, deleting.ID)
	assert.Equal(t, string(state.PhaseRunning), got.Phase, "the deleting row was not stopped")
	assert.Equal(t, store.DeletionStateDeleting, got.DeletionState)
}

// Restore answers with the enriched GET shape, including the deletion view.
func TestAgentDeleteRouted_RestoreReturnsEnrichedShape(t *testing.T) {
	srv, s, _, _ := engineTestServer(t)
	agent := setupBrokerAgentInPhase(t, s, "restore-enriched", state.PhaseStopped)
	agent.DeletedAt = time.Now()
	require.NoError(t, s.UpdateAgent(context.Background(), agent))
	// An ordinary failure from an earlier attempt, no outstanding intent:
	// restore is allowed, and the response carries the banner.
	seedAgentDeletion(t, s, agent.ID, deleteSeed{
		name: "failed", state: store.DeletionStateFailed, leaseIn: -time.Minute, code: store.DeletionCodeRuntimeError,
	})

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/projects/"+agent.ProjectID+"/agents/"+agent.ID+"/restore", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var body map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Contains(t, body, "harnessCapabilities", "the enriched GET shape")
	require.Contains(t, body, "deletion")
	var view store.DeletionInfo
	require.NoError(t, json.Unmarshal(body["deletion"], &view))
	assert.Equal(t, store.DeletionCodeRuntimeError, view.Code)
	assert.True(t, mustGetAgent(t, s, agent.ID).DeletedAt.IsZero(), "restored")
}

// The finalize seam (ptone/scion#2121 attachment point) runs inside the
// finalize transaction for soft and hard deletes; an error from it rolls the
// finalize back: finalize_failed, the row stays live in finalizing, and a
// retry finalizes.
func TestAgentDeleteEngine_FinalizeSeamErrorRollsBack(t *testing.T) {
	for _, tc := range []struct {
		name      string
		retention time.Duration
		wantMode  store.DeletionFinalizeMode
	}{{"hard", 0, store.DeletionFinalizeHard}, {"soft", time.Hour, store.DeletionFinalizeSoft}} {
		t.Run(tc.name, func(t *testing.T) {
			srv, s, _, disp := engineTestServer(t)
			srv.config.SoftDeleteRetention = tc.retention
			agent := setupBrokerAgentInPhase(t, s, "seam-"+tc.name, state.PhaseRunning)

			old := agentDeletionFinalizeSeam
			t.Cleanup(func() { agentDeletionFinalizeSeam = old })
			var modes []store.DeletionFinalizeMode
			agentDeletionFinalizeSeam = func(_ context.Context, tx store.Store, a *store.Agent, mode store.DeletionFinalizeMode) error {
				modes = append(modes, mode)
				return errors.New("seam refused")
			}

			rec := doRequest(t, srv, http.MethodDelete, "/api/v1/agents/"+agent.ID, nil)
			require.Equal(t, http.StatusBadGateway, rec.Code, rec.Body.String())
			_, details := errorBody(t, rec)
			assert.Equal(t, store.DeletionCodeFinalizeFailed, details["deletionCode"])
			require.NotEmpty(t, modes)
			assert.Equal(t, tc.wantMode, modes[0])
			got := mustGetAgent(t, s, agent.ID)
			assert.True(t, got.DeletedAt.IsZero(), "rolled back: the row is live")
			assert.Equal(t, store.DeletionStateFinalizing, got.DeletionState)

			agentDeletionFinalizeSeam = old
			rec = doRequest(t, srv, http.MethodDelete, "/api/v1/agents/"+agent.ID, nil)
			require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
			assert.Equal(t, 1, disp.callCount(), "the finalizing re-claim does not re-dispatch")
			if tc.retention > 0 {
				assert.False(t, mustGetAgent(t, s, agent.ID).DeletedAt.IsZero())
			} else {
				assert.True(t, agentGone(t, s, agent.ID))
			}
		})
	}
}

// hookRecordingExecutor records lifecycle hook executions.
type hookRecordingExecutor struct {
	mu       sync.Mutex
	triggers []string
}

func (x *hookRecordingExecutor) Execute(_ context.Context, _ *store.LifecycleHook, _ *store.Agent, trigger string) error {
	x.mu.Lock()
	x.triggers = append(x.triggers, trigger)
	x.mu.Unlock()
	return nil
}

func (x *hookRecordingExecutor) fired(trigger string) int {
	x.mu.Lock()
	defer x.mu.Unlock()
	n := 0
	for _, tr := range x.triggers {
		if tr == trigger {
			n++
		}
	}
	return n
}

// Acceptance (e): no hub-emitted event fires a stopped or error hook on a
// soft or a failed delete, and lease renewals of an already-stopped agent
// fire no hook.
func TestAgentDeleteEngine_NoStoppedOrErrorHook(t *testing.T) {
	setDeleteKnob(t, &deleteLeaseRenewInterval, 20*time.Millisecond)
	for _, tc := range []struct {
		name  string
		phase state.Phase
		fail  bool
	}{
		{"running soft", state.PhaseRunning, false},
		{"running failed", state.PhaseRunning, true},
		{"stopped soft with renewals", state.PhaseStopped, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, s, _, disp := engineTestServer(t)
			srv.config.SoftDeleteRetention = time.Hour
			ctx := context.Background()
			for _, trigger := range []string{store.LifecycleHookTriggerStopped, store.LifecycleHookTriggerError} {
				require.NoError(t, s.CreateLifecycleHook(ctx, &store.LifecycleHook{
					ID: uuid.NewString(), Name: "hook-" + trigger, ScopeType: store.LifecycleHookScopeHub,
					Trigger: trigger, Enabled: true, ExecutionIdentity: uuid.NewString(),
					Action: &store.LifecycleHookAction{
						Type: store.LifecycleHookActionHTTP, Method: http.MethodPost,
						URL: "http://hooks.invalid/x", OnError: store.LifecycleHookOnErrorLog, TimeoutSeconds: 1,
					},
					Created: time.Now(), Updated: time.Now(),
				}))
			}
			exec := &hookRecordingExecutor{}
			bus := srv.events.(*deleteRecordingPublisher).EventPublisher
			ev := NewLifecycleHookEvaluator(s, bus, exec, slog.Default())
			ev.Start()
			defer ev.Stop()

			agent := setupBrokerAgentInPhase(t, s, "hooks-"+strings.ReplaceAll(tc.name, " ", "-"), tc.phase)
			// The agent's own earlier transition (as in production).
			srv.events.PublishAgentStatus(ctx, mustGetAgent(t, s, agent.ID))
			require.Eventually(t, func() bool {
				return tc.phase != state.PhaseStopped || exec.fired(store.LifecycleHookTriggerStopped) == 1
			}, 3*time.Second, 10*time.Millisecond)
			base := exec.fired(store.LifecycleHookTriggerStopped)

			if tc.fail {
				disp.setFn(func(context.Context, *store.Agent) error { return errors.New("broker boom") })
			} else {
				disp.setFn(func(context.Context, *store.Agent) error {
					time.Sleep(120 * time.Millisecond) // several renewals
					return nil
				})
			}
			rec := doRequest(t, srv, http.MethodDelete, "/api/v1/agents/"+agent.ID, nil)
			if tc.fail {
				require.Equal(t, http.StatusBadGateway, rec.Code, rec.Body.String())
			} else {
				require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
			}
			time.Sleep(200 * time.Millisecond) // let the evaluator drain
			assert.Equal(t, base, exec.fired(store.LifecycleHookTriggerStopped), "no stopped hook")
			assert.Zero(t, exec.fired(store.LifecycleHookTriggerError), "no error hook")
		})
	}
}

// Routed item: a failed marker from an earlier attempt does not survive a
// soft delete, so a restored agent carries no stale banner.
func TestAgentDeleteEngine_SoftFinishClearsFailedMarker(t *testing.T) {
	srv, s, _, disp := engineTestServer(t)
	srv.config.SoftDeleteRetention = time.Hour
	agent := setupBrokerAgentInPhase(t, s, "softclear", state.PhaseRunning)

	disp.setFn(func(context.Context, *store.Agent) error { return errors.New("broker boom") })
	rec := doRequest(t, srv, http.MethodDelete, "/api/v1/agents/"+agent.ID, nil)
	require.Equal(t, http.StatusBadGateway, rec.Code, rec.Body.String())
	require.Equal(t, store.DeletionCodeRuntimeError, mustGetAgent(t, s, agent.ID).DeletionCode)

	disp.setFn(nil)
	rec = doRequest(t, srv, http.MethodDelete, "/api/v1/agents/"+agent.ID, nil)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
	got := mustGetAgent(t, s, agent.ID)
	assert.False(t, got.DeletedAt.IsZero())
	assert.Equal(t, store.DeletionStateNone, got.DeletionState)
	assert.Empty(t, got.DeletionCode)
	assert.Empty(t, got.DeletionError)
	assert.Nil(t, store.ComputeAgentDeletion(got, time.Now()))

	rec = doRequest(t, srv, http.MethodPost, "/api/v1/projects/"+agent.ProjectID+"/agents/"+agent.ID+"/restore", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var body map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	if raw, ok := body["deletion"]; ok {
		assert.Equal(t, "null", string(raw), "no stale banner after restore")
	}
}
