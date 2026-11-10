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
	"errors"
	"time"

	"github.com/google/uuid"

	"github.com/GoogleCloudPlatform/scion/pkg/ent"
	entpendingsecret "github.com/GoogleCloudPlatform/scion/pkg/ent/pendingsecret"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// pendingSecretWriteAttempts bounds CreateOrReplacePendingSecret's retries
// after losing a race on the (key, scope, scope_id) unique index: the retry
// sees the winner's row and decides against it.
const pendingSecretWriteAttempts = 3

// PendingSecretStore implements store.PendingSecretStore using Ent ORM.
type PendingSecretStore struct {
	client *ent.Client
	inTx   bool // true when client wraps an ambient WithTx transaction
}

// NewPendingSecretStore creates a new Ent-backed PendingSecretStore.
func NewPendingSecretStore(client *ent.Client) *PendingSecretStore {
	return &PendingSecretStore{client: client}
}

var _ store.PendingSecretStore = (*PendingSecretStore)(nil)

// entPendingSecretToStore converts an Ent PendingSecret to the store model.
// The value ref is copied only when includeRef is true.
func entPendingSecretToStore(e *ent.PendingSecret, includeRef bool) store.PendingSecret {
	p := store.PendingSecret{
		ID:               e.ID.String(),
		Key:              e.Key,
		SecretType:       string(e.SecretType),
		Target:           e.Target,
		Scope:            e.Scope,
		ScopeID:          e.ScopeID,
		AllowProgeny:     e.AllowProgeny,
		InjectionMode:    string(e.InjectionMode),
		Description:      e.Description,
		ValueLen:         e.ValueLen,
		AgentID:          e.AgentID,
		AgentSlug:        e.AgentSlug,
		ProjectID:        e.ProjectID,
		ProjectName:      e.ProjectName,
		Created:          e.Created,
		Updated:          e.Updated,
		ReplacesSecretID: e.ReplacesSecretID,
		ReplacesVersion:  e.ReplacesVersion,
		ExpiresAt:        e.ExpiresAt,
	}
	if p.Target == "" {
		p.Target = p.Key
	}
	if includeRef {
		p.ValueRef = e.ValueRef
	}
	return p
}

// CreateOrReplacePendingSecret implements store.PendingSecretStore.
func (s *PendingSecretStore) CreateOrReplacePendingSecret(ctx context.Context, p *store.PendingSecret, now time.Time) (*store.PendingSecretWriteResult, error) {
	if p == nil {
		return nil, store.ErrInvalidInput
	}
	var (
		res *store.PendingSecretWriteResult
		err error
	)
	for attempt := 0; attempt < pendingSecretWriteAttempts; attempt++ {
		err = runInEntTx(ctx, s.client, s.inTx, func(c *ent.Client) error {
			var txErr error
			res, txErr = createOrReplacePendingSecret(ctx, c, p, now)
			return txErr
		})
		// A lost race on the unique index is retried in a new transaction,
		// which sees the winner's row. Inside an ambient transaction there
		// is no retry: the caller owns that transaction.
		if !errors.Is(err, store.ErrAlreadyExists) || s.inTx {
			break
		}
	}
	if err != nil {
		return nil, err
	}
	return res, nil
}

