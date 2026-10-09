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
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Tests for the phase 1a-1 delete guards and start gate (design
// ptone/scion#2483 §2.1, §2.4). Delete markers are seeded through
// UpdateAgentDeletion, the only writer of the deletion columns.

// deleteSeed describes a delete marker to seed on an agent.
type deleteSeed struct {
	name    string
	state   string
	leaseIn time.Duration // lease_at = now + leaseIn
	code    string
	intent  bool // also insert an outstanding (pending) broker delete intent
}

var (
	seedLiveDeleting     = deleteSeed{name: "live deleting", state: store.DeletionStateDeleting, leaseIn: time.Minute}
	seedFailedIntent     = deleteSeed{name: "failed with outstanding intent", state: store.DeletionStateFailed, leaseIn: -time.Minute, code: store.DeletionCodeRuntimeError, intent: true}
	seedInDoubtIntent    = deleteSeed{name: "in_doubt with outstanding intent", state: store.DeletionStateFailed, leaseIn: -time.Minute, code: store.DeletionCodeInDoubt, intent: true}
	seedExpiredFinalize  = deleteSeed{name: "lease-expired finalizing", state: store.DeletionStateFinalizing, leaseIn: -time.Minute}
	seedRevokeFailedFinl = deleteSeed{name: "revoke_failed finalizing", state: store.DeletionStateFinalizing, leaseIn: -time.Minute, code: store.DeletionCodeRevokeFailed}

	// Every marker that must block start.
	blockingSeeds = []deleteSeed{seedLiveDeleting, seedFailedIntent, seedInDoubtIntent, seedExpiredFinalize, seedRevokeFailedFinl}
)

func seedAgentDeletion(t *testing.T, s store.Store, agentID string, d deleteSeed) {
	t.Helper()
	ctx := context.Background()
	now := time.Now()
	lease := now.Add(d.leaseIn)
	st := d.state
	set := store.DeletionFields{State: &st, BumpClaim: true, LeaseAt: &lease, StartedAt: &now}
	if d.code != "" {
		code := d.code
		set.Code = &code
	}
	if d.state == store.DeletionStateFailed {
		set.FailedAt = &now
	}
	n, err := s.UpdateAgentDeletion(ctx, agentID, store.DeletionPredicate{}, set)
	require.NoError(t, err)
	require.Equal(t, 1, n)
	if d.intent {
		require.NoError(t, s.InsertBrokerDispatch(ctx, &store.BrokerDispatch{
			ID: uuid.NewString(), BrokerID: uuid.NewString(), AgentID: agentID, Op: brokerDispatchOpDelete,
		}))
	}
}

func requireDeleteInProgress(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), ErrCodeDeleteInProgress)
}

// deleteGuardDispatcher counts start and stop dispatches.
type deleteGuardDispatcher struct {
	createAgentDispatcher
	starts int
	stops  int
}

func (d *deleteGuardDispatcher) DispatchAgentStart(_ context.Context, agent *store.Agent, _ string, _ bool) error {
	d.starts++
	agent.Phase = string(state.PhaseRunning)
	return nil
}

func (d *deleteGuardDispatcher) DispatchAgentStop(_ context.Context, _ *store.Agent) error {
	d.stops++
	return nil
}

// Acceptance (n), (u), (x): start and restart on every blocking marker →
// 409 delete_in_progress, with nothing dispatched.
func TestDeleteGate_StartRestart_Blocked(t *testing.T) {
	for i, seed := range blockingSeeds {
		for _, action := range []string{api.AgentActionStart, api.AgentActionRestart} {
			t.Run(seed.name+"/"+action, func(t *testing.T) {
				srv, s := testServer(t)
				disp := &deleteGuardDispatcher{}
				srv.SetDispatcher(disp)
				agent := setupBrokerAgentInPhase(t, s, "dg-"+action+"-"+string(rune('a'+i)), state.PhaseStopped)
				seedAgentDeletion(t, s, agent.ID, seed)

				rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+agent.ID+"/"+action, nil)
				requireDeleteInProgress(t, rec)
				assert.Zero(t, disp.starts, "no start dispatch")
				assert.Zero(t, disp.stops, "no stop dispatch")

				got, err := s.GetAgent(context.Background(), agent.ID)
				require.NoError(t, err)
				assert.Equal(t, string(state.PhaseStopped), got.Phase)
				assert.Equal(t, seed.state, got.DeletionState, "the marker is untouched")
			})
		}
	}
}

// Acceptance (n), (u): stop during a delete (live, or finalizing even after
// its lease expired) is a 200 no-op.
func TestDeleteGate_Stop_Noop(t *testing.T) {
	for i, seed := range []deleteSeed{seedLiveDeleting, seedExpiredFinalize, seedRevokeFailedFinl} {
		t.Run(seed.name, func(t *testing.T) {
			srv, s := testServer(t)
			disp := &deleteGuardDispatcher{}
			srv.SetDispatcher(disp)
			agent := setupBrokerAgentInPhase(t, s, "dg-stop-"+string(rune('a'+i)), state.PhaseRunning)
			seedAgentDeletion(t, s, agent.ID, seed)

			rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+agent.ID+"/stop", nil)
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			assert.Zero(t, disp.stops, "stop is a no-op: nothing dispatched")

			var body map[string]interface{}
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
			del, ok := body["deletion"].(map[string]interface{})
			require.True(t, ok, "response carries the deletion view: %s", rec.Body.String())
			if seed.state == store.DeletionStateFinalizing && seed.leaseIn < 0 {
				assert.Equal(t, store.DeletionStateFailed, del["state"])
				_, hasExpiry := del["expiresAt"]
				assert.False(t, hasExpiry, "a finalizing row never expires from view")
			}

			got, err := s.GetAgent(context.Background(), agent.ID)
			require.NoError(t, err)
			assert.Equal(t, string(state.PhaseRunning), got.Phase, "phase unchanged")
		})
	}
}

