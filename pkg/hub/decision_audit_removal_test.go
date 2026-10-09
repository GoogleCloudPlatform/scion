// Copyright 2026 Google LLC
// SPDX-License-Identifier: Apache-2.0

//go:build !no_sqlite

package hub

import (
	"context"
	"errors"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/entadapter"
	"github.com/stretchr/testify/require"
)

// The extra method is deliberately outside store.Store: a future structural
// persistence assertion must trip this sentinel instead of silently writing.
type removedDecisionPersistenceProbe struct {
	store.Store
	calls atomic.Int64
}

func (s *removedDecisionPersistenceProbe) CreateDecisionAudit(context.Context, *store.DecisionAuditRecord) error {
	s.calls.Add(1)
	return errors.New("decision persistence is retired")
}

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
			probe := &removedDecisionPersistenceProbe{Store: inner}
			srv, _ := testServerWithStore(t, probe)
			project := &store.Project{ID: tid("removed-audit-project"), Name: "decision fixture", Slug: "decision-fixture", CreatedBy: DevUserID, OwnerID: DevUserID}
			require.NoError(t, probe.CreateProject(ctx, project))
			identity := NewAuthenticatedUser(DevUserID, "dev@localhost", "Development User", "admin", "api")
			allowed := srv.authzService.CheckAccess(ctx, identity, Resource{Type: "project", ID: project.ID}, ActionRead)
			require.True(t, allowed.Allowed, allowed.Reason)
			denied := srv.authzService.Decide(ctx, AuthzRequest{})
			require.False(t, denied.Allowed)
			bearer := srv.authzService.EvaluateBearerCeiling(ctx, principalContextForIdentity(identity), projectBoundary(project.ID), bearerCeiling(t, "project:read"), "project.read", Resource{Type: "project", ID: project.ID}, BearerOptions{})
			require.True(t, bearer.Decision.Allowed, bearer.Decision.Reason)
			agentID := tid("removed-audit-delegated-agent")
			createDCAgent(t, probe, agentID, project.ID, DevUserID, AgentRoleFull)
			createDCEdge(t, probe, store.DelegationPrincipalUser, DevUserID, store.DelegationPrincipalAgent, agentID, store.RoleScopeProject, project.ID, string(AgentRoleFull))
			agentCtx := contextWithIdentity(ctx, dcAgentIdentity(agentID, project.ID, AgentRoleFull))
			request := AuthzRequestFromContext(agentCtx, Resource{Type: "project", ID: project.ID}, ActionRead)
			request.Permission = "project.read"
			delegated := srv.authzService.Decide(agentCtx, request)
			require.True(t, delegated.Allowed, delegated.Reason)
			require.IsType(t, noopDecisionAuditEmitter{}, srv.decisionAuditRouter.legacy)
			requireNoDecisionPersistenceTable(t, cs)
			require.NoError(t, srv.Shutdown(ctx))
			srv.authzService.Decide(ctx, AuthzRequest{})
			require.Zero(t, probe.calls.Load())
			require.NoError(t, probe.Migrate(ctx))
			requireNoDecisionPersistenceTable(t, cs)
		})
	}
}

func TestDecisionAuditRemoval_NoWriterInstalled(t *testing.T) {
	check := func() {
		buf := make([]byte, 2<<20)
		n := runtime.Stack(buf, true)
		require.Less(t, n, len(buf), "stack snapshot must be complete")
		stacks := string(buf[:n])
		for _, signature := range []string{"StoreDecisionAuditEmitter", "decisionAuditWriter", "dropLogLoop"} {
			require.False(t, strings.Contains(stacks, signature), "retired writer frame: %s", signature)
		}
	}
	check()
	srv, _ := testServer(t)
	require.IsType(t, noopDecisionAuditEmitter{}, srv.decisionAuditRouter.legacy)
	require.Nil(t, srv.decisionAuditRouter.admission)
	check()
	require.NoError(t, srv.Shutdown(context.Background()))
	check()
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
