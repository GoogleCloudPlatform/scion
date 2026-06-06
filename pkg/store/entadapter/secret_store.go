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

	"github.com/GoogleCloudPlatform/scion/pkg/ent"
	entenvvar "github.com/GoogleCloudPlatform/scion/pkg/ent/envvar"
	entsecret "github.com/GoogleCloudPlatform/scion/pkg/ent/secret"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// SecretStore implements store.SecretStore and store.EnvVarStore using Ent ORM.
//
// Secrets and env vars are polymorphically scoped to different entity types
// (hub/user/project/runtime_broker) via the (scope, scope_id) pair, so there
// are no FK edges; lookups are keyed by the (key, scope, scope_id) triple,
// mirroring the legacy SQLite implementation.
type SecretStore struct {
	client *ent.Client
}

// NewSecretStore creates a new Ent-backed SecretStore.
func NewSecretStore(client *ent.Client) *SecretStore {
	return &SecretStore{client: client}
}

// =============================================================================
// Secret operations
// =============================================================================

// entSecretRowToStore converts an Ent Secret entity to a store.Secret model.
// When includeValue is false the EncryptedValue is left empty, matching the
// SQLite listing queries that never select the encrypted payload.
func entSecretRowToStore(e *ent.Secret, includeValue bool) store.Secret {
	s := store.Secret{
		ID:            e.ID.String(),
		Key:           e.Key,
		SecretRef:     e.SecretRef,
		SecretType:    string(e.SecretType),
		Target:        e.Target,
		Scope:         e.Scope,
		ScopeID:       e.ScopeID,
		Description:   e.Description,
		InjectionMode: string(e.InjectionMode),
		AllowProgeny:  e.AllowProgeny,
		Version:       e.Version,
		Created:       e.Created,
		Updated:       e.Updated,
		CreatedBy:     e.CreatedBy,
		UpdatedBy:     e.UpdatedBy,
	}
	// Mirror SQLite's COALESCE(target, key): an unset target projects to the key.
	if s.Target == "" {
		s.Target = s.Key
	}
	if includeValue {
		s.EncryptedValue = e.EncryptedValue
	}
	return s
}

// CreateSecret creates a new secret.
func (s *SecretStore) CreateSecret(ctx context.Context, secret *store.Secret) error {
	uid, err := parseUUID(secret.ID)
	if err != nil {
		return err
	}

	now := time.Now()
	secret.Created = now
	secret.Updated = now
	secret.Version = 1

	if secret.SecretType == "" {
		secret.SecretType = store.SecretTypeEnvironment
	}
	if secret.Target == "" {
		secret.Target = secret.Key
	}
	if secret.InjectionMode == "" {
		secret.InjectionMode = store.InjectionModeAsNeeded
	}

	create := s.client.Secret.Create().
		SetID(uid).
		SetKey(secret.Key).
		SetEncryptedValue(secret.EncryptedValue).
		SetSecretType(entsecret.SecretType(secret.SecretType)).
		SetTarget(secret.Target).
		SetScope(secret.Scope).
		SetScopeID(secret.ScopeID).
		SetInjectionMode(entsecret.InjectionMode(secret.InjectionMode)).
		SetAllowProgeny(secret.AllowProgeny).
		SetVersion(secret.Version).
		SetCreated(secret.Created).
		SetUpdated(secret.Updated)

	if secret.SecretRef != "" {
		create.SetSecretRef(secret.SecretRef)
	}
	if secret.Description != "" {
		create.SetDescription(secret.Description)
	}
	if secret.CreatedBy != "" {
		create.SetCreatedBy(secret.CreatedBy)
	}
	if secret.UpdatedBy != "" {
		create.SetUpdatedBy(secret.UpdatedBy)
	}

	if _, err := create.Save(ctx); err != nil {
		return mapError(err)
	}
	return nil
}

// GetSecret retrieves secret metadata (including the encrypted value) by key,
// scope, and scopeID.
func (s *SecretStore) GetSecret(ctx context.Context, key, scope, scopeID string) (*store.Secret, error) {
	e, err := s.client.Secret.Query().
		Where(
			entsecret.KeyEQ(key),
			entsecret.ScopeEQ(scope),
			entsecret.ScopeIDEQ(scopeID),
		).
		Only(ctx)
	if err != nil {
		return nil, mapError(err)
	}
	sec := entSecretRowToStore(e, true)
	return &sec, nil
}

