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
	"testing"

	"entgo.io/ent/dialect"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	_ "github.com/jackc/pgx/v5/stdlib"
)

// TestDecideSchemaShadow covers the guard's decision without a database.
func TestDecideSchemaShadow(t *testing.T) {
	hub := func(name string) searchPathSchema { return searchPathSchema{name: name, hasTables: true} }
	empty := func(name string) searchPathSchema { return searchPathSchema{name: name} }

	cases := []struct {
		name    string
		current string
		path    []searchPathSchema
		shadow  string // "" means no refusal
	}{
		{"custom search_path shadows public", "custom", []searchPathSchema{empty("custom"), hub("public")}, "public"},
		{"empty $user schema shadows public", "scion", []searchPathSchema{empty("scion"), hub("public")}, "public"},
		{"first hub schema reported", "a", []searchPathSchema{empty("a"), empty("b"), hub("c"), hub("d")}, "c"},
		{"fresh install", "custom", []searchPathSchema{empty("custom"), empty("public")}, ""},
		{"fresh install single schema", "public", []searchPathSchema{empty("public")}, ""},
		{"existing install in current schema", "custom", []searchPathSchema{hub("custom"), empty("public")}, ""},
		{"existing install in both schemas", "custom", []searchPathSchema{hub("custom"), hub("public")}, ""},
		{"existing install default path", "public", []searchPathSchema{hub("public")}, ""},
		{"empty path", "public", nil, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := decideSchemaShadow(tc.current, tc.path)
			if tc.shadow == "" {
				assert.Nil(t, got)
				return
			}
			require.NotNil(t, got)
			assert.Equal(t, tc.current, got.CurrentSchema)
			assert.Equal(t, tc.shadow, got.ShadowedSchema)
		})
	}
}

// TestSchemaShadowError_Text checks that the refusal names both schemas and
// the override, in its settings and environment spellings.
func TestSchemaShadowError_Text(t *testing.T) {
	msg := (&SchemaShadowError{CurrentSchema: "custom", ShadowedSchema: "public"}).Error()
	assert.Contains(t, msg, `current schema "custom" has no hub tables`)
	assert.Contains(t, msg, `schema "public" later on the search_path does`)
	assert.Contains(t, msg, "server.database.allow_shadowed_schema: true")
	assert.Contains(t, msg, "SCION_SERVER_DATABASE_ALLOWSHADOWEDSCHEMA=true")
	assert.Contains(t, msg, `drop the "custom" schema if nothing else uses it`)
	assert.NotContains(t, msg, "drop the empty", "the guard does not know the schema is empty")
}

// TestSchemaShadowMarkerTables pins the marker: both tables are required, so
// a schema holding only one of them is not a hub install.
func TestSchemaShadowMarkerTables(t *testing.T) {
	assert.Equal(t, [2]string{"agents", "runtime_brokers"}, schemaShadowMarkerTables)
	assert.Contains(t, schemaShadowQuery, "current_schemas(false)")
	assert.Contains(t, schemaShadowQuery, "n.nspname = p.nspname AND c.relname = $1")
	assert.Contains(t, schemaShadowQuery, "n.nspname = p.nspname AND c.relname = $2")
	assert.Contains(t, schemaShadowQuery, "))\n\tAND EXISTS (SELECT 1", "both marker tables are required")
}

// TestCheckSchemaShadowing_DialectGate checks that only Postgres queries the
// database. The handle is closed, so any query fails: the Postgres call must
// report that failure and the SQLite call must not touch the handle.
func TestCheckSchemaShadowing_DialectGate(t *testing.T) {
	db, err := sql.Open("pgx", "postgres://placeholder.invalid/db")
	require.NoError(t, err)
	require.NoError(t, db.Close())
	ctx := context.Background()

	assert.NoError(t, checkSchemaShadowing(ctx, dialect.SQLite, db, false, nil))
	assert.NoError(t, checkSchemaShadowing(ctx, dialect.Postgres, nil, false, nil))
	err = checkSchemaShadowing(ctx, dialect.Postgres, db, false, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "checking PostgreSQL search_path")
}
