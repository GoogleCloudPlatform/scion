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

	"entgo.io/ent/dialect"

	"github.com/GoogleCloudPlatform/scion/pkg/ent"
	entskillregistry "github.com/GoogleCloudPlatform/scion/pkg/ent/skillregistry"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// SkillRegistryStore implements store.SkillRegistryStore using Ent ORM.
type SkillRegistryStore struct {
	client *ent.Client
}

// NewSkillRegistryStore creates a new Ent-backed SkillRegistryStore.
func NewSkillRegistryStore(client *ent.Client) *SkillRegistryStore {
	return &SkillRegistryStore{client: client}
}

func entSkillRegistryToStore(e *ent.SkillRegistry) *store.SkillRegistry {
	r := &store.SkillRegistry{
		ID:          e.ID.String(),
		Name:        e.Name,
		Endpoint:    e.Endpoint,
		Description: e.Description,
		Type:        string(e.Type),
		TrustLevel:  string(e.TrustLevel),
		AuthToken:   e.AuthToken,
		ResolvePath: e.ResolvePath,
		Status:      string(e.Status),
		CreatedBy:   e.CreatedBy,
		Created:     e.Created,
		Updated:     e.Updated,
	}
	if e.PinnedHashes != "" {
		_ = json.Unmarshal([]byte(e.PinnedHashes), &r.PinnedHashes)
	}
	return r
}

func (s *SkillRegistryStore) CreateSkillRegistry(ctx context.Context, registry *store.SkillRegistry) error {
	pinnedHashesJSON := ""
	if len(registry.PinnedHashes) > 0 {
		b, _ := json.Marshal(registry.PinnedHashes)
		pinnedHashesJSON = string(b)
	}

	create := s.client.SkillRegistry.Create().
		SetName(registry.Name).
		SetEndpoint(registry.Endpoint).
		SetDescription(registry.Description).
		SetType(entskillregistry.Type(registry.Type)).
		SetTrustLevel(entskillregistry.TrustLevel(registry.TrustLevel)).
		SetResolvePath(registry.ResolvePath).
		SetStatus(entskillregistry.Status(registry.Status))

	if registry.AuthToken != "" {
		create.SetAuthToken(registry.AuthToken)
	}
	if pinnedHashesJSON != "" {
		create.SetPinnedHashes(pinnedHashesJSON)
	}
	if registry.CreatedBy != "" {
		create.SetCreatedBy(registry.CreatedBy)
	}

	e, err := create.Save(ctx)
	if err != nil {
		return mapError(err)
	}
	registry.ID = e.ID.String()
	registry.Created = e.Created
	registry.Updated = e.Updated
	return nil
}

func (s *SkillRegistryStore) GetSkillRegistry(ctx context.Context, id string) (*store.SkillRegistry, error) {
	uid, err := parseUUID(id)
	if err != nil {
		return nil, store.ErrNotFound
	}
	e, err := s.client.SkillRegistry.Get(ctx, uid)
	if err != nil {
		return nil, mapError(err)
	}
	return entSkillRegistryToStore(e), nil
}

func (s *SkillRegistryStore) GetSkillRegistryByName(ctx context.Context, name string) (*store.SkillRegistry, error) {
	e, err := s.client.SkillRegistry.Query().
		Where(entskillregistry.NameEQ(name)).
		Only(ctx)
	if err != nil {
		return nil, mapError(err)
	}
	return entSkillRegistryToStore(e), nil
}

