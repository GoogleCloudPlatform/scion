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
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/go-jose/go-jose/v4/jwt"
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
	return storeAgentScopeScheduleAt(t, s, projectID, name, h, status, time.Time{})
}

// storeAgentScopeScheduleAt is storeAgentScopeSchedule with the row's
// creation time (zero: the store's).
func storeAgentScopeScheduleAt(t *testing.T, s store.Store, projectID, name string, h agentScopeHolder, status string, createdAt time.Time) *store.Schedule {
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
		CreatedAt:            createdAt,
		CreatedBy:            authzHelperAgentID,
		InitiatorAttribution: h.attribution(),
	}
	require.NoError(t, s.CreateSchedule(context.Background(), sc))
	return sc
}

// storeAgentScopeEvent stores a pending scheduled event held by h.
func storeAgentScopeEvent(t *testing.T, s store.Store, projectID string, h agentScopeHolder) *store.ScheduledEvent {
	t.Helper()
	return storeAgentScopeEventAt(t, s, projectID, h, time.Time{})
}

// storeAgentScopeEventAt is storeAgentScopeEvent with the row's creation
// time (zero: the store's).
func storeAgentScopeEventAt(t *testing.T, s store.Store, projectID string, h agentScopeHolder, createdAt time.Time) *store.ScheduledEvent {
	t.Helper()
	evt := &store.ScheduledEvent{
		ID:                   api.NewUUID(),
		ProjectID:            projectID,
		EventType:            "message",
		FireAt:               time.Now().UTC().Add(time.Hour),
		Payload:              `{"agentName":"agent-scope-worker","message":"hi"}`,
		Status:               store.ScheduledEventPending,
		CreatedAt:            createdAt,
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

// An agent's schedule list holds only the rows it holds, counted; a
// user's list is every row with its count.
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
	assert.Equal(t, 1, resp.TotalCount)

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
	assert.Equal(t, 1, resp.TotalCount)

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

// PATCH of a schedule another agent holds is denied to an agent caller,
// and the row is unchanged.
func TestAgentScheduleScope_PatchDeniedOnOtherAgentRow(t *testing.T) {
	srv, s, projectID := setupScheduleTest(t)
	sc := storeAgentScopeSchedule(t, s, projectID, "scope-patch-other", holderOtherAgent, store.ScheduleStatusActive)

	rec := doAuthoredScheduleRequest(t, srv, agentScopeCaller(projectID), projectID, sc.ID, http.MethodPatch,
		UpdateScheduleRequest{Name: "scope-patch-other-renamed"})
	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	assert.JSONEq(t, agentScopeForbiddenBody, rec.Body.String())
	after, err := s.GetSchedule(context.Background(), sc.ID)
	require.NoError(t, err)
	assert.Equal(t, "scope-patch-other", after.Name)
	assert.Equal(t, sc.InitiatorAttribution, after.InitiatorAttribution)
}

// scheduledDecisionCounter counts the scheduled_event authorization
// decisions the authz service records.
type scheduledDecisionCounter struct {
	mu sync.Mutex
	n  int
}

func (c *scheduledDecisionCounter) EmitDecisionAudit(_ context.Context, rec *store.DecisionAuditRecord) {
	if rec != nil && rec.ResourceType == "scheduled_event" {
		c.mu.Lock()
		c.n++
		c.mu.Unlock()
	}
}

func (c *scheduledDecisionCounter) take() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := c.n
	c.n = 0
	return n
}

// countScheduledDecisions records every scheduled_event decision of srv's
// authz service (no sampling) for the rest of the test.
func countScheduledDecisions(t *testing.T, srv *Server) *scheduledDecisionCounter {
	t.Helper()
	c := &scheduledDecisionCounter{}
	prevEmitter, prevRate := srv.authzService.decisionAuditEmitter, srv.authzService.DecisionAuditSampleRate
	srv.authzService.SetDecisionAuditEmitter(c)
	srv.authzService.DecisionAuditSampleRate = 1.0
	t.Cleanup(func() {
		srv.authzService.SetDecisionAuditEmitter(prevEmitter)
		srv.authzService.DecisionAuditSampleRate = prevRate
	})
	return c
}

// A user caller is decided once per request: a per-ID read or pause makes
// one scheduled_event decision, and a health poll over several failing
// schedules of one project makes one.
func TestAgentScheduleScope_UserCallerDecidedOnce(t *testing.T) {
	srv, s, projectID := setupScheduleTest(t)
	owner := setupScopedDispatchAgentOwner(t, srv, s, projectID, tid("scope-once-owner"))
	ctx := context.Background()
	ranAt := time.Now().UTC().Add(-time.Minute)
	var ids []string
	for _, h := range agentScopeHolders {
		sc := storeAgentScopeSchedule(t, s, projectID, "scope-once-"+string(h), h, store.ScheduleStatusActive)
		require.NoError(t, s.UpdateScheduleAfterRun(ctx, sc.ID, ranAt, ranAt.Add(time.Hour), "run failed"))
		ids = append(ids, sc.ID)
	}
	decisions := countScheduledDecisions(t, srv)

	rec := doAuthoredScheduleRequest(t, srv, owner, projectID, ids[0], http.MethodGet, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, 1, decisions.take(), "a per-ID read is decided once")

	rec = doAuthoredScheduleRequest(t, srv, owner, projectID, ids[1]+"/pause", http.MethodPost, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, 1, decisions.take(), "a per-ID pause is decided once")

	r := httptest.NewRequest(http.MethodGet, "/api/v1/admin/health/summary", nil)
	r = r.WithContext(contextWithIdentity(r.Context(), owner))
	sec, err := srv.healthSummarySchedules(r)
	require.NoError(t, err)
	assert.Len(t, sec.Failing, len(ids)-1, "every active failing schedule is shown to the owner")
	assert.Equal(t, 1, decisions.take(), "a health poll over one project is decided once")
}

// An agent's list is narrowed before paging: with a page size of one and
// newer rows held by others ahead of it, the agent's first page is the row
// it holds, for schedules and for scheduled events.
func TestAgentScheduleScope_ListPagesHeldRowsFirst(t *testing.T) {
	srv, s, projectID := setupScheduleTest(t)
	base := time.Now().UTC().Add(-time.Hour)
	held := storeAgentScopeScheduleAt(t, s, projectID, "scope-page-held", holderCallerAgent, store.ScheduleStatusActive, base)
	heldEvt := storeAgentScopeEventAt(t, s, projectID, holderCallerAgent, base)
	for i, h := range []agentScopeHolder{holderOtherAgent, holderUser, holderLegacy, holderOtherAgent, holderUser} {
		at := base.Add(time.Duration(i+1) * time.Minute)
		storeAgentScopeScheduleAt(t, s, projectID, "scope-page-"+string(h), h, store.ScheduleStatusActive, at)
		storeAgentScopeEventAt(t, s, projectID, h, at)
	}
	caller := agentScopeCaller(projectID)

	req := authoredRequest(t, caller, http.MethodGet, "/api/v1/projects/"+projectID+"/schedules?limit=1", nil)
	rec := httptest.NewRecorder()
	srv.handleSchedules(rec, req, projectID, "")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var list ListSchedulesResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&list))
	require.Len(t, list.Schedules, 1)
	assert.Equal(t, held.ID, list.Schedules[0].ID)
	assert.Equal(t, 1, list.TotalCount)
	assert.Empty(t, list.NextCursor)

	req = authoredRequest(t, caller, http.MethodGet, "/api/v1/projects/"+projectID+"/scheduled-events?limit=1", nil)
	rec = httptest.NewRecorder()
	srv.handleScheduledEvents(rec, req, projectID, "")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var events ListScheduledEventsResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&events))
	require.Len(t, events.Events, 1)
	assert.Equal(t, heldEvt.ID, events.Events[0].ID)
	assert.Equal(t, 1, events.TotalCount)
}

