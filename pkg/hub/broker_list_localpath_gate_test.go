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

	"github.com/go-jose/go-jose/v4/jwt"
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

// productionAgentIdentity is the identity a verified agent token carries
// (agentIdentityWrapper over AgentTokenClaims), so authorization treats it
// as a real agent principal.
func productionAgentIdentity(id, projectID string, scopes []AgentTokenScope) Identity {
	return &agentIdentityWrapper{&AgentTokenClaims{
		Claims: jwt.Claims{Subject: id}, ProjectID: projectID, Scopes: scopes,
	}}
}

// TestCallerMayReadProviders_GoodStandingAgent is the positive control for
// the provider-reader gate's agent branch (ptone/scion#6844): a
// good-standing agent of the project passes, while a held agent of the same
// project, an agent of another project and an unknown agent do not. Every
// case runs as a real agent principal (productionAgentIdentity), and each
// refusal is checked for its own cause: an identity the authorization
// kernel does not recognize as an agent would be refused whatever the case,
// so the negative cases could not fail with one.
func TestCallerMayReadProviders_GoodStandingAgent(t *testing.T) {
	srv, s, alice, _, project := setupTemplateAuthzTest(t)
	ctx := context.Background()
	scopes := ScopesForRole(AgentRoleFull)
	asAgent := func(id, projectID string) context.Context {
		return contextWithIdentity(ctx, productionAgentIdentity(id, projectID, scopes))
	}
	// readDecision is the kernel's project read decision for the caller.
	readDecision := func(callerCtx context.Context) Decision {
		return srv.authzService.CheckAccess(callerCtx, GetIdentityFromContext(callerCtx), projectResource(project), ActionRead)
	}
	// mayRead checks the gate and, on a mismatch, reports the project read
	// decision's reason, so a failure names its cause.
	mayRead := func(callerCtx context.Context, want bool, what string) {
		t.Helper()
		if got := srv.callerMayReadProviders(callerCtx, project.ID); got != want {
			t.Errorf("%s: callerMayReadProviders = %v, want %v (project read decision: %q)", what, got, want, readDecision(callerCtx).Reason)
		}
	}
	const unrecognized = "unrecognized principal kind"

	good := createProjectAgentForGate(t, srv, s, project.ID, alice.ID, "gate-good-agent", false)
	held := createProjectAgentForGate(t, srv, s, project.ID, alice.ID, "gate-held-agent", true)
	missing := tid("gate-missing-agent")
	otherProject := tid("gate-other-project")

	// The positive: the kernel grants the read through the agent's token
	// scopes, and the gate passes.
	d := readDecision(asAgent(good, project.ID))
	require.True(t, d.Allowed, "a good-standing agent's project read: %q", d.Reason)
	require.Equal(t, "role binding grant", d.Reason)
	mayRead(asAgent(good, project.ID), true, "a good-standing agent of the project")

	// Held: the same project and token shape as good; only the hold differs,
	// and the gate refuses on standing.
	require.Equal(t, standingReasonAgentHeld, standingReason(srv.agentStanding(ctx, held)))
	heldDecision := readDecision(asAgent(held, project.ID))
	require.NotEqual(t, unrecognized, heldDecision.Reason, "held: a real agent principal")
	// The kernel allows this principal, so the gate's own standing check is
	// what refuses it.
	require.True(t, heldDecision.Allowed, "held: the kernel allows the project read, so the gate's standing check refuses it: %q", heldDecision.Reason)
	mayRead(asAgent(held, project.ID), false, "a held agent of the same project")

	// Another project: the good agent itself, asserting another project, is
	// refused on the project check (the same agent passes above).
	require.NotEqual(t, unrecognized, readDecision(asAgent(good, otherProject)).Reason, "other project: a real agent principal")
	mayRead(asAgent(good, otherProject), false, "an agent identity of another project")

	// Unknown: no agent record, so the gate refuses on standing.
	require.Equal(t, standingReasonAgentMissing, standingReason(srv.agentStanding(ctx, missing)))
	missingDecision := readDecision(asAgent(missing, project.ID))
	require.NotEqual(t, unrecognized, missingDecision.Reason, "unknown agent: a real agent principal")
	require.True(t, missingDecision.Allowed, "unknown agent: the kernel allows the project read, so the gate's standing check refuses it: %q", missingDecision.Reason)
	mayRead(asAgent(missing, project.ID), false, "an unknown agent")
}
