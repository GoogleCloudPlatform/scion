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

package enttest

import (
	"context"
	"database/sql"
	"os"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/ent/entc"

	_ "github.com/jackc/pgx/v5/stdlib"
)

// TestMain wires the package's Postgres lifecycle (MainSetup/MainTeardown):
// a no-op in the default (SQLite) build, and provisioning/dropping a
// per-package ephemeral Postgres database in the `integration` build when
// SCION_TEST_POSTGRES_URL is set.
//
// Note for anyone running the wider -tags integration suite alongside these
// tests: pkg/store/entadapter has one unrelated, pre-existing failure,
// TestUpsertConversationByExternalRef_FieldClassification/B_immutable (a
// nanosecond-vs-microsecond timestamp precision comparison). Verified
// directly, isolating just that one test: with the pre-fix
// normalizeBrokerLabels, it fails during setup at the migration step
// (SQLSTATE 25P02, the same production error this file's tests cover) and
// never reaches the CreatedAt assertion at all; with the fix applied, setup
// succeeds and the test proceeds to fail on the precision comparison
// instead. So this fix is what makes that assertion reachable in the first
// place — it doesn't cause the failure, but it isn't invisible to it
// either. Pre-existing and out of scope for this fix regardless.
func TestMain(m *testing.M) {
	MainSetup()
	code := m.Run()
	MainTeardown()
	os.Exit(code)
}

// TestAutoMigrate_FreshPostgres_ThenIdempotent is the fresh-database
// regression test for the normalizeBrokerLabels migration bug
// (pkg/ent/entc/client.go): two UPDATE runtime_brokers statements failed
// with 42P01 on a database where that table didn't exist yet, and the
// discarded Go error left the migration transaction aborted, so the next
// statement (the real migration's own SAVEPOINT) failed with an unrelated-
// looking SQLSTATE 25P02.
//
// Skips cleanly (does nothing) unless built with -tags integration and run
// with SCION_TEST_POSTGRES_URL set to a live Postgres server — see the
// enttest package doc and enttest_postgres.go.
//
//	go test -tags integration ./pkg/store/enttest/... \
//	  -run TestAutoMigrate_FreshPostgres_ThenIdempotent -v
//	SCION_TEST_POSTGRES_URL=postgres://postgres:postgres@127.0.0.1:5432/postgres?sslmode=disable
func TestAutoMigrate_FreshPostgres_ThenIdempotent(t *testing.T) {
	if !Active() {
		t.Skip("SCION_TEST_POSTGRES_URL not set (or built without -tags integration); skipping Postgres-only regression test")
	}

	// NewClient creates a brand-new, empty schema and calls entc.AutoMigrate
	// on it internally (enttest_postgres.go:158) — this alone is the exact
	// fresh-database scenario that crashed in production. NewClient calls
	// t.Fatalf on migration failure, so reaching the next line already
	// proves the first, fresh-database migration succeeded.
	client := NewClient(t)

	// Re-running migration against the now-populated schema must also
	// succeed: idempotence. (This does not, by itself, exercise the UPDATE
	// statements' "table exists" path or skipExistingRelations' 42P07 skip
	// — see TestNormalizeBrokerLabels_ExistingDatabase_NormalizesVarcharRows
	// below for that.)
	if err := entc.AutoMigrate(context.Background(), client); err != nil {
		t.Fatalf("second AutoMigrate run against an already-migrated schema failed (idempotence broken): %v", err)
	}
}

// TestNormalizeBrokerLabels_ExistingDatabase_NormalizesVarcharRows covers
// the existing-database path that TestAutoMigrate_FreshPostgres_ThenIdempotent
// does not: a pre-jsonb schema where labels/annotations are still varchar,
// with a row holding a literal empty string (the legacy raw-string-marshaled
// representation) rather than valid JSON. Re-running AutoMigrate must
// normalize that empty string to NULL before casting the column to jsonb —
// otherwise the ALTER COLUMN ... USING "labels"::jsonb fails on the invalid
// empty-string input (SQLSTATE 22P02).
//
// Skips cleanly unless built with -tags integration and run with
// SCION_TEST_POSTGRES_URL set — see TestAutoMigrate_FreshPostgres_ThenIdempotent.
func TestNormalizeBrokerLabels_ExistingDatabase_NormalizesVarcharRows(t *testing.T) {
	if !Active() {
		t.Skip("SCION_TEST_POSTGRES_URL not set (or built without -tags integration); skipping Postgres-only regression test")
	}

	// NewSchemaURL migrates a fresh schema (labels/annotations are jsonb)
	// and returns its DSN so we can drop to raw SQL against the same
	// schema/search_path.
	dsn := NewSchemaURL(t)

	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("opening raw connection: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	// Downgrade to the pre-jsonb representation and insert a row holding
	// the legacy empty-string value, simulating a database that predates
	// the labels/annotations jsonb migration.
	if _, err := db.ExecContext(context.Background(),
		`ALTER TABLE runtime_brokers
		   ALTER COLUMN labels TYPE varchar USING labels::text,
		   ALTER COLUMN annotations TYPE varchar USING annotations::text`,
	); err != nil {
		t.Fatalf("downgrading labels/annotations to varchar: %v", err)
	}
	if _, err := db.ExecContext(context.Background(),
		`INSERT INTO runtime_brokers (id, name, slug, labels, annotations, created, updated)
		 VALUES (gen_random_uuid(), 'test-broker', 'test-broker', '', '', now(), now())`,
	); err != nil {
		t.Fatalf("inserting legacy row with empty-string labels/annotations: %v", err)
	}

	// Re-open an Ent client against the same schema and re-migrate. This is
	// the exact scenario normalizeBrokerLabels exists for: UPDATE ... SET
	// labels = NULL WHERE labels::text = '' must run (and succeed) before
	// the plan's ALTER COLUMN ... TYPE jsonb USING "labels"::jsonb, or the
	// cast fails on the invalid empty string.
	client, err := entc.OpenPostgres(dsn, entc.PoolConfig{MaxOpenConns: 2, MaxIdleConns: 1})
	if err != nil {
		t.Fatalf("opening postgres ent client: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	if err := entc.AutoMigrate(context.Background(), client); err != nil {
		t.Fatalf("AutoMigrate against an existing varchar schema with a legacy empty-string row failed: %v", err)
	}

	// The column must be jsonb again, and the row's labels/annotations must
	// have been normalized to NULL, not left as an invalid empty string.
	var dataType string
	if err := db.QueryRowContext(context.Background(),
		`SELECT data_type FROM information_schema.columns
		 WHERE table_name = 'runtime_brokers' AND column_name = 'labels'`,
	).Scan(&dataType); err != nil {
		t.Fatalf("querying labels column type: %v", err)
	}
	if dataType != "jsonb" {
		t.Errorf("labels column type = %q, want jsonb", dataType)
	}

	var labels, annotations sql.NullString
	if err := db.QueryRowContext(context.Background(),
		`SELECT labels, annotations FROM runtime_brokers WHERE slug = 'test-broker'`,
	).Scan(&labels, &annotations); err != nil {
		t.Fatalf("querying normalized row: %v", err)
	}
	if labels.Valid {
		t.Errorf("labels = %q, want NULL", labels.String)
	}
	if annotations.Valid {
		t.Errorf("annotations = %q, want NULL", annotations.String)
	}
}
