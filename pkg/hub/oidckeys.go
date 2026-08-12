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

package hub

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/secret"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/go-jose/go-jose/v4"
	"github.com/google/uuid"
)

const (
	// SecretKeyOIDCSigningKey is the secret key name for the OIDC signing key.
	SecretKeyOIDCSigningKey = "oidc_signing_key"

	// oidcKIDPrefix is the prefix for OIDC key IDs.
	oidcKIDPrefix = "scion-oidc-"

	// oidcRSAKeyBits is the RSA key size for OIDC signing keys.
	oidcRSAKeyBits = 2048

	// oidcKeyOverlapWindow is the duration that rotated-out keys remain in the
	// JWKS after rotation, allowing external systems to pick up the new key.
	// 24 hours provides ample margin for all JWKS caching consumers.
	oidcKeyOverlapWindow = 24 * time.Hour

	// oidcCleanupInterval is how often the background cleanup loop checks for
	// expired rotated keys.
	oidcCleanupInterval = 1 * time.Hour

	// SecretKeyOIDCKeyset is the secret key name for the OIDC signing keyset.
	// Unlike SecretKeyOIDCSigningKey which stores a single PEM, this entry
	// stores a JSON array of all keys (active + rotated) so that all hub
	// instances serve the same JWKS and rotated keys survive restarts.
	SecretKeyOIDCKeyset = "oidc_signing_keyset"

	// oidcJWKSRefreshInterval is how often each instance refreshes its
	// in-memory key set from the database. This ensures all instances
	// converge to the same JWKS within this window.
	oidcJWKSRefreshInterval = 30 * time.Second
)

// OIDCSigningKey holds an RSA key pair used for signing OIDC identity tokens.
type OIDCSigningKey struct {
	KeyID         string
	PrivateKey    *rsa.PrivateKey
	PublicKey     *rsa.PublicKey
	CreatedAt     time.Time
	DeactivatedAt time.Time // zero until rotated out
	Active        bool
}

// oidcKeyRecord is a JSON-serializable representation of an OIDC signing key,
// used to persist the full keyset (active + rotated) to the database. This
// enables all hub instances to serve an identical JWKS and preserves rotated
// keys across restarts.
type oidcKeyRecord struct {
	KID           string    `json:"kid"`
	PrivateKeyPEM string    `json:"private_key_pem"`
	Active        bool      `json:"active"`
	CreatedAt     time.Time `json:"created_at"`
	DeactivatedAt time.Time `json:"deactivated_at,omitempty"`
}

// OIDCKeyManager manages RSA key pairs for OIDC identity token signing.
// It loads or generates keys on initialization and provides thread-safe
// access to the jose.Signer and JWKS for downstream consumers.
type OIDCKeyManager struct {
	mu        sync.RWMutex
	activeKey *OIDCSigningKey
	allKeys   []*OIDCSigningKey
	signer    jose.Signer
	store     store.Store
	backend   secret.SecretBackend
	hubID     string
	issuerURL string
	log       *slog.Logger
}

// OIDCKeyManagerConfig holds the configuration needed to initialize an OIDCKeyManager.
type OIDCKeyManagerConfig struct {
	Store                   store.Store
	Backend                 secret.SecretBackend
	HubID                   string
	IssuerURL               string
	RequireStableSigningKey bool
	Log                     *slog.Logger
}

