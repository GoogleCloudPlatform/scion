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
	"encoding/pem"
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
	// Note: only the active key is loaded on startup. Previously rotated keys
	// (serving JWKS overlap) are not restored. The overlap window does not
	// survive hub restarts.
	mgr.allKeys = []*OIDCSigningKey{signingKey}
	mgr.signer = signer

	log.Info("OIDC key manager initialized",
		"kid", kid,
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
	defer m.mu.Unlock()

	now := time.Now()
	kept := make([]*OIDCSigningKey, 0, len(m.allKeys))
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
	}
	m.allKeys = kept
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

	// Save to store as backup (or primary if no backend)
	if persistErr := m.backupKeyToStore(ctx, keyName, pemStr, hubID); persistErr != nil {
		m.log.Warn("Failed to persist OIDC signing key to store", "key", keyName, "error", persistErr)
	} else {
		m.log.Info("Persisted OIDC signing key to store", "key", keyName)
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
