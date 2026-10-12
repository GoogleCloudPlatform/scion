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
	"log/slog"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/hub/auditevent"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/util/logging"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestDecide_RecordsDeniedBy checks that Decide's single audit exit records
// Decision.DeniedBy verbatim in the in-memory record: a delegation-ceiling
// deny records "delegation_ceiling" and an allow records "".
func TestDecide_RecordsDeniedBy(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	srv.authzService.DecisionAuditSampleRate = 1.0
	auditEmitter := &recordingDecisionAuditEmitter{}
	srv.authzService.SetDecisionAuditEmitter(auditEmitter)

	projectID := tid("deniedby-proj")
	ownerID := tid("deniedby-owner")
	liveAgentID := tid("deniedby-live-agent")
	orphanAgentID := tid("deniedby-orphan-agent")
	goneUserID := tid("deniedby-gone-user")

	createDCProject(t, s, projectID, "deniedby-project")
	createDCUser(t, s, ownerID, "deniedby-owner@example.com", projectID, store.ProjectRoleOwner)

	// Live chain: the owner delegates to the agent.
	createDCAgent(t, s, liveAgentID, projectID, ownerID, AgentRoleFull)
	createDCEdge(t, s, store.DelegationPrincipalUser, ownerID,
		store.DelegationPrincipalAgent, liveAgentID,
		store.RoleScopeProject, projectID, string(AgentRoleFull))

	// Non-live chain: the delegator user does not exist.
	createDCAgent(t, s, orphanAgentID, projectID, goneUserID, AgentRoleFull)
	createDCEdge(t, s, store.DelegationPrincipalUser, goneUserID,
		store.DelegationPrincipalAgent, orphanAgentID,
		store.RoleScopeProject, projectID, string(AgentRoleFull))

	decideRead := func(agentID string) Decision {
		agentCtx := contextWithIdentity(ctx, dcAgentIdentity(agentID, projectID, AgentRoleFull))
		req := AuthzRequestFromContext(agentCtx, Resource{Type: "project", ID: projectID}, ActionRead)
		req.Permission = "project.read"
		return srv.authzService.Decide(agentCtx, req)
	}

	t.Run("ceiling deny records delegation_ceiling", func(t *testing.T) {
		decision := decideRead(orphanAgentID)
		require.False(t, decision.Allowed)
		require.Equal(t, DeniedByDelegationCeiling, decision.DeniedBy)

		require.Len(t, auditEmitter.records, 1)
		rec := auditEmitter.records[0]
		assert.Equal(t, "delegation_ceiling", rec.DeniedBy)
		assert.Equal(t, decision.Reason, rec.Reason)
	})

	t.Run("allow records empty denied_by", func(t *testing.T) {
		decision := decideRead(liveAgentID)
		require.True(t, decision.Allowed, decision.Reason)
		require.Empty(t, decision.DeniedBy)

		require.Len(t, auditEmitter.records, 2)
		rec := auditEmitter.records[1]
		assert.Empty(t, rec.DeniedBy)
	})
}

// TestBuildDecisionAuditRecord_DeniedBy checks the field mapping directly.
func TestBuildDecisionAuditRecord_DeniedBy(t *testing.T) {
	ctx := context.Background()
	req := AuthzRequest{Resource: Resource{Type: "project", ID: "p1"}, Action: ActionRead}

	denied := BuildDecisionAuditRecord(ctx, req, Decision{Allowed: false, DeniedBy: DeniedByDelegationCeiling})
	assert.Equal(t, "delegation_ceiling", denied.DeniedBy)

	unattributed := BuildDecisionAuditRecord(ctx, req, Decision{Allowed: false})
	assert.Empty(t, unattributed.DeniedBy)

	allowed := BuildDecisionAuditRecord(ctx, req, Decision{Allowed: true})
	assert.Empty(t, allowed.DeniedBy)
}