// UpdateSecret updates an existing secret, incrementing its version.
func (s *SecretStore) UpdateSecret(ctx context.Context, secret *store.Secret) error {
	secret.Updated = time.Now()
	secret.Version++ // Increment version on each update

	if secret.SecretType == "" {
		secret.SecretType = store.SecretTypeEnvironment
	}
	if secret.Target == "" {
		secret.Target = secret.Key
	}
	if secret.InjectionMode == "" {
		secret.InjectionMode = store.InjectionModeAsNeeded
	}

	update := s.client.Secret.Update().
		Where(
			entsecret.KeyEQ(secret.Key),
			entsecret.ScopeEQ(secret.Scope),
			entsecret.ScopeIDEQ(secret.ScopeID),
		).
		SetEncryptedValue(secret.EncryptedValue).
		SetSecretType(entsecret.SecretType(secret.SecretType)).
		SetTarget(secret.Target).
		SetDescription(secret.Description).
		SetInjectionMode(entsecret.InjectionMode(secret.InjectionMode)).
		SetAllowProgeny(secret.AllowProgeny).
		SetVersion(secret.Version).
		SetUpdatedBy(secret.UpdatedBy).
		SetUpdated(secret.Updated)

	if secret.SecretRef != "" {
		update.SetSecretRef(secret.SecretRef)
	} else {
		update.ClearSecretRef()
	}

	n, err := update.Save(ctx)
	if err != nil {
		return mapError(err)
	}
	if n == 0 {
		return store.ErrNotFound
	}
	return nil
}

// UpsertSecret creates or updates a secret, keyed by (key, scope, scopeID).
func (s *SecretStore) UpsertSecret(ctx context.Context, secret *store.Secret) (bool, error) {
	now := time.Now()
	secret.Updated = now

	existing, err := s.GetSecret(ctx, secret.Key, secret.Scope, secret.ScopeID)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return false, err
	}

	if existing != nil {
		// Update existing: preserve identity/creation metadata. UpdateSecret
		// increments the version from the existing baseline.
		secret.ID = existing.ID
		secret.Created = existing.Created
		secret.CreatedBy = existing.CreatedBy
		secret.Version = existing.Version
		if err := s.UpdateSecret(ctx, secret); err != nil {
			return false, err
		}
		return false, nil
	}

	secret.Created = now
	if err := s.CreateSecret(ctx, secret); err != nil {
		return false, err
	}
	return true, nil
}

// DeleteSecret removes a secret by key, scope, and scopeID.
func (s *SecretStore) DeleteSecret(ctx context.Context, key, scope, scopeID string) error {
	n, err := s.client.Secret.Delete().
		Where(
			entsecret.KeyEQ(key),
			entsecret.ScopeEQ(scope),
			entsecret.ScopeIDEQ(scopeID),
		).
		Exec(ctx)
	if err != nil {
		return err
	}
	if n == 0 {
		return store.ErrNotFound
	}
	return nil
}

// DeleteSecretsByScope removes all secrets for a given scope, returning the
// number of deleted records.
func (s *SecretStore) DeleteSecretsByScope(ctx context.Context, scope, scopeID string) (int, error) {
	n, err := s.client.Secret.Delete().
		Where(
			entsecret.ScopeEQ(scope),
			entsecret.ScopeIDEQ(scopeID),
		).
		Exec(ctx)
	if err != nil {
		return 0, err
	}
	return n, nil
}

// ListSecrets returns secret metadata matching the filter. The EncryptedValue
// is never populated.
func (s *SecretStore) ListSecrets(ctx context.Context, filter store.SecretFilter) ([]store.Secret, error) {
	query := s.client.Secret.Query()

	if filter.Scope != "" {
		query.Where(entsecret.ScopeEQ(filter.Scope))
	}
	if filter.ScopeID != "" {
		query.Where(entsecret.ScopeIDEQ(filter.ScopeID))
	}
	if filter.Key != "" {
		query.Where(entsecret.KeyEQ(filter.Key))
	}
	if filter.Type != "" {
		query.Where(entsecret.SecretTypeEQ(entsecret.SecretType(filter.Type)))
	}

	rows, err := query.Order(entsecret.ByKey()).All(ctx)
	if err != nil {
		return nil, err
	}

	secrets := make([]store.Secret, 0, len(rows))
	for _, e := range rows {
		secrets = append(secrets, entSecretRowToStore(e, false))
	}
	return secrets, nil
}

