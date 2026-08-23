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
	"time"

	entsql "entgo.io/ent/dialect/sql"

	"github.com/GoogleCloudPlatform/scion/pkg/ent"
	entskillinjection "github.com/GoogleCloudPlatform/scion/pkg/ent/skillinjection"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
)

// SkillInjectionStore implements store.SkillInjectionStore using Ent ORM.
type SkillInjectionStore struct {
	client *ent.Client
}

// NewSkillInjectionStore creates a new Ent-backed SkillInjectionStore.
func NewSkillInjectionStore(client *ent.Client) *SkillInjectionStore {
	return &SkillInjectionStore{client: client}
}

// entSkillInjectionToStore converts an ent.SkillInjection to a store.SkillInjection.
func entSkillInjectionToStore(e *ent.SkillInjection) store.SkillInjection {
	return store.SkillInjection{
		ID:           e.ID.String(),
		Scope:        string(e.Scope), // convert enum to string
		ScopeID:      e.ScopeID,
		SkillURI:     e.SkillURI,
		SkillAs:      e.SkillAs,
		Optional:     e.Optional,
		AllowProgeny: e.AllowProgeny,
		SortOrder:    e.SortOrder,
		CreatedAt:    e.CreatedAt,
		CreatedBy:    e.CreatedBy,
	}
}

// ListSkillInjections returns all skill injections for a given scope+scopeID,
// ordered by sort_order ascending.
func (s *SkillInjectionStore) ListSkillInjections(ctx context.Context, scope, scopeID string) ([]store.SkillInjection, error) {
	rows, err := s.client.SkillInjection.Query().
		Where(
			entskillinjection.ScopeEQ(entskillinjection.Scope(scope)),
			entskillinjection.ScopeIDEQ(scopeID),
		).
		Order(entskillinjection.BySortOrder(entsql.OrderAsc())).
		All(ctx)
	if err != nil {
		return nil, mapError(err)
	}

	result := make([]store.SkillInjection, 0, len(rows))
	for _, e := range rows {
		result = append(result, entSkillInjectionToStore(e))
	}
	return result, nil
}

// AddSkillInjection creates a new skill injection entry.
// If si.ID is empty a UUID is auto-generated and written back to si.ID on
// success, mirroring the behaviour of SetSkillInjections. Callers that need a
// stable ID before the call can pre-populate si.ID with any valid UUID string.
// Returns ErrAlreadyExists if an entry with the same (scope, scope_id, skill_uri) already exists.
func (s *SkillInjectionStore) AddSkillInjection(ctx context.Context, si *store.SkillInjection) error {
	var uid uuid.UUID
	if si.ID == "" {
		uid = uuid.New()
	} else {
		var err error
		uid, err = parseUUID(si.ID)
		if err != nil {
			return err
		}
	}

	createdAt := time.Now()

	_, err := s.client.SkillInjection.Create().
		SetID(uid).
		SetScope(entskillinjection.Scope(si.Scope)).
		SetScopeID(si.ScopeID).
		SetSkillURI(si.SkillURI).
		SetNillableSkillAs(nullableString(si.SkillAs)).
		SetOptional(si.Optional).
		SetAllowProgeny(si.AllowProgeny).
		SetSortOrder(si.SortOrder).
		SetCreatedAt(createdAt).
		SetNillableCreatedBy(nullableString(si.CreatedBy)).
		Save(ctx)
	if err != nil {
		return mapError(err)
	}

	// Only write back to si after Save succeeds to avoid mutating caller's
	// struct with a CreatedAt/ID that was never persisted.
	si.ID = uid.String()
	si.CreatedAt = createdAt
	return nil
}

// UpdateSkillInjection updates the mutable fields of a skill injection entry.
// Returns ErrNotFound if the entry with the given ID does not exist.
func (s *SkillInjectionStore) UpdateSkillInjection(ctx context.Context, si *store.SkillInjection) error {
	uid, err := parseUUID(si.ID)
	if err != nil {
		return err
	}

	update := s.client.SkillInjection.UpdateOneID(uid).
		SetSkillURI(si.SkillURI).
		SetOptional(si.Optional).
		SetAllowProgeny(si.AllowProgeny).
		SetSortOrder(si.SortOrder)

	if si.SkillAs != "" {
		update = update.SetSkillAs(si.SkillAs)
	} else {
		update = update.ClearSkillAs()
	}

	_, err = update.Save(ctx)
	if err != nil {
		return mapError(err)
	}
	return nil
}

