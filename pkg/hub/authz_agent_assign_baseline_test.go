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

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/go-jose/go-jose/v4/jwt"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Tests for step 3b of checkAccessForAgent: the project-scoped service-account
// assign baseline (svc-accnt P0.4).
//
// The arm exists because converting the SA-assignment gate from ActionRead to
// ActionAssign would otherwise deny every agent caller hub-wide —
// checkAccessForAgent has no admin or owner bypass and no seeded policy grants
// assign. The security in that conversion comes from the GCP actAs check, not
// from narrowing the Hub policy layer.

// projectSA builds a project-scoped service account resource in the given
// project. Project-scoped is what gives it a project parent, which is what the
// baseline keys on.
func projectSA(t *testing.T, id, projectID string) Resource {
	t.Helper()
	r := gcpServiceAccountResource(&store.GCPServiceAccount{
		ID:      id,
		Scope:   store.ScopeProject,
		ScopeID: projectID,
		Email:   id + "@example.iam.gserviceaccount.com",
	})
	require.Equal(t, projectID, projectIDForResource(r),
		"fixture must carry a project parent for the test to mean anything")
	return r
}

// TestAuthz_AgentAssignBaseline_AllowsOwnProject verifies that an agent
// assigning a service account in its own project is handled by the authz layer.
//
// CO1: gcp_service_account.assign has no AgentScopes mapping in the permissions
// registry. The agent scope restriction blocks this permission even with
// explicit role bindings. SA assignment for agents is now gated at the handler
// level (authorizeSAAssignment) rather than through the authz kernel.
func TestAuthz_AgentAssignBaseline_AllowsOwnProject(t *testing.T) {
	f := newAgentBaselineFixture(t)
	ctx := context.Background()

	sa := projectSA(t, tid("assign-sa-own"), f.ownProject.ID)

	decision := f.authz.CheckAccess(ctx, f.identity, sa, ActionAssign)
	// CO1: The agent scope restriction blocks gcp_service_account.assign
	// because it has no AgentScopes mapping.
	assert.False(t, decision.Allowed,
		"CO1: gcp_service_account.assign blocked by agent scope restriction")
}

// TestAuthz_AgentAssignBaseline_MatchesReadBaselineReach pins the property the
// arm is justified by: it admits exactly the service accounts step 3 already
// admits under ActionRead. Same predicate, different action.
//
// This is the narrow claim. It does NOT say the conversion preserves every way
// an agent could reach the gate under ActionRead — policy- and
// delegation-granted read are deliberately not preserved. See
// TestAuthz_AgentAssignBaseline_DoesNotInheritReadPolicy.
func TestAuthz_AgentAssignBaseline_MatchesReadBaselineReach(t *testing.T) {
	f := newAgentBaselineFixture(t)
	ctx := context.Background()

	for name, sa := range map[string]Resource{
		"own project":   projectSA(t, tid("assign-parity-own"), f.ownProject.ID),
		"other project": projectSA(t, tid("assign-parity-other"), f.otherProject.ID),
	} {
		t.Run(name, func(t *testing.T) {
			readDecision := f.authz.CheckAccess(ctx, f.identity, sa, ActionRead)
			assignDecision := f.authz.CheckAccess(ctx, f.identity, sa, ActionAssign)
			assert.Equal(t, readDecision.Allowed, assignDecision.Allowed,
				"assign baseline must admit exactly the SAs the read baseline admits")
		})
	}
}

// TestAuthz_AgentAssignBaseline_CrossProjectDenied pins project isolation. This
// is the confinement the whole grant rests on: it is safe only because an agent
// cannot reach a service account outside its own project.
// CO1: gcp_service_account.assign has no AgentScopes, so it is blocked by the
// scope restriction for all agents, including cross-project.
func TestAuthz_AgentAssignBaseline_CrossProjectDenied(t *testing.T) {
	f := newAgentBaselineFixture(t)
	ctx := context.Background()

	sa := projectSA(t, tid("assign-sa-foreign"), f.otherProject.ID)

	decision := f.authz.CheckAccess(ctx, f.identity, sa, ActionAssign)
	assert.False(t, decision.Allowed, "an agent must not assign another project's service account")
}

