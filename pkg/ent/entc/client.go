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
	"fmt"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
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
		db.Close()
		return nil, fmt.Errorf("enabling foreign keys: %w", err)
	}
	if _, err := db.Exec("PRAGMA journal_mode = WAL"); err != nil {
		db.Close()
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
		db.Close()
		return nil, fmt.Errorf("enabling foreign keys: %w", err)
	}
	if _, err := db.Exec("PRAGMA query_only = ON"); err != nil {
		db.Close()
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
	// Use the pgx stdlib driver, which registers itself as "pgx" via the
	// blank import in driver_postgres.go. It accepts both keyword/value DSNs
	// ("host=... port=...") and URL-style ("postgres://...") connection strings.
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, fmt.Errorf("opening postgres connection: %w", err)
	}
	pool.apply(db)
	drv := entsql.OpenDB(dialect.Postgres, db)
	client := ent.NewClient(append(opts, ent.Driver(drv))...)
	return client, nil
}

// AutoMigrate runs automatic schema migration on the given client.
func AutoMigrate(ctx context.Context, client *ent.Client) error {
	if err := client.Schema.Create(ctx, migrate.WithDropIndex(true), migrate.WithDropColumn(true)); err != nil {
		return fmt.Errorf("running auto-migration: %w", err)
	}
	return nil
}