// A stop on a failed row without an outstanding intent is a real stop.
func TestDeleteGate_Stop_FailedRowStillStops(t *testing.T) {
	srv, s := testServer(t)
	disp := &deleteGuardDispatcher{}
	srv.SetDispatcher(disp)
	agent := setupBrokerAgentInPhase(t, s, "dg-stop-failed", state.PhaseRunning)
	seedAgentDeletion(t, s, agent.ID, deleteSeed{state: store.DeletionStateFailed, leaseIn: -time.Minute, code: store.DeletionCodeRuntimeError})

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+agent.ID+"/stop", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, 1, disp.stops)
	got, err := s.GetAgent(context.Background(), agent.ID)
	require.NoError(t, err)
	assert.Equal(t, string(state.PhaseStopped), got.Phase)
	assert.Equal(t, store.DeletionStateNone, got.DeletionState, "a successful stop clears the failed marker")
}

// A successful start/restart clears a failed marker (state=failed, or an
// abandoned deleting row), and the response and event carry deletion:null.
func TestDeleteGate_SuccessfulStartClearsFailedMarker(t *testing.T) {
	seeds := []deleteSeed{
		{name: "failed", state: store.DeletionStateFailed, leaseIn: -time.Minute, code: store.DeletionCodeConflict},
		{name: "abandoned deleting", state: store.DeletionStateDeleting, leaseIn: -time.Minute},
	}
	for i, seed := range seeds {
		for _, action := range []string{api.AgentActionStart, api.AgentActionRestart} {
			t.Run(seed.name+"/"+action, func(t *testing.T) {
				srv, s := testServer(t)
				disp := &deleteGuardDispatcher{}
				srv.SetDispatcher(disp)
				pub := &trackingEventPublisher{}
				srv.SetEventPublisher(pub)
				agent := setupBrokerAgentInPhase(t, s, "dg-clear-"+action+"-"+string(rune('a'+i)), state.PhaseStopped)
				seedAgentDeletion(t, s, agent.ID, seed)
				errMsg, prior, request := "broker busy", `{"phase":"stopped"}`, `{"soft":true}`
				n, err := s.UpdateAgentDeletion(context.Background(), agent.ID, store.DeletionPredicate{},
					store.DeletionFields{Error: &errMsg, Prior: &prior, Request: &request})
				require.NoError(t, err)
				require.Equal(t, 1, n)

				rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+agent.ID+"/"+action, nil)
				require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
				assert.Equal(t, 1, disp.starts)

				var body map[string]interface{}
				require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
				v, ok := body["deletion"]
				assert.True(t, ok && v == nil, "deletion must be an explicit null after the clear: %s", rec.Body.String())

				got, err := s.GetAgent(context.Background(), agent.ID)
				require.NoError(t, err)
				assert.Equal(t, store.DeletionStateNone, got.DeletionState)
				assert.Empty(t, got.DeletionCode)
				assert.Empty(t, got.DeletionError)
				assert.Empty(t, got.DeletionPrior)
				assert.Empty(t, got.DeletionRequest)
				assert.Nil(t, got.DeletionLeaseAt)
				assert.Nil(t, got.DeletionStartedAt)
				assert.Nil(t, got.DeletionFailedAt)
				assert.Equal(t, int64(1), got.DeletionClaim, "the claim epoch is kept")

				published := pub.publishedAgents()
				require.NotEmpty(t, published)
				assert.Nil(t, store.ComputeAgentDeletion(published[len(published)-1], time.Now()))
			})
		}
	}
}

// A failed row whose delete intent is still outstanding stays blocked and
// keeps its marker; clearFailedDeletion never clears it.
func TestClearFailedDeletion_KeepsMarkerWithOutstandingIntentOrFinalizing(t *testing.T) {
	for i, seed := range []deleteSeed{seedFailedIntent, seedExpiredFinalize, seedLiveDeleting} {
		t.Run(seed.name, func(t *testing.T) {
			srv, s := testServer(t)
			agent := setupBrokerAgentInPhase(t, s, "dg-keep-"+string(rune('a'+i)), state.PhaseStopped)
			seedAgentDeletion(t, s, agent.ID, seed)
			a, err := s.GetAgent(context.Background(), agent.ID)
			require.NoError(t, err)
			srv.clearFailedDeletion(context.Background(), a)
			got, err := s.GetAgent(context.Background(), agent.ID)
			require.NoError(t, err)
			assert.Equal(t, seed.state, got.DeletionState)
			assert.Equal(t, seed.state, a.DeletionState, "in-memory agent untouched")
		})
	}
}

// Acceptance (w): authz runs before the gate — a caller who may not manage
// the agent gets 403, not 409.
func TestDeleteGate_UnauthorizedGets403(t *testing.T) {
	f := handleExistingAgentAuthzSetup(t)
	agent := f.agent(t, "dg-authz-agent", string(state.PhaseStopped))
	seedAgentDeletion(t, f.store, agent.ID, seedLiveDeleting)

	for _, action := range []string{api.AgentActionStart, api.AgentActionRestart, api.AgentActionStop} {
		rec := doRequestAsUser(t, f.srv, f.member, http.MethodPost, "/api/v1/agents/"+agent.ID+"/"+action, nil)
		assert.Equal(t, http.StatusForbidden, rec.Code, "%s: %s", action, rec.Body.String())
		assert.NotContains(t, rec.Body.String(), ErrCodeDeleteInProgress)
	}

	// The owner, who is authorized, gets the 409.
	rec := doRequestAsUser(t, f.srv, f.owner, http.MethodPost, "/api/v1/agents/"+agent.ID+"/start", nil)
	requireDeleteInProgress(t, rec)
}

