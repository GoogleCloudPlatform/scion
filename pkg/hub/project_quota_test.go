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
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// Tests for the per-project agent limit following the agent lifecycle
// (ptone/scion#6033): only agents that hold capacity count, every
// start-type admission re-checks the limit, the total is shared by every
// creator in the project, and the cap compared against it is the one
// resolved for the agent's creator.

// projectLimitDefID returns the max_agents_per_project limit definition ID.
func projectLimitDefID(t *testing.T, s store.Store) string {
	t.Helper()
	def, err := s.GetLimitDefinitionByName(context.Background(), store.LimitMaxAgentsPerProject)
	require.NoError(t, err)
	return def.ID
}

// seedProjectRow records agent's project reservation directly (subject: its
// creator), as an earlier admission would have, and returns it.
func seedProjectRow(t *testing.T, s store.Store, agent *store.Agent) *store.UsageReservation {
	t.Helper()
	return seedProjectRowAt(t, s, agent, time.Time{})
}

// seedProjectRowAt is seedProjectRow with the row's creation time.
func seedProjectRowAt(t *testing.T, s store.Store, agent *store.Agent, createdAt time.Time) *store.UsageReservation {
	t.Helper()
	r, err := s.CreateUsageReservation(context.Background(), &store.UsageReservation{
		LimitDefinitionID: projectLimitDefID(t, s),
		SubjectID:         projectQuotaSubject(agent),
		ScopeType:         store.QuotaScopeProject,
		ScopeID:           agent.ProjectID,
		ResourceID:        agent.ID,
		Reserved:          1,
		CreatedAt:         createdAt,
	})
	require.NoError(t, err)
	return r
}

// hasProjectRow reports whether agentID holds an active project reservation.
func hasProjectRow(t *testing.T, s store.Store, agentID string) bool {
	t.Helper()
	return hasReservation(t, s, store.LimitMaxAgentsPerProject, agentID)
}

// projectRowCount returns the number of active project reservations of
// projectID, every creator included.
func projectRowCount(t *testing.T, s store.Store, projectID string) int64 {
	t.Helper()
	counts, err := s.CountActiveReservationsByScope(context.Background(), projectLimitDefID(t, s), store.QuotaScopeProject)
	require.NoError(t, err)
	return counts[projectID]
}

// setUserProjectCap gives userID a user-scoped entitlement binding of value
// for max_agents_per_project, applying in every project.
func setUserProjectCap(t *testing.T, s store.Store, userID string, value int64) {
	t.Helper()
	now := time.Now()
	_, err := s.CreateEntitlementBinding(context.Background(), &store.EntitlementBinding{
		ID:                uuid.New().String(),
		LimitDefinitionID: projectLimitDefID(t, s),
		SubjectType:       store.EntitlementSubjectUser,
		SubjectID:         userID,
		ScopeType:         store.QuotaScopeSystem,
		Value:             value,
		CreatedAt:         now,
		UpdatedAt:         now,
	})
	require.NoError(t, err)
}

// newCreatorAgent is newQuotaTestAgent with the agent's creator set; the
// creator is a live project member, so the agent is in good standing.
func newCreatorAgent(t *testing.T, s store.Store, broker *store.RuntimeBroker, project *store.Project, name string, phase state.Phase, creator string) *store.Agent {
	t.Helper()
	ensureStandingRoot(t, s, project.ID, creator)
	agent := &store.Agent{
		ID:              tid("agent-" + name),
		Slug:            name,
		Name:            name,
		ProjectID:       project.ID,
		RuntimeBrokerID: broker.ID,
		Phase:           string(phase),
		CreatedBy:       creator,
		OwnerID:         creator,
		Ancestry:        []string{creator},
	}
	require.NoError(t, s.CreateAgent(context.Background(), agent))
	return agent
}

// newSubAgent creates an agent created by parent (another agent), in good
// standing through parent's root user.
func newSubAgent(t *testing.T, s store.Store, broker *store.RuntimeBroker, project *store.Project, name string, phase state.Phase, parent *store.Agent) *store.Agent {
	t.Helper()
	agent := &store.Agent{
		ID:              tid("agent-" + name),
		Slug:            name,
		Name:            name,
		ProjectID:       project.ID,
		RuntimeBrokerID: broker.ID,
		Phase:           string(phase),
		CreatedBy:       parent.ID,
		OwnerID:         parent.OwnerID,
		Ancestry:        append(append([]string(nil), parent.Ancestry...), parent.ID),
	}
	require.NoError(t, s.CreateAgent(context.Background(), agent))
	return agent
}

// assertProjectQuotaExceeded checks rec is the project-limit refusal: 429
// quota_exceeded naming the project, its count and its limit, and the
// remedy.
func assertProjectQuotaExceeded(t *testing.T, rec *httptest.ResponseRecorder, projectID string, count, max int64) {
	t.Helper()
	require.Equal(t, http.StatusTooManyRequests, rec.Code, rec.Body.String())
	var resp ErrorResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp), rec.Body.String())
	assert.Equal(t, ErrCodeQuotaExceeded, resp.Error.Code)
	assert.Equal(t, projectRefusalMessage(projectID, count, max), resp.Error.Message)
	assert.Equal(t, projectID, resp.Error.Details["projectId"])
	assert.EqualValues(t, count, resp.Error.Details["count"])
	assert.EqualValues(t, max, resp.Error.Details["limit"])
}

// projectRefusalMessage is the expected project-limit refusal message.
func projectRefusalMessage(projectID string, count, max int64) string {
	return "quota exceeded: max_agents_per_project (project " + projectID + ": " +
		strconv.FormatInt(count, 10) + " of " + strconv.FormatInt(max, 10) +
		" agents in use); stop or delete an agent in this project to free a slot"
}

// --- Release sites ---------------------------------------------------------

