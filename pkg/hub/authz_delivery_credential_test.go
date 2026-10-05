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

// Tests for the internal delivery credential (ptone/scion#2228 part 2): the
// hub_delivery type, its constructor, the identity classification arms,
// Step 0b, the 7b delivery_credential restriction, the stage-2/progeny
// evidence reads and the step-10 gate arm's terminal deny.
// deliveryCredentialKinds stays empty throughout: these tests reach later
// pipeline stages only through withDeliveryCredentialKinds or a direct
// checkDelegationCeiling call.

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/permissions"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newHubDeliveryTestAgent creates a store.Agent in projectID, descending
// from ownerID, for a hubDeliveryIdentity built through the real
// constructor. It also records the active user-to-agent delegation edge in
// projectID, so the execution-project relationship stage resolves ownerID
// as the agent's authoritative source user and later stages decide the
// outcome. ownerID must be a stored user admitted to projectID.
func newHubDeliveryTestAgent(t *testing.T, s store.Store, agentID, projectID, ownerID string) {
	t.Helper()
	require.NoError(t, s.CreateAgent(context.Background(), &store.Agent{
		ID: agentID, Slug: "slug-" + agentID[:8], Name: "name-" + agentID[:8],
		ProjectID: projectID, Phase: string(state.PhaseRunning),
		OwnerID: ownerID, CreatedBy: ownerID, Ancestry: []string{ownerID},
	}))
	// A recorded edge (session provenance, principal ceiling): delivery
	// permissions require recorded provenance on every hop, so an edge
	// without provenance would deny the ordinary-proof controls below.
	seedRecordedDelegationEdge(t, s, store.DelegationPrincipalUser, ownerID, store.DelegationPrincipalAgent, agentID,
		store.RoleScopeProject, projectID, string(AgentRoleFull))
}

// TestHubDelivery_PipelineReachesRelationshipStage is the end-to-end
// vertical slice for the hub_delivery credential: under
// withDeliveryCredentialKinds(hub_delivery), a hub_delivery request for a
// progeny child reaches the relationship stage, the progeny candidate is
// accepted at stages 2-5 using the credential's stored-agent evidence, and
// step 10 denies with the gate arm's terminal deny — deliveryCredentialKinds
// gates Step 0 only; the step-10 admission walk is a separate, later change.
func TestHubDelivery_PipelineReachesRelationshipStage(t *testing.T) {
	f := newGoldenFixture(t)
	withDeliveryCredentialKinds(t, CredentialKindHubDelivery)

	// The fixture seeds a live execution source from the start — the
	// opted-in secret's owner (f.projectOwnerID) is an active user, and the
	// agent's own ancestry names it directly — so this test's outcome does
	// not depend on whether a relationship-source stage beyond the ones
	// exercised here also runs for this request.
	agentC := tid("hd-pipeline-agent")
	newHubDeliveryTestAgent(t, f.store, agentC, f.projectAlpha.ID, f.projectOwnerID)

	h, err := f.authz.newHubDeliveryIdentity(context.Background(), agentC)
	require.NoError(t, err)

	d := decidePerm(f.authz, h, Resource{Type: "secret", ID: f.secretID}, ActionDeliver, "secret.deliver", true)
	assert.False(t, d.Allowed, "reason %q", d.Reason)
	assert.Equal(t, "delivery credential admission is not enabled", d.Reason)
	assert.Equal(t, DenyCause(""), d.DenyCause)

	r := relationshipResult(t, d, RelationshipRuleProgeny)
	assert.True(t, r.Accepted, "progeny candidate must be accepted at stages 2-5: %+v", r)
}

// TestHubDelivery_ListPredicateAndPointEvaluationMatchNothing pins
// non-attestation: ProgenyListPredicate and
// EvaluateProgeny call relationshipAncestryAttested, which is false for a
// hub_delivery principal, so they match nothing — even though
// the same stored agent record, wrapped in a storedAgentIdentity, is a
// positive control that does match.
func TestHubDelivery_ListPredicateAndPointEvaluationMatchNothing(t *testing.T) {
	f := newGoldenFixture(t)
	ctx := context.Background()

	agentC := tid("hd-list-agent")
	newHubDeliveryTestAgent(t, f.store, agentC, f.projectAlpha.ID, f.projectOwnerID)

	h, err := f.authz.newHubDeliveryIdentity(ctx, agentC)
	require.NoError(t, err)

	hdPrincipal := PrincipalContext{Kind: PrincipalKindAgent, ID: h.ID(), Identity: h}
	pred := f.authz.ProgenyListPredicate(ctx, hdPrincipal, "secret")
	assert.Equal(t, ProgenyPredicate{Kind: "secret"}, pred,
		"a hub_delivery principal must not be treated as attested by the list predicate")

	src := SharingSource{Kind: "secret", ID: f.secretID, OwnerID: f.projectOwnerID, Policy: SharingPolicyOptInRequired, OptedIn: true}
	assert.False(t, f.authz.EvaluateProgeny(ctx, hdPrincipal, src),
		"a hub_delivery principal must not be admitted by a point progeny evaluation")

	// Positive control: the same agent, as a storedAgentIdentity (hub-attested),
	// does match — showing the negative results above are about the
	// hub_delivery identity specifically, not the fixture.
	stored := &storedAgentIdentity{agent: &store.Agent{ID: agentC, ProjectID: f.projectAlpha.ID, Ancestry: []string{f.projectOwnerID}}}
	storedPrincipal := PrincipalContext{Kind: PrincipalKindAgent, ID: agentC, Identity: stored}
	assert.True(t, f.authz.EvaluateProgeny(ctx, storedPrincipal, src), "positive control must match")
}