// ListProgenySecrets returns user-scoped secrets with allowProgeny=true whose
// createdBy is in the given set of ancestor IDs. The EncryptedValue is never
// populated. This preserves the progeny-inheritance semantics of the legacy
// SQLite query via an IN-list over created_by.
func (s *SecretStore) ListProgenySecrets(ctx context.Context, ancestorIDs []string) ([]store.Secret, error) {
	if len(ancestorIDs) == 0 {
		return nil, nil
	}

	rows, err := s.client.Secret.Query().
		Where(
			entsecret.ScopeEQ(store.ScopeUser),
			entsecret.AllowProgenyEQ(true),
			entsecret.CreatedByIn(ancestorIDs...),
		).
		Order(entsecret.ByKey()).
		All(ctx)
	if err != nil {
		return nil, err
	}

	secrets := make([]store.Secret, 0, len(rows))
	for _, e := range rows {
		secrets = append(secrets, entSecretRowToStore(e, false))
	}
	return secrets, nil
}

// GetSecretValue retrieves the encrypted value of a secret.
func (s *SecretStore) GetSecretValue(ctx context.Context, key, scope, scopeID string) (string, error) {
	e, err := s.client.Secret.Query().
		Where(
			entsecret.KeyEQ(key),
			entsecret.ScopeEQ(scope),
			entsecret.ScopeIDEQ(scopeID),
		).
		Only(ctx)
	if err != nil {
		return "", mapError(err)
	}
	return e.EncryptedValue, nil
}

// =============================================================================
// EnvVar operations
// =============================================================================

// entEnvVarToStore converts an Ent EnvVar entity to a store.EnvVar model.
func entEnvVarToStore(e *ent.EnvVar) store.EnvVar {
	return store.EnvVar{
		ID:            e.ID.String(),
		Key:           e.Key,
		Value:         e.Value,
		Scope:         e.Scope,
		ScopeID:       e.ScopeID,
		Description:   e.Description,
		Sensitive:     e.Sensitive,
		InjectionMode: string(e.InjectionMode),
		Secret:        e.Secret,
		Created:       e.Created,
		Updated:       e.Updated,
		CreatedBy:     e.CreatedBy,
	}
}

// CreateEnvVar creates a new environment variable.
func (s *SecretStore) CreateEnvVar(ctx context.Context, envVar *store.EnvVar) error {
	uid, err := parseUUID(envVar.ID)
	if err != nil {
		return err
	}

	now := time.Now()
	envVar.Created = now
	envVar.Updated = now

	// The Ent enum column rejects empty values; normalize to the default the
	// legacy schema applied (as_needed) and reflect it back on the model.
	if envVar.InjectionMode == "" {
		envVar.InjectionMode = store.InjectionModeAsNeeded
	}

	create := s.client.EnvVar.Create().
		SetID(uid).
		SetKey(envVar.Key).
		SetValue(envVar.Value).
		SetScope(envVar.Scope).
		SetScopeID(envVar.ScopeID).
		SetSensitive(envVar.Sensitive).
		SetInjectionMode(entenvvar.InjectionMode(envVar.InjectionMode)).
		SetSecret(envVar.Secret).
		SetCreated(envVar.Created).
		SetUpdated(envVar.Updated)

	if envVar.Description != "" {
		create.SetDescription(envVar.Description)
	}
	if envVar.CreatedBy != "" {
		create.SetCreatedBy(envVar.CreatedBy)
	}

	if _, err := create.Save(ctx); err != nil {
		return mapError(err)
	}
	return nil
}

