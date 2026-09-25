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

//go:build !no_sqlite

package entadapter

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/ent/entc"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// oldSkillsDDL is the frozen schema for the "skills" table as it existed
// before ptone/scion#1903 removed the `visibility` column from the ent
// schema. This is what an already-deployed database looks like right after
// upgrade: the column and its NOT NULL DEFAULT 'private' constraint stay in
// place (ent does not drop columns on auto-migrate — see
// migrate.WithDropColumn(false) in pkg/ent/entc/client.go), even though the
// application no longer reads or writes it.
const oldSkillsDDL = "CREATE TABLE `skills` (" +
	"`id` uuid NOT NULL, `name` text NOT NULL, `slug` text NOT NULL, " +
	"`description` text NULL, `tags` text NULL, " +
	"`scope` text NOT NULL DEFAULT ('global'), `scope_id` text NULL, " +
	"`storage_uri` text NULL, `storage_bucket` text NULL, `storage_path` text NULL, " +
	"`status` text NOT NULL DEFAULT ('active'), `owner_id` text NULL, " +
	"`created_by` text NULL, `updated_by` text NULL, " +
	"`visibility` text NOT NULL DEFAULT ('private'), " +
	"`created` datetime NOT NULL, `updated` datetime NOT NULL, PRIMARY KEY (`id`))"

// TestSkillStore_CreateAgainstOldSchemaWithVisibilityColumn proves that
// removing `visibility` from the ent schema does not break skill creation
// against an already-upgraded database (ptone/scion#1903 review request).
// The column stays — NOT NULL with the SQL-level DEFAULT 'private' that was
// baked in when the column was originally created — and current code no
// longer references it at all, so a new insert simply omits it from the
// column list and SQLite (and, by the same DEFAULT-column mechanism,
// Postgres — see entgo.io/ent/dialect/sql/schema/{sqlite,postgres}.go)
// supplies the stored default. Without that default this insert would fail
// with a NOT NULL constraint violation.
func TestSkillStore_CreateAgainstOldSchemaWithVisibilityColumn(t *testing.T) {
	dsn := "file:" + filepath.Join(t.TempDir(), "old-schema.db")

	raw, err := sql.Open("sqlite", dsn)
	require.NoError(t, err)
	_, err = raw.ExecContext(context.Background(), oldSkillsDDL)
	require.NoError(t, err)
	require.NoError(t, raw.Close())

	client, err := entc.OpenSQLite(dsn, entc.PoolConfig{MaxOpenConns: 1})
	require.NoError(t, err)
	require.NoError(t, entc.AutoMigrate(context.Background(), client))

	cs := NewCompositeStore(client)
	t.Cleanup(func() { _ = cs.Close() })

	skill := &store.Skill{
		ID:     uuid.New().String(),
		Name:   "old-schema-skill",
		Slug:   "old-schema-skill",
		Scope:  "global",
		Status: "active",
	}
	require.NoError(t, cs.CreateSkill(context.Background(), skill),
		"creating a skill against a pre-existing NOT NULL visibility column must succeed via its SQL-level default")

	db := cs.DB()
	require.NotNil(t, db)
	var visibility string
	require.NoError(t, db.QueryRow("SELECT visibility FROM skills WHERE id = ?", skill.ID).Scan(&visibility))
	assert.Equal(t, "private", visibility,
		"the DB-level default should populate visibility even though the application never sets it")
}
