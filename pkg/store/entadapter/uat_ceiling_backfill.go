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
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	"github.com/GoogleCloudPlatform/scion/pkg/ent"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/useraccesstoken"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/permissions"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

const uatCeilingBackfillMarkerSection = "migration_uat_ceiling_backfill_v1"

// uatCeilingBackfillPageSize is a var, not a const, so a test can shrink it
// to exercise pagination across multiple pages without creating hundreds of
// rows.
var uatCeilingBackfillPageSize = 500

// BackfillUATCeilings persists a normalized permission ceiling for every
// existing user_access_tokens row that has never had one computed
// (ptone/scion#2118).
//
// A row in scope is identified by ceiling_permission_ids IS NULL — "never
// backfilled" — not by ceiling_version, and never by project_id (a later
// change makes project_id nullable for hub-boundary rows; every row minted
// under that scheme sets ceiling_permission_ids itself, so it is never a
// backfill target). PermissionIDs is computed via
// permissions.NormalizeLegacyUATScopes — a fixed table, never the live,
// mutable permissions.ResolveSelector — from each row's existing Scopes
// column, which is left untouched, as are ID, KeyHash, Prefix, ExpiresAt,
// and Revoked. Idempotent via a HubSetting completion marker, the same
// pattern as BackfillDelegationEdges; safe to run on every startup.
func (c *CompositeStore) BackfillUATCeilings(ctx context.Context) error {
	if _, err := c.GetHubSetting(ctx, uatCeilingBackfillMarkerSection); err == nil {
		return nil
	} else if !errors.Is(err, store.ErrNotFound) {
		return err
	}

	var lastID *ent.UserAccessToken
	var updated int

	for {
		q := c.client.UserAccessToken.Query().
			Where(useraccesstoken.CeilingPermissionIdsIsNil()).
			Order(ent.Asc(useraccesstoken.FieldID)).
			Limit(uatCeilingBackfillPageSize)
		if lastID != nil {
			q = q.Where(useraccesstoken.IDGT(lastID.ID))
		}
		rows, err := q.All(ctx)
		if err != nil {
			return fmt.Errorf("query legacy user access tokens for ceiling backfill: %w", err)
		}
		if len(rows) == 0 {
			break
		}

		for _, row := range rows {
			var scopes []string
			if row.Scopes != "" {
				if err := json.Unmarshal([]byte(row.Scopes), &scopes); err != nil {
					slog.Warn("user access token ceiling backfill: scopes column is not valid JSON, treating as no scopes",
						"token_id", row.ID, "error", err)
					scopes = nil
				}
			}
			// NormalizeLegacyUATScopes always returns a non-nil slice, so the
			// persisted value is always non-nil too: a backfilled row is
			// never left looking "never backfilled" (NULL) again, even when
			// it resolves to zero permissions.
			ids := permissions.NormalizeLegacyUATScopes(scopes)
			persisted := marshalCeilingPermissionIDs(ids)
			if err := c.client.UserAccessToken.UpdateOneID(row.ID).
				SetCeilingVersion(int32(permissions.CeilingVersionUnspecified)).
				SetCeilingPermissionIds(*persisted).
				Exec(ctx); err != nil {
				return fmt.Errorf("backfill ceiling for user access token %s: %w", row.ID, err)
			}
			updated++
		}

		lastID = rows[len(rows)-1]
		if len(rows) < uatCeilingBackfillPageSize {
			break
		}
	}

	if updated > 0 {
		slog.Info("backfilled user access token permission ceilings", "rows_updated", updated)
	}

	_, err := c.UpsertHubSetting(ctx, uatCeilingBackfillMarkerSection,
		json.RawMessage(`{"schema_version":1,"completed":true}`), "migration", 0, "seeded")
	if errors.Is(err, store.ErrRevisionConflict) {
		return nil
	}
	return err
}
