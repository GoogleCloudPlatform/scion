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
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Agent callers manage only the schedules and scheduled events whose
// current initiator principal is the calling agent. User callers and
// create are covered for comparison.

// agentScopeForbiddenBody is the body of a row-level denial.
const agentScopeForbiddenBody = `{"error":{"code":"forbidden","message":"Insufficient permissions"}}`

// agentScopeHolder names whose authority a fixture row is held under.
type agentScopeHolder string

const (
	holderCallerAgent agentScopeHolder = "caller agent"
	holderOtherAgent  agentScopeHolder = "other agent"
	holderUser        agentScopeHolder = "user"
	holderLegacy      agentScopeHolder = "legacy unknown"
)

var agentScopeHolders = []agentScopeHolder{holderCallerAgent, holderOtherAgent, holderUser, holderLegacy}

// attribution is the current initiator attribution of a row held by h.
// The legacy row carries a CreatedBy naming the caller agent elsewhere, so
// a decision that read CreatedBy would admit it.
func (h agentScopeHolder) attribution() store.InitiatorAttribution {
	switch h {
	case holderCallerAgent:
		return store.InitiatorAttribution{
			InitiatorPrincipalKind:  store.DelegationPrincipalAgent,
			InitiatorPrincipalID:    authzHelperAgentID,
			InitiatorCredentialKind: store.InitiatorCredentialKindAgent,
			AttributionVersion:      1,
			AuthorizationRevision:   1,
		}
	case holderOtherAgent:
		return store.InitiatorAttribution{
			InitiatorPrincipalKind:  store.DelegationPrincipalAgent,
			InitiatorPrincipalID:    tid("agent-scope-other-agent"),
			InitiatorCredentialKind: store.InitiatorCredentialKindAgent,
			AttributionVersion:      1,
			AuthorizationRevision:   1,
		}
	case holderUser:
		return store.InitiatorAttribution{
			InitiatorPrincipalKind:  store.DelegationPrincipalUser,
			InitiatorPrincipalID:    tid("agent-scope-user"),
			InitiatorCredentialKind: store.InitiatorCredentialKindSession,
			AttributionVersion:      1,
			AuthorizationRevision:   1,
		}
	default:
		return store.InitiatorAttribution{}
	}
}

// agentScopeCaller is the agent identity the tests act as. It holds the
// rows attributed to holderCallerAgent.
func agentScopeCaller(projectID string) AgentIdentity {
	return authzHelperAgent(projectID, ScopeProjectRead, ScopeAgentCreate)
}

// storeAgentScopeSchedule stores a dispatch_agent schedule held by h with
// the given status, and returns it. CreatedBy always names the caller
// agent, so only the current attribution separates the rows.
func storeAgentScopeSchedule(t *testing.T, s store.Store, projectID, name string, h agentScopeHolder, status string) *store.Schedule {
	t.Helper()
	next := time.Now().UTC().Add(time.Hour)
	sc := &store.Schedule{
		ID:                   api.NewUUID(),
		ProjectID:            projectID,
		Name:                 name,
		CronExpr:             "0 * * * *",
		EventType:            "dispatch_agent",
		Payload:              `{"agentName":"agent-scope-worker"}`,
		Status:               status,
		NextRunAt:            &next,
		CreatedBy:            authzHelperAgentID,
		InitiatorAttribution: h.attribution(),
	}
	require.NoError(t, s.CreateSchedule(context.Background(), sc))
	return sc
}

// storeAgentScopeEvent stores a pending scheduled event held by h.
func storeAgentScopeEvent(t *testing.T, s store.Store, projectID string, h agentScopeHolder) *store.ScheduledEvent {
	t.Helper()
	evt := &store.ScheduledEvent{
		ID:                   api.NewUUID(),
		ProjectID:            projectID,
		EventType:            "message",
		FireAt:               time.Now().UTC().Add(time.Hour),
		Payload:              `{"agentName":"agent-scope-worker","message":"hi"}`,
		Status:               store.ScheduledEventPending,
		CreatedBy:            authzHelperAgentID,
		InitiatorAttribution: h.attribution(),
	}
	require.NoError(t, s.CreateScheduledEvent(context.Background(), evt))
	return evt
}

