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

package secret

import (
	"context"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// LocalBackend implements SecretBackend using the local store.SecretStore.
// Values are stored directly in the Hub database.
type LocalBackend struct {
	store store.SecretStore
	hubID string
}

// NewLocalBackend creates a LocalBackend wrapping the given SecretStore.
func NewLocalBackend(s store.SecretStore, hubID string) *LocalBackend {
	return &LocalBackend{store: s, hubID: hubID}
}

// HubID returns the hub instance ID used for hub-scoped secret namespacing.
func (b *LocalBackend) HubID() string {
	return b.hubID
}

func (b *LocalBackend) Get(ctx context.Context, name, scope, scopeID string) (*SecretWithValue, error) {
	s, err := b.store.GetSecret(ctx, name, scope, scopeID)
	if err != nil {
		return nil, err
	}
	return fromStoreSecretWithValue(s), nil
}

func (b *LocalBackend) Set(ctx context.Context, input *SetSecretInput) (bool, *SecretMeta, error) {
	s := toStoreSecret(input)
	created, err := b.store.UpsertSecret(ctx, s)
	if err != nil {
		return false, nil, err
	}
	// Re-read the stored secret to get server-assigned fields (version, timestamps).
	stored, err := b.store.GetSecret(ctx, input.Name, input.Scope, input.ScopeID)
	if err != nil {
		return created, nil, err
	}
	return created, fromStoreSecretMeta(stored), nil
}

func (b *LocalBackend) Delete(ctx context.Context, name, scope, scopeID string) error {
	return b.store.DeleteSecret(ctx, name, scope, scopeID)
}

func (b *LocalBackend) List(ctx context.Context, filter Filter) ([]SecretMeta, error) {
	secrets, err := b.store.ListSecrets(ctx, toStoreFilter(filter))
	if err != nil {
		return nil, err
	}
	result := make([]SecretMeta, len(secrets))
	for i, s := range secrets {
		result[i] = *fromStoreSecretMeta(&s)
	}
	return result, nil
}

func (b *LocalBackend) GetMeta(ctx context.Context, name, scope, scopeID string) (*SecretMeta, error) {
	s, err := b.store.GetSecret(ctx, name, scope, scopeID)
	if err != nil {
		return nil, err
	}
	return fromStoreSecretMeta(s), nil
}

func (b *LocalBackend) Resolve(ctx context.Context, userID, projectID, brokerID string, opts *ResolveOpts) ([]SecretWithValue, error) {
	merged := make(map[string]SecretWithValue)

	type scopeEntry struct {
		scope   string
		scopeID string
	}

	var scopes []scopeEntry
	// Hub scope is always included as lowest precedence
	scopes = append(scopes, scopeEntry{scope: store.ScopeHub, scopeID: b.hubID})
	if userID != "" {
		scopes = append(scopes, scopeEntry{scope: store.ScopeUser, scopeID: userID})
	}
	if projectID != "" {
		scopes = append(scopes, scopeEntry{scope: store.ScopeProject, scopeID: projectID})
	}
	if brokerID != "" {
		scopes = append(scopes, scopeEntry{scope: store.ScopeRuntimeBroker, scopeID: brokerID})
	}

	for _, sc := range scopes {
		secrets, err := b.store.ListSecrets(ctx, store.SecretFilter{
			Scope:   sc.scope,
			ScopeID: sc.scopeID,
		})
		if err != nil {
			return nil, err
		}

		for _, s := range secrets {
			// Never project hub-internal infrastructure secrets (e.g. signing keys)
			// into agent environments.
			if s.SecretType == store.SecretTypeInternal {
				continue
			}

			value, err := b.store.GetSecretValue(ctx, s.Key, sc.scope, sc.scopeID)
			if err != nil {
				continue
			}

			secretType := s.SecretType
			if secretType == "" {
				secretType = store.SecretTypeEnvironment
			}
			target := s.Target
			if target == "" {
				target = s.Key
			}

			merged[s.Key] = SecretWithValue{
				SecretMeta: SecretMeta{
					ID:            s.ID,
					Name:          s.Key,
					SecretType:    secretType,
					Target:        target,
					Scope:         sc.scope,
					ScopeID:       sc.scopeID,
					Description:   s.Description,
					InjectionMode: s.InjectionMode,
					SecretRef:     s.SecretRef,
					AllowProgeny:  s.AllowProgeny,
					Version:       s.Version,
					Created:       s.Created,
					Updated:       s.Updated,
					CreatedBy:     s.CreatedBy,
					UpdatedBy:     s.UpdatedBy,
				},
				Value: value,
			}
		}
	}

	// Progeny secret resolution: when the caller is an agent with ancestry,
	// include user-scoped secrets marked allowProgeny whose creator is in the
	// ancestry chain. These are added at user-scope precedence — grove/broker
	// secrets with the same key will already have overridden them.
	if opts != nil && len(opts.AgentAncestry) > 0 {
		progenySecrets, err := b.store.ListProgenySecrets(ctx, opts.AgentAncestry)
		if err != nil {
			return nil, err
		}
		for _, s := range progenySecrets {
			// Skip if a higher-precedence scope already set this key
			if _, exists := merged[s.Key]; exists {
				continue
			}
			// Skip internal secrets
			if s.SecretType == store.SecretTypeInternal {
				continue
			}

			meta := fromStoreSecretMeta(&s)

			// Verify access via policy engine if checker is provided
			if opts.AuthzCheck != nil && !opts.AuthzCheck(*meta) {
				continue
			}

			value, err := b.store.GetSecretValue(ctx, s.Key, s.Scope, s.ScopeID)
			if err != nil {
				continue
			}

			secretType := s.SecretType
			if secretType == "" {
				secretType = store.SecretTypeEnvironment
			}
			target := s.Target
			if target == "" {
				target = s.Key
			}

			merged[s.Key] = SecretWithValue{
				SecretMeta: SecretMeta{
					ID:            s.ID,
					Name:          s.Key,
					SecretType:    secretType,
					Target:        target,
					Scope:         s.Scope,
					ScopeID:       s.ScopeID,
					Description:   s.Description,
					InjectionMode: s.InjectionMode,
					SecretRef:     s.SecretRef,
					AllowProgeny:  s.AllowProgeny,
					Version:       s.Version,
					Created:       s.Created,
					Updated:       s.Updated,
					CreatedBy:     s.CreatedBy,
					UpdatedBy:     s.UpdatedBy,
				},
				Value: value,
			}
		}
	}

	result := make([]SecretWithValue, 0, len(merged))
	for _, sv := range merged {
		result = append(result, sv)
	}
	return DeduplicateByTarget(result), nil
}

// toStoreSecret converts a SetSecretInput to a store.Secret.
func toStoreSecret(input *SetSecretInput) *store.Secret {
	secretType := input.SecretType
	if secretType == "" {
		secretType = store.SecretTypeEnvironment
	}
	target := input.Target
	if target == "" {
		target = input.Name
	}
	injectionMode := input.InjectionMode
	if injectionMode == "" {
		injectionMode = store.InjectionModeAsNeeded
	}
	return &store.Secret{
		ID:             api.NewUUID(),
		Key:            input.Name,
		EncryptedValue: input.Value,
		SecretType:     secretType,
		Target:         target,
		Scope:          input.Scope,
		ScopeID:        input.ScopeID,
		Description:    input.Description,
		InjectionMode:  injectionMode,
		AllowProgeny:   input.AllowProgeny,
		CreatedBy:      input.CreatedBy,
		UpdatedBy:      input.UpdatedBy,
	}
}

// toStoreFilter converts a secret.Filter to a store.SecretFilter.
func toStoreFilter(f Filter) store.SecretFilter {
	return store.SecretFilter{
		Scope:   f.Scope,
		ScopeID: f.ScopeID,
		Key:     f.Name,
		Type:    f.Type,
	}
}

// fromStoreSecretMeta converts a store.Secret to a SecretMeta.
func fromStoreSecretMeta(s *store.Secret) *SecretMeta {
	return &SecretMeta{
		ID:            s.ID,
		Name:          s.Key,
		SecretType:    s.SecretType,
		Target:        s.Target,
		Scope:         s.Scope,
		ScopeID:       s.ScopeID,
		Description:   s.Description,
		InjectionMode: s.InjectionMode,
		SecretRef:     s.SecretRef,
		AllowProgeny:  s.AllowProgeny,
		Version:       s.Version,
		Created:       s.Created,
		Updated:       s.Updated,
		CreatedBy:     s.CreatedBy,
		UpdatedBy:     s.UpdatedBy,
	}
}

// fromStoreSecretWithValue converts a store.Secret (with EncryptedValue) to SecretWithValue.
func fromStoreSecretWithValue(s *store.Secret) *SecretWithValue {
	return &SecretWithValue{
		SecretMeta: *fromStoreSecretMeta(s),
		Value:      s.EncryptedValue,
	}
}
