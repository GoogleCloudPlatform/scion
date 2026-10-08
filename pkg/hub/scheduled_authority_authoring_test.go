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
	"github.com/go-jose/go-jose/v4/jwt"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The revision ceiling of a scoped UAT is the token's frozen ceiling, with
// its project boundary. revisionAuthorityCeiling is called directly: the
// schedule routes refuse every UAT at the authoring credential gate before
// any schedule handler logic runs
// (TestSchedUATAuthoringRefusedAtBoundary). Recording through the routes
// is covered by session-authored schedules
// (TestScheduleAuthoringRecordsSessionCeiling,
// TestSchedSessionReauthoringRecordsCeiling).
func TestSchedUATRevisionCeilingIsTokenCeiling(t *testing.T) {
	srv, s, projectID := setupScheduleTest(t)
	owner := setupScopedDispatchAgentOwner(t, srv, s, projectID, tid("sched-uat-ceiling-owner"))
	uat := NewScopedUserIdentity(owner, projectID, []string{"scheduled_event:create", "agent:create"})
	want := uat.Ceiling()

	c, ok, rec := fireRevisionCeiling(srv, uat, projectID, "dispatch_agent")
	require.True(t, ok, rec.Body.String())
	assert.Equal(t, store.EffectCeilingBounded, c.Kind)
	assert.Equal(t, want.Version, c.Version)
	assert.Equal(t, want.PermissionIDs, c.PermissionIDs)
	assert.Equal(t, projectID, c.BoundaryProjectID)
}

// A project-scoped UAT cannot author a schedule or a one-shot event of any
// type: the authoring credential gate refuses it and nothing is written, and
// scheduled_event.create is not eligible for a project boundary either. The
// scheduled-message rule for scoped UATs is checked directly in
// TestSessionOnlyGate_ReasonIsReported.
func TestSchedUATAuthoringRefusedAtBoundary(t *testing.T) {
	srv, s, projectID := setupScheduleTest(t)
	owner := setupScopedDispatchAgentOwner(t, srv, s, projectID, tid("sched-uat-boundary-owner"))
	uat := NewScopedUserIdentity(owner, projectID, []string{"scheduled_event:create", "agent:create", "agent:message"})

	schedules := []CreateScheduleRequest{
		{Name: "uat-dispatch", CronExpr: "0 * * * *", EventType: "dispatch_agent", AgentName: "uat-worker"},
		{Name: "uat-message", CronExpr: "0 * * * *", EventType: "message", AgentName: authzHelperAgentSlug, Message: "hi"},
	}
	for _, req := range schedules {
		rec := doAuthoredScheduleRequest(t, srv, uat, projectID, "", http.MethodPost, req)
		assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
		res, err := s.ListSchedules(context.Background(), store.ScheduleFilter{ProjectID: projectID, Name: req.Name}, store.ListOptions{})
		require.NoError(t, err)
		assert.Empty(t, res.Items, req.Name)
	}

	rec := doAuthoredEventRequest(t, srv, uat, projectID,
		CreateScheduledEventRequest{EventType: "dispatch_agent", FireIn: "1h", AgentName: "uat-one-shot"})
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	events, err := s.ListScheduledEvents(context.Background(), store.ScheduledEventFilter{ProjectID: projectID}, store.ListOptions{})
	require.NoError(t, err)
	assert.Empty(t, events.Items)

	assertScheduledEventBoundaryIneligible(t, srv, uat, projectID, ActionCreate)
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
// dispatch_agent ceiling rule: a credential whose ceiling cannot be recorded
// is refused with 403 and nothing changes, while the same credential's
// change to a message schedule records the unrecorded ceiling.
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

	// An agent whose row is not stored: its write ceiling cannot be
	// computed, so it cannot be recorded as an authority source.
	unrecordable := &agentIdentityWrapper{&AgentTokenClaims{
		Claims:    jwt.Claims{Subject: tid("sched-typechange-unstored-agent")},
		ProjectID: projectID,
		Scopes:    []AgentTokenScope{ScopeProjectRead, ScopeAgentCreate},
	}}

	rec = doAuthoredScheduleRequest(t, srv, unrecordable, projectID, created.ID, http.MethodPatch,
		UpdateScheduleRequest{EventType: "dispatch_agent", Payload: `{"agentName":"ghost-worker"}`})
	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), string(DeniedByDelegationCeiling))
	after, err := s.GetSchedule(context.Background(), created.ID)
	require.NoError(t, err)
	assert.Equal(t, "message", after.EventType)
	assert.Equal(t, before.InitiatorAttribution, after.InitiatorAttribution)
	assert.Equal(t, before.AuthorityCeiling, after.AuthorityCeiling)

	// The same credential re-targeting the message schedule records the
	// unrecorded ceiling (scheduled-message authority is its own rule).
	rec = doAuthoredScheduleRequest(t, srv, unrecordable, projectID, created.ID, http.MethodPatch,
		UpdateScheduleRequest{Payload: `{"agentName":"ghost-target-2","message":"hi"}`})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	after, err = s.GetSchedule(context.Background(), created.ID)
	require.NoError(t, err)
	assert.Equal(t, store.EffectCeiling{}, after.AuthorityCeiling)
	assert.Equal(t, before.AuthorizationRevision+1, after.AuthorizationRevision)
}
