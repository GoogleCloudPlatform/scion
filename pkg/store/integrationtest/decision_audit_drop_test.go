// Copyright 2026 Google LLC
// SPDX-License-Identifier: Apache-2.0

//go:build integration

package integrationtest

import (
	"context"
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
	_, err = cs.DB().Exec("CREATE TABLE decision_audits (id TEXT PRIMARY KEY, reason TEXT); INSERT INTO decision_audits VALUES ('one','allow'),('two','deny')")
	require.NoError(t, err)
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
