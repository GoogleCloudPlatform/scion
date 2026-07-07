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

// Package entc provides factory functions for creating Ent clients with
// SQLite or PostgreSQL backends.
package entc

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/stdlib"

	"github.com/GoogleCloudPlatform/scion/pkg/ent"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/migrate"
)

// PoolConfig holds connection pool settings applied to the underlying
// *sql.DB after it is opened. A zero value leaves the corresponding pool
// setting at the database/sql default (i.e. the field is only applied when
// it is greater than zero).
//
// NOTE: for SQLite, MaxOpenConns must be 1 to serialize writes and avoid
// "database is locked" errors; callers are responsible for supplying that.
type PoolConfig struct {
	MaxOpenConns    int
	MaxIdleConns    int
	ConnMaxLifetime time.Duration
	// ConnMaxIdleTime bounds how long a connection may sit idle in the pool
	// before being closed. Set it shorter than the server/proxy idle timeout
	// (CloudSQL drops idle connections after ~10m) so the pool recycles a
	// connection before the remote silently closes it; otherwise the first
	// request after an idle period stalls waiting for a dead connection to time
	// out. A zero value leaves the database/sql default (no idle limit).
	ConnMaxIdleTime time.Duration
}

// apply sets the pool parameters on db, skipping any unset (non-positive) field.
func (p PoolConfig) apply(db *sql.DB) {
	if p.MaxOpenConns > 0 {
		db.SetMaxOpenConns(p.MaxOpenConns)
	}
	if p.MaxIdleConns > 0 {
		db.SetMaxIdleConns(p.MaxIdleConns)
	}
	if p.ConnMaxLifetime > 0 {
		db.SetConnMaxLifetime(p.ConnMaxLifetime)
	}
	if p.ConnMaxIdleTime > 0 {
		db.SetConnMaxIdleTime(p.ConnMaxIdleTime)
	}
}

// OpenSQLite creates an Ent client backed by SQLite.
// The dsn should be a SQLite connection string (e.g. "file:ent?mode=memory&cache=shared").
// Foreign keys and WAL journal mode are enabled automatically.
// This uses the modernc.org/sqlite pure-Go driver which registers as "sqlite".
func OpenSQLite(dsn string, pool PoolConfig, opts ...ent.Option) (*ent.Client, error) {
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("opening sqlite connection: %w", err)
	}
	// Enable foreign keys and WAL mode, matching existing store pattern.
	if _, err := db.Exec("PRAGMA foreign_keys = ON"); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("enabling foreign keys: %w", err)
	}
	if _, err := db.Exec("PRAGMA journal_mode = WAL"); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("enabling WAL mode: %w", err)
	}
	pool.apply(db)
	drv := entsql.OpenDB(dialect.SQLite, db)
	client := ent.NewClient(append(opts, ent.Driver(drv))...)
	return client, nil
}

// OpenSQLiteReadOnly creates an Ent client backed by a read-only SQLite
// database. It is used by the migration tool to read from a source SQLite file
// without mutating it: the connection is opened with `PRAGMA query_only = ON`
// so any accidental write fails loudly, and—unlike OpenSQLite—it does NOT
// switch the journal to WAL mode (doing so would write to the database header
// and fail on a query-only connection).
//
// MaxOpenConns is forced to 1 because the query_only and foreign_keys pragmas
// are connection-scoped; with a larger pool, unprimed connections would not
// inherit them.
func OpenSQLiteReadOnly(dsn string, opts ...ent.Option) (*ent.Client, error) {
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("opening sqlite connection: %w", err)
	}
	// Pin to a single connection so the pragmas below apply to every query.
	db.SetMaxOpenConns(1)
	// Foreign keys on for read consistency; query_only to guarantee the source
	// is never modified during migration.
	if _, err := db.Exec("PRAGMA foreign_keys = ON"); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("enabling foreign keys: %w", err)
	}
	if _, err := db.Exec("PRAGMA query_only = ON"); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("enabling query_only mode: %w", err)
	}
	drv := entsql.OpenDB(dialect.SQLite, db)
	client := ent.NewClient(append(opts, ent.Driver(drv))...)
	return client, nil
}

