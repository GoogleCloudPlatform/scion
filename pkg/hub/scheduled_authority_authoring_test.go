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

// A scoped UAT covering agent creation authors a dispatch_agent schedule
// and one-shot event; each records the token's frozen ceiling with the uat
// attribution.
func TestSchedUATAuthoringDispatchAgentAllowedAfterCeiling(t *testing.T) {
	srv, s, projectID := setupScheduleTest(t)
	owner := setupScopedDispatchAgentOwner(t, srv, s, projectID, tid("sched-uat-allowed-owner"))
	selectors := []string{"scheduled_event:create", "agent:create"}
	uat := NewScopedUserIdentity(owner, projectID, selectors)
	ceiling := uat.Ceiling()

	rec := doAuthoredScheduleRequest(t, srv, uat, projectID, "", http.MethodPost,
		CreateScheduleRequest{Name: "uat-dispatch", CronExpr: "0 * * * *", EventType: "dispatch_agent", AgentName: "uat-worker"})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	var created store.Schedule
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&created))
	rev := loadScheduleRevision(t, s, created.ID)
	assert.Equal(t, store.InitiatorCredentialKindUAT, rev.Attribution.InitiatorCredentialKind)
	assert.Equal(t, store.EffectCeilingBounded, rev.Ceiling.Kind)
	assert.Equal(t, ceiling.Version, rev.Ceiling.Version)
	assert.Equal(t, ceiling.PermissionIDs, rev.Ceiling.PermissionIDs)
	assert.Equal(t, projectID, rev.Ceiling.BoundaryProjectID)

	rec = doAuthoredEventRequest(t, srv, uat, projectID,
		CreateScheduledEventRequest{EventType: "dispatch_agent", FireIn: "1h", AgentName: "uat-one-shot"})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	var evt store.ScheduledEvent
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&evt))
	stored, err := s.GetScheduledEvent(context.Background(), evt.ID)
	require.NoError(t, err)
	assert.Equal(t, ceiling.PermissionIDs, stored.AuthorityCeiling.PermissionIDs)
}

// The scheduled-message gate for scoped UATs is unchanged: a scoped UAT
// cannot author a message schedule to a resolvable target.
func TestSchedUATMessageGateUnchanged(t *testing.T) {
	srv, s, projectID := setupScheduleTest(t)
	owner := setupScopedDispatchAgentOwner(t, srv, s, projectID, tid("sched-uat-msg-owner"))
	uat := NewScopedUserIdentity(owner, projectID, []string{"scheduled_event:create", "agent:message"})

	rec := doAuthoredScheduleRequest(t, srv, uat, projectID, "", http.MethodPost,
		CreateScheduleRequest{Name: "uat-message", CronExpr: "0 * * * *", EventType: "message", AgentName: authzHelperAgentSlug, Message: "hi"})
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), "scope")
	res, err := s.ListSchedules(context.Background(), store.ScheduleFilter{ProjectID: projectID, Name: "uat-message"}, store.ListOptions{})
	require.NoError(t, err)
	assert.Empty(t, res.Items)
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
