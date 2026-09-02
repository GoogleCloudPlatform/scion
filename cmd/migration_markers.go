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
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// migrationsSectionName is the hub_settings section used to track data
// migration state. The underscore prefix marks it as internal, following
// the _meta sentinel precedent (cmd/server_foreground.go:2078).
const migrationsSectionName = "_migrations"

// migrationsDoc is the JSON structure persisted in the _migrations section.
// Each field is a pointer so that absent keys unmarshal as nil rather than
// zero-value structs, letting IsMigrationComplete distinguish "never run"
// from "run with a zero-time completion."
type migrationsDoc struct {
	DMKeyMigration  *migrationMarker `json:"dm_key_migration,omitempty"`
	MessageBackfill *migrationMarker `json:"message_backfill,omitempty"`
}

// migrationMarker records the completion state of a single migration.
//
// A marker with a non-nil CompletedAt means the migration's full pass
// completed without a run-level failure. Row-level refusals (deterministic,
// non-retryable per-row outcomes) are counted in Residuals but do not
// prevent marker creation — M-1' distinguishes "the pass did not happen"
// from "the pass happened and some rows are permanent non-participants."
type migrationMarker struct {
	CompletedAt  *time.Time `json:"completed_at"`            // nil => not yet complete
	Residuals    int        `json:"residuals,omitempty"`      // row-level refusals (permanent, non-retryable)
	ProjectsDone []string   `json:"projects_done,omitempty"` // backfill only: per-project progress
}

// IsMigrationComplete returns true if the named migration has a completion
// marker with a non-nil CompletedAt timestamp. A missing _migrations section,
// a malformed document, or a missing/null CompletedAt are all treated as
// "not complete" — which means the migration will be retried, and that is
// always the safe direction.
func IsMigrationComplete(ctx context.Context, s store.Store, name string) (bool, error) {
	doc, err := loadMigrationsDoc(ctx, s)
	if err != nil {
		return false, err
	}
	if doc == nil {
		return false, nil
	}

	marker := doc.markerFor(name)
	return marker != nil && marker.CompletedAt != nil, nil
}

// MarkMigrationComplete records that the named migration's full pass completed.
//
// This helper is intentionally generic: it records a completion timestamp and
// an associated residual count without making the write-or-not policy decision.
// The caller is responsible for deciding whether to call this function —
// typically: do not call on a run-level error (context cancelled, store
// unavailable), do call even when there are row-level refusals (which are
// deterministic and non-retryable). The residuals parameter records how many
// rows were refused, for diagnostic reporting.
//
// The write is an unconditional upsert (expectedRevision = -1), which is
// conflict-safe on its own merits. This matters because the advisory lock
// is a no-op on SQLite (design F5), so the marker write must not depend on
// the lock for correctness.
func MarkMigrationComplete(ctx context.Context, s store.Store, name string, residuals int) error {
	doc, err := loadMigrationsDoc(ctx, s)
	if err != nil {
		return fmt.Errorf("loading migrations doc: %w", err)
	}
	if doc == nil {
		doc = &migrationsDoc{}
	}

	now := time.Now().UTC()
	marker := &migrationMarker{
		CompletedAt: &now,
		Residuals:   residuals,
	}
	doc.setMarker(name, marker)

	return persistMigrationsDoc(ctx, s, doc)
}

// loadMigrationsDoc reads and unmarshals the _migrations section from
// hub_settings. Returns (nil, nil) if the section does not exist.
// Returns (nil, nil) if the section exists but is malformed — treating
// corruption as "not complete" (retry) rather than as a hard error is the
// safe direction.
func loadMigrationsDoc(ctx context.Context, s store.Store) (*migrationsDoc, error) {
	hs, err := s.GetHubSetting(ctx, migrationsSectionName)
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", migrationsSectionName, err)
	}

	var doc migrationsDoc
	if err := json.Unmarshal(hs.Value, &doc); err != nil {
		// Malformed document: treat as absent (safe direction is retry).
		return nil, nil
	}
	return &doc, nil
}

// persistMigrationsDoc marshals and upserts the _migrations section.
// Uses expectedRevision = -1 (unconditional upsert) for conflict safety.
func persistMigrationsDoc(ctx context.Context, s store.Store, doc *migrationsDoc) error {
	raw, err := json.Marshal(doc)
	if err != nil {
		return fmt.Errorf("marshaling migrations doc: %w", err)
	}

	if _, err := s.UpsertHubSetting(ctx, migrationsSectionName, raw, "system", -1, "seeded"); err != nil {
		return fmt.Errorf("upserting %s: %w", migrationsSectionName, err)
	}
	return nil
}

// markerFor returns the marker for the named migration, or nil if absent.
func (d *migrationsDoc) markerFor(name string) *migrationMarker {
	switch name {
	case "dm_key_migration":
		return d.DMKeyMigration
	case "message_backfill":
		return d.MessageBackfill
	default:
		return nil
	}
}

// setMarker sets the marker for the named migration.
func (d *migrationsDoc) setMarker(name string, m *migrationMarker) {
	switch name {
	case "dm_key_migration":
		d.DMKeyMigration = m
	case "message_backfill":
		d.MessageBackfill = m
	}
}
