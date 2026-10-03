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
	"net/http"
	"net/http/httptest"
	"strings"
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
				assert.Nil(t, got.DeletionLeaseAt)
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
				SenderIdentity: &wakeDMTestIdentity{id: sender.ID, projectID: sender.ProjectID, ancestry: sender.Ancestry},
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
		agent := setupBrokerAgentInPhase(t, s, "dg-status-del", state.PhaseRunning)
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
// activity, and a soft-deleted row's heartbeat publishes nothing.
func TestDeleteGuard_Heartbeat(t *testing.T) {
	t.Run("deleting", func(t *testing.T) {
		srv, s, brokerID, projectID, slug := setupHeartbeatExitCodeTest(t)
		agent := getAgentState(t, s, slug, projectID)
		seedAgentDeletion(t, s, agent.ID, seedLiveDeleting)

		ec := 137
		code := sendHeartbeat(t, srv, brokerID, projectID, brokerAgentHeartbeat{
			Slug: slug, Phase: "stopped", Activity: "crashed", ExitCode: &ec, ExitReason: "crashed", Message: "dead",
		})
		assert.Equal(t, http.StatusOK, code)
		got := getAgentState(t, s, slug, projectID)
		assert.Equal(t, "running", got.Phase)
		assert.Equal(t, "working", got.Activity)
		assert.Nil(t, got.ExitCode)
		assert.Empty(t, got.Message)

		// Legacy (no structured phase) path is suppressed too.
		code = sendHeartbeat(t, srv, brokerID, projectID, brokerAgentHeartbeat{Slug: slug, ContainerStatus: "Exited (1) 3 seconds ago"})
		assert.Equal(t, http.StatusOK, code)
		got = getAgentState(t, s, slug, projectID)
		assert.Equal(t, "running", got.Phase)
	})

	t.Run("soft-deleted: no publish", func(t *testing.T) {
		srv, s, brokerID, projectID, slug := setupHeartbeatExitCodeTest(t)
		pub := &trackingEventPublisher{}
		srv.SetEventPublisher(pub)
		agent := getAgentState(t, s, slug, projectID)
		agent.DeletedAt = time.Now()
		require.NoError(t, s.UpdateAgent(context.Background(), agent))

		code := sendHeartbeat(t, srv, brokerID, projectID, brokerAgentHeartbeat{Slug: slug, Phase: "stopped", Activity: "crashed"})
		assert.Equal(t, http.StatusOK, code)
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
// delete, explicit null otherwise.
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
				assert.Equal(t, float64(2), d["claim"])
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
