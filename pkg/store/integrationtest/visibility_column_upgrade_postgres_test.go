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

package integrationtest

import (
	"context"
	"database/sql"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/ent/entc"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/entadapter"
	"github.com/GoogleCloudPlatform/scion/pkg/store/enttest"
)

// TestVisibilityColumnRemoval_UpgradeSucceeds_Postgres is the Postgres
// counterpart of pkg/store/entadapter's SQLite version (see that file for the
// full rationale). It requires SCION_TEST_POSTGRES_URL — run with:
//
//	go test -tags integration ./pkg/store/integrationtest/... \
//	  -run TestVisibilityColumnRemoval_UpgradeSucceeds_Postgres \
//	  with SCION_TEST_POSTGRES_URL set (enttest.NewSchemaURL skips otherwise).
//
// It proves that an already-upgraded Postgres database — which still carries
// the legacy `visibility` column (NOT NULL, with the SQL-level DEFAULT that
// Ent's Atlas-based migration engine stamps onto any field.Default) — keeps
// accepting inserts from code that no longer sets `visibility` at all.
func TestVisibilityColumnRemoval_UpgradeSucceeds_Postgres(t *testing.T) {
	ctx := context.Background()
	dsn := enttest.NewSchemaURL(t) // skips the test when Postgres is not configured

	raw, err := sql.Open("pgx", dsn)
	require.NoError(t, err)
	defer func() { _ = raw.Close() }()

	// Simulate a hub that ran the pre-#1904 code: the visibility column is
	// still physically present on all three tables, NOT NULL with a SQL-level
	// default — exactly what Ent's old schema (field.String("visibility").
	// Default("private")) created.
	for _, stmt := range []string{
		`ALTER TABLE agents ADD COLUMN visibility text NOT NULL DEFAULT 'private'`,
		`ALTER TABLE templates ADD COLUMN visibility text NOT NULL DEFAULT 'private'`,
		`ALTER TABLE harness_configs ADD COLUMN visibility text NOT NULL DEFAULT 'private'`,
	} {
		_, err := raw.ExecContext(ctx, stmt)
		require.NoError(t, err, stmt)
	}

	client, err := entc.OpenPostgres(dsn, entc.PoolConfig{MaxOpenConns: 2})
	require.NoError(t, err)
	defer func() { _ = client.Close() }()

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

	agentStore := entadapter.NewAgentStore(client)
	require.NoError(t, agentStore.CreateAgent(ctx, &store.Agent{
		ID:        uuid.NewString(),
		Slug:      "upgrade-agent",
		Name:      "Upgrade Agent",
		Template:  "default",
		ProjectID: projectID.String(),
		Phase:     "running",
	}), "agent create must succeed against a table with a legacy NOT NULL visibility column")

	templateStore := entadapter.NewTemplateStore(client)
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

	var got string
	require.NoError(t, raw.QueryRowContext(ctx, `SELECT visibility FROM agents WHERE slug = $1`, "upgrade-agent").Scan(&got))
	require.Equal(t, "private", got)
}
