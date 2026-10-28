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

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The revision ceiling of a scoped UAT is the token's frozen ceiling, with
// its project boundary. revisionAuthorityCeiling is called directly here;
// recording through the routes is covered by
// TestSchedUATAuthoringRecordsTokenCeiling and, for real tokens, by
// TestScheduleAuthoringToken_RequiresCreateSelector.
func TestSchedUATRevisionCeilingIsTokenCeiling(t *testing.T) {
	srv, s, projectID := setupScheduleTest(t)
	owner := setupScopedDispatchAgentOwner(t, srv, s, projectID, tid("sched-uat-ceiling-owner"))
	uat := NewScopedUserIdentity(owner, projectID, []string{"scheduled_event:create", "agent:create"})
	want := uat.Ceiling()

	c, ok, rec := fireRevisionCeiling(srv, uat, projectID)
	require.True(t, ok, rec.Body.String())
	assert.Equal(t, store.EffectCeilingBounded, c.Kind)
	assert.Equal(t, want.Version, c.Version)
	assert.Equal(t, want.PermissionIDs, c.PermissionIDs)
	assert.Equal(t, projectID, c.BoundaryProjectID)
}

// A project-scoped UAT with scheduled_event:create and the event type's
// agent selector authors a schedule or a one-shot event of either type,
// and each row records the token's attribution and its bounded ceiling with
// the project boundary. scheduled_event.create is eligible for a project
// boundary.
func TestSchedUATAuthoringRecordsTokenCeiling(t *testing.T) {
	srv, s, projectID := setupScheduleTest(t)
	owner := setupScopedDispatchAgentOwner(t, srv, s, projectID, tid("sched-uat-boundary-owner"))
	uat := NewScopedUserIdentity(owner, projectID, []string{"scheduled_event:create", "agent:create", "agent:message"})
	want := uat.Ceiling()
	assertRecorded := func(t *testing.T, label string, attr store.InitiatorAttribution, c store.EffectCeiling) {
		t.Helper()
		assert.Equal(t, store.InitiatorCredentialKindUAT, attr.InitiatorCredentialKind, label)
		assert.Equal(t, owner.ID(), attr.InitiatorPrincipalID, label)
		assert.Equal(t, store.EffectCeilingBounded, c.Kind, label)
		assert.Equal(t, want.PermissionIDs, c.PermissionIDs, label)
		assert.Equal(t, string(BoundaryKindProject), c.BoundaryKind, label)
		assert.Equal(t, projectID, c.BoundaryProjectID, label)
	}

	schedules := []CreateScheduleRequest{
		{Name: "uat-dispatch", CronExpr: "0 * * * *", EventType: "dispatch_agent", AgentName: "uat-worker"},
		{Name: "uat-message", CronExpr: "0 * * * *", EventType: "message", AgentName: authzHelperAgentSlug, Message: "hi"},
	}
	for _, req := range schedules {
		rec := doAuthoredScheduleRequest(t, srv, uat, projectID, "", http.MethodPost, req)
		require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
		res, err := s.ListSchedules(context.Background(), store.ScheduleFilter{ProjectID: projectID, Name: req.Name}, store.ListOptions{})
		require.NoError(t, err)
		require.Len(t, res.Items, 1, req.Name)
		assertRecorded(t, req.Name, res.Items[0].InitiatorAttribution, res.Items[0].AuthorityCeiling)
	}

	rec := doAuthoredEventRequest(t, srv, uat, projectID,
		CreateScheduledEventRequest{EventType: "dispatch_agent", FireIn: "1h", AgentName: "uat-one-shot"})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	var evt store.ScheduledEvent
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&evt))
	stored, err := s.GetScheduledEvent(context.Background(), evt.ID)
	require.NoError(t, err)
	assertRecorded(t, "one-shot dispatch_agent event", stored.InitiatorAttribution, stored.AuthorityCeiling)

	decision := srv.authzService.Decide(context.Background(), AuthzRequest{
		Principal:  principalContextForIdentity(uat),
		Credential: credentialContextForIdentity(uat),
		Resource:   Resource{Type: "scheduled_event", ParentType: "project", ParentID: projectID},
		Action:     ActionCreate,
		Permission: "scheduled_event.create",
	})
	assert.True(t, decision.Allowed, decision.Reason)
}

