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

package main

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"sort"
	"strings"

	entsql "entgo.io/ent/dialect/sql"
	"github.com/google/uuid"

	"github.com/GoogleCloudPlatform/scion/pkg/ent/entc"
)

// schemaMigrationsTable is the bookkeeping table excluded from coverage — it is
// not a domain table, so it carries no fixture rows.
const schemaMigrationsTable = "schema_migrations"

// TableCount records the number of fixture rows seeded into a table.
type TableCount struct {
	Table string
	Count int
}

// Report summarizes a fixture generation run.
type Report struct {
	Path    string       // path to the generated .db
	Counts  []TableCount // per-table row counts (sorted by table name)
	Missing []string     // domain tables with zero rows (coverage failures)
}

type tableColumn struct {
	Type string
}

type tableColumns map[string]tableColumn

// TotalTables returns the number of domain tables (excluding schema_migrations)
// the report covers.
func (r *Report) TotalTables() int { return len(r.Counts) }

// Generate builds a fresh fixture database at path by running the schema
// migrations and seeding the Go-defined Spec, then performs the coverage check.
// Foreign-key enforcement is disabled during seeding so rows can be inserted in
// spec order without a topological sort; the resulting .db is still loadable.
func Generate(ctx context.Context, path string) (*Report, error) {
	// Start from a clean file so re-runs are deterministic.
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("removing existing fixture %s: %w", path, err)
	}

	client, err := entc.OpenSQLite("file:"+path, entc.PoolConfig{})
	if err != nil {
		return nil, fmt.Errorf("opening fixture db: %w", err)
	}
	defer func() { _ = client.Close() }()

	if err := entc.AutoMigrate(ctx, client); err != nil {
		return nil, fmt.Errorf("migrating fixture db: %w", err)
	}

	drv, ok := client.Driver().(*entsql.Driver)
	if !ok {
		return nil, fmt.Errorf("ent client driver does not expose a *sql.DB")
	}
	db := drv.DB()
	if _, err := db.ExecContext(ctx, "PRAGMA foreign_keys = OFF"); err != nil {
		return nil, fmt.Errorf("disabling foreign keys: %w", err)
	}

	columns := map[string]tableColumns{}
	for _, tf := range Spec() {
		tableCols, err := columnsForTable(ctx, db, tf.Table)
		if err != nil {
			return nil, err
		}
		columns[tf.Table] = tableCols
		for i, r := range tf.Rows {
			if err := insertRow(ctx, db, tf.Table, i, r, columns[tf.Table]); err != nil {
				return nil, fmt.Errorf("seeding %s row %d: %w", tf.Table, i, err)
			}
		}
	}

	report, err := checkCoverage(ctx, db, path)
	if err != nil {
		return nil, err
	}
	return report, nil
}

// checkCoverage lists every domain table in the database and counts its rows.
// A table with zero rows is recorded in Report.Missing.
func checkCoverage(ctx context.Context, db *sql.DB, path string) (*Report, error) {
	tables, err := listTables(ctx, db)
	if err != nil {
		return nil, err
	}

	report := &Report{Path: path}
	for _, t := range tables {
		var n int
		if err := db.QueryRowContext(ctx, fmt.Sprintf("SELECT COUNT(*) FROM %q", t)).Scan(&n); err != nil {
			return nil, fmt.Errorf("counting rows in %s: %w", t, err)
		}
		report.Counts = append(report.Counts, TableCount{Table: t, Count: n})
		if n == 0 {
			report.Missing = append(report.Missing, t)
		}
	}
	return report, nil
}

// listTables returns the sorted set of domain table names (excluding SQLite
// internal tables and the schema_migrations bookkeeping table).
func listTables(ctx context.Context, db *sql.DB) ([]string, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%' ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("listing tables: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var tables []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		if name == schemaMigrationsTable {
			continue
		}
		tables = append(tables, name)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.Strings(tables)
	return tables, nil
}

func columnsForTable(ctx context.Context, db *sql.DB, table string) (tableColumns, error) {
	rows, err := db.QueryContext(ctx, fmt.Sprintf("PRAGMA table_info(%q)", table))
	if err != nil {
		return nil, fmt.Errorf("listing columns for %s: %w", table, err)
	}
	defer func() { _ = rows.Close() }()

	cols := tableColumns{}
	for rows.Next() {
		var cid int
		var name, typ string
		var notNull int
		var defaultValue any
		var pk int
		if err := rows.Scan(&cid, &name, &typ, &notNull, &defaultValue, &pk); err != nil {
			return nil, fmt.Errorf("scanning columns for %s: %w", table, err)
		}
		cols[name] = tableColumn{Type: strings.ToLower(typ)}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(cols) == 0 {
		return nil, fmt.Errorf("table %s does not exist", table)
	}
	return cols, nil
}

// insertRow inserts a single fixture row using a parameterized statement so
// values are escaped by the driver. Columns are sorted for deterministic SQL.
func insertRow(ctx context.Context, db *sql.DB, table string, index int, r row, columns tableColumns) error {
	cols := make([]string, 0, len(r))
	values := make(row, len(r))
	for c, v := range r {
		col := normalizeColumn(table, c, columns)
		if col == "" {
			return fmt.Errorf("column %s does not exist", c)
		}
		values[col] = v
	}
	if col, ok := columns["id"]; ok && !strings.Contains(col.Type, "int") {
		if _, ok := values["id"]; !ok {
			values["id"] = uuid.NewSHA1(uuid.NameSpaceOID, []byte(fmt.Sprintf("fixture:%s:%d", table, index))).String()
		}
	}
	if _, ok := columns["created"]; ok {
		if _, ok := values["created"]; !ok {
			values["created"] = baseTime
		}
	}
	if _, ok := columns["updated"]; ok {
		if _, ok := values["updated"]; !ok {
			values["updated"] = baseTime
		}
	}
	for c := range values {
		cols = append(cols, c)
	}
	sort.Strings(cols)

	placeholders := make([]string, len(cols))
	vals := make([]any, len(cols))
	quoted := make([]string, len(cols))
	for i, c := range cols {
		placeholders[i] = "?"
		quoted[i] = fmt.Sprintf("%q", c)
		vals[i] = encode(values[c])
	}

	q := fmt.Sprintf("INSERT INTO %q (%s) VALUES (%s)",
		table, strings.Join(quoted, ", "), strings.Join(placeholders, ", "))
	_, err := db.ExecContext(ctx, q, vals...)
	return err
}

func normalizeColumn(table, column string, columns tableColumns) string {
	if _, ok := columns[column]; ok {
		return column
	}
	switch column {
	case "created_at":
		if _, ok := columns["created"]; ok {
			return "created"
		}
	case "updated_at":
		if _, ok := columns["updated"]; ok {
			return "updated"
		}
	case "agent_id":
		if _, ok := columns["slug"]; table == "agents" && ok {
			return "slug"
		}
	}
	return ""
}

// encode normalizes Go values into forms the SQLite driver accepts. Booleans
// become 0/1 integers; everything else (string, int, []byte, time.Time, nil)
// passes through unchanged.
func encode(v any) any {
	if b, ok := v.(bool); ok {
		if b {
			return 1
		}
		return 0
	}
	return v
}
