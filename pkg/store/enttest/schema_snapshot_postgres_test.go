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

package enttest

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
	"testing"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"

	"github.com/GoogleCloudPlatform/scion/pkg/ent"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/entc"
)

// TestSchemaSnapshot_MatchesAutoMigrate proves that a schema provisioned by
// replaying the snapshot is identical to one provisioned by entc.AutoMigrate:
// the same relations, columns (order, type, nullability, default, identity,
// generation, collation), constraints, indexes, sequences, comments, enum and
// domain types, triggers, functions and row counts. It also checks that a
// second AutoMigrate run executes the same statements on both schemas, so
// Atlas sees no difference between them either. Any difference fails the test.
func TestSchemaSnapshot_MatchesAutoMigrate(t *testing.T) {
	if !Active() {
		t.Skip("SCION_TEST_POSTGRES_URL not set (or built without -tags integration); skipping Postgres-only snapshot test")
	}
	t.Setenv(forceAutoMigrateEnv, "") // this test is about the snapshot path
	ctx := context.Background()

	refURL, refSchema := newEmptySchema(t)
	refClient := openTestClient(t, refURL)
	if err := entc.AutoMigrate(ctx, refClient); err != nil {
		t.Fatalf("AutoMigrate into reference schema: %v", err)
	}

	snapURL, snapSchema := newEmptySchema(t)
	snapClient := openTestClient(t, snapURL)
	if err := migrateSchema(ctx, snapClient); err != nil {
		t.Fatalf("replaying the snapshot: %v", err)
	}

	ref := catalogFacts(t, refSchema)
	snap := catalogFacts(t, snapSchema)

	// Guard against comparing two empty catalogs.
	tables := 0
	for _, f := range ref {
		if strings.HasPrefix(f, "relation ") && strings.HasSuffix(f, " kind=r persistence=p") {
			tables++
		}
	}
	if tables < 50 {
		t.Fatalf("reference schema has %d tables, want the full Ent schema (more than 50)", tables)
	}
	if diff := diffFacts(ref, snap); diff != "" {
		t.Fatalf("snapshot-provisioned schema differs from the AutoMigrate one (- AutoMigrate, + snapshot):\n%s", diff)
	}
	t.Logf("compared %d catalog facts (%d tables): identical", len(ref), tables)

	// A further AutoMigrate run must see both schemas the same way.
	refRerun := recordAutoMigrate(t, refURL)
	snapRerun := recordAutoMigrate(t, snapURL)
	if refRerun != snapRerun {
		t.Fatalf("a second AutoMigrate run executed different statements:\nAutoMigrate schema:\n%s\nsnapshot schema:\n%s", refRerun, snapRerun)
	}
	if diff := diffFacts(ref, catalogFacts(t, snapSchema)); diff != "" {
		t.Fatalf("a second AutoMigrate run changed the snapshot schema:\n%s", diff)
	}

	// The comparison must notice a difference: drop one index from the
	// snapshot schema and expect a diff.
	var index string
	if err := adminDB.QueryRowContext(ctx,
		`SELECT indexname FROM pg_indexes i
		 WHERE schemaname = $1
		   AND NOT EXISTS (SELECT 1 FROM pg_constraint c WHERE c.conname = i.indexname AND c.connamespace = $1::regnamespace)
		 ORDER BY indexname LIMIT 1`, snapSchema).Scan(&index); err != nil {
		t.Fatalf("finding an index to drop: %v", err)
	}
	if _, err := adminDB.ExecContext(ctx, fmt.Sprintf(`DROP INDEX %s.%q`, snapSchema, index)); err != nil {
		t.Fatalf("dropping index %s: %v", index, err)
	}
	if diffFacts(ref, catalogFacts(t, snapSchema)) == "" {
		t.Fatalf("catalog comparison did not notice the dropped index %s", index)
	}

	// The same for column facts: drop one column default on a fresh
	// snapshot schema and expect a diff.
	colURL, colSchema := newEmptySchema(t)
	if err := migrateSchema(ctx, openTestClient(t, colURL)); err != nil {
		t.Fatalf("replaying the snapshot: %v", err)
	}
	if diff := diffFacts(ref, catalogFacts(t, colSchema)); diff != "" {
		t.Fatalf("second snapshot-provisioned schema differs from the AutoMigrate one:\n%s", diff)
	}
	var table, column string
	if err := adminDB.QueryRowContext(ctx,
		`SELECT table_name, column_name FROM information_schema.columns
		 WHERE table_schema = $1 AND column_default IS NOT NULL AND is_identity = 'NO'
		 ORDER BY table_name, column_name LIMIT 1`, colSchema).Scan(&table, &column); err != nil {
		t.Fatalf("finding a column with a default: %v", err)
	}
	if _, err := adminDB.ExecContext(ctx, fmt.Sprintf(`ALTER TABLE %s.%q ALTER COLUMN %q DROP DEFAULT`, colSchema, table, column)); err != nil {
		t.Fatalf("dropping the default of %s.%s: %v", table, column, err)
	}
	if diffFacts(ref, catalogFacts(t, colSchema)) == "" {
		t.Fatalf("catalog comparison did not notice the dropped default of %s.%s", table, column)
	}
}