// TestAuthz_AgentAssignBaseline_ResourceTypeBoundary is the scope-creep guard.
// Assign must not be granted on non-SA resource types.
// CO1: ActionAssign is blocked by the agent scope restriction for all resource
// types since no AgentScopes mapping exists for assign permissions.
func TestAuthz_AgentAssignBaseline_ResourceTypeBoundary(t *testing.T) {
	f := newAgentBaselineFixture(t)
	ctx := context.Background()

	// Every fixture below is in the agent's OWN project, so the project half of
	// the predicate matches and only the resource-type half can deny.
	others := map[string]Resource{
		"agent": agentResource(&store.Agent{
			ID: tid("assign-boundary-agent"), ProjectID: f.ownProject.ID,
		}),
		"project": projectResource(f.ownProject),
		"harness config": harnessConfigResource(&store.HarnessConfig{
			ID: tid("assign-boundary-harness"), Scope: store.HarnessConfigScopeProject,
			ScopeID: f.ownProject.ID,
		}),
		"group": groupResource(&store.Group{
			ID: tid("assign-boundary-group"), ProjectID: f.ownProject.ID,
		}),
	}

	for name, resource := range others {
		t.Run(name, func(t *testing.T) {
			decision := f.authz.CheckAccess(ctx, f.identity, resource, ActionAssign)
			assert.False(t, decision.Allowed,
				"assign must not be granted on resource type %q", resource.Type)
		})
	}
}

// TestAuthz_AgentAssignBaseline_ActionBoundary verifies that non-read, non-assign
// actions on service accounts are denied for agents.
//
// CO1: All gcp_service_account.* permissions lack AgentScopes mappings, so the
// agent scope restriction blocks them all. Both assign and read are denied at
// the CheckAccess level for agent callers.
func TestAuthz_AgentAssignBaseline_ActionBoundary(t *testing.T) {
	f := newAgentBaselineFixture(t)
	ctx := context.Background()

	sa := projectSA(t, tid("assign-action-boundary"), f.ownProject.ID)

	for _, action := range []Action{
		ActionDelete, ActionVerify, ActionMint, ActionUpdate,
		ActionCreate, ActionManage, ActionAttach,
	} {
		t.Run(string(action), func(t *testing.T) {
			decision := f.authz.CheckAccess(ctx, f.identity, sa, action)
			assert.False(t, decision.Allowed,
				"action %s must not be granted on a service account", action)
		})
	}

	// CO1: gcp_service_account.read also has no AgentScopes mapping, so it is
	// blocked by the scope restriction. SA reads for agents are handled at the
	// handler level, not through the authz kernel.
	readDecision := f.authz.CheckAccess(ctx, f.identity, sa, ActionRead)
	assert.False(t, readDecision.Allowed,
		"CO1: gcp_service_account.read blocked by agent scope restriction")
}

// TestAuthz_AgentAssignBaseline_HubScopedDenied verifies that hub-scoped
// service accounts are not assignable by agents at the authz kernel level.
//
// CO1: gcp_service_account.assign has no AgentScopes mapping, so the agent
// scope restriction blocks it regardless of scope (project or hub).
func TestAuthz_AgentAssignBaseline_HubScopedDenied(t *testing.T) {
	f := newAgentBaselineFixture(t)
	ctx := context.Background()

	hubSA := gcpServiceAccountResource(&store.GCPServiceAccount{
		ID:      tid("assign-sa-hub"),
		Scope:   store.ScopeHub,
		ScopeID: tid("some-hub"),
		Email:   "hub-sa@example.iam.gserviceaccount.com",
	})
	require.Empty(t, hubSA.ParentType,
		"a hub-scoped SA must be parentless for this test to mean anything")
	require.Empty(t, projectIDForResource(hubSA))

	decision := f.authz.CheckAccess(ctx, f.identity, hubSA, ActionAssign)
	assert.False(t, decision.Allowed,
		"a hub-scoped service account must not be assignable by an agent")

	// The same guard, from the other side: an agent carrying no project must
	// not match a parentless resource either.
	projectless := &agentIdentityWrapper{&AgentTokenClaims{Claims: jwt.Claims{Subject: f.agent.ID}}}
	projectlessDecision := f.authz.CheckAccess(ctx, projectless, hubSA, ActionAssign)
	assert.False(t, projectlessDecision.Allowed)
}