// Every explicit stop or suspend path frees the project slot along with the
// broker slot.
func TestProjectQuota_ExplicitStopAndSuspendFreeSlot(t *testing.T) {
	cases := []struct {
		name   string
		action func(t *testing.T, srv *Server, a *store.Agent)
		phase  state.Phase
	}{
		{"stop", func(t *testing.T, srv *Server, a *store.Agent) {
			rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+a.ID+"/stop", nil)
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		}, state.PhaseStopped},
		{"suspend", func(t *testing.T, srv *Server, a *store.Agent) {
			rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+a.ID+"/suspend", nil)
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		}, state.PhaseSuspended},
		{"stop-all", func(t *testing.T, srv *Server, a *store.Agent) {
			rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/stop-all", nil)
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		}, state.PhaseStopped},
		{"auto-suspend", func(t *testing.T, srv *Server, a *store.Agent) {
			markAgentStalled(t, srv.store, a.ID)
			srv.autoSuspendStalledAgents(context.Background(), []store.Agent{*getAgent(t, srv.store, a.ID)})
		}, state.PhaseSuspended},
		{"membership-hold", func(t *testing.T, srv *Server, a *store.Agent) {
			require.NoError(t, srv.stopHeldAgent(context.Background(), a.ID, 1))
		}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, s := testServer(t)
			srv.SetDispatcher(&quotaLifecycleDispatcher{})
			broker, project := newQuotaTestBrokerAndProject(t, s, "pq-rel-"+tc.name)
			a := newCreatorAgent(t, s, broker, project, "pq-rel-"+tc.name, state.PhaseRunning, tid("user-a"))
			reserveBrokerSlot(t, s, broker, a.ID)
			seedProjectRow(t, s, a)

			tc.action(t, srv, a)

			got := getAgent(t, s, a.ID)
			if tc.phase != "" {
				require.Equal(t, string(tc.phase), got.Phase)
			} else {
				require.False(t, isBrokerQuotaCountedPhase(got.Phase), "the held agent rests: %s", got.Phase)
			}
			assert.False(t, hasReservation(t, s, store.LimitMaxAgentsPerBroker, a.ID), "the broker slot is freed")
			assert.False(t, hasProjectRow(t, s, a.ID), "the project slot is freed with it")
		})
	}
}

// A restart whose start leg definitely did not act records the agent
// stopped and frees both slots (handlers_agent_lifecycle.go, the
// confirmed-failure branch).
func TestProjectQuota_RestartConfirmedStartFailureFreesSlot(t *testing.T) {
	srv, s := testServer(t)
	srv.SetDispatcher(&notSentStartDispatcher{})
	broker, project := newQuotaTestBrokerAndProject(t, s, "pq-restart-notsent")
	a := newCreatorAgent(t, s, broker, project, "pq-restart-notsent", state.PhaseRunning, tid("user-a"))
	reserveBrokerSlot(t, s, broker, a.ID)
	seedProjectRow(t, s, a)
	_, err := s.SetAgentRunID(context.Background(), a.ID, "run-x", nil)
	require.NoError(t, err)

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+a.ID+"/restart", nil)
	require.GreaterOrEqual(t, rec.Code, 500, rec.Body.String())
	assert.Equal(t, string(state.PhaseStopped), getAgent(t, s, a.ID).Phase)
	assert.False(t, hasProjectRow(t, s, a.ID), "the confirmed failure frees the project slot")
}

// Observed exits free the project slot through every path that reports a
// phase: the agent's own status report, the broker heartbeat and the
// runtime-broker agent status endpoint.
func TestProjectQuota_ObservedExitFreesSlot(t *testing.T) {
	t.Run("status-endpoint", func(t *testing.T) {
		srv, s := testServer(t)
		broker, project := newQuotaTestBrokerAndProject(t, s, "pq-oe-self")
		a := newCreatorAgent(t, s, broker, project, "pq-oe-self", state.PhaseRunning, tid("user-a"))
		seedProjectRow(t, s, a)
		selfReport(t, srv, a, state.PhaseError)
		assert.Equal(t, string(state.PhaseError), getAgent(t, s, a.ID).Phase)
		assert.False(t, hasProjectRow(t, s, a.ID))
	})
	t.Run("heartbeat", func(t *testing.T) {
		f := newReconcileFixture(t)
		a := f.addAgent("pq-oe-hb", "running", "working")
		seedProjectRow(t, f.s, a)
		f.send(brokerHeartbeatRequest{
			Status: store.BrokerStatusOnline,
			Projects: []brokerProjectHeartbeat{{ProjectID: f.projectID, Agents: []brokerAgentHeartbeat{
				{Slug: a.Slug, Phase: "stopped", ContainerStatus: "Exited (0)", RuntimeTarget: "docker"},
			}}},
		})
		assert.Equal(t, string(state.PhaseStopped), getAgent(t, f.s, a.ID).Phase)
		assert.False(t, hasProjectRow(t, f.s, a.ID))
	})
	t.Run("heartbeat-missing", func(t *testing.T) {
		f := newReconcileFixture(t)
		a := f.addAgent("pq-oe-missing", "running", "working")
		seedProjectRow(t, f.s, a)
		f.heartbeat(completeInventory())
		f.expireClock(a.ID)
		f.heartbeat(completeInventory())
		assert.Equal(t, string(state.PhaseError), getAgent(t, f.s, a.ID).Phase, "marked missing")
		assert.False(t, hasProjectRow(t, f.s, a.ID))
	})
	t.Run("back-to-running-re-reserves", func(t *testing.T) {
		srv, s := testServer(t)
		broker, project := newQuotaTestBrokerAndProject(t, s, "pq-oe-back")
		a := newCreatorAgent(t, s, broker, project, "pq-oe-back", state.PhaseStopped, tid("user-a"))
		srv.reconcileProjectQuotaOnPhaseChange(context.Background(), a, string(state.PhaseStopped), string(state.PhaseRunning))
		assert.True(t, hasProjectRow(t, s, a.ID), "an observed start is recorded")
	})
}

