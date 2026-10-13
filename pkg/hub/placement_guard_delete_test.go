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

// Tests of the saved-placement guard on the paths that would otherwise end
// an agent's accounting: the delete engine (pre-check and backstop, under
// force and best-effort too), env-gather recreate under cleanupMode=force,
// the queued-dispatch drain, and the ambiguous duplicate-run answers.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// placementRefusalFor is the placement refusal a Runtime Broker dispatch of
// a gets from another node's backstop, rebuilt from the dispatch row as a
// plain *RuntimeTargetRefusal (dispatchFailureError).
func placementRefusalFor(a *store.Agent) *RuntimeTargetRefusal {
	return stalePinRefusal(a, "test: refused by the executing node's backstop")
}

// --- delete ------------------------------------------------------------------

// A delete of an agent whose saved placement is inconsistent is refused
// before the claim, force or not: 409 with the placement code, nothing
// dispatched, the row and its accounting untouched.
func TestAgentDelete_PlacementPreCheckRefusesBeforeClaim(t *testing.T) {
	for _, query := range []string{"", "?force=true"} {
		t.Run("query="+query, func(t *testing.T) {
			f := newFlatHubFixture(t, flatHubOpts{experimentOn: true, linkFlat: true})
			a := f.stalePinnedAgent(t, "stale-delete", string(state.PhaseRunning))
			reserveBrokerSlot(t, f.s, f.legacy, a.ID)
			before := mustGetAgent(t, f.s, a.ID)

			rec := doRequest(t, f.srv, http.MethodDelete, "/api/v1/agents/"+a.ID+query, nil)
			requireAPIError(t, rec, http.StatusConflict, ErrCodeRuntimeTargetPinStale)

			assert.False(t, f.client.deleteCalled, "nothing is dispatched")
			after := mustGetAgent(t, f.s, a.ID)
			assert.True(t, after.DeletedAt.IsZero(), "the row is kept")
			assert.Equal(t, before.DeletionState, after.DeletionState, "no claim is taken")
			assert.Equal(t, before.DeletionClaim, after.DeletionClaim)
			assert.Equal(t, before.Phase, after.Phase)
			assert.Equal(t, before.RunIntent, after.RunIntent)
			assert.Equal(t, int64(1), brokerReservationCount(t, f.s, f.legacy.ID), "the reservation is kept")
		})
	}
}

// The dispatcher backstop's refusal (here as rebuilt from another node's
// dispatch row) is classified like a run mismatch: the delete fails as a
// conflict and the row is kept, under force and for a best-effort
// (created, no launch) dispatch too.
func TestAgentDelete_PlacementBackstopRefusesUnderForceAndBestEffort(t *testing.T) {
	for _, tc := range []struct {
		name  string
		phase state.Phase
		query string
	}{
		{"running, force", state.PhaseRunning, "?force=true"},
		{"running, plain", state.PhaseRunning, ""},
		{"created no launch (best-effort), plain", state.PhaseCreated, ""},
		{"created no launch (best-effort), force", state.PhaseCreated, "?force=true"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newRunMismatchFixture(t, "pg-del-"+uuid.NewString()[:8], tc.phase)
			refusal := placementRefusalFor(f.agent)
			f.client.answer = func(string) error { return refusal }

			r := f.del(t, tc.query)
			require.Equal(t, http.StatusConflict, r.rec.Code, r.rec.Body.String())
			code, details := errorBody(t, r.rec)
			assert.Equal(t, ErrCodeConflict, code)
			assert.Equal(t, store.DeletionCodeConflict, details["deletionCode"])
			assert.Contains(t, r.rec.Body.String(), "saved placement is inconsistent")

			got := mustGetAgent(t, f.store, f.agent.ID)
			assert.True(t, got.DeletedAt.IsZero(), "the row is not finalized")
			assert.Equal(t, store.DeletionStateFailed, got.DeletionState)
			assert.Equal(t, store.DeletionCodeConflict, got.DeletionCode)
			assert.Equal(t, string(tc.phase), got.Phase, "the prior phase is restored")
			assert.Equal(t, []string{"run-a"}, f.client.deleted(), "no previous-run delete after the refusal")
		})
	}
}

// seedAbandonedDeletion puts agent id in deletion state st with an expired
// lease and a running prior, as an engine that died would leave it.
func seedAbandonedDeletion(t *testing.T, s store.Store, id, st string) {
	t.Helper()
	seedAbandonedDeletionWithPrior(t, s, id, st, `{"phase":"running","activity":"working"}`)
}