// TestHubDelivery_Step10ArmDeniesWithoutStep0b calls checkDelegationCeiling
// directly, so Step 0b is never consulted, and shows the step-10 gate arm
// denies every condition on its own.
//
// The fixture isolates the arm: the valid agent (and the other agent named
// by the bound-agent rows) is a stored agent with a recorded delegation edge
// from an active super-admin, so the ordinary delegation proof
// (walkDelegationChain) allows the same request for a storedAgentIdentity
// principal — the positive control below. Every hub_delivery row then runs
// twice:
//   - against the fixture's AuthzService, where a fall-through to the
//     ordinary proof would allow, so allowed == false is a real assertion;
//   - against an AuthzService whose store fails every delegation edge, user
//     and role-binding read — the reads the ordinary proof performs — so a
//     fall-through to that proof surfaces as a non-nil error.
//
// Each run asserts allowed == false, err == nil, the exact reason,
// DenyCause == "", a delegation_ceiling_delivery_denied step, and no
// delegation_ceiling_allowed step.
func TestHubDelivery_Step10ArmDeniesWithoutStep0b(t *testing.T) {
	f := newGoldenFixture(t)
	ctx := context.Background()

	validAgentID := tid("hd-step10-valid-agent")
	otherAgentID := tid("hd-step10-other-agent")
	newHubDeliveryTestAgent(t, f.store, validAgentID, f.projectAlpha.ID, f.superAdminID)
	newHubDeliveryTestAgent(t, f.store, otherAgentID, f.projectAlpha.ID, f.superAdminID)

	failing := NewAuthzService(&materialFailingStore{
		Store:                            f.store,
		getUserErr:                       errors.New("injected user lookup failure"),
		listRoleBindingsForPrincipalErr:  errors.New("injected role binding lookup failure"),
		listRoleBindingsForPrincipalsErr: errors.New("injected role binding lookup failure"),
		getDelegationEdgesForDelegateErr: errors.New("injected edge lookup failure"),
	}, slog.Default())

	secretRes := Resource{Type: "secret", ID: f.secretID}

	// Positive control: the same agents, as stored-agent identities, are
	// allowed by the ordinary proof for the deliver request, and the
	// failing-store service turns that proof into an error. So a
	// hub_delivery row below that fell through to the ordinary proof would
	// be allowed (fixture service) or fail with an error (failing service).
	t.Run("positive_control_stored_agent", func(t *testing.T) {
		for _, id := range []string{validAgentID, otherAgentID} {
			agent, err := f.store.GetAgent(ctx, id)
			require.NoError(t, err)
			req := AuthzRequest{
				Principal:  PrincipalContext{Kind: PrincipalKindAgent, ID: id, Identity: &storedAgentIdentity{agent: agent}},
				Resource:   secretRes,
				Action:     ActionDeliver,
				Permission: "secret.deliver",
			}
			var explain []DecisionStep
			allowed, reason, err := f.authz.checkDelegationCeiling(ctx, req, "secret.deliver", id, &explain, nil)
			require.NoError(t, err)
			require.True(t, allowed, "the ordinary proof must allow the control request: %q", reason)
			require.True(t, hasDecisionStep(explain, "delegation_ceiling_allowed"), "explain: %+v", explain)

			_, _, err = failing.checkDelegationCeiling(ctx, req, "secret.deliver", id, nil, nil)
			require.Error(t, err, "the failing-store service must surface the ordinary proof's store read")
		}
	})

	baseIdentity := func() *hubDeliveryIdentity {
		return &hubDeliveryIdentity{
			agentID:      validAgentID,
			projectID:    f.projectAlpha.ID,
			ancestry:     []string{f.superAdminID},
			originUserID: f.superAdminID,
			boundAgentID: validAgentID,
		}
	}

	cases := []struct {
		name       string
		identity   *hubDeliveryIdentity
		typedNil   bool
		agentIDArg string // argument passed to checkDelegationCeiling; defaults to validAgentID
		permission string
		action     Action
		resource   Resource
		wantReason string
	}{
		{
			name:       "wrong_bound",
			identity:   func() *hubDeliveryIdentity { h := baseIdentity(); h.boundAgentID = otherAgentID; return h }(),
			permission: "secret.deliver", action: ActionDeliver,
			resource:   secretRes,
			wantReason: "delivery credential is bound to a different agent",
		},
		{
			name:       "empty_bound",
			identity:   func() *hubDeliveryIdentity { h := baseIdentity(); h.boundAgentID = ""; return h }(),
			permission: "secret.deliver", action: ActionDeliver,
			resource:   secretRes,
			wantReason: "delivery credential is bound to a different agent",
		},
		{
			name:       "agentID_argument_differs",
			identity:   baseIdentity(),
			agentIDArg: otherAgentID,
			permission: "secret.deliver", action: ActionDeliver,
			resource:   secretRes,
			wantReason: "delivery credential is bound to a different agent",
		},
		{
			// The bound agent and the agentID argument agree with each
			// other but not with the identity's own agentID, so only the
			// identity-ID comparison denies this row.
			name: "identity_agentID_differs_from_bound_and_argument",
			identity: func() *hubDeliveryIdentity {
				h := baseIdentity()
				h.boundAgentID = otherAgentID
				return h
			}(),
			agentIDArg: otherAgentID,
			permission: "secret.deliver", action: ActionDeliver,
			resource:   secretRes,
			wantReason: "delivery credential is bound to a different agent",
		},
		{
			name:       "action_read",
			identity:   baseIdentity(),
			permission: "secret.deliver", action: ActionRead,
			resource:   secretRes,
			wantReason: "delivery credential is limited to deliver permissions",
		},
		{
			name:       "action_use",
			identity:   baseIdentity(),
			permission: "secret.deliver", action: ActionUse,
			resource:   secretRes,
			wantReason: "delivery credential is limited to deliver permissions",
		},
		{
			name:       "permission_read",
			identity:   baseIdentity(),
			permission: "secret.read", action: ActionDeliver,
			resource:   secretRes,
			wantReason: "delivery credential is limited to deliver permissions",
		},
		{
			// An empty permission argument is not one of the three deliver
			// permissions, so it denies the same way an explicit
			// non-deliver permission does.
			name:       "permission_empty",
			identity:   baseIdentity(),
			permission: "", action: ActionDeliver,
			resource:   Resource{Type: "agent", ID: validAgentID},
			wantReason: "delivery credential is limited to deliver permissions",
		},
		{
			name:       "typed_nil",
			typedNil:   true,
			permission: "secret.deliver", action: ActionDeliver,
			resource:   secretRes,
			wantReason: "delivery credential is missing",
		},
		{
			name:       "empty_project",
			identity:   func() *hubDeliveryIdentity { h := baseIdentity(); h.projectID = ""; return h }(),
			permission: "secret.deliver", action: ActionDeliver,
			resource:   secretRes,
			wantReason: "delivery credential has no project",
		},
		{
			// Every condition holds: the correct bound agent, a deliver
			// permission, ActionDeliver and a non-empty project. The arm's
			// terminal deny decides it.
			name:       "valid",
			identity:   baseIdentity(),
			permission: "secret.deliver", action: ActionDeliver,
			resource:   secretRes,
			wantReason: "delivery credential admission is not enabled",
		},
	}

	services := []struct {
		name  string
		authz *AuthzService
	}{
		{"fixture_store", f.authz},
		{"failing_store", failing},
	}

	for _, tc := range cases {
		for _, svc := range services {
			t.Run(tc.name+"/"+svc.name, func(t *testing.T) {
				var identity Identity
				if tc.typedNil {
					identity = (*hubDeliveryIdentity)(nil)
				} else {
					identity = tc.identity
				}
				agentIDArg := tc.agentIDArg
				if agentIDArg == "" {
					agentIDArg = validAgentID
				}
				req := AuthzRequest{
					Principal:  PrincipalContext{Kind: PrincipalKindAgent, ID: validAgentID, Identity: identity},
					Resource:   tc.resource,
					Action:     tc.action,
					Permission: tc.permission,
				}
				var explain []DecisionStep
				var cause DenyCause
				allowed, reason, err := svc.authz.checkDelegationCeiling(ctx, req, tc.permission, agentIDArg, &explain, &cause)
				require.NoError(t, err, "the arm performs no store read and never reaches the ordinary proof")
				assert.False(t, allowed)
				assert.Equal(t, tc.wantReason, reason)
				assert.Equal(t, DenyCause(""), cause)
				assert.True(t, hasDecisionStep(explain, "delegation_ceiling_delivery_denied"), "explain: %+v", explain)
				assert.False(t, hasDecisionStep(explain, "delegation_ceiling_allowed"), "the ordinary proof must never run for this type")
			})
		}
	}
}