// A stopped report while a start-kind claim is held keeps the
// project row: the start in flight owns the slot. Once the claim clears,
// the claim-finish release frees it.
func TestProjectQuota_StoppedReportUnderStartClaimKeepsSlot(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	broker, project := newQuotaTestBrokerAndProject(t, s, "pq-claim-held")
	a := newCreatorAgent(t, s, broker, project, "pq-claim-held", state.PhaseRunning, tid("user-a"))
	seedProjectRow(t, s, a)

	run, err := srv.acquireStartClaim(ctx, a, store.StartClaimUser)
	require.NoError(t, err)
	selfReport(t, srv, a, state.PhaseStopped)
	assert.True(t, hasProjectRow(t, s, a.ID), "kept while a start-kind claim is held")

	run.finish(startReleased)
	assert.Empty(t, getAgent(t, s, a.ID).StartClaimID)
	assert.False(t, hasProjectRow(t, s, a.ID), "freed when the claim cleared")
}

// A stopped report for a run the row no longer has keeps the
// project row (ExpectedRunID).
func TestProjectQuota_StoppedReportOfOldRunKeepsSlot(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	broker, project := newQuotaTestBrokerAndProject(t, s, "pq-old-run")
	a := newCreatorAgent(t, s, broker, project, "pq-old-run", state.PhaseRunning, tid("user-a"))
	seedProjectRow(t, s, a)
	_, err := s.SetAgentRunID(ctx, a.ID, "run-new", nil)
	require.NoError(t, err)

	stale := *a
	stale.RunID = "run-old"
	srv.reconcileProjectQuotaOnPhaseChange(ctx, &stale, string(state.PhaseRunning), string(state.PhaseStopped))
	assert.True(t, hasProjectRow(t, s, a.ID), "a report read with another run releases nothing")

	cur := getAgent(t, s, a.ID)
	srv.reconcileProjectQuotaOnPhaseChange(ctx, cur, string(state.PhaseRunning), string(state.PhaseStopped))
	assert.False(t, hasProjectRow(t, s, a.ID), "control: the same report with the row's run releases it")
}

// --- Admission -------------------------------------------------------------

// Resuming a suspended agent re-checks the project limit; at a full
// project it is refused with the project message and nothing is dispatched.
func TestProjectQuota_ResumeAtFullProjectRefused(t *testing.T) {
	srv, s := testServer(t)
	disp := &quotaLifecycleDispatcher{}
	srv.SetDispatcher(disp)
	setProjectAgentCeiling(t, s, 1)
	broker, project := newQuotaTestBrokerAndProject(t, s, "pq-resume-full")
	occupant := newCreatorAgent(t, s, broker, project, "pq-resume-occ", state.PhaseRunning, tid("user-a"))
	seedProjectRow(t, s, occupant)
	cand := newCreatorAgent(t, s, broker, project, "pq-resume-cand", state.PhaseSuspended, tid("user-a"))

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+cand.ID+"/start", nil)
	assertProjectQuotaExceeded(t, rec, project.ID, 1, 1)
	assert.Zero(t, disp.startCount.Load(), "nothing dispatched")
	assert.Equal(t, string(state.PhaseSuspended), getAgent(t, s, cand.ID).Phase)
	assert.False(t, hasProjectRow(t, s, cand.ID))
	assert.False(t, hasReservation(t, s, store.LimitMaxAgentsPerBroker, cand.ID), "the broker slot taken first is rolled back")

	// Control: once the occupant stops, the resume is admitted and reserves.
	rec = doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+occupant.ID+"/stop", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	rec = doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+cand.ID+"/start", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.True(t, hasProjectRow(t, s, cand.ID))
}

// Restarting a stopped agent at a full project is refused before
// anything happens: no stop or start dispatch, phase and run intent
// unchanged.
func TestProjectQuota_RestartStoppedAtFullProjectRefused(t *testing.T) {
	srv, s := testServer(t)
	disp := &quotaLifecycleDispatcher{}
	srv.SetDispatcher(disp)
	setProjectAgentCeiling(t, s, 1)
	broker, project := newQuotaTestBrokerAndProject(t, s, "pq-restart-full")
	occupant := newCreatorAgent(t, s, broker, project, "pq-restart-occ", state.PhaseRunning, tid("user-a"))
	seedProjectRow(t, s, occupant)
	cand := newCreatorAgent(t, s, broker, project, "pq-restart-cand", state.PhaseStopped, tid("user-a"))
	before := getAgent(t, s, cand.ID)

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+cand.ID+"/restart", nil)
	assertProjectQuotaExceeded(t, rec, project.ID, 1, 1)
	assert.Zero(t, disp.startCount.Load())
	assert.Zero(t, disp.stopCount.Load())
	got := getAgent(t, s, cand.ID)
	assert.Equal(t, string(state.PhaseStopped), got.Phase)
	assert.Equal(t, before.RunIntent, got.RunIntent)
	assert.False(t, hasProjectRow(t, s, cand.ID))
}

// A running agent's restart at a full project is admitted: it already holds
// its slot.
func TestProjectQuota_RestartRunningAtFullProjectSucceeds(t *testing.T) {
	srv, s := testServer(t)
	srv.SetDispatcher(&quotaLifecycleDispatcher{})
	setProjectAgentCeiling(t, s, 1)
	broker, project := newQuotaTestBrokerAndProject(t, s, "pq-restart-run")
	a := newCreatorAgent(t, s, broker, project, "pq-restart-run", state.PhaseRunning, tid("user-a"))
	reserveBrokerSlot(t, s, broker, a.ID)
	seedProjectRow(t, s, a)

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+a.ID+"/restart", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.True(t, hasProjectRow(t, s, a.ID))
	assert.EqualValues(t, 1, projectRowCount(t, s, project.ID))
}

