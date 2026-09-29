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
// G's verified-agent-actor fields land in the same store.DecisionAuditRecord/
// MutationAuditRecord types, in G's own field block. E's writers never
// populate them — this file asserts that behavior, not a type shape that
// would break the moment G's migration lands its fields.
// ---------------------------------------------------------------------------

// gReservedFieldNames are the store-record Go struct field names E's writers
// must never populate. G's migration commit adds more (ParentGrantID,
// ExchangeAgentCredentialID, an actor-kind field, and the aggregated
// record's kind/counts) and updates this list, and
// gReservedFieldNameToLabelKey below, in the same commit.
var gReservedFieldNames = []string{
	"ActorAgentID",
	"AuthorizingUserID",
	"SourceGrantID",
	"DelegationEdgeID",
	"AgentDelegationCode",
}

// gReservedFieldNameToLabelKey maps each actor-identity entry in
// gReservedFieldNames to its corresponding entry in gVerifiedActorFieldNames
// (credential_decoration.go), the single canonical, E.1-owned reserved
// label-key set. AgentDelegationCode is a denial fine-code, not an actor
// identity, and is deliberately absent: it is not a reserved label key.
var gReservedFieldNameToLabelKey = map[string]string{
	"ActorAgentID":      "actor_agent_id",
	"AuthorizingUserID": "authorizing_user_id",
	"SourceGrantID":     "source_grant_id",
	"DelegationEdgeID":  "delegation_edge_id",
}

// TestGReservedFieldNames_MatchCanonicalLabelKeys pins gReservedFieldNames
// against gVerifiedActorFieldNames through gReservedFieldNameToLabelKey, so
// the store-record field list and the label-key reservation list cannot
// silently drift apart. AgentDelegationCode is explicitly excluded: it is a
// denial fine-code, not an actor identity, and must not be a reserved label
// key — TestValidateCredentialMetadata_AcceptsAgentDelegationCodeAsALabelKey,
// below, proves the exclusion holds in the validator itself, not just in
// this list.
func TestGReservedFieldNames_MatchCanonicalLabelKeys(t *testing.T) {
	canonical := make(map[string]bool, len(gVerifiedActorFieldNames))
	for _, k := range gVerifiedActorFieldNames {
		canonical[k] = true
	}
	for _, fieldName := range gReservedFieldNames {
		labelKey, mapped := gReservedFieldNameToLabelKey[fieldName]
		if fieldName == "AgentDelegationCode" {
			require.False(t, mapped, "AgentDelegationCode must stay excluded from gReservedFieldNameToLabelKey")
			continue
		}
		require.True(t, mapped, "gReservedFieldNames entry %q has no corresponding label key in gReservedFieldNameToLabelKey", fieldName)
		require.True(t, canonical[labelKey], "gReservedFieldNames entry %q maps to label key %q, which is not in gVerifiedActorFieldNames (credential_decoration.go)", fieldName, labelKey)
	}
	require.NotContains(t, gVerifiedActorFieldNames, "agent_delegation_code", "agent_delegation_code must not be a reserved label key")
}

// TestValidateCredentialMetadata_AcceptsAgentDelegationCodeAsALabelKey proves
// the exclusion is real at the validator, not just in the list above: a
// token issuer can use "agent_delegation_code" as a label key today.
func TestValidateCredentialMetadata_AcceptsAgentDelegationCodeAsALabelKey(t *testing.T) {
	err := ValidateCredentialMetadata("n", "", map[string]string{"agent_delegation_code": "v"})
	require.NoError(t, err)
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

// gLikeLabelValue is the synthetic value used for every reserved key in
// gLikeDecoration's labels, distinct enough to search for but not resembling
// any real identifier.
const gLikeLabelValue = "label-value-"

// gLikeDecoration builds a credential decoration whose labels use every name
// in gVerifiedActorFieldNames, the canonical reserved label-key set — the
// same keys ValidateCredentialMetadata now rejects at mint (E.1). This
// decoration is constructed directly, outside that validator, to test what
// E's audit writers do if such a decoration reaches them regardless (for
// example, a row present before the reservation existed).
func gLikeDecoration(tokenID, projectID string) *CredentialDecoration {
	labels := make(map[string]string, len(gVerifiedActorFieldNames))
	for i, key := range gVerifiedActorFieldNames {
		labels[key] = gLikeLabelValue + string(rune('a'+i))
	}
	return &CredentialDecoration{
		Kind:      CredentialKindUAT,
		TokenID:   tokenID,
		TokenName: "g-like-token",
		Boundary:  decorationBoundary{Kind: "project", ProjectID: projectID},
		Labels:    labels,
	}
}

// assertLabelsOnlyInCredentialLabels asserts every gLikeLabelValue-prefixed
// value appears only inside the bounded CredentialLabels snapshot, never in
// any identity/credential-ID field.
func assertLabelsOnlyInCredentialLabels(t *testing.T, label, principalID, credentialID, credentialLabels string) {
	t.Helper()
	require.NotContains(t, principalID, gLikeLabelValue, "%s: label value leaked into PrincipalID", label)
	require.NotContains(t, credentialID, gLikeLabelValue, "%s: label value leaked into CredentialID", label)
	require.Contains(t, credentialLabels, gLikeLabelValue, "%s: expected the label values to survive in CredentialLabels", label)
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