// seedAbandonedDeletionWithPrior is seedAbandonedDeletion with the given
// prior. The dead engine had recorded the stop intent, as dispatch() does.
func seedAbandonedDeletionWithPrior(t *testing.T, s store.Store, id, st, prior string) {
	t.Helper()
	_, err := s.SetRunIntent(context.Background(), id, store.RunIntentStopped)
	require.NoError(t, err)
	past := time.Now().Add(-time.Minute)
	stopping := string(state.PhaseStopping)
	n, err := s.UpdateAgentDeletion(context.Background(), id, store.DeletionPredicate{}, store.DeletionFields{
		State: &st, BumpClaim: true, LeaseAt: &past, StartedAt: &past, Prior: &prior, Phase: &stopping,
	})
	require.NoError(t, err)
	require.Equal(t, 1, n)
}

// The pre-check's skip list. A finalizing row has already torn down: its
// re-claim finishes without a dispatch even when the pin is stale. A
// managed runtime reaches no Runtime Broker and is not refused.
func TestAgentDelete_PlacementPreCheckSkipList(t *testing.T) {
	t.Run("finalizing row with an expired lease finishes without a broker call", func(t *testing.T) {
		f := newFlatHubFixture(t, flatHubOpts{experimentOn: true, linkFlat: true})
		a := f.stalePinnedAgent(t, "stale-finalizing", string(state.PhaseRunning))
		seedAbandonedDeletion(t, f.s, a.ID, store.DeletionStateFinalizing)

		rec := doRequest(t, f.srv, http.MethodDelete, "/api/v1/agents/"+a.ID, nil)
		require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
		assert.False(t, f.client.deleteCalled, "a finalizing re-claim skips the dispatch")
		got, err := f.s.GetAgent(context.Background(), a.ID)
		assert.True(t, errors.Is(err, store.ErrNotFound) || (err == nil && !got.DeletedAt.IsZero()), "the row is finalized (err %v)", err)
	})

	t.Run("managed runtime is not refused", func(t *testing.T) {
		f := newFlatHubFixture(t, flatHubOpts{experimentOn: true, linkFlat: true})
		// Unpinned on the flat row: refused for any non-managed agent.
		a := f.unpinnedAgentOnWith(t, "managed-on-flat", f.flat.ID, string(state.PhaseRunning), func(a *store.Agent) {
			a.Runtime = ManagedRuntimePrefix + "test"
		})
		rec := doRequest(t, f.srv, http.MethodDelete, "/api/v1/agents/"+a.ID+"?force=true", nil)
		require.Less(t, rec.Code, 300, rec.Body.String())
		assert.False(t, f.client.deleteCalled, "a managed runtime never reaches a Runtime Broker")
	})
}

// An abandoned deleting row (expired lease) of a stale-pinned agent is not
// refused by the pre-check, where it would stay stuck in deleting and
// stopping: the re-claim's dispatch check refuses it and the rollback
// restores the prior phase.
func TestAgentDelete_AbandonedDeletingRowRollsBack(t *testing.T) {
	f := newFlatHubFixture(t, flatHubOpts{experimentOn: true, linkFlat: true})
	a := f.stalePinnedAgent(t, "stale-deleting", string(state.PhaseRunning))
	seedAbandonedDeletion(t, f.s, a.ID, store.DeletionStateDeleting)

	rec := doRequest(t, f.srv, http.MethodDelete, "/api/v1/agents/"+a.ID, nil)
	require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
	assert.False(t, f.client.deleteCalled, "nothing is dispatched")
	got := mustGetAgent(t, f.s, a.ID)
	assert.True(t, got.DeletedAt.IsZero(), "the row is kept")
	assert.Equal(t, store.DeletionStateFailed, got.DeletionState, "no longer stuck in deleting")
	assert.Equal(t, store.DeletionCodeConflict, got.DeletionCode)
	assert.Equal(t, string(state.PhaseRunning), got.Phase, "the prior phase is restored, not stopping")
	assert.Equal(t, store.RunIntentStopped, got.RunIntent, "the stop intent the abandoned engine recorded stays (the deletion prior keeps no intent)")
}

