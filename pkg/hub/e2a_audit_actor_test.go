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

// TestApplyActor_PresetPrincipalDoesNotInheritAmbientCredential proves a
// caller that presets the principal to someone other than ctx's own
// principal must not have that record's credential fields filled from ctx's
// credential — that would name principal A (the preset) with principal B's
// (ctx's) credential ID, name, boundary, and labels.
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
// correlation ID. Uses a decorated scoped UAT (a dev session carries no
// decoration), and exercises two real production writers sharing the same
// context — UserAccessTokenService's in-transaction createAuditRecord, and
// the fire-and-forget emitMutationAudit.
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

// TestBoundedLabelsJSON_SanitizesControlCharacterKey proves a labels map that
// never went through ValidateCredentialMetadata (the shape of a row written
// directly to the store, outside the issuance-time validation path) still
// renders its key sanitized. The assertion runs against the *unmarshalled*
// key, not the raw JSON string: json.Marshal always escapes a raw control
// byte to "\u0000", so checking the raw string for "\x00" would pass even if
// sanitizeForLog were never called. Decoding first and checking for the
// actual NUL byte, and for the U+FFFD replacement sanitizeForLog inserts,
// tests the real behavior.
func TestBoundedLabelsJSON_SanitizesControlCharacterKey(t *testing.T) {
	labels := map[string]string{"bad\x00key": "v"}

	out := boundedLabelsJSON(labels)
	var parsed map[string]string
	require.NoError(t, json.Unmarshal([]byte(out), &parsed), "snapshot must always be valid JSON")

	require.Len(t, parsed, 1)
	for k := range parsed {
		require.NotContains(t, k, "\x00", "the rendered key must not contain a raw control character")
		require.Contains(t, k, "�", "the rendered key must contain the sanitizer's replacement character")
	}
}

// TestBoundedLabelsJSON_TruncatesOnByteBoundAfterEscaping proves the byte
// cap is a reachable path, not dead code. Per-field sanitization caps each
// value at uatMaxLabelValueBytes raw bytes, but JSON escapes characters like
// "<" to six bytes ("<"), which can multiply a sanitized value's
// encoded size well past what the raw per-field caps alone would suggest.
// Eight labels of uatMaxLabelValueBytes "<" characters each drive the
// marshalled size over maxAuditLabelsBytes, forcing entries to be dropped.
func TestBoundedLabelsJSON_TruncatesOnByteBoundAfterEscaping(t *testing.T) {
	labels := make(map[string]string, uatMaxLabelCount)
	for i := 0; i < uatMaxLabelCount; i++ {
		labels["k"+string(rune('a'+i))] = strings.Repeat("<", uatMaxLabelValueBytes)
	}

	out := boundedLabelsJSON(labels)
	require.LessOrEqual(t, len(out), maxAuditLabelsBytes, "snapshot must stay within the 1 KiB bound")

	var parsed map[string]string
	require.NoError(t, json.Unmarshal([]byte(out), &parsed))

	marker, ok := parsed[auditLabelsTruncatedMarker]
	require.True(t, ok, "expected the truncation marker once escaping forces entries to be dropped")
	require.Equal(t, "true", marker)
	require.Less(t, len(parsed)-1, uatMaxLabelCount, "expected at least one label to have been dropped")
}

// TestBoundedLabelsJSON_MarksCountCapTruncationToo proves the truncation
// marker is set when entries are dropped by the count cap alone, not only by
// the byte cap: more than uatMaxLabelCount small labels never approach the
// byte bound, so the marker's presence is the only signal that any were
// dropped.
func TestBoundedLabelsJSON_MarksCountCapTruncationToo(t *testing.T) {
	labels := make(map[string]string, uatMaxLabelCount+4)
	for i := 0; i < uatMaxLabelCount+4; i++ {
		labels["k"+string(rune('a'+i))] = "v"
	}

	out := boundedLabelsJSON(labels)
	var parsed map[string]string
	require.NoError(t, json.Unmarshal([]byte(out), &parsed))

	marker, ok := parsed[auditLabelsTruncatedMarker]
	require.True(t, ok, "expected the truncation marker when the count cap drops entries")
	require.Equal(t, "true", marker)
	require.Len(t, parsed, uatMaxLabelCount+1, "expected exactly uatMaxLabelCount surviving labels plus the marker")
}

// TestBoundedLabelsJSON_MarkerCannotBeSpoofedByALegacyKey proves a source
// label literally named "_truncated" cannot be mistaken for the real
// truncation marker: it is renamed on render, regardless of whether any
// actual truncation occurred, so a reader checking for the marker key never
// sees issuer-supplied content there.
func TestBoundedLabelsJSON_MarkerCannotBeSpoofedByALegacyKey(t *testing.T) {
	labels := map[string]string{auditLabelsTruncatedMarker: "true"}

	out := boundedLabelsJSON(labels)
	var parsed map[string]string
	require.NoError(t, json.Unmarshal([]byte(out), &parsed))

	_, hasRealMarkerKey := parsed[auditLabelsTruncatedMarker]
	require.False(t, hasRealMarkerKey, "no truncation occurred, so the marker key must not be present")

	found := false
	for k, v := range parsed {
		if v == "true" {
			found = true
			require.NotEqual(t, auditLabelsTruncatedMarker, k, "the legacy key must have been renamed away from the marker key")
		}
	}
	require.True(t, found, "the legacy label's value must still survive under a renamed key")
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
	var decisionParsed map[string]string
	require.NoError(t, json.Unmarshal([]byte(decisionRecord.CredentialLabels), &decisionParsed))
	for k := range decisionParsed {
		require.NotContains(t, k, "\x00", "decision audit: rendered key must not contain a raw control character")
	}

	mutationRecord := &store.MutationAuditRecord{MutationType: "test_mutation", TargetType: "project", TargetID: project.ID}
	auditActorFromContext(reqCtx).ApplyActor(mutationRecord)
	require.LessOrEqual(t, len(mutationRecord.CredentialLabels), maxAuditLabelsBytes)
	var mutationParsed map[string]string
	require.NoError(t, json.Unmarshal([]byte(mutationRecord.CredentialLabels), &mutationParsed))
	for k := range mutationParsed {
		require.NotContains(t, k, "\x00", "mutation audit: rendered key must not contain a raw control character")
	}
}