func createOrReplacePendingSecret(ctx context.Context, c *ent.Client, p *store.PendingSecret, now time.Time) (*store.PendingSecretWriteResult, error) {
	q := c.PendingSecret.Query().Where(
		entpendingsecret.KeyEQ(p.Key),
		entpendingsecret.ScopeEQ(p.Scope),
		entpendingsecret.ScopeIDEQ(p.ScopeID),
	)
	if isPostgres(c) {
		q = q.ForUpdate()
	}
	existing, err := q.Only(ctx)
	if err != nil && !ent.IsNotFound(err) {
		return nil, mapError(err)
	}

	res := &store.PendingSecretWriteResult{}
	created := now
	expiresAt := now.Add(store.PendingSecretTTL)
	if existing != nil {
		switch {
		case !now.Before(existing.ExpiresAt):
			// An expired row is gone: it never blocks a new write.
		case existing.AgentID != p.AgentID:
			return nil, store.ErrPendingSecretHeldByOtherAgent
		default:
			res.Replaced = true
			created = existing.Created
			expiresAt = existing.ExpiresAt
		}
		if err := c.PendingSecret.DeleteOneID(existing.ID).Exec(ctx); err != nil {
			return nil, mapError(err)
		}
		res.DroppedValueRefs = append(res.DroppedValueRefs, existing.ValueRef)
	}

	id := uuid.New()
	if p.ID != "" {
		id, err = parseUUID(p.ID)
		if err != nil {
			return nil, err
		}
	}
	secretType := p.SecretType
	if secretType == "" {
		secretType = store.SecretTypeEnvironment
	}
	injectionMode := p.InjectionMode
	if injectionMode == "" {
		injectionMode = store.InjectionModeAsNeeded
	}
	target := p.Target
	if target == "" {
		target = p.Key
	}

	create := c.PendingSecret.Create().
		SetID(id).
		SetKey(p.Key).
		SetSecretType(entpendingsecret.SecretType(secretType)).
		SetTarget(target).
		SetScope(p.Scope).
		SetScopeID(p.ScopeID).
		SetAllowProgeny(p.AllowProgeny).
		SetInjectionMode(entpendingsecret.InjectionMode(injectionMode)).
		SetDescription(p.Description).
		SetValueRef(p.ValueRef).
		SetValueLen(p.ValueLen).
		SetAgentID(p.AgentID).
		SetAgentSlug(p.AgentSlug).
		SetProjectID(p.ProjectID).
		SetProjectName(p.ProjectName).
		SetCreated(created).
		SetUpdated(now).
		SetExpiresAt(expiresAt)
	if p.ReplacesSecretID != "" {
		create = create.SetReplacesSecretID(p.ReplacesSecretID).SetReplacesVersion(p.ReplacesVersion)
	}
	row, err := create.Save(ctx)
	if err != nil {
		return nil, mapError(err)
	}
	stored := entPendingSecretToStore(row, true)
	res.Secret = &stored
	return res, nil
}

// GetPendingSecret implements store.PendingSecretStore.
func (s *PendingSecretStore) GetPendingSecret(ctx context.Context, id string) (*store.PendingSecret, error) {
	uid, err := uuid.Parse(id)
	if err != nil {
		// A malformed ID names no row.
		return nil, store.ErrNotFound
	}
	row, err := s.client.PendingSecret.Get(ctx, uid)
	if err != nil {
		return nil, mapError(err)
	}
	p := entPendingSecretToStore(row, true)
	return &p, nil
}

// ListPendingSecretsByOwner implements store.PendingSecretStore.
func (s *PendingSecretStore) ListPendingSecretsByOwner(ctx context.Context, scope, scopeID string) ([]store.PendingSecret, error) {
	rows, err := s.client.PendingSecret.Query().
		Where(
			entpendingsecret.ScopeEQ(scope),
			entpendingsecret.ScopeIDEQ(scopeID),
		).
		Order(ent.Asc(entpendingsecret.FieldKey)).
		All(ctx)
	if err != nil {
		return nil, mapError(err)
	}
	out := make([]store.PendingSecret, 0, len(rows))
	for _, row := range rows {
		out = append(out, entPendingSecretToStore(row, false))
	}
	return out, nil
}

// DeletePendingSecret implements store.PendingSecretStore.
func (s *PendingSecretStore) DeletePendingSecret(ctx context.Context, id string) error {
	uid, err := uuid.Parse(id)
	if err != nil {
		return store.ErrNotFound
	}
	n, err := s.client.PendingSecret.Delete().Where(entpendingsecret.IDEQ(uid)).Exec(ctx)
	if err != nil {
		return mapError(err)
	}
	if n == 0 {
		return store.ErrNotFound
	}
	return nil
}