// The engine's check of the claimed snapshot is the only placement guard
// for an abandoned deleting row (the pre-check skips it) on the two paths
// that finalize without a dispatch: a best-effort row whose Runtime Broker
// is offline, and a pinned agent with no Runtime Broker. Under force, both
// are refused as a conflict, the row and reservation kept, nothing sent.
func TestAgentDelete_SnapshotCheckGuardsDispatchShortcuts(t *testing.T) {
	t.Run("best-effort row, Runtime Broker offline", func(t *testing.T) {
		ctx := context.Background()
		f := newFlatHubFixture(t, flatHubOpts{experimentOn: true, linkFlat: true})
		a := f.stalePinnedAgent(t, "stale-best-effort", string(state.PhaseCreated))
		reserveBrokerSlot(t, f.s, f.legacy, a.ID)
		seedAbandonedDeletionWithPrior(t, f.s, a.ID, store.DeletionStateDeleting, `{"phase":"created"}`)
		require.NoError(t, f.s.UpdateRuntimeBrokerHeartbeat(ctx, f.legacy.ID, store.BrokerStatusOffline))

		rec := doRequest(t, f.srv, http.MethodDelete, "/api/v1/agents/"+a.ID+"?force=true", nil)
		require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
		got := mustGetAgent(t, f.s, a.ID)
		assert.True(t, got.DeletedAt.IsZero(), "the row is kept")
		assert.Equal(t, store.DeletionStateFailed, got.DeletionState)
		assert.Equal(t, store.DeletionCodeConflict, got.DeletionCode)
		assert.False(t, f.client.deleteCalled, "nothing is sent")
		assert.Equal(t, int64(1), brokerReservationCount(t, f.s, f.legacy.ID), "the reservation is kept")
	})

	t.Run("pinned agent with no Runtime Broker", func(t *testing.T) {
		ctx := context.Background()
		f := newFlatHubFixture(t, flatHubOpts{experimentOn: true, linkFlat: true})
		a := f.pinnedAgent(t, "pinned-no-broker", string(state.PhaseRunning))
		reserveBrokerSlot(t, f.s, f.flat, a.ID)
		a.RuntimeBrokerID = ""
		require.NoError(t, f.s.UpdateAgent(ctx, a))
		require.True(t, mustGetAgent(t, f.s, a.ID).IsPinned())
		seedAbandonedDeletion(t, f.s, a.ID, store.DeletionStateDeleting)

		rec := doRequest(t, f.srv, http.MethodDelete, "/api/v1/agents/"+a.ID+"?force=true", nil)
		require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
		got := mustGetAgent(t, f.s, a.ID)
		assert.True(t, got.DeletedAt.IsZero(), "the row is kept")
		assert.Equal(t, store.DeletionStateFailed, got.DeletionState)
		assert.Equal(t, store.DeletionCodeConflict, got.DeletionCode)
		assert.False(t, f.client.deleteCalled, "nothing is sent")
		assert.Equal(t, int64(1), brokerReservationCount(t, f.s, f.flat.ID), "the reservation is kept")
	})
}

// Fixture purge reports an agent refused by the placement pre-check as
// failed/placement_conflict, and leaves its row and claim untouched.
func TestFixturePurge_PlacementRefusalLabel(t *testing.T) {
	ctx := context.Background()
	f := newFlatHubFixture(t, flatHubOpts{experimentOn: true, linkFlat: true})
	stamp := store.FixtureProvenance{RootID: uuid.NewString(), IssuerID: uuid.NewString()}
	a := f.stalePinnedAgentWith(t, "stale-fixture", string(state.PhaseRunning), func(a *store.Agent) {
		a.Fixture = stamp
	})
	require.Equal(t, stamp.RootID, a.Fixture.RootID)
	before := mustGetAgent(t, f.s, a.ID)

	var retained []TestIdentityPurgeItem
	deleted, item := f.srv.purgeFixtureAgent(ctx, a, "fixture-purge-test", &retained)
	assert.False(t, deleted)
	require.NotNil(t, item)
	assert.Equal(t, "failed", item.Status)
	assert.Equal(t, "placement_conflict", item.Reason)
	after := mustGetAgent(t, f.s, a.ID)
	assert.Equal(t, before.DeletionState, after.DeletionState, "no claim is taken")
	assert.Equal(t, before.DeletionClaim, after.DeletionClaim)
	assert.False(t, f.client.deleteCalled)
}

// --- env-gather recreate ---------------------------------------------------------