// scheduledListReadStore fails every schedule and scheduled-event list read
// and counts them.
type scheduledListReadStore struct {
	store.Store
	reads int
}

func (f *scheduledListReadStore) ListSchedules(context.Context, store.ScheduleFilter, store.ListOptions) (*store.ListResult[store.Schedule], error) {
	f.reads++
	return nil, errors.New("schedule list read")
}

func (f *scheduledListReadStore) ListScheduledEvents(context.Context, store.ScheduledEventFilter, store.ListOptions) (*store.ListResult[store.ScheduledEvent], error) {
	f.reads++
	return nil, errors.New("scheduled event list read")
}

// An agent caller with an empty ID holds no rows: its schedule and
// scheduled-event lists and its health summary section are empty, carry no
// cursor or count, and do not read the store.
func TestAgentScheduleScope_EmptyAgentIDListsNothing(t *testing.T) {
	srv, s, projectID := setupScheduleTest(t)
	storeAgentScopeSchedule(t, s, projectID, "scope-empty-id-legacy", holderLegacy, store.ScheduleStatusActive)
	noID := &agentIdentityWrapper{&AgentTokenClaims{
		Claims:    jwt.Claims{Subject: ""},
		ProjectID: projectID,
		Scopes:    []AgentTokenScope{ScopeProjectRead},
	}}
	reads := &scheduledListReadStore{Store: s}
	srv.store = reads
	t.Cleanup(func() { srv.store = s })

	rec := doAuthoredScheduleRequest(t, srv, noID, projectID, "", http.MethodGet, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var list ListSchedulesResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&list))
	assert.Empty(t, list.Schedules)
	assert.Empty(t, list.NextCursor)
	assert.Zero(t, list.TotalCount)

	rec = doAgentScopeEventRequest(t, srv, noID, projectID, "", http.MethodGet)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var events ListScheduledEventsResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&events))
	assert.Empty(t, events.Events)
	assert.Empty(t, events.NextCursor)
	assert.Zero(t, events.TotalCount)

	r := httptest.NewRequest(http.MethodGet, "/api/v1/admin/health/summary", nil)
	r = r.WithContext(contextWithIdentity(r.Context(), noID))
	sec, err := srv.healthSummarySchedules(r)
	require.NoError(t, err)
	assert.Empty(t, sec.Failing)
	assert.Zero(t, sec.Total)

	assert.Zero(t, reads.reads, "an agent caller with an empty ID does not read the store")
}

