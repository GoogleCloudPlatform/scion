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
	"sort"

	"entgo.io/ent/dialect/sql"

	"github.com/GoogleCloudPlatform/scion/pkg/ent"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/hubsetting"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// UpsertHubSettingsGuarded implements store.HubSettingGuardedBatchWriter:
// one transaction serializes against every other guarded batch, compares
// the read revisions and the writes' expected revisions, writes or deletes
// every section and bumps the operational-settings revision once. Any
// mismatch, including a lost concurrent create, returns
// store.ErrRevisionConflict and nothing is written.
//
// On Postgres the transaction first takes the store.LockHubSettingsGuardedBatch
// advisory lock (held to commit or rollback), so a read guard on a section
// with no row, which FOR UPDATE cannot lock, still holds; the rows are also
// locked FOR UPDATE in section order against the single-section writers.
// On SQLite the hub's pool has one connection (MaxOpenConns is forced to 1
// for SQLite), so the transaction holds the only connection and no other
// write can start before it ends.
func (s *HubSettingStore) UpsertHubSettingsGuarded(
	ctx context.Context,
	writes []store.HubSettingWrite,
	reads map[string]int64,
	updatedBy, origin string,
) ([]store.HubSetting, error) {
	if len(writes) == 0 {
		return nil, fmt.Errorf("%w: guarded hub settings batch has no writes", store.ErrInvalidInput)
	}
	seen := make(map[string]bool, len(writes)+len(reads))
	check := func(section string) error {
		if section == store.HubSettingsRevisionSection {
			return errSettingsRevisionRowReserved
		}
		if seen[section] {
			return fmt.Errorf("%w: hub setting section %q appears twice in a guarded batch", store.ErrInvalidInput, section)
		}
		seen[section] = true
		return nil
	}
	for _, w := range writes {
		if err := check(w.Section); err != nil {
			return nil, err
		}
	}
	for section := range reads {
		if err := check(section); err != nil {
			return nil, err
		}
	}
	sections := make([]string, 0, len(seen))
	for section := range seen {
		sections = append(sections, section)
	}
	sort.Strings(sections)

	tx, err := s.client.Tx(ctx)
	if err != nil {
		return nil, fmt.Errorf("guarded hub settings batch: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if s.usesRowLocks() {
		var res sql.Result
		if err := tx.Client().Driver().Exec(ctx, "SELECT pg_advisory_xact_lock($1)",
			[]any{int64(store.LockHubSettingsGuardedBatch)}, &res); err != nil {
			return nil, fmt.Errorf("guarded hub settings batch: lock: %w", err)
		}
	}

	rows := make(map[string]*ent.HubSetting, len(sections))
	for _, section := range sections {
		q := tx.HubSetting.Query().Where(hubsetting.SectionEQ(section))
		if s.usesRowLocks() {
			q = q.ForUpdate()
		}
		row, err := q.Only(ctx)
		if ent.IsNotFound(err) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("guarded hub settings batch: query %q: %w", section, err)
		}
		rows[section] = row
	}

	revisionOf := func(section string) int64 {
		if row := rows[section]; row != nil {
			return row.Revision
		}
		return 0
	}
	for section, rev := range reads {
		if revisionOf(section) != rev {
			return nil, store.ErrRevisionConflict
		}
	}
	for _, w := range writes {
		switch {
		case w.Delete && rows[w.Section] == nil:
			if w.ExpectedRevision == -1 {
				return nil, store.ErrNotFound
			}
			return nil, store.ErrRevisionConflict
		case w.ExpectedRevision == -1:
		case w.ExpectedRevision == 0:
			if rows[w.Section] != nil {
				return nil, store.ErrRevisionConflict
			}
		case rows[w.Section] == nil || rows[w.Section].Revision != w.ExpectedRevision:
			return nil, store.ErrRevisionConflict
		}
	}

	bump := false
	out := make([]store.HubSetting, 0, len(writes))
	for _, w := range writes {
		var saved *ent.HubSetting
		if w.Delete {
			err = tx.HubSetting.DeleteOneID(rows[w.Section].ID).Exec(ctx)
		} else if existing := rows[w.Section]; existing == nil {
			create := tx.HubSetting.Create().SetSection(w.Section).SetValue(w.Value).SetRevision(1)
			if updatedBy != "" {
				create.SetUpdatedBy(updatedBy)
			}
			if origin != "" {
				create.SetOrigin(hubsetting.Origin(origin))
			}
			saved, err = create.Save(ctx)
		} else {
			update := tx.HubSetting.UpdateOneID(existing.ID).SetValue(w.Value).SetRevision(existing.Revision + 1)
			if updatedBy != "" {
				update.SetUpdatedBy(updatedBy)
			} else {
				update.ClearUpdatedBy()
			}
			if origin != "" {
				update.SetOrigin(hubsetting.Origin(origin))
			}
			saved, err = update.Save(ctx)
		}
		if err != nil {
			if isHubSettingSectionConflict(err) {
				return nil, store.ErrRevisionConflict
			}
			return nil, fmt.Errorf("guarded hub settings batch: write %q: %w", w.Section, err)
		}
		if isOperationalSection(w.Section) {
			bump = true
		}
		if w.Delete {
			out = append(out, store.HubSetting{Section: w.Section})
			continue
		}
		out = append(out, *entHubSettingToStore(saved))
	}
	var settingsRev int64
	if bump {
		if settingsRev, err = bumpSettingsRevision(ctx, tx); err != nil {
			return nil, fmt.Errorf("guarded hub settings batch: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		if isHubSettingSectionConflict(err) {
			return nil, store.ErrRevisionConflict
		}
		return nil, fmt.Errorf("guarded hub settings batch: commit: %w", err)
	}
	for i := range out {
		out[i].SettingsRevision = settingsRev
	}
	return out, nil
}
