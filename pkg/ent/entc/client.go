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
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"

	"github.com/GoogleCloudPlatform/scion/pkg/ent"
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

// AutoMigrate runs automatic schema migration on the given client.
func AutoMigrate(ctx context.Context, client *ent.Client) error {
	if err := client.Schema.Create(ctx); err != nil {
		return fmt.Errorf("running auto-migration: %w", err)
	}
	return nil
}