// doAgentScopeEventRequest sends a request to the scheduled-events route.
func doAgentScopeEventRequest(t *testing.T, srv *Server, identity Identity, projectID, eventPath, method string) *httptest.ResponseRecorder {
	t.Helper()
	path := "/api/v1/projects/" + projectID + "/scheduled-events"
	if eventPath != "" {
		path += "/" + eventPath
	}
	req := authoredRequest(t, identity, method, path, nil)
	rec := httptest.NewRecorder()
	srv.handleScheduledEvents(rec, req, projectID, eventPath)
	return rec
}

// agentScopeScheduleAction is one per-ID schedule action.
type agentScopeScheduleAction struct {
	name    string
	suffix  string
	method  string
	status  string // row status the action needs to succeed
	allowed int    // status code when the caller holds the row
}

var agentScopeScheduleActions = []agentScopeScheduleAction{
	{"read", "", http.MethodGet, store.ScheduleStatusActive, http.StatusOK},
	{"history", "/history", http.MethodGet, store.ScheduleStatusActive, http.StatusOK},
	{"pause", "/pause", http.MethodPost, store.ScheduleStatusActive, http.StatusOK},
	{"resume", "/resume", http.MethodPost, store.ScheduleStatusPaused, http.StatusOK},
	{"delete", "", http.MethodDelete, store.ScheduleStatusActive, http.StatusNoContent},
}

// Every per-ID schedule action is allowed for an agent on a row it holds,
// and denied with 403 on a row held by another agent, a user, or no one
// recorded (legacy unknown). A denied action changes nothing.
func TestAgentScheduleScope_PerIDActions(t *testing.T) {
	srv, s, projectID := setupScheduleTest(t)
	caller := agentScopeCaller(projectID)

	for _, action := range agentScopeScheduleActions {
		for _, h := range agentScopeHolders {
			t.Run(action.name+"/"+string(h), func(t *testing.T) {
				sc := storeAgentScopeSchedule(t, s, projectID, "scope-"+action.name, h, action.status)
				before, err := s.GetSchedule(context.Background(), sc.ID)
				require.NoError(t, err)

				rec := doAuthoredScheduleRequest(t, srv, caller, projectID, sc.ID+action.suffix, action.method, nil)
				if h == holderCallerAgent {
					require.Equal(t, action.allowed, rec.Code, rec.Body.String())
					return
				}
				require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
				assert.JSONEq(t, agentScopeForbiddenBody, rec.Body.String())
				after, err := s.GetSchedule(context.Background(), sc.ID)
				require.NoError(t, err, "a denied action leaves the row in place")
				assert.Equal(t, before.Status, after.Status)
				assert.Equal(t, before.InitiatorAttribution, after.InitiatorAttribution)
			})
		}
	}
}

// Every per-ID scheduled-event action (read, cancel) is allowed for an
// agent on an event it holds and denied with 403 otherwise.
func TestAgentScheduledEventScope_PerIDActions(t *testing.T) {
	srv, s, projectID := setupScheduledEventTest(t)
	caller := agentScopeCaller(projectID)

	actions := []struct {
		name    string
		method  string
		allowed int
	}{
		{"read", http.MethodGet, http.StatusOK},
		{"cancel", http.MethodDelete, http.StatusNoContent},
	}
	for _, action := range actions {
		for _, h := range agentScopeHolders {
			t.Run(action.name+"/"+string(h), func(t *testing.T) {
				evt := storeAgentScopeEvent(t, s, projectID, h)
				rec := doAgentScopeEventRequest(t, srv, caller, projectID, evt.ID, action.method)
				if h == holderCallerAgent {
					require.Equal(t, action.allowed, rec.Code, rec.Body.String())
					return
				}
				require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
				assert.JSONEq(t, agentScopeForbiddenBody, rec.Body.String())
				after, err := s.GetScheduledEvent(context.Background(), evt.ID)
				require.NoError(t, err)
				assert.Equal(t, store.ScheduledEventPending, after.Status, "a denied cancel leaves the event pending")
			})
		}
	}
}