// A DM wake of a suspended agent at a full project is refused with the
// project message and no resume dispatch.
func TestProjectQuota_WakeAtFullProjectRefused(t *testing.T) {
	srv, s := testServer(t)
	disp := &wakeTrackingDispatcher{}
	srv.SetDispatcher(disp)
	setProjectAgentCeiling(t, s, 1)
	broker, project := newQuotaTestBrokerAndProject(t, s, "pq-wake")
	held := newCreatorAgent(t, s, broker, project, "pq-wake-held", state.PhaseRunning, tid("user-a"))
	seedProjectRow(t, s, held)
	target := newCreatorAgent(t, s, broker, project, "pq-wake-target", state.PhaseSuspended, tid("user-a"))

	_, dmErr := srv.wakeAgentForDM(context.Background(), target)
	require.NotNil(t, dmErr, "wake at a full project must fail")
	assert.Equal(t, http.StatusTooManyRequests, dmErr.HTTPStatus)
	assert.Equal(t, ErrCodeQuotaExceeded, dmErr.Code)
	assert.Equal(t, projectRefusalMessage(project.ID, 1, 1), dmErr.Message)
	assert.Len(t, disp.getStartCalls(), 0, "no resume dispatch")
	assert.False(t, hasProjectRow(t, s, target.ID))
}

// The project total is shared by every creator. With a cap of 1, one
// creator's running agent refuses another creator's create and resume;
// once it stops, the other creator's resume is admitted.
func TestProjectQuota_TotalIsSharedByEveryCreator(t *testing.T) {
	srv, s, project := setupCreateAgentServer(t, &quotaLifecycleDispatcher{})
	setProjectAgentCeiling(t, s, 1)
	broker, err := s.GetRuntimeBroker(context.Background(), project.DefaultRuntimeBrokerID)
	require.NoError(t, err)
	a := newCreatorAgent(t, s, broker, project, "pq-shared-a", state.PhaseRunning, tid("user-a"))
	seedProjectRow(t, s, a)
	b := newCreatorAgent(t, s, broker, project, "pq-shared-b", state.PhaseStopped, tid("user-b"))

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{Name: "pq-shared-new", ProjectID: project.ID})
	assertProjectQuotaExceeded(t, rec, project.ID, 1, 1)

	rec = doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+b.ID+"/start", nil)
	assertProjectQuotaExceeded(t, rec, project.ID, 1, 1)

	rec = doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+a.ID+"/stop", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	rec = doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+b.ID+"/start", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.True(t, hasProjectRow(t, s, b.ID))
}

// The shared total is compared against the creator's own
// resolved cap, so two creators with different caps get different answers
// at the same total. An agent created by another agent is judged on that
// agent's cap (the default here), not on a user's raise.
func TestProjectQuota_CreatorCapDecidesAtSameTotal(t *testing.T) {
	srv, s := testServer(t)
	srv.SetDispatcher(&quotaLifecycleDispatcher{})
	setProjectAgentCeiling(t, s, 5)
	setUserProjectCap(t, s, tid("user-a"), 10)
	broker, project := newQuotaTestBrokerAndProject(t, s, "pq-caps")
	var fillers []*store.Agent
	for _, n := range []string{"f1", "f2", "f3", "f4", "f5"} {
		f := newCreatorAgent(t, s, broker, project, "pq-caps-"+n, state.PhaseRunning, tid("user-c"))
		seedProjectRow(t, s, f)
		fillers = append(fillers, f)
	}
	aAgent := newCreatorAgent(t, s, broker, project, "pq-caps-a", state.PhaseStopped, tid("user-a"))
	bAgent := newCreatorAgent(t, s, broker, project, "pq-caps-b", state.PhaseStopped, tid("user-b"))
	// A sub-agent of an agent of user A: judged on its creator's (the
	// parent agent's) cap, the default, not on A's raise.
	parent := newCreatorAgent(t, s, broker, project, "pq-caps-parent", state.PhaseStopped, tid("user-a"))
	subAgent := newSubAgent(t, s, broker, project, "pq-caps-sub", state.PhaseStopped, parent)

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+bAgent.ID+"/start", nil)
	assertProjectQuotaExceeded(t, rec, project.ID, 5, 5)
	rec = doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+subAgent.ID+"/start", nil)
	assertProjectQuotaExceeded(t, rec, project.ID, 5, 5)

	rec = doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+aAgent.ID+"/start", nil)
	require.Equal(t, http.StatusOK, rec.Code, "creator with a raise is admitted at the same total: %s", rec.Body.String())
	assert.EqualValues(t, 6, projectRowCount(t, s, project.ID))

	// Once the total drops below B's cap, B is admitted.
	for _, f := range fillers[:2] {
		rec = doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+f.ID+"/stop", nil)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	}
	require.EqualValues(t, 4, projectRowCount(t, s, project.ID))
	rec = doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+bAgent.ID+"/start", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
}

// A user who resumes another user's agent is judged on the
// agent creator's cap, not on their own. The dev user (no binding) resumes
// A's agent (A's cap 10) at a total of 5 and is admitted; resuming B's agent
// (B on the default 5) at a total of 6 is refused.
func TestProjectQuota_OtherUserResumeJudgedOnCreatorCap(t *testing.T) {
	srv, s := testServer(t)
	srv.SetDispatcher(&quotaLifecycleDispatcher{})
	setProjectAgentCeiling(t, s, 5)
	setUserProjectCap(t, s, tid("user-a"), 10)
	broker, project := newQuotaTestBrokerAndProject(t, s, "pq-other")
	for _, n := range []string{"f1", "f2", "f3", "f4", "f5"} {
		f := newCreatorAgent(t, s, broker, project, "pq-other-"+n, state.PhaseRunning, tid("user-c"))
		seedProjectRow(t, s, f)
	}
	aAgent := newCreatorAgent(t, s, broker, project, "pq-other-a", state.PhaseStopped, tid("user-a"))
	bAgent := newCreatorAgent(t, s, broker, project, "pq-other-b", state.PhaseStopped, tid("user-b"))

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+aAgent.ID+"/start", nil)
	require.Equal(t, http.StatusOK, rec.Code, "judged on A's cap: %s", rec.Body.String())

	rec = doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+bAgent.ID+"/start", nil)
	assertProjectQuotaExceeded(t, rec, project.ID, 6, 5)
}

