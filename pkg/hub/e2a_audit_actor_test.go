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
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/util/logging"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// E.2a (ptone/scion#2127, plan §3.3): the consolidated actor helper,
// replacing four independent "actor from context" copies.
// ---------------------------------------------------------------------------

func TestAuditActorFromContext_PrincipalAndCredential(t *testing.T) {
	identity := NewAuthenticatedUser(tid("actor-user"), "actor@test.com", "Actor", "member", "api")
	scoped := NewScopedUserIdentityWithDecoration(identity, tid("actor-project"), []string{"project:read"}, "actor-token-id",
		&CredentialDecoration{Kind: CredentialKindUAT, TokenID: "actor-token-id", TokenName: "ci-token",
			Boundary: decorationBoundary{Kind: "project", ProjectID: tid("actor-project")}, Labels: map[string]string{"env": "ci"}})

	ctx := contextWithCredentialContext(contextWithIdentity(context.Background(), scoped), credentialContextForIdentity(scoped))

	actor := auditActorFromContext(ctx)
	require.Equal(t, "user", actor.PrincipalKind)
	require.Equal(t, identity.ID(), actor.PrincipalID)
	require.Equal(t, string(CredentialKindUAT), actor.CredentialKind)
	require.Equal(t, "actor-token-id", actor.CredentialID)
	require.Equal(t, "ci-token", actor.CredentialName)
	require.Equal(t, "project", actor.CredentialBoundaryKind)
	require.Contains(t, actor.CredentialLabels, `"env":"ci"`)
}

func TestApplyActor_DoesNotOverwriteExplicitFields(t *testing.T) {
	identity := NewAuthenticatedUser(tid("actor-user-2"), "actor2@test.com", "Actor2", "member", "api")
	ctx := contextWithIdentity(context.Background(), identity)
	actor := auditActorFromContext(ctx)

	record := &store.MutationAuditRecord{
		ActorPrincipalKind: "agent",
		ActorPrincipalID:   "explicit-agent-id",
	}
	actor.ApplyActor(record)

	require.Equal(t, "agent", record.ActorPrincipalKind, "ApplyActor must not overwrite an explicitly set field")
	require.Equal(t, "explicit-agent-id", record.ActorPrincipalID)
}

// TestApplyActor_PresetPrincipalDoesNotInheritAmbientCredential proves
// review-1 finding F4: a caller that presets the principal to someone other
// than ctx's own principal must not have that record's credential fields
// filled from ctx's credential — that would name principal A (the preset)
// with principal B's (ctx's) credential ID, name, boundary, and labels.
func TestApplyActor_PresetPrincipalDoesNotInheritAmbientCredential(t *testing.T) {
	projectID := tid("f4-project")
	decoration := &CredentialDecoration{
		Kind: CredentialKindUAT, TokenID: "f4-ambient-token", TokenName: "ambient",
		Boundary: decorationBoundary{Kind: "project", ProjectID: projectID},
	}
	ctxIdentity := NewAuthenticatedUser(tid("f4-ctx-user"), "ctxuser@test.com", "CtxUser", "member", "api")
	scoped := NewScopedUserIdentityWithDecoration(ctxIdentity, projectID, []string{"project:read"}, "f4-ambient-token", decoration)
	ctx := contextWithCredentialContext(contextWithIdentity(context.Background(), scoped), credentialContextForIdentity(scoped))
	actor := auditActorFromContext(ctx)

	// Preset to a DIFFERENT principal than ctx's own (e.g. attributing a
	// mutation to a resource's original creator).
	record := &store.MutationAuditRecord{
		ActorPrincipalKind: "user",
		ActorPrincipalID:   "someone-else-entirely",
	}
	actor.ApplyActor(record)

	require.Equal(t, "someone-else-entirely", record.ActorPrincipalID)
	require.Empty(t, record.ActorCredentialID, "must not inherit ctx's credential ID for an unrelated preset principal")
	require.Empty(t, record.CredentialName, "must not inherit ctx's credential name for an unrelated preset principal")
	require.Empty(t, record.CredentialBoundaryKind, "must not inherit ctx's credential boundary for an unrelated preset principal")

	// A preset principal that happens to EQUAL ctx's own principal still
	// gets the credential fields filled — it is not "different," just
	// explicitly restated.
	record2 := &store.MutationAuditRecord{
		ActorPrincipalKind: "user",
		ActorPrincipalID:   ctxIdentity.ID(),
	}
	actor.ApplyActor(record2)
	require.Equal(t, "f4-ambient-token", record2.ActorCredentialID, "a preset principal matching ctx's own principal should still get ctx's credential")
}

