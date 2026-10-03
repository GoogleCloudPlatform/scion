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
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit/grant"
	"github.com/GoogleCloudPlatform/scion/pkg/secret"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// Conduit stream grant keys (design v2.1 §3.5 "Key scheme", §3.10).
//
// The hub signs stream grants with a dedicated Ed25519 key ring. It is not
// the HS256 agent/user token secret, and its private half never leaves the
// hub: it is not returned by any API, sent to agents or brokers, or logged.
//
// Storage. Every hub node must sign with keys the targets accept, so the ring
// lives in the shared hub database as one hub-scope internal secret
// (conduitGrantKeySecretName) under a fixed scope ID rather than the per-node
// hub ID the HS256 keys use. Its value is encrypted at rest with the same
// AES-256-GCM key the hub's signing-key backups use (derived from the shared
// signing secret; plaintext only when none is configured, as for those keys).
//
// Bootstrap. The first node to need the ring creates it; CreateSecret's
// uniqueness makes a concurrent bootstrap converge on one ring (the loser
// reloads). With a SharedSigningSecret the bootstrap key is also derived
// deterministically, so every node would produce the same key anyway.
//
// Rotation. rotateConduitGrantKey adds a key that is published at once and
// signs only after an activation delay, and schedules every older key to
// retire at activation + overlap (overlap >= grant.MaxValidity). Nodes reload
// the ring every conduitGrantKeyRefresh, so the activation delay must exceed
// that interval. Rotation is a read-modify-write of one row and is meant to
// be operator-driven, one at a time.
const (
	conduitExperiment = "hub.conduit"

	// conduitGrantKeySecretName is the ring's secret key (design
	// "conduit.grant_key").
	conduitGrantKeySecretName = "conduit.grant_key"
	// conduitGrantKeyScopeID is the fixed hub-scope ID the ring is stored
	// under, shared by every node.
	conduitGrantKeyScopeID = "conduit"

	// conduitGrantIssuer is the iss of every grant.
	conduitGrantIssuer = "scion-hub"
	// conduitGrantTTL is the exp−nbf of minted grants.
	conduitGrantTTL = 30 * time.Second

	// conduitGrantKeyRefresh is how long a node caches the ring.
	conduitGrantKeyRefresh = time.Minute
	// Default rotation timing: publish 15 minutes before signing, keep the
	// old key for an hour after the switch.
	conduitGrantKeyDefaultActivation = 15 * time.Minute
	conduitGrantKeyDefaultOverlap    = time.Hour
)

var (
	errConduitDisabled  = errors.New("conduit experiment is disabled")
	errConduitForbidden = errors.New("conduit stream forbidden")
	errConduitInvalid   = errors.New("invalid conduit stream request")
)

// conduitGrantKeyStore persists the key ring.
type conduitGrantKeyStore interface {
	// Load returns the ring, or store.ErrNotFound.
	Load(ctx context.Context) (*grant.KeyRing, error)
	// Create stores a ring where none exists, or returns
	// store.ErrAlreadyExists.
	Create(ctx context.Context, ring *grant.KeyRing) error
	// Update replaces the stored ring.
	Update(ctx context.Context, ring *grant.KeyRing) error
}

// dbConduitGrantKeyStore stores the ring as an encrypted hub-scope secret.
type dbConduitGrantKeyStore struct {
	store         store.SecretStore
	encryptionKey []byte
}

func (d *dbConduitGrantKeyStore) Load(ctx context.Context) (*grant.KeyRing, error) {
	val, err := d.store.GetSecretValue(ctx, conduitGrantKeySecretName, store.ScopeHub, conduitGrantKeyScopeID)
	if err != nil {
		return nil, err
	}
	if val == "" {
		return nil, store.ErrNotFound
	}
	if d.encryptionKey != nil {
		plain, _, err := secret.DecryptValue(val, d.encryptionKey)
		if err != nil {
			return nil, fmt.Errorf("decrypt conduit grant key ring: %w", err)
		}
		val = plain
	}
	var ring grant.KeyRing
	if err := json.Unmarshal([]byte(val), &ring); err != nil {
		return nil, fmt.Errorf("decode conduit grant key ring: %w", err)
	}
	if err := ring.Validate(); err != nil {
		return nil, err
	}
	return &ring, nil
}

