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

	"entgo.io/ent/dialect"

	"github.com/GoogleCloudPlatform/scion/pkg/ent"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/brokersetting"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// BrokerSettingStore implements store.BrokerSettingStore backed by Ent.
// Modeled on HubSettingStore (pkg/store/entadapter/hubsetting_store.go): a
// single JSON document per key (here, per broker) with revision-based CAS.
type BrokerSettingStore struct {
	client *ent.Client
}

// NewBrokerSettingStore creates a new Ent-backed BrokerSettingStore.
func NewBrokerSettingStore(client *ent.Client) *BrokerSettingStore {
	return &BrokerSettingStore{client: client}
}

// usesRowLocks returns true when the underlying database supports SELECT …
// FOR UPDATE (i.e. Postgres). SQLite uses a single-writer lock instead, so
// ForUpdate must be skipped — it returns an error on SQLite.
//
// The dialect is read from the driver (a construction-time property), with
// no query — same idiom as CompositeStore.isPostgres (locking.go).
func (s *BrokerSettingStore) usesRowLocks(context.Context) bool {
	return s.client.Driver().Dialect() == dialect.Postgres
}

// entBrokerSettingToStore converts an Ent BrokerSetting entity to the store
// model, unmarshaling the stored JSON document into a typed
// store.BrokerSettings. An unmarshal failure is treated as an empty document
// rather than a hard error, matching the "unknown/legacy shape is inert"
// convention used elsewhere for JSON document columns.
func entBrokerSettingToStore(e *ent.BrokerSetting) *store.BrokerSettingsRecord {
	var settings store.BrokerSettings
	if len(e.Value) > 0 {
		_ = json.Unmarshal(e.Value, &settings)
	}
	return &store.BrokerSettingsRecord{
		BrokerID:  e.BrokerID,
		Settings:  settings,
		Revision:  e.Revision,
		UpdatedBy: e.UpdatedBy,
		Updated:   e.UpdateTime,
	}
}

// GetBrokerSettings retrieves brokerID's settings document.
func (s *BrokerSettingStore) GetBrokerSettings(ctx context.Context, brokerID string) (*store.BrokerSettingsRecord, error) {
	row, err := s.client.BrokerSetting.Query().
		Where(brokersetting.BrokerIDEQ(brokerID)).
		Only(ctx)
	if err != nil {
		return nil, mapError(err)
	}
	return entBrokerSettingToStore(row), nil
}

// PutBrokerSettings creates or replaces brokerID's settings document with CAS
// semantics.
//
// expectedRevision semantics:
//
//	0:  create-only — returns ErrRevisionConflict if a row already exists.
//	>0: CAS update — returns ErrRevisionConflict if current revision != expectedRevision.
//
// The write is a full replace of the document (design.md §5.4): callers pass
// the complete BrokerSettings they want stored, not a delta.
func (s *BrokerSettingStore) PutBrokerSettings(
	ctx context.Context,
	brokerID string,
	settings store.BrokerSettings,
	expectedRevision int64,
	updatedBy string,
) (*store.BrokerSettingsRecord, error) {
	value, err := json.Marshal(settings)
	if err != nil {
		return nil, fmt.Errorf("put broker settings: marshal: %w", err)
	}

	// usesRowLocks is a pure driver read; computed before Tx for clarity.
	useLock := s.usesRowLocks(ctx)

	tx, err := s.client.Tx(ctx)
	if err != nil {
		return nil, fmt.Errorf("put broker settings: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	q := tx.BrokerSetting.Query().Where(brokersetting.BrokerIDEQ(brokerID))
	if useLock {
		q = q.ForUpdate()
	}
	existing, err := q.Only(ctx)

	if ent.IsNotFound(err) {
		if expectedRevision > 0 {
			// CAS update with a positive revision but no row → conflict.
			return nil, store.ErrRevisionConflict
		}
		create := tx.BrokerSetting.Create().
			SetBrokerID(brokerID).
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
			return nil, fmt.Errorf("put broker settings: create: %w", err)
		}
		if err := tx.Commit(); err != nil {
			if ent.IsConstraintError(err) {
				return nil, store.ErrRevisionConflict
			}
			return nil, fmt.Errorf("put broker settings: commit create: %w", err)
		}
		return entBrokerSettingToStore(row), nil
	}
	if err != nil {
		return nil, fmt.Errorf("put broker settings: query: %w", err)
	}

	// Row exists.
	if expectedRevision == 0 {
		// create-only mode but row already exists → conflict.
		return nil, store.ErrRevisionConflict
	}
	if existing.Revision != expectedRevision {
		return nil, store.ErrRevisionConflict
	}

	newRevision := existing.Revision + 1
	update := tx.BrokerSetting.UpdateOneID(existing.ID).
		SetValue(value).
		SetRevision(newRevision)
	if updatedBy != "" {
		update.SetUpdatedBy(updatedBy)
	} else {
		update.ClearUpdatedBy()
	}
	row, err := update.Save(ctx)
	if err != nil {
		return nil, fmt.Errorf("put broker settings: update: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("put broker settings: commit update: %w", err)
	}
	return entBrokerSettingToStore(row), nil
}

// DeleteBrokerSettings removes brokerID's settings row, if any. Unlike
// DeleteHubSetting, a missing row is not an error: this is called
// unconditionally from DeleteRuntimeBroker, which must not fail just because
// the broker never had a settings row.
func (s *BrokerSettingStore) DeleteBrokerSettings(ctx context.Context, brokerID string) error {
	_, err := s.client.BrokerSetting.Delete().
		Where(brokersetting.BrokerIDEQ(brokerID)).
		Exec(ctx)
	if err != nil {
		return fmt.Errorf("delete broker settings: %w", err)
	}
	return nil
}

// Compile-time assertion.
var _ store.BrokerSettingStore = (*BrokerSettingStore)(nil)