// GetEnvVar retrieves an environment variable by key, scope, and scopeID.
func (s *SecretStore) GetEnvVar(ctx context.Context, key, scope, scopeID string) (*store.EnvVar, error) {
	e, err := s.client.EnvVar.Query().
		Where(
			entenvvar.KeyEQ(key),
			entenvvar.ScopeEQ(scope),
			entenvvar.ScopeIDEQ(scopeID),
		).
		Only(ctx)
	if err != nil {
		return nil, mapError(err)
	}
	ev := entEnvVarToStore(e)
	return &ev, nil
}

// UpdateEnvVar updates an existing environment variable.
func (s *SecretStore) UpdateEnvVar(ctx context.Context, envVar *store.EnvVar) error {
	envVar.Updated = time.Now()

	if envVar.InjectionMode == "" {
		envVar.InjectionMode = store.InjectionModeAsNeeded
	}

	n, err := s.client.EnvVar.Update().
		Where(
			entenvvar.KeyEQ(envVar.Key),
			entenvvar.ScopeEQ(envVar.Scope),
			entenvvar.ScopeIDEQ(envVar.ScopeID),
		).
		SetValue(envVar.Value).
		SetDescription(envVar.Description).
		SetSensitive(envVar.Sensitive).
		SetInjectionMode(entenvvar.InjectionMode(envVar.InjectionMode)).
		SetSecret(envVar.Secret).
		SetUpdated(envVar.Updated).
		Save(ctx)
	if err != nil {
		return mapError(err)
	}
	if n == 0 {
		return store.ErrNotFound
	}
	return nil
}

// UpsertEnvVar creates or updates an environment variable, keyed by
// (key, scope, scopeID).
func (s *SecretStore) UpsertEnvVar(ctx context.Context, envVar *store.EnvVar) (bool, error) {
	now := time.Now()
	envVar.Updated = now

	existing, err := s.GetEnvVar(ctx, envVar.Key, envVar.Scope, envVar.ScopeID)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return false, err
	}

	if existing != nil {
		envVar.ID = existing.ID
		envVar.Created = existing.Created
		envVar.CreatedBy = existing.CreatedBy
		if err := s.UpdateEnvVar(ctx, envVar); err != nil {
			return false, err
		}
		return false, nil
	}

	envVar.Created = now
	if err := s.CreateEnvVar(ctx, envVar); err != nil {
		return false, err
	}
	return true, nil
}

// DeleteEnvVar removes an environment variable by key, scope, and scopeID.
func (s *SecretStore) DeleteEnvVar(ctx context.Context, key, scope, scopeID string) error {
	n, err := s.client.EnvVar.Delete().
		Where(
			entenvvar.KeyEQ(key),
			entenvvar.ScopeEQ(scope),
			entenvvar.ScopeIDEQ(scopeID),
		).
		Exec(ctx)
	if err != nil {
		return err
	}
	if n == 0 {
		return store.ErrNotFound
	}
	return nil
}

// DeleteEnvVarsByScope removes all environment variables for a given scope,
// returning the number of deleted records.
func (s *SecretStore) DeleteEnvVarsByScope(ctx context.Context, scope, scopeID string) (int, error) {
	n, err := s.client.EnvVar.Delete().
		Where(
			entenvvar.ScopeEQ(scope),
			entenvvar.ScopeIDEQ(scopeID),
		).
		Exec(ctx)
	if err != nil {
		return 0, err
	}
	return n, nil
}

// ListEnvVars returns environment variables matching the filter, ordered by key.
func (s *SecretStore) ListEnvVars(ctx context.Context, filter store.EnvVarFilter) ([]store.EnvVar, error) {
	query := s.client.EnvVar.Query()

	if filter.Scope != "" {
		query.Where(entenvvar.ScopeEQ(filter.Scope))
	}
	if filter.ScopeID != "" {
		query.Where(entenvvar.ScopeIDEQ(filter.ScopeID))
	}
	if filter.Key != "" {
		query.Where(entenvvar.KeyEQ(filter.Key))
	}

	rows, err := query.Order(entenvvar.ByKey()).All(ctx)
	if err != nil {
		return nil, err
	}

	envVars := make([]store.EnvVar, 0, len(rows))
	for _, e := range rows {
		envVars = append(envVars, entEnvVarToStore(e))
	}
	return envVars, nil
}