// Env-gather recreate of a stale-pinned provisioning agent is refused
// before the old agent's stop intent: 409 with the placement code, nothing
// dispatched, the row kept.
func TestEnvGatherRecreate_PlacementPreCheckBeforeIntent(t *testing.T) {
	f := newFlatHubFixture(t, flatHubOpts{experimentOn: true, linkFlat: true})
	a := f.stalePinnedAgent(t, "stale-gather", string(state.PhaseProvisioning))
	before := mustGetAgent(t, f.s, a.ID)

	body := map[string]interface{}{"name": a.Name, "projectId": f.project.ID, "template": "claude", "gatherEnv": true, "cleanupMode": "force"}
	rec := doRequest(t, f.srv, http.MethodPost, "/api/v1/agents", body)
	require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
	code, _ := errorBody(t, rec)
	assert.Equal(t, ErrCodeRuntimeTargetPinStale, code)
	assert.False(t, f.client.deleteCalled, "nothing is dispatched")
	after := mustGetAgent(t, f.s, a.ID)
	assert.Equal(t, before.RunIntent, after.RunIntent, "no stop intent is recorded")
	assert.Equal(t, before.Phase, after.Phase)
}

// envGatherPlacementClient answers every delete with a placement refusal
// once refuse is set.
type envGatherPlacementClient struct {
	*envGatherMockBrokerClient
	refuse  bool
	refusal error
}

func (c *envGatherPlacementClient) DeleteAgent(_ context.Context, _, _, _, _ string, _ DeleteAgentOptions) error {
	if !c.refuse {
		return nil
	}
	return c.refusal
}

// Env-gather recreate does not let cleanupMode=force override a placement
// refusal of its delete: the provisioning row is kept and the request gets
// the placement 409.
func TestEnvGatherRecreate_PlacementRefusalNotOverriddenByForce(t *testing.T) {
	for _, cleanupMode := range []string{"", "force"} {
		t.Run("cleanupMode="+cleanupMode, func(t *testing.T) {
			srv, st := testServer(t)
			ctx := context.Background()
			suffix := uuid.NewString()[:8]
			project := &store.Project{ID: tid("pg-eg-project-" + suffix), Name: "pg-eg-" + suffix, Slug: "pg-eg-" + suffix}
			require.NoError(t, st.CreateProject(ctx, project))
			broker := &store.RuntimeBroker{ID: tid("pg-eg-broker-" + suffix), Name: "pg-eg-broker-" + suffix, Slug: "pg-eg-broker-" + suffix,
				Endpoint: "http://localhost:9800", Status: store.BrokerStatusOnline}
			require.NoError(t, st.CreateRuntimeBroker(ctx, broker))
			require.NoError(t, st.AddProjectProvider(ctx, &store.ProjectProvider{
				ProjectID: project.ID, BrokerID: broker.ID, BrokerName: "test-broker", LocalPath: "/tmp/test-project",
			}))
			client := &envGatherPlacementClient{envGatherMockBrokerClient: &envGatherMockBrokerClient{
				gatherReturnEnvReqs: &RemoteEnvRequirementsResponse{AgentID: "x", Required: []string{"GEMINI_API_KEY"}, Needs: []string{"GEMINI_API_KEY"}},
			}}
			d := NewHTTPAgentDispatcherWithClient(st, client, true, slog.Default())
			d.SetTokenGenerator(&fakeMintingTokenGenerator{store: st})
			srv.SetDispatcher(d)

			body := map[string]interface{}{"name": "pg-eg-agent", "projectId": project.ID, "template": "claude", "gatherEnv": true}
			rec1 := doRequest(t, srv, http.MethodPost, "/api/v1/agents", body)
			require.Equal(t, http.StatusAccepted, rec1.Code, rec1.Body.String())
			var resp1 CreateAgentResponse
			require.NoError(t, json.Unmarshal(rec1.Body.Bytes(), &resp1))
			oldID := resp1.Agent.ID
			old := mustGetAgent(t, st, oldID)

			client.refusal = placementRefusalFor(old)
			client.refuse = true
			if cleanupMode != "" {
				body["cleanupMode"] = cleanupMode
			}
			rec2 := doRequest(t, srv, http.MethodPost, "/api/v1/agents", body)
			require.Equal(t, http.StatusConflict, rec2.Code, rec2.Body.String())
			code, _ := errorBody(t, rec2)
			assert.Equal(t, ErrCodeRuntimeTargetPinStale, code)
			_, err := st.GetAgent(ctx, oldID)
			require.NoError(t, err, "the provisioning row is kept")
		})
	}
}