// hasDecisionStep reports whether explain contains a step named step.
func hasDecisionStep(explain []DecisionStep, step string) bool {
	for _, s := range explain {
		if s.Step == step {
			return true
		}
	}
	return false
}

// newHubDeliveryNoItemGrantIdentity builds a hub_delivery identity for the
// role-does-not-substitute fixture: agent C's ancestry names an alpha
// project member with no opted-in secret, env var or skill injection, so no association, progeny
// or skill-default grant exists for it on any golden fixture resource. A
// role binding naming a deliver permission is therefore the only grant the
// kernel could match for it.
func newHubDeliveryNoItemGrantIdentity(t *testing.T, f *goldenFixture, agentID string) *hubDeliveryIdentity {
	t.Helper()
	newHubDeliveryTestAgent(t, f.store, agentID, f.projectAlpha.ID, f.memberAlphaID)
	h, err := f.authz.newHubDeliveryIdentity(context.Background(), agentID)
	require.NoError(t, err)
	return h
}

// newDeliverRoleDefinition creates a minimal system-scope custom role
// naming only secret.deliver. The built-in super-admin role also holds it
// (through allPermissionIDs), but super-admin is direct-user-only
// (store/entadapter's directUserOnlyRoles) and cannot be bound to an agent
// or a group, so a role binding onto agent:<C> or a group needs its own
// role instead.
func newDeliverRoleDefinition(t *testing.T, s store.Store) *store.RoleDefinition {
	t.Helper()
	rd, err := s.CreateRoleDefinition(context.Background(), &store.RoleDefinition{
		Name:        "hd-role-only-deliver-" + tid(t.Name())[:8],
		Description: "holds secret.deliver only, for the role-does-not-substitute fixture",
		ScopeType:   store.RoleScopeSystem,
		Permissions: []string{"secret.deliver"},
	})
	require.NoError(t, err)
	return rd
}

// bindDeliverRoleToAgent gives agentID a system-scope role binding,
// naming only secret.deliver, directly.
func bindDeliverRoleToAgent(t *testing.T, s store.Store, agentID string) {
	t.Helper()
	ctx := context.Background()
	rd := newDeliverRoleDefinition(t, s)
	_, err := s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: rd.ID,
		PrincipalType:    store.RoleBindingPrincipalAgent,
		PrincipalID:      agentID,
		ScopeType:        store.RoleScopeSystem,
		CreatedBy:        store.SystemReconcileCreatedBy,
	})
	require.NoError(t, err)
}