func (d *dbConduitGrantKeyStore) encode(ring *grant.KeyRing) (string, error) {
	raw, err := json.Marshal(ring)
	if err != nil {
		return "", err
	}
	if d.encryptionKey == nil {
		return string(raw), nil
	}
	return secret.EncryptValue(string(raw), d.encryptionKey)
}

func (d *dbConduitGrantKeyStore) Create(ctx context.Context, ring *grant.KeyRing) error {
	val, err := d.encode(ring)
	if err != nil {
		return err
	}
	return d.store.CreateSecret(ctx, &store.Secret{
		ID:             signingKeySecretID(conduitGrantKeySecretName, conduitGrantKeyScopeID),
		Key:            conduitGrantKeySecretName,
		EncryptedValue: val,
		Scope:          store.ScopeHub,
		ScopeID:        conduitGrantKeyScopeID,
		SecretType:     store.SecretTypeInternal,
		Description:    "Conduit stream grant signing key ring",
	})
}

func (d *dbConduitGrantKeyStore) Update(ctx context.Context, ring *grant.KeyRing) error {
	val, err := d.encode(ring)
	if err != nil {
		return err
	}
	existing, err := d.store.GetSecret(ctx, conduitGrantKeySecretName, store.ScopeHub, conduitGrantKeyScopeID)
	if err != nil {
		return err
	}
	existing.EncryptedValue = val
	return d.store.UpdateSecret(ctx, existing)
}

// conduitGrantKeys caches the ring for one hub node.
type conduitGrantKeys struct {
	store conduitGrantKeyStore
	now   func() time.Time
	// bootstrapSeed, when set, is the deterministic seed of the first key.
	bootstrapSeed []byte

	mu       sync.Mutex
	ring     *grant.KeyRing
	loadedAt time.Time
}

func newConduitGrantKeys(st conduitGrantKeyStore, sharedSecret string, now func() time.Time) *conduitGrantKeys {
	if now == nil {
		now = time.Now
	}
	k := &conduitGrantKeys{store: st, now: now}
	if sharedSecret != "" {
		k.bootstrapSeed = deriveSharedSigningKey(sharedSecret, conduitGrantKeySecretName+":bootstrap")
	}
	return k
}

func (k *conduitGrantKeys) bootstrapKey(now time.Time) (grant.RingKey, error) {
	if len(k.bootstrapSeed) != ed25519.SeedSize {
		return grant.NewRingKey(now, now)
	}
	pub := ed25519.NewKeyFromSeed(k.bootstrapSeed).Public().(ed25519.PublicKey)
	sum := sha256.Sum256(pub)
	return grant.RingKey{
		KeyID:      "cg-" + hex.EncodeToString(sum[:8]),
		Seed:       append([]byte(nil), k.bootstrapSeed...),
		CreatedAt:  now.UTC(),
		ActivateAt: now.UTC(),
	}, nil
}

// current returns the cached ring, reloading it when stale and creating it
// when absent. Callers must hold k.mu.
func (k *conduitGrantKeys) currentLocked(ctx context.Context) (*grant.KeyRing, error) {
	now := k.now()
	if k.ring != nil && now.Sub(k.loadedAt) < conduitGrantKeyRefresh {
		return k.ring, nil
	}
	ring, err := k.store.Load(ctx)
	if errors.Is(err, store.ErrNotFound) {
		key, kerr := k.bootstrapKey(now)
		if kerr != nil {
			return nil, kerr
		}
		ring = &grant.KeyRing{Keys: []grant.RingKey{key}}
		err = k.store.Create(ctx, ring)
		if errors.Is(err, store.ErrAlreadyExists) {
			// Another node won the bootstrap; use its ring.
			ring, err = k.store.Load(ctx)
		}
		if err == nil {
			slog.Info("Conduit grant key ring created", "kid", key.KeyID)
		}
	}
	if err != nil {
		return nil, fmt.Errorf("conduit grant key ring: %w", err)
	}
	k.ring, k.loadedAt = ring, now
	return ring, nil
}

// signer returns the key to sign a grant with now.
func (k *conduitGrantKeys) signer(ctx context.Context) (*grant.Signer, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	ring, err := k.currentLocked(ctx)
	if err != nil {
		return nil, err
	}
	return ring.Signer(k.now())
}