// An agent whose creator has no cap is still recorded, so it
// counts in the shared total a capped creator is checked against.
func TestProjectQuota_UncappedCreatorStillCounted(t *testing.T) {
	srv, s := testServer(t)
	srv.SetDispatcher(&quotaLifecycleDispatcher{})
	// The definition default stays 0 (unlimited): only user B is capped.
	setUserProjectCap(t, s, tid("user-b"), 2)
	broker, project := newQuotaTestBrokerAndProject(t, s, "pq-uncapped")
	for _, n := range []string{"a1", "a2"} {
		a := newCreatorAgent(t, s, broker, project, "pq-uncapped-"+n, state.PhaseStopped, tid("user-a"))
		rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+a.ID+"/start", nil)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		assert.True(t, hasProjectRow(t, s, a.ID), "an uncapped creator's agent is recorded")
	}
	b := newCreatorAgent(t, s, broker, project, "pq-uncapped-b", state.PhaseStopped, tid("user-b"))
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+b.ID+"/start", nil)
	assertProjectQuotaExceeded(t, rec, project.ID, 2, 2)
}

// Rollback ordering in beginStartDispatch: a failed start rolls back both
// slots; a full broker answers with the unchanged broker message and takes
// no project slot; a broker reservation taken before a project refusal is
// released.
func TestProjectQuota_StartRollbackAndOrder(t *testing.T) {
	t.Run("failed-start-rolls-back-both", func(t *testing.T) {
		srv, s := testServer(t)
		srv.SetDispatcher(&notSentStartDispatcher{})
		broker, project := newQuotaTestBrokerAndProject(t, s, "pq-rb-fail")
		a := newCreatorAgent(t, s, broker, project, "pq-rb-fail", state.PhaseStopped, tid("user-a"))
		rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+a.ID+"/start", nil)
		require.GreaterOrEqual(t, rec.Code, 500, rec.Body.String())
		assert.False(t, hasReservation(t, s, store.LimitMaxAgentsPerBroker, a.ID))
		assert.False(t, hasProjectRow(t, s, a.ID))
	})
	t.Run("broker-full-keeps-broker-message", func(t *testing.T) {
		srv, s := testServer(t)
		srv.SetDispatcher(&quotaLifecycleDispatcher{})
		setBrokerAgentCeiling(t, s, 1)
		setProjectAgentCeiling(t, s, 5)
		broker, project := newQuotaTestBrokerAndProject(t, s, "pq-rb-broker")
		occ := newCreatorAgent(t, s, broker, project, "pq-rb-broker-occ", state.PhaseRunning, tid("user-a"))
		reserveBrokerSlot(t, s, broker, occ.ID)
		a := newCreatorAgent(t, s, broker, project, "pq-rb-broker", state.PhaseStopped, tid("user-a"))
		rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+a.ID+"/start", nil)
		assertBrokerQuotaExceeded(t, rec)
		assert.False(t, hasProjectRow(t, s, a.ID), "the broker refusal comes first; no project slot is taken")
	})
	t.Run("project-refusal-releases-broker", func(t *testing.T) {
		srv, s := testServer(t)
		srv.SetDispatcher(&quotaLifecycleDispatcher{})
		setBrokerAgentCeiling(t, s, 5)
		setProjectAgentCeiling(t, s, 1)
		broker, project := newQuotaTestBrokerAndProject(t, s, "pq-rb-project")
		occ := newCreatorAgent(t, s, broker, project, "pq-rb-project-occ", state.PhaseRunning, tid("user-a"))
		seedProjectRow(t, s, occ)
		a := newCreatorAgent(t, s, broker, project, "pq-rb-project", state.PhaseStopped, tid("user-a"))
		rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+a.ID+"/start", nil)
		assertProjectQuotaExceeded(t, rec, project.ID, 1, 1)
		assert.False(t, hasReservation(t, s, store.LimitMaxAgentsPerBroker, a.ID), "the broker slot taken first is released")
	})
}

// A restart of a stopped agent whose start leg has an unknown
// outcome keeps the project row its hold created, as it keeps the broker
// row; the reaper's release then frees both once nothing is seen running.
func TestProjectQuota_AmbiguousRestartKeepsProjectRow(t *testing.T) {
	f, d, _ := newClaimFixture(t)
	noObservationLag(t)
	setBrokerAgentCeiling(t, f.s, 5)
	setProjectAgentCeiling(t, f.s, 5)
	a := restartAgentIn(t, f, "pq-amb", state.PhaseStopped, false)
	failStartAmbiguously(d)

	code, _ := lifecycle(t, f, a.ID, "restart")
	assertRestartHeld(t, f, code, a.ID, "the hold's broker reservation outlives an ambiguous start")
	assert.True(t, hasProjectRow(t, f.s, a.ID), "the hold's project reservation outlives an ambiguous start")

	f.heartbeat(completeInventory())
	f.srv.reapStartClaims(context.Background())
	assert.Empty(t, getAgent(t, f.s, a.ID).StartClaimID, "the reaper released the claim")
	assert.False(t, hasProjectRow(t, f.s, a.ID), "the project slot frees at the reaper's release")
}

// --- Idle releases at the claim-end sites -----------------------------------

