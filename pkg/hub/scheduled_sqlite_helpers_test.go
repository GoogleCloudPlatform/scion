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
	"errors"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/permissions"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// withAgentRevision returns evt carrying the recorded authorization revision
// a create or resume by the stored agent agentID writes: agent attribution
// and the agent's own write ceiling, computed at seed time from its stored row and
// edge, as the authoring handler computes it.
func withAgentRevision(t *testing.T, srv *Server, evt store.ScheduledEvent, agentID string) store.ScheduledEvent {
	t.Helper()
	ctx := context.Background()
	agent, err := srv.store.GetAgent(ctx, agentID)
	require.NoError(t, err)
	ceiling, err := srv.authzService.agentRowEffectCeiling(ctx, agent)
	require.NoError(t, err)
	evt.InitiatorAttribution = store.InitiatorAttribution{
		InitiatorPrincipalKind:  store.DelegationPrincipalAgent,
		InitiatorPrincipalID:    agentID,
		InitiatorCredentialKind: store.InitiatorCredentialKindAgent,
		InitiatorCredentialID:   "jti-" + agentID,
		AttributionVersion:      1,
		AuthorizationRevision:   1,
	}
	evt.AuthorityCeiling = ceiling
	return evt
}

// schedFire is a scheduled dispatch fixture: a project with a member user
// who may create agents (the creator), and no dispatcher unless a test
// installs one.
type schedFire struct {
	*uatCreateFixture
}

func newSchedFire(t *testing.T, name string) *schedFire {
	t.Helper()
	return &schedFire{uatCreateFixture: newUATCreateFixture(t, name)}
}

// withUATRevision returns evt carrying the revision a create by tok writes:
// uat attribution and the token's frozen ceiling.
func withUATRevision(evt store.ScheduledEvent, tok *store.UserAccessToken) store.ScheduledEvent {
	evt.InitiatorAttribution = store.InitiatorAttribution{
		InitiatorPrincipalKind:  store.DelegationPrincipalUser,
		InitiatorPrincipalID:    tok.UserID,
		InitiatorCredentialKind: store.InitiatorCredentialKindUAT,
		InitiatorCredentialID:   tok.ID,
		AttributionVersion:      1,
		AuthorizationRevision:   3,
	}
	evt.AuthorityCeiling = store.EffectCeiling{
		Kind:              store.EffectCeilingBounded,
		Version:           tok.CeilingVersion,
		PermissionIDs:     append([]string(nil), tok.CeilingPermissionIDs...),
		BoundaryKind:      tok.BoundaryKind,
		BoundaryProjectID: tok.ProjectID,
		SourceExpiresAt:   tok.ExpiresAt,
	}
	return evt
}

func assertSchedulerProvenance(t *testing.T, evt store.ScheduledEvent, edge *store.DelegationEdge, principalKind, principalID string) {
	t.Helper()
	assert.Equal(t, principalKind, edge.DelegatorType)
	assert.Equal(t, principalID, edge.DelegatorID)
	assert.Equal(t, store.ProvenanceVersionV1, edge.ProvenanceVersion)
	assert.Equal(t, store.SourceCredentialScheduler, edge.SourceCredentialKind)
	assert.Equal(t, principalKind, edge.SourcePrincipalKind)
	assert.Equal(t, principalID, edge.SourcePrincipalID)
	assert.Empty(t, edge.SourceCredentialID)
	assert.Equal(t, evt.ID, edge.SourceEventID)
	assert.Equal(t, evt.ScheduleID, edge.SourceScheduleID)
	assert.Equal(t, evt.AuthorizationRevision, edge.SourceAuthorizationRevision)
	assert.Equal(t, evt.InitiatorPrincipalKind, edge.InitiatorPrincipalKind)
	assert.Equal(t, evt.InitiatorPrincipalID, edge.InitiatorPrincipalID)
	assert.Equal(t, evt.InitiatorCredentialKind, edge.InitiatorCredentialKind)
	assert.Equal(t, evt.InitiatorCredentialID, edge.InitiatorCredentialID)
}

