// Copyright 2026 Google LLC
// SPDX-License-Identifier: Apache-2.0

//go:build !no_sqlite

package hub

import (
	"context"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/entadapter"
	"github.com/stretchr/testify/require"
)

func requireNoDecisionPersistenceTable(t *testing.T, cs *entadapter.CompositeStore) {
	t.Helper()
	var count int
	require.NoError(t, cs.DB().QueryRow("SELECT count(*) FROM sqlite_master WHERE name='decision_audits'").Scan(&count))
	require.Zero(t, count)
}

func TestDecisionAuditRemoval_NoPersistence(t *testing.T) {
	for _, upgrade := range []bool{false, true} {
		name := "fresh"
		if upgrade {
			name = "upgrade"
		}
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			inner, err := newTestStore(t, ":memory:")
			require.NoError(t, err)
			cs := inner.(*entadapter.CompositeStore)
			if upgrade {
				_, err := cs.DB().Exec("CREATE TABLE decision_audits (id TEXT PRIMARY KEY, reason TEXT); INSERT INTO decision_audits VALUES ('legacy','deny')")
				require.NoError(t, err)
			}
			srv, _ := testServerWithStore(t, cs)
			project := &store.Project{ID: tid("removed-audit-project"), Name: "decision fixture", Slug: "decision-fixture", CreatedBy: DevUserID, OwnerID: DevUserID}
			require.NoError(t, cs.CreateProject(ctx, project))
			identity := NewAuthenticatedUser(DevUserID, "dev@localhost", "Development User", "admin", "api")
			allowed := srv.authzService.CheckAccess(ctx, identity, Resource{Type: "project", ID: project.ID}, ActionRead)
			require.True(t, allowed.Allowed, allowed.Reason)
			denied := srv.authzService.Decide(ctx, AuthzRequest{})
			require.False(t, denied.Allowed)
			bearer := srv.authzService.EvaluateBearerCeiling(ctx, principalContextForIdentity(identity), projectBoundary(project.ID), bearerCeiling(t, "project:read"), "project.read", Resource{Type: "project", ID: project.ID}, BearerOptions{})
			require.True(t, bearer.Decision.Allowed, bearer.Decision.Reason)
			agentID := tid("removed-audit-delegated-agent")
			createDCAgent(t, cs, agentID, project.ID, DevUserID, AgentRoleFull)
			createDCEdge(t, cs, store.DelegationPrincipalUser, DevUserID, store.DelegationPrincipalAgent, agentID, store.RoleScopeProject, project.ID, string(AgentRoleFull))
			agentCtx := contextWithIdentity(ctx, dcAgentIdentity(agentID, project.ID, AgentRoleFull))
			request := AuthzRequestFromContext(agentCtx, Resource{Type: "project", ID: project.ID}, ActionRead)
			request.Permission = "project.read"
			delegated := srv.authzService.Decide(agentCtx, request)
			require.True(t, delegated.Allowed, delegated.Reason)
			require.IsType(t, inertDecisionAuditTarget, srv.decisionAuditRouter.legacy)
			require.True(t, sameDecisionAuditReference(inertDecisionAuditTarget, srv.decisionAuditRouter.legacy))
			requireNoDecisionPersistenceTable(t, cs)
			require.NoError(t, srv.Shutdown(ctx))
			srv.authzService.Decide(ctx, AuthzRequest{})
			require.NoError(t, cs.Migrate(ctx))
			requireNoDecisionPersistenceTable(t, cs)
		})
	}
}

func TestDecisionAuditRemoval_InertTargetIdentity(t *testing.T) {
	srv, s := testServer(t)
	require.IsType(t, inertDecisionAuditTarget, srv.decisionAuditRouter.legacy)
	require.True(t, sameDecisionAuditReference(inertDecisionAuditTarget, srv.decisionAuditRouter.legacy))
	require.False(t, sameDecisionAuditReference(&noopDecisionAuditEmitter{}, srv.decisionAuditRouter.legacy))
	require.Nil(t, srv.decisionAuditRouter.admission)
	require.Equal(t, "healthy", srv.decisionAuditRouter.healthProjection())
	requireNoDecisionPersistenceTable(t, s.(*entadapter.CompositeStore))
	require.NoError(t, srv.CleanupResources(context.Background()))
	require.NoError(t, srv.Shutdown(context.Background()))
	requireNoDecisionPersistenceTable(t, s.(*entadapter.CompositeStore))
}

func TestDecisionAuditRemoval_HealthUnavailable(t *testing.T) {
	srv, _ := testServer(t)
	checks := map[string]string{"database": "healthy"}
	srv.checkDecisionAuditHealth(checks)
	require.NotContains(t, checks, decisionAuditLegacyHealthKey)
	require.Equal(t, "healthy", checks[decisionAuditNewHealthKey])
	require.Equal(t, HealthStatusHealthy, deriveHealthStatus(checks))
	checks["database"] = "unhealthy"
	require.Equal(t, HealthStatusUnhealthy, deriveHealthStatus(checks))
}

func TestDecisionAuditRemoval_MutationHistoryUnchanged(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	record := &store.MutationAuditRecord{MutationType: "keep_mutation", ActorPrincipalKind: "user", ActorPrincipalID: DevUserID, TargetType: "project", TargetID: tid("kept-project"), CorrelationID: "keep-correlation"}
	var historyID string
	require.NoError(t, s.WithTx(ctx, func(tx store.Store) error {
		if err := tx.CreateMutationAudit(ctx, record); err != nil {
			return err
		}
		constraint, err := tx.CreateAccessConstraint(ctx, &store.AccessConstraint{Name: "history fixture", SubjectKind: store.ConstraintSubjectAllPrincipals, ScopeType: "system", MaximumPermissions: []string{"project.read"}, Purpose: "fixture"})
		if err != nil {
			return err
		}
		historyID = constraint.ID
		return tx.AppendConstraintHistoryTx(ctx, &store.AccessConstraintHistory{EventID: "keep-history", ConstraintID: constraint.ID, OccurredAt: record.Timestamp, Operation: "create", ActorKind: "user", ActorID: DevUserID})
	}))
	before, _, err := s.ListMutationAudits(ctx, store.MutationAuditFilter{CorrelationID: "keep-correlation", Limit: 10})
	require.NoError(t, err)
	history, err := s.ListConstraintHistory(ctx, historyID)
	require.NoError(t, err)
	require.Len(t, history, 1)
	srv.authzService.Decide(ctx, AuthzRequest{})
	require.NoError(t, srv.Shutdown(ctx))
	after, _, err := s.ListMutationAudits(ctx, store.MutationAuditFilter{CorrelationID: "keep-correlation", Limit: 10})
	require.NoError(t, err)
	require.Equal(t, before, after)
	afterHistory, err := s.ListConstraintHistory(ctx, historyID)
	require.NoError(t, err)
	require.Equal(t, history, afterHistory)
	requireNoDecisionPersistenceTable(t, s.(*entadapter.CompositeStore))
}
