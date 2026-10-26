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

	"entgo.io/ent/dialect"

	"github.com/GoogleCloudPlatform/scion/pkg/config/opsettings"
	"github.com/GoogleCloudPlatform/scion/pkg/ent"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/hubsetting"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/predicate"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// errSettingsRevisionRowReserved refuses a caller write or delete of the
// store.HubSettingsRevisionSection row, which only bumpSettingsRevision
// writes.
var errSettingsRevisionRowReserved = fmt.Errorf("%w: hub setting section %q is reserved",
	store.ErrInvalidInput, store.HubSettingsRevisionSection)

// isOperationalSection reports whether a write of section bumps the
// operational-settings revision: the sections registered in opsettings.
// Other rows in the table (migration markers, the release-update notice,
// caches) do not, so they never change the revision a settings reader or a
// revision-checked save compares.
func isOperationalSection(section string) bool {
	return opsettings.SectionByName(section) != nil
}

// bumpSettingsRevision increments the operational-settings revision in the
// store.HubSettingsRevisionSection row inside tx, creating the row at 1
// when it does not exist, and returns the new value. It is the one place
// that writes the row. The upsert takes the row lock on Postgres (held to
// commit, after the section row lock, so the order is always section row
// then revision row) and runs under SQLite's single writer, so concurrent
// writers get distinct, increasing values.
func bumpSettingsRevision(ctx context.Context, tx *ent.Tx) (int64, error) {
	err := tx.HubSetting.Create().
		SetSection(store.HubSettingsRevisionSection).
		SetValue(json.RawMessage(`{}`)).
		SetRevision(1).
		OnConflictColumns(hubsetting.FieldSection).
		AddRevision(1).
		Exec(ctx)
	if err != nil {
		return 0, fmt.Errorf("bump settings revision: %w", err)
	}
	row, err := tx.HubSetting.Query().
		Where(hubsetting.SectionEQ(store.HubSettingsRevisionSection)).
		Only(ctx)
	if err != nil {
		return 0, fmt.Errorf("read settings revision: %w", err)
	}
	return row.Revision, nil
}

// HubSettingStore implements store.HubSettingStore backed by Ent.
type HubSettingStore struct {
	client *ent.Client
}

// NewHubSettingStore creates a new Ent-backed HubSettingStore.
func NewHubSettingStore(client *ent.Client) *HubSettingStore {
	return &HubSettingStore{client: client}
}

// usesRowLocks returns true when the underlying database supports SELECT …
// FOR UPDATE (i.e. Postgres). SQLite uses a single-writer lock instead, so
// ForUpdate must be skipped — it returns an error on SQLite. The dialect is
// read from the driver with no query, the same idiom as
// BrokerSettingStore.usesRowLocks.
func (s *HubSettingStore) usesRowLocks() bool {
	return s.client.Driver().Dialect() == dialect.Postgres
}

// entHubSettingToStore converts an Ent HubSetting entity to the store model.
func entHubSettingToStore(e *ent.HubSetting) *store.HubSetting {
	return &store.HubSetting{
		ID:        e.ID.String(),
		Section:   e.Section,
		Value:     e.Value,
		Revision:  e.Revision,
		UpdatedBy: e.UpdatedBy,
		Origin:    string(e.Origin),
		CreatedAt: e.CreateTime,
		UpdatedAt: e.UpdateTime,
	}
}

// GetHubSetting retrieves a hub setting by section name.
func (s *HubSettingStore) GetHubSetting(ctx context.Context, section string) (*store.HubSetting, error) {
	row, err := s.client.HubSetting.Query().
		Where(hubsetting.SectionEQ(section)).
		Only(ctx)
	if err != nil {
		return nil, mapError(err)
	}
	return entHubSettingToStore(row), nil
}

// ListHubSettings returns all hub settings ordered by section.
func (s *HubSettingStore) ListHubSettings(ctx context.Context) ([]store.HubSetting, error) {
	rows, err := s.client.HubSetting.Query().
		Order(hubsetting.BySection()).
		All(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]store.HubSetting, 0, len(rows))
	for _, e := range rows {
		out = append(out, *entHubSettingToStore(e))
	}
	return out, nil
}