// RemoveSkillInjection deletes a skill injection entry by ID.
// Returns ErrNotFound if the entry doesn't exist.
func (s *SkillInjectionStore) RemoveSkillInjection(ctx context.Context, id string) error {
	uid, err := parseUUID(id)
	if err != nil {
		return err
	}

	err = s.client.SkillInjection.DeleteOneID(uid).Exec(ctx)
	if err != nil {
		return mapError(err)
	}
	return nil
}

// SetSkillInjections atomically replaces the full list for a scope+scopeID.
// All existing entries for (scope, scopeID) are deleted and the new entries are
// bulk-inserted in a single serializable transaction. On Postgres the transaction
// is retried on serialization failure so concurrent calls are safe under READ
// COMMITTED default isolation. SortOrder is taken from each entry as-is.
func (s *SkillInjectionStore) SetSkillInjections(ctx context.Context, scope, scopeID string, entries []store.SkillInjection, createdBy string) error {
	return runSerializableEntTx(ctx, s.client, func(ctx context.Context, tx *ent.Tx) error {
		// Delete all existing entries for this scope+scopeID.
		_, err := tx.SkillInjection.Delete().
			Where(
				entskillinjection.ScopeEQ(entskillinjection.Scope(scope)),
				entskillinjection.ScopeIDEQ(scopeID),
			).
			Exec(ctx)
		if err != nil {
			return mapError(err)
		}

		if len(entries) == 0 {
			return nil
		}

		// Bulk-insert all new entries in a single round-trip.
		now := time.Now()
		builders := make([]*ent.SkillInjectionCreate, 0, len(entries))
		for _, entry := range entries {
			builders = append(builders, tx.SkillInjection.Create().
				SetID(uuid.New()).
				SetScope(entskillinjection.Scope(scope)).
				SetScopeID(scopeID).
				SetSkillURI(entry.SkillURI).
				SetNillableSkillAs(nullableString(entry.SkillAs)).
				SetOptional(entry.Optional).
				SetAllowProgeny(entry.AllowProgeny).
				SetSortOrder(entry.SortOrder).
				SetCreatedAt(now).
				SetNillableCreatedBy(nullableString(createdBy)))
		}

		return mapError(tx.SkillInjection.CreateBulk(builders...).Exec(ctx))
	})
}

// ListProgenySkillInjections returns user-scoped skill injections with
// allowProgeny=true whose createdBy is in the given set of ancestor IDs.
func (s *SkillInjectionStore) ListProgenySkillInjections(ctx context.Context, ancestorIDs []string) ([]store.SkillInjection, error) {
	if len(ancestorIDs) == 0 {
		return nil, nil
	}

	rows, err := s.client.SkillInjection.Query().
		Where(
			entskillinjection.ScopeEQ(entskillinjection.ScopeUser),
			entskillinjection.AllowProgenyEQ(true),
			entskillinjection.CreatedByIn(ancestorIDs...),
		).
		Order(entskillinjection.BySortOrder(entsql.OrderAsc())).
		All(ctx)
	if err != nil {
		return nil, mapError(err)
	}

	result := make([]store.SkillInjection, 0, len(rows))
	for _, e := range rows {
		result = append(result, entSkillInjectionToStore(e))
	}
	return result, nil
}

// DeleteSkillInjectionsByScope removes all skill injection entries for the given
// scope+scopeID. It is used during project or user deletion to cascade-clean
// entries that have no FK cascade. Returns the number of rows deleted.
func (s *SkillInjectionStore) DeleteSkillInjectionsByScope(ctx context.Context, scope, scopeID string) (int, error) {
	n, err := s.client.SkillInjection.Delete().
		Where(
			entskillinjection.ScopeEQ(entskillinjection.Scope(scope)),
			entskillinjection.ScopeIDEQ(scopeID),
		).
		Exec(ctx)
	return n, mapError(err)
}