// An agent's schedule list holds only the rows it holds, and omits the
// total count; a user's list is every row with its count.
func TestAgentScheduleScope_List(t *testing.T) {
	srv, s, projectID := setupScheduleTest(t)
	ids := map[agentScopeHolder]string{}
	for _, h := range agentScopeHolders {
		ids[h] = storeAgentScopeSchedule(t, s, projectID, "scope-list-"+string(h), h, store.ScheduleStatusActive).ID
	}

	rec := doAuthoredScheduleRequest(t, srv, agentScopeCaller(projectID), projectID, "", http.MethodGet, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var resp ListSchedulesResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	require.Len(t, resp.Schedules, 1)
	assert.Equal(t, ids[holderCallerAgent], resp.Schedules[0].ID)
	assert.Zero(t, resp.TotalCount)

	rec = doRequest(t, srv, http.MethodGet, "/api/v1/projects/"+projectID+"/schedules", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	resp = ListSchedulesResponse{}
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	assert.Len(t, resp.Schedules, len(agentScopeHolders))
	assert.Equal(t, len(agentScopeHolders), resp.TotalCount)
}

// An agent's scheduled-event list holds only the events it holds; a
// user's list is every event.
func TestAgentScheduledEventScope_List(t *testing.T) {
	srv, s, projectID := setupScheduledEventTest(t)
	ids := map[agentScopeHolder]string{}
	for _, h := range agentScopeHolders {
		ids[h] = storeAgentScopeEvent(t, s, projectID, h).ID
	}

	rec := doAgentScopeEventRequest(t, srv, agentScopeCaller(projectID), projectID, "", http.MethodGet)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var resp ListScheduledEventsResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	require.Len(t, resp.Events, 1)
	assert.Equal(t, ids[holderCallerAgent], resp.Events[0].ID)
	assert.Zero(t, resp.TotalCount)

	rec = doRequest(t, srv, http.MethodGet, "/api/v1/projects/"+projectID+"/scheduled-events", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	resp = ListScheduledEventsResponse{}
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	assert.Len(t, resp.Events, len(agentScopeHolders))
}

// A user caller reads, pauses and deletes rows whatever their holder.
func TestAgentScheduleScope_UserCallerAnyHolder(t *testing.T) {
	srv, s, projectID := setupScheduleTest(t)
	for _, h := range agentScopeHolders {
		t.Run(string(h), func(t *testing.T) {
			sc := storeAgentScopeSchedule(t, s, projectID, "scope-user-"+string(h), h, store.ScheduleStatusActive)
			base := "/api/v1/projects/" + projectID + "/schedules/" + sc.ID
			rec := doRequest(t, srv, http.MethodGet, base, nil)
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			rec = doRequest(t, srv, http.MethodGet, base+"/history", nil)
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			rec = doRequest(t, srv, http.MethodPost, base+"/pause", nil)
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			rec = doRequest(t, srv, http.MethodPatch, base, UpdateScheduleRequest{Name: "scope-user-renamed"})
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			rec = doRequest(t, srv, http.MethodDelete, base, nil)
			require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())

			evt := storeAgentScopeEvent(t, s, projectID, h)
			rec = doRequest(t, srv, http.MethodGet, "/api/v1/projects/"+projectID+"/scheduled-events/"+evt.ID, nil)
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			rec = doRequest(t, srv, http.MethodDelete, "/api/v1/projects/"+projectID+"/scheduled-events/"+evt.ID, nil)
			require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
		})
	}
}

// PATCH of a schedule definition is denied to agent callers, including on
// a row the agent holds, and the row is unchanged.
func TestAgentScheduleScope_PatchDeniedOnHeldRow(t *testing.T) {
	srv, s, projectID := setupScheduleTest(t)
	sc := storeAgentScopeSchedule(t, s, projectID, "scope-patch", holderCallerAgent, store.ScheduleStatusActive)

	rec := doAuthoredScheduleRequest(t, srv, agentScopeCaller(projectID), projectID, sc.ID, http.MethodPatch,
		UpdateScheduleRequest{Name: "scope-patch-renamed"})
	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	assert.JSONEq(t, agentScopeForbiddenBody, rec.Body.String())
	after, err := s.GetSchedule(context.Background(), sc.ID)
	require.NoError(t, err)
	assert.Equal(t, "scope-patch", after.Name)
	assert.Equal(t, sc.InitiatorAttribution, after.InitiatorAttribution)
}

// Create is unchanged for agent callers: an agent creates a schedule and a
// scheduled event, holds both, and sees them in its lists.
func TestAgentScheduleScope_CreateThenHeld(t *testing.T) {
	srv, s, projectID := setupScheduleTest(t)
	caller := agentScopeCaller(projectID)
	storeAgentScopeSchedule(t, s, projectID, "scope-create-user-row", holderUser, store.ScheduleStatusActive)

	id := createOwnerSchedule(t, srv, caller, projectID, "scope-create", "dispatch_agent")
	rec := doAuthoredScheduleRequest(t, srv, caller, projectID, id, http.MethodGet, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	rec = doAuthoredScheduleRequest(t, srv, caller, projectID, "", http.MethodGet, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var list ListSchedulesResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&list))
	require.Len(t, list.Schedules, 1)
	assert.Equal(t, id, list.Schedules[0].ID)

	rec = doAuthoredEventRequest(t, srv, caller, projectID,
		CreateScheduledEventRequest{EventType: "dispatch_agent", FireIn: "1h", AgentName: "scope-one-shot"})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	var evt store.ScheduledEvent
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&evt))
	rec = doAgentScopeEventRequest(t, srv, caller, projectID, evt.ID, http.MethodGet)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
}