// NewOIDCKeyManager creates a new OIDCKeyManager, loading or generating
// the RSA signing key pair. The initialization flow mirrors ensureSigningKey
// in server.go but works with PEM-encoded RSA private keys instead of
// base64-encoded symmetric keys.
//
// Resolution order:
//  1. Secret backend (e.g. GCP Secret Manager)
//  2. SQLite store fallback
//  3. Generate new RSA-2048 key pair (or fail if RequireStableSigningKey)
func NewOIDCKeyManager(ctx context.Context, cfg OIDCKeyManagerConfig) (*OIDCKeyManager, error) {
	log := cfg.Log
	if log == nil {
		log = slog.Default()
	}

	mgr := &OIDCKeyManager{
		store:     cfg.Store,
		backend:   cfg.Backend,
		hubID:     cfg.HubID,
		issuerURL: cfg.IssuerURL,
		log:       log,
	}

	privKey, err := mgr.loadOrCreateKey(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("OIDC key initialization: %w", err)
	}

	kid := computeKeyID(&privKey.PublicKey)

	signingKey := &OIDCSigningKey{
		KeyID:      kid,
		PrivateKey: privKey,
		PublicKey:  &privKey.PublicKey,
		CreatedAt:  time.Now(),
		Active:     true,
	}

	// Create the jose.Signer with RS256 and kid in the header.
	signer, err := jose.NewSigner(
		jose.SigningKey{Algorithm: jose.RS256, Key: privKey},
		(&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", kid),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create OIDC RS256 signer: %w", err)
	}

	mgr.activeKey = signingKey
	mgr.allKeys = []*OIDCSigningKey{signingKey}
	mgr.signer = signer

	// Restore rotated keys from the DB keyset so the JWKS overlap window
	// survives hub restarts and is consistent across instances.
	if cfg.Store != nil {
		if restoreErr := mgr.restoreRotatedKeysFromDB(ctx); restoreErr != nil {
			log.Warn("Failed to restore rotated OIDC keys from DB keyset", "error", restoreErr)
		}
		// Persist the current keyset (active + any restored rotated keys) to DB.
		if saveErr := mgr.saveKeysetToDB(ctx); saveErr != nil {
			log.Warn("Failed to save OIDC keyset to DB", "error", saveErr)
		}
	}

	log.Info("OIDC key manager initialized",
		"kid", kid,
		"key_count", len(mgr.allKeys),
		"issuer_url", cfg.IssuerURL,
	)

	return mgr, nil
}

// Signer returns the RS256 jose.Signer for signing identity tokens.
// Thread-safe for concurrent reads.
func (m *OIDCKeyManager) Signer() jose.Signer {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.signer
}

// JWKS returns the public key set as a jose.JSONWebKeySet for the
// /.well-known/jwks.json endpoint. All keys (active and rotated) are
// included to support key rotation overlap.
// Thread-safe for concurrent reads.
func (m *OIDCKeyManager) JWKS() jose.JSONWebKeySet {
	m.mu.RLock()
	defer m.mu.RUnlock()

	var keys []jose.JSONWebKey
	for _, k := range m.allKeys {
		keys = append(keys, jose.JSONWebKey{
			Key:       k.PublicKey,
			KeyID:     k.KeyID,
			Algorithm: string(jose.RS256),
			Use:       "sig",
		})
	}
	return jose.JSONWebKeySet{Keys: keys}
}

// IssuerURL returns the configured OIDC issuer URL.
func (m *OIDCKeyManager) IssuerURL() string {
	return m.issuerURL
}