// bindDeliverRoleToAgentGroup gives agentID the same role indirectly,
// through membership in a group holding the system-scope role binding
// (authorizationPrincipals adds GetEffectiveGroupsForAgent to the principal
// closure, authz.go).
func bindDeliverRoleToAgentGroup(t *testing.T, s store.Store, groupID, agentID string) {
	t.Helper()
	ctx := context.Background()
	require.NoError(t, s.CreateGroup(ctx, &store.Group{
		ID: groupID, Name: "hub delivery role-only group", Slug: "hd-role-only-" + groupID,
		GroupType: store.GroupTypeExplicit,
	}))
	require.NoError(t, s.AddGroupMember(ctx, &store.GroupMember{
		GroupID: groupID, MemberType: store.GroupMemberTypeAgent, MemberID: agentID, Role: store.GroupMemberRoleMember,
	}))
	rd := newDeliverRoleDefinition(t, s)
	_, err := s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: rd.ID,
		PrincipalType:    store.RoleBindingPrincipalGroup,
		PrincipalID:      groupID,
		ScopeType:        store.RoleScopeSystem,
		CreatedBy:        store.SystemReconcileCreatedBy,
	})
	require.NoError(t, err)
}

// TestHubDelivery_RoleGrantExcludedNonExplain pins the role-does-not-
// substitute rule without Explain: a role binding naming secret.deliver is
// excluded by Step 8b even when the caller never asks for the
// relationship-candidate provenance.
func TestHubDelivery_RoleGrantExcludedNonExplain(t *testing.T) {
	f := newGoldenFixture(t)
	withDeliveryCredentialKinds(t, CredentialKindHubDelivery)

	agentC := tid("hd-role-grant-non-explain")
	h := newHubDeliveryNoItemGrantIdentity(t, f, agentC)
	bindDeliverRoleToAgent(t, f.store, agentC)

	d := decidePerm(f.authz, h, Resource{Type: "secret", ID: f.secretID}, ActionDeliver, "secret.deliver", false)
	assert.False(t, d.Allowed, "reason %q", d.Reason)
	assert.Equal(t, deliverRoleGrantReason, d.Reason)
	assert.Empty(t, d.MatchedGrant)
	assert.Empty(t, d.RoleName)
	assert.Empty(t, d.BindingID)
	assert.Empty(t, d.Scope)
	require.NotNil(t, d.Provenance)
	assert.NotEmpty(t, d.Provenance.Grants, "the kernel-matched role binding is kept in Provenance.Grants for audit")
}

// TestHubDelivery_ProgenyCandidatePassesStage5WithRoleBinding asserts that a
// role binding does not short-circuit a progeny candidate: with both a role
// binding naming secret.deliver and a progeny grant, Explain=true shows the
// progeny candidate accepted at stage 5, the decision does not name the
// role, and the step-10 gate arm's terminal deny is the reason the request
// is denied.
func TestHubDelivery_ProgenyCandidatePassesStage5WithRoleBinding(t *testing.T) {
	f := newGoldenFixture(t)
	withDeliveryCredentialKinds(t, CredentialKindHubDelivery)

	agentC := tid("hd-progeny-with-role")
	// Ancestry names the golden secret's owner, so the progeny grant exists
	// independently of the role binding below.
	newHubDeliveryTestAgent(t, f.store, agentC, f.projectAlpha.ID, f.projectOwnerID)
	bindDeliverRoleToAgent(t, f.store, agentC)

	h, err := f.authz.newHubDeliveryIdentity(context.Background(), agentC)
	require.NoError(t, err)

	d := decidePerm(f.authz, h, Resource{Type: "secret", ID: f.secretID}, ActionDeliver, "secret.deliver", true)
	assert.False(t, d.Allowed, "reason %q", d.Reason)
	assert.Equal(t, "delivery credential admission is not enabled", d.Reason)
	assert.Empty(t, d.RoleName, "the decision must not name the excluded role grant")
	assert.Equal(t, ScopeTypeRelationship, d.Scope, "the decision names the relationship grant, not the role")

	r := relationshipResult(t, d, RelationshipRuleProgeny)
	assert.True(t, r.Accepted, "progeny candidate must be accepted at stage 5 despite the role binding: %+v", r)
}

