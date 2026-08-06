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
	"log/slog"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/ent/allowlistentry"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/user"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
)

// MigrateAllowListToInvitedUsers converts existing AllowListEntry records to
// User records with status="invited". For each AllowListEntry:
//   - If a User with that email already exists: optionally backfill invited_by
//     from the AllowListEntry.AddedBy field.
//   - If no User exists: create User(status=invited, email=entry.Email,
//     invited_by=entry.AddedBy).
//
// The migration is idempotent: it can be run multiple times safely. It does NOT
// delete AllowListEntry records — those are retained for rollback safety and
// will be removed in Phase 4 (cleanup).
func (c *CompositeStore) MigrateAllowListToInvitedUsers(ctx context.Context) error {
	// Load all allow list entries.
	entries, err := c.client.AllowListEntry.Query().
		Order(allowlistentry.ByCreated()).
		All(ctx)
	if err != nil {
		return err
	}

	if len(entries) == 0 {
		slog.Info("allowlist→invited migration: no AllowListEntry records found, nothing to migrate")
		return nil
	}

	var created, backfilled, skipped int

	for _, entry := range entries {
		email := normalizeEmail(entry.Email)
		if strings.TrimSpace(email) == "" {
			skipped++
			continue
		}

		// Check if a User with this email already exists.
		existing, err := c.client.User.Query().
			Where(user.EmailEqualFold(email)).
			Only(ctx)
		if err == nil {
			// User exists — optionally backfill invited_by / invite_note if not already set.
			needsBackfill := false
			updateOp := c.client.User.UpdateOneID(existing.ID)
			if existing.InvitedBy == nil && entry.AddedBy != "" {
				updateOp.SetInvitedBy(entry.AddedBy)
				needsBackfill = true
			}
			if existing.InviteNote == nil && entry.Note != "" {
				updateOp.SetInviteNote(entry.Note)
				needsBackfill = true
			}
			if needsBackfill {
				if err := updateOp.Exec(ctx); err != nil {
					slog.Warn("allowlist→invited migration: failed to backfill user fields",
						"email", email, "error", err)
				} else {
					backfilled++
				}
			} else {
				skipped++
			}
			continue
		}

		// User does not exist — create with status=invited.
		uid := uuid.New()
		createdAt := entry.Created
		if createdAt.IsZero() {
			createdAt = time.Now()
		}

		createOp := c.client.User.Create().
			SetID(uid).
			SetEmail(email).
			SetDisplayName(""). // Will be populated on first login
			SetStatus(user.StatusInvited).
			SetRole(user.RoleMember).
			SetCreated(createdAt)

		if entry.AddedBy != "" {
			createOp.SetInvitedBy(entry.AddedBy)
		}
		if entry.Note != "" {
			createOp.SetInviteNote(entry.Note)
		}

		if err := createOp.Exec(ctx); err != nil {
			// If creation fails due to a race (user was created between our
			// check and now), log and continue — the migration is idempotent.
			slog.Warn("allowlist→invited migration: failed to create invited user",
				"email", email, "error", err)
			continue
		}
		created++
	}

	slog.Info("allowlist→invited migration complete",
		"total_entries", len(entries),
		"users_created", created,
		"users_backfilled", backfilled,
		"users_skipped", skipped,
	)
	return nil
}

// MigrateAllowListToInvitedUsersFromStore is a convenience wrapper that runs the
// migration against a store.Store. It type-asserts to CompositeStore internally.
// Returns nil if the store is not a CompositeStore (e.g., in tests using mocks).
func MigrateAllowListToInvitedUsersFromStore(ctx context.Context, st store.Store) error {
	cs, ok := st.(*CompositeStore)
	if !ok {
		return nil
	}
	return cs.MigrateAllowListToInvitedUsers(ctx)
}