// Resuming a held dispatch_agent schedule still runs the agent-create
// reauthorization: without agent:create the holder is refused by that
// gate and the revision is unchanged; with it the resume succeeds.
func TestAgentScheduleScope_ResumeKeepsAgentCreateGate(t *testing.T) {
	srv, s, projectID := setupScheduleTest(t)
	sc := storeAgentScopeSchedule(t, s, projectID, "scope-resume-da", holderCallerAgent, store.ScheduleStatusPaused)
	before := loadScheduleRevision(t, s, sc.ID)

	rec := doAuthoredScheduleRequest(t, srv, authzHelperAgent(projectID, ScopeProjectRead), projectID, sc.ID+"/resume", http.MethodPost, nil)
	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), string(ScopeAgentCreate), "the denial is the resume's agent-create reauthorization")
	assert.Equal(t, before, loadScheduleRevision(t, s, sc.ID))

	rec = doAuthoredScheduleRequest(t, srv, agentScopeCaller(projectID), projectID, sc.ID+"/resume", http.MethodPost, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	after := loadScheduleRevision(t, s, sc.ID)
	assert.Equal(t, authzHelperAgentID, after.Attribution.InitiatorPrincipalID)
	assert.Equal(t, before.Attribution.AuthorizationRevision+1, after.Attribution.AuthorizationRevision)
}