// Acceptance (x): POST /agents {name, resume:true} on an existing agent
// with a delete in progress → 409, for both a live and a failed/in_doubt
// row with an outstanding intent.
func TestDeleteGate_CreateExistingResume(t *testing.T) {
	for i, seed := range []deleteSeed{seedLiveDeleting, seedFailedIntent, seedInDoubtIntent} {
		t.Run(seed.name, func(t *testing.T) {
			f := handleExistingAgentAuthzSetup(t)
			disp := &deleteGuardDispatcher{}
			f.srv.SetDispatcher(disp)
			name := "dg-resume-" + string(rune('a'+i))
			agent := f.agent(t, name, string(state.PhaseStopped))
			seedAgentDeletion(t, f.store, agent.ID, seed)

			rec := doRequestAsUser(t, f.srv, f.owner, http.MethodPost, "/api/v1/agents", map[string]interface{}{
				"name": agent.Slug, "projectId": f.project.ID, "resume": true,
			})
			requireDeleteInProgress(t, rec)
			assert.Zero(t, disp.starts)
			assert.False(t, disp.deleteCalled, "the stale-agent cleanup branch must not run")

			got, err := f.store.GetAgent(context.Background(), agent.ID)
			require.NoError(t, err)
			assert.Equal(t, string(state.PhaseStopped), got.Phase)
		})
	}

	// A non-owner still gets the existing name-conflict answer, not 409
	// delete_in_progress: the gate runs after the lifecycle authz.
	t.Run("non-owner sees plain conflict", func(t *testing.T) {
		f := handleExistingAgentAuthzSetup(t)
		agent := f.agent(t, "dg-resume-member", string(state.PhaseStopped))
		seedAgentDeletion(t, f.store, agent.ID, seedLiveDeleting)
		rec := doRequestAsUser(t, f.srv, f.member, http.MethodPost, "/api/v1/agents", map[string]interface{}{
			"name": agent.Slug, "projectId": f.project.ID, "resume": true,
		})
		assert.Equal(t, http.StatusConflict, rec.Code)
		assert.NotContains(t, rec.Body.String(), ErrCodeDeleteInProgress)
	})
}

// Acceptance (x): managed-runtime start and restart are gated before the
// managed branch.
func TestDeleteGate_ManagedStartRestart(t *testing.T) {
	for i, seed := range []deleteSeed{seedLiveDeleting, seedInDoubtIntent} {
		for _, action := range []string{api.AgentActionStart, api.AgentActionRestart} {
			t.Run(seed.name+"/"+action, func(t *testing.T) {
				srv, s := testServer(t)
				agent := setupBrokerAgentInPhase(t, s, "dg-managed-"+action+"-"+string(rune('a'+i)), state.PhaseStopped)
				agent.Runtime = ManagedRuntimePrefix + "test"
				require.NoError(t, s.UpdateAgent(context.Background(), agent))
				seedAgentDeletion(t, s, agent.ID, seed)

				rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+agent.ID+"/"+action, nil)
				requireDeleteInProgress(t, rec)
				got, err := s.GetAgent(context.Background(), agent.ID)
				require.NoError(t, err)
				assert.Equal(t, string(state.PhaseStopped), got.Phase)
			})
		}
	}
}

// Acceptance (x): reincarnate is gated after its authz.
func TestDeleteGate_Reincarnate(t *testing.T) {
	for i, seed := range []deleteSeed{seedLiveDeleting, seedFailedIntent} {
		t.Run(seed.name, func(t *testing.T) {
			srv, s := testServer(t)
			agent := setupBrokerAgentInPhase(t, s, "dg-reinc-"+string(rune('a'+i)), state.PhaseRunning)
			seedAgentDeletion(t, s, agent.ID, seed)
			rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+agent.ID+"/reincarnate", ReincarnateAgentRequest{})
			requireDeleteInProgress(t, rec)
			got, err := s.GetAgent(context.Background(), agent.ID)
			require.NoError(t, err)
			assert.Equal(t, store.ReincarnationStateNone, got.ReincarnationState)
		})
	}
}

// Acceptance (x): restoring a soft-deleted row whose delete still has an
// outstanding intent (or a live lease) → 409; the row stays deleted.
func TestDeleteGate_Restore(t *testing.T) {
	for i, seed := range []deleteSeed{seedLiveDeleting, seedInDoubtIntent} {
		t.Run(seed.name, func(t *testing.T) {
			srv, s := testServer(t)
			agent := setupBrokerAgentInPhase(t, s, "dg-restore-"+string(rune('a'+i)), state.PhaseStopped)
			agent.DeletedAt = time.Now()
			require.NoError(t, s.UpdateAgent(context.Background(), agent))
			seedAgentDeletion(t, s, agent.ID, seed)

			rec := doRequest(t, srv, http.MethodPost, "/api/v1/projects/"+agent.ProjectID+"/agents/"+agent.ID+"/restore", nil)
			requireDeleteInProgress(t, rec)
			got, err := s.GetAgent(context.Background(), agent.ID)
			require.NoError(t, err)
			assert.False(t, got.DeletedAt.IsZero(), "the row stays soft-deleted")
		})
	}
}

// Acceptance (x): DM wake of a suspended agent with a delete in progress →
// 409, with no DispatchAgentStart, no quota reserved and no message
// dispatched.
func TestDeleteGate_DMWake(t *testing.T) {
	for _, seed := range []deleteSeed{seedLiveDeleting, seedFailedIntent, seedInDoubtIntent} {
		t.Run(seed.name+"/wake helper", func(t *testing.T) {
			u := newWakeQuotaFixture(t, "dg-wake-"+strings.ReplaceAll(strings.ReplaceAll(seed.name, " ", "-"), "_", "-"), 1)
			disp := &wakeTrackingDispatcher{}
			u.srv.SetDispatcher(disp)
			seedAgentDeletion(t, u.s, u.target.ID, seed)
			target, err := u.s.GetAgent(context.Background(), u.target.ID)
			require.NoError(t, err)

			res, dmErr := u.srv.wakeAgentForDM(context.Background(), target)
			assert.Nil(t, res)
			require.NotNil(t, dmErr)
			assert.Equal(t, http.StatusConflict, dmErr.HTTPStatus)
			assert.Equal(t, ErrCodeDeleteInProgress, dmErr.Code)
			assert.Empty(t, disp.getStartCalls(), "no DispatchAgentStart")
			assert.Zero(t, u.count(t), "no broker quota reserved")
		})

		t.Run(seed.name+"/ExecuteAgentDM", func(t *testing.T) {
			srv, s, sender, target := createWakeDMFixtures(t, string(state.PhaseSuspended))
			disp := &wakeTrackingDispatcher{}
			srv.SetDispatcher(disp)
			seedAgentDeletion(t, s, target.ID, seed)
			fresh, err := s.GetAgent(context.Background(), target.ID)
			require.NoError(t, err)

			result, dmErr := srv.ExecuteAgentDM(context.Background(), &AgentDMInput{
				SenderAgent:    sender,
				SenderIdentity: wakeDMSenderIdentity(sender, ScopeProjectRead, ScopeAgentLifecycle),
				TargetAgent:    fresh,
				Msg:            "hello",
				Type:           "instruction",
				ProjectID:      sender.ProjectID,
				Wake:           true,
			})
			require.NotNil(t, dmErr, "result: %+v", result)
			assert.Equal(t, http.StatusConflict, dmErr.HTTPStatus)
			assert.Equal(t, ErrCodeDeleteInProgress, dmErr.Code)
			assert.Empty(t, disp.getStartCalls(), "no DispatchAgentStart")
			assert.Empty(t, disp.getMessageCalls(), "the message is not dispatched")
		})
	}
}