// Every claim-end site frees an idle agent's project slot, beside the
// broker slot: the launch report, the reaper's unconfirmed release, the
// reaper's release on a deregistered broker, and claim finish.
func TestProjectQuota_IdleReleaseAtClaimEndSites(t *testing.T) {
	t.Run("launch-report", func(t *testing.T) {
		f := newReconcileFixture(t)
		a, launchID := launchClaimedAgent(t, f, "pq-le")
		seedProjectRow(t, f.s, a)
		selfReport(t, f.srv, a, state.PhaseStopped)
		require.True(t, hasProjectRow(t, f.s, a.ID), "skipped under the create claim")
		rec := postLaunchReport(t, f.srv, f.brokerID, a.ID, f.brokerID, AgentLaunchReport{
			LaunchID: launchID, InstanceID: "i1", Seq: 1, State: store.LaunchReportStateFailed, ErrorCode: "runtime_error",
		})
		require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
		assert.False(t, hasProjectRow(t, f.s, a.ID), "freed right after the report")
	})
	t.Run("reaper-unconfirmed", func(t *testing.T) {
		f, _, a := newClaimFixture(t) // a is stopped
		noObservationLag(t)
		ctx := context.Background()
		seedProjectRow(t, f.s, a)
		_, err := f.s.ClaimAgentStart(ctx, a.ID, "dead-hub", store.StartClaimRestart, "", time.Millisecond)
		require.NoError(t, err)
		time.Sleep(5 * time.Millisecond)
		f.srv.reapStartClaims(ctx)
		assert.True(t, hasProjectRow(t, f.s, a.ID), "held while the claim is held")
		f.heartbeat(completeInventory())
		f.srv.reapStartClaims(ctx)
		assert.Empty(t, getAgent(t, f.s, a.ID).StartClaimID)
		assert.False(t, hasProjectRow(t, f.s, a.ID), "freed once the claim was released")
	})
	t.Run("reaper-ended-launch", func(t *testing.T) {
		f := newReconcileFixture(t)
		noObservationLag(t)
		ctx := context.Background()
		a, _ := launchClaimedAgent(t, f, "pq-ended")
		seedProjectRow(t, f.s, a)
		selfReport(t, f.srv, a, state.PhaseStopped)
		params := store.ReaperParams{KeepaliveInterval: time.Millisecond, ReaperInterval: time.Minute}
		_, err := f.s.RunLaunchReaperTick(ctx, params)
		require.NoError(t, err)
		time.Sleep(30 * time.Millisecond)
		_, err = f.s.RunLaunchReaperTick(ctx, params)
		require.NoError(t, err)
		require.True(t, hasProjectRow(t, f.s, a.ID), "held right after the launch reaper's write")
		f.heartbeat(completeInventory())
		f.srv.reapStartClaims(ctx)
		assert.Empty(t, getAgent(t, f.s, a.ID).StartClaimID)
		assert.False(t, hasProjectRow(t, f.s, a.ID), "freed at the reaper's release")
	})
	t.Run("reaper-deregistered-broker", func(t *testing.T) {
		f, d, _ := newClaimFixture(t)
		ctx := context.Background()
		a := restartAgentIn(t, f, "pq-dereg", state.PhaseStopped, false)
		failStartAmbiguously(d)
		code, _ := lifecycle(t, f, a.ID, "restart")
		require.GreaterOrEqual(t, code, 500)
		require.True(t, hasProjectRow(t, f.s, a.ID), "the hold's project row outlives the ambiguous start")
		require.NoError(t, f.s.DeleteRuntimeBroker(ctx, f.brokerID))
		f.srv.reapStartClaims(ctx)
		require.Empty(t, getAgent(t, f.s, a.ID).StartClaimID)
		assert.False(t, hasProjectRow(t, f.s, a.ID), "freed at the deregistered-broker release")
	})
	t.Run("claim-finish-running-keeps", func(t *testing.T) {
		srv, s := testServer(t)
		broker, project := newQuotaTestBrokerAndProject(t, s, "pq-finish-run")
		a := newCreatorAgent(t, s, broker, project, "pq-finish-run", state.PhaseRunning, tid("user-a"))
		seedProjectRow(t, s, a)
		run, err := srv.acquireStartClaim(context.Background(), a, store.StartClaimUser)
		require.NoError(t, err)
		run.finish(startReleased)
		assert.True(t, hasProjectRow(t, s, a.ID), "a running agent keeps its slot")
	})
}

// The idle release keeps a stop_queued agent's
// project row (NotCounted). The row is seeded before the claim, and the
// claim ends with a released outcome, so the claim-finish release runs;
// control C, the same row without stop_queued, is released by it.
func TestProjectQuota_IdleReleaseKeepsStopQueuedAgent(t *testing.T) {
	for _, stopQueued := range []bool{true, false} {
		name := "K-stop-queued-kept"
		if !stopQueued {
			name = "C-control-released"
		}
		t.Run(name, func(t *testing.T) {
			srv, s := testServer(t)
			ctx := context.Background()
			broker, project := newQuotaTestBrokerAndProject(t, s, "pq-sq-"+name)
			a := newCreatorAgent(t, s, broker, project, "pq-sq-"+name, state.PhaseStopped, tid("user-a"))
			if stopQueued {
				require.NoError(t, s.UpdateAgentStatus(ctx, a.ID, store.AgentStatusUpdate{ContainerStatus: containerStatusStopQueued}))
			}
			seedProjectRow(t, s, a)

			run, err := srv.acquireStartClaim(ctx, getAgent(t, s, a.ID), store.StartClaimUser)
			require.NoError(t, err)
			run.finish(startReleased)

			require.Empty(t, getAgent(t, s, a.ID).StartClaimID, "the claim was released")
			if stopQueued {
				assert.True(t, hasProjectRow(t, s, a.ID), "a stop_queued agent keeps its project slot")
			} else {
				assert.False(t, hasProjectRow(t, s, a.ID), "control: the idle release ran and freed the slot")
			}
		})
	}
}

// --- Restart backstops -------------------------------------------------------