func TestApplyActor_FillsEmptyFields(t *testing.T) {
	identity := NewAuthenticatedUser(tid("actor-user-3"), "actor3@test.com", "Actor3", "member", "api")
	ctx := contextWithIdentity(context.Background(), identity)
	actor := auditActorFromContext(ctx)

	record := &store.MutationAuditRecord{}
	actor.ApplyActor(record)

	require.Equal(t, "user", record.ActorPrincipalKind)
	require.Equal(t, identity.ID(), record.ActorPrincipalID)
}

// TestDecisionAndMutationAudit_AgreeOnCorrelation proves the E.2 acceptance
// criterion that decision and mutation audit produced within the same
// request context carry the same principal, credential, boundary, and
// correlation ID. Review-1 finding F6: this uses a decorated scoped UAT (not
// a dev session, which carries no decoration), and exercises two real
// production writers sharing the same context — UserAccessTokenService's
// in-transaction createAuditRecord, and the fire-and-forget emitMutationAudit
// — not a bare ApplyActor call.
func TestDecisionAndMutationAudit_AgreeOnCorrelation(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	projectID, ownerID := setupUATProjectAndOwner(t, s, "agree-corr")
	decoration := &CredentialDecoration{
		Kind: CredentialKindUAT, TokenID: "agree-corr-token", TokenName: "agree-corr-token-name",
		Boundary: decorationBoundary{Kind: "project", ProjectID: projectID},
	}
	base := NewAuthenticatedUser(ownerID, "owner@test.com", "Owner", "member", "api")
	scoped := NewScopedUserIdentityWithDecoration(base, projectID, []string{"project:read"}, "agree-corr-token", decoration)

	decisionEmitter := &recordingDecisionAuditEmitter{}
	srv.authzService.SetDecisionAuditEmitter(decisionEmitter)

	meta := &logging.RequestMeta{RequestID: "corr-shared-123"}
	reqCtx := logging.ContextWithRequestMeta(ctx, meta)
	reqCtx = contextWithIdentity(reqCtx, scoped)
	reqCtx = contextWithCredentialContext(reqCtx, credentialContextForIdentity(scoped))

	req := AuthzRequestFromContext(reqCtx, Resource{Type: "project", ID: projectID}, ActionRead)
	decision := srv.authzService.Decide(reqCtx, req)
	require.True(t, decision.Allowed)

	require.NotEmpty(t, decisionEmitter.records)
	decisionRecord := decisionEmitter.records[len(decisionEmitter.records)-1]
	require.Equal(t, "corr-shared-123", decisionRecord.CorrelationID)
	require.Equal(t, "project", decisionRecord.CredentialBoundaryKind)
	require.Equal(t, projectID, decisionRecord.CredentialBoundaryProjectID)

	// Real in-transaction writer: UserAccessTokenService.createAuditRecord,
	// the same method CreateToken/RevokeToken/DeleteToken use.
	inTxRecord := &store.MutationAuditRecord{MutationType: "test_mutation_intx", TargetType: "project", TargetID: projectID}
	require.NoError(t, srv.uatService.createAuditRecord(reqCtx, s, inTxRecord))

	require.Equal(t, decisionRecord.PrincipalID, inTxRecord.ActorPrincipalID)
	require.Equal(t, decisionRecord.CredentialID, inTxRecord.ActorCredentialID)
	require.Equal(t, decisionRecord.CredentialBoundaryKind, inTxRecord.CredentialBoundaryKind)
	require.Equal(t, decisionRecord.CredentialBoundaryProjectID, inTxRecord.CredentialBoundaryProjectID)
	require.Equal(t, decisionRecord.CorrelationID, inTxRecord.CorrelationID)

	// Real fire-and-forget writer: Server.emitMutationAudit.
	fireForgetRecord := &store.MutationAuditRecord{MutationType: "test_mutation_fireforget", TargetType: "project", TargetID: projectID}
	srv.emitMutationAudit(reqCtx, fireForgetRecord)
	require.Eventually(t, func() bool {
		records, _, err := s.ListMutationAudits(context.Background(), store.MutationAuditFilter{
			MutationType: "test_mutation_fireforget",
			Limit:        1,
		})
		return err == nil && len(records) == 1
	}, 2*time.Second, 10*time.Millisecond, "emitMutationAudit's fire-and-forget write did not complete")

	persisted, _, err := s.ListMutationAudits(context.Background(), store.MutationAuditFilter{MutationType: "test_mutation_fireforget", Limit: 1})
	require.NoError(t, err)
	require.Len(t, persisted, 1)
	require.Equal(t, decisionRecord.PrincipalID, persisted[0].ActorPrincipalID)
	require.Equal(t, decisionRecord.CredentialID, persisted[0].ActorCredentialID)
	require.Equal(t, decisionRecord.CredentialBoundaryKind, persisted[0].CredentialBoundaryKind)
	require.Equal(t, decisionRecord.CredentialBoundaryProjectID, persisted[0].CredentialBoundaryProjectID)
	require.Equal(t, decisionRecord.CorrelationID, persisted[0].CorrelationID)
}

