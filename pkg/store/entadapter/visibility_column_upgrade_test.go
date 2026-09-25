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
	"github.com/stretchr/testify/require"
)

// TestVisibilityColumnRemoval_UpgradeSucceeds guards ptone/scion#1904's
// column-retention decision: the `visibility` field was removed from the
// agent/template/harness-config Ent schemas, but the physical database column
// is deliberately NOT dropped (schema.WithDropColumn(false) in
// pkg/ent/entc/client.go). An already-upgraded database therefore still has a
// `visibility` column on all three tables — NOT NULL, with the SQL-level
// DEFAULT('private') that Ent's Atlas-based migration engine stamps onto any
// field.Default (see entgo.io/ent@v0.14.5/dialect/sql/schema/atlas.go:atDefault
// and sqlite.go/postgres.go's supportsDefault, both unconditionally true).
//
// This test proves the load-bearing consequence: code that no longer sets
// `visibility` at all can still INSERT into that legacy NOT-NULL column,
// because the column's own SQL DEFAULT satisfies the constraint. Without a
// DB-level default this would fail with a NOT NULL constraint violation on
// every agent/template/harness-config create after the upgrade.
func TestVisibilityColumnRemoval_UpgradeSucceeds(t *testing.T) {
	dsn := "file:" + filepath.Join(t.TempDir(), "test.db")
	ctx := context.Background()

	client, err := entc.OpenSQLite(dsn, entc.PoolConfig{MaxOpenConns: 1})
	require.NoError(t, err)
	require.NoError(t, entc.AutoMigrate(ctx, client))

	// Simulate a hub that ran the pre-#1904 code: the visibility column is
	// still physically present on all three tables, NOT NULL with a SQL-level
	// default — exactly what Ent's old schema (field.String("visibility").
	// Default("private")) created.
	raw, err := sql.Open("sqlite", dsn)
	require.NoError(t, err)
	for _, stmt := range []string{
		"ALTER TABLE agents ADD COLUMN visibility text NOT NULL DEFAULT 'private'",
		"ALTER TABLE templates ADD COLUMN visibility text NOT NULL DEFAULT 'private'",
		"ALTER TABLE harness_configs ADD COLUMN visibility text NOT NULL DEFAULT 'private'",
	} {
		_, err := raw.ExecContext(ctx, stmt)
		require.NoError(t, err, stmt)
	}
	require.NoError(t, raw.Close())

	// Re-running AutoMigrate must leave the now-unknown column alone
	// (WithDropColumn(false)) rather than erroring or dropping it.
	require.NoError(t, entc.AutoMigrate(ctx, client))

	projectID := uuid.New()
	_, err = client.Project.Create().
		SetID(projectID).
		SetName("upgrade-test-project").
		SetSlug("upgrade-test-project").
		Save(ctx)
	require.NoError(t, err)

	agentStore := NewAgentStore(client)
	require.NoError(t, agentStore.CreateAgent(ctx, &store.Agent{
		ID:        uuid.NewString(),
		Slug:      "upgrade-agent",
		Name:      "Upgrade Agent",
		Template:  "default",
		ProjectID: projectID.String(),
		Phase:     "running",
	}), "agent create must succeed against a table with a legacy NOT NULL visibility column")

	templateStore := NewTemplateStore(client)
	require.NoError(t, templateStore.CreateTemplate(ctx, &store.Template{
		ID:      uuid.New().String(),
		Name:    "upgrade-template",
		Slug:    "upgrade-template",
		Harness: "claude",
		Scope:   store.TemplateScopeGlobal,
	}), "template create must succeed against a table with a legacy NOT NULL visibility column")

	require.NoError(t, templateStore.CreateHarnessConfig(ctx, &store.HarnessConfig{
		ID:      uuid.New().String(),
		Name:    "upgrade-harness-config",
		Slug:    "upgrade-harness-config",
		Harness: "claude",
		Scope:   store.HarnessConfigScopeGlobal,
	}), "harness config create must succeed against a table with a legacy NOT NULL visibility column")

	// Belt-and-braces: confirm the SQL default actually fired, rather than the
	// insert having succeeded for some unrelated reason.
	raw2, err := sql.Open("sqlite", dsn)
	require.NoError(t, err)
	defer func() { _ = raw2.Close() }()
	var got string
	require.NoError(t, raw2.QueryRowContext(ctx, "SELECT visibility FROM agents WHERE slug = ?", "upgrade-agent").Scan(&got))
	require.Equal(t, "private", got)

	require.NoError(t, client.Close())
}
