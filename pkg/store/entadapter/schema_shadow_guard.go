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
	"fmt"
	"log/slog"

	"entgo.io/ent/dialect"
)

// schemaShadowMarkerTables are the tables whose presence marks a Postgres
// schema as holding a hub table set. Both are core hub tables that every Ent
// schema has had since the Postgres backend was added, created in the same
// Schema.Create transaction, so every Postgres hub install has both. Requiring two
// hub-specific names keeps an unrelated application's table (a lone "agents",
// say) from counting as a hub install.
var schemaShadowMarkerTables = [2]string{"agents", "runtime_brokers"}

// schemaShadowQuery lists the schemas on the connection's effective
// search_path, in order, and whether each holds the marker tables.
// current_schemas(false) expands "$user" and leaves out schemas that do not
// exist, as name resolution does; current_schema() is the first of them, where
// the Ent migration creates and looks up its unqualified tables. The catalog
// lookup filters on the namespace name explicitly and uses pg_class, which is
// visible whatever the table privileges.
const schemaShadowQuery = `
SELECT current_schema(), p.nspname,
	EXISTS (SELECT 1 FROM pg_catalog.pg_class c
		JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = p.nspname AND c.relname = $1 AND c.relkind IN ('r', 'p'))
	AND EXISTS (SELECT 1 FROM pg_catalog.pg_class c
		JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = p.nspname AND c.relname = $2 AND c.relkind IN ('r', 'p'))
FROM unnest(current_schemas(false)) WITH ORDINALITY AS p(nspname, ord)
ORDER BY p.ord`

// searchPathSchema is one schema on the effective search_path.
type searchPathSchema struct {
	name      string
	hasTables bool // holds the hub marker tables
}

// SchemaShadowError reports that the hub's migration would create a new,
// empty table set in the current schema while an existing hub table set sits
// in a later schema on the search_path.
type SchemaShadowError struct {
	// CurrentSchema is the first existing schema on the search_path, which
	// has no hub tables.
	CurrentSchema string
	// ShadowedSchema is the later schema on the search_path that holds the
	// existing hub tables.
	ShadowedSchema string
}

func (e *SchemaShadowError) Error() string {
	return fmt.Sprintf("refusing to start: PostgreSQL current schema %q has no hub tables, but schema %q later on the search_path does. "+
		"Migrating now would create a new, empty hub table set in %q that hides the existing data in %q. "+
		"Fix the connection's search_path (for example put %q first, or drop the %q schema if nothing else uses it), "+
		"or set server.database.allow_shadowed_schema: true (SCION_SERVER_DATABASE_ALLOWSHADOWEDSCHEMA=true) "+
		"to create the new table set in %q anyway",
		e.CurrentSchema, e.ShadowedSchema, e.CurrentSchema, e.ShadowedSchema,
		e.ShadowedSchema, e.CurrentSchema, e.CurrentSchema)
}

// decideSchemaShadow returns the error the guard reports for the given
// current schema and search_path, or nil when migrating is safe: the current
// schema already holds the hub tables (an existing install), or no schema on
// the path does (a fresh install). path lists the search_path schemas in
// order; the current schema is normally its first entry.
func decideSchemaShadow(current string, path []searchPathSchema) *SchemaShadowError {
	for _, s := range path {
		if s.name == current && s.hasTables {
			return nil
		}
	}
	for _, s := range path {
		if s.name != current && s.hasTables {
			return &SchemaShadowError{CurrentSchema: current, ShadowedSchema: s.name}
		}
	}
	return nil
}

// checkSchemaShadowing refuses a Postgres migration that would create a fresh
// hub table set shadowing an existing one later on the search_path
// (ptone/scion#4348). With allowShadowed set it logs a warning and lets the
// migration continue. Other dialects return nil without querying.
func checkSchemaShadowing(ctx context.Context, d string, db *sql.DB, allowShadowed bool, logger *slog.Logger) error {
	if d != dialect.Postgres || db == nil {
		return nil
	}
	rows, err := db.QueryContext(ctx, schemaShadowQuery, schemaShadowMarkerTables[0], schemaShadowMarkerTables[1])
	if err != nil {
		return fmt.Errorf("checking PostgreSQL search_path for existing hub tables: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var current sql.NullString
	var path []searchPathSchema
	for rows.Next() {
		var s searchPathSchema
		if err := rows.Scan(&current, &s.name, &s.hasTables); err != nil {
			return fmt.Errorf("checking PostgreSQL search_path for existing hub tables: %w", err)
		}
		path = append(path, s)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("checking PostgreSQL search_path for existing hub tables: %w", err)
	}
	// No existing schema on the search_path: there is nothing to shadow, and
	// the migration reports the missing schema itself.
	if !current.Valid {
		return nil
	}

	shadow := decideSchemaShadow(current.String, path)
	if shadow == nil {
		return nil
	}
	if !allowShadowed {
		return shadow
	}
	if logger == nil {
		logger = slog.Default()
	}
	logger.Warn("PostgreSQL search_path shadows existing hub tables; continuing because server.database.allow_shadowed_schema is set",
		"current_schema", shadow.CurrentSchema, "shadowed_schema", shadow.ShadowedSchema)
	return nil
}
