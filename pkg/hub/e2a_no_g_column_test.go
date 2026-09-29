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
	"reflect"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// E.2a (ptone/scion#2127, plan §3.7): G's fields are written only by G's own
// code paths; E's writers leave them zero. G adds its own columns in its own
// migration, in the same store.DecisionAuditRecord/MutationAuditRecord types
// (ruling, G integration 22:06: the wrapper emits one record, "G fields in
// their own block") — so this test asserts a behavior that survives that
// addition, not a type shape that a struct-literal reflection check would
// break the moment G lands its fields.
// ---------------------------------------------------------------------------

// gReservedFieldNames are the field names plan §3.7 and the rulings reserve
// for G's verified-agent actor extension. None of E's writers may populate
// them, whether or not the field exists yet on the record types. G's
// migration commit adds more (ParentGrantID, ExchangeAgentCredentialID, an
// actor kind, and the aggregated record's kind/counts) and keeps this list
// in sync with the final names it lands.
var gReservedFieldNames = []string{
	"ActorAgentID",
	"AuthorizingUserID",
	"SourceGrantID",
	"DelegationEdgeID",
	"AgentDelegationCode",
}

// assertReservedFieldsZero uses reflection to assert that every field of v
// whose name is in gReservedFieldNames is at its zero value. If the field
// does not exist (true today, before G lands), it is skipped — the point is
// that E's writers never populate it once it exists, not that it must not
// exist.
func assertReservedFieldsZero(t *testing.T, label string, v any) {
	t.Helper()
	rv := reflect.ValueOf(v)
	rt := rv.Type()
	for i := 0; i < rt.NumField(); i++ {
		field := rt.Field(i)
		reserved := false
		for _, name := range gReservedFieldNames {
			if field.Name == name {
				reserved = true
				break
			}
		}
		if !reserved {
			continue
		}
		fv := rv.Field(i)
		if !fv.IsZero() {
			t.Errorf("%s: E wrote a value into reserved G field %s.%s: %v", label, rt.Name(), field.Name, fv.Interface())
		}
	}
}

// gLikeDecoration builds a credential decoration whose labels use G-reserved
// key names (as an adversarial/careless issuer might try, or as a row
// written directly to the store rather than through the validator would
// allow — ValidateCredentialMetadata itself rejects these keys at mint,
// which is E.1's job and is covered there; this test is about what E's
// audit writers do with such a decoration if one reaches them).
func gLikeDecoration(tokenID, projectID string) *CredentialDecoration {
	return &CredentialDecoration{
		Kind:      CredentialKindUAT,
		TokenID:   tokenID,
		TokenName: "g-like-token",
		Boundary:  decorationBoundary{Kind: "project", ProjectID: projectID},
		Labels: map[string]string{
			"actor_agent_id":      "evil-agent-id",
			"authorizing_user_id": "evil-user-id",
		},
	}
}

// assertLabelsOnlyInCredentialLabels asserts the adversarial label values
// appear only inside the bounded CredentialLabels snapshot, never in any
// identity/credential-ID field.
func assertLabelsOnlyInCredentialLabels(t *testing.T, label, principalID, credentialID, credentialLabels string) {
	t.Helper()
	require.NotContains(t, principalID, "evil-agent-id", "%s: label value leaked into PrincipalID", label)
	require.NotContains(t, principalID, "evil-user-id", "%s: label value leaked into PrincipalID", label)
	require.NotContains(t, credentialID, "evil-agent-id", "%s: label value leaked into CredentialID", label)
	require.NotContains(t, credentialID, "evil-user-id", "%s: label value leaked into CredentialID", label)
	require.Contains(t, credentialLabels, "evil-agent-id", "%s: expected the label value to survive in CredentialLabels", label)
}

// TestNoEPathWritesGColumns_DecisionAudit runs Decide on allow, deny, and
// UAT-gate-denied paths with a G-like decoration on context, and asserts
// none of E's reserved-field/leakage rules is violated in the resulting
// decision audit records.
func TestNoEPathWritesGColumns_DecisionAudit(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	projectID, ownerID := setupUATProjectAndOwner(t, s, "nog-decision")
	otherProjectID := tid("nog-decision-other-project")
	require.NoError(t, s.CreateProject(ctx, &store.Project{
		ID: otherProjectID, Name: "other", Slug: "nog-decision-other", CreatedBy: ownerID, OwnerID: ownerID,
	}))

	decoration := gLikeDecoration("nog-decision-token", projectID)
	base := NewAuthenticatedUser(ownerID, "owner@test.com", "Owner", "member", "api")
	scoped := NewScopedUserIdentityWithDecoration(base, projectID, []string{"project:read"}, "nog-decision-token", decoration)
	reqCtx := contextWithCredentialContext(contextWithIdentity(ctx, scoped), credentialContextForIdentity(scoped))

	cases := []struct {
		name     string
		resource Resource
		action   Action
	}{
		{"allow", Resource{Type: "project", ID: projectID}, ActionRead},
		{"uat-gate-deny", Resource{Type: "project", ID: otherProjectID}, ActionRead},
		{"scope-deny", Resource{Type: "project", ID: projectID}, ActionDelete},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := AuthzRequestFromContext(reqCtx, tc.resource, tc.action)
			decision := srv.authzService.Decide(reqCtx, req)
			record := BuildDecisionAuditRecord(reqCtx, req, decision)

			assertReservedFieldsZero(t, "decision audit ("+tc.name+")", *record)
			assertLabelsOnlyInCredentialLabels(t, "decision audit ("+tc.name+")", record.PrincipalID, record.CredentialID, record.CredentialLabels)
		})
	}
}

// TestNoEPathWritesGColumns_MutationAudit runs both a fire-and-forget-style
// ApplyActor call and one real in-transaction writer
// (UserAccessTokenService.createAuditRecord) with the same G-like decoration
// on context, and asserts the same rules hold for mutation audit.
func TestNoEPathWritesGColumns_MutationAudit(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	projectID, ownerID := setupUATProjectAndOwner(t, s, "nog-mutation")
	decoration := gLikeDecoration("nog-mutation-token", projectID)
	base := NewAuthenticatedUser(ownerID, "owner@test.com", "Owner", "member", "api")
	scoped := NewScopedUserIdentityWithDecoration(base, projectID, []string{"project:read"}, "nog-mutation-token", decoration)
	reqCtx := contextWithCredentialContext(contextWithIdentity(ctx, scoped), credentialContextForIdentity(scoped))

	// ApplyActor, as every fire-and-forget writer uses it.
	applied := &store.MutationAuditRecord{MutationType: "test_mutation", TargetType: "project", TargetID: projectID}
	auditActorFromContext(reqCtx).ApplyActor(applied)
	assertReservedFieldsZero(t, "mutation audit (ApplyActor)", *applied)
	assertLabelsOnlyInCredentialLabels(t, "mutation audit (ApplyActor)", applied.ActorPrincipalID, applied.ActorCredentialID, applied.CredentialLabels)

	// One real in-transaction writer: UserAccessTokenService.createAuditRecord,
	// the same method CreateToken/RevokeToken/DeleteToken use.
	inTx := &store.MutationAuditRecord{MutationType: "test_mutation_intx", TargetType: "project", TargetID: projectID}
	require.NoError(t, srv.uatService.createAuditRecord(reqCtx, s, inTx))
	assertReservedFieldsZero(t, "mutation audit (in-transaction writer)", *inTx)
	assertLabelsOnlyInCredentialLabels(t, "mutation audit (in-transaction writer)", inTx.ActorPrincipalID, inTx.ActorCredentialID, inTx.CredentialLabels)
}