// RotateKey generates a new RSA key pair, makes it the active signing key,
// and retains the old key in the JWKS for the overlap window. The old key
// will be removed by CleanupExpiredKeys after oidcKeyOverlapWindow (24h).
func (m *OIDCKeyManager) RotateKey(ctx context.Context) error {
	// 1. Generate new RSA-2048 key pair (outside the lock).
	newPrivKey, err := generateRSAKeyPair()
	if err != nil {
		return fmt.Errorf("generating new OIDC signing key: %w", err)
	}

	newKID := computeKeyID(&newPrivKey.PublicKey)

	// 2. Create new jose.Signer with the new key.
	newSigner, err := jose.NewSigner(
		jose.SigningKey{Algorithm: jose.RS256, Key: newPrivKey},
		(&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", newKID),
	)
	if err != nil {
		return fmt.Errorf("creating signer for rotated OIDC key: %w", err)
	}

	// 3. Persist new key to backend/store BEFORE in-memory swap.
	pemData, err := encodePEMPrivateKey(newPrivKey)
	if err != nil {
		return fmt.Errorf("PEM-encoding rotated OIDC key: %w", err)
	}
	pemStr := string(pemData)

	// Overwrite the stored key with the new active key so it is loaded on restart.
	keyName := SecretKeyOIDCSigningKey
	persisted := false
	if m.backend != nil {
		input := &secret.SetSecretInput{
			Name:        keyName,
			Value:       pemStr,
			SecretType:  store.SecretTypeInternal,
			Scope:       store.ScopeHub,
			ScopeID:     m.hubID,
			Description: "OIDC identity token signing key (RSA-2048)",
		}
		if _, _, setErr := m.backend.Set(ctx, input); setErr != nil {
			m.log.Warn("Failed to persist rotated OIDC key to secret backend",
				"kid", newKID, "error", setErr)
		} else {
			persisted = true
		}
	}
	if m.store != nil {
		if persistErr := m.backupKeyToStore(ctx, keyName, pemStr, m.hubID); persistErr != nil {
			m.log.Warn("Failed to persist rotated OIDC key to store",
				"kid", newKID, "error", persistErr)
		} else {
			persisted = true
		}
	}

	// If ALL persistence paths failed, do NOT swap in-memory — return an error.
	if !persisted {
		return fmt.Errorf("persisting rotated OIDC key: all persistence paths failed for kid %s", newKID)
	}

	// 4. Swap keys under write lock (only after successful persistence).
	newKey := &OIDCSigningKey{
		KeyID:      newKID,
		PrivateKey: newPrivKey,
		PublicKey:  &newPrivKey.PublicKey,
		CreatedAt:  time.Now(),
		Active:     true,
	}

	m.mu.Lock()
	oldKID := ""
	if m.activeKey != nil {
		oldKID = m.activeKey.KeyID
		m.activeKey.Active = false
		m.activeKey.DeactivatedAt = time.Now()
	}
	m.activeKey = newKey
	m.allKeys = append([]*OIDCSigningKey{newKey}, m.allKeys...)
	m.signer = newSigner
	m.mu.Unlock()

	// Persist the full keyset (active + rotated) to DB so all instances
	// serve a consistent JWKS. Also clean up expired keys while we're at it.
	if m.store != nil {
		if err := m.saveKeysetToDB(ctx); err != nil {
			m.log.Warn("Failed to persist rotated OIDC keyset to DB", "error", err)
		}
	}

	m.log.Info("OIDC signing key rotated",
		"old_kid", oldKID,
		"new_kid", newKID,
	)

	return nil
}

// CleanupExpiredKeys removes inactive keys that have been rotated out
// longer than the overlap window (24 hours). The active key is never
// removed regardless of age.
func (m *OIDCKeyManager) CleanupExpiredKeys() {
	m.mu.Lock()

	now := time.Now()
	kept := make([]*OIDCSigningKey, 0, len(m.allKeys))
	removed := 0
	for _, k := range m.allKeys {
		if k.Active {
			kept = append(kept, k)
			continue
		}
		age := now.Sub(k.DeactivatedAt)
		if age < oidcKeyOverlapWindow {
			kept = append(kept, k)
			continue
		}
		m.log.Info("Removing expired OIDC key from JWKS",
			"kid", k.KeyID,
			"deactivated_at", k.DeactivatedAt,
			"age", age.Round(time.Second),
		)
		removed++
	}
	m.allKeys = kept
	m.mu.Unlock()

	// Persist cleaned-up keyset to DB so other instances don't keep
	// serving the expired keys.
	//
	// Note: there is a minor TOCTOU window between the Unlock above and
	// the saveKeysetToDB call below — a concurrent RotateKey could modify
	// m.allKeys in between. This is harmless: saveKeysetToDB re-reads
	// m.allKeys under RLock, so it will capture the rotation's changes.
	// In the worst case (a concurrent rotation writes the keyset between
	// our unlock and save), the expired keys reappear briefly and are
	// cleaned up on the next cycle or refresh.
	if removed > 0 && m.store != nil {
		if err := m.saveKeysetToDB(context.Background()); err != nil {
			m.log.Warn("Failed to persist cleaned-up OIDC keyset to DB", "error", err)
		}
	}
}

// StartCleanupLoop starts a background goroutine that periodically removes
// expired rotated keys from the JWKS. Call this once after initialization.
// The goroutine stops when ctx is canceled.
func (m *OIDCKeyManager) StartCleanupLoop(ctx context.Context) {
	go func() {
		ticker := time.NewTicker(oidcCleanupInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				m.CleanupExpiredKeys()
			}
		}
	}()
}

