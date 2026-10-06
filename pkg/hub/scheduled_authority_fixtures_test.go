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
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/hub/permissions"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/require"
)

// seedScheduleAuthorAgent stores the agent that authzHelperAgent names, in
// projectID, so the agent can author schedules: a schedule revision records
// the author's effect ceiling, which for an agent is computed from its
// stored row. The agent has no edge, the shape of an agent created before
// the edge backfill (which these tests leave incomplete).
func seedScheduleAuthorAgent(t *testing.T, s store.Store, projectID string) {
	t.Helper()
	require.NoError(t, s.CreateAgent(context.Background(), &store.Agent{
		ID:            authzHelperAgentID,
		Slug:          "schedule-author-agent",
		Name:          "schedule-author-agent",
		ProjectID:     projectID,
		Phase:         "running",
		AppliedConfig: &store.AgentAppliedConfig{AgentRole: string(AgentRoleFull)},
	}))
}

// withSessionRevision returns evt carrying the recorded authorization
// revision a session create or re-save by userID writes: session attribution
// for the user and the principal ceiling.
func withSessionRevision(evt store.ScheduledEvent, userID string) store.ScheduledEvent {
	evt.InitiatorAttribution = store.InitiatorAttribution{
		InitiatorPrincipalKind:  store.DelegationPrincipalUser,
		InitiatorPrincipalID:    userID,
		InitiatorCredentialKind: store.InitiatorCredentialKindSession,
		AttributionVersion:      1,
		AuthorizationRevision:   1,
	}
	evt.AuthorityCeiling = store.EffectCeiling{Kind: store.EffectCeilingPrincipal}
	return evt
}

// withAgentRevision returns evt carrying the recorded authorization revision
// a create or re-save by the stored agent agentID writes: agent attribution
// and the agent's own write ceiling, computed now from its stored row and
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

// withMockAgentRevision returns evt carrying an agent revision for agentID
// whose recorded ceiling allows every registry permission, for stores where
// the author's write ceiling is not computed. A fire intersects it with the
// agent's write ceiling at fire time, so the result is that ceiling.
func withMockAgentRevision(evt store.ScheduledEvent, agentID string) store.ScheduledEvent {
	ids := make([]string, 0, len(permissions.Registry))
	for _, p := range permissions.Registry {
		ids = append(ids, p.ID)
	}
	evt.InitiatorAttribution = store.InitiatorAttribution{
		InitiatorPrincipalKind:  store.DelegationPrincipalAgent,
		InitiatorPrincipalID:    agentID,
		InitiatorCredentialKind: store.InitiatorCredentialKindAgent,
		InitiatorCredentialID:   "jti-" + agentID,
		AttributionVersion:      1,
		AuthorizationRevision:   1,
	}
	evt.AuthorityCeiling = store.EffectCeiling{
		Kind:          store.EffectCeilingBounded,
		Version:       permissions.CeilingVersionV1,
		PermissionIDs: sortedUniqueIDs(ids),
	}
	return evt
}