// UpsertHubSetting creates or updates a hub setting with CAS semantics.
//
// expectedRevision semantics:
//
//	0:  create-only — returns ErrRevisionConflict if the section already exists.
//	-1: unconditional upsert (for seeding) — succeeds unless every one of
//	    a bounded number of attempts loses a concurrent create of the
//	    same section (see below).
//	>0: CAS update — returns ErrRevisionConflict if current revision != expectedRevision.
//
// The upsert is atomic: a transaction with SELECT FOR UPDATE (Postgres) or
// implicit single-writer serialization (SQLite) ensures that two concurrent
// CAS writers cannot both succeed.
//
// For -1, a lost create race is retried in a fresh transaction (at most
// upsertCreateRaceAttempts attempts), so the write succeeds on Postgres
// too unless every attempt loses; then it returns a wrapped
// ErrRevisionConflict naming the section and the attempt count. Only a
// unique violation on the section key counts as a lost create race
// (isHubSettingSectionConflict); any other constraint error is returned
// as it is, never retried. Concurrent writers there all find no row (FOR
// UPDATE locks nothing), all INSERT, and the losers fail on the unique
// section index once the winner commits. Postgres aborts the loser's whole
// transaction, so nothing of it (no revision bump) survives. The retry runs
// in a new transaction: under READ COMMITTED its SELECT ... FOR UPDATE sees
// the winner's committed row and locks it, and the write takes the update
// path, bumping the operational-settings revision as the last lock, held to
// commit. Every committed write thus gets exactly one revision, distinct and
// in commit order. Create-only (0) and CAS (>0) keep returning
// ErrRevisionConflict for a lost create race.
func (s *HubSettingStore) UpsertHubSetting(
	ctx context.Context,
	section string,
	value json.RawMessage,
	updatedBy string,
	expectedRevision int64,
	origin string,
) (*store.HubSetting, error) {
	return retryHubSettingCreateRace(section, expectedRevision, func() (*store.HubSetting, error) {
		return s.upsertHubSettingOnce(ctx, section, value, updatedBy, expectedRevision, origin)
	})
}

// retryHubSettingCreateRace runs once, retrying a lost create race
// (errHubSettingCreateRace) for expectedRevision -1 until
// upsertCreateRaceAttempts attempts have run. A lost race is reported as
// store.ErrRevisionConflict: bare for create-only (0) and CAS (>0), and
// for -1 wrapped with the section and the attempt count, so an exhausted
// bound can be told apart from a CAS conflict in logs while errors.Is
// still matches.
func retryHubSettingCreateRace(section string, expectedRevision int64, once func() (*store.HubSetting, error)) (*store.HubSetting, error) {
	for attempt := 1; ; attempt++ {
		row, err := once()
		if !errors.Is(err, errHubSettingCreateRace) {
			return row, err
		}
		if expectedRevision != -1 {
			return nil, store.ErrRevisionConflict
		}
		if attempt >= upsertCreateRaceAttempts {
			return nil, fmt.Errorf("%w: hub setting %q: lost a concurrent create in all %d attempts",
				store.ErrRevisionConflict, section, attempt)
		}
	}
}

// upsertCreateRaceAttempts bounds UpsertHubSetting's attempts for -1 after a
// lost create race.
const upsertCreateRaceAttempts = 3

// errHubSettingCreateRace reports that the create path lost a concurrent
// insert of the same section (a unique violation on the section key,
// isHubSettingSectionConflict).
// UpsertHubSetting retries it for -1 and maps it to ErrRevisionConflict
// otherwise.
var errHubSettingCreateRace = errors.New("hub setting create lost a concurrent insert")