// StartRefreshLoop starts a background goroutine that periodically refreshes
// the in-memory key set from the database. This ensures all hub instances
// converge to the same JWKS within oidcJWKSRefreshInterval (30s), so that
// keys rotated or cleaned up by one instance become visible to all others.
// The goroutine stops when ctx is canceled.
func (m *OIDCKeyManager) StartRefreshLoop(ctx context.Context) {
	go func() {
		ticker := time.NewTicker(oidcJWKSRefreshInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				m.refreshKeysFromDB(ctx)
			}
		}
	}()
}

// loadOrCreateKey attempts to load an existing OIDC signing key from the
// secret backend or store, falling back to generating a new key pair.
func (m *OIDCKeyManager) loadOrCreateKey(ctx context.Context, cfg OIDCKeyManagerConfig) (*rsa.PrivateKey, error) {
	keyName := SecretKeyOIDCSigningKey
	hubID := cfg.HubID
	hasBackend := cfg.Backend != nil

	// 1. Try the secret backend (e.g. GCP Secret Manager)
	if hasBackend {
		sv, err := cfg.Backend.Get(ctx, keyName, store.ScopeHub, hubID)
		if err == nil {
			m.log.Info("Loading OIDC signing key from secret backend", "key", keyName)
			privKey, parseErr := decodePEMPrivateKey([]byte(sv.Value))
			if parseErr != nil {
				return nil, fmt.Errorf("failed to decode OIDC signing key from secret backend: %w", parseErr)
			}
			// Backfill to SQLite as local backup
			if persistErr := m.backupKeyToStore(ctx, keyName, sv.Value, hubID); persistErr != nil {
				m.log.Warn("Failed to persist OIDC key backup to store after loading from backend",
					"key", keyName, "error", persistErr)
			}
			return privKey, nil
		}
		if err != store.ErrNotFound {
			m.log.Warn("Failed to load OIDC signing key from secret backend, trying store",
				"key", keyName, "error", err)
		}
	}

	// 2. Try the SQLite store
	if cfg.Store != nil {
		val, err := cfg.Store.GetSecretValue(ctx, keyName, store.ScopeHub, hubID)
		if err == nil && val != "" {
			m.log.Info("Loading OIDC signing key from store", "key", keyName)
			privKey, parseErr := decodePEMPrivateKey([]byte(val))
			if parseErr != nil {
				return nil, fmt.Errorf("failed to decode OIDC signing key from store: %w", parseErr)
			}
			// Sync to secret backend for future loads
			if hasBackend {
				if syncErr := m.syncKeyToBackend(ctx, keyName, val, hubID); syncErr != nil {
					m.log.Warn("Failed to sync OIDC signing key to secret backend",
						"key", keyName, "error", syncErr)
				}
			}
			return privKey, nil
		}
		if err != nil && err != store.ErrNotFound {
			return nil, fmt.Errorf("failed to load OIDC signing key from store: %w", err)
		}
	}

	// 3. No existing key found — generate or fail
	if cfg.RequireStableSigningKey {
		return nil, fmt.Errorf("refusing to generate a new OIDC signing key: RequireStableSigningKey is set and no existing key was found; " +
			"pre-provision the key via the secret backend or store")
	}

	privKey, err := generateRSAKeyPair()
	if err != nil {
		return nil, fmt.Errorf("failed to generate OIDC RSA key pair: %w", err)
	}
	m.log.Warn("Generated new OIDC signing key; all previously issued identity tokens are invalid", "key", keyName)

	pemData, err := encodePEMPrivateKey(privKey)
	if err != nil {
		return nil, fmt.Errorf("failed to PEM-encode OIDC signing key: %w", err)
	}
	pemStr := string(pemData)

	// CAS: use CreateSecret so that if two instances cold-start
	// simultaneously, only the first writer's key is used. The loser
	// reloads the winner's key from the store.
	if cfg.Store != nil {
		casKey := m.casCreateKeyInStore(ctx, keyName, pemStr, hubID)
		if casKey != nil {
			// Another instance created a key first — use theirs.
			m.log.Info("Another instance already created OIDC signing key, using theirs", "key", keyName)
			return casKey, nil
		}
	}

	// Persist to secret backend first, then store as backup
	if hasBackend {
		input := &secret.SetSecretInput{
			Name:        keyName,
			Value:       pemStr,
			SecretType:  store.SecretTypeInternal,
			Scope:       store.ScopeHub,
			ScopeID:     hubID,
			Description: "OIDC identity token signing key (RSA-2048)",
		}
		if _, _, err := cfg.Backend.Set(ctx, input); err != nil {
			_, isGCP := cfg.Backend.(*secret.GCPBackend)
			if isGCP {
				return nil, fmt.Errorf("failed to persist OIDC signing key to Secret Manager: %w", err)
			}
			m.log.Warn("Secret backend unavailable for OIDC signing key, falling back to store",
				"key", keyName, "error", err)
		} else {
			m.log.Info("Persisted new OIDC signing key via secret backend", "key", keyName)
		}
	}

	return privKey, nil
}