// A project row released by an unguarded path during a restart is restored
// by the re-assert after the stop leg (start_dispatch reassertReservation)
// or after the final write, and each write is audited.
func TestProjectQuota_RestartBackstopsRestoreRow(t *testing.T) {
	t.Run("after-stop-leg", func(t *testing.T) {
		srv, s := testServer(t)
		disp := &hookedStartDispatcher{}
		srv.SetDispatcher(disp)
		broker, project := newQuotaTestBrokerAndProject(t, s, "pq-bs-stop")
		a := newCreatorAgent(t, s, broker, project, "pq-bs-stop", state.PhaseRunning, tid("user-a"))
		seedSlot(t, s, broker.ID, a.ID)
		seedProjectRow(t, s, a)
		var heldAtStart bool
		disp.onStop = func(*store.Agent) {
			srv.quotaService.Release(context.Background(), store.LimitMaxAgentsPerProject, a.ID)
		}
		disp.onStart = func(*store.Agent) { heldAtStart = hasProjectRow(t, s, a.ID) }
		rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+a.ID+"/restart", nil)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		assert.True(t, heldAtStart, "re-asserted after the stop leg, before the start leg")
		assert.NotEmpty(t, capacityAudits(t, s, auditQuotaProjectBackstopReassert, a.ID))
	})
	t.Run("after-final-write", func(t *testing.T) {
		srv, s := testServer(t)
		disp := &hookedStartDispatcher{}
		srv.SetDispatcher(disp)
		broker, project := newQuotaTestBrokerAndProject(t, s, "pq-bs-final")
		a := newCreatorAgent(t, s, broker, project, "pq-bs-final", state.PhaseRunning, tid("user-a"))
		seedSlot(t, s, broker.ID, a.ID)
		seedProjectRow(t, s, a)
		disp.onStart = func(*store.Agent) {
			srv.quotaService.Release(context.Background(), store.LimitMaxAgentsPerProject, a.ID)
		}
		rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+a.ID+"/restart", nil)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		assert.True(t, hasProjectRow(t, s, a.ID), "re-asserted after the final write")
		audits := capacityAudits(t, s, auditQuotaProjectBackstopReassert, a.ID)
		require.Len(t, audits, 1)
		assert.Contains(t, audits[0].AfterSummary, backstopSiteRestart)
	})
}

// --- Sweep and backfill ------------------------------------------------------

// The project pass releases a stale idle row and a missing agent's row,
// keeps a young idle row (the grace period) and a running agent's row, and
// backfills every capacity-holding agent across every page, hub-direct
// agents included, without the cap check.
func TestProjectQuota_ReconcileReleasesAndBackfills(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	setProjectAgentCeiling(t, s, 1)
	old := projectBackfillPageSize
	projectBackfillPageSize = 2
	t.Cleanup(func() { projectBackfillPageSize = old })

	broker, project := newQuotaTestBrokerAndProject(t, s, "pq-sweep")
	staleIdle := newCreatorAgent(t, s, broker, project, "pq-sweep-stale", state.PhaseStopped, tid("user-a"))
	seedProjectRowAt(t, s, staleIdle, time.Now().Add(-2*reconcileMinReservationAge))
	youngIdle := newCreatorAgent(t, s, broker, project, "pq-sweep-young", state.PhaseStopped, tid("user-a"))
	seedProjectRow(t, s, youngIdle)
	running := newCreatorAgent(t, s, broker, project, "pq-sweep-running", state.PhaseRunning, tid("user-a"))
	seedProjectRowAt(t, s, running, time.Now().Add(-2*reconcileMinReservationAge))
	ghost := &store.Agent{ID: tid("agent-pq-sweep-ghost"), CreatedBy: tid("user-a"), ProjectID: project.ID}
	seedProjectRow(t, s, ghost)

	// Five capacity-holding agents with no row (more than two pages), one
	// of them hub-direct, plus an idle one that must not be backfilled.
	var missing []*store.Agent
	for _, n := range []string{"m1", "m2", "m3", "m4"} {
		missing = append(missing, newCreatorAgent(t, s, broker, project, "pq-sweep-"+n, state.PhaseRunning, tid("user-b")))
	}
	hubDirect := &store.Agent{
		ID: tid("agent-pq-sweep-direct"), Slug: "pq-sweep-direct", Name: "pq-sweep-direct",
		ProjectID: project.ID, Phase: string(state.PhaseRunning), CreatedBy: tid("user-b"),
	}
	require.NoError(t, s.CreateAgent(ctx, hubDirect))
	missing = append(missing, hubDirect)
	idleNoRow := newCreatorAgent(t, s, broker, project, "pq-sweep-idle", state.PhaseSuspended, tid("user-b"))

	srv.ReconcileStaleProjectQuotaReservations(ctx)

	assert.False(t, hasProjectRow(t, s, staleIdle.ID), "a stale idle row is released")
	assert.True(t, hasProjectRow(t, s, youngIdle.ID), "a young idle row is kept (grace)")
	assert.True(t, hasProjectRow(t, s, running.ID), "a running agent's row is kept")
	assert.False(t, hasProjectRow(t, s, ghost.ID), "a missing agent's row is released")
	for _, m := range missing {
		assert.True(t, hasProjectRow(t, s, m.ID), "backfilled without the cap check: %s", m.Slug)
	}
	assert.False(t, hasProjectRow(t, s, idleNoRow.ID), "an idle agent is not backfilled")
}

// The scheduled reconcile runs the project pass after the broker pass.
func TestProjectQuota_ScheduledReconcileRunsProjectPass(t *testing.T) {
	srv, s := testServer(t)
	broker, project := newQuotaTestBrokerAndProject(t, s, "pq-sched")
	a := newCreatorAgent(t, s, broker, project, "pq-sched", state.PhaseRunning, tid("user-a"))
	srv.reconcileQuotaReservations(context.Background())
	assert.True(t, hasProjectRow(t, s, a.ID), "the project pass backfilled the running agent")
}

// --- Admin usage -------------------------------------------------------------

