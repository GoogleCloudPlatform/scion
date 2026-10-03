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
// Every key, including the first, is generated from crypto/rand.
//
// Storage. The ring is persisted only when encryption at rest is configured
// (the hub has a shared signing secret, from which its AES-256-GCM backup
// key is derived). It is then one hub-scope internal secret
// (conduitGrantKeySecretName) under a fixed scope ID shared by every node,
// so all nodes sign with keys the targets accept. Otherwise the ring is
// ephemeral: generated on first use and held in this process's memory only,
// so a restart (or another node) has a different ring. Grants are
// short-lived, so a restart only invalidates grants that are in flight.
//
// Recovery. If the persisted ring cannot be decrypted or decoded (for
// example the shared signing secret changed, so the encryption key no longer
// matches), every grant mint and key publication fails closed, and an ERROR
// naming the secret is logged at most once per conduitGrantKeyRefresh. The
// ring holds no long-term data: discarding it is always safe and loses only
// grants in flight. To recover, fix the shared signing secret on every node,
// or delete the hub-scope secret conduit.grant_key (scope ID "conduit") from
// the hub database; the next request then creates a new ring. The hub never
// replaces an unreadable ring by itself, since that would hide a
// misconfiguration between nodes.
//
// Multi-node. A memory-only ring is per node: a grant minted on one node
// does not verify against another node's keys. A deployment whose nodes must
// share signing keys (a GCP secret backend or RequireStableSigningKey) gets
// a one-time WARN when the ring is memory-only; such deployments must set a
// shared signing secret.
//
// Bootstrap. The first node to need the ring creates it; CreateSecret's
// uniqueness makes a concurrent bootstrap converge on one ring (the loser
// reloads).
//
// Rotation. rotate adds a key that is published at once and signs only after
// an activation delay, and schedules every older key to retire at activation
// + overlap (overlap >= grant.MaxValidity). The activation delay is
// ServerConfig.ConduitGrantKeyActivation: it must be at least the maximum
// interval at which targets refresh their key set (Welcome and token refresh),
// or a target that has not yet learned the new key refuses its grants. It
// must also be at least conduitGrantKeyRefresh, the interval at which nodes
// reload the ring. Rotation is a compare-and-swap on the stored row, retried
// a bounded number of times, so concurrent rotations never lose a key.
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
	// conduitGrantKeyDefaultActivation is the default publish-before-sign
	// delay when ServerConfig.ConduitGrantKeyActivation is unset.
	conduitGrantKeyDefaultActivation = 15 * time.Minute
	// conduitGrantKeyDefaultOverlap keeps an old key for an hour after the
	// switch.
	conduitGrantKeyDefaultOverlap = time.Hour
	// conduitGrantKeyRotateAttempts bounds the compare-and-swap retries of
	// one rotation.
	conduitGrantKeyRotateAttempts = 5
)

var (
	// errConduitGrantRingUnreadable marks a persisted ring that exists but
	// cannot be decrypted, decoded or validated.
	errConduitGrantRingUnreadable = errors.New("stored conduit grant key ring is unreadable")

	errConduitDisabled  = errors.New("conduit experiment is disabled")
	errConduitForbidden = errors.New("conduit stream forbidden")
	errConduitInvalid   = errors.New("invalid conduit stream request")
)

// conduitGrantKeyStore holds the key ring.
type conduitGrantKeyStore interface {
	// Load returns the ring and its revision, or store.ErrNotFound.
	Load(ctx context.Context) (*grant.KeyRing, int, error)
	// Create stores a ring where none exists, or returns
	// store.ErrAlreadyExists.
	Create(ctx context.Context, ring *grant.KeyRing) error
	// CompareAndSwap replaces the ring only if its revision is still rev.
	// It returns applied=false when another writer got there first.
	CompareAndSwap(ctx context.Context, ring *grant.KeyRing, rev int) (applied bool, err error)
}

// dbConduitGrantKeyStore stores the ring as an encrypted hub-scope secret.
// It is used only when an encryption key is configured.
type dbConduitGrantKeyStore struct {
	store         store.SecretStore
	encryptionKey []byte
}

func newDBConduitGrantKeyStore(st store.SecretStore, encryptionKey []byte) (*dbConduitGrantKeyStore, error) {
	if len(encryptionKey) == 0 {
		return nil, errors.New("conduit grant key ring: persistence requires an encryption key")
	}
	return &dbConduitGrantKeyStore{store: st, encryptionKey: encryptionKey}, nil
}