// backupKeyToStore saves the PEM-encoded key to SQLite as a local backup.
func (m *OIDCKeyManager) backupKeyToStore(ctx context.Context, keyName, pemValue, hubID string) error {
	if m.store == nil {
		return nil
	}
	existing, err := m.store.GetSecret(ctx, keyName, store.ScopeHub, hubID)
	if err == nil {
		existing.EncryptedValue = pemValue
		return m.store.UpdateSecret(ctx, existing)
	}
	if err != store.ErrNotFound {
		return fmt.Errorf("checking existing OIDC key record: %w", err)
	}
	sec := &store.Secret{
		ID:             oidcSigningKeySecretID(hubID),
		Key:            keyName,
		EncryptedValue: pemValue,
		Scope:          store.ScopeHub,
		ScopeID:        hubID,
		SecretType:     store.SecretTypeInternal,
		Description:    "OIDC identity token signing key (RSA-2048)",
	}
	_, err = m.store.UpsertSecret(ctx, sec)
	return err
}

// syncKeyToBackend syncs a PEM-encoded key to the secret backend.
func (m *OIDCKeyManager) syncKeyToBackend(ctx context.Context, keyName, pemValue, hubID string) error {
	if m.backend == nil {
		return nil
	}
	input := &secret.SetSecretInput{
		Name:        keyName,
		Value:       pemValue,
		SecretType:  store.SecretTypeInternal,
		Scope:       store.ScopeHub,
		ScopeID:     hubID,
		Description: "OIDC identity token signing key (RSA-2048)",
	}
	_, _, err := m.backend.Set(ctx, input)
	return err
}

// oidcSigningKeySecretID returns a deterministic primary key for the OIDC
// signing key record, scoped to the hub instance.
func oidcSigningKeySecretID(hubID string) string {
	return uuid.NewSHA1(uuid.NameSpaceOID, []byte("hub-oidc-signing-key:"+hubID)).String()
}

// casCreateKeyInStore attempts to create the signing key in the store using
// CreateSecret (which returns ErrAlreadyExists if the key already exists).
// If another instance already created a key, it loads and returns that key.
// Returns nil if our key was successfully created (caller should use theirs).
func (m *OIDCKeyManager) casCreateKeyInStore(ctx context.Context, keyName, pemValue, hubID string) *rsa.PrivateKey {
	sec := &store.Secret{
		ID:             oidcSigningKeySecretID(hubID),
		Key:            keyName,
		EncryptedValue: pemValue,
		Scope:          store.ScopeHub,
		ScopeID:        hubID,
		SecretType:     store.SecretTypeInternal,
		Description:    "OIDC identity token signing key (RSA-2048)",
	}
	err := m.store.CreateSecret(ctx, sec)
	if err == nil {
		// We won the race — our key was created.
		m.log.Info("Persisted OIDC signing key to store via CAS", "key", keyName)
		return nil
	}
	if !errors.Is(err, store.ErrAlreadyExists) {
		// Unexpected error — fall through to existing UpsertSecret path.
		m.log.Warn("CAS create failed for OIDC signing key, falling back to upsert",
			"key", keyName, "error", err)
		if persistErr := m.backupKeyToStore(ctx, keyName, pemValue, hubID); persistErr != nil {
			m.log.Warn("Failed to persist OIDC signing key to store", "key", keyName, "error", persistErr)
		}
		return nil
	}

	// Another instance won. Load their key.
	val, getErr := m.store.GetSecretValue(ctx, keyName, store.ScopeHub, hubID)
	if getErr != nil || val == "" {
		m.log.Warn("CAS lost but could not load winner's OIDC key from store, using ours",
			"key", keyName, "error", getErr)
		return nil
	}
	existingKey, parseErr := decodePEMPrivateKey([]byte(val))
	if parseErr != nil {
		m.log.Warn("CAS lost but could not parse winner's OIDC key, using ours",
			"key", keyName, "error", parseErr)
		return nil
	}
	return existingKey
}

