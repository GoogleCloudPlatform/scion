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
	"github.com/GoogleCloudPlatform/scion/pkg/ent/useraccesstoken"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/permissions"
)

// uatBoundaryValidatePageSize is a var, not a const, so a test can shrink it
// to exercise pagination across multiple pages without creating hundreds of
// rows.
var uatBoundaryValidatePageSize = 500

// ValidateUserAccessTokenBoundaries defends and reports on the
// boundary_kind/project_id invariant after schema migration. It is
// idempotent and safe to run on every startup, and it never writes "hub":
//
//  1. It repairs any row left with an empty boundary_kind (a partially
//     applied migration, or a row written through a path that skipped the
//     column's Go-level NotEmpty validation) to "project" — the same value
//     the column's own schema default would have applied. This is
//     defensive, not the primary mechanism: entc.AutoMigrate's ADD COLUMN
//     already backfills every pre-existing row via that default.
//  2. It then counts rows that still violate the kind/project-id invariant
//     (via store.UserAccessToken.ValidateBoundary) and logs their sanitized
//     IDs at Error. It does NOT auto-repair or delete these rows, and it
//     does not fail boot: a row that fails ValidateBoundary is rejected at
//     load by UserAccessTokenService.ValidateToken (fails closed per token),
//     which keeps every other valid token working instead of bricking the
//     hub or silently rewriting stored authority.
func (c *CompositeStore) ValidateUserAccessTokenBoundaries(ctx context.Context) error {
	repaired, err := c.client.UserAccessToken.Update().
		Where(useraccesstoken.BoundaryKindEQ("")).
		SetBoundaryKind(string(permissions.BoundaryKindProject)).
		Save(ctx)
	if err != nil {
		return fmt.Errorf("repair empty user access token boundary kind: %w", err)
	}
	if repaired > 0 {
		slog.Warn("repaired empty user access token boundary_kind to project", "rows_updated", repaired)
	}

	var lastID *ent.UserAccessToken
	var invalidIDs []string

	for {
		q := c.client.UserAccessToken.Query().
			Order(ent.Asc(useraccesstoken.FieldID)).
			Limit(uatBoundaryValidatePageSize)
		if lastID != nil {
			q = q.Where(useraccesstoken.IDGT(lastID.ID))
		}
		rows, err := q.All(ctx)
		if err != nil {
			return fmt.Errorf("query user access tokens for boundary validation: %w", err)
		}
		if len(rows) == 0 {
			break
		}
		for _, row := range rows {
			t := entUATToStore(row)
			if verr := t.ValidateBoundary(); verr != nil {
				invalidIDs = append(invalidIDs, row.ID.String())
			}
		}
		lastID = rows[len(rows)-1]
		if len(rows) < uatBoundaryValidatePageSize {
			break
		}
	}

	if len(invalidIDs) > 0 {
		slog.Error("user access tokens with an invalid boundary were found; they are rejected at load, not repaired or deleted",
			"count", len(invalidIDs), "token_ids", invalidIDs)
	}
	return nil
}