// TestSchemaSnapshot_ForceAutoMigrateEnv checks the values that switch the
// harness back to AutoMigrate for every schema.
func TestSchemaSnapshot_ForceAutoMigrateEnv(t *testing.T) {
	for value, want := range map[string]bool{
		"": false, "0": false, "false": false, "FALSE": false,
		"1": true, "true": true, "yes": true,
	} {
		t.Setenv(forceAutoMigrateEnv, value)
		if got := forceAutoMigrate(); got != want {
			t.Errorf("%s=%q: forceAutoMigrate() = %v, want %v", forceAutoMigrateEnv, value, got, want)
		}
	}
}

// TestStatementRecorder covers the recorder's bookkeeping without a database:
// failed statements, savepoint control and rolled-back statements are left
// out of the script, and bind arguments are rejected.
func TestStatementRecorder(t *testing.T) {
	failed := errors.New("relation does not exist")

	t.Run("effective statements only", func(t *testing.T) {
		r := &statementRecorder{}
		r.begin()
		r.exec("SAVEPOINT normalize_0", []any{}, nil)
		r.exec("UPDATE runtime_brokers SET labels = NULL", []any{}, failed)
		r.exec("ROLLBACK TO SAVEPOINT normalize_0", []any{}, nil)
		r.exec("SAVEPOINT migrate_change_0", []any{}, nil)
		r.exec(`CREATE TABLE "a" ("id" bigint)`, []any{}, nil)
		r.exec("RELEASE SAVEPOINT migrate_change_0", []any{}, nil)
		r.exec("SAVEPOINT migrate_change_1", []any{}, nil)
		r.exec(`CREATE TABLE "b" ("id" bigint)`, []any{}, nil)
		r.exec("ROLLBACK TO SAVEPOINT migrate_change_1", []any{}, nil)
		r.exec(`CREATE INDEX "a_id" ON "a" ("id");`, []any{}, nil)
		r.commit()

		tx := r.begin()
		r.exec(`CREATE TABLE "c" ("id" bigint)`, []any{}, nil)
		r.rollback(tx)

		r.exec("DROP TABLE IF EXISTS decision_audits", []any{}, nil)

		got, err := r.script()
		if err != nil {
			t.Fatalf("script: %v", err)
		}
		want := `CREATE TABLE "a" ("id" bigint);` + "\n" +
			`CREATE INDEX "a_id" ON "a" ("id");` + "\n" +
			"DROP TABLE IF EXISTS decision_audits;\n"
		if got != want {
			t.Fatalf("script =\n%s\nwant\n%s", got, want)
		}
	})

	t.Run("rollback after commit keeps committed statements", func(t *testing.T) {
		r := &statementRecorder{}
		tx := &recordingTx{Tx: fakeTx{}, rec: r, start: r.begin()}
		if err := tx.Exec(context.Background(), `CREATE TABLE "a" ("id" bigint)`, []any{}, nil); err != nil {
			t.Fatalf("exec: %v", err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatalf("commit: %v", err)
		}
		_ = tx.Rollback() // the defer tx.Rollback() pattern
		got, err := r.script()
		if err != nil {
			t.Fatalf("script: %v", err)
		}
		if want := `CREATE TABLE "a" ("id" bigint);` + "\n"; got != want {
			t.Fatalf("script = %q, want %q", got, want)
		}
	})

	t.Run("rollback discards the transaction", func(t *testing.T) {
		r := &statementRecorder{}
		r.exec(`CREATE TABLE "a" ("id" bigint)`, []any{}, nil)
		tx := &recordingTx{Tx: fakeTx{}, rec: r, start: r.begin()}
		if err := tx.Exec(context.Background(), `CREATE TABLE "b" ("id" bigint)`, []any{}, nil); err != nil {
			t.Fatalf("exec: %v", err)
		}
		_ = tx.Rollback()
		got, err := r.script()
		if err != nil {
			t.Fatalf("script: %v", err)
		}
		if want := `CREATE TABLE "a" ("id" bigint);` + "\n"; got != want {
			t.Fatalf("script = %q, want %q", got, want)
		}
	})

	// txScript runs one CREATE TABLE "b" in a recordingTx over fake after a
	// CREATE TABLE "a" outside it, ends the transaction with end, and returns
	// the recorded script.
	txScript := func(t *testing.T, fake fakeTx, end func(tx *recordingTx)) string {
		t.Helper()
		r := &statementRecorder{}
		r.exec(`CREATE TABLE "a" ("id" bigint)`, []any{}, nil)
		tx := &recordingTx{Tx: fake, rec: r, start: r.begin()}
		if err := tx.Exec(context.Background(), `CREATE TABLE "b" ("id" bigint)`, []any{}, nil); err != nil {
			t.Fatalf("exec: %v", err)
		}
		end(tx)
		got, err := r.script()
		if err != nil {
			t.Fatalf("script: %v", err)
		}
		return got
	}
	onlyA := `CREATE TABLE "a" ("id" bigint);` + "\n"
	aAndB := onlyA + `CREATE TABLE "b" ("id" bigint);` + "\n"

	t.Run("rollback then commit stays discarded", func(t *testing.T) {
		got := txScript(t, fakeTx{}, func(tx *recordingTx) {
			_ = tx.Rollback()
			_ = tx.Commit()
		})
		if got != onlyA {
			t.Fatalf("script = %q, want %q", got, onlyA)
		}
	})

	t.Run("double commit keeps statements once", func(t *testing.T) {
		got := txScript(t, fakeTx{}, func(tx *recordingTx) {
			_ = tx.Commit()
			_ = tx.Commit()
		})
		if got != aAndB {
			t.Fatalf("script = %q, want %q", got, aAndB)
		}
	})

	t.Run("failed commit then rollback discards the transaction only", func(t *testing.T) {
		got := txScript(t, fakeTx{commitErr: errors.New("commit failed")}, func(tx *recordingTx) {
			if err := tx.Commit(); err == nil {
				t.Fatal("commit: want an error")
			}
			_ = tx.Rollback()
		})
		if got != onlyA {
			t.Fatalf("script = %q, want %q", got, onlyA)
		}
	})

	t.Run("bind arguments", func(t *testing.T) {
		r := &statementRecorder{}
		r.exec("INSERT INTO ent_types (type) VALUES ($1)", []any{"agents"}, nil)
		if _, err := r.script(); err == nil {
			t.Fatal("script accepted a statement with bind arguments")
		}
	})

	t.Run("unknown savepoint", func(t *testing.T) {
		r := &statementRecorder{}
		r.exec(`CREATE TABLE "a" ("id" bigint)`, []any{}, nil)
		r.exec("ROLLBACK TO SAVEPOINT nope", []any{}, nil)
		if _, err := r.script(); err == nil {
			t.Fatal("script accepted a rollback to an unknown savepoint")
		}
	})

	t.Run("empty", func(t *testing.T) {
		if _, err := (&statementRecorder{}).script(); err == nil {
			t.Fatal("script accepted an empty recording")
		}
	})
}

// fakeTx is a dialect.Tx whose statements always succeed, for exercising
// recordingTx without a database.
// Commit returns commitErr.
type fakeTx struct{ commitErr error }

func (fakeTx) Exec(context.Context, string, any, any) error  { return nil }
func (fakeTx) Query(context.Context, string, any, any) error { return nil }
func (f fakeTx) Commit() error                               { return f.commitErr }
func (fakeTx) Rollback() error                               { return nil }

// openTestClient opens a small Ent client on dsn, closed on cleanup.
func openTestClient(t *testing.T, dsn string) *ent.Client {
	t.Helper()
	client, err := entc.OpenPostgres(dsn, entc.PoolConfig{MaxOpenConns: 2, MaxIdleConns: 1})
	if err != nil {
		t.Fatalf("opening postgres ent client: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return client
}

// recordAutoMigrate runs entc.AutoMigrate against dsn and returns the
// statements that took effect, as a script ("" when there were none).
func recordAutoMigrate(t *testing.T, dsn string) string {
	t.Helper()
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("opening connection: %v", err)
	}
	defer db.Close()
	rec := &statementRecorder{}
	client := ent.NewClient(ent.Driver(&recordingDriver{Driver: entsql.OpenDB(dialect.Postgres, db), rec: rec}))
	if err := entc.AutoMigrate(context.Background(), client); err != nil {
		t.Fatalf("AutoMigrate: %v", err)
	}
	script, err := rec.script()
	if err != nil && len(rec.stmts) > 0 {
		t.Fatalf("recording AutoMigrate: %v", err)
	}
	return script
}

// catalogQueries describe a schema's objects. Each query takes the schema name
// as $1 and returns text columns; every row becomes one fact.
var catalogQueries = map[string]string{
	"relation": `
		SELECT c.relname, 'kind=' || c.relkind, 'persistence=' || c.relpersistence
		FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = $1`,
	"column": `
		SELECT c.relname, a.attnum::text, a.attname,
		       format_type(a.atttypid, a.atttypmod),
		       'notnull=' || a.attnotnull,
		       'default=' || COALESCE(pg_get_expr(d.adbin, d.adrelid), ''),
		       'identity=' || a.attidentity,
		       'generated=' || a.attgenerated,
		       'collation=' || COALESCE(co.collname, '')
		FROM pg_attribute a
		JOIN pg_class c ON c.oid = a.attrelid
		JOIN pg_namespace n ON n.oid = c.relnamespace
		LEFT JOIN pg_attrdef d ON d.adrelid = a.attrelid AND d.adnum = a.attnum
		LEFT JOIN pg_collation co ON co.oid = a.attcollation AND a.attcollation <> 0
		WHERE n.nspname = $1 AND a.attnum > 0 AND NOT a.attisdropped`,
	"constraint": `
		SELECT c.relname, con.conname, 'type=' || con.contype,
		       pg_get_constraintdef(con.oid),
		       'deferrable=' || con.condeferrable, 'deferred=' || con.condeferred,
		       'validated=' || con.convalidated
		FROM pg_constraint con
		JOIN pg_class c ON c.oid = con.conrelid
		JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = $1`,
	"index": `
		SELECT tablename, indexname, indexdef FROM pg_indexes WHERE schemaname = $1`,
	"sequence": `
		SELECT sequencename, data_type::text, start_value::text, min_value::text,
		       max_value::text, increment_by::text, 'cycle=' || cycle, cache_size::text
		FROM pg_sequences WHERE schemaname = $1`,
	"comment": `
		SELECT c.relname, COALESCE(a.attname, ''), d.description
		FROM pg_description d
		JOIN pg_class c ON c.oid = d.objoid AND d.classoid = 'pg_class'::regclass
		JOIN pg_namespace n ON n.oid = c.relnamespace
		LEFT JOIN pg_attribute a ON a.attrelid = c.oid AND a.attnum = d.objsubid AND d.objsubid > 0
		WHERE n.nspname = $1`,
	"type": `
		SELECT t.typname, 'type=' || t.typtype,
		       COALESCE((SELECT string_agg(e.enumlabel, ',' ORDER BY e.enumsortorder) FROM pg_enum e WHERE e.enumtypid = t.oid), ''),
		       COALESCE(format_type(t.typbasetype, t.typtypmod), '')
		FROM pg_type t JOIN pg_namespace n ON n.oid = t.typnamespace
		WHERE n.nspname = $1 AND t.typtype IN ('e', 'd')`,
	"trigger": `
		SELECT c.relname, tg.tgname, pg_get_triggerdef(tg.oid)
		FROM pg_trigger tg
		JOIN pg_class c ON c.oid = tg.tgrelid
		JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = $1 AND NOT tg.tgisinternal`,
	"function": `
		SELECT p.proname, pg_get_function_identity_arguments(p.oid), pg_get_functiondef(p.oid)
		FROM pg_proc p JOIN pg_namespace n ON n.oid = p.pronamespace
		WHERE n.nspname = $1 AND p.prokind IN ('f', 'p')`,
}

// catalogFacts returns a sorted list of one-line facts describing schema, with
// the schema name replaced by a placeholder so two schemas compare equal when
// their contents do. It includes the row count of every table.
func catalogFacts(t *testing.T, schema string) []string {
	t.Helper()
	ctx := context.Background()
	var facts []string
	kinds := make([]string, 0, len(catalogQueries))
	for kind := range catalogQueries {
		kinds = append(kinds, kind)
	}
	sort.Strings(kinds)
	for _, kind := range kinds {
		rows, err := adminDB.QueryContext(ctx, catalogQueries[kind], schema)
		if err != nil {
			t.Fatalf("catalog query %s: %v", kind, err)
		}
		cols, err := rows.Columns()
		if err != nil {
			rows.Close()
			t.Fatalf("catalog query %s columns: %v", kind, err)
		}
		for rows.Next() {
			vals := make([]sql.NullString, len(cols))
			ptrs := make([]any, len(cols))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			if err := rows.Scan(ptrs...); err != nil {
				rows.Close()
				t.Fatalf("catalog query %s scan: %v", kind, err)
			}
			parts := []string{kind}
			for _, v := range vals {
				parts = append(parts, v.String)
			}
			facts = append(facts, strings.ReplaceAll(strings.Join(parts, " "), schema, "<schema>"))
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			t.Fatalf("catalog query %s rows: %v", kind, err)
		}
		rows.Close()
	}

	tables, err := adminDB.QueryContext(ctx,
		`SELECT c.relname FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
		 WHERE n.nspname = $1 AND c.relkind IN ('r', 'p') ORDER BY c.relname`, schema)
	if err != nil {
		t.Fatalf("listing tables: %v", err)
	}
	var names []string
	for tables.Next() {
		var name string
		if err := tables.Scan(&name); err != nil {
			tables.Close()
			t.Fatalf("scanning table name: %v", err)
		}
		names = append(names, name)
	}
	if err := tables.Err(); err != nil {
		tables.Close()
		t.Fatalf("listing tables: %v", err)
	}
	tables.Close()
	for _, name := range names {
		var n int64
		if err := adminDB.QueryRowContext(ctx, fmt.Sprintf(`SELECT count(*) FROM %s.%q`, schema, name)).Scan(&n); err != nil {
			t.Fatalf("counting rows of %s: %v", name, err)
		}
		facts = append(facts, fmt.Sprintf("rows %s %d", name, n))
	}

	sort.Strings(facts)
	return facts
}

// diffFacts returns the facts only in want (prefixed "-") and only in got
// (prefixed "+"), one per line, or "" when the lists are equal.
func diffFacts(want, got []string) string {
	count := map[string]int{}
	for _, f := range want {
		count[f]++
	}
	for _, f := range got {
		count[f]--
	}
	var lines []string
	for f, n := range count {
		for ; n > 0; n-- {
			lines = append(lines, "- "+f)
		}
		for ; n < 0; n++ {
			lines = append(lines, "+ "+f)
		}
	}
	sort.Slice(lines, func(i, j int) bool { return lines[i][2:] < lines[j][2:] })
	return strings.Join(lines, "\n")
}