// Acceptance (c): a status report during a delete changes nothing; a
// report after a soft delete publishes nothing.
func TestDeleteGuard_StatusReport(t *testing.T) {
	post := func(t *testing.T, srv *Server, agent *store.Agent) {
		t.Helper()
		ec := 1
		body, err := json.Marshal(store.AgentStatusUpdate{
			Phase: "error", Activity: "crashed", Message: "container exited", ExitCode: &ec, ExitReason: "crashed",
		})
		require.NoError(t, err)
		req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/"+agent.ID+"/status", bytes.NewReader(body))
		req = req.WithContext(contextWithIdentity(req.Context(), agentIdentityFor(agent.ID, agent.ProjectID, ScopeAgentStatusUpdate)))
		rec := httptest.NewRecorder()
		srv.updateAgentStatus(rec, req, agent.ID)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	}

	t.Run("deleting: nothing changes", func(t *testing.T) {
		srv, s := testServer(t)
		broker, project := newQuotaTestBrokerAndProject(t, s, "dg-status")
		agent := newQuotaTestAgent(t, s, broker, project, "dg-status-del", state.PhaseRunning)
		reserveBrokerSlot(t, s, broker, agent.ID)
		seedAgentDeletion(t, s, agent.ID, seedLiveDeleting)
		before, err := s.GetAgent(context.Background(), agent.ID)
		require.NoError(t, err)

		post(t, srv, agent)

		got, err := s.GetAgent(context.Background(), agent.ID)
		require.NoError(t, err)
		assert.Equal(t, before.Phase, got.Phase)
		assert.Equal(t, before.Activity, got.Activity)
		assert.Equal(t, before.Message, got.Message)
		assert.Nil(t, got.ExitCode)
		assert.Empty(t, got.ExitReason)
		// Guard 0c also keeps the quota reconcile from seeing "error".
		assert.EqualValues(t, 1, brokerReservationCount(t, s, broker.ID), "a report during a delete must not release the quota slot")
	})

	t.Run("soft-deleted: no publish", func(t *testing.T) {
		srv, s := testServer(t)
		pub := &trackingEventPublisher{}
		srv.SetEventPublisher(pub)
		agent := setupBrokerAgentInPhase(t, s, "dg-status-soft", state.PhaseRunning)
		agent.DeletedAt = time.Now()
		require.NoError(t, s.UpdateAgent(context.Background(), agent))

		post(t, srv, agent)

		assert.Empty(t, pub.publishedAgents(), "a soft-deleted row publishes nothing")
		got, err := s.GetAgent(context.Background(), agent.ID)
		require.NoError(t, err)
		assert.Equal(t, string(state.PhaseRunning), got.Phase)
	})

	t.Run("guard 0c", func(t *testing.T) {
		a := &store.Agent{Phase: "running", DeletionState: store.DeletionStateDeleting}
		lease := time.Now().Add(time.Minute)
		a.DeletionLeaseAt = &lease
		ec := 1
		su := store.AgentStatusUpdate{Phase: "error", Activity: "crashed", Message: "m", ExitCode: &ec, ExitReason: "crashed"}
		guardAgentPhaseTransition(a, &su)
		assert.Equal(t, store.AgentStatusUpdate{}, su)
	})
}

// Acceptance (c): a heartbeat during a delete does not change phase or
// activity. The broker quota reservation is the hub-level half: it is
// reconciled from the heartbeat's phase before the store write, so only the
// heartbeat's own suppression (not the in-tx store guard) keeps a "stopped"
// report from releasing the slot mid-delete.
func TestDeleteGuard_Heartbeat(t *testing.T) {
	t.Run("deleting", func(t *testing.T) {
		srv, s := testServer(t)
		grantDevUserRuntimeBrokerAccess(t, s)
		broker, project := newQuotaTestBrokerAndProject(t, s, "dg-hb")
		agent := newQuotaTestAgent(t, s, broker, project, "dg-hb-agent", state.PhaseRunning)
		reserveBrokerSlot(t, s, broker, agent.ID)
		seedAgentDeletion(t, s, agent.ID, seedLiveDeleting)
		before := getAgentState(t, s, agent.Slug, project.ID)

		ec := 137
		code := sendHeartbeat(t, srv, broker.ID, project.ID, brokerAgentHeartbeat{
			Slug: agent.Slug, Phase: "stopped", Activity: "crashed", ExitCode: &ec, ExitReason: "crashed", Message: "dead",
		})
		require.Equal(t, http.StatusOK, code)
		got := getAgentState(t, s, agent.Slug, project.ID)
		assert.Equal(t, string(state.PhaseRunning), got.Phase)
		assert.Equal(t, before.Activity, got.Activity)
		assert.Nil(t, got.ExitCode)
		assert.Empty(t, got.Message)
		assert.EqualValues(t, 1, brokerReservationCount(t, s, broker.ID), "a heartbeat during a delete must not release the quota slot")

		// Legacy (no structured phase) path is suppressed too.
		code = sendHeartbeat(t, srv, broker.ID, project.ID, brokerAgentHeartbeat{Slug: agent.Slug, ContainerStatus: "Exited (1) 3 seconds ago"})
		require.Equal(t, http.StatusOK, code)
		got = getAgentState(t, s, agent.Slug, project.ID)
		assert.Equal(t, string(state.PhaseRunning), got.Phase)
		assert.EqualValues(t, 1, brokerReservationCount(t, s, broker.ID), "legacy heartbeat during a delete must not release the quota slot")
	})

	// A soft-deleted row is never resolved by the heartbeat (GetAgentBySlug
	// skips deleted rows), so its report changes and publishes nothing.
	t.Run("soft-deleted: slug unresolved", func(t *testing.T) {
		srv, s, brokerID, projectID, slug := setupHeartbeatExitCodeTest(t)
		pub := &trackingEventPublisher{}
		srv.SetEventPublisher(pub)
		agent := getAgentState(t, s, slug, projectID)
		agent.DeletedAt = time.Now()
		require.NoError(t, s.UpdateAgent(context.Background(), agent))
		_, err := s.GetAgentBySlug(context.Background(), projectID, slug)
		require.ErrorIs(t, err, store.ErrNotFound, "precondition: the heartbeat cannot resolve a soft-deleted slug")

		code := sendHeartbeat(t, srv, brokerID, projectID, brokerAgentHeartbeat{Slug: slug, Phase: "stopped", Activity: "crashed"})
		assert.Equal(t, http.StatusOK, code)
		got, err := s.GetAgent(context.Background(), agent.ID)
		require.NoError(t, err)
		assert.Equal(t, agent.Phase, got.Phase)
		for _, a := range pub.publishedAgents() {
			assert.NotEqual(t, agent.ID, a.ID, "a soft-deleted row publishes nothing")
		}
	})
}