// A session-authored edit that changes what a future dispatch does records
// the editor's principal ceiling and session attribution, and bumps the
// revision: converting a message schedule to dispatch_agent, and
// re-targeting a dispatch_agent schedule authored by another principal.
func TestSchedSessionReauthoringRecordsCeiling(t *testing.T) {
	srv, s, projectID := setupScheduleTest(t)
	owner := setupScopedDispatchAgentOwner(t, srv, s, projectID, tid("sched-session-reauth-owner"))

	t.Run("convert message schedule to dispatch_agent", func(t *testing.T) {
		author := authzHelperAgent(projectID, ScopeProjectRead, ScopeAgentCreate)
		id := createOwnerSchedule(t, srv, author, projectID, "session-convert", "message")
		before := loadScheduleRevision(t, s, id)
		require.Equal(t, store.EffectCeilingBounded, before.Ceiling.Kind)

		rec := doAuthoredScheduleRequest(t, srv, owner, projectID, id, http.MethodPatch,
			UpdateScheduleRequest{EventType: "dispatch_agent", Payload: `{"agentName":"worker-c"}`})
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		after := loadScheduleRevision(t, s, id)
		assert.Equal(t, store.EffectCeiling{Kind: store.EffectCeilingPrincipal}, after.Ceiling)
		assert.Equal(t, store.InitiatorCredentialKindSession, after.Attribution.InitiatorCredentialKind)
		assert.Equal(t, owner.ID(), after.Attribution.InitiatorPrincipalID)
		assert.Equal(t, before.Attribution.AuthorizationRevision+1, after.Attribution.AuthorizationRevision)
	})

	t.Run("re-target a dispatch_agent schedule", func(t *testing.T) {
		author := authzHelperAgent(projectID, ScopeProjectRead, ScopeAgentCreate)
		id := createOwnerSchedule(t, srv, author, projectID, "session-retarget", "dispatch_agent")
		before := loadScheduleRevision(t, s, id)
		require.Equal(t, store.EffectCeilingBounded, before.Ceiling.Kind)

		rec := doAuthoredScheduleRequest(t, srv, owner, projectID, id, http.MethodPatch,
			UpdateScheduleRequest{Payload: `{"agentName":"worker-b"}`})
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		after := loadScheduleRevision(t, s, id)
		assert.Equal(t, store.EffectCeiling{Kind: store.EffectCeilingPrincipal}, after.Ceiling)
		assert.Equal(t, store.InitiatorCredentialKindSession, after.Attribution.InitiatorCredentialKind)
		assert.Equal(t, owner.ID(), after.Attribution.InitiatorPrincipalID)
		assert.Equal(t, before.Attribution.AuthorizationRevision+1, after.Attribution.AuthorizationRevision)
	})
}

// An update that changes a schedule's type to dispatch_agent is held to the
// ceiling rule: a credential whose ceiling cannot be recorded is refused
// with 403 and nothing changes. The same credential's change to the message
// schedule is refused the same way.
func TestScheduleTypeChangeToDispatchAgentUsesDispatchRule(t *testing.T) {
	srv, s, projectID := setupScheduleTest(t)
	owner := setupScopedDispatchAgentOwner(t, srv, s, projectID, tid("sched-typechange-owner"))
	rec := doAuthoredScheduleRequest(t, srv, owner, projectID, "", http.MethodPost,
		CreateScheduleRequest{Name: "type-change", CronExpr: "0 * * * *", EventType: "message", AgentName: "ghost-target", Message: "hi"})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	var created store.Schedule
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&created))
	before, err := s.GetSchedule(context.Background(), created.ID)
	require.NoError(t, err)

	// A local development user other than the recognized one, holding the
	// project owner role: it may create agents, but its ceiling cannot be
	// recorded as an authority source. Agent callers do not edit schedule
	// definitions, so the PATCH caller is a user.
	devID := tid("sched-typechange-dev")
	require.NoError(t, s.CreateUser(context.Background(), &store.User{
		ID: devID, Email: "devtypechange@example.com", DisplayName: "Dev", Role: "member", Status: "active",
	}))
	require.NoError(t, srv.createProjectOwnerRoleBinding(context.Background(), projectID, devID))
	unrecordable := &DevUser{id: devID, email: "devtypechange@example.com", displayName: "Dev"}

	rec = doAuthoredScheduleRequest(t, srv, unrecordable, projectID, created.ID, http.MethodPatch,
		UpdateScheduleRequest{EventType: "dispatch_agent", Payload: `{"agentName":"ghost-worker"}`})
	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), string(DeniedByDelegationCeiling))
	assert.Contains(t, rec.Body.String(), "cannot authorize scheduled work")
	after, err := s.GetSchedule(context.Background(), created.ID)
	require.NoError(t, err)
	assert.Equal(t, "message", after.EventType)
	assert.Equal(t, before.InitiatorAttribution, after.InitiatorAttribution)
	assert.Equal(t, before.AuthorityCeiling, after.AuthorityCeiling)

	// The same credential re-targeting the message schedule is refused too.
	rec = doAuthoredScheduleRequest(t, srv, unrecordable, projectID, created.ID, http.MethodPatch,
		UpdateScheduleRequest{Payload: `{"agentName":"ghost-target-2","message":"hi"}`})
	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), string(DeniedByDelegationCeiling))
	assert.Contains(t, rec.Body.String(), "cannot authorize scheduled work")
	after, err = s.GetSchedule(context.Background(), created.ID)
	require.NoError(t, err)
	assert.Equal(t, before.Payload, after.Payload)
	assert.Equal(t, before.InitiatorAttribution, after.InitiatorAttribution)
	assert.Equal(t, before.AuthorityCeiling, after.AuthorityCeiling)
}