// TestHubDelivery_EntryBlockRows is table-driven over Decide's entry-block
// treatment of the hub_delivery credential kind, keyed by subtest name so
// further rows can be added without restructuring this test. Every row
// except the override row is denied by the entry block itself, before the
// unsupported-principal switch and the Step 0 gate, so each asserts the
// entry block's exact reason with deliveryCredentialKinds left empty.
//
// override_ignores_supplied_credential pins the forced derived-credential
// override (authz.go, immediately after the entry block): a caller-supplied
// CredentialContext whose Kind matches the identity's own derived kind is
// accepted by the entry-block compatibility check, like any other kind, but
// for hub_delivery it is then replaced by the derived credential, so the
// caller's ID, Type, Scopes and Ceiling travel no further and change nothing
// about the outcome.
func TestHubDelivery_EntryBlockRows(t *testing.T) {
	f := newGoldenFixture(t)
	ctx := context.Background()

	agentC := tid("hd-entry-agent")
	newHubDeliveryTestAgent(t, f.store, agentC, f.projectAlpha.ID, f.projectOwnerID)
	h, err := f.authz.newHubDeliveryIdentity(ctx, agentC)
	require.NoError(t, err)

	owner := NewAuthenticatedUser(f.projectOwnerID, "owner@golden.test", "Owner", "member", "web")
	uat := NewScopedUserIdentity(owner, f.projectAlpha.ID, []string{"secret.deliver", "secret.read"})
	agentJWT := progenyPairAgent(tid("hd-entry-jwt-agent"), f.projectAlpha.ID, []string{f.projectOwnerID}, allRegisteredAgentScopes())
	fed := NewFederatedAgentIdentity("https://peer.example", tid("hd-entry-fed"), f.projectAlpha.ID,
		"fed", f.projectOwnerID, []string{f.projectOwnerID}, allRegisteredAgentScopes())
	dev := NewDevUser(DevUserConfig{Username: "dev", DisplayName: "Dev User", Email: "dev@golden.test"})
	broker := NewBrokerIdentity(tid("hd-entry-broker"))

	secret := Resource{Type: "secret", ID: f.secretID}
	suppliedHubDelivery := func(id Identity) AuthzRequest {
		return deliveryGateRequest(id, CredentialKindHubDelivery, secret, "secret.deliver")
	}

	const kindMismatch = "credential kind does not match identity"
	rows := []struct {
		name       string
		request    AuthzRequest
		wantReason string
	}{
		{
			name:       "hub_delivery_identity_supplied_agent_jwt",
			request:    deliveryGateRequest(h, CredentialKindAgentJWT, secret, "secret.deliver"),
			wantReason: kindMismatch,
		},
		{name: "agent_jwt_identity_supplied_hub_delivery", request: suppliedHubDelivery(agentJWT), wantReason: kindMismatch},
		{name: "interactive_identity_supplied_hub_delivery", request: suppliedHubDelivery(owner), wantReason: kindMismatch},
		{name: "uat_identity_supplied_hub_delivery", request: suppliedHubDelivery(uat), wantReason: kindMismatch},
		{name: "federated_identity_supplied_hub_delivery", request: suppliedHubDelivery(fed), wantReason: kindMismatch},
		{name: "dev_user_supplied_hub_delivery", request: suppliedHubDelivery(dev), wantReason: kindMismatch},
		{
			// The entry block denies the broker principal before the
			// unsupported-principal switch would.
			name:       "broker_identity_supplied_hub_delivery",
			request:    suppliedHubDelivery(broker),
			wantReason: kindMismatch,
		},
		{
			name: "hub_delivery_identity_principal_kind_user",
			request: AuthzRequest{
				Principal:  PrincipalContext{Kind: PrincipalKindUser, ID: h.ID(), Identity: h},
				Credential: CredentialContext{Kind: CredentialKindHubDelivery},
				Resource:   secret, Action: ActionDeliver, Permission: "secret.deliver", Explain: true,
			},
			wantReason: "principal kind does not match identity",
		},
		{
			name: "hub_delivery_identity_principal_id_differs",
			request: AuthzRequest{
				Principal:  PrincipalContext{Kind: PrincipalKindAgent, ID: tid("hd-entry-other-agent"), Identity: h},
				Credential: CredentialContext{Kind: CredentialKindHubDelivery},
				Resource:   secret, Action: ActionDeliver, Permission: "secret.deliver", Explain: true,
			},
			wantReason: "principal id does not match identity",
		},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			d := f.authz.Decide(ctx, row.request)
			assert.False(t, d.Allowed, "reason %q", d.Reason)
			assert.Equal(t, row.wantReason, d.Reason)
		})
	}

	t.Run("override_ignores_supplied_credential", func(t *testing.T) {
		// The request reaches the relationship stage, so a supplied Scopes
		// or Ceiling that travelled with the credential could change the
		// stage-5 restriction outcome.
		withDeliveryCredentialKinds(t, CredentialKindHubDelivery)

		baseReq := AuthzRequest{
			Principal:  principalContextForIdentity(h),
			Resource:   secret,
			Action:     ActionDeliver,
			Permission: "secret.deliver",
			Explain:    true,
		}
		derivedOnly := baseReq
		want := f.authz.Decide(ctx, derivedOnly)
		wantProgeny := relationshipResult(t, want, RelationshipRuleProgeny)
		require.True(t, wantProgeny.Accepted, "the derived-only request must reach and pass stage 5: %+v", wantProgeny)

		supplied := map[string]CredentialContext{
			"narrowing": {
				Kind: CredentialKindHubDelivery, ID: "x", Type: "x",
				Scopes:  []string{"secret.read", "env_var.read"},
				Ceiling: permissions.FrozenPermissionCeiling{PermissionIDs: []string{"secret.read"}},
			},
			"widening": {
				Kind: CredentialKindHubDelivery, ID: "x", Type: "x",
				Scopes:  []string{"secret.deliver", "env_var.deliver", "skill_injection.deliver", "agent.create"},
				Ceiling: permissions.FrozenPermissionCeiling{PermissionIDs: []string{"secret.deliver", "agent.create"}},
			},
		}
		for name, cred := range supplied {
			t.Run(name, func(t *testing.T) {
				req := baseReq
				req.Credential = cred
				got := f.authz.Decide(ctx, req)

				assert.Equal(t, string(CredentialKindHubDelivery), got.CredentialKind)
				assert.Empty(t, got.CredentialID, "no caller-supplied ID travels with the derived credential")
				assert.Empty(t, got.CredentialType, "no caller-supplied Type travels with the derived credential")
				assert.Equal(t, want.Allowed, got.Allowed, "a supplied credential must not change the outcome")
				assert.Equal(t, want.Reason, got.Reason, "a supplied credential must not change the outcome")
				assert.Equal(t, want.DenyCause, got.DenyCause)
				gotProgeny := relationshipResult(t, got, RelationshipRuleProgeny)
				assert.Equal(t, wantProgeny.Accepted, gotProgeny.Accepted)
				assert.Equal(t, wantProgeny.RejectedBy, gotProgeny.RejectedBy)
			})
		}
	})
}