// --- drain -------------------------------------------------------------------

// A stop queued while the agent's Runtime Broker was offline (the real
// queueOfflineStop row: intent, run and superseded claim in its args), whose
// pinned agent's placement became inconsistent before the drain, is settled
// failed with placement_conflict and not dispatched. The queued stop's claim
// is never taken, and the agent keeps stop_queued, the offline-stop message
// and its reservation (design §3.3).
func TestDrain_PlacementConflictKeepsStopQueuedAndReservation(t *testing.T) {
	ctx := context.Background()
	f := newFlatHubFixture(t, flatHubOpts{experimentOn: true, linkFlat: true,
		storeWrap: func(inner store.Store, _ *storeFaultSwitch) store.Store {
			return &stopClaimCountingStore{Store: inner}
		}})
	stopClaims := f.storeWrapper.(*stopClaimCountingStore)
	a := f.pinnedAgent(t, "queued-stop", string(state.PhaseRunning))
	_, err := f.s.SetAgentRunID(ctx, a.ID, "run-q", nil)
	require.NoError(t, err)
	reserveBrokerSlot(t, f.s, f.flat, a.ID)

	// Stop with the Runtime Broker offline: the stop is queued.
	require.NoError(t, f.s.UpdateRuntimeBrokerHeartbeat(ctx, f.flat.ID, store.BrokerStatusOffline))
	rec := doRequest(t, f.srv, http.MethodPost, "/api/v1/agents/"+a.ID+"/stop", nil)
	require.Equal(t, http.StatusAccepted, rec.Code, "body: %s", rec.Body.String())
	rows, err := f.s.ListPendingDispatch(ctx, f.flat.ID)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	row := rows[0]
	require.Equal(t, "stop", row.Op)
	args, err := UnmarshalStopArgs(row.Args)
	require.NoError(t, err)
	require.NotNil(t, args.IntentAt, "a real queued stop carries its intent")
	queued := mustGetAgent(t, f.s, a.ID)
	require.Equal(t, containerStatusStopQueued, queued.ContainerStatus)
	require.Equal(t, offlineStopMessage, queued.Message)
	require.Equal(t, string(state.PhaseStopped), queued.Phase)

	// The placement becomes inconsistent before the drain (as an older
	// binary moving runtime_broker_id would do).
	queued.RuntimeBrokerID = f.legacy.ID
	require.NoError(t, f.s.UpdateAgent(ctx, queued))
	before := mustGetAgent(t, f.s, a.ID)
	require.False(t, before.PinValid())
	claimsBefore := stopClaims.calls.Load()

	f.srv.ReconcileBroker(ctx, f.flat.ID)

	failed, err := f.s.GetBrokerDispatch(ctx, row.ID)
	require.NoError(t, err)
	require.Equal(t, store.DispatchStateFailed, failed.State)
	assert.True(t, strings.HasPrefix(failed.Error, drainPlacementConflictCode+": "), "error: %q", failed.Error)
	var refusal *RuntimeTargetRefusal
	require.True(t, errors.As(dispatchFailureError(failed), &refusal), "the originator rebuilds the refusal")
	assert.Equal(t, ErrCodeRuntimeTargetPinStale, refusal.Code)
	assert.False(t, f.client.stopCalled, "nothing is dispatched")

	got := mustGetAgent(t, f.s, a.ID)
	assert.Equal(t, containerStatusStopQueued, got.ContainerStatus, "stop_queued is kept")
	assert.Equal(t, offlineStopMessage, got.Message, "the offline-stop message is kept")
	assert.Equal(t, before.Phase, got.Phase)
	assert.Equal(t, claimsBefore, stopClaims.calls.Load(), "the queued stop's claim is never taken (no ClaimAgentStop call)")
	assert.Equal(t, before.StartClaimID, got.StartClaimID, "no claim is left held")
	assert.Equal(t, before.RunIntent, got.RunIntent)
	assert.Equal(t, int64(1), brokerReservationCount(t, f.s, f.flat.ID), "the reservation is kept")
}