func (d *dbConduitGrantKeyStore) Load(ctx context.Context) (*grant.KeyRing, int, error) {
	rec, err := d.store.GetSecret(ctx, conduitGrantKeySecretName, store.ScopeHub, conduitGrantKeyScopeID)
	if err != nil {
		return nil, 0, err
	}
	if rec.EncryptedValue == "" {
		return nil, 0, store.ErrNotFound
	}
	plain, _, err := secret.DecryptValue(rec.EncryptedValue, d.encryptionKey)
	if err != nil {
		return nil, 0, fmt.Errorf("%w: decrypt: %w", errConduitGrantRingUnreadable, err)
	}
	var ring grant.KeyRing
	if err := json.Unmarshal([]byte(plain), &ring); err != nil {
		return nil, 0, fmt.Errorf("%w: decode: %w", errConduitGrantRingUnreadable, err)
	}
	if err := ring.Validate(); err != nil {
		return nil, 0, fmt.Errorf("%w: %w", errConduitGrantRingUnreadable, err)
	}
	return &ring, rec.Version, nil
}

func (d *dbConduitGrantKeyStore) encode(ring *grant.KeyRing) (string, error) {
	raw, err := json.Marshal(ring)
	if err != nil {
		return "", err
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

func (d *dbConduitGrantKeyStore) CompareAndSwap(ctx context.Context, ring *grant.KeyRing, rev int) (bool, error) {
	val, err := d.encode(ring)
	if err != nil {
		return false, err
	}
	return d.store.UpdateSecretValueIfVersion(ctx, conduitGrantKeySecretName, store.ScopeHub, conduitGrantKeyScopeID, rev, val)
}

// memoryConduitGrantKeyStore holds the ring in process memory only. It is
// used when no encryption key is configured, so the ring is never written
// to storage.
type memoryConduitGrantKeyStore struct {
	mu   sync.Mutex
	ring *grant.KeyRing
	rev  int
}

func cloneKeyRing(r *grant.KeyRing) *grant.KeyRing {
	return &grant.KeyRing{Keys: append([]grant.RingKey(nil), r.Keys...)}
}

func (m *memoryConduitGrantKeyStore) Load(context.Context) (*grant.KeyRing, int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.ring == nil {
		return nil, 0, store.ErrNotFound
	}
	return cloneKeyRing(m.ring), m.rev, nil
}

func (m *memoryConduitGrantKeyStore) Create(_ context.Context, ring *grant.KeyRing) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.ring != nil {
		return store.ErrAlreadyExists
	}
	m.ring, m.rev = cloneKeyRing(ring), 1
	return nil
}

func (m *memoryConduitGrantKeyStore) CompareAndSwap(_ context.Context, ring *grant.KeyRing, rev int) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.ring == nil || m.rev != rev {
		return false, nil
	}
	m.ring, m.rev = cloneKeyRing(ring), m.rev+1
	return true, nil
}

// conduitGrantKeys caches the ring for one hub node.
type conduitGrantKeys struct {
	store conduitGrantKeyStore
	now   func() time.Time

	mu       sync.Mutex
	ring     *grant.KeyRing
	loadedAt time.Time
	// unreadableLoggedAt rate-limits the unreadable-ring ERROR.
	unreadableLoggedAt time.Time
}

func newConduitGrantKeys(st conduitGrantKeyStore, now func() time.Time) *conduitGrantKeys {
	if now == nil {
		now = time.Now
	}
	return &conduitGrantKeys{store: st, now: now}
}

// currentLocked returns the cached ring, reloading it when stale and
// creating it when absent. Callers must hold k.mu.
func (k *conduitGrantKeys) currentLocked(ctx context.Context) (*grant.KeyRing, error) {
	now := k.now()
	if k.ring != nil && now.Sub(k.loadedAt) < conduitGrantKeyRefresh {
		return k.ring, nil
	}
	ring, _, err := k.store.Load(ctx)
	if errors.Is(err, store.ErrNotFound) {
		ring, err = k.bootstrap(ctx, now)
	}
	if err != nil {
		if errors.Is(err, errConduitGrantRingUnreadable) {
			k.logUnreadableLocked(now, err)
		}
		return nil, fmt.Errorf("conduit grant key ring: %w", err)
	}
	k.ring, k.loadedAt = ring, now
	return ring, nil
}

// logUnreadableLocked reports an unreadable stored ring at most once per
// conduitGrantKeyRefresh. Callers must hold k.mu.
func (k *conduitGrantKeys) logUnreadableLocked(now time.Time, err error) {
	if !k.unreadableLoggedAt.IsZero() && now.Sub(k.unreadableLoggedAt) < conduitGrantKeyRefresh {
		return
	}
	k.unreadableLoggedAt = now
	slog.Error("Conduit grant key ring cannot be read; Conduit grants are unavailable. "+
		"Check that every hub node has the same shared signing secret. "+
		"Discarding the ring is safe (only in-flight grants are lost): delete the hub-scope secret "+
		"and the next request creates a new ring.",
		"secret_key", conduitGrantKeySecretName,
		"scope", store.ScopeHub,
		"scope_id", conduitGrantKeyScopeID,
		"error", err)
}

