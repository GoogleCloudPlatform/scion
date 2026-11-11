// Copyright 2026 Google LLC
// SPDX-License-Identifier: Apache-2.0

//go:build !no_sqlite

package entadapter

import (
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/ent"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/accessconstraint"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/entc"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/require"
)

func TestDecisionAuditDrop_CompositeMigrationConservesAudits(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "conservation.db")
	open := func() (*ent.Client, *CompositeStore) {
		client, err := entc.OpenSQLite(path, entc.PoolConfig{MaxOpenConns: 1})
		require.NoError(t, err)
		return client, NewCompositeStore(client)
	}
	client, cs := open()
	require.NoError(t, cs.MigrateWithSchemaLock(ctx))
	user, err := client.User.Create().SetEmail("conservation@example.com").SetDisplayName("keep user").Save(ctx)
	require.NoError(t, err)
	project, err := client.Project.Create().SetName("keep project").SetSlug("keep-project").Save(ctx)
	require.NoError(t, err)
	constraint, err := client.AccessConstraint.Create().SetName("keep constraint").SetSubjectKind(accessconstraint.SubjectKindAllPrincipals).SetScopeType(accessconstraint.ScopeTypeSystem).SetMaximumPermissions([]string{"project.read"}).Save(ctx)
	require.NoError(t, err)
	mutation := &store.MutationAuditRecord{MutationType: "test_conservation", ActorPrincipalKind: "user", ActorPrincipalID: user.ID.String(), TargetType: "project", TargetID: project.ID.String(), CorrelationID: "keep-correlation"}
	history := &store.AccessConstraintHistory{EventID: "keep-history", ConstraintID: constraint.ID.String(), OccurredAt: time.Now().UTC(), Operation: "create", ActorKind: "user", ActorID: user.ID.String()}
	require.NoError(t, cs.WithTx(ctx, func(tx store.Store) error {
		if err := tx.CreateMutationAudit(ctx, mutation); err != nil {
			return err
		}
		return tx.AppendConstraintHistoryTx(ctx, history)
	}))
	beforeMutation, _, err := cs.ListMutationAudits(ctx, store.MutationAuditFilter{CorrelationID: "keep-correlation", Limit: 10})
	require.NoError(t, err)
	beforeHistory, err := cs.ListConstraintHistory(ctx, constraint.ID.String())
	require.NoError(t, err)
	tableCounts := func() map[string]int {
		rows, err := cs.DB().Query("SELECT name FROM sqlite_master WHERE type='table' AND name != 'decision_audits'")
		require.NoError(t, err)
		var names []string
		for rows.Next() {
			var name string
			require.NoError(t, rows.Scan(&name))
			names = append(names, name)
		}
		require.NoError(t, rows.Err())
		require.NoError(t, rows.Close())
		counts := map[string]int{}
		for _, name := range names {
			var count int
			require.NoError(t, cs.DB().QueryRow("SELECT count(*) FROM \""+name+"\"").Scan(&count))
			counts[name] = count
		}
		return counts
	}
	beforeUser, err := json.Marshal(user)
	require.NoError(t, err)
	beforeProject, err := json.Marshal(project)
	require.NoError(t, err)
	beforeCounts := tableCounts()
	createLegacyDecisionAuditFixture(t, cs.DB())
	for round := range 2 {
		require.NoError(t, cs.MigrateWithSchemaLock(ctx))
		var count int
		require.NoError(t, cs.DB().QueryRow("SELECT count(*) FROM sqlite_master WHERE name='decision_audits' OR name IN ('decisionaudit_timestamp', 'decisionaudit_principal_kind_principal_id', 'decisionaudit_credential_id', 'decisionaudit_route', 'decisionaudit_resource_type_resource_id', 'decisionaudit_result', 'decisionaudit_correlation_id', 'decisionaudit_denied_by')").Scan(&count))
		require.Zero(t, count)
		require.Equal(t, beforeCounts, tableCounts(), "every unrelated table's row count survives")
		gotMutation, _, err := cs.ListMutationAudits(ctx, store.MutationAuditFilter{CorrelationID: "keep-correlation", Limit: 10})
		require.NoError(t, err)
		require.Equal(t, beforeMutation, gotMutation)
		gotHistory, err := cs.ListConstraintHistory(ctx, constraint.ID.String())
		require.NoError(t, err)
		require.Equal(t, beforeHistory, gotHistory)
		gotUser, err := client.User.Get(ctx, user.ID)
		require.NoError(t, err)
		gotUserData, err := json.Marshal(gotUser)
		require.NoError(t, err)
		require.JSONEq(t, string(beforeUser), string(gotUserData))
		gotProject, err := client.Project.Get(ctx, project.ID)
		require.NoError(t, err)
		gotProjectData, err := json.Marshal(gotProject)
		require.NoError(t, err)
		require.JSONEq(t, string(beforeProject), string(gotProjectData))
		if round == 0 {
			require.NoError(t, cs.Close())
			client, cs = open()
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