// startGate fails closed when the dispatch table cannot be read.
func TestStartGate_StoreErrorFailsClosed(t *testing.T) {
	srv, s := testServer(t)
	agent := setupBrokerAgentInPhase(t, s, "dg-failclosed", state.PhaseStopped)
	srv.store = &dispatchErrStore{Store: s}
	ref := srv.startGate(context.Background(), agent, startEntryStart)
	require.True(t, ref.refuses())
	assert.Equal(t, http.StatusInternalServerError, ref.HTTPStatus)
}

type dispatchErrStore struct{ store.Store }

func (d *dispatchErrStore) HasOutstandingBrokerDispatch(context.Context, string, string) (bool, error) {
	return false, errors.New("boom")
}

// AgentStatusEvent always carries the deletion key: populated during a
// delete (the generic view), explicit null otherwise.
func TestAgentStatusEvent_Deletion(t *testing.T) {
	pub := NewChannelEventPublisher()
	defer pub.Close()
	ch, unsub := pub.Subscribe("agent.a1.status")
	defer unsub()

	lease := time.Now().Add(time.Minute)
	pub.PublishAgentStatus(context.Background(), &store.Agent{
		ID: "a1", ProjectID: "g1", Phase: "stopping",
		DeletionState: store.DeletionStateDeleting, DeletionLeaseAt: &lease, DeletionClaim: 2,
	})
	pub.PublishAgentStatus(context.Background(), &store.Agent{ID: "a1", ProjectID: "g1", Phase: "running"})

	for i, want := range []bool{true, false} {
		select {
		case evt := <-ch:
			var m map[string]interface{}
			require.NoError(t, json.Unmarshal(evt.Data, &m))
			v, ok := m["deletion"]
			require.True(t, ok, "event %d must carry the deletion key: %s", i, evt.Data)
			if want {
				d, isMap := v.(map[string]interface{})
				require.True(t, isMap)
				assert.Equal(t, "deleting", d["state"])
				// The event carries the generic view for every
				// subscriber: no claim (ptone/scion#3122).
				assert.NotContains(t, d, "claim")
			} else {
				assert.Nil(t, v)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("timed out waiting for event")
		}
	}
}

// revokeAgentCredentials returns the store's error; the best-effort wrapper
// swallows it.
func TestRevokeAgentCredentials_ReturnsStoreError(t *testing.T) {
	boom := errors.New("revoke failed")
	cs := &revokeErrCredStore{err: boom}
	err := revokeAgentCredentials(context.Background(), cs, "agent-1", agentCredentialRevokeReasonDeleted)
	assert.ErrorIs(t, err, boom)
	assert.Equal(t, agentCredentialRevokeReasonDeleted, cs.reason)

	assert.Error(t, revokeAgentCredentials(context.Background(), nil, "agent-1", agentCredentialRevokeReasonDeleted))

	cs.err = nil
	require.NoError(t, revokeAgentCredentials(context.Background(), cs, "agent-1", agentCredentialRevokeReasonDeleted))

	// The best-effort wrapper never panics or propagates.
	cs.err = boom
	revokeAgentCredentialsBestEffort(context.Background(), cs, "agent-1", agentCredentialRevokeReasonCreateFailed)
	assert.Equal(t, agentCredentialRevokeReasonCreateFailed, cs.reason)
}

type revokeErrCredStore struct {
	store.AgentCredentialStore
	err    error
	reason string
}

func (r *revokeErrCredStore) RevokeAgentCredentialsByAgent(_ context.Context, _ string, _ string, reason string) (int, error) {
	r.reason = reason
	return 0, r.err
}

// Review N1: the clear is pinned to the claim observed at load, so a newer
// delete that claimed and failed while the action ran keeps its marker.
func TestClearFailedDeletion_PinsObservedClaim(t *testing.T) {
	for i, first := range []deleteSeed{
		{name: "failed", state: store.DeletionStateFailed, leaseIn: -time.Minute, code: store.DeletionCodeConflict},
		{name: "abandoned deleting", state: store.DeletionStateDeleting, leaseIn: -time.Minute},
	} {
		t.Run(first.name, func(t *testing.T) {
			srv, s := testServer(t)
			agent := setupBrokerAgentInPhase(t, s, "dg-pin-"+string(rune('a'+i)), state.PhaseStopped)
			seedAgentDeletion(t, s, agent.ID, first)
			loaded, err := s.GetAgent(context.Background(), agent.ID)
			require.NoError(t, err)
			require.Equal(t, int64(1), loaded.DeletionClaim)

			// A newer delete claims (claim 2) and fails while the action runs.
			seedAgentDeletion(t, s, agent.ID, deleteSeed{state: store.DeletionStateFailed, leaseIn: -time.Minute, code: store.DeletionCodeRuntimeError})

			srv.clearFailedDeletion(context.Background(), loaded)
			got, err := s.GetAgent(context.Background(), agent.ID)
			require.NoError(t, err)
			assert.Equal(t, store.DeletionStateFailed, got.DeletionState, "the newer failure survives")
			assert.Equal(t, store.DeletionCodeRuntimeError, got.DeletionCode)
			assert.Equal(t, int64(2), got.DeletionClaim)
			assert.Equal(t, first.state, loaded.DeletionState, "in-memory agent untouched")
		})
	}
}

// raceClaimDispatcher seeds a delete marker from inside DispatchAgentStart,
// as a delete that claims the row while the start is dispatching would.
type raceClaimDispatcher struct {
	deleteGuardDispatcher
	t    *testing.T
	s    store.Store
	seed deleteSeed
}

func (d *raceClaimDispatcher) DispatchAgentStart(ctx context.Context, agent *store.Agent, task string, resume bool) error {
	if err := d.deleteGuardDispatcher.DispatchAgentStart(ctx, agent, task, resume); err != nil {
		return err
	}
	seedAgentDeletion(d.t, d.s, agent.ID, d.seed)
	return nil
}

// A start whose dispatch succeeds while a delete claims the row answers 409
// delete_in_progress with no agent body (ptone/scion#3255), not 200 with
// the stored row: the delete won after the start landed. Nothing is written
// or published after the dispatch: the claim keeps the phase the start found
// and its marker. The dispatcher is a mock, so no compensating delete or
// warning is involved (lifecycle_landed_delete_test.go covers those).
func TestDeleteGate_StartLosesRaceToDeleteClaim(t *testing.T) {
	for i, initial := range []*deleteSeed{
		nil,
		{name: "failed", state: store.DeletionStateFailed, leaseIn: -time.Minute, code: store.DeletionCodeConflict},
	} {
		name := "no marker"
		if initial != nil {
			name = initial.name
		}
		t.Run(name, func(t *testing.T) {
			srv, s := testServer(t)
			disp := &raceClaimDispatcher{t: t, s: s, seed: seedLiveDeleting}
			srv.SetDispatcher(disp)
			pub := &trackingEventPublisher{}
			srv.SetEventPublisher(pub)
			agent := setupBrokerAgentInPhase(t, s, "dg-race-"+string(rune('a'+i)), state.PhaseStopped)
			if initial != nil {
				seedAgentDeletion(t, s, agent.ID, *initial)
			}

			rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+agent.ID+"/start", nil)
			requireIntentDeleteInProgress(t, rec, agent.ID)
			require.Equal(t, 1, disp.starts)
			var raw map[string]json.RawMessage
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &raw))
			assert.NotContains(t, raw, "id", "no agent body")
			assert.NotContains(t, raw, "agent", "no agent body")

			for _, a := range pub.publishedAgents() {
				assert.NotEqual(t, string(state.PhaseRunning), a.Phase, "the start publishes no running status")
				assert.NotEqual(t, store.DeletionStateDeleting, a.DeletionState, "no status publish after the claim")
			}

			// The start marked the stopped agent starting before it
			// dispatched (beginStartDispatch, ptone/scion#2014); the claim
			// keeps that phase, and the start writes nothing after it.
			got, err := s.GetAgent(context.Background(), agent.ID)
			require.NoError(t, err)
			assert.Equal(t, string(state.PhaseStarting), got.Phase)
			assert.Equal(t, store.DeletionStateDeleting, got.DeletionState, "the racing claim's marker is kept")
		})
	}
}

