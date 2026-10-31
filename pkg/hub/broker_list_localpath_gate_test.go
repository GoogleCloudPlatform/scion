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
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// createProjectAgentForGate creates an agent in projectID owned by
// ownerID (its standing root through the stored owner link) and, when held
// is true, puts it on hold. It returns the agent ID.
func createProjectAgentForGate(t *testing.T, srv *Server, s store.Store, projectID, ownerID, name string, held bool) string {
	t.Helper()
	ctx := context.Background()
	id := tid(name)
	require.NoError(t, s.CreateAgent(ctx, &store.Agent{
		ID: id, Slug: name, Name: name,
		ProjectID: projectID, OwnerID: ownerID, Created: time.Now(), Updated: time.Now(),
	}))
	if held {
		_, err := s.CreateAgentHolds(ctx, []*store.AgentHold{{
			AgentID: id, ProjectID: projectID, Cause: store.AgentHoldCauseOwnerAccessEnded,
			RootPrincipalType: store.AgentHoldRootUser, RootPrincipalID: ownerID,
			Trigger: store.MembershipLossTriggerMemberRemove, ActorKind: "system", ActorID: "hub", CorrelationID: "test",
		}})
		require.NoError(t, err)
		require.Equal(t, standingReasonAgentHeld, standingReason(srv.agentStanding(ctx, id)), "fixture: the agent is held")
	} else {
		require.NoError(t, srv.agentStanding(ctx, id), "fixture: the agent is in good standing")
	}
	return id
}

// TestCallerMayReadProviders_GoodStandingAgent is the positive control for
// the provider-reader gate's agent branch (ptone/scion#6844): a
// good-standing agent of the project passes, while a held agent of the same
// project, an agent of another project and an unknown agent do not.
func TestCallerMayReadProviders_GoodStandingAgent(t *testing.T) {
	srv, s, alice, _, project := setupTemplateAuthzTest(t)
	ctx := context.Background()
	scopes := ScopesForRole(AgentRoleFull)
	asAgent := func(id, projectID string) context.Context {
		return contextWithIdentity(ctx, newTestAgentIdentity(id, projectID, scopes))
	}

	good := createProjectAgentForGate(t, srv, s, project.ID, alice.ID, "gate-good-agent", false)
	held := createProjectAgentForGate(t, srv, s, project.ID, alice.ID, "gate-held-agent", true)

	assert.True(t, srv.callerMayReadProviders(asAgent(good, project.ID), project.ID), "a good-standing agent of the project")
	assert.False(t, srv.callerMayReadProviders(asAgent(held, project.ID), project.ID), "a held agent of the same project")
	assert.False(t, srv.callerMayReadProviders(asAgent(good, tid("gate-other-project")), project.ID), "an agent identity of another project")
	assert.False(t, srv.callerMayReadProviders(asAgent(tid("gate-missing-agent"), project.ID), project.ID), "an unknown agent")
}