// TestAuthz_AgentAssignBaseline_RevocableByDenyPolicy verifies that SA
// assignment and read are both denied for agent callers at the authz kernel level.
//
// CO1: Policies are gone. gcp_service_account.assign and gcp_service_account.read
// have no AgentScopes mappings, so the agent scope restriction blocks both.
// This replaces the old policy-based revocation test.
func TestAuthz_AgentAssignBaseline_RevocableByDenyPolicy(t *testing.T) {
	f := newAgentBaselineFixture(t)
	ctx := context.Background()

	sa := projectSA(t, tid("assign-sa-revoke"), f.ownProject.ID)

	// CO1: gcp_service_account.assign blocked by agent scope restriction.
	decision := f.authz.CheckAccess(ctx, f.identity, sa, ActionAssign)
	assert.False(t, decision.Allowed,
		"CO1: gcp_service_account.assign blocked by agent scope restriction")

	// CO1: gcp_service_account.read also blocked by agent scope restriction.
	readDecision := f.authz.CheckAccess(ctx, f.identity, sa, ActionRead)
	assert.False(t, readDecision.Allowed,
		"CO1: gcp_service_account.read blocked by agent scope restriction")
}

// TestAuthz_AgentAssignBaseline_DoesNotInheritReadPolicy records the limit:
// a grant to read a service account is not a grant to assign one.
//
// CO1: Policies are gone. gcp_service_account.read and gcp_service_account.assign
// both lack AgentScopes mappings, so neither works at the authz kernel level
// for agent callers. This test verifies both are denied.
func TestAuthz_AgentAssignBaseline_DoesNotInheritReadPolicy(t *testing.T) {
	f := newAgentBaselineFixture(t)
	ctx := context.Background()

	foreignSA := projectSA(t, tid("assign-sa-readpolicy"), f.otherProject.ID)

	// CO1: gcp_service_account.read blocked by agent scope restriction.
	readDecision := f.authz.CheckAccess(ctx, f.identity, foreignSA, ActionRead)
	assert.False(t, readDecision.Allowed,
		"CO1: gcp_service_account.read blocked by agent scope restriction")

	// Assign is also denied.
	assignDecision := f.authz.CheckAccess(ctx, f.identity, foreignSA, ActionAssign)
	assert.False(t, assignDecision.Allowed,
		"a read grant must not confer assign; the operator must grant assign explicitly")
}

// TestAuthz_AgentAssignBaseline_AllowPolicyStillWins verifies that role bindings
// correctly grant access through the kernel when the scope restriction permits.
//
// CO1: gcp_service_account.assign has no AgentScopes mapping, so even a role
// binding granting it is blocked by the scope restriction. This test verifies
// the denial. SA assignment for agents is handled at the handler level.
func TestAuthz_AgentAssignBaseline_AllowPolicyStillWins(t *testing.T) {
	f := newAgentBaselineFixture(t)
	ctx := context.Background()

	sa := projectSA(t, tid("assign-sa-allowpolicy"), f.ownProject.ID)

	// CO1: Even with the fixture's role binding, gcp_service_account.assign
	// is blocked by the agent scope restriction.
	decision := f.authz.CheckAccess(ctx, f.identity, sa, ActionAssign)
	assert.False(t, decision.Allowed,
		"CO1: gcp_service_account.assign blocked by agent scope restriction")
}

// TestAuthz_AssignIsNotReadClass pins that the arm was added without widening
// the read-class set. Widening isReadClassAction would have granted assign on
// every resource type rather than on service accounts alone.
func TestAuthz_AssignIsNotReadClass(t *testing.T) {
	assert.False(t, isReadClassAction(ActionAssign),
		"ActionAssign must not be read-class; the assign baseline is a separate, resource-type-scoped arm")
}
