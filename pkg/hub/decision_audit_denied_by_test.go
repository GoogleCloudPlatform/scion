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
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// waitForDecisionAudit returns the single persisted decision audit record for
// principalID with the given result. The store emitter writes asynchronously,
// so after the first record appears the helper keeps watching and fails if a
// second one is written.
func waitForDecisionAudit(t *testing.T, s store.Store, principalID, result string) *store.DecisionAuditRecord {
	t.Helper()
	// count returns -1 on a list error so that neither wait accepts it.
	var mu sync.Mutex
	var last []*store.DecisionAuditRecord
	count := func() int {
		records, _, err := s.ListDecisionAudits(context.Background(), store.DecisionAuditFilter{
			PrincipalID: principalID, Result: result, Limit: 10,
		})
		if err != nil {
			return -1
		}
		mu.Lock()
		last = records
		mu.Unlock()
		return len(records)
	}
	require.Eventually(t, func() bool { return count() >= 1 }, 5*time.Second, 10*time.Millisecond,
		"expected a persisted %s record for %s", result, principalID)
	require.Never(t, func() bool { return count() != 1 }, 300*time.Millisecond, 20*time.Millisecond,
		"expected exactly one persisted %s record for %s", result, principalID)
	mu.Lock()
	defer mu.Unlock()
	require.Len(t, last, 1)
	return last[0]
}

// syncStoreDecisionAuditEmitter writes each record to the store before
// returning and counts the records it was given.
type syncStoreDecisionAuditEmitter struct {
	store   store.Store
	mu      sync.Mutex
	records []*store.DecisionAuditRecord
	errs    []error
}

func (e *syncStoreDecisionAuditEmitter) EmitDecisionAudit(ctx context.Context, record *store.DecisionAuditRecord) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.records = append(e.records, record)
	if err := e.store.CreateDecisionAudit(ctx, record); err != nil {
		e.errs = append(e.errs, err)
	}
}

func (e *syncStoreDecisionAuditEmitter) snapshot() ([]*store.DecisionAuditRecord, []error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]*store.DecisionAuditRecord(nil), e.records...), append([]error(nil), e.errs...)
}

// TestDecide_PersistsDeniedBy checks that Decide's single audit exit stores
// Decision.DeniedBy verbatim in the denied_by column: a delegation-ceiling
// deny stores "delegation_ceiling" and an allow stores "".
func TestDecide_PersistsDeniedBy(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	srv.authzService.DecisionAuditSampleRate = 1.0
	auditEmitter := NewStoreDecisionAuditEmitter(s, slog.Default())
	t.Cleanup(func() { auditEmitter.Close(context.Background()) })
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

// TestDecide_CeilingDenyEmitsOneAuditRecord pins, with a synchronous
// emitter, that one Decide call for a delegation-ceiling deny emits exactly
// one audit record and that the record persists denied_by
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
			emitter := &syncStoreDecisionAuditEmitter{store: s}
			tc.authz.DecisionAuditSampleRate = 1.0
			tc.authz.SetDecisionAuditEmitter(emitter)

			agentCtx := contextWithIdentity(ctx, dcAgentIdentity(tc.agentID, projectID, AgentRoleFull))
			req := AuthzRequestFromContext(agentCtx, Resource{Type: "project", ID: projectID}, ActionRead)
			req.Permission = "project.read"
			decision := tc.authz.Decide(agentCtx, req)
			require.False(t, decision.Allowed)
			require.Equal(t, DeniedByDelegationCeiling, decision.DeniedBy, "reason %q", decision.Reason)
			require.Equal(t, tc.cause, decision.DenyCause, "reason %q", decision.Reason)

			records, errs := emitter.snapshot()
			require.Empty(t, errs)
			require.Len(t, records, 1, "one Decide call emits one audit record")
			assert.Equal(t, "delegation_ceiling", records[0].DeniedBy)

			persisted, _, err := s.ListDecisionAudits(ctx, store.DecisionAuditFilter{
				PrincipalID: tc.agentID, Result: "deny", Limit: 10,
			})
			require.NoError(t, err)
			require.Len(t, persisted, 1)
			assert.Equal(t, "delegation_ceiling", persisted[0].DeniedBy)
			assert.Equal(t, decision.Reason, persisted[0].Reason)
		})
	}
}
