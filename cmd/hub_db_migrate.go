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

package cmd

import (
	"context"
	"fmt"

	"github.com/GoogleCloudPlatform/scion/pkg/ent"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/entc"
	"github.com/GoogleCloudPlatform/scion/pkg/store/entadapter"
)

// This file holds the only places in package cmd, apart from hub server
// startup (initStore in server_foreground.go), that migrate the hub
// database. Each one runs the PostgreSQL search_path shadow guard first,
// with the same server.database.allow_shadowed_schema override as server
// startup (ptone/scion#4432). TestHubMigrationCallsUseSharedHelpers fails
// when another file in package cmd calls a migrate entry point directly.

// migrateHubStore runs cs.Migrate with the search_path shadow guard turned
// on, as a maintenance command: a refusal says it refuses to migrate rather
// than to start. allowShadowed is server.database.allow_shadowed_schema.
func migrateHubStore(ctx context.Context, cs *entadapter.CompositeStore, allowShadowed bool) error {
	cs.EnableMaintenanceSchemaShadowGuard(allowShadowed)
	return cs.Migrate(ctx)
}

// migrateHubSchema runs the search_path shadow guard, then (when preMigrate
// is set) entadapter.PreMigrate, then entc.AutoMigrate on client. It is for
// the commands that migrate a raw client rather than a CompositeStore.
// allowShadowed is server.database.allow_shadowed_schema. A guard refusal is
// returned unwrapped, as a *entadapter.SchemaShadowError with Maintenance
// set. The guard only reads the PostgreSQL catalog, so a refusal leaves the
// database unchanged, including in a dry run.
func migrateHubSchema(ctx context.Context, client *ent.Client, allowShadowed, preMigrate bool) error {
	if err := entadapter.CheckSchemaShadowing(ctx, client, allowShadowed, nil); err != nil {
		return err
	}
	if preMigrate {
		if err := entadapter.PreMigrate(ctx, client); err != nil {
			return fmt.Errorf("pre-migration steps: %w", err)
		}
	}
	if err := entc.AutoMigrate(ctx, client); err != nil {
		return fmt.Errorf("auto-migrate: %w", err)
	}
	return nil
}