// publicKeys returns every published verification key.
func (k *conduitGrantKeys) publicKeys(ctx context.Context) ([]grant.PublicKey, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	ring, err := k.currentLocked(ctx)
	if err != nil {
		return nil, err
	}
	return ring.PublicKeys(k.now()), nil
}

// rotate adds a new key and returns its kid. It always rereads the stored
// ring first so it never writes back a stale cache.
func (k *conduitGrantKeys) rotate(ctx context.Context, activateAfter, overlap time.Duration) (string, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if activateAfter < conduitGrantKeyRefresh {
		return "", fmt.Errorf("activation delay %s must be at least the ring refresh interval %s", activateAfter, conduitGrantKeyRefresh)
	}
	k.ring = nil
	ring, err := k.currentLocked(ctx)
	if err != nil {
		return "", err
	}
	next := *ring
	next.Keys = append([]grant.RingKey(nil), ring.Keys...)
	now := k.now()
	key, err := grant.NewRingKey(now, now)
	if err != nil {
		return "", err
	}
	next.Prune(now)
	if err := next.Rotate(now, key, activateAfter, overlap); err != nil {
		return "", err
	}
	if err := k.store.Update(ctx, &next); err != nil {
		return "", fmt.Errorf("store rotated conduit grant key ring: %w", err)
	}
	k.ring, k.loadedAt = &next, now
	slog.Info("Conduit grant key rotated", "kid", key.KeyID, "activate_at", now.Add(activateAfter))
	return key.KeyID, nil
}

// conduitGrantKeySet returns this node's ring cache, creating it on first
// use. No storage is touched until a key is needed.
func (s *Server) conduitGrantKeySet() *conduitGrantKeys {
	s.conduitGrantsOnce.Do(func() {
		if s.conduitGrants == nil {
			s.conduitGrants = newConduitGrantKeys(
				&dbConduitGrantKeyStore{store: s.store, encryptionKey: s.encryptionKey},
				s.config.SharedSigningSecret, nil)
		}
	})
	return s.conduitGrants
}

// ConduitGrantPublicKeys returns the grant verification keys to publish to
// targets (Welcome.grant_keys, the token-refresh response): every key still
// within not_after. It returns errConduitDisabled when the experiment is off.
func (s *Server) ConduitGrantPublicKeys(ctx context.Context) ([]grant.PublicKey, error) {
	if !s.experimentEnabled(conduitExperiment) {
		return nil, errConduitDisabled
	}
	return s.conduitGrantKeySet().publicKeys(ctx)
}

// RotateConduitGrantKey adds a new grant signing key with the default
// timing and returns its kid. It is the hook for an operator rotation
// surface; none is wired in Phase 1.
func (s *Server) RotateConduitGrantKey(ctx context.Context) (string, error) {
	if !s.experimentEnabled(conduitExperiment) {
		return "", errConduitDisabled
	}
	return s.conduitGrantKeySet().rotate(ctx, conduitGrantKeyDefaultActivation, conduitGrantKeyDefaultOverlap)
}

// conduitGrantKeysResponse is the body of GET /api/v1/conduit/grant-keys.
type conduitGrantKeysResponse struct {
	Keys []grant.WireKey `json:"keys"`
}

// handleConduitGrantKeys serves GET /api/v1/conduit/grant-keys: the public
// verification keys only. Targets never trust this unauthenticated-to-them
// fetch alone; they take keys from Welcome and the token refresh.
//
// The experiment is checked per request (404 when off) rather than with
// requireExperiment at route registration, the same as the gcs/object
// route: requireExperiment panics when the server's registry lacks the
// name, which breaks servers built over a test registry.
func (s *Server) handleConduitGrantKeys(w http.ResponseWriter, r *http.Request) {
	if !s.experimentEnabled(conduitExperiment) {
		NotFound(w, "route")
		return
	}
	if r.Method != http.MethodGet {
		MethodNotAllowed(w, http.MethodGet)
		return
	}
	keys, err := s.conduitGrantKeySet().publicKeys(r.Context())
	if err != nil {
		slog.Error("conduit grant keys unavailable", "error", err)
		writeError(w, http.StatusServiceUnavailable, ErrCodeInternalError, "grant keys unavailable", nil)
		return
	}
	writeJSON(w, http.StatusOK, conduitGrantKeysResponse{Keys: grant.ToWire(keys)})
}