// Review N3: a successful managed start/restart clears a failed marker.
func TestDeleteGate_ManagedStartClearsFailedMarker(t *testing.T) {
	for i, action := range []string{api.AgentActionStart, api.AgentActionRestart} {
		t.Run(action, func(t *testing.T) {
			srv, s := testServer(t)
			agent := setupBrokerAgentInPhase(t, s, "dg-managed-clear-"+string(rune('a'+i)), state.PhaseStopped)
			agent.Runtime = ManagedRuntimePrefix + "test"
			require.NoError(t, s.UpdateAgent(context.Background(), agent))
			seedAgentDeletion(t, s, agent.ID, deleteSeed{state: store.DeletionStateFailed, leaseIn: -time.Minute, code: store.DeletionCodeConflict})

			rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+agent.ID+"/"+action, nil)
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			var body map[string]interface{}
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
			v, ok := body["deletion"]
			assert.True(t, ok && v == nil, "deletion must be an explicit null after the clear: %s", rec.Body.String())

			got, err := s.GetAgent(context.Background(), agent.ID)
			require.NoError(t, err)
			assert.Equal(t, string(state.PhaseRunning), got.Phase)
			assert.Equal(t, store.DeletionStateNone, got.DeletionState)
			assert.Equal(t, int64(1), got.DeletionClaim, "the claim epoch is kept")
		})
	}
}

// reincarnateClaimDispatcher seeds a newer delete failure from inside the
// worker's start dispatch.
type reincarnateClaimDispatcher struct {
	*reincarnateTestDispatcher
	t *testing.T
	s store.Store
}

func (d *reincarnateClaimDispatcher) DispatchAgentStart(ctx context.Context, agent *store.Agent, task string, resume bool) error {
	if err := d.reincarnateTestDispatcher.DispatchAgentStart(ctx, agent, task, resume); err != nil {
		return err
	}
	seedAgentDeletion(d.t, d.s, agent.ID, deleteSeed{state: store.DeletionStateFailed, leaseIn: -time.Minute, code: store.DeletionCodeRuntimeError})
	return nil
}

// completionFailStore fails the reincarnate worker's completion write (the
// only UpdateAgent that returns reincarnation_state to "" with a generation
// past the original) and signals when it did.
type completionFailStore struct {
	store.Store
	fromGeneration int
	once           sync.Once
	failed         chan struct{}
}

