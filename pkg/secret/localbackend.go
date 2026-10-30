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
	"errors"
	"fmt"
	"log/slog"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// LocalBackend implements SecretBackend using the local store.SecretStore.
// Values are encrypted at rest by the shared ValueCipher (ptone/scion#1530
// §5) before they are written to the Hub database: AES-256-GCM in the
// enc:v1: or enc:v2:<kid>: envelope the cipher's ring selects.
type LocalBackend struct {
	store  store.SecretStore
	hubID  string
	cipher *ValueCipher // the shared cipher; nil fails closed
}

// NewLocalBackend creates a LocalBackend wrapping the given SecretStore,
// with the legacy-mode cipher over sharedSecret (NewLegacyValueCipher).
// When sharedSecret is empty, encryption is disabled and values are stored
// as plaintext (with a warning). Production wiring shares one cipher with
// the Hub through NewLocalBackendWithCipher.
func NewLocalBackend(s store.SecretStore, hubID, sharedSecret string) *LocalBackend {
	return NewLocalBackendWithCipher(s, hubID, NewLegacyValueCipher(sharedSecret))
}

// NewLocalBackendWithCipher creates a LocalBackend that encrypts with c,
// the server's one shared ValueCipher. A nil c fails closed: every value
// operation returns ErrCipherNotConfigured (the disabled plaintext mode is
// only NewLegacyValueCipher("")).
func NewLocalBackendWithCipher(s store.SecretStore, hubID string, c *ValueCipher) *LocalBackend {
	if c != nil && !c.Enabled() {
		slog.Warn("local secret backend: no shared signing secret configured; " +
			"secret values will be stored WITHOUT encryption")
	}
	return &LocalBackend{store: s, hubID: hubID, cipher: c}
}

// Cipher returns the backend's ValueCipher (for identity checks: the Hub
// and the backend must share one object).
func (b *LocalBackend) Cipher() *ValueCipher { return b.cipher }

// HubID returns the hub instance ID used for hub-scoped secret namespacing.
func (b *LocalBackend) HubID() string {
	return b.hubID
}

func (b *LocalBackend) Get(ctx context.Context, name, scope, scopeID string) (*SecretWithValue, error) {
	s, err := b.store.GetSecret(ctx, name, scope, scopeID)
	if err != nil {
		return nil, err
	}
	return b.decryptStoreSecret(s)
}

// FetchValues returns values for exactly the given metadata records, matched
// by ID, Version, AllowProgeny, CreatedBy and SecretType, keyed by each
// record's ID in the returned map. There is no name-based fallback: a record
// that is no longer in the store, or whose current ID, Version, AllowProgeny,
// CreatedBy or SecretType no longer matches the recorded metadata, is
// reported as store.ErrNotFound for that item. A decrypt failure is also
// reported as a per-item error, never as an empty value delivered in place
// of an error. A record whose current SecretType is internal is refused with
// store.ErrNotFound, since internal secrets are never candidates for
// delivery. The returned outer error reports only a failure of the whole
// call, not a per-item failure.
//
// store.SecretStore has no primary-key lookup, so each record is located by
// its Name/Scope/ScopeID triple — the same composite key GetSecret uses —
// and then verified against the recorded metadata before its value is
// decrypted (see recordGenerationChanged). Name/Scope/ScopeID are immutable
// for the life of a record (UpdateSecretMeta never changes them); a secret
// that was deleted and recreated under the same triple gets a new ID
// (toStoreSecret assigns api.NewUUID() on every create), so this
// verification rejects the new record exactly as a primary-key lookup would.
// The additional AllowProgeny/CreatedBy/SecretType comparison catches a
// same-Version metadata race that ID+Version alone would miss (see
// recordGenerationChanged's doc comment).
func (b *LocalBackend) FetchValues(ctx context.Context, metas []SecretMeta) (map[string]FetchResult, error) {
	results := make(map[string]FetchResult, len(metas))
	for _, meta := range metas {
		results[meta.ID] = b.fetchValue(ctx, meta)
	}
	return results, nil
}

func (b *LocalBackend) fetchValue(ctx context.Context, meta SecretMeta) FetchResult {
	s, err := b.store.GetSecret(ctx, meta.Name, meta.Scope, meta.ScopeID)
	if err != nil {
		return FetchResult{Err: err}
	}
	if recordGenerationChanged(s, meta) {
		// The record has been replaced, rotated or reclassified since the
		// caller's metadata was recorded; treat it the same as not found
		// rather than returning a value for a different record generation.
		return FetchResult{Err: store.ErrNotFound}
	}
	sv, err := b.decryptStoreSecret(s)
	if err != nil {
		// The typed error Resolve returns for an undecryptable value
		// (errors.Is ErrResolveDecrypt), naming the key and scope only.
		return FetchResult{Err: newResolveDecryptError([]ResolveFailure{{Key: s.Key, Scope: s.Scope, Kind: DecryptFailureKind(err)}})}
	}
	// The stored ref verbatim, as Resolve reports it.
	return FetchResult{Value: sv.Value, Ref: s.SecretRef}
}