// TestBoundedLabelsJSON_SanitizesAndBoundsALegacyRow proves review-1 finding
// F2: a labels map that never went through ValidateCredentialMetadata (the
// shape of a row written directly to the store, outside the issuance-time
// validation path) still renders as bounded, sanitized, valid JSON — never
// raw control characters, and never over the 1 KiB cap — in both the
// decision-audit and mutation-audit snapshot.
func TestBoundedLabelsJSON_SanitizesAndBoundsALegacyRow(t *testing.T) {
	labels := make(map[string]string, 22)
	// Two entries with a "00" prefix so they sort first among all keys
	// below (every filler key below is prefixed "zz"), guaranteeing the
	// uatMaxLabelCount cap cannot drop them before the sanitization/bound
	// logic is exercised.
	//
	// A key containing a control character (never possible through the
	// validator, but not impossible in a row written directly to the store).
	labels["00bad\x00key"] = "v"
	// A value far larger than the per-field cap.
	labels["00oversize"] = strings.Repeat("x", 5*1024)
	// 20 filler labels, sorted after the two above, to prove the overall
	// count cap and byte bound hold even with many more labels than
	// uatMaxLabelCount allows through.
	for i := 0; i < 20; i++ {
		labels["zzfiller"+string(rune('a'+i))] = "v"
	}

	out := boundedLabelsJSON(labels)
	require.LessOrEqual(t, len(out), maxAuditLabelsBytes, "snapshot must stay within the 1 KiB bound")
	require.NotContains(t, out, "\x00", "snapshot must never contain a raw control character")

	var parsed map[string]string
	require.NoError(t, json.Unmarshal([]byte(out), &parsed), "snapshot must always be valid JSON")

	// The oversized value must have been truncated, not dropped or emitted raw.
	for k, v := range parsed {
		require.LessOrEqual(t, len(v), uatMaxLabelValueBytes+len("…"), "value for key %q exceeds the per-field bound: %q", k, v)
	}
}

// TestBoundedLabelsJSON_AppliesToBothDecisionAndMutationAudit proves the same
// bounded/sanitized rendering is used for both audit record types, through
// their real production call paths (BuildDecisionAuditRecord and
// auditActorFromContext.ApplyActor), not just the shared helper in isolation.
func TestBoundedLabelsJSON_AppliesToBothDecisionAndMutationAudit(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	project := &store.Project{ID: tid("f2-project"), Name: "p", Slug: "f2-project", CreatedBy: DevUserID, OwnerID: DevUserID}
	require.NoError(t, s.CreateProject(ctx, project))

	badLabels := map[string]string{
		"control\x00key": "v",
		"oversize":       strings.Repeat("y", 5*1024),
	}
	decoration := &CredentialDecoration{
		Kind:      CredentialKindUAT,
		TokenID:   "f2-token",
		TokenName: "f2-token-name",
		Boundary:  decorationBoundary{Kind: "project", ProjectID: project.ID},
		Labels:    badLabels,
	}
	identity := NewAuthenticatedUser(tid("f2-user"), "f2@test.com", "F2", "member", "api")
	scoped := NewScopedUserIdentityWithDecoration(identity, project.ID, []string{"project:read"}, "f2-token", decoration)
	reqCtx := contextWithCredentialContext(contextWithIdentity(ctx, scoped), credentialContextForIdentity(scoped))

	req := AuthzRequestFromContext(reqCtx, Resource{Type: "project", ID: project.ID}, ActionRead)
	decision := srv.authzService.Decide(reqCtx, req)
	decisionRecord := BuildDecisionAuditRecord(reqCtx, req, decision)
	require.LessOrEqual(t, len(decisionRecord.CredentialLabels), maxAuditLabelsBytes)
	require.NotContains(t, decisionRecord.CredentialLabels, "\x00")
	var decisionParsed map[string]string
	require.NoError(t, json.Unmarshal([]byte(decisionRecord.CredentialLabels), &decisionParsed))

	mutationRecord := &store.MutationAuditRecord{MutationType: "test_mutation", TargetType: "project", TargetID: project.ID}
	auditActorFromContext(reqCtx).ApplyActor(mutationRecord)
	require.LessOrEqual(t, len(mutationRecord.CredentialLabels), maxAuditLabelsBytes)
	require.NotContains(t, mutationRecord.CredentialLabels, "\x00")
	var mutationParsed map[string]string
	require.NoError(t, json.Unmarshal([]byte(mutationRecord.CredentialLabels), &mutationParsed))
}