// saveKeysetToDB persists the full keyset (active + rotated keys) to the
// database as a JSON array. This is the source of truth for the JWKS that
// all hub instances serve, ensuring cross-instance consistency.
func (m *OIDCKeyManager) saveKeysetToDB(ctx context.Context) error {
	if m.store == nil {
		return nil
	}

	m.mu.RLock()
	records := make([]oidcKeyRecord, 0, len(m.allKeys))
	for _, k := range m.allKeys {
		pemData, err := encodePEMPrivateKey(k.PrivateKey)
		if err != nil {
			m.mu.RUnlock()
			return fmt.Errorf("encoding key %s: %w", k.KeyID, err)
		}
		records = append(records, oidcKeyRecord{
			KID:           k.KeyID,
			PrivateKeyPEM: string(pemData),
			Active:        k.Active,
			CreatedAt:     k.CreatedAt,
			DeactivatedAt: k.DeactivatedAt,
		})
	}
	m.mu.RUnlock()

	data, err := json.Marshal(records)
	if err != nil {
		return fmt.Errorf("marshaling OIDC keyset: %w", err)
	}

	sec := &store.Secret{
		ID:             oidcKeysetSecretID(m.hubID),
		Key:            SecretKeyOIDCKeyset,
		EncryptedValue: string(data),
		Scope:          store.ScopeHub,
		ScopeID:        m.hubID,
		SecretType:     store.SecretTypeInternal,
		Description:    "OIDC signing keyset (all active and rotated keys)",
	}
	if _, err := m.store.UpsertSecret(ctx, sec); err != nil {
		return fmt.Errorf("upserting OIDC keyset: %w", err)
	}
	return nil
}

// loadKeysetFromDB loads the full keyset from the database and returns
// the parsed signing keys.
func (m *OIDCKeyManager) loadKeysetFromDB(ctx context.Context) ([]*OIDCSigningKey, error) {
	if m.store == nil {
		return nil, store.ErrNotFound
	}

	val, err := m.store.GetSecretValue(ctx, SecretKeyOIDCKeyset, store.ScopeHub, m.hubID)
	if err != nil {
		return nil, err
	}
	if val == "" {
		return nil, store.ErrNotFound
	}

	var records []oidcKeyRecord
	if err := json.Unmarshal([]byte(val), &records); err != nil {
		return nil, fmt.Errorf("unmarshaling OIDC keyset: %w", err)
	}

	keys := make([]*OIDCSigningKey, 0, len(records))
	for _, rec := range records {
		privKey, err := decodePEMPrivateKey([]byte(rec.PrivateKeyPEM))
		if err != nil {
			m.log.Warn("Skipping unparseable key in OIDC keyset", "kid", rec.KID, "error", err)
			continue
		}
		keys = append(keys, &OIDCSigningKey{
			KeyID:         rec.KID,
			PrivateKey:    privKey,
			PublicKey:     &privKey.PublicKey,
			Active:        rec.Active,
			CreatedAt:     rec.CreatedAt,
			DeactivatedAt: rec.DeactivatedAt,
		})
	}
	return keys, nil
}

// restoreRotatedKeysFromDB loads the keyset from the database and merges
// any rotated (inactive) keys into the in-memory key list. This restores
// the JWKS overlap window across hub restarts.
func (m *OIDCKeyManager) restoreRotatedKeysFromDB(ctx context.Context) error {
	keys, err := m.loadKeysetFromDB(ctx)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil // No keyset yet — first run.
		}
		return err
	}

	// Build a set of KIDs already in m.allKeys.
	m.mu.Lock()
	defer m.mu.Unlock()

	existing := make(map[string]bool, len(m.allKeys))
	for _, k := range m.allKeys {
		existing[k.KeyID] = true
	}

	restored := 0
	for _, k := range keys {
		if existing[k.KeyID] {
			continue
		}
		// Only restore inactive keys that are still within the overlap window.
		if !k.Active && !k.DeactivatedAt.IsZero() {
			age := time.Since(k.DeactivatedAt)
			if age >= oidcKeyOverlapWindow {
				continue // Already expired — don't restore.
			}
		}
		m.allKeys = append(m.allKeys, k)
		restored++
	}

	if restored > 0 {
		m.log.Info("Restored rotated OIDC keys from DB keyset",
			"restored_count", restored,
			"total_keys", len(m.allKeys),
		)
	}
	return nil
}

