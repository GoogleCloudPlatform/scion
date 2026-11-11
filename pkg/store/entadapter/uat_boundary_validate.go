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
)

// defaultUATBoundaryValidatePageSize is used whenever a CompositeStore's
// uatBoundaryValidatePageSize field is left at its zero value.
const defaultUATBoundaryValidatePageSize = 500

func (c *CompositeStore) uatBoundaryValidatePageSizeOrDefault() int {
	if c.uatBoundaryValidatePageSize > 0 {
		return c.uatBoundaryValidatePageSize
	}
	return defaultUATBoundaryValidatePageSize
}

func (c *CompositeStore) uatBoundaryLoggerOrDefault() *slog.Logger {
	if c.uatBoundaryLogger != nil {
		return c.uatBoundaryLogger
	}
	return slog.Default()
}

// ValidateUserAccessTokenBoundaries reports user access token rows that
// violate the boundary_kind/project_id invariant after schema migration. It
// only reads: it never inserts, updates or deletes a row, so it is
// idempotent and safe to run on every startup.
//
// Every row is checked with store.UserAccessToken.ValidateBoundary. The IDs
// of rows that fail are logged at Error; the IDs are server-issued record
// identifiers, never key material or project IDs. Such a row is left as
// stored and does not fail boot: UserAccessTokenService.ValidateToken
// rejects it at load, so it never authenticates while every valid token
// keeps authenticating.
//
// A row whose stored project_id cannot be scanned as a UUID at all (only a
// hand-edited SQLite row can hold one; Postgres's uuid column and the API
// cannot) fails the query, and this function returns that error, which
// fails Migrate. In practice BackfillUATCeilings, which Migrate runs ahead
// of this step, scans the same column and fails Migrate on such a row.
// The remediation is to correct or delete the row.
func (c *CompositeStore) ValidateUserAccessTokenBoundaries(ctx context.Context) error {
	pageSize := c.uatBoundaryValidatePageSizeOrDefault()
	var lastID *ent.UserAccessToken
	var invalidIDs []string

	for {
		q := c.client.UserAccessToken.Query().
			Order(ent.Asc(useraccesstoken.FieldID)).
			Limit(pageSize)
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
		if len(rows) < pageSize {
			break
		}
	}

	if len(invalidIDs) > 0 {
		c.uatBoundaryLoggerOrDefault().Error("user access tokens with an invalid boundary were found; they are rejected at load, not repaired or deleted",
			"count", len(invalidIDs), "token_ids", invalidIDs)
	}
	return nil
}
