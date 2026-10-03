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

	"github.com/GoogleCloudPlatform/scion/pkg/ent"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/externalidentity"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
)

// ExternalIdentityStore implements store.ExternalIdentityStore using Ent ORM.
// Bindings are durably stored in the external_identities table with a unique
// composite index on (provider, issuer, subject), ensuring that a given
// external identity maps to exactly one local user even under concurrent
// binding creation from multiple Hub instances.
type ExternalIdentityStore struct {
	client *ent.Client
}

// NewExternalIdentityStore creates a new Ent-backed ExternalIdentityStore.
func NewExternalIdentityStore(client *ent.Client) *ExternalIdentityStore {
	return &ExternalIdentityStore{client: client}
}

// entExternalIdentityToStore converts an Ent ExternalIdentity to a store model.
func entExternalIdentityToStore(e *ent.ExternalIdentity) *store.ExternalIdentityBinding {
	return &store.ExternalIdentityBinding{
		ID:        e.ID.String(),
		Provider:  e.Provider,
		Issuer:    e.Issuer,
		Subject:   e.Subject,
		UserID:    e.UserID.String(),
		Email:     e.Email,
		CreatedAt: e.CreatedAt,
		UpdatedAt: e.UpdatedAt,
	}
}

// GetExternalIdentity looks up a binding by (provider, issuer, subject).
// Returns store.ErrNotFound if no binding exists.
func (s *ExternalIdentityStore) GetExternalIdentity(ctx context.Context, provider, issuer, subject string) (*store.ExternalIdentityBinding, error) {
	e, err := s.client.ExternalIdentity.Query().
		Where(
			externalidentity.ProviderEQ(provider),
			externalidentity.IssuerEQ(issuer),
			externalidentity.SubjectEQ(subject),
		).
		Only(ctx)
	if err != nil {
		if ent.IsNotFound(err) {
			return nil, store.ErrNotFound
		}
		return nil, fmt.Errorf("query external identity: %w", err)
	}
	return entExternalIdentityToStore(e), nil
}

// CreateExternalIdentity atomically creates a new binding. The unique composite
// index on (provider, issuer, subject) ensures that concurrent creation
// attempts for the same external identity will fail with a constraint violation,
// preventing duplicate bindings across Hub replicas.
func (s *ExternalIdentityStore) CreateExternalIdentity(ctx context.Context, binding *store.ExternalIdentityBinding) error {
	userID, err := uuid.Parse(binding.UserID)
	if err != nil {
		return fmt.Errorf("invalid user ID %q: %w", binding.UserID, err)
	}

	// Use the provided ID if set, otherwise generate a new one.
	var bindingID uuid.UUID
	if binding.ID != "" {
		bindingID, err = uuid.Parse(binding.ID)
		if err != nil {
			return fmt.Errorf("invalid binding ID %q: %w", binding.ID, err)
		}
	} else {
		bindingID = uuid.New()
	}

	_, err = s.client.ExternalIdentity.Create().
		SetID(bindingID).
		SetProvider(binding.Provider).
		SetIssuer(binding.Issuer).
		SetSubject(binding.Subject).
		SetUserID(userID).
		SetEmail(binding.Email).
		Save(ctx)
	if err != nil {
		if ent.IsConstraintError(err) {
			return fmt.Errorf("external identity binding already exists for %s:%s:%s: %w",
				binding.Provider, binding.Issuer, binding.Subject, err)
		}
		return fmt.Errorf("create external identity: %w", err)
	}

	return nil
}

// UpdateExternalIdentityEmail updates the email field of an existing binding.
func (s *ExternalIdentityStore) UpdateExternalIdentityEmail(ctx context.Context, id, email string) error {
	uid, err := uuid.Parse(id)
	if err != nil {
		return fmt.Errorf("invalid binding ID %q: %w", id, err)
	}

	n, err := s.client.ExternalIdentity.Update().
		Where(externalidentity.IDEQ(uid)).
		SetEmail(email).
		Save(ctx)
	if err != nil {
		return fmt.Errorf("update external identity email: %w", err)
	}
	if n == 0 {
		return store.ErrNotFound
	}
	return nil
}

// GetExternalIdentitiesByUserID returns all bindings for a given user.
func (s *ExternalIdentityStore) GetExternalIdentitiesByUserID(ctx context.Context, userID string) ([]*store.ExternalIdentityBinding, error) {
	uid, err := uuid.Parse(userID)
	if err != nil {
		return nil, fmt.Errorf("invalid user ID %q: %w", userID, err)
	}

	entities, err := s.client.ExternalIdentity.Query().
		Where(externalidentity.UserIDEQ(uid)).
		All(ctx)
	if err != nil {
		return nil, fmt.Errorf("query external identities by user: %w", err)
	}

	result := make([]*store.ExternalIdentityBinding, len(entities))
	for i, e := range entities {
		result[i] = entExternalIdentityToStore(e)
	}
	return result, nil
}