// assertStep0bDenied asserts that Decide denied the request at Step 0b with
// reason: the explain provenance holds only that reason, and neither role
// binding nor relationship evaluation ran.
func assertStep0bDenied(t *testing.T, d Decision, reason, perm string) {
	t.Helper()
	assert.False(t, d.Allowed, "reason %q", d.Reason)
	assert.Equal(t, reason, d.Reason)
	assert.Equal(t, string(CredentialKindHubDelivery), d.CredentialKind)
	require.NotNil(t, d.Provenance)
	assert.Equal(t, []string{reason}, d.Provenance.DenyReasons)
	assert.Equal(t, perm, d.Provenance.Permission)
	assert.Empty(t, d.Provenance.Grants, "Step 0b precedes role binding evaluation")
	assert.Empty(t, d.Provenance.Relationships, "Step 0b precedes relationship evaluation")
}

// TestHubDelivery_Step0bDeniesNonDeliverRequests pins Step 0b's action and
// permission conjunction through Decide: with hub_delivery in the delivery
// set, so the Step 0 gate admits a deliver permission, a hub_delivery
// request for a deliver permission under any action other than
// ActionDeliver, or for any permission outside hubDeliveryPermissionIDs
// under any action including ActionDeliver, is denied at Step 0b. The fixture is a progeny child, so a request that got
// past Step 0b would reach relationship evaluation.
func TestHubDelivery_Step0bDeniesNonDeliverRequests(t *testing.T) {
	f := newGoldenFixture(t)
	withDeliveryCredentialKinds(t, CredentialKindHubDelivery)

	agentC := tid("hd-step0b-agent")
	newHubDeliveryTestAgent(t, f.store, agentC, f.projectAlpha.ID, f.projectOwnerID)
	h, err := f.authz.newHubDeliveryIdentity(context.Background(), agentC)
	require.NoError(t, err)

	secret := Resource{Type: "secret", ID: f.secretID}
	cases := []struct {
		name   string
		perm   string
		action Action
		res    Resource
	}{
		{"secret.deliver/action_read", "secret.deliver", ActionRead, secret},
		{"secret.deliver/action_use", "secret.deliver", ActionUse, secret},
		{"env_var.deliver/action_read", "env_var.deliver", ActionRead, Resource{Type: "env_var", ID: f.envVarID}},
		{"secret.read", "secret.read", ActionRead, secret},
		{"secret.use", "secret.use", ActionUse, secret},
		{"agent.create", "agent.create", ActionCreate, Resource{Type: "agent", ParentType: "project", ParentID: f.projectAlpha.ID}},
		// A non-deliver permission requested under ActionDeliver isolates
		// the permission arm from the action arm.
		{"secret.read/action_deliver", "secret.read", ActionDeliver, secret},
		{"agent.create/action_deliver", "agent.create", ActionDeliver, Resource{Type: "agent", ParentType: "project", ParentID: f.projectAlpha.ID}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := decidePerm(f.authz, h, tc.res, tc.action, tc.perm, true)
			assertStep0bDenied(t, d, "delivery credential is limited to deliver permissions", tc.perm)
		})
	}
}

// TestHubDelivery_AgentSyntheticBindingsSkipped pins the Step 5b and 5b2
// skips for a hub_delivery principal. 5b2 is reachable: a
// skill_injection.deliver request naming a hub-wide skill resource passes
// Step 0b and reaches it, and without the skip the agent skill-catalog
// binding would appear among the kernel's granting bindings. 5b adds nothing
// for this type either way, because Scopes() is always nil; the direct
// buildAgentSyntheticBindings assertion pins that property, which is what
// makes the 5b skip a statement of the rule rather than a change in
// outcome.
func TestHubDelivery_AgentSyntheticBindingsSkipped(t *testing.T) {
	f := newGoldenFixture(t)
	withDeliveryCredentialKinds(t, CredentialKindHubDelivery)

	agentC := tid("hd-synthetic-skip-agent")
	newHubDeliveryTestAgent(t, f.store, agentC, f.projectAlpha.ID, f.projectOwnerID)
	h, err := f.authz.newHubDeliveryIdentity(context.Background(), agentC)
	require.NoError(t, err)

	cands, roles := f.authz.buildAgentSyntheticBindings(h)
	assert.Empty(t, cands, "a hub_delivery principal has no JWT scopes to synthesize a binding from")
	assert.Empty(t, roles)

	d := decidePerm(f.authz, h, Resource{Type: "skill", ID: tid("hd-synthetic-skip-skill"), ScopeKind: store.SkillScopeGlobal},
		ActionDeliver, "skill_injection.deliver", true)
	assert.False(t, d.Allowed, "reason %q", d.Reason)
	require.NotNil(t, d.Provenance)
	for _, g := range append(append([]GrantDetail{}, d.Provenance.Grants...), d.Provenance.InactiveGrants...) {
		assert.False(t, strings.HasPrefix(g.BindingID, "synthetic:"),
			"no synthetic agent binding may be built for a hub_delivery principal: %+v", g)
	}
}

// TestHubDelivery_PermissionSetMatchesDeliverRegistry pins that the fixed
// hub_delivery permission set equals the registry-derived deliver set. A
// new registry *.deliver permission fails this test, so it is admitted
// under hub_delivery only through a deliberate edit of
// hubDeliveryPermissionIDs.
func TestHubDelivery_PermissionSetMatchesDeliverRegistry(t *testing.T) {
	assert.Equal(t, deliverPermissionIDs, hubDeliveryPermissionIDs)
}

