// Copyright 2026 Google LLC
// SPDX-License-Identifier: Apache-2.0

//go:build integration

package integrationtest

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/ent/entc"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/entadapter"
	"github.com/GoogleCloudPlatform/scion/pkg/store/enttest"
	"github.com/stretchr/testify/require"
)

func TestDecisionAuditDrop_PostgresUpgradeRestart(t *testing.T) {
	ctx := context.Background()
	dsn := enttest.NewSchemaURL(t)
	open := func() *entadapter.CompositeStore {
		client, err := entc.OpenPostgres(dsn, entc.PoolConfig{MaxOpenConns: 4})
		require.NoError(t, err)
		return entadapter.NewCompositeStore(client)
	}
	cs := open()
	require.NoError(t, cs.MigrateWithSchemaLock(ctx))
	absent := func() {
		var count int
		require.NoError(t, cs.DB().QueryRow("SELECT count(*) FROM information_schema.tables WHERE table_schema=current_schema() AND table_name='decision_audits'").Scan(&count))
		require.Zero(t, count)
		require.NoError(t, cs.DB().QueryRow("SELECT count(*) FROM pg_indexes WHERE schemaname=current_schema() AND indexname IN ('decisionaudit_timestamp', 'decisionaudit_principal_kind_principal_id', 'decisionaudit_credential_id', 'decisionaudit_route', 'decisionaudit_resource_type_resource_id', 'decisionaudit_result', 'decisionaudit_correlation_id', 'decisionaudit_denied_by')").Scan(&count))
		require.Zero(t, count)
	}
	absent()
	project := seedProject(t, cs)
	mutation := &store.MutationAuditRecord{MutationType: "keep_mutation", ActorPrincipalKind: "user", ActorPrincipalID: "fixture-user", TargetType: "project", TargetID: project.ID}
	require.NoError(t, cs.WithTx(ctx, func(tx store.Store) error { return tx.CreateMutationAudit(ctx, mutation) }))
	constraint, err := cs.CreateAccessConstraint(ctx, &store.AccessConstraint{Name: "keep constraint", SubjectKind: store.ConstraintSubjectAllPrincipals, ScopeType: "system", MaximumPermissions: []string{"project.read"}})
	require.NoError(t, err)
	require.NoError(t, cs.WithTx(ctx, func(tx store.Store) error {
		return tx.AppendConstraintHistoryTx(ctx, &store.AccessConstraintHistory{EventID: "keep-history", ConstraintID: constraint.ID, OccurredAt: time.Now().UTC(), Operation: "create"})
	}))
	beforeHistory, err := cs.ListConstraintHistory(ctx, constraint.ID)
	require.NoError(t, err)
	counts := func() map[string]int {
		rows, err := cs.DB().Query("SELECT table_name FROM information_schema.tables WHERE table_schema=current_schema() AND table_type='BASE TABLE' AND table_name != 'decision_audits'")
		require.NoError(t, err)
		var names []string
		for rows.Next() {
			var name string
			require.NoError(t, rows.Scan(&name))
			names = append(names, name)
		}
		require.NoError(t, rows.Err())
		require.NoError(t, rows.Close())
		result := map[string]int{}
		for _, name := range names {
			var count int
			require.NoError(t, cs.DB().QueryRow("SELECT count(*) FROM \""+name+"\"").Scan(&count))
			result[name] = count
		}
		return result
	}
	beforeCounts := counts()
	before, _, err := cs.ListMutationAudits(ctx, store.MutationAuditFilter{MutationType: "keep_mutation", Limit: 10})
	require.NoError(t, err)
	createLegacyDecisionAuditFixture(t, cs.DB())
	for round := range 2 {
		require.NoError(t, cs.MigrateWithSchemaLock(ctx))
		absent()
		require.Equal(t, beforeCounts, counts())
		gotHistory, err := cs.ListConstraintHistory(ctx, constraint.ID)
		require.NoError(t, err)
		require.Equal(t, beforeHistory, gotHistory)
		got, _, err := cs.ListMutationAudits(ctx, store.MutationAuditFilter{MutationType: "keep_mutation", Limit: 10})
		require.NoError(t, err)
		require.Equal(t, before, got)
		gotProject, err := cs.GetProject(ctx, project.ID)
		require.NoError(t, err)
		require.Equal(t, project.Name, gotProject.Name)
		if round == 0 {
			require.NoError(t, cs.Close())
			cs = open()
		}
	}
	require.NoError(t, cs.Close())
}

// This disposable fixture follows all 25 columns and eight secondary indexes
// in the immutable baseline schema; it has no foreign keys.
func createLegacyDecisionAuditFixture(t *testing.T, db *sql.DB) {
	t.Helper()
	_, err := db.Exec(`CREATE TABLE decision_audits (
 id UUID NOT NULL PRIMARY KEY,
 timestamp TIMESTAMPTZ NOT NULL,
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
	require.NoError(t, db.QueryRow("SELECT count(*) FROM information_schema.columns WHERE table_schema=current_schema() AND table_name='decision_audits'").Scan(&columns))
	require.Equal(t, 25, columns)
	require.NoError(t, db.QueryRow("SELECT count(*) FROM pg_indexes WHERE schemaname=current_schema() AND indexname IN ('decisionaudit_timestamp', 'decisionaudit_principal_kind_principal_id', 'decisionaudit_credential_id', 'decisionaudit_route', 'decisionaudit_resource_type_resource_id', 'decisionaudit_result', 'decisionaudit_correlation_id', 'decisionaudit_denied_by')").Scan(&indexes))
	require.Equal(t, 8, indexes)
}