func (c *completionFailStore) UpdateAgent(ctx context.Context, a *store.Agent) error {
	if a.ReincarnationState == store.ReincarnationStateNone && a.Generation > c.fromGeneration {
		c.once.Do(func() { close(c.failed) })
		return errors.New("completion write failed")
	}
	return c.Store.UpdateAgent(ctx, a)
}

// recordOutcomeStore intercepts the reincarnate worker's record CAS to
// completed. With sweep set it first resolves the record to failed (as the
// replica-safe sweep would) and then lets the real CAS run, which finds 0
// rows; otherwise it returns an error on every attempt.
type recordOutcomeStore struct {
	store.Store
	sweep bool
}

func (r *recordOutcomeStore) TryAdvanceAgentReincarnation(ctx context.Context, rec *store.AgentReincarnation, expectState string, olderThan time.Time) (bool, error) {
	if rec.State != store.AgentReincarnationStateCompleted {
		return r.Store.TryAdvanceAgentReincarnation(ctx, rec, expectState, olderThan)
	}
	if !r.sweep {
		return false, errors.New("record CAS failed")
	}
	now := time.Now()
	swept := &store.AgentReincarnation{ID: rec.ID, State: store.AgentReincarnationStateFailed, Error: "swept", UpdatedAt: now, CompletedAt: &now}
	if ok, err := r.Store.TryAdvanceAgentReincarnation(ctx, swept, expectState, time.Time{}); err != nil || !ok {
		return false, fmt.Errorf("test sweep did not land: ok=%v err=%v", ok, err)
	}
	return r.Store.TryAdvanceAgentReincarnation(ctx, rec, expectState, olderThan)
}

// Review N3: reincarnate clears a failed marker once the new generation is
// live and the worker has won its record, pinned to the claim the handler's
// gate admitted. The clear runs just before the completion write, so a
// caller that sees completion also sees the cleared marker, and a failed
// completion write (bookkeeping only) still leaves it cleared.
func TestDeleteGate_ReincarnateCompletionClearsFailedMarker(t *testing.T) {
	failedSeed := deleteSeed{state: store.DeletionStateFailed, leaseIn: -time.Minute, code: store.DeletionCodeConflict}
	reincarnate := func(t *testing.T, srv *Server, agent *store.Agent, projectID string) {
		t.Helper()
		rec := httptest.NewRecorder()
		srv.handleReincarnateAgent(rec, reincarnateRequest(t, agent.ID, agentIdentityFor(agent.ID, projectID), ReincarnateAgentRequest{Handoff: "h"}), agent.ID)
		require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())
	}

	// This subtest alone cannot catch a regression that moves the clear
	// back after the completion write (that only races); "completion write
	// fails" below pins the ordering, so do not delete that one.
	t.Run("completion clears", func(t *testing.T) {
		srv, s, project, broker := setupReincarnateTestServer(t, newReincarnateTestDispatcher())
		agent := newReincarnateTestAgent(t, s, project, broker, nil)
		seedAgentDeletion(t, s, agent.ID, failedSeed)

		reincarnate(t, srv, agent, project.ID)
		rec := waitForReincarnationSettled(t, s, agent.ID)
		require.Equal(t, store.AgentReincarnationStateCompleted, rec.State)
		got, err := s.GetAgent(context.Background(), agent.ID)
		require.NoError(t, err)
		assert.Equal(t, store.DeletionStateNone, got.DeletionState)
		assert.Equal(t, int64(1), got.DeletionClaim, "the claim epoch is kept")
		// The completion write absorbed the clear's state_version bump.
		assert.Equal(t, rec.ToGeneration, got.Generation)
		assert.Equal(t, store.ReincarnationStateNone, got.ReincarnationState)
		assert.Empty(t, got.Message, "the migrating message is cleared")
		require.NotNil(t, got.AppliedConfig)
		require.NotNil(t, rec.NewAppliedConfig)
		assert.Equal(t, rec.NewAppliedConfig.Task, got.AppliedConfig.Task)
	})

	t.Run("newer claim during the worker survives", func(t *testing.T) {
		disp := &reincarnateClaimDispatcher{reincarnateTestDispatcher: newReincarnateTestDispatcher(), t: t}
		srv, s, project, broker := setupReincarnateTestServer(t, disp)
		disp.s = s
		agent := newReincarnateTestAgent(t, s, project, broker, nil)
		seedAgentDeletion(t, s, agent.ID, failedSeed)

		reincarnate(t, srv, agent, project.ID)
		rec := waitForReincarnationSettled(t, s, agent.ID)
		require.Equal(t, store.AgentReincarnationStateCompleted, rec.State)
		got, err := s.GetAgent(context.Background(), agent.ID)
		require.NoError(t, err)
		assert.Equal(t, store.DeletionStateFailed, got.DeletionState)
		assert.Equal(t, store.DeletionCodeRuntimeError, got.DeletionCode)
		assert.Equal(t, int64(2), got.DeletionClaim)
	})

	// Review round 2 (D): the clear is pinned to the claim the handler's gate
	// admitted, so a delete that claims between admission and the worker's
	// first read (here: right after the reincarnation record is created)
	// keeps its marker.
	t.Run("claim between admission and worker survives", func(t *testing.T) {
		srv, s, project, broker := setupReincarnateTestServer(t, newReincarnateTestDispatcher())
		agent := newReincarnateTestAgent(t, s, project, broker, nil)
		seedAgentDeletion(t, s, agent.ID, failedSeed)
		srv.store = &admissionClaimStore{Store: s, t: t}

		reincarnate(t, srv, agent, project.ID)
		rec := waitForReincarnationSettled(t, s, agent.ID)
		require.Equal(t, store.AgentReincarnationStateCompleted, rec.State)
		got, err := s.GetAgent(context.Background(), agent.ID)
		require.NoError(t, err)
		assert.Equal(t, store.DeletionStateFailed, got.DeletionState)
		assert.Equal(t, store.DeletionCodeRuntimeError, got.DeletionCode)
		assert.Equal(t, int64(2), got.DeletionClaim)
	})

	t.Run("completion write fails: marker cleared, generation not advanced", func(t *testing.T) {
		srv, s, project, broker := setupReincarnateTestServer(t, newReincarnateTestDispatcher())
		agent := newReincarnateTestAgent(t, s, project, broker, nil)
		seedAgentDeletion(t, s, agent.ID, failedSeed)
		fs := &completionFailStore{Store: s, fromGeneration: agent.Generation, failed: make(chan struct{})}
		srv.store = fs
		// The worker logs "reincarnation completed" as its last act, after
		// the clear and the (failed) completion write.
		done := &logSignalHandler{msg: "reincarnation completed", ch: make(chan struct{})}
		srv.agentLifecycleLog = slog.New(done)

		reincarnate(t, srv, agent, project.ID)
		select {
		case <-fs.failed:
		case <-time.After(5 * time.Second):
			t.Fatal("worker never reached its completion write")
		}
		select {
		case <-done.ch:
		case <-time.After(5 * time.Second):
			t.Fatal("worker never finished")
		}
		got, err := s.GetAgent(context.Background(), agent.ID)
		require.NoError(t, err)
		assert.Equal(t, store.DeletionStateNone, got.DeletionState, "gen N+1 is live: the start succeeded")
		assert.Equal(t, agent.Generation, got.Generation, "the completion write did not land")
	})

	// The paths where the new generation is not confirmed as this worker's
	// success never clear.
	noClear := func(t *testing.T, srv *Server, s store.Store, agent *store.Agent, wait func()) {
		t.Helper()
		reincarnate(t, srv, agent, agent.ProjectID)
		wait()
		got, err := s.GetAgent(context.Background(), agent.ID)
		require.NoError(t, err)
		assert.Equal(t, store.DeletionStateFailed, got.DeletionState)
		assert.Equal(t, store.DeletionCodeConflict, got.DeletionCode)
		assert.Equal(t, agent.Generation, got.Generation)
	}
	waitLog := func(t *testing.T, srv *Server, msg string) func() {
		h := &logSignalHandler{msg: msg, ch: make(chan struct{})}
		srv.agentLifecycleLog = slog.New(h)
		return func() {
			select {
			case <-h.ch:
			case <-time.After(10 * time.Second):
				t.Fatalf("worker never logged %q", msg)
			}
		}
	}

	t.Run("start failure: marker kept", func(t *testing.T) {
		disp := newReincarnateTestDispatcher()
		disp.startErr = errors.New("start boom")
		srv, s, project, broker := setupReincarnateTestServer(t, disp)
		agent := newReincarnateTestAgent(t, s, project, broker, nil)
		seedAgentDeletion(t, s, agent.ID, failedSeed)
		noClear(t, srv, s, agent, func() {
			rec := waitForReincarnationSettled(t, s, agent.ID)
			require.Equal(t, store.AgentReincarnationStateFailed, rec.State)
		})
	})

	t.Run("sweep won the record: marker kept", func(t *testing.T) {
		srv, s, project, broker := setupReincarnateTestServer(t, newReincarnateTestDispatcher())
		agent := newReincarnateTestAgent(t, s, project, broker, nil)
		seedAgentDeletion(t, s, agent.ID, failedSeed)
		srv.store = &recordOutcomeStore{Store: s, sweep: true}
		noClear(t, srv, s, agent, waitLog(t, srv, "reincarnation worker: record no longer non-terminal at completion; agent already started on the new generation but bookkeeping is skipped"))
	})

	t.Run("record CAS error: marker kept", func(t *testing.T) {
		srv, s, project, broker := setupReincarnateTestServer(t, newReincarnateTestDispatcher())
		agent := newReincarnateTestAgent(t, s, project, broker, nil)
		seedAgentDeletion(t, s, agent.ID, failedSeed)
		srv.store = &recordOutcomeStore{Store: s}
		noClear(t, srv, s, agent, waitLog(t, srv, "reincarnation worker: agent started on new generation but failed to advance the record to completed"))
	})
}

