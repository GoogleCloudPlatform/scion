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

package entc

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// ProjectSlugResolver can look up a project by slug.
type ProjectSlugResolver interface {
	GetProjectBySlug(ctx context.Context, slug string) (*store.Project, error)
}

// MigrateGroveToProjectData performs an idempotent data migration on the Ent
// database, fixing group records that still reference the old "grove" naming.
// It requires the main database store to resolve project IDs by slug.
//
// The migration:
//  1. Merges duplicate groups where both "grove:<slug>:X" and "project:<slug>:X" exist.
//  2. Renames remaining "grove:" slugs to "project:".
//  3. Fixes group_type values from old enum variants to current ones.
//  4. Backfills project_id for groups that are missing it.
func MigrateGroveToProjectData(ctx context.Context, entDSN string, projectStore ProjectSlugResolver) error {
	return migrateGroveToProjectDataWithDSN(ctx, "file:"+entDSN+"?cache=shared", projectStore)
}

// migrateGroveToProjectDataWithDSN is the internal implementation that accepts
// a fully-formed SQLite DSN. This enables testing with in-memory databases.
func migrateGroveToProjectDataWithDSN(ctx context.Context, sqliteDSN string, projectStore ProjectSlugResolver) error {
	db, err := sql.Open("sqlite", sqliteDSN)
	if err != nil {
		return fmt.Errorf("opening ent database for data migration: %w", err)
	}
	defer db.Close()

	// Enable foreign keys to match Ent's connection settings.
	if _, err := db.Exec("PRAGMA foreign_keys = ON"); err != nil {
		return fmt.Errorf("enabling foreign keys: %w", err)
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("beginning migration transaction: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	// Step 1: Handle duplicate groups (both "grove:" and "project:" versions exist).
	merged, err := mergeDuplicateGroups(ctx, tx)
	if err != nil {
		return fmt.Errorf("merging duplicate groups: %w", err)
	}

	// Step 2: Rename remaining "grove:" slugs to "project:".
	slugsFixed, err := fixGroupSlugs(ctx, tx)
	if err != nil {
		return fmt.Errorf("fixing group slugs: %w", err)
	}

	// Step 3: Fix group_type enum values.
	typesFixed, err := fixGroupTypes(ctx, tx)
	if err != nil {
		return fmt.Errorf("fixing group types: %w", err)
	}

	// Step 4: Backfill project_id from the main database.
	projectsBackfilled, err := backfillProjectIDs(ctx, tx, projectStore)
	if err != nil {
		return fmt.Errorf("backfilling project IDs: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("committing migration transaction: %w", err)
	}

	if merged+slugsFixed+typesFixed+projectsBackfilled > 0 {
		log.Printf("grove→project data migration: merged=%d slugs_fixed=%d types_fixed=%d project_ids_backfilled=%d",
			merged, slugsFixed, typesFixed, projectsBackfilled)
	}

	return nil
}

// mergeDuplicateGroups finds groups where both "grove:<slug>:X" and
// "project:<slug>:X" exist, merges memberships from the old group into the new
// one, and deletes the old group.
func mergeDuplicateGroups(ctx context.Context, tx *sql.Tx) (int, error) {
	// Find all grove: groups that have a corresponding project: group.
	rows, err := tx.QueryContext(ctx, `
		SELECT old.id, old.slug, new.id
		FROM groups old
		JOIN groups new ON new.slug = REPLACE(old.slug, 'grove:', 'project:')
		WHERE old.slug LIKE 'grove:%'
	`)
	if err != nil {
		return 0, fmt.Errorf("querying duplicate groups: %w", err)
	}
	defer rows.Close()

	type duplicate struct {
		oldID   string
		oldSlug string
		newID   string
	}
	var dups []duplicate
	for rows.Next() {
		var d duplicate
		if err := rows.Scan(&d.oldID, &d.oldSlug, &d.newID); err != nil {
			return 0, fmt.Errorf("scanning duplicate group: %w", err)
		}
		dups = append(dups, d)
	}
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("iterating duplicate groups: %w", err)
	}

	for _, d := range dups {
		// Move memberships from old group to new group, skipping any that
		// already exist in the target group (matched by user_id or agent_id).
		_, err := tx.ExecContext(ctx, `
			INSERT OR IGNORE INTO group_memberships (id, role, added_by, added_at, group_id, user_id, agent_id)
			SELECT lower(hex(randomblob(4)) || '-' || hex(randomblob(2)) || '-4' || substr(hex(randomblob(2)),2) || '-' || substr('89ab', abs(random()) % 4 + 1, 1) || substr(hex(randomblob(2)),2) || '-' || hex(randomblob(6))),
			       role, added_by, added_at, ?, user_id, agent_id
			FROM group_memberships
			WHERE group_id = ?
		`, d.newID, d.oldID)
		if err != nil {
			return 0, fmt.Errorf("migrating memberships from group %s: %w", d.oldSlug, err)
		}

		// Delete old memberships.
		if _, err := tx.ExecContext(ctx, `DELETE FROM group_memberships WHERE group_id = ?`, d.oldID); err != nil {
			return 0, fmt.Errorf("deleting old memberships for group %s: %w", d.oldSlug, err)
		}

		// Delete the old group.
		if _, err := tx.ExecContext(ctx, `DELETE FROM groups WHERE id = ?`, d.oldID); err != nil {
			return 0, fmt.Errorf("deleting old group %s: %w", d.oldSlug, err)
		}
	}

	return len(dups), nil
}

// fixGroupSlugs renames remaining "grove:" prefixed slugs to "project:".
func fixGroupSlugs(ctx context.Context, tx *sql.Tx) (int, error) {
	result, err := tx.ExecContext(ctx, `
		UPDATE groups SET slug = REPLACE(slug, 'grove:', 'project:') WHERE slug LIKE 'grove:%'
	`)
	if err != nil {
		return 0, err
	}
	n, _ := result.RowsAffected()
	return int(n), nil
}

// fixGroupTypes updates old group_type enum values to the current Ent enum values.
func fixGroupTypes(ctx context.Context, tx *sql.Tx) (int, error) {
	var total int64

	result, err := tx.ExecContext(ctx, `
		UPDATE groups SET group_type = 'project_agents' WHERE group_type = 'grove_agents'
	`)
	if err != nil {
		return 0, err
	}
	n, _ := result.RowsAffected()
	total += n

	result, err = tx.ExecContext(ctx, `
		UPDATE groups SET group_type = 'explicit' WHERE group_type = 'grove_members'
	`)
	if err != nil {
		return 0, err
	}
	n, _ = result.RowsAffected()
	total += n

	return int(total), nil
}

// backfillProjectIDs sets project_id for groups that are missing it.
// It parses the project slug from the group slug ("project:<slug>:agents" or
// "project:<slug>:members") and looks it up in the main database.
func backfillProjectIDs(ctx context.Context, tx *sql.Tx, projectStore ProjectSlugResolver) (int, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT id, slug FROM groups
		WHERE project_id IS NULL
		AND (slug LIKE 'project:%:agents' OR slug LIKE 'project:%:members')
	`)
	if err != nil {
		return 0, fmt.Errorf("querying groups for project_id backfill: %w", err)
	}
	defer rows.Close()

	type groupRecord struct {
		id   string
		slug string
	}
	var records []groupRecord
	for rows.Next() {
		var r groupRecord
		if err := rows.Scan(&r.id, &r.slug); err != nil {
			return 0, fmt.Errorf("scanning group for backfill: %w", err)
		}
		records = append(records, r)
	}
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("iterating groups for backfill: %w", err)
	}

	var updated int
	for _, r := range records {
		projectSlug := parseProjectSlug(r.slug)
		if projectSlug == "" {
			log.Printf("grove→project migration: skipping group %s with unparseable slug %q", r.id, r.slug)
			continue
		}

		project, err := projectStore.GetProjectBySlug(ctx, projectSlug)
		if err != nil {
			log.Printf("grove→project migration: skipping group %s: project slug %q not found: %v", r.id, projectSlug, err)
			continue
		}

		_, err = tx.ExecContext(ctx, `UPDATE groups SET project_id = ? WHERE id = ?`, project.ID, r.id)
		if err != nil {
			return updated, fmt.Errorf("updating project_id for group %s: %w", r.id, err)
		}
		updated++
	}

	return updated, nil
}

// parseProjectSlug extracts the project slug from a group slug.
// Group slugs follow the pattern "project:<slug>:agents" or "project:<slug>:members".
func parseProjectSlug(groupSlug string) string {
	parts := strings.Split(groupSlug, ":")
	if len(parts) != 3 {
		return ""
	}
	return parts[1]
}
