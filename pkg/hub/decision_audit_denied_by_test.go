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
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// waitForDecisionAudit returns the single persisted decision audit record for
// principalID with the given result. The store emitter writes asynchronously.
func waitForDecisionAudit(t *testing.T, s store.Store, principalID, result string) *store.DecisionAuditRecord {
	t.Helper()
	var rec *store.DecisionAuditRecord
	require.Eventually(t, func() bool {
		records, _, err := s.ListDecisionAudits(context.Background(), store.DecisionAuditFilter{
			PrincipalID: principalID, Result: result, Limit: 10,
		})
		if err != nil || len(records) != 1 {
			return false
		}
		rec = records[0]
		return true
	}, 5*time.Second, 10*time.Millisecond, "expected exactly one persisted %s record for %s", result, principalID)
	return rec
}

// TestDecide_PersistsDeniedBy checks that Decide's single audit exit stores
// Decision.DeniedBy verbatim in the denied_by column: a delegation-ceiling
// deny stores "delegation_ceiling" and an allow stores "".
func TestDecide_PersistsDeniedBy(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	srv.authzService.DecisionAuditSampleRate = 1.0
	srv.authzService.SetDecisionAuditEmitter(NewStoreDecisionAuditEmitter(s, slog.Default()))

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

	t.Run("ceiling deny persists delegation_ceiling", func(t *testing.T) {
		decision := decideRead(orphanAgentID)
		require.False(t, decision.Allowed)
		require.Equal(t, DeniedByDelegationCeiling, decision.DeniedBy)

		rec := waitForDecisionAudit(t, s, orphanAgentID, "deny")
		assert.Equal(t, "delegation_ceiling", rec.DeniedBy)
		assert.Equal(t, decision.Reason, rec.Reason)
	})

	t.Run("allow persists empty denied_by", func(t *testing.T) {
		decision := decideRead(liveAgentID)
		require.True(t, decision.Allowed, decision.Reason)
		require.Empty(t, decision.DeniedBy)

		rec := waitForDecisionAudit(t, s, liveAgentID, "allow")
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