// upsertHubSettingOnce is one transaction of UpsertHubSetting.
func (s *HubSettingStore) upsertHubSettingOnce(
	ctx context.Context,
	section string,
	value json.RawMessage,
	updatedBy string,
	expectedRevision int64,
	origin string,
) (*store.HubSetting, error) {
	if section == store.HubSettingsRevisionSection {
		return nil, errSettingsRevisionRowReserved
	}
	useLock := s.usesRowLocks()
	bump := isOperationalSection(section)

	tx, err := s.client.Tx(ctx)
	if err != nil {
		return nil, fmt.Errorf("upsert hub setting: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// Look up existing row (with FOR UPDATE on Postgres).
	q := tx.HubSetting.Query().Where(hubsetting.SectionEQ(section))
	if useLock {
		q = q.ForUpdate()
	}
	existing, err := q.Only(ctx)

	if ent.IsNotFound(err) {
		// No existing row — this is a create.
		if expectedRevision > 0 {
			// CAS update with a positive revision but no row → conflict.
			return nil, store.ErrRevisionConflict
		}
		// Create the row (expectedRevision == 0 or -1).
		create := tx.HubSetting.Create().
			SetSection(section).
			SetValue(value).
			SetRevision(1)
		if updatedBy != "" {
			create.SetUpdatedBy(updatedBy)
		}
		if origin != "" {
			create.SetOrigin(hubsetting.Origin(origin))
		}
		row, err := create.Save(ctx)
		if err != nil {
			if isHubSettingSectionConflict(err) {
				// Concurrent insert race — the other writer won.
				return nil, errHubSettingCreateRace
			}
			return nil, fmt.Errorf("upsert hub setting: create: %w", err)
		}
		var settingsRev int64
		if bump {
			if settingsRev, err = bumpSettingsRevision(ctx, tx); err != nil {
				return nil, fmt.Errorf("upsert hub setting: %w", err)
			}
		}
		if err := tx.Commit(); err != nil {
			if isHubSettingSectionConflict(err) {
				return nil, errHubSettingCreateRace
			}
			return nil, fmt.Errorf("upsert hub setting: commit create: %w", err)
		}
		out := entHubSettingToStore(row)
		out.SettingsRevision = settingsRev
		return out, nil
	}
	if err != nil {
		return nil, fmt.Errorf("upsert hub setting: query: %w", err)
	}

	// Row exists.
	if expectedRevision == 0 {
		// create-only mode but row already exists → conflict.
		return nil, store.ErrRevisionConflict
	}
	if expectedRevision > 0 && existing.Revision != expectedRevision {
		// CAS mismatch.
		return nil, store.ErrRevisionConflict
	}

	// Update: bump revision, set new value.
	newRevision := existing.Revision + 1
	update := tx.HubSetting.UpdateOneID(existing.ID).
		SetValue(value).
		SetRevision(newRevision)
	if updatedBy != "" {
		update.SetUpdatedBy(updatedBy)
	} else {
		update.ClearUpdatedBy()
	}
	if origin != "" {
		update.SetOrigin(hubsetting.Origin(origin))
	}
	row, err := update.Save(ctx)
	if err != nil {
		return nil, fmt.Errorf("upsert hub setting: update: %w", err)
	}
	var settingsRev int64
	if bump {
		if settingsRev, err = bumpSettingsRevision(ctx, tx); err != nil {
			return nil, fmt.Errorf("upsert hub setting: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("upsert hub setting: commit update: %w", err)
	}
	out := entHubSettingToStore(row)
	out.SettingsRevision = settingsRev
	return out, nil
}

// DeleteHubSetting removes a hub setting by section name. Deleting an
// operational section bumps the operational-settings revision in the same
// transaction (see DeleteHubSettingRevision).
func (s *HubSettingStore) DeleteHubSetting(ctx context.Context, section string) error {
	_, err := s.DeleteHubSettingRevision(ctx, section)
	return err
}

// DeleteHubSettingRevision is DeleteHubSetting, also returning the
// operational-settings revision the delete's transaction bumped to (0 for
// a section that is not operational). It implements the optional
// store.HubSettingRevisionDeleter.
func (s *HubSettingStore) DeleteHubSettingRevision(ctx context.Context, section string) (int64, error) {
	if section == store.HubSettingsRevisionSection {
		return 0, errSettingsRevisionRowReserved
	}
	tx, err := s.client.Tx(ctx)
	if err != nil {
		return 0, fmt.Errorf("delete hub setting: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	n, err := tx.HubSetting.Delete().
		Where(hubsetting.SectionEQ(section)).
		Exec(ctx)
	if err != nil {
		return 0, fmt.Errorf("delete hub setting: %w", err)
	}
	if n == 0 {
		return 0, store.ErrNotFound
	}
	var settingsRev int64
	if isOperationalSection(section) {
		if settingsRev, err = bumpSettingsRevision(ctx, tx); err != nil {
			return 0, fmt.Errorf("delete hub setting: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("delete hub setting: commit: %w", err)
	}
	return settingsRev, nil
}

// BackfillOrigin sets the origin column for existing rows that predate
// the origin field. Rows with updated_by="seed" keep origin="seeded" (the
// column default). Rows with updated_by!="seed" (admin writes) get
// origin="managed". The _meta row and the reserved
// store.HubSettingsRevisionSection row are exempt by name, as in
// opsettings.EffectiveOrigin. Idempotent — rows that already have the
// correct origin are unaffected because the column default is "seeded"
// and this only updates the non-seed rows.
//
// The origin of an operational section is part of the settings a reader
// sees (a seeded endpoints row's public_url is not a saved value), so when
// the update changes an operational row, the operational-settings revision
// is bumped in the same transaction. The rows are updated in two
// statements partitioned by section name (the sections registered in
// opsettings.SectionNames), never by the rows a query found
// first, so a row committed between the statements is still counted on
// the right side; the bump is decided by the rows the operational update
// changed. A replica that backfills concurrently finds its rows already
// changed once the first replica commits (Postgres re-checks the predicate
// after the row lock), updates none and does not bump.
func (s *HubSettingStore) BackfillOrigin(ctx context.Context) error {
	tx, err := s.client.Tx(ctx)
	if err != nil {
		return fmt.Errorf("backfill origin: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	match := []predicate.HubSetting{
		hubsetting.UpdatedByNEQ("seed"),
		hubsetting.SectionNEQ("_meta"),
		hubsetting.SectionNEQ(store.HubSettingsRevisionSection),
		hubsetting.OriginEQ(hubsetting.OriginSeeded),
	}
	// Early exit when nothing matches; the updates below do not depend on
	// this read.
	found, err := tx.HubSetting.Query().Where(match...).Exist(ctx)
	if err != nil {
		return fmt.Errorf("backfill origin: query: %w", err)
	}
	if !found {
		return nil
	}
	operational := opsettings.SectionNames()
	if _, err := tx.HubSetting.Update().
		Where(append(match, hubsetting.SectionNotIn(operational...))...).
		SetOrigin(hubsetting.OriginManaged).
		Save(ctx); err != nil {
		return fmt.Errorf("backfill origin: %w", err)
	}
	n, err := tx.HubSetting.Update().
		Where(append(match, hubsetting.SectionIn(operational...))...).
		SetOrigin(hubsetting.OriginManaged).
		Save(ctx)
	if err != nil {
		return fmt.Errorf("backfill origin: %w", err)
	}
	if n > 0 {
		if _, err := bumpSettingsRevision(ctx, tx); err != nil {
			return fmt.Errorf("backfill origin: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("backfill origin: commit: %w", err)
	}
	return nil
}

// Compile-time assertions. The composite store the hub server runs on
// promotes the optional revision-returning delete from the embedded
// HubSettingStore.
var (
	_ store.HubSettingStore           = (*HubSettingStore)(nil)
	_ store.HubSettingRevisionDeleter = (*HubSettingStore)(nil)
	_ store.HubSettingRevisionDeleter = (*CompositeStore)(nil)
)
