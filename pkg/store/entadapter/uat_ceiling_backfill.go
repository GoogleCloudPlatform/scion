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

// defaultUATCeilingBackfillPageSize is used whenever a CompositeStore's
// uatCeilingBackfillPageSize field is left at its zero value. The page size
// lives on the store instance, not a package-level variable, so a test that
// shrinks it to exercise pagination only affects its own store and cannot
// race with any other test's migration running concurrently.
const defaultUATCeilingBackfillPageSize = 500

// uatCeilingBackfillPageSizeOrDefault resolves the effective page size for
// BackfillUATCeilings: the store's configured value when positive, otherwise
// defaultUATCeilingBackfillPageSize. Extracted into its own method so the
// resolution itself — including the <= 0 fallback — is directly testable,
// independent of any particular row count exercising the backfill's
// pagination loop (a small row count processed in a single page cannot
// distinguish a shrunk field from the default being used regardless).
func (c *CompositeStore) uatCeilingBackfillPageSizeOrDefault() int {
	if c.uatCeilingBackfillPageSize > 0 {
		return c.uatCeilingBackfillPageSize
	}
	return defaultUATCeilingBackfillPageSize
}

// BackfillUATCeilings persists a normalized permission ceiling for every
// existing user_access_tokens row in scope: ceiling_version = 0
// (unversioned) AND ceiling_permission_ids IS NULL — "never backfilled".
// Rows are never selected by project_id (a later change makes project_id
// nullable for hub-boundary rows; every row minted under that scheme sets
// ceiling_permission_ids itself, so it is never a backfill target). A row
// with any other version and no permission list is malformed and is left
// as is; it denies both before and after this migration runs, since
// NormalizedCeiling only interprets version 0 through the legacy snapshot.
// PermissionIDs is computed via permissions.NormalizeLegacyUATScopes — a
// fixed table, never the live, mutable permissions.ResolveSelector — from
// each row's existing Scopes column, which is left untouched, as are ID,
// KeyHash, Prefix, ExpiresAt, and Revoked. Idempotent via a HubSetting
// completion marker, the same pattern as BackfillDelegationEdges; safe to
// run on every startup.
func (c *CompositeStore) BackfillUATCeilings(ctx context.Context) error {
	if _, err := c.GetHubSetting(ctx, uatCeilingBackfillMarkerSection); err == nil {
		return nil
	} else if !errors.Is(err, store.ErrNotFound) {
		return err
	}

	pageSize := c.uatCeilingBackfillPageSizeOrDefault()

	var lastID *ent.UserAccessToken
	var updated int

	for {
		q := c.client.UserAccessToken.Query().
			Where(
				useraccesstoken.CeilingPermissionIdsIsNil(),
				useraccesstoken.CeilingVersionEQ(int32(permissions.CeilingVersionUnspecified)),
			).
			Order(ent.Asc(useraccesstoken.FieldID)).
			Limit(pageSize)
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
			ids := permissions.NormalizeLegacyUATScopes(scopes)
			value, ok := persistedCeilingColumnValue(ids)
			if !ok {
				slog.Error("user access token ceiling backfill: normalization returned no permission list (nil), skipping row",
					"token_id", row.ID)
				continue
			}
			if err := c.client.UserAccessToken.UpdateOneID(row.ID).
				SetCeilingVersion(int32(permissions.CeilingVersionUnspecified)).
				SetCeilingPermissionIds(value).
				Exec(ctx); err != nil {
				return fmt.Errorf("backfill ceiling for user access token %s: %w", row.ID, err)
			}
			updated++
		}

		lastID = rows[len(rows)-1]
		if len(rows) < pageSize {
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

// persistedCeilingColumnValue converts a computed permission-ID list into
// the ceiling_permission_ids column value BackfillUATCeilings persists. ok
// is false when ids is nil: permissions.NormalizeLegacyUATScopes is
// documented to always return a non-nil slice, so the only way ids is nil
// here is that invariant being violated by a future change. Defaulting to
// the literal "[]" in that case would mark the row as an intentionally
// issued, permission-less ceiling — a different, stronger claim than "not
// yet resolved" — and would silently paper over the violation forever
// instead of surfacing it. Returning ok == false instead lets the caller
// skip the row (the same fail-closed choice BackfillDelegationEdges makes
// for its own malformed input) rather than dereference a nil pointer:
// NormalizedCeiling denies a row left NULL at load time regardless, via the
// identical nil-safe computation on a slice field, which cannot panic the
// way marshalCeilingPermissionIDs's pointer result could.
func persistedCeilingColumnValue(ids []string) (value string, ok bool) {
	persisted := marshalCeilingPermissionIDs(ids)
	if persisted == nil {
		return "", false
	}
	return *persisted, true
}