// refreshKeysFromDB reloads the full keyset from the database and updates
// the in-memory state. This is called periodically by StartRefreshLoop to
// ensure all instances converge to the same JWKS.
func (m *OIDCKeyManager) refreshKeysFromDB(ctx context.Context) {
	keys, err := m.loadKeysetFromDB(ctx)
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			m.log.Warn("Failed to refresh OIDC keyset from DB", "error", err)
		}
		return
	}

	if len(keys) == 0 {
		return // Don't clear our keys if DB returned empty.
	}

	// Find the active key in the refreshed set.
	var newActiveKey *OIDCSigningKey
	for _, k := range keys {
		if k.Active {
			newActiveKey = k
			break
		}
	}
	if newActiveKey == nil {
		m.log.Warn("DB keyset has no active key, skipping refresh")
		return
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	// Check if the active key changed (another instance rotated).
	if m.activeKey != nil && m.activeKey.KeyID != newActiveKey.KeyID {
		// Rebuild signer with the new active key.
		newSigner, err := jose.NewSigner(
			jose.SigningKey{Algorithm: jose.RS256, Key: newActiveKey.PrivateKey},
			(&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", newActiveKey.KeyID),
		)
		if err != nil {
			m.log.Error("Failed to create signer for refreshed OIDC key",
				"kid", newActiveKey.KeyID, "error", err)
			return
		}
		m.signer = newSigner
		m.log.Info("OIDC active key updated from DB refresh",
			"old_kid", m.activeKey.KeyID,
			"new_kid", newActiveKey.KeyID,
		)
	}

	m.activeKey = newActiveKey
	m.allKeys = keys
}

// oidcKeysetSecretID returns a deterministic primary key for the OIDC
// signing keyset record, scoped to the hub instance.
func oidcKeysetSecretID(hubID string) string {
	return uuid.NewSHA1(uuid.NameSpaceOID, []byte("hub-oidc-signing-keyset:"+hubID)).String()
}

// generateRSAKeyPair generates a new RSA-2048 key pair.
func generateRSAKeyPair() (*rsa.PrivateKey, error) {
	return rsa.GenerateKey(rand.Reader, oidcRSAKeyBits)
}

// encodePEMPrivateKey encodes an RSA private key to PEM format using
// PKCS#8 encoding (standard "PRIVATE KEY" block type).
func encodePEMPrivateKey(key *rsa.PrivateKey) ([]byte, error) {
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal private key to PKCS#8: %w", err)
	}
	block := &pem.Block{
		Type:  "PRIVATE KEY",
		Bytes: der,
	}
	return pem.EncodeToMemory(block), nil
}

// decodePEMPrivateKey decodes a PEM-encoded private key, expecting PKCS#8 format.
func decodePEMPrivateKey(data []byte) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, fmt.Errorf("no PEM block found in key data")
	}
	if block.Type != "PRIVATE KEY" {
		return nil, fmt.Errorf("unexpected PEM block type %q, expected PRIVATE KEY", block.Type)
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("failed to parse PKCS#8 private key: %w", err)
	}
	rsaKey, ok := parsed.(*rsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("parsed key is not RSA (got %T)", parsed)
	}
	return rsaKey, nil
}

// computeKeyID generates a deterministic key ID from an RSA public key.
// Format: "scion-oidc-" + first 12 hex chars of SHA-256(DER-encoded public key).
func computeKeyID(pub *rsa.PublicKey) string {
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		// This should never fail for a valid RSA public key.
		panic(fmt.Sprintf("failed to marshal public key to DER: %v", err))
	}
	hash := sha256.Sum256(der)
	return oidcKIDPrefix + hex.EncodeToString(hash[:6]) // 12 hex chars = 6 bytes
}