// admissionClaimStore seeds a newer delete failure right after the
// reincarnate handler's claim transaction creates its record and commits:
// after the gate, before the worker.
type admissionClaimStore struct {
	store.Store
	t *testing.T
}

func (a *admissionClaimStore) WithTx(ctx context.Context, fn func(tx store.Store) error) error {
	rec := &admissionClaimTx{}
	if err := a.Store.WithTx(ctx, func(tx store.Store) error {
		rec.Store = tx
		return fn(rec)
	}); err != nil {
		return err
	}
	if rec.agentID != "" {
		seedAgentDeletion(a.t, a.Store, rec.agentID, deleteSeed{state: store.DeletionStateFailed, leaseIn: -time.Minute, code: store.DeletionCodeRuntimeError})
	}
	return nil
}

// admissionClaimTx records the agent a reincarnation record is created for.
type admissionClaimTx struct {
	store.Store
	agentID string
}

func (a *admissionClaimTx) CreateAgentReincarnation(ctx context.Context, rec *store.AgentReincarnation) error {
	if err := a.Store.CreateAgentReincarnation(ctx, rec); err != nil {
		return err
	}
	a.agentID = rec.AgentID
	return nil
}

// logSignalHandler closes ch the first time a record with message msg is
// logged.
type logSignalHandler struct {
	msg  string
	ch   chan struct{}
	once sync.Once
}

func (h *logSignalHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *logSignalHandler) Handle(_ context.Context, r slog.Record) error {
	if r.Message == h.msg {
		h.once.Do(func() { close(h.ch) })
	}
	return nil
}
func (h *logSignalHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *logSignalHandler) WithGroup(string) slog.Handler      { return h }

// The guard predicates treat a nil agent as "no delete".
func TestDeleteGuards_NilAgent(t *testing.T) {
	assert.False(t, deletionActive(nil))
	assert.False(t, deleteStopNoop(nil))
	srv, _ := testServer(t)
	blocked, err := srv.deleteBlocksStart(context.Background(), nil)
	require.NoError(t, err)
	assert.False(t, blocked)
}