// bootstrap creates the ring with one fresh random key, or loads the ring
// another node created first.
func (k *conduitGrantKeys) bootstrap(ctx context.Context, now time.Time) (*grant.KeyRing, error) {
	key, err := grant.NewRingKey(now, now)
	if err != nil {
		return nil, err
	}
	ring := &grant.KeyRing{Keys: []grant.RingKey{key}}
	err = k.store.Create(ctx, ring)
	if errors.Is(err, store.ErrAlreadyExists) {
		// Another node won the bootstrap; use its ring.
		ring, _, err = k.store.Load(ctx)
		return ring, err
	}
	if err != nil {
		return nil, err
	}
	slog.Info("Conduit grant key ring created", "kid", key.KeyID)
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

// rotate adds a new key and returns its kid. Each attempt rereads the stored
// ring and writes it back with a compare-and-swap on its revision, so a
// concurrent rotation (on this or another node) is never overwritten; after
// conduitGrantKeyRotateAttempts lost races it gives up with an error.
func (k *conduitGrantKeys) rotate(ctx context.Context, activateAfter, overlap time.Duration) (string, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if activateAfter < conduitGrantKeyRefresh {
		return "", fmt.Errorf("activation delay %s must be at least the ring refresh interval %s", activateAfter, conduitGrantKeyRefresh)
	}
	// Make sure a ring exists (bootstrapping it if needed).
	k.ring = nil
	if _, err := k.currentLocked(ctx); err != nil {
		return "", err
	}
	now := k.now()
	key, err := grant.NewRingKey(now, now)
	if err != nil {
		return "", err
	}
	for range conduitGrantKeyRotateAttempts {
		ring, rev, err := k.store.Load(ctx)
		if err != nil {
			return "", fmt.Errorf("load conduit grant key ring: %w", err)
		}
		next := cloneKeyRing(ring)
		next.Prune(now)
		if err := next.Rotate(now, key, activateAfter, overlap); err != nil {
			return "", err
		}
		applied, err := k.store.CompareAndSwap(ctx, next, rev)
		if err != nil {
			return "", fmt.Errorf("store rotated conduit grant key ring: %w", err)
		}
		if applied {
			k.ring, k.loadedAt = next, now
			slog.Info("Conduit grant key rotated", "kid", key.KeyID, "activate_at", now.Add(activateAfter))
			return key.KeyID, nil
		}
	}
	return "", fmt.Errorf("conduit grant key rotation: ring changed concurrently %d times; retry", conduitGrantKeyRotateAttempts)
}

// conduitGrantKeySet returns this node's ring cache, creating it on first
// use. No storage is touched until a key is needed.
func (s *Server) conduitGrantKeySet() *conduitGrantKeys {
	s.conduitGrantsOnce.Do(func() {
		if s.conduitGrants != nil {
			return
		}
		var st conduitGrantKeyStore
		if db, err := newDBConduitGrantKeyStore(s.store, s.encryptionKey); err == nil {
			st = db
		} else {
			slog.Info("Conduit grant key ring is ephemeral: no persistence configured; it is held in memory and regenerated on restart")
			if s.requiresSharedSigningKeys() {
				slog.Warn("Conduit grant key ring is per node: grants minted on one hub node will not verify " +
					"against another node's keys until a shared signing secret is configured")
			}
			st = &memoryConduitGrantKeyStore{}
		}
		s.conduitGrants = newConduitGrantKeys(st, nil)
	})
	return s.conduitGrants
}

// requiresSharedSigningKeys reports whether this deployment's nodes must
// agree on signing keys: the same signal the hub's other signing keys use
// (a GCP secret backend or RequireStableSigningKey).
func (s *Server) requiresSharedSigningKeys() bool {
	_, isGCPBackend := s.secretBackend.(*secret.GCPBackend)
	return isGCPBackend || s.config.RequireStableSigningKey
}

// conduitGrantKeyActivation returns the configured publish-before-sign
// delay, or the default.
func (s *Server) conduitGrantKeyActivation() time.Duration {
	if s.config.ConduitGrantKeyActivation > 0 {
		return s.config.ConduitGrantKeyActivation
	}
	return conduitGrantKeyDefaultActivation
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

// RotateConduitGrantKey adds a new grant signing key, activating after
// ServerConfig.ConduitGrantKeyActivation, and returns its kid. It is the hook for an operator rotation
// surface; none is wired in Phase 1.
func (s *Server) RotateConduitGrantKey(ctx context.Context) (string, error) {
	if !s.experimentEnabled(conduitExperiment) {
		return "", errConduitDisabled
	}
	return s.conduitGrantKeySet().rotate(ctx, s.conduitGrantKeyActivation(), conduitGrantKeyDefaultOverlap)
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
