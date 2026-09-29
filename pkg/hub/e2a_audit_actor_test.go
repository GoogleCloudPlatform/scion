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
// request context carry the same principal, credential, and correlation ID:
// both read the correlation ID from the same *logging.RequestMeta a real
// request's RequestLogMiddleware installs (this test installs one directly,
// exercising the identical mechanism a live HTTP request uses end to end).
func TestDecisionAndMutationAudit_AgreeOnCorrelation(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	project := &store.Project{ID: tid("agree-corr-project"), Name: "p", Slug: "agree-corr-project", CreatedBy: DevUserID, OwnerID: DevUserID}
	require.NoError(t, s.CreateProject(ctx, project))

	decisionEmitter := &recordingDecisionAuditEmitter{}
	srv.authzService.SetDecisionAuditEmitter(decisionEmitter)

	meta := &logging.RequestMeta{RequestID: "corr-shared-123"}
	reqCtx := logging.ContextWithRequestMeta(ctx, meta)
	identity := NewAuthenticatedUser(DevUserID, "dev@localhost", "Dev", "admin", "api")
	reqCtx = contextWithIdentity(reqCtx, identity)
	reqCtx = contextWithCredentialContext(reqCtx, credentialContextForIdentity(identity))

	req := AuthzRequestFromContext(reqCtx, Resource{Type: "project", ID: project.ID}, ActionRead)
	decision := srv.authzService.Decide(reqCtx, req)
	require.True(t, decision.Allowed)

	require.NotEmpty(t, decisionEmitter.records)
	decisionRecord := decisionEmitter.records[len(decisionEmitter.records)-1]
	require.Equal(t, "corr-shared-123", decisionRecord.CorrelationID)

	mutationRecord := &store.MutationAuditRecord{MutationType: "test_mutation", TargetType: "project", TargetID: project.ID}
	auditActorFromContext(reqCtx).ApplyActor(mutationRecord)

	require.Equal(t, decisionRecord.PrincipalID, mutationRecord.ActorPrincipalID)
	require.Equal(t, decisionRecord.CredentialID, mutationRecord.ActorCredentialID)
	require.Equal(t, decisionRecord.CorrelationID, mutationRecord.CorrelationID, "decision and mutation audit must share the same correlation ID for one request")
}