// Resuming a held message schedule still runs the scheduled-message
// reauthorization: a target in another project is refused by that gate
// while cross-project messaging is off, and the revision is unchanged.
func TestAgentScheduleScope_ResumeKeepsMessageGate(t *testing.T) {
	srv, s, projectID := setupScheduleTest(t)
	ctx := context.Background()
	other := &store.Project{ID: tid("agent-scope-other-project"), Name: "Agent Scope Other", Slug: "agent-scope-other"}
	require.NoError(t, s.CreateProject(ctx, other))
	target := &store.Agent{ID: tid("agent-scope-xp-target"), Slug: "agent-scope-xp-target", Name: "agent-scope-xp-target", ProjectID: other.ID, Phase: "running"}
	require.NoError(t, s.CreateAgent(ctx, target))

	sc := storeAgentScopeSchedule(t, s, projectID, "scope-resume-msg", holderCallerAgent, store.ScheduleStatusPaused)
	sc.EventType = "message"
	sc.Payload = `{"agentId":"` + target.ID + `","message":"hi"}`
	require.NoError(t, s.UpdateSchedule(ctx, sc, store.ScheduleFieldMask{EventType: true, Payload: true}, 1, true, nil))
	before := loadScheduleRevision(t, s, sc.ID)

	rec := doAuthoredScheduleRequest(t, srv, agentScopeCaller(projectID), projectID, sc.ID+"/resume", http.MethodPost, nil)
	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), "cross-project scheduled messages require cross-project messaging to be enabled",
		"the denial is the resume's scheduled-message reauthorization")
	assert.Equal(t, before, loadScheduleRevision(t, s, sc.ID))
}

// Resuming moves authority to the resumer: a schedule a user resumed is
// held by that user, so the agent that held it before the resume is
// denied it. A user pause does not move authority, so the agent still
// holds a schedule a user paused.
func TestAgentScheduleScope_ResumeHandOver(t *testing.T) {
	srv, s, projectID := setupScheduleTest(t)
	caller := agentScopeCaller(projectID)
	sc := storeAgentScopeSchedule(t, s, projectID, "scope-handover", holderCallerAgent, store.ScheduleStatusActive)
	base := "/api/v1/projects/" + projectID + "/schedules/" + sc.ID

	rec := doRequest(t, srv, http.MethodPost, base+"/pause", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	rec = doAuthoredScheduleRequest(t, srv, caller, projectID, sc.ID, http.MethodGet, nil)
	require.Equal(t, http.StatusOK, rec.Code, "a user pause keeps the agent as holder: %s", rec.Body.String())

	rec = doRequest(t, srv, http.MethodPost, base+"/resume", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	rec = doAuthoredScheduleRequest(t, srv, caller, projectID, sc.ID, http.MethodGet, nil)
	require.Equal(t, http.StatusForbidden, rec.Code, "the user resume makes the user the holder: %s", rec.Body.String())
	rec = doAuthoredScheduleRequest(t, srv, caller, projectID, "", http.MethodGet, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var list ListSchedulesResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&list))
	assert.Empty(t, list.Schedules)
}

// The health summary shows an agent only the failing schedules it holds.
func TestAgentScheduleScope_HealthSummaryHidesUnheldRows(t *testing.T) {
	srv, s, projectID := setupScheduleTest(t)
	ctx := context.Background()
	ranAt := time.Now().UTC().Add(-time.Minute)
	ids := map[agentScopeHolder]string{}
	for _, h := range agentScopeHolders {
		sc := storeAgentScopeSchedule(t, s, projectID, "scope-health-"+string(h), h, store.ScheduleStatusActive)
		require.NoError(t, s.UpdateScheduleAfterRun(ctx, sc.ID, ranAt, ranAt.Add(time.Hour), "run failed"))
		ids[h] = sc.ID
	}

	r := httptest.NewRequest(http.MethodGet, "/api/v1/admin/health/summary", nil)
	r = r.WithContext(contextWithIdentity(r.Context(), agentScopeCaller(projectID)))
	sec, err := srv.healthSummarySchedules(r)
	require.NoError(t, err)
	assert.Equal(t, []string{ids[holderCallerAgent]}, failingScheduleIDs(sec))
	assert.Equal(t, 1, sec.Total)
}
