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

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/GoogleCloudPlatform/scion/pkg/ent"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/migrate"
)

// OpenSQLite creates an Ent client backed by SQLite.
// The dsn should be a SQLite connection string (e.g. "file:ent?mode=memory&cache=shared").
// Foreign keys and WAL journal mode are enabled automatically.
// This uses the modernc.org/sqlite pure-Go driver which registers as "sqlite".
// The returned *sql.DB is the same handle wrapped by the Ent client; callers
// that don't need it can discard with _. Closing the client closes the DB.
func OpenSQLite(dsn string, opts ...ent.Option) (*ent.Client, *sql.DB, error) {
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, nil, fmt.Errorf("opening sqlite connection: %w", err)
	}
	// Enable foreign keys and WAL mode, matching existing store pattern.
	if _, err := db.Exec("PRAGMA foreign_keys = ON"); err != nil {
		db.Close()
		return nil, nil, fmt.Errorf("enabling foreign keys: %w", err)
	}
	if _, err := db.Exec("PRAGMA journal_mode = WAL"); err != nil {
		db.Close()
		return nil, nil, fmt.Errorf("enabling WAL mode: %w", err)
	}
	drv := entsql.OpenDB(dialect.SQLite, db)
	client := ent.NewClient(append(opts, ent.Driver(drv))...)
	return client, db, nil
}

// OpenPostgres creates an Ent client backed by PostgreSQL.
// The dsn should be a PostgreSQL connection string
// (e.g. "host=localhost port=5432 user=scion dbname=scion sslmode=disable").
// Returns the underlying *sql.DB so callers can apply pool configuration.
func OpenPostgres(dsn string, opts ...ent.Option) (*ent.Client, *sql.DB, error) {
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		return nil, nil, fmt.Errorf("opening postgres connection: %w", err)
	}
	drv := entsql.OpenDB(dialect.Postgres, db)
	client := ent.NewClient(append(opts, ent.Driver(drv))...)
	return client, db, nil
}

// AutoMigrate runs automatic schema migration on the given client.
func AutoMigrate(ctx context.Context, client *ent.Client) error {
	if err := client.Schema.Create(ctx, migrate.WithDropIndex(true), migrate.WithDropColumn(true)); err != nil {
		return fmt.Errorf("running auto-migration: %w", err)
	}
	return nil
}
