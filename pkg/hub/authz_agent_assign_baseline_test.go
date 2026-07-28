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

// assignBaselineReason is the Decision.Reason emitted by the assign baseline.
// Asserted explicitly so a baseline allow is distinguishable from a policy
// allow, and so this arm is distinguishable from the read baseline.
const assignBaselineReason = "agent project service-account assign baseline"

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

// TestAuthz_AgentAssignBaseline_AllowsOwnProject covers the traffic the arm
// exists to keep working: an agent assigning a service account that lives in
// its own project.
func TestAuthz_AgentAssignBaseline_AllowsOwnProject(t *testing.T) {
	f := newAgentBaselineFixture(t)
	ctx := context.Background()

	sa := projectSA(t, tid("assign-sa-own"), f.ownProject.ID)

	decision := f.authz.CheckAccess(ctx, f.identity, sa, ActionAssign)
	assert.True(t, decision.Allowed, "expected allow, got: %s", decision.Reason)
	assert.Equal(t, assignBaselineReason, decision.Reason)
	assert.Equal(t, "project", decision.Scope)
	assert.Empty(t, decision.PolicyID, "baseline allow must not claim a policy")
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
func TestAuthz_AgentAssignBaseline_CrossProjectDenied(t *testing.T) {
	f := newAgentBaselineFixture(t)
	ctx := context.Background()

	sa := projectSA(t, tid("assign-sa-foreign"), f.otherProject.ID)

	decision := f.authz.CheckAccess(ctx, f.identity, sa, ActionAssign)
	assert.False(t, decision.Allowed, "an agent must not assign another project's service account")
	assert.Equal(t, "default deny", decision.Reason)
}

// TestAuthz_AgentAssignBaseline_ResourceTypeBoundary is the scope-creep guard.
// The arm must grant assign on gcp_service_account and on nothing else.
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
				"the assign baseline must not reach resource type %q", resource.Type)
			assert.Equal(t, "default deny", decision.Reason)
		})
	}
}

// TestAuthz_AgentAssignBaseline_ActionBoundary pins the other half of the
// conjunction: on a service account in its own project, an agent gets assign
// and read, and nothing else.
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
			assert.Equal(t, "default deny", decision.Reason)
		})
	}

	// Read still works, via the separate read baseline at step 3.
	readDecision := f.authz.CheckAccess(ctx, f.identity, sa, ActionRead)
	assert.True(t, readDecision.Allowed)
	assert.Equal(t, baselineReason, readDecision.Reason,
		"read must still be served by the read baseline, not the assign arm")
}

// TestAuthz_AgentAssignBaseline_HubScopedDenied is the Goal 2 tripwire.
//
// A hub-scoped service account is parentless — gcpServiceAccountResource gives
// a project parent only to project-scoped accounts — so projectIDForResource
// returns "" and the pid != "" guard stops the arm firing. That is correct
// today. Goal 2 relaxes scope confinement, at which point this arm becomes the
// only thing standing between any agent and any service account on the hub.
//
// If this test starts failing, scope confinement has been relaxed. Do not
// simply update it: the coupling is escalated to ptone as task #19 and the
// relaxation must not land before it is ruled.
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
		"a hub-scoped service account must not be assignable via the project baseline")
	assert.Equal(t, "default deny", decision.Reason)

	// The same guard, from the other side: an agent carrying no project must
	// not match a parentless resource either.
	projectless := &evaluateAgentIdentity{id: f.agent.ID, projectID: ""}
	projectlessDecision := f.authz.CheckAccess(ctx, projectless, hubSA, ActionAssign)
	assert.False(t, projectlessDecision.Allowed)
}

// TestAuthz_AgentAssignBaseline_RevocableByDenyPolicy pins the placement
// decision. The arm runs after evaluatePolicies, so an explicit deny bound to
// the project's implicit "project:<slug>:agents" group wins. If the arm is
// moved ahead of policy evaluation, this test fails.
func TestAuthz_AgentAssignBaseline_RevocableByDenyPolicy(t *testing.T) {
	f := newAgentBaselineFixture(t)
	ctx := context.Background()

	denyPolicy := &store.Policy{
		ID:           tid("assign-deny-policy"),
		Name:         "Deny agent SA assignment in own project",
		ScopeType:    "project",
		ScopeID:      f.ownProject.ID,
		ResourceType: "gcp_service_account",
		Actions:      []string{"assign"},
		Effect:       "deny",
	}
	require.NoError(t, f.store.CreatePolicy(ctx, denyPolicy))
	require.NoError(t, f.store.AddPolicyBinding(ctx, &store.PolicyBinding{
		PolicyID:      denyPolicy.ID,
		PrincipalType: "group",
		PrincipalID:   f.agentsGroup.ID,
	}))

	sa := projectSA(t, tid("assign-sa-revoke"), f.ownProject.ID)

	decision := f.authz.CheckAccess(ctx, f.identity, sa, ActionAssign)
	assert.False(t, decision.Allowed,
		"an explicit deny bound to project:<slug>:agents must override the assign baseline")
	assert.Equal(t, denyPolicy.ID, decision.PolicyID)

	// Read is a different action, so the deny above does not cover it and the
	// read baseline still applies. Confirms the deny won on its merits rather
	// than merely disabling the arm.
	readDecision := f.authz.CheckAccess(ctx, f.identity, sa, ActionRead)
	assert.True(t, readDecision.Allowed)
	assert.Equal(t, baselineReason, readDecision.Reason)
}

