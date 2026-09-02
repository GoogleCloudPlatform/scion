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

// MigrationName is a typed constant for migration identifiers, so that a
// typo in a migration name is a compile error rather than a silent no-op
// that produces a permanent livelock (marker never written, migration
// retries every boot, making no progress).
type MigrationName string

const (
	MigrationDMKey    MigrationName = "dm_key_migration"
	MigrationBackfill MigrationName = "message_backfill"
)

// migrationMarker records the completion state of a single migration.
//
// A marker with a non-nil CompletedAt means the migration's full pass
// completed without a run-level failure. Row-level refusals (deterministic,
// non-retryable per-row outcomes) are counted in Residuals but do not
// prevent marker creation — M-1' distinguishes "the pass did not happen"
// from "the pass happened and some rows are permanent non-participants."
type migrationMarker struct {
	CompletedAt *time.Time `json:"completed_at"`       // nil => not yet complete
	Residuals   int        `json:"residuals,omitempty"` // row-level refusals (permanent, non-retryable)
}

// IsMigrationComplete returns true if the named migration has a completion
// marker with a non-nil CompletedAt timestamp. A missing _migrations section,
// a malformed document, or a missing/null CompletedAt are all treated as
// "not complete" — which means the migration will be retried, and that is
// always the safe direction.
func IsMigrationComplete(ctx context.Context, s store.Store, name MigrationName) (bool, error) {
	_, raw, err := loadMigrationsDoc(ctx, s)
	if err != nil {
		return false, err
	}
	if raw == nil {
		return false, nil
	}

	entry, ok := raw[string(name)]
	if !ok {
		return false, nil
	}

	var marker migrationMarker
	if err := json.Unmarshal(entry, &marker); err != nil {
		return false, nil // malformed entry: safe direction is retry
	}
	return marker.CompletedAt != nil, nil
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
// Returns ErrUnknownMigration if name is not a recognised MigrationName.
// This is a backstop; the typed constant should prevent this at compile time.
//
// The write is an unconditional upsert (expectedRevision = -1), which is
// conflict-safe on its own merits. This matters because the advisory lock
// is a no-op on SQLite (design F5), so the marker write must not depend on
// the lock for correctness.
//
// Unknown keys in the persisted document are preserved across read-modify-write
// cycles. A newer binary may write markers that an older binary does not know
// about; the older binary must not silently delete them.
func MarkMigrationComplete(ctx context.Context, s store.Store, name MigrationName, residuals int) error {
	if !isKnownMigration(name) {
		return fmt.Errorf("%w: %q", ErrUnknownMigration, name)
	}

	_, raw, err := loadMigrationsDoc(ctx, s)
	if err != nil {
		return fmt.Errorf("loading migrations doc: %w", err)
	}
	if raw == nil {
		raw = make(map[string]json.RawMessage)
	}

	now := time.Now().UTC()
	marker := &migrationMarker{
		CompletedAt: &now,
		Residuals:   residuals,
	}

	markerJSON, err := json.Marshal(marker)
	if err != nil {
		return fmt.Errorf("marshaling marker for %s: %w", name, err)
	}
	raw[string(name)] = markerJSON

	return persistMigrationsDoc(ctx, s, raw)
}

// ErrUnknownMigration is returned by MarkMigrationComplete when the caller
// passes an unrecognised MigrationName. This is a backstop for the typed
// constant; a typo should be caught at compile time.
var ErrUnknownMigration = errors.New("unknown migration name")

// isKnownMigration returns true if name is a recognised MigrationName.
func isKnownMigration(name MigrationName) bool {
	switch name {
	case MigrationDMKey, MigrationBackfill:
		return true
	default:
		return false
	}
}

// loadMigrationsDoc reads the _migrations section from hub_settings and
// returns it as a raw key-value map. Unknown keys are preserved so that
// a newer binary's markers survive a read-modify-write cycle by an older
// binary.
//
// Returns (nil, nil, nil) if the section does not exist.
// Returns (nil, nil, nil) if the section exists but is not a JSON object —
// treating corruption as "not complete" (retry) is the safe direction.
func loadMigrationsDoc(ctx context.Context, s store.Store) (*store.HubSetting, map[string]json.RawMessage, error) {
	hs, err := s.GetHubSetting(ctx, migrationsSectionName)
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, fmt.Errorf("reading %s: %w", migrationsSectionName, err)
	}

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(hs.Value, &raw); err != nil {
		// Not a JSON object: treat as absent (safe direction is retry).
		return nil, nil, nil
	}
	return hs, raw, nil
}

// persistMigrationsDoc marshals and upserts the _migrations section.
// Uses expectedRevision = -1 (unconditional upsert) for conflict safety.
func persistMigrationsDoc(ctx context.Context, s store.Store, raw map[string]json.RawMessage) error {
	docJSON, err := json.Marshal(raw)
	if err != nil {
		return fmt.Errorf("marshaling migrations doc: %w", err)
	}

	if _, err := s.UpsertHubSetting(ctx, migrationsSectionName, docJSON, "system", -1, "seeded"); err != nil {
		return fmt.Errorf("upserting %s: %w", migrationsSectionName, err)
	}
	return nil
}