// TestBuildDecisionAuditRecord_ResourceScopeEvidence checks that the
// evaluated resource's scope evidence is copied verbatim (design C1.4) and
// decides the decision-log scope rule.
func TestBuildDecisionAuditRecord_ResourceScopeEvidence(t *testing.T) {
	ctx := context.Background()
	plain := BuildDecisionAuditRecord(ctx, AuthzRequest{Resource: Resource{Type: "project", ID: "p1", OwnerID: "u1", Labels: map[string]string{"a": "b"}}, Action: ActionRead}, Decision{Allowed: true})
	assert.Empty(t, plain.ResourceParentType)
	assert.Empty(t, plain.ResourceParentID)
	assert.Zero(t, plain.ResourceAncestryLen)
	assert.Empty(t, plain.ResourceScopeKind)
	assert.False(t, plain.ResourceScopeUserIDSet)

	contained := BuildDecisionAuditRecord(ctx, AuthzRequest{Resource: Resource{
		Type: "agent", ID: "a1", ParentType: "project", ParentID: "p1", Ancestry: []string{"root", "p1"},
		ScopeKind: "user", ScopeUserID: "u1",
	}, Action: ActionRead}, Decision{Allowed: true})
	assert.Equal(t, "project", contained.ResourceParentType)
	assert.Equal(t, "p1", contained.ResourceParentID)
	assert.Equal(t, 2, contained.ResourceAncestryLen)
	assert.Equal(t, "user", contained.ResourceScopeKind)
	assert.True(t, contained.ResourceScopeUserIDSet)
}

// TestDecide_CeilingDenyEmitsOneAuditRecord pins, with a synchronous
// emitter, that one Decide call for a delegation-ceiling deny emits exactly
// one audit record and that the record carries denied_by
// "delegation_ceiling". It covers the deny for a non-live delegator and the
// deny on a ceiling store error (DenyCauseCeilingError).
func TestDecide_CeilingDenyEmitsOneAuditRecord(t *testing.T) {
	_, s := testServer(t)
	ctx := context.Background()

	projectID := tid("deniedby-once-proj")
	ownerID := tid("deniedby-once-owner")
	orphanAgentID := tid("deniedby-once-orphan")
	errAgentID := tid("deniedby-once-err")

	createDCProject(t, s, projectID, "deniedby-once-project")
	createDCUser(t, s, ownerID, "deniedby-once-owner@example.com", projectID, store.ProjectRoleOwner)

	// Non-live chain: the delegator user does not exist.
	createDCAgent(t, s, orphanAgentID, projectID, ownerID, AgentRoleFull)
	createDCEdge(t, s, store.DelegationPrincipalUser, tid("deniedby-once-gone-user"),
		store.DelegationPrincipalAgent, orphanAgentID,
		store.RoleScopeProject, projectID, string(AgentRoleFull))

	// Live chain whose edge lookup fails in the faulty service below.
	createDCAgent(t, s, errAgentID, projectID, ownerID, AgentRoleFull)
	createDCEdge(t, s, store.DelegationPrincipalUser, ownerID,
		store.DelegationPrincipalAgent, errAgentID,
		store.RoleScopeProject, projectID, string(AgentRoleFull))

	for _, tc := range []struct {
		name    string
		agentID string
		authz   *AuthzService
		cause   DenyCause
	}{
		{"non-live delegator", orphanAgentID, NewAuthzService(s, slog.Default()), DenyCauseCeilingOrphaned},
		{"edge lookup error", errAgentID, NewAuthzService(&edgeLookupErrStore{Store: s, failID: errAgentID}, slog.Default()), DenyCauseCeilingError},
	} {
		t.Run(tc.name, func(t *testing.T) {
			emitter := &recordingDecisionAuditEmitter{}
			tc.authz.DecisionAuditSampleRate = 1.0
			tc.authz.SetDecisionAuditEmitter(emitter)

			agentCtx := contextWithIdentity(ctx, dcAgentIdentity(tc.agentID, projectID, AgentRoleFull))
			req := AuthzRequestFromContext(agentCtx, Resource{Type: "project", ID: projectID}, ActionRead)
			req.Permission = "project.read"
			decision := tc.authz.Decide(agentCtx, req)
			require.False(t, decision.Allowed)
			require.Equal(t, DeniedByDelegationCeiling, decision.DeniedBy, "reason %q", decision.Reason)
			require.Equal(t, tc.cause, decision.DenyCause, "reason %q", decision.Reason)

			records := emitter.records
			require.Len(t, records, 1, "one Decide call emits one audit record")
			assert.Equal(t, "delegation_ceiling", records[0].DeniedBy)

			assert.Equal(t, decision.Reason, records[0].Reason)

			// The decision log carries the attribution into the
			// authorization/decide envelope (remaining-audit P1).
			logCtx := logging.ContextWithRequestMeta(agentCtx, &logging.RequestMeta{RequestID: "deniedby-" + tc.agentID})
			env, d := mapDecisionEnvelope(logCtx, records[0])
			require.Equal(t, decisionAuditEnqueued, d, "agent principal on a system-scoped project is in domain")
			payload := env.Payload.(auditevent.AuthorizationDecisionPayload)
			assert.Equal(t, "delegation_ceiling", payload.DeniedBy)
			assert.Equal(t, auditevent.OutcomeDeny, env.Outcome)
		})
	}
}