// The admin usage summary and detail count project reservations at their
// real scope.
func TestProjectQuota_AdminUsageShowsProjectRows(t *testing.T) {
	srv, s := testServer(t)
	broker, project := newQuotaTestBrokerAndProject(t, s, "pq-admin")
	for _, n := range []string{"a", "b"} {
		a := newCreatorAgent(t, s, broker, project, "pq-admin-"+n, state.PhaseRunning, tid("user-a"))
		seedProjectRow(t, s, a)
	}
	defID := projectLimitDefID(t, s)

	rec := doRequest(t, srv, http.MethodGet, "/api/v1/admin/usage", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var summary usageSummaryResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &summary))
	found := false
	for _, e := range summary.Items {
		if e.LimitDefinition.ID == defID {
			found = true
			assert.Equal(t, 2, e.ActiveCount)
		}
	}
	require.True(t, found, "the project limit is in the summary")

	rec = doRequest(t, srv, http.MethodGet, "/api/v1/admin/usage/"+defID, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var detail usageByLimitResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &detail))
	assert.Equal(t, 2, detail.TotalActive)
	assert.Len(t, detail.Reservations, 2)
}

// An agent with no recorded creator still gets a project reservation (a
// reservation needs a subject; it is recorded under the project ID), so its
// start is admitted and counted, with and without a project cap.
func TestProjectQuota_AgentWithoutCreatorIsRecorded(t *testing.T) {
	for _, capped := range []bool{false, true} {
		name := "unlimited"
		if capped {
			name = "capped"
		}
		t.Run(name, func(t *testing.T) {
			srv, s := testServer(t)
			disp := &quotaLifecycleDispatcher{}
			srv.SetDispatcher(disp)
			if capped {
				setProjectAgentCeiling(t, s, 2)
			}
			broker, project := newQuotaTestBrokerAndProject(t, s, "pq-nocreator-"+name)
			a := newQuotaTestAgent(t, s, broker, project, "pq-nocreator-"+name, state.PhaseStopped)
			require.Empty(t, a.CreatedBy)

			rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+a.ID+"/start", nil)
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			assert.EqualValues(t, 1, disp.startCount.Load())
			held := activeReservation(t, s, projectLimitDefID(t, s), a.ID)
			require.NotNil(t, held, "the start recorded a project reservation")
			assert.Equal(t, project.ID, held.SubjectID, "recorded under the project ID")
		})
	}
}

// The empty-subject case: a project reservation requested with no subject
// (an agent with no recorded creator) is recorded under the project ID when
// a project cap is set. A create attributed to no creator is refused later by
// the agent create write (its delegation edge needs a creator), so this is
// pinned at the QuotaService; TestProjectQuota_AgentWithoutCreatorIsRecorded
// pins the start path.
func TestProjectQuota_ReserveWithoutCreatorRecordedUnderProject(t *testing.T) {
	t.Run("reserve", func(t *testing.T) {
		srv, s := testServer(t)
		setProjectAgentCeiling(t, s, 2)
		projectID := tid("proj-nocreator-reserve")
		created, err := srv.quotaService.Reserve(context.Background(), store.LimitMaxAgentsPerProject, "", store.QuotaScopeProject, projectID, tid("agent-nocreator-reserve"))
		require.NoError(t, err)
		assert.True(t, created)
		held := activeReservation(t, s, projectLimitDefID(t, s), tid("agent-nocreator-reserve"))
		require.NotNil(t, held)
		assert.Equal(t, projectID, held.SubjectID)
	})
}

// The project total counts only that project's active reservations, every
// creator included: rows of another project and a released row do not count.
func TestProjectQuota_CountIsPerProject(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	setProjectAgentCeiling(t, s, 2)
	defID := projectLimitDefID(t, s)
	projectP, projectQ := tid("proj-count-p"), tid("proj-count-q")
	record := func(project, subject, resource string) {
		t.Helper()
		_, err := s.CreateUsageReservation(ctx, &store.UsageReservation{
			LimitDefinitionID: defID, SubjectID: subject, ScopeType: store.QuotaScopeProject,
			ScopeID: project, ResourceID: resource, Reserved: 1,
		})
		require.NoError(t, err)
	}
	// Project P: one active row of creator A, and one released row of creator B.
	record(projectP, tid("user-a"), tid("agent-count-p1"))
	record(projectP, tid("user-b"), tid("agent-count-p2"))
	require.NoError(t, s.ReleaseReservation(ctx, defID, tid("agent-count-p2")))
	// Project Q: three active rows, over P's cap on their own.
	for _, r := range []string{"q1", "q2", "q3"} {
		record(projectQ, tid("user-q"), tid("agent-count-"+r))
	}

	created, err := srv.quotaService.Reserve(ctx, store.LimitMaxAgentsPerProject, tid("user-c"), store.QuotaScopeProject, projectP, tid("agent-count-p3"))
	require.NoError(t, err, "P holds one active row: Q's rows and P's released row do not count")
	assert.True(t, created)

	_, err = srv.quotaService.Reserve(ctx, store.LimitMaxAgentsPerProject, tid("user-d"), store.QuotaScopeProject, projectP, tid("agent-count-p4"))
	var exceeded *QuotaExceededError
	require.ErrorAs(t, err, &exceeded, "P is now full with two active rows of two creators")
	assert.EqualValues(t, 2, exceeded.Count)
	assert.EqualValues(t, 2, exceeded.Max)
	assert.Equal(t, projectP, exceeded.ScopeID)
}

// An exec answered agent_not_found marks a running agent's container missing
// (phase error) and frees its project slot with the broker slot.
func TestProjectQuota_ExecAgentNotFoundFreesSlot(t *testing.T) {
	srv, s := testServer(t)
	broker, project := newQuotaTestBrokerAndProject(t, s, "pq-exec-missing")
	a := newCreatorAgent(t, s, broker, project, "pq-exec-missing", state.PhaseRunning, tid("user-a"))
	seedSlot(t, s, broker.ID, a.ID)
	seedProjectRow(t, s, a)

	srv.reconcileExecAgentNotFound(context.Background(), getAgent(t, s, a.ID), time.Now())

	require.Equal(t, string(state.PhaseError), getAgent(t, s, a.ID).Phase, "marked missing")
	assert.False(t, hasReservation(t, s, store.LimitMaxAgentsPerBroker, a.ID))
	assert.False(t, hasProjectRow(t, s, a.ID), "the project slot is freed with the broker slot")
}