// OpenPostgres creates an Ent client backed by PostgreSQL.
// The dsn should be a PostgreSQL connection string
// (e.g. "host=localhost port=5432 user=scion dbname=scion sslmode=disable").
func OpenPostgres(dsn string, pool PoolConfig, opts ...ent.Option) (*ent.Client, error) {
	// Parse the DSN with pgx (accepts both keyword/value DSNs "host=... port=..."
	// and URL-style "postgres://..." connection strings) so we can attach TCP
	// keepalive settings to the connection before handing it to database/sql via
	// stdlib.OpenDB. Keepalives let the OS detect a connection silently dropped by
	// a peer (e.g. CloudSQL recycling idle backends or a NAT timeout) instead of
	// the first query after idle hanging on a dead socket.
	connConfig, err := pgx.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("parsing postgres dsn: %w", err)
	}
	applyKeepalives(connConfig.RuntimeParams)
	if connConfig.ConnectTimeout == 0 {
		connConfig.ConnectTimeout = connectTimeout
	}

	db := stdlib.OpenDB(*connConfig)
	pool.apply(db)
	drv := entsql.OpenDB(dialect.Postgres, db)
	client := ent.NewClient(append(opts, ent.Driver(drv))...)
	return client, nil
}

const connectTimeout = 10 * time.Second

// applyKeepalives sets server-side TCP keepalive GUCs as pgx RuntimeParams so the
// kernel probes idle connections and tears down dead ones promptly. Values mirror
// the pgx event pool (events_postgres.go): probe after 60s idle, every 15s, give
// up after 4 missed probes (~2 min to detect a dead peer). Existing keys are not
// overwritten so an explicit DSN setting wins.
func applyKeepalives(params map[string]string) {
	defaults := map[string]string{
		"tcp_keepalives_idle":     "60",
		"tcp_keepalives_interval": "15",
		"tcp_keepalives_count":    "4",
	}
	for k, v := range defaults {
		if _, ok := params[k]; !ok {
			params[k] = v
		}
	}
}

// TODO: integration test for Postgres reset path (requires postgres container)

// AutoMigrate runs automatic schema migration on the given client.
// For Postgres, it uses a two-pass strategy:
//  1. First pass: DROP all tables in the public schema that were created by
//     a prior Ent schema version (this clears the slate for new tables).
//  2. Second pass: run Schema.Create to create all tables fresh.
//
// Tables that exist with the correct schema are unaffected because DROP is
// selective (only tables that belong to the Ent schema are dropped).
// This is safe for a hosted deployment where schema changes are additive
// and data is re-created from the Hub state on each deployment.
func AutoMigrate(ctx context.Context, client *ent.Client) error {
	// First: try a clean Schema.Create. If it succeeds (empty DB), we're done.
	err := client.Schema.Create(
		ctx,
		migrate.WithDropColumn(false),
		migrate.WithDropIndex(false),
	)
	if err == nil {
		return nil
	}

	// If we got "already exists" (42P07), the DB has a prior schema.
	// Drop all Ent-managed tables so Schema.Create can run cleanly.
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "42P07" {
		return fmt.Errorf("running auto-migration: %w", err)
	}

	// Use the raw DB connection to drop all tables in the Ent schema.
	drv, ok := client.Driver().(*entsql.Driver)
	if !ok {
		return fmt.Errorf("migration reset requires an entsql.Driver, got %T", client.Driver())
	}
	db := drv.DB()
	// Get all table names from the Ent schema via information_schema.
	rows, err := db.QueryContext(ctx,
		"SELECT tablename FROM pg_tables WHERE schemaname='public' ORDER BY tablename")
	if err != nil {
		return fmt.Errorf("listing tables for migration reset: %w", err)
	}
	defer rows.Close()
	var tables []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return fmt.Errorf("scanning table name: %w", err)
		}
		tables = append(tables, name)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterating tables: %w", err)
	}

	// Create a map of Ent-managed tables for selective dropping.
	entTables := make(map[string]bool)
	for _, t := range migrate.Tables {
		entTables[t.Name] = true
	}

	slog.Warn("AutoMigrate: dropping all Ent-managed tables for schema reset (hosted mode only)")

	// Tables are dropped individually without a transaction. A crash mid-drop
	// leaves the DB in a partially-dropped state; this is acceptable for hosted
	// deployments where data is reconstructed from Hub state on startup.
	for _, t := range tables {
		if entTables[t] {
			quotedName := pgx.Identifier{t}.Sanitize()
			if _, err := db.ExecContext(ctx, `DROP TABLE IF EXISTS `+quotedName+` CASCADE`); err != nil {
				return fmt.Errorf("dropping table %q: %w", t, err)
			}
		}
	}

	// Now run Schema.Create on the empty schema.
	if err := client.Schema.Create(
		ctx,
		migrate.WithDropColumn(false),
		migrate.WithDropIndex(false),
	); err != nil {
		return fmt.Errorf("running auto-migration after reset: %w", err)
	}
	return nil
}
