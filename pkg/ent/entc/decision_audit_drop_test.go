// Copyright 2026 Google LLC
// SPDX-License-Identifier: Apache-2.0

//go:build !no_sqlite

package entc

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/GoogleCloudPlatform/scion/pkg/ent"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/migrate"
	"github.com/stretchr/testify/require"
)

func requireNoDecisionTable(t *testing.T, client *ent.Client) {
	t.Helper()
	db := client.Driver().(*entsql.Driver).DB()
	var count int
	require.NoError(t, db.QueryRow("SELECT count(*) FROM sqlite_master WHERE name = 'decision_audits' OR name = 'legacy_decision_index'").Scan(&count))
	require.Zero(t, count)
}

func TestDecisionAuditDrop_Fresh(t *testing.T) {
	client := newTestClient(t)
	requireNoDecisionTable(t, client)
	for _, table := range migrate.Tables {
		require.NotEqual(t, "decision_audits", table.Name)
	}
	require.NoError(t, client.Schema.Create(context.Background()))
	requireNoDecisionTable(t, client)
}

func TestDecisionAuditDrop_UpgradeRestart(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "upgrade.db")
	open := func() *ent.Client {
		client, err := OpenSQLite(path, PoolConfig{MaxOpenConns: 1})
		require.NoError(t, err)
		return client
	}
	client := open()
	db := client.Driver().(*entsql.Driver).DB()
	_, err := db.Exec("CREATE TABLE decision_audits (id TEXT PRIMARY KEY, reason TEXT NOT NULL)")
	require.NoError(t, err)
	_, err = db.Exec("CREATE INDEX legacy_decision_index ON decision_audits(reason)")
	require.NoError(t, err)
	_, err = db.Exec("INSERT INTO decision_audits VALUES ('one','allow'), ('two','deny')")
	require.NoError(t, err)
	_, err = db.Exec("CREATE TABLE migration_sentinel (id TEXT PRIMARY KEY, payload BLOB)")
	require.NoError(t, err)
	_, err = db.Exec("INSERT INTO migration_sentinel VALUES ('keep', X'000102FF')")
	require.NoError(t, err)
	require.NoError(t, AutoMigrate(ctx, client))
	requireNoDecisionTable(t, client)
	require.NoError(t, client.Close())
	client = open()
	defer client.Close()
	for range 2 {
		require.NoError(t, AutoMigrate(ctx, client))
		requireNoDecisionTable(t, client)
	}
	var payload []byte
	db = client.Driver().(*entsql.Driver).DB()
	require.NoError(t, db.QueryRow("SELECT payload FROM migration_sentinel WHERE id='keep'").Scan(&payload))
	require.Equal(t, []byte{0, 1, 2, 255}, payload)
	var count int
	require.NoError(t, db.QueryRow("SELECT count(*) FROM migration_sentinel").Scan(&count))
	require.Equal(t, 1, count)
}

type decisionDropDriver struct {
	dialect.Driver
	name     string
	tx       *decisionDropTx
	beginErr error
	begins   int
}

func (d *decisionDropDriver) Dialect() string { return d.name }
func (d *decisionDropDriver) Tx(context.Context) (dialect.Tx, error) {
	d.begins++
	return d.tx, d.beginErr
}

type decisionDropTx struct {
	dialect.Tx
	execErr, commitErr error
	statements         []string
	commits, rollbacks int
}

func (tx *decisionDropTx) Exec(_ context.Context, query string, args, result any) error {
	tx.statements = append(tx.statements, query)
	return tx.execErr
}
func (tx *decisionDropTx) Commit() error   { tx.commits++; return tx.commitErr }
func (tx *decisionDropTx) Rollback() error { tx.rollbacks++; return nil }

func TestDecisionAuditDrop_AtomicFailure(t *testing.T) {
	fault := errors.New("fixture fault")
	for _, stage := range []string{"begin", "exec", "commit"} {
		t.Run(stage, func(t *testing.T) {
			tx := &decisionDropTx{}
			driver := &decisionDropDriver{name: dialect.SQLite, tx: tx}
			switch stage {
			case "begin":
				driver.beginErr = fault
			case "exec":
				tx.execErr = fault
			case "commit":
				tx.commitErr = fault
			}
			client := ent.NewClient(ent.Driver(driver))
			require.ErrorIs(t, dropDecisionAuditTable(context.Background(), client), fault)
			require.Equal(t, 1, driver.begins)
			if stage != "begin" {
				require.Equal(t, 1, tx.rollbacks)
			}
			if stage == "exec" {
				require.Zero(t, tx.commits)
			}
		})
	}
}

func TestDecisionAuditDrop_PostgresDialect(t *testing.T) {
	tx := &decisionDropTx{}
	driver := &decisionDropDriver{name: dialect.Postgres, tx: tx}
	client := ent.NewClient(ent.Driver(driver))
	for range 2 {
		require.NoError(t, dropDecisionAuditTable(context.Background(), client))
	}
	require.Equal(t, []string{"DROP TABLE IF EXISTS decision_audits", "DROP TABLE IF EXISTS decision_audits"}, tx.statements)
	require.Equal(t, 2, tx.commits)
	require.Zero(t, tx.rollbacks)
	driver.name = "unsupported"
	require.ErrorContains(t, dropDecisionAuditTable(context.Background(), client), "unsupported dialect")
	require.Equal(t, 2, driver.begins)
}