func (b *LocalBackend) Set(ctx context.Context, input *SetSecretInput) (bool, *SecretMeta, error) {
	s := toStoreSecret(input)

	// Encrypt the value before persisting. The disabled cipher (legacy dev
	// mode, no server secret) stores the plaintext as-is. A write the
	// database encryption policy refuses returns
	// store.ErrEncryptionPolicyMismatch, never an unencrypted fallback.
	encrypted, err := b.cipher.Encrypt(s.EncryptedValue)
	if err != nil {
		return false, nil, fmt.Errorf("encrypting secret value: %w", err)
	}
	s.EncryptedValue = encrypted

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

func (b *LocalBackend) UpdateMeta(ctx context.Context, input *UpdateMetaInput) (*SecretMeta, error) {
	meta := &store.SecretMetaUpdate{
		Description:   input.Description,
		InjectionMode: input.InjectionMode,
		SecretType:    input.SecretType,
		Target:        input.Target,
		AllowProgeny:  input.AllowProgeny,
		UpdatedBy:     input.UpdatedBy,
	}
	updated, err := b.store.UpdateSecretMeta(ctx, input.Name, input.Scope, input.ScopeID, meta)
	if err != nil {
		return nil, err
	}
	return fromStoreSecretMeta(updated), nil
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

	// Scope precedence, lowest first: runtime_broker < hub < project < user.
	// Later entries in this slice overwrite earlier ones in the merge loop
	// below, so this order must match envScopePrecedence in
	// pkg/hub/httpdispatcher.go — broker is the most infrastructural and
	// least specific of the four scopes, so it is intentionally the
	// weakest, not an override nobody can escape. (Previously this listed
	// hub, user, project, broker, which put broker last and therefore
	// strongest — the opposite of every other precedence-ordered resolver
	// in this codebase, and the reason a stale broker-scoped secret could
	// silently shadow a project-scoped one regardless of which was meant
	// to win.)
	// Decrypt failures are collected and fail the whole resolution
	// (ptone/scion#1530 §5): a value that cannot be read is never treated
	// as a missing secret, so no agent starts with an apparently complete
	// but partial set.
	var failures []ResolveFailure

	scopes := make([]scopeEntry, 0, 4)
	if brokerID != "" {
		scopes = append(scopes, scopeEntry{scope: store.ScopeRuntimeBroker, scopeID: brokerID})
	}
	scopes = append(scopes, scopeEntry{scope: store.ScopeHub, scopeID: b.hubID})
	if projectID != "" {
		scopes = append(scopes, scopeEntry{scope: store.ScopeProject, scopeID: projectID})
	}
	if userID != "" {
		scopes = append(scopes, scopeEntry{scope: store.ScopeUser, scopeID: userID})
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
			// The owner path: a user-scope row of userID is held to the
			// caller's UserScopeCheck before its value is read. A rejected
			// row is skipped, so a lower-scope row with the same key stays
			// in effect.
			if sc.scope == store.ScopeUser && opts != nil && opts.UserScopeCheck != nil && !opts.UserScopeCheck(*fromStoreSecretMeta(&s)) {
				continue
			}

			rawValue, err := b.store.GetSecretValue(ctx, s.Key, sc.scope, sc.scopeID)
			if errors.Is(err, store.ErrNotFound) {
				continue // deleted since it was listed
			}
			if err != nil {
				return nil, err
			}
			value, err := b.decryptPayload(rawValue, s.SecretRef)
			if err != nil {
				failures = append(failures, ResolveFailure{Key: s.Key, Scope: sc.scope, Kind: DecryptFailureKind(err)})
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
	// ancestry chain. These are added at user-scope precedence — project/broker
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

			// Verify access via the policy engine. With no checker configured,
			// a progeny secret is excluded rather than included by default:
			// the caller must supply an explicit policy decision before any
			// progeny value is read.
			if opts.AuthzCheck == nil || !opts.AuthzCheck(*meta) {
				continue
			}

			rawValue, err := b.store.GetSecretValue(ctx, s.Key, s.Scope, s.ScopeID)
			if errors.Is(err, store.ErrNotFound) {
				continue // deleted since it was listed
			}
			if err != nil {
				return nil, err
			}
			value, err := b.decryptPayload(rawValue, s.SecretRef)
			if err != nil {
				failures = append(failures, ResolveFailure{Key: s.Key, Scope: s.Scope, Kind: DecryptFailureKind(err)})
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

	if len(failures) > 0 {
		return nil, newResolveDecryptError(failures)
	}

	result := make([]SecretWithValue, 0, len(merged))
	for _, sv := range merged {
		result = append(result, sv)
	}
	return DeduplicateByTarget(result), nil
}

// toStoreSecret converts a SetSecretInput to a store.Secret. The returned
// Secret's EncryptedValue initially holds the plaintext; the caller (Set)
// is responsible for encrypting it before persisting.
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

// decryptStoreSecret converts a store.Secret to SecretWithValue, decrypting
// its EncryptedValue with the shared cipher. Every failure is a typed
// ValueCipher or envelope error naming the secret key, never a value.
func (b *LocalBackend) decryptStoreSecret(s *store.Secret) (*SecretWithValue, error) {
	value, err := b.decryptPayload(s.EncryptedValue, s.SecretRef)
	if err != nil {
		return nil, fmt.Errorf("decrypting secret %q: %w", s.Key, err)
	}
	return &SecretWithValue{
		SecretMeta: *fromStoreSecretMeta(s),
		Value:      value,
	}, nil
}

// decryptPayload is decryptRawValue for a row with backend reference ref.
// A reference-only row (an empty value with a non-empty SecretRef, as the
// GCP backend writes) holds no local payload: it is not decrypted and
// reads as the empty value, exactly as before, so a managed cipher does
// not misreport it as unadmitted plaintext (ptone/scion#1530 §7).
func (b *LocalBackend) decryptPayload(raw, ref string) (string, error) {
	if raw == "" && ref != "" {
		return "", nil
	}
	return b.decryptRawValue(raw)
}

// decryptRawValue decrypts a raw stored value with the shared cipher. On
// failure the value is always empty and the error is typed: a failure is
// reported to the caller, never delivered as an indistinguishable empty
// value.
func (b *LocalBackend) decryptRawValue(raw string) (string, error) {
	plaintext, err := b.cipher.Decrypt(raw)
	if err != nil {
		return "", fmt.Errorf("decrypting secret value: %w", err)
	}
	return plaintext, nil
}