// hubPackageSyntax parses every Go file in this package directory, without
// comments, keyed by file base name.
func hubPackageSyntax(t *testing.T) map[string]*ast.File {
	t.Helper()
	names, err := filepath.Glob("*.go")
	require.NoError(t, err)
	require.NotEmpty(t, names)
	fset := token.NewFileSet()
	files := make(map[string]*ast.File, len(names))
	for _, name := range names {
		file, err := parser.ParseFile(fset, name, nil, parser.SkipObjectResolution)
		require.NoError(t, err, name)
		files[name] = file
	}
	return files
}

// calledName returns the bare function or method name a call expression
// invokes, and its package or receiver qualifier when it has one.
func calledName(call *ast.CallExpr) (qualifier, name string) {
	switch fn := call.Fun.(type) {
	case *ast.Ident:
		return "", fn.Name
	case *ast.SelectorExpr:
		if x, ok := fn.X.(*ast.Ident); ok {
			return x.Name, fn.Sel.Name
		}
		return "", fn.Sel.Name
	}
	return "", ""
}

// referencesIdent reports whether node mentions an identifier named one of
// names.
func referencesIdent(node ast.Node, names ...string) bool {
	found := false
	ast.Inspect(node, func(n ast.Node) bool {
		if id, ok := n.(*ast.Ident); ok {
			for _, name := range names {
				found = found || id.Name == name
			}
		}
		return !found
	})
	return found
}

// TestHubDelivery_ConstructorCallSites pins, by AST scan, where a
// hubDeliveryIdentity can be produced: newHubDeliveryIdentity is called only
// from authz_delivery_credential.go and its test, and no other production
// file builds the type as a composite literal or with new.
func TestHubDelivery_ConstructorCallSites(t *testing.T) {
	allowedCallers := map[string]bool{
		"authz_delivery_credential.go":      true,
		"authz_delivery_credential_test.go": true,
	}
	calls := 0
	for name, file := range hubPackageSyntax(t) {
		isTest := strings.HasSuffix(name, "_test.go")
		ast.Inspect(file, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.CallExpr:
				_, fn := calledName(n)
				if fn == "newHubDeliveryIdentity" {
					calls++
					assert.True(t, allowedCallers[name], "newHubDeliveryIdentity is called from %s", name)
				}
				if fn == "new" && len(n.Args) == 1 && !isTest && name != "authz_delivery_credential.go" {
					assert.False(t, referencesIdent(n.Args[0], "hubDeliveryIdentity"), "new(hubDeliveryIdentity) in %s", name)
				}
			case *ast.CompositeLit:
				if !isTest && name != "authz_delivery_credential.go" && n.Type != nil {
					assert.False(t, referencesIdent(n.Type, "hubDeliveryIdentity"), "hubDeliveryIdentity literal in %s", name)
				}
			}
			return true
		})
	}
	assert.Positive(t, calls, "the scan must find the test call sites, or it is not scanning this package")
}

// TestHubDelivery_NotInRequestContext pins, by AST scan of the production
// files, that a hubDeliveryIdentity never enters a request context: no
// function that mentions the type or its constructor passes a value to a
// context setter (context.WithValue, or a contextWith* setter given any
// argument besides the parent context), the file holding the constructor
// passes no value to a context setter at all, and no *FromContext
// extractor mentions the type. A setter that takes only the parent
// context, such as contextWithDelegationCeilingCache, stores nothing the
// caller supplies and is not counted. Together with
// TestHubDelivery_ConstructorCallSites, the request-context identity
// extractors can never return one.
func TestHubDelivery_NotInRequestContext(t *testing.T) {
	isContextSetter := func(call *ast.CallExpr) bool {
		qualifier, fn := calledName(call)
		if qualifier == "context" && fn == "WithValue" {
			return true
		}
		return strings.HasPrefix(fn, "contextWith") && len(call.Args) > 1
	}
	typeNames := []string{"hubDeliveryIdentity", "newHubDeliveryIdentity"}
	mentioning := 0
	for name, file := range hubPackageSyntax(t) {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		if name == "authz_delivery_credential.go" {
			ast.Inspect(file, func(n ast.Node) bool {
				if call, ok := n.(*ast.CallExpr); ok {
					assert.False(t, isContextSetter(call), "%s stores a value in a context", name)
				}
				return true
			})
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil || !referencesIdent(fn.Body, typeNames...) {
				continue
			}
			mentioning++
			assert.False(t, strings.HasSuffix(fn.Name.Name, "FromContext"),
				"%s: request-context extractor %s mentions hubDeliveryIdentity", name, fn.Name.Name)
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				if call, ok := n.(*ast.CallExpr); ok {
					assert.False(t, isContextSetter(call),
						"%s: %s mentions hubDeliveryIdentity and stores a value in a context", name, fn.Name.Name)
				}
				return true
			})
		}
	}
	assert.Positive(t, mentioning, "the scan must find the functions that handle the type")
}

