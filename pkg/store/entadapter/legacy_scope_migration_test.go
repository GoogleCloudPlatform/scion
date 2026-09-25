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
	"path/filepath"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/storage"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// TestNormalizeLegacyGroveScopes_MigratesAndIsIdempotent seeds one
// scope='grove' row per affected table with raw SQL (simulating rows
// written before the grove→project rename's schema migration, which never
// touched templates or harness_configs, plus a row subscription_templates
// could accumulate before its create handler validated scope), runs
// CompositeStore.Migrate a second time, and asserts every row is rewritten
// to scope='project'. It also asserts that ResourceStoragePath resolves the
// migrated row's now-canonical "project" scope to the same
// .../groves/<id>/<slug> path the legacy "grove" scope used to resolve to
// directly — the reason the migration must run before any reader resolves
// a stored scope, since ResourceStoragePath has no arm for "grove".
func TestNormalizeLegacyGroveScopes_MigratesAndIsIdempotent(t *testing.T) {
	dsn := "file:" + filepath.Join(t.TempDir(), "test.db")
	ctx := context.Background()

	cs := newTestCompositeStoreFromDSN(t, dsn)

	// First Migrate call: creates the schema. No legacy rows exist yet, so
	// NormalizeLegacyGroveScopes is a no-op here.
	require.NoError(t, cs.Migrate(ctx))

	db := cs.DB()
	require.NotNil(t, db)

	templateID := uuid.New().String()
	harnessConfigID := uuid.New().String()
	subscriptionTemplateID := uuid.New().String()
	projectID := uuid.New().String()

	// Seed one scope='grove' row per table directly with raw SQL — bypassing
	// the ent client and any request-level scope validation — to simulate
	// rows that predate this migration.
	_, err := db.ExecContext(ctx,
		`INSERT INTO templates (id, name, slug, harness, scope, scope_id, project_id, created, updated)
		 VALUES (?, 'legacy-template', 'legacy-template', 'antigravity', 'grove', ?, ?, '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z')`,
		templateID, projectID, projectID)
	require.NoError(t, err)

	_, err = db.ExecContext(ctx,
		`INSERT INTO harness_configs (id, name, slug, harness, scope, scope_id, created, updated)
		 VALUES (?, 'legacy-harness-config', 'legacy-harness-config', 'antigravity', 'grove', ?, '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z')`,
		harnessConfigID, projectID)
	require.NoError(t, err)

	_, err = db.ExecContext(ctx,
		`INSERT INTO subscription_templates (id, name, scope, trigger_activities, project_id, created_by)
		 VALUES (?, 'legacy-subscription-template', 'grove', '["COMPLETED"]', ?, 'test')`,
		subscriptionTemplateID, projectID)
	require.NoError(t, err)

	assertScope := func(t *testing.T, table, id, want string) {
		t.Helper()
		var got string
		require.NoError(t, db.QueryRowContext(ctx,
			"SELECT scope FROM "+table+" WHERE id = ?", id).Scan(&got))
		assert.Equal(t, want, got, "table %s row %s", table, id)
	}

	assertScope(t, "templates", templateID, "grove")
	assertScope(t, "harness_configs", harnessConfigID, "grove")
	assertScope(t, "subscription_templates", subscriptionTemplateID, "grove")

	// Second Migrate call: AutoMigrate is a no-op (schema unchanged), but
	// NormalizeLegacyGroveScopes now finds and rewrites the seeded rows.
	require.NoError(t, cs.Migrate(ctx))

	assertScope(t, "templates", templateID, store.TemplateScopeProject)
	assertScope(t, "harness_configs", harnessConfigID, store.HarnessConfigScopeProject)
	assertScope(t, "subscription_templates", subscriptionTemplateID, store.SubscriptionScopeProject)

	// Idempotent: a third call finds nothing left to rewrite and does not error.
	require.NoError(t, cs.Migrate(ctx))
	assertScope(t, "templates", templateID, store.TemplateScopeProject)
	assertScope(t, "harness_configs", harnessConfigID, store.HarnessConfigScopeProject)
	assertScope(t, "subscription_templates", subscriptionTemplateID, store.SubscriptionScopeProject)

	// The now-canonical "project" scope resolves to the same storage path
	// the legacy "grove" scope used to. Load the migrated row back and use
	// its actual Scope/ScopeID (rather than assuming what the migration
	// wrote) so this assertion would fail if the migration wrote something
	// other than "project". A reader that rebuilds the path from the stored
	// scope (e.g. pkg/hub/storage_migration.go, which runs after
	// CompositeStore.Migrate at hub boot) sees the correct, namespaced path
	// rather than the flattened default.
	migratedTemplate, err := cs.GetTemplate(ctx, templateID)
	require.NoError(t, err)
	got := storage.ResourceStoragePath("", storage.ResourceKindTemplate, migratedTemplate.Scope, migratedTemplate.ScopeID, "legacy-template")
	assert.Equal(t, "templates/groves/"+projectID+"/legacy-template", got)
}