// An unpinned agent keeps today's drain: a row queued for another Runtime
// Broker than the agent's own still executes against the agent's current
// one (the placement guard applies to pinned agents only).
func TestDrain_UnpinnedAgentRowOfAnotherRuntimeBrokerExecutes(t *testing.T) {
	ctx := context.Background()
	f := newFlatHubFixture(t, flatHubOpts{experimentOn: true, linkFlat: true})
	a := f.unpinnedAgentOn(t, "unpinned-drain", f.legacy.ID, string(state.PhaseRunning))
	other := &store.RuntimeBroker{ID: tid("drain-other-legacy"), Name: "drain-other-legacy", Slug: "drain-other-legacy",
		Status: store.BrokerStatusOnline, Endpoint: "http://drain-other.invalid"}
	require.NoError(t, f.s.CreateRuntimeBroker(ctx, other))
	row := &store.BrokerDispatch{ID: tid("drain-unpinned-" + t.Name()), BrokerID: other.ID, AgentID: a.ID, Op: "stop"}
	require.NoError(t, f.s.InsertBrokerDispatch(ctx, row))

	f.srv.ReconcileBroker(ctx, other.ID)

	done, err := f.s.GetBrokerDispatch(ctx, row.ID)
	require.NoError(t, err)
	assert.Equal(t, store.DispatchStateDone, done.State, "error: %q", done.Error)
	assert.False(t, strings.HasPrefix(done.Error, drainPlacementConflictCode), "error: %q", done.Error)
	assert.True(t, f.client.stopCalled, "the stop is dispatched as today")
	assert.Equal(t, f.legacy.ID, f.client.lastBrokerID, "against the agent's current Runtime Broker")
}

// stopClaimCountingStore counts ClaimAgentStop calls, the claim a queued
// stop is applied under (claimQueuedStop).
type stopClaimCountingStore struct {
	store.Store
	calls atomic.Int64
}

func (s *stopClaimCountingStore) ClaimAgentStop(ctx context.Context, agentID, owner string, intentAt time.Time, ttl time.Duration) (store.StartClaim, error) {
	s.calls.Add(1)
	return s.Store.ClaimAgentStop(ctx, agentID, owner, intentAt, ttl)
}

// A row queued for another Runtime Broker than the pinned agent's own is
// refused even when the pin itself is consistent.
func TestDrain_PlacementConflictForRowOfAnotherRuntimeBroker(t *testing.T) {
	ctx := context.Background()
	f := newFlatHubFixture(t, flatHubOpts{experimentOn: true, linkFlat: true})
	a := f.pinnedAgent(t, "pinned-drain", string(state.PhaseRunning))
	row := &store.BrokerDispatch{ID: tid("drain-other-" + t.Name()), BrokerID: f.legacy.ID, AgentID: a.ID, Op: "stop"}
	require.NoError(t, f.s.InsertBrokerDispatch(ctx, row))

	f.srv.ReconcileBroker(ctx, f.legacy.ID)

	failed, err := f.s.GetBrokerDispatch(ctx, row.ID)
	require.NoError(t, err)
	require.Equal(t, store.DispatchStateFailed, failed.State)
	assert.True(t, strings.HasPrefix(failed.Error, drainPlacementConflictCode+": "), "error: %q", failed.Error)
	assert.False(t, f.client.stopCalled)
	assert.Equal(t, a.ContainerStatus, mustGetAgent(t, f.s, a.ID).ContainerStatus)
}

// A claimless delete intent whose agent row is gone is still sent to a
// flat Runtime Broker: the target takes the pin of the Runtime Broker the
// intent names, so the dispatcher's backstop does not read it as an
// unpinned agent on a flat row.
func TestDrain_RowlessDeleteTargetTakesFlatPin(t *testing.T) {
	f := newFlatHubFixture(t, flatHubOpts{experimentOn: true, linkFlat: true})
	target := &store.Agent{ID: "gone", Slug: "gone", RuntimeBrokerID: f.flat.ID}
	f.srv.pinRowlessDeleteTarget(context.Background(), target)
	assert.True(t, target.PinValid())
	assert.Equal(t, f.flat.RuntimeTarget.ID, target.PinnedRuntimeTargetID)
	_, err := requirePinnedPlacement(context.Background(), f.s, target)
	assert.NoError(t, err)

	legacy := &store.Agent{ID: "gone-legacy", Slug: "gone-legacy", RuntimeBrokerID: f.legacy.ID}
	f.srv.pinRowlessDeleteTarget(context.Background(), legacy)
	assert.False(t, legacy.IsPinned(), "a legacy row gives no pin")
}

// --- run_in_progress / run_already_started -----------------------------------

