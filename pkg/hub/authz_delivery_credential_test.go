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
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newHubDeliveryTestAgent creates a store.Agent in projectID, descending
// from ownerID, for a hubDeliveryIdentity built through the real
// constructor.
func newHubDeliveryTestAgent(t *testing.T, s store.Store, agentID, projectID, ownerID string) {
	t.Helper()
	require.NoError(t, s.CreateAgent(context.Background(), &store.Agent{
		ID: agentID, Slug: "slug-" + agentID[:8], Name: "name-" + agentID[:8],
		ProjectID: projectID, Phase: string(state.PhaseRunning),
		Ancestry: []string{ownerID},
	}))
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
// precondition 3 (Q3 "Non-attestation"): ProgenyListPredicate and
// EvaluateProgeny call the unchanged relationshipAncestryAttested, which is
// false for a hub_delivery principal, so they match nothing — even though
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
// denies every condition on its own. Every subtest asserts allowed == false,
// err == nil, the exact reason, DenyCause == "", and that no
// delegation_ceiling_allowed step appears in explain — the ordinary
// delegation proof (walkDelegationChain) is never reached for this type, at
// any stage.
func TestHubDelivery_Step10ArmDeniesWithoutStep0b(t *testing.T) {
	f := newGoldenFixture(t)
	ctx := context.Background()

	const validAgentID = "hd-step10-valid-agent"

	baseIdentity := func() *hubDeliveryIdentity {
		return &hubDeliveryIdentity{
			agentID:      validAgentID,
			projectID:    f.projectAlpha.ID,
			ancestry:     []string{f.projectOwnerID},
			originUserID: f.projectOwnerID,
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
			identity:   func() *hubDeliveryIdentity { h := baseIdentity(); h.boundAgentID = "hd-step10-other-agent"; return h }(),
			permission: "secret.deliver", action: ActionDeliver,
			resource:   Resource{Type: "secret", ID: f.secretID},
			wantReason: "delivery credential is bound to a different agent",
		},
		{
			name:       "empty_bound",
			identity:   func() *hubDeliveryIdentity { h := baseIdentity(); h.boundAgentID = ""; return h }(),
			permission: "secret.deliver", action: ActionDeliver,
			resource:   Resource{Type: "secret", ID: f.secretID},
			wantReason: "delivery credential is bound to a different agent",
		},
		{
			name:       "agentID_argument_differs",
			identity:   baseIdentity(),
			agentIDArg: "hd-step10-different-arg",
			permission: "secret.deliver", action: ActionDeliver,
			resource:   Resource{Type: "secret", ID: f.secretID},
			wantReason: "delivery credential is bound to a different agent",
		},
		{
			name:       "action_read",
			identity:   baseIdentity(),
			permission: "secret.deliver", action: ActionRead,
			resource:   Resource{Type: "secret", ID: f.secretID},
			wantReason: "delivery credential is limited to deliver permissions",
		},
		{
			name:       "action_use",
			identity:   baseIdentity(),
			permission: "secret.deliver", action: ActionUse,
			resource:   Resource{Type: "secret", ID: f.secretID},
			wantReason: "delivery credential is limited to deliver permissions",
		},
		{
			name:       "permission_read",
			identity:   baseIdentity(),
			permission: "secret.read", action: ActionDeliver,
			resource:   Resource{Type: "secret", ID: f.secretID},
			wantReason: "delivery credential is limited to deliver permissions",
		},
		{
			// An empty permission is resolved exactly as walkDelegationChain
			// resolves it (resolvePermissionID): no registered permission
			// pairs resource type "agent" with ActionDeliver, so it falls
			// back to a non-deliver synthetic ID, which denies the same way
			// an explicit non-deliver permission does.
			name:       "permission_empty_resolves_non_deliver",
			identity:   baseIdentity(),
			permission: "", action: ActionDeliver,
			resource:   Resource{Type: "agent", ID: validAgentID},
			wantReason: "delivery credential is limited to deliver permissions",
		},
		{
			name:       "typed_nil",
			typedNil:   true,
			permission: "secret.deliver", action: ActionDeliver,
			resource:   Resource{Type: "secret", ID: f.secretID},
			wantReason: "delivery credential is missing",
		},
		{
			name:       "empty_project",
			identity:   func() *hubDeliveryIdentity { h := baseIdentity(); h.projectID = ""; return h }(),
			permission: "secret.deliver", action: ActionDeliver,
			resource:   Resource{Type: "secret", ID: f.secretID},
			wantReason: "delivery credential has no project",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
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
			allowed, reason, err := f.authz.checkDelegationCeiling(ctx, req, agentIDArg, &explain, &cause)
			require.NoError(t, err)
			assert.False(t, allowed)
			assert.Equal(t, tc.wantReason, reason)
			assert.Equal(t, DenyCause(""), cause)
			for _, step := range explain {
				assert.NotEqual(t, "delegation_ceiling_allowed", step.Step, "the ordinary proof must never run for this type")
			}
		})
	}
}