// An agent's health summary scan reads only the failing schedules it holds
// in its own project, so rows it cannot see do not use up the scan limit.
func TestAgentScheduleScope_HealthScanSkipsUnheldRows(t *testing.T) {
	srv, s, projectID := setupScheduleTest(t)
	ctx := context.Background()
	base := time.Now().UTC().Add(-time.Hour)
	held := storeAgentScopeScheduleAt(t, s, projectID, "scope-scan-held", holderCallerAgent, store.ScheduleStatusActive, base)
	require.NoError(t, s.UpdateScheduleAfterRun(ctx, held.ID, base, base.Add(time.Hour), "run failed"))
	for i, h := range []agentScopeHolder{holderOtherAgent, holderUser, holderLegacy} {
		at := base.Add(time.Duration(i+1) * time.Minute)
		sc := storeAgentScopeScheduleAt(t, s, projectID, "scope-scan-"+string(h), h, store.ScheduleStatusActive, at)
		require.NoError(t, s.UpdateScheduleAfterRun(ctx, sc.ID, at, at.Add(time.Hour), "run failed"))
	}
	prevScan, prevPage := healthSummaryScheduleScanLimit, healthSummarySchedulePageSize
	healthSummaryScheduleScanLimit, healthSummarySchedulePageSize = 1, 1
	t.Cleanup(func() { healthSummaryScheduleScanLimit, healthSummarySchedulePageSize = prevScan, prevPage })

	r := httptest.NewRequest(http.MethodGet, "/api/v1/admin/health/summary", nil)
	r = r.WithContext(contextWithIdentity(r.Context(), agentScopeCaller(projectID)))
	sec, err := srv.healthSummarySchedules(r)
	require.NoError(t, err)
	assert.Equal(t, []string{held.ID}, failingScheduleIDs(sec))
	assert.Equal(t, 1, sec.Total)
	assert.False(t, sec.ScanLimited)
}