// event returns a dispatch_agent event for agentName whose history creator
// is the fixture creator and which carries no authorization revision.
func (f *schedFire) event(agentName string) store.ScheduledEvent {
	return store.ScheduledEvent{
		ID:         api.NewUUID(),
		ProjectID:  f.proj.ID,
		EventType:  "dispatch_agent",
		Payload:    `{"agentName":"` + agentName + `"}`,
		CreatedBy:  f.creator.ID,
		ScheduleID: "sched-" + agentName,
		FireAt:     time.Now(),
	}
}

func (f *schedFire) fire(t *testing.T, evt store.ScheduledEvent) error {
	t.Helper()
	return f.srv.dispatchAgentEventHandler()(context.Background(), evt)
}

// child returns the scheduled child for slug and its single active edge.
func (f *schedFire) child(t *testing.T, slug string) (*store.Agent, *store.DelegationEdge) {
	t.Helper()
	agent, err := f.store.GetAgentBySlug(context.Background(), f.proj.ID, slug)
	require.NoError(t, err, "scheduled child %s not created", slug)
	edges := activeEdgesFor(t, f.store, agent.ID)
	require.Len(t, edges, 1)
	return agent, edges[0]
}

func (f *schedFire) assertNoChild(t *testing.T, slug string) {
	t.Helper()
	_, err := f.store.GetAgentBySlug(context.Background(), f.proj.ID, slug)
	assert.ErrorIs(t, err, store.ErrNotFound, "no scheduled child %s", slug)
}

// sessionAgent creates an agent with the creator's session and returns it.
func (f *schedFire) sessionAgent(t *testing.T, slug string) *store.Agent {
	t.Helper()
	a, _ := f.createdAgent(t, f.create(t, authUser(f.creator), CreateAgentRequest{Name: slug, AgentRole: string(AgentRoleFull)}), slug)
	return a
}

// storedUAT stores a live access token for the creator holding selectors
// and returns it.
func (f *schedFire) storedUAT(t *testing.T, selectors ...string) *store.UserAccessToken {
	t.Helper()
	return f.storedUATExpiring(t, time.Now().Add(24*time.Hour), selectors...)
}

// storedUATExpiring is storedUAT with an explicit expiry.
func (f *schedFire) storedUATExpiring(t *testing.T, exp time.Time, selectors ...string) *store.UserAccessToken {
	t.Helper()
	return f.storedUATOwnedBy(t, f.creator.ID, exp, selectors...)
}

// storedUATOwnedBy stores a live access token for userID holding selectors
// and returns it.
func (f *schedFire) storedUATOwnedBy(t *testing.T, userID string, exp time.Time, selectors ...string) *store.UserAccessToken {
	t.Helper()
	c := uatCeilingFromSelectors(t, selectors...)
	tok := &store.UserAccessToken{
		ID:                   api.NewUUID(),
		UserID:               userID,
		Name:                 "sched-uat",
		Prefix:               "scion_pat_x",
		KeyHash:              api.NewUUID(),
		BoundaryKind:         string(permissions.BoundaryKindProject),
		ProjectID:            f.proj.ID,
		Scopes:               selectors,
		CeilingVersion:       permissions.CeilingVersionV1,
		CeilingPermissionIDs: c.PermissionIDs,
		ExpiresAt:            &exp,
		Created:              time.Now(),
	}
	require.NoError(t, f.store.CreateUserAccessToken(context.Background(), tok))
	return tok
}

// seedDevLocalUser stores the local development user with a project role
// and turns local development authority on or off.
func (f *schedFire) seedDevLocalUser(t *testing.T, status string, enabled bool) {
	t.Helper()
	ctx := context.Background()
	if _, err := f.store.GetUser(ctx, DevUserID); errors.Is(err, store.ErrNotFound) {
		require.NoError(t, f.store.CreateUser(ctx, &store.User{ID: DevUserID, Email: "dev@localhost", DisplayName: "Dev", Role: store.UserRoleMember, Status: status, Created: time.Now()}))
		grantFixtureRole(t, f.bypassAgentsFixture, DevUserID, store.ProjectRoleMember)
	} else {
		u, err := f.store.GetUser(ctx, DevUserID)
		require.NoError(t, err)
		u.Status = status
		require.NoError(t, f.store.UpdateUser(ctx, u))
	}
	f.srv.authzService.setDevLocalAuthorityEnabled(enabled)
}
