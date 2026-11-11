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
	"fmt"
	"log/slog"

	"entgo.io/ent/dialect"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// MigrateWithSchemaLock runs Migrate serialized across processes. On Postgres
// it holds the session-level advisory lock store.LockSchemaMigration on a
// dedicated connection for the whole of Migrate (pre-migration backfills,
// entc.AutoMigrate, post-migration backfills and seed data), so concurrent Hub
// replicas booting against the same database apply the schema one at a time.
// entc.AutoMigrate itself has no locking, and concurrent CREATE TABLE races on
// the Postgres catalog (SQLSTATE 23505 on pg_type_typname_nsp_index), which the
// 42P07 skip hook does not cover; this lock is the only guard
// (ptone/scion#1078). Other dialects (SQLite) are single-writer and run Migrate
// directly.
func (c *CompositeStore) MigrateWithSchemaLock(ctx context.Context) error {
	if c.Dialect() != dialect.Postgres {
		return c.Migrate(ctx)
	}

	db := c.DB()
	if db == nil {
		return fmt.Errorf("postgres store does not expose a database connection")
	}
	conn, err := db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("acquiring migration lock connection: %w", err)
	}
	defer func() { _ = conn.Close() }()

	if _, err := conn.ExecContext(ctx, "SELECT pg_advisory_lock($1)", int64(store.LockSchemaMigration)); err != nil {
		return fmt.Errorf("acquiring migration advisory lock: %w", err)
	}
	locked := true
	defer func() {
		if locked {
			if _, err := conn.ExecContext(context.Background(), "SELECT pg_advisory_unlock($1)", int64(store.LockSchemaMigration)); err != nil {
				slog.Error("Failed to release migration advisory lock", "error", err)
			}
		}
	}()

	if err := c.Migrate(ctx); err != nil {
		return err
	}

	if _, err := conn.ExecContext(context.Background(), "SELECT pg_advisory_unlock($1)", int64(store.LockSchemaMigration)); err != nil {
		return fmt.Errorf("releasing migration advisory lock: %w", err)
	}
	locked = false
	return nil
}
