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
	"fmt"
	"log/slog"

	"github.com/GoogleCloudPlatform/scion/pkg/ent/harnessconfig"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/subscriptiontemplate"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/template"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// legacyGroveScope is the pre-rename scope value. The May 2026 grove→project
// rename's schema migration (sqlite migrationV48) rewrote scope="grove" to
// "project" in env_vars, secrets, policies, gcp_service_accounts,
// notification_subscriptions and subscription_templates, but never touched
// templates or harness_configs — the storage-path switch kept accepting
// "grove" as an input alongside "project" specifically to avoid needing that
// data migration. subscription_templates is included here too because,
// before its create handler validated scope, it could store fresh "grove"
// rows after V48 ran.
const legacyGroveScope = "grove"

// NormalizeLegacyGroveScopes rewrites any remaining scope="grove" rows in
// templates, harness_configs and subscription_templates to scope="project".
//
// This must run before any code path resolves a stored scope into a storage
// path (pkg/storage.ResourceStoragePath's scope-input switch has no "grove"
// arm) — otherwise those rows silently resolve into the wrong (flattened,
// unscoped) path the next time they are read, which can collide with
// another project's slug and lose data. It runs from CompositeStore.Migrate,
// immediately after entc.AutoMigrate and before
// pkg/hub/storage_migration.go's namespacing migration walks the store, so
// no reader ever observes a stored "grove" scope after Migrate returns.
//
// Idempotent: each Update is a no-op once no row matches scope="grove", so
// no completion marker is needed. Safe to run on every boot, including
// concurrently across replicas — on Postgres it runs under the same
// schema-migration advisory lock as the other Migrate backfills.
func (c *CompositeStore) NormalizeLegacyGroveScopes(ctx context.Context) error {
	templatesUpdated, err := c.client.Template.Update().
		Where(template.ScopeEQ(legacyGroveScope)).
		SetScope(store.TemplateScopeProject).
		Save(ctx)
	if err != nil {
		return fmt.Errorf("normalize legacy grove scope on templates: %w", err)
	}
	if templatesUpdated > 0 {
		slog.Info("normalized legacy grove scope", "table", "templates", "rows_updated", templatesUpdated)
	}

	harnessConfigsUpdated, err := c.client.HarnessConfig.Update().
		Where(harnessconfig.ScopeEQ(legacyGroveScope)).
		SetScope(store.HarnessConfigScopeProject).
		Save(ctx)
	if err != nil {
		return fmt.Errorf("normalize legacy grove scope on harness_configs: %w", err)
	}
	if harnessConfigsUpdated > 0 {
		slog.Info("normalized legacy grove scope", "table", "harness_configs", "rows_updated", harnessConfigsUpdated)
	}

	subscriptionTemplatesUpdated, err := c.client.SubscriptionTemplate.Update().
		Where(subscriptiontemplate.ScopeEQ(legacyGroveScope)).
		SetScope(store.SubscriptionScopeProject).
		Save(ctx)
	if err != nil {
		return fmt.Errorf("normalize legacy grove scope on subscription_templates: %w", err)
	}
	if subscriptionTemplatesUpdated > 0 {
		slog.Info("normalized legacy grove scope", "table", "subscription_templates", "rows_updated", subscriptionTemplatesUpdated)
	}

	return nil
}