// TestHubDelivery_Stage2RejectsNonDeliverPermission runs the relationship
// stages directly, outside Decide, so Step 0b is never consulted. Stage 2
// reads a hub_delivery principal's attestation from its stored-agent
// evidence only for the three deliver permissions: for any other
// permission it reports not attested, so the progeny candidate is rejected
// with untrusted_ancestry. A paired secret.deliver row for the same
// principal, resource and candidate is accepted, so the rejection is caused
// by the permission alone. The nil-evidence rows pin the stage-2 and
// progeny-fact guards for a credential with no stored-agent evidence.
func TestHubDelivery_Stage2RejectsNonDeliverPermission(t *testing.T) {
	f := newGoldenFixture(t)
	ctx := context.Background()

	agentID := tid("hd-stage2-agent")
	newHubDeliveryTestAgent(t, f.store, agentID, f.projectAlpha.ID, f.projectOwnerID)
	h, err := f.authz.newHubDeliveryIdentity(ctx, agentID)
	require.NoError(t, err)
	require.NotNil(t, h.evidence)
	require.True(t, AncestryIsHubAttested(h.evidence), "the stored-agent evidence must be hub-attested")

	noEvidence := *h
	noEvidence.evidence = nil

	principalFor := func(identity *hubDeliveryIdentity) PrincipalContext {
		return PrincipalContext{Kind: PrincipalKindAgent, ID: identity.ID(), Identity: identity}
	}
	secretRes := Resource{Type: "secret", ID: f.secretID}

	// progenyCandidate returns the progeny candidate relationshipCandidates
	// builds for the request, failing the test if there is none.
	progenyCandidate := func(t *testing.T, principal PrincipalContext, action Action, permissionID string) relationshipCandidate {
		t.Helper()
		for _, c := range f.authz.relationshipCandidates(principal, secretRes, action, permissionID) {
			if c.rule == RelationshipRuleProgeny {
				return c
			}
		}
		t.Fatalf("no progeny candidate for %s/%s", permissionID, action)
		return relationshipCandidate{}
	}

	// runStages runs the candidate through runRelationshipStages with no
	// restrictions and returns whether it was accepted and the first
	// rejection kind and detail, if any.
	runStages := func(t *testing.T, principal PrincipalContext, action Action, permissionID string) (bool, string, string) {
		t.Helper()
		c := progenyCandidate(t, principal, action, permissionID)
		var rejectedBy, detail string
		policyKind := permissions.RelationshipPrincipalKind(string(principal.Kind))
		_, ok := f.authz.runRelationshipStages(ctx, principal, policyKind, secretRes, permissionID, nil, c,
			func(kind, d string) { rejectedBy, detail = kind, d })
		return ok, rejectedBy, detail
	}

	t.Run("stage2_entry", func(t *testing.T) {
		// secret.read is not listed for the progeny relationship on a
		// secret, so stage 1 rejects it before stage 2 runs; the stage-2
		// check itself is pinned directly here for it and for the other
		// non-deliver and empty permissions.
		for _, perm := range []string{"secret.read", "secret.use", permissionProjectSecretRead, ""} {
			assert.False(t, relationshipStageAncestryAttested(principalFor(h), perm),
				"stage 2 must not attest a hub_delivery principal for %q", perm)
		}
		for perm := range hubDeliveryPermissionIDs {
			assert.True(t, relationshipStageAncestryAttested(principalFor(h), perm),
				"stage 2 attests a hub_delivery principal with stored-agent evidence for %q", perm)
			assert.False(t, relationshipStageAncestryAttested(principalFor(&noEvidence), perm),
				"stage 2 must not attest a hub_delivery principal with no evidence for %q", perm)
		}
	})

	for _, tc := range []struct {
		name       string
		action     Action
		permission string
	}{
		// Both pairs pass stage 1 (relationship policy) for the progeny
		// rule on a secret, so stage 2 is the stage that decides them.
		{"secret_use", ActionUse, "secret.use"},
		{"project_secret_read", ActionRead, permissionProjectSecretRead},
	} {
		t.Run("non_deliver_rejected/"+tc.name, func(t *testing.T) {
			require.True(t, permissions.RelationshipPolicyAllows(string(RelationshipRuleProgeny), "agent", "secret", tc.permission),
				"the row must pass stage 1 so that stage 2 decides it")
			ok, rejectedBy, detail := runStages(t, principalFor(h), tc.action, tc.permission)
			assert.False(t, ok)
			assert.Equal(t, RelationshipRejectUntrustedAncestry, rejectedBy, "detail %q", detail)
		})
	}

	t.Run("deliver_accepted", func(t *testing.T) {
		ok, rejectedBy, detail := runStages(t, principalFor(h), ActionDeliver, "secret.deliver")
		assert.True(t, ok, "rejected by %q: %q", rejectedBy, detail)
		assert.Empty(t, rejectedBy)
	})

	t.Run("nil_evidence_rejected_at_stage2", func(t *testing.T) {
		ok, rejectedBy, detail := runStages(t, principalFor(&noEvidence), ActionDeliver, "secret.deliver")
		assert.False(t, ok)
		assert.Equal(t, RelationshipRejectUntrustedAncestry, rejectedBy, "detail %q", detail)
	})

	t.Run("nil_evidence_progeny_fact", func(t *testing.T) {
		// The fact closure is called directly, independently of stage 2.
		// With evidence it holds for this fixture; without evidence it
		// reports the named reason instead of reading the credential's own
		// ancestry.
		withEvidence := progenyCandidate(t, principalFor(h), ActionDeliver, "secret.deliver")
		require.NotNil(t, withEvidence.fact)
		_, holds, detail := withEvidence.fact(ctx)
		require.True(t, holds, "positive control: the fact holds with stored-agent evidence: %q", detail)

		c := progenyCandidate(t, principalFor(&noEvidence), ActionDeliver, "secret.deliver")
		require.NotNil(t, c.fact)
		src, holds, detail := c.fact(ctx)
		assert.False(t, holds)
		assert.Nil(t, src)
		assert.Equal(t, "delivery credential has no stored-agent evidence", detail)
	})
}
