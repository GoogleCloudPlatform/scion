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

	"github.com/GoogleCloudPlatform/scion/pkg/ent"
)

// PreMigrate runs the data fixes that must happen before the Ent schema
// migration (entc.AutoMigrate) can succeed on an existing hub database, in
// this order:
//
//  1. access_policies rows with a NULL scope_id get an empty scope_id.
//  2. Duplicate access_policies rows per (name, scope_type, scope_id) are
//     removed, keeping the oldest.
//  3. Duplicate active delegation_edges rows per (delegate_type, delegate_id,
//     scope_type, scope_id) are removed, keeping the oldest.
//  4. Duplicate agent_session_metrics rows per (agent_id, session_id,
//     started_at) are removed, keeping the first stored.
//
// Without them, creating the unique indexes on those tables fails on a
// database that already holds such rows. Every command that runs
// entc.AutoMigrate against a hub database must call PreMigrate first;
// CompositeStore.Migrate does so. Each step is idempotent and a no-op on a
// fresh database where the tables do not exist yet.
//
// Keep the order: see the comment on the scope_id backfill.
func PreMigrate(ctx context.Context, client *ent.Client) error {
	// Backfill null scope_id to empty string before dedup and schema migration.
	// Must run BEFORE dedup: in SQL NULL != NULL, so dedup won't detect
	// duplicate rows where scope_id IS NULL. Converting to '' first lets
	// dedup catch all real duplicates. Also prevents SQLSTATE 23502 when
	// the schema migration applies the NOT NULL constraint.
	if db := clientDB(client); db != nil {
		exists, err := accessPoliciesTableExists(ctx, client, db)
		if err != nil {
			return fmt.Errorf("pre-migration null scope_id check: %w", err)
		}
		if exists {
			result, err := db.ExecContext(ctx,
				"UPDATE access_policies SET scope_id = '' WHERE scope_id IS NULL")
			if err != nil {
				return fmt.Errorf("pre-migration null scope_id backfill: %w", err)
			}
			if n, _ := result.RowsAffected(); n > 0 {
				slog.Info("backfilled null scope_id before migration", "rows_updated", n)
			}
		}
	}

	// Deduplicate access_policies before migration adds a unique index.
	// Existing databases may have duplicate (name, scope_type, scope_id) rows
	// (including former NULL scope_id rows now normalized to '') which would
	// cause the UNIQUE constraint migration to fail.
	if err := deduplicateAccessPolicies(ctx, client); err != nil {
		return fmt.Errorf("pre-migration dedup: %w", err)
	}

	// Deduplicate delegation_edges before migration adds a partial unique index.
	// Existing databases that ran the initial backfill and were interrupted may
	// have duplicate active edges that would violate the new constraint.
	if err := deduplicateDelegationEdges(ctx, client); err != nil {
		return fmt.Errorf("pre-migration delegation edge dedup: %w", err)
	}

	// Deduplicate agent_session_metrics before migration adds the unique
	// (agent_id, session_id, started_at) index. Before it, a repeated
	// report of a session segment was stored again.
	if err := deduplicateAgentSessionMetrics(ctx, client); err != nil {
		return fmt.Errorf("pre-migration agent session metrics dedup: %w", err)
	}

	return nil
}