func (s *SkillRegistryStore) UpdateSkillRegistry(ctx context.Context, registry *store.SkillRegistry) error {
	uid, err := parseUUID(registry.ID)
	if err != nil {
		return store.ErrNotFound
	}

	update := s.client.SkillRegistry.UpdateOneID(uid)

	if registry.Name != "" {
		update.SetName(registry.Name)
	}
	if registry.Endpoint != "" {
		update.SetEndpoint(registry.Endpoint)
	}
	update.SetDescription(registry.Description)
	if registry.Type != "" {
		update.SetType(entskillregistry.Type(registry.Type))
	}
	if registry.TrustLevel != "" {
		update.SetTrustLevel(entskillregistry.TrustLevel(registry.TrustLevel))
	}
	if registry.AuthToken != "" {
		update.SetAuthToken(registry.AuthToken)
	}
	if registry.ResolvePath != "" {
		update.SetResolvePath(registry.ResolvePath)
	}
	if registry.Status != "" {
		update.SetStatus(entskillregistry.Status(registry.Status))
	}
	if len(registry.PinnedHashes) > 0 {
		b, _ := json.Marshal(registry.PinnedHashes)
		update.SetPinnedHashes(string(b))
	}

	e, err := update.Save(ctx)
	if err != nil {
		return mapError(err)
	}
	registry.Updated = e.Updated
	return nil
}

func (s *SkillRegistryStore) DeleteSkillRegistry(ctx context.Context, id string) error {
	uid, err := parseUUID(id)
	if err != nil {
		return store.ErrNotFound
	}
	err = s.client.SkillRegistry.DeleteOneID(uid).Exec(ctx)
	return mapError(err)
}

func (s *SkillRegistryStore) ListSkillRegistries(ctx context.Context, opts store.ListOptions) (*store.ListResult[store.SkillRegistry], error) {
	query := s.client.SkillRegistry.Query().
		Order(ent.Asc(entskillregistry.FieldName))

	total, err := query.Clone().Count(ctx)
	if err != nil {
		return nil, mapError(err)
	}

	if opts.Limit > 0 {
		query.Limit(opts.Limit)
	}

	entries, err := query.All(ctx)
	if err != nil {
		return nil, mapError(err)
	}

	items := make([]store.SkillRegistry, 0, len(entries))
	for _, e := range entries {
		items = append(items, *entSkillRegistryToStore(e))
	}

	return &store.ListResult[store.SkillRegistry]{
		Items:      items,
		TotalCount: total,
	}, nil
}

func (s *SkillRegistryStore) PinSkillHash(ctx context.Context, registryID string, uri string, hash string) error {
	uid, err := parseUUID(registryID)
	if err != nil {
		return store.ErrNotFound
	}

	tx, err := s.client.Tx(ctx)
	if err != nil {
		return mapError(err)
	}
	defer tx.Rollback()

	query := tx.SkillRegistry.Query().
		Where(entskillregistry.ID(uid))
	// ForUpdate prevents lost updates from concurrent PinSkillHash calls on
	// Postgres (read-modify-write on PinnedHashes JSON). SQLite does not
	// support SELECT ... FOR UPDATE but serialises writes at the engine level.
	if s.client.Driver().Dialect() == dialect.Postgres {
		query = query.ForUpdate()
	}
	e, err := query.Only(ctx)
	if err != nil {
		return mapError(err)
	}

	hashes := make(map[string]string)
	if e.PinnedHashes != "" {
		_ = json.Unmarshal([]byte(e.PinnedHashes), &hashes)
	}
	hashes[uri] = hash

	b, _ := json.Marshal(hashes)
	_, err = tx.SkillRegistry.UpdateOneID(uid).
		SetPinnedHashes(string(b)).
		Save(ctx)
	if err != nil {
		return mapError(err)
	}

	return mapError(tx.Commit())
}

func (s *SkillRegistryStore) GetPinnedHash(ctx context.Context, registryID string, uri string) (string, error) {
	uid, err := parseUUID(registryID)
	if err != nil {
		return "", store.ErrNotFound
	}
	e, err := s.client.SkillRegistry.Get(ctx, uid)
	if err != nil {
		return "", mapError(err)
	}

	if e.PinnedHashes == "" {
		return "", store.ErrNotFound
	}

	hashes := make(map[string]string)
	if err := json.Unmarshal([]byte(e.PinnedHashes), &hashes); err != nil {
		return "", store.ErrNotFound
	}
	h, ok := hashes[uri]
	if !ok {
		return "", store.ErrNotFound
	}
	return h, nil
}
