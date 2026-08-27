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

package entadapter

import (
	"context"
	"database/sql"
	"log/slog"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
)

// deduplicateAccessPolicies removes duplicate access_policies rows before the
// Ent auto-migration adds a UNIQUE index on (name, scope_type, scope_id).
// Existing databases may contain duplicate rows from before the unique
// constraint was introduced (PR #993), which would cause the migration to fail
// with "UNIQUE constraint failed". For each set of duplicates the oldest row
// (by "created" timestamp) is kept and the rest are deleted.
//
// The function is idempotent: when no duplicates exist (or the table does not
// exist yet on a fresh database) it is a no-op.
func (c *CompositeStore) deduplicateAccessPolicies(ctx context.Context) error {
	db := c.DB()
	if db == nil {
		return nil
	}

	exists, err := c.accessPoliciesTableExists(ctx, db)
	if err != nil || !exists {
		return err
	}

	// Delete duplicate rows, keeping the oldest per (name, scope_type, scope_id).
	// ROW_NUMBER() OVER … is supported by both SQLite (≥3.25) and Postgres.
	result, err := db.ExecContext(ctx, `
		DELETE FROM access_policies
		WHERE id IN (
			SELECT id FROM (
				SELECT id, ROW_NUMBER() OVER (
					PARTITION BY name, scope_type, scope_id
					ORDER BY created ASC
				) AS rn
				FROM access_policies
			) sub WHERE rn > 1
		)
	`)
	if err != nil {
		return err
	}

	if n, _ := result.RowsAffected(); n > 0 {
		slog.Info("deduplicated access_policies before migration", "rows_deleted", n)
	}
	return nil
}

// deduplicateDelegationEdges removes duplicate active delegation_edges rows
// before the Ent auto-migration adds a partial UNIQUE index on
// (delegate_type, delegate_id, scope_type, scope_id) WHERE active = true.
// Existing databases that ran the initial backfill at commit 3597507 and were
// interrupted mid-backfill may contain duplicate active edges for the same
// (delegate, scope) tuple. For each set of duplicates the oldest row (by
// "created" timestamp) is kept and the rest are deleted.
//
// The function is idempotent: when no duplicates exist (or the table does not
// exist yet on a fresh database) it is a no-op.
func (c *CompositeStore) deduplicateDelegationEdges(ctx context.Context) error {
	db := c.DB()
	if db == nil {
		return nil
	}

	exists, err := c.tableExists(ctx, db, "delegation_edges")
	if err != nil || !exists {
		return err
	}

	// Delete duplicate active rows, keeping the oldest per
	// (delegate_type, delegate_id, scope_type, scope_id).
	// ROW_NUMBER() OVER … is supported by both SQLite (≥3.25) and Postgres.
	result, err := db.ExecContext(ctx, `
		DELETE FROM delegation_edges
		WHERE id IN (
			SELECT id FROM (
				SELECT id, ROW_NUMBER() OVER (
					PARTITION BY delegate_type, delegate_id, scope_type, scope_id
					ORDER BY created ASC
				) AS rn
				FROM delegation_edges
				WHERE active = true
			) sub WHERE rn > 1
		)
	`)
	if err != nil {
		return err
	}

	if n, _ := result.RowsAffected(); n > 0 {
		slog.Info("deduplicated delegation_edges before migration", "rows_deleted", n)
	}
	return nil
}

// tableExists checks whether a table exists in the database.
// SQLite and Postgres use different system catalogs.
func (c *CompositeStore) tableExists(ctx context.Context, db *sql.DB, tableName string) (bool, error) {
	drv, ok := c.client.Driver().(*entsql.Driver)
	if !ok {
		return false, nil
	}

	var query string
	switch drv.Dialect() {
	case dialect.Postgres:
		query = `SELECT table_name FROM information_schema.tables WHERE table_schema = 'public' AND table_name = '` + tableName + `'`
	case dialect.SQLite:
		query = `SELECT name FROM sqlite_master WHERE type='table' AND name='` + tableName + `'`
	default:
		return false, nil
	}

	var name string
	err := db.QueryRowContext(ctx, query).Scan(&name)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// accessPoliciesTableExists checks whether the access_policies table exists
// in the database. SQLite and Postgres use different system catalogs.
func (c *CompositeStore) accessPoliciesTableExists(ctx context.Context, db *sql.DB) (bool, error) {
	drv, ok := c.client.Driver().(*entsql.Driver)
	if !ok {
		return false, nil
	}

	var query string
	switch drv.Dialect() {
	case dialect.Postgres:
		query = `SELECT table_name FROM information_schema.tables WHERE table_schema = 'public' AND table_name = 'access_policies'`
	case dialect.SQLite:
		query = `SELECT name FROM sqlite_master WHERE type='table' AND name='access_policies'`
	default:
		return false, nil
	}

	var name string
	err := db.QueryRowContext(ctx, query).Scan(&name)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}
