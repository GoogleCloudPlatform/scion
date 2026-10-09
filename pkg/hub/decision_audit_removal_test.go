// Copyright 2026 Google LLC
// SPDX-License-Identifier: Apache-2.0

//go:build !no_sqlite

package hub

import (
	"context"
	"database/sql"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/entadapter"
	"github.com/stretchr/testify/require"
)

func requireNoDecisionPersistenceTable(t *testing.T, cs *entadapter.CompositeStore) {
	t.Helper()
	var count int
	require.NoError(t, cs.DB().QueryRow("SELECT count(*) FROM sqlite_master WHERE name='decision_audits' OR name IN ('decisionaudit_timestamp', 'decisionaudit_principal_kind_principal_id', 'decisionaudit_credential_id', 'decisionaudit_route', 'decisionaudit_resource_type_resource_id', 'decisionaudit_result', 'decisionaudit_correlation_id', 'decisionaudit_denied_by')").Scan(&count))
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
				createLegacyDecisionAuditFixture(t, cs.DB())
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

func TestDecisionAuditRemoval_HealthOmitsLegacyWriter(t *testing.T) {
	srv, _ := testServer(t)
	info := srv.GetHealthInfo(context.Background())
	require.NotContains(t, info.Checks, decisionAuditLegacyHealthKey)
	require.Equal(t, "healthy", info.Checks[decisionAuditNewHealthKey])
	require.Equal(t, "healthy", info.Checks["database"])
	require.Equal(t, "healthy", info.Status)
}

// This disposable fixture follows all 25 columns and eight secondary indexes
// in the immutable baseline schema; it has no foreign keys.
func createLegacyDecisionAuditFixture(t *testing.T, db *sql.DB) {
	t.Helper()
	_, err := db.Exec(`CREATE TABLE decision_audits (
 id UUID NOT NULL PRIMARY KEY,
 timestamp DATETIME NOT NULL,
 principal_kind VARCHAR(255) NOT NULL,
 principal_id VARCHAR(255) NOT NULL,
 credential_id VARCHAR(255),
 credential_type VARCHAR(255),
 route VARCHAR(255),
 resource_type VARCHAR(255) NOT NULL,
 resource_id VARCHAR(255),
 permission VARCHAR(255) NOT NULL,
 result VARCHAR(255) NOT NULL,
 reason VARCHAR(255) NOT NULL,
 matched_policy VARCHAR(255),
 matched_grant VARCHAR(255),
 policy_id VARCHAR(255),
 correlation_id VARCHAR(255),
 sampled BOOLEAN NOT NULL DEFAULT false,
 permission_id VARCHAR(255),
 credential_name VARCHAR(255),
 credential_boundary_kind VARCHAR(255),
 credential_boundary_project_id VARCHAR(255),
 credential_labels VARCHAR(255),
 executor_kind VARCHAR(255),
 executor_id VARCHAR(255),
 denied_by VARCHAR(255)
)`)
	require.NoError(t, err)
	for _, statement := range []string{
		"CREATE INDEX decisionaudit_timestamp ON decision_audits (timestamp)",
		"CREATE INDEX decisionaudit_principal_kind_principal_id ON decision_audits (principal_kind, principal_id)",
		"CREATE INDEX decisionaudit_credential_id ON decision_audits (credential_id)",
		"CREATE INDEX decisionaudit_route ON decision_audits (route)",
		"CREATE INDEX decisionaudit_resource_type_resource_id ON decision_audits (resource_type, resource_id)",
		"CREATE INDEX decisionaudit_result ON decision_audits (result)",
		"CREATE INDEX decisionaudit_correlation_id ON decision_audits (correlation_id)",
		"CREATE INDEX decisionaudit_denied_by ON decision_audits (denied_by)",
	} {
		_, err := db.Exec(statement)
		require.NoError(t, err)
	}
	_, err = db.Exec(`INSERT INTO decision_audits
 (id, timestamp, principal_kind, principal_id, resource_type, permission, result, reason) VALUES
 ('00000000-0000-4000-8000-000000000001', '2026-01-01T00:00:00Z', 'user', 'fixture-user', 'project', 'project.read', 'allow', 'fixture allow'),
 ('00000000-0000-4000-8000-000000000002', '2026-01-01T00:00:00Z', 'user', 'fixture-user', 'project', 'project.read', 'deny', 'fixture deny')`)
	require.NoError(t, err)
	var rows, columns, indexes int
	require.NoError(t, db.QueryRow("SELECT count(*) FROM decision_audits").Scan(&rows))
	require.Equal(t, 2, rows)
	require.NoError(t, db.QueryRow("SELECT count(*) FROM pragma_table_info('decision_audits')").Scan(&columns))
	require.Equal(t, 25, columns)
	require.NoError(t, db.QueryRow("SELECT count(*) FROM sqlite_master WHERE type='index' AND name IN ('decisionaudit_timestamp', 'decisionaudit_principal_kind_principal_id', 'decisionaudit_credential_id', 'decisionaudit_route', 'decisionaudit_resource_type_resource_id', 'decisionaudit_result', 'decisionaudit_correlation_id', 'decisionaudit_denied_by')").Scan(&indexes))
	require.Equal(t, 8, indexes)
}
