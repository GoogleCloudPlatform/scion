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

//go:build integration

package entc_test

import (
	"context"
	"database/sql"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/GoogleCloudPlatform/scion/pkg/ent/entc"
)

// oldSkillsDDLPostgres mirrors the frozen pre-ptone/scion#1903 "skills"
// table schema (see pkg/store/entadapter/skill_visibility_column_test.go for
// the SQLite counterpart, which does run locally): a NOT NULL `visibility`
// column with a SQL-level DEFAULT 'private', exactly as an already-deployed
// Postgres database looks like right after upgrade — the column stays (ent
// does not drop columns on auto-migrate; see migrate.WithDropColumn(false)
// in client.go) even though current code never references it.
const oldSkillsDDLPostgres = `CREATE TABLE skills (
	id uuid PRIMARY KEY,
	name text NOT NULL,
	slug text NOT NULL,
	description text NULL,
	tags text NULL,
	scope text NOT NULL DEFAULT 'global',
	scope_id text NULL,
	storage_uri text NULL,
	storage_bucket text NULL,
	storage_path text NULL,
	status text NOT NULL DEFAULT 'active',
	owner_id text NULL,
	created_by text NULL,
	updated_by text NULL,
	visibility text NOT NULL DEFAULT 'private',
	created timestamptz NOT NULL,
	updated timestamptz NOT NULL
)`

// TestSkillStore_CreateAgainstOldSchemaWithVisibilityColumn_Postgres is the
// Postgres counterpart requested for ptone/scion#1903 review: proves that
// removing `visibility` from the ent schema does not break skill creation
// against an already-upgraded Postgres database. Requires a real Postgres
// instance; not available in this sandbox (no postgres binary, no CI job
// wires SCION_PG_TEST_DSN for this package — same limitation as the
// pre-existing TestMigrateBeta_SQLiteToPostgres in this file). Run with:
//
//	SCION_PG_TEST_DSN='postgres://user:pass@host:5432/db?sslmode=require' \
//	  go test -tags integration -run TestSkillStore_CreateAgainstOldSchemaWithVisibilityColumn_Postgres ./pkg/ent/entc/...
func TestSkillStore_CreateAgainstOldSchemaWithVisibilityColumn_Postgres(t *testing.T) {
	dsn := os.Getenv("SCION_PG_TEST_DSN")
	if dsn == "" {
		t.Skip("SCION_PG_TEST_DSN not set; skipping Postgres integration test")
	}
	ctx := context.Background()

	resetPostgresSchema(t, dsn)

	raw, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open postgres: %v", err)
	}
	defer raw.Close()
	if _, err := raw.ExecContext(ctx, oldSkillsDDLPostgres); err != nil {
		t.Fatalf("create old-schema skills table: %v", err)
	}

	client, err := entc.OpenPostgres(dsn, entc.PoolConfig{MaxOpenConns: 5})
	if err != nil {
		t.Fatalf("open ent client: %v", err)
	}
	defer client.Close()
	if err := entc.AutoMigrate(ctx, client); err != nil {
		t.Fatalf("auto-migrate: %v", err)
	}

	id := uuid.New()
	now := time.Now()
	_, err = client.Skill.Create().
		SetID(id).
		SetName("old-schema-skill").
		SetSlug("old-schema-skill").
		SetScope("global").
		SetCreated(now).
		SetUpdated(now).
		Save(ctx)
	if err != nil {
		t.Fatalf("creating a skill against a pre-existing NOT NULL visibility column must succeed via its SQL-level default: %v", err)
	}

	var visibility string
	if err := raw.QueryRowContext(ctx, "SELECT visibility FROM skills WHERE id = $1", id).Scan(&visibility); err != nil {
		t.Fatalf("query visibility: %v", err)
	}
	if visibility != "private" {
		t.Errorf("expected DB-level default 'private', got %q", visibility)
	}
}