// ambiguousStartOutcome is what a start that failed with a given broker
// answer leaves behind.
type ambiguousStartOutcome struct {
	revoked      bool
	runReverted  bool
	claimState   store.StartClaimState
	phase        string
	runIntent    store.RunIntent
	reservations int64
}

// startOutcomeFor runs the same harness for brokerErr: a dispatcher start
// (credential revoke, run revert) and a claimed start through
// startAgentCore (claim state, phase, intent, capacity).
func startOutcomeFor(t *testing.T, name string, brokerErr error) ambiguousStartOutcome {
	t.Helper()
	ctx := context.Background()
	var out ambiguousStartOutcome

	// Dispatcher leg.
	memStore := createTestStore(t)
	broker := newTestBrokerForRevoke(t, memStore, "amb-"+name)
	project := &store.Project{ID: tid("amb-project-" + name), Name: "amb-" + name, Slug: "amb-" + name}
	require.NoError(t, memStore.CreateProject(ctx, project))
	agent := &store.Agent{ID: tid("amb-agent-" + name), Name: "amb", Slug: "amb", ProjectID: project.ID,
		RuntimeBrokerID: broker.ID, Phase: string(state.PhaseStopped)}
	require.NoError(t, memStore.CreateAgent(ctx, agent))
	_, err := memStore.SetAgentRunID(ctx, agent.ID, "run-prev", nil)
	require.NoError(t, err)
	agent = mustGetAgent(t, memStore, agent.ID)

	client := &mockRuntimeBrokerClient{returnErr: brokerErr}
	d := NewHTTPAgentDispatcherWithClient(memStore, client, false, slog.Default())
	gen := &fakeMintingTokenGenerator{store: memStore}
	d.SetTokenGenerator(gen)
	require.Error(t, d.DispatchAgentStart(ctx, agent, "", false))
	require.Len(t, gen.jtis, 1)
	out.revoked = getTestAgentCredential(t, memStore, gen.lastJTI()).RevokedAt != nil
	out.runReverted = mustGetAgent(t, memStore, agent.ID).RunID == "run-prev"

	// Claimed start leg.
	f, cd, a := newClaimFixture(t)
	cd.start = func(context.Context, *store.Agent) error { return brokerErr }
	require.Error(t, f.srv.startAgentCore(ctx, a, StartOpts{Kind: store.StartClaimCreate}))
	got := getAgent(t, f.s, a.ID)
	out.claimState = got.StartClaimState
	out.phase = got.Phase
	out.runIntent = got.RunIntent
	out.reservations = brokerReservationCount(t, f.s, f.brokerID)
	return out
}

func TestDispatch_RunInProgressIsAmbiguous(t *testing.T) {
	timeout := startOutcomeFor(t, "timeout", fmt.Errorf("failed to send request: %w", context.DeadlineExceeded))
	require.False(t, timeout.revoked, "a timeout does not revoke")
	require.False(t, timeout.runReverted, "a timeout does not revert the run")
	require.Equal(t, store.StartClaimUnconfirmed, timeout.claimState, "a timeout keeps the claim unconfirmed")

	// The harness tells a confirmed rejection apart.
	rejected := startOutcomeFor(t, "rejected", brokerEnvelope(t, http.StatusConflict, "conflict", nil))
	require.True(t, rejected.revoked)
	require.True(t, rejected.runReverted)
	require.NotEqual(t, timeout, rejected)

	for _, code := range []string{brokerErrorCodeRunInProgress, brokerErrorCodeRunAlreadyStarted} {
		t.Run(code, func(t *testing.T) {
			e := brokerEnvelope(t, http.StatusConflict, code, nil)
			assert.False(t, isConfirmedBrokerRejection(e))
			assert.False(t, isConfirmedStartNotActedOnError(e))
			assert.Equal(t, startUnconfirmed, startOutcomeOf(e))
			// The same answer with a 500 status is not confirmed either.
			assert.False(t, isConfirmedBrokerRejection(brokerEnvelope(t, http.StatusInternalServerError, code, nil)))

			got := startOutcomeFor(t, code, e)
			assert.False(t, got.revoked, "no credential revoke")
			assert.False(t, got.runReverted, "no run revert")
			assert.Equal(t, store.StartClaimUnconfirmed, got.claimState, "the claim is kept unconfirmed")
			assert.Equal(t, timeout, got, "the end state equals the response-timeout case")
		})
	}
}
