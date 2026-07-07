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
	"fmt"
	"sync"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"

	"github.com/GoogleCloudPlatform/scion/pkg/ent"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/hubsetting"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// HubSettingStore implements store.HubSettingStore backed by Ent.
type HubSettingStore struct {
	client      *ent.Client
	dialectOnce sync.Once
	dialectName string
}

// NewHubSettingStore creates a new Ent-backed HubSettingStore.
func NewHubSettingStore(client *ent.Client) *HubSettingStore {
	return &HubSettingStore{client: client}
}

// usesRowLocks returns true when the underlying database supports SELECT …
// FOR UPDATE (i.e. Postgres). SQLite uses a single-writer lock instead, so
// ForUpdate must be skipped — it returns an error on SQLite.
func (s *HubSettingStore) usesRowLocks(ctx context.Context) bool {
	s.dialectOnce.Do(func() {
		_, _ = s.client.HubSetting.Query().
			Where(func(sel *entsql.Selector) { s.dialectName = sel.Dialect() }).
			Exist(ctx)
	})
	return s.dialectName == dialect.Postgres
}

// entHubSettingToStore converts an Ent HubSetting entity to the store model.
func entHubSettingToStore(e *ent.HubSetting) *store.HubSetting {
	return &store.HubSetting{
		ID:        e.ID.String(),
		Section:   e.Section,
		Value:     e.Value,
		Revision:  e.Revision,
		UpdatedBy: e.UpdatedBy,
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
//	-1: unconditional upsert (for seeding) — always succeeds.
//	>0: CAS update — returns ErrRevisionConflict if current revision != expectedRevision.
//
// The upsert is atomic: a transaction with SELECT FOR UPDATE (Postgres) or
// implicit single-writer serialization (SQLite) ensures that two concurrent
// CAS writers cannot both succeed.
func (s *HubSettingStore) UpsertHubSetting(
	ctx context.Context,
	section string,
	value json.RawMessage,
	updatedBy string,
	expectedRevision int64,
) (*store.HubSetting, error) {
	// Detect dialect BEFORE opening a transaction — with SQLite's
	// MaxOpenConns=1 the dialect-probe query would deadlock if the
	// tx already held the single connection.
	useLock := s.usesRowLocks(ctx)

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
		row, err := create.Save(ctx)
		if err != nil {
			if ent.IsConstraintError(err) {
				// Concurrent insert race — the other writer won.
				return nil, store.ErrRevisionConflict
			}
			return nil, fmt.Errorf("upsert hub setting: create: %w", err)
		}
		if err := tx.Commit(); err != nil {
			if ent.IsConstraintError(err) {
				return nil, store.ErrRevisionConflict
			}
			return nil, fmt.Errorf("upsert hub setting: commit create: %w", err)
		}
		return entHubSettingToStore(row), nil
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
	row, err := update.Save(ctx)
	if err != nil {
		return nil, fmt.Errorf("upsert hub setting: update: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("upsert hub setting: commit update: %w", err)
	}
	return entHubSettingToStore(row), nil
}

// DeleteHubSetting removes a hub setting by section name.
func (s *HubSettingStore) DeleteHubSetting(ctx context.Context, section string) error {
	n, err := s.client.HubSetting.Delete().
		Where(hubsetting.SectionEQ(section)).
		Exec(ctx)
	if err != nil {
		return fmt.Errorf("delete hub setting: %w", err)
	}
	if n == 0 {
		return store.ErrNotFound
	}
	return nil
}

// Compile-time assertion.
var _ store.HubSettingStore = (*HubSettingStore)(nil)
