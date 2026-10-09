// Copyright 2026 Google LLC
// SPDX-License-Identifier: Apache-2.0

//go:build !no_sqlite

package entadapter

import (
	"context"
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
	_, err = cs.DB().Exec("CREATE TABLE decision_audits (id TEXT PRIMARY KEY, reason TEXT); INSERT INTO decision_audits VALUES ('a','allow'),('b','deny')")
	require.NoError(t, err)
	for round := range 2 {
		require.NoError(t, cs.MigrateWithSchemaLock(ctx))
		var count int
		require.NoError(t, cs.DB().QueryRow("SELECT count(*) FROM sqlite_master WHERE name='decision_audits'").Scan(&count))
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