// TestAuthz_AgentAssignBaseline_DoesNotInheritReadPolicy records the limit of
// the parity claim, so that "reachability-preserving" is not later read as
// covering more than it does.
//
// Under ActionRead an agent could reach the assignment gate via a
// hand-authored read policy. That path is deliberately NOT carried over: a
// grant to read a service account is not a grant to assign one, and mirroring
// it would over-grant on the very surface the conversion exists to gate. Such
// an operator must grant assign explicitly.
func TestAuthz_AgentAssignBaseline_DoesNotInheritReadPolicy(t *testing.T) {
	f := newAgentBaselineFixture(t)
	ctx := context.Background()

	// A hand-authored policy granting agents read on service accounts hub-wide,
	// including in another project.
	readPolicy := &store.Policy{
		ID:           tid("assign-read-policy"),
		Name:         "Allow agent SA reads hub-wide",
		ScopeType:    "hub",
		ResourceType: "gcp_service_account",
		Actions:      []string{"read"},
		Effect:       "allow",
	}
	require.NoError(t, f.store.CreatePolicy(ctx, readPolicy))
	require.NoError(t, f.store.AddPolicyBinding(ctx, &store.PolicyBinding{
		PolicyID:      readPolicy.ID,
		PrincipalType: "group",
		PrincipalID:   f.agentsGroup.ID,
	}))

	foreignSA := projectSA(t, tid("assign-sa-readpolicy"), f.otherProject.ID)

	// The read policy does grant read, including cross-project.
	readDecision := f.authz.CheckAccess(ctx, f.identity, foreignSA, ActionRead)
	require.True(t, readDecision.Allowed, "fixture precondition: the read policy grants read")
	assert.Equal(t, readPolicy.ID, readDecision.PolicyID)

	// It does not grant assign.
	assignDecision := f.authz.CheckAccess(ctx, f.identity, foreignSA, ActionAssign)
	assert.False(t, assignDecision.Allowed,
		"a read grant must not confer assign; the operator must grant assign explicitly")
	assert.Equal(t, "default deny", assignDecision.Reason)
}

// TestAuthz_AgentAssignBaseline_AllowPolicyStillWins ensures the arm does not
// shadow a matching allow policy: policy evaluation runs first and keeps its
// attribution, which the baseline path does not set.
func TestAuthz_AgentAssignBaseline_AllowPolicyStillWins(t *testing.T) {
	f := newAgentBaselineFixture(t)
	ctx := context.Background()

	allowPolicy := &store.Policy{
		ID:           tid("assign-allow-policy"),
		Name:         "Allow agent SA assignment hub-wide",
		ScopeType:    "hub",
		ResourceType: "gcp_service_account",
		Actions:      []string{"assign"},
		Effect:       "allow",
	}
	require.NoError(t, f.store.CreatePolicy(ctx, allowPolicy))
	require.NoError(t, f.store.AddPolicyBinding(ctx, &store.PolicyBinding{
		PolicyID:      allowPolicy.ID,
		PrincipalType: "group",
		PrincipalID:   f.agentsGroup.ID,
	}))

	sa := projectSA(t, tid("assign-sa-allowpolicy"), f.ownProject.ID)

	decision := f.authz.CheckAccess(ctx, f.identity, sa, ActionAssign)
	assert.True(t, decision.Allowed)
	assert.Equal(t, allowPolicy.ID, decision.PolicyID)
	assert.Equal(t, "policy match", decision.Reason)
}

// TestAuthz_AssignIsNotReadClass pins that the arm was added without widening
// the read-class set. Widening isReadClassAction would have granted assign on
// every resource type rather than on service accounts alone.
func TestAuthz_AssignIsNotReadClass(t *testing.T) {
	assert.False(t, isReadClassAction(ActionAssign),
		"ActionAssign must not be read-class; the assign baseline is a separate, resource-type-scoped arm")
}
