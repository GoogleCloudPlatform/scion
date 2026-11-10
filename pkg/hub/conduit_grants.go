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
// a bounded number of times, so concurrent rotations never lose a key. A
// rotation is refused (errConduitGrantKeyPending, 409 at the admin route)
// while a key rotated in earlier has not activated yet, so the published
// key set grows by at most one pending key at a time.
// Operators rotate with POST /api/v1/admin/conduit/grant-keys/rotate
// (handleAdminConduitGrantKeyRotate), which is limited to hub admins.
//
// Retention. Rotation sets not_after on every outgoing key, so a grant signed
// with an old kid verifies until its own exp or the key's not_after,
// whichever comes first. Every ring load and write settles the ring
// (grant.KeyRing.Settle): once the rotated-in key activates, the outgoing
// key's seed is dropped, so it can verify until not_after but never sign
// again; a key past not_after moves to the ring's retained set, its public
// half only, kept for re-check verification until not_after plus the
// largest stream authorization interval plus 30s (conduitGrantKeyRetention;
// retention only grows). Retained keys are never published and never
// verify a stream grant at admission. Operators withdraw a key at once
// with POST /api/v1/admin/conduit/grant-keys/revoke
// (handleAdminConduitGrantKeyRevoke): rotate first, wait for the new key to
// activate, then revoke the old kid.
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
	// conduitGrantKeyDefaultRetention is the default retention of a retired
	// key's public half for re-check verification: the default user stream
	// authorization interval plus the targets' 30s allowance.
	conduitGrantKeyDefaultRetention = defaultConduitUserStreamAuthzMax + 30*time.Second
	// conduitGrantKeyRotateAttempts bounds the compare-and-swap retries of
	// one rotation.
	conduitGrantKeyRotateAttempts = 5
)

var (
	// errConduitGrantRingUnreadable marks a persisted ring that exists but
	// cannot be decrypted, decoded or validated.
	errConduitGrantRingUnreadable = errors.New("stored conduit grant key ring is unreadable")

	// errConduitGrantKeyPending refuses a rotation while a key rotated in
	// earlier has not activated yet (the handler maps it to 409).
	errConduitGrantKeyPending = errors.New("previous key not yet active")

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
	// CompareAndSwap replaces the ring only if its revision is still rev;
	// an applied swap stores it at revision rev+1. It returns
	// applied=false when another writer got there first.
	CompareAndSwap(ctx context.Context, ring *grant.KeyRing, rev int) (applied bool, err error)
}

// dbConduitGrantKeyStore stores the ring as an encrypted hub-scope secret,
// sealed by the server's shared ValueCipher (ptone/scion#1530 §5). It is
// used only when encryption is enabled.
type dbConduitGrantKeyStore struct {
	store  store.SecretStore
	cipher *secret.ValueCipher
}

func newDBConduitGrantKeyStore(st store.SecretStore, c *secret.ValueCipher) (*dbConduitGrantKeyStore, error) {
	if !c.Enabled() {
		return nil, errors.New("conduit grant key ring: persistence requires an encryption key")
	}
	return &dbConduitGrantKeyStore{store: st, cipher: c}, nil
}

func (d *dbConduitGrantKeyStore) Load(ctx context.Context) (*grant.KeyRing, int, error) {
	rec, err := d.store.GetSecret(ctx, conduitGrantKeySecretName, store.ScopeHub, conduitGrantKeyScopeID)
	if err != nil {
		return nil, 0, err
	}
	if rec == nil || rec.EncryptedValue == "" {
		return nil, 0, store.ErrNotFound
	}
	plain, err := d.cipher.Decrypt(rec.EncryptedValue)
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
	return d.cipher.Encrypt(string(raw))
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
	return &grant.KeyRing{
		Keys:     append([]grant.RingKey(nil), r.Keys...),
		Retained: append([]grant.RetainedKey(nil), r.Retained...),
		Revoked:  append([]grant.RevokedKey(nil), r.Revoked...),
	}
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
	// retain is how long a retired key's public half is kept for re-check
	// verification: the largest stream authorization interval plus the
	// targets' 30s allowance (design §3.5, Re-check). Nil means
	// conduitGrantKeyDefaultRetention.
	retain func() time.Duration

	mu   sync.Mutex
	ring *grant.KeyRing
	// loadedAt is when the read that produced ring began; the cache's
	// freshness is measured from it.
	loadedAt time.Time
	// gen counts changes to the cache: every install and every
	// invalidate bumps it. A fresh read installs its ring only if gen is
	// unchanged since the read began, so it never replaces a newer ring or
	// undoes an invalidate.
	gen uint64
	// reads numbers ring reads in the order they begin (nextReadLocked);
	// ringRead is the number of the read that produced ring. A ring whose
	// read began after a re-check snapshot (recheckKeysGen) is newer than
	// the keys that snapshot verified with.
	reads    uint64
	ringRead uint64
	// unreadableLoggedAt rate-limits the unreadable-ring ERROR.
	unreadableLoggedAt time.Time
}

func newConduitGrantKeys(st conduitGrantKeyStore, now func() time.Time) *conduitGrantKeys {
	if now == nil {
		now = time.Now
	}
	return &conduitGrantKeys{store: st, now: now}
}

// retention returns how long retired keys' public halves are kept.
func (k *conduitGrantKeys) retention() time.Duration {
	if k.retain != nil {
		return k.retain()
	}
	return conduitGrantKeyDefaultRetention
}

// currentLocked returns the cached ring, reloading it when stale and
// creating it when absent. A reloaded ring is settled (grant.KeyRing.Settle):
// a key with an activated successor loses its seed, retired keys move to
// the verify-only retained set, and retention only grows. A settled ring
// that changed is written back with a compare-and-swap. If that write loses
// to another writer, the newer stored ring is read once more and used
// instead. Callers must hold k.mu.
func (k *conduitGrantKeys) currentLocked(ctx context.Context) (*grant.KeyRing, error) {
	now := k.now()
	if k.ring != nil && now.Sub(k.loadedAt) < conduitGrantKeyRefresh {
		return k.ring, nil
	}
	read := k.nextReadLocked()
	ring, err := k.loadSettled(ctx)
	if errors.Is(err, store.ErrNotFound) {
		ring, err = k.bootstrap(ctx, now)
	}
	if err != nil {
		if errors.Is(err, errConduitGrantRingUnreadable) {
			k.logUnreadableLocked(now, err)
		}
		return nil, fmt.Errorf("conduit grant key ring: %w", err)
	}
	k.ring, k.loadedAt, k.ringRead = ring, now, read
	k.gen++
	return ring, nil
}

// nextReadLocked numbers a ring read that is about to begin. Callers must
// hold k.mu.
func (k *conduitGrantKeys) nextReadLocked() uint64 {
	k.reads++
	return k.reads
}

// loadRing reads the stored ring and its revision. It returns the store's
// error unchanged (so store.ErrNotFound still selects the bootstrap). A
// store that returns neither a ring nor an error is an unreadable ring
// (errConduitGrantRingUnreadable), never an empty one.
func (k *conduitGrantKeys) loadRing(ctx context.Context) (*grant.KeyRing, int, error) {
	ring, rev, err := k.store.Load(ctx)
	if err != nil {
		return nil, 0, err
	}
	if ring == nil {
		return nil, 0, fmt.Errorf("%w: the store returned no ring", errConduitGrantRingUnreadable)
	}
	return ring, rev, nil
}

// loadSettled reads the stored ring and settles it. A settled ring that
// changed is written back with a compare-and-swap. If that write loses to
// another writer (for example a revocation), the newer stored ring is read
// once more and settled in memory only, rather than returning a ring settled
// from the superseded revision. It does not touch the cache, so callers need
// not hold k.mu.
func (k *conduitGrantKeys) loadSettled(ctx context.Context) (*grant.KeyRing, error) {
	now := k.now()
	ring, rev, err := k.loadRing(ctx)
	if err != nil {
		return nil, err
	}
	next := cloneKeyRing(ring)
	if !next.Settle(now, k.retention()) {
		return ring, nil
	}
	applied, cerr := k.store.CompareAndSwap(ctx, next, rev)
	switch {
	case cerr != nil:
		slog.Warn("Conduit grant key ring: could not store the settled ring", "error", cerr)
		return next, nil
	case applied:
		return next, nil
	}
	if ring, _, err = k.loadRing(ctx); err != nil {
		return nil, err
	}
	ring = cloneKeyRing(ring)
	ring.Settle(now, k.retention())
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
		ring, _, err = k.loadRing(ctx)
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

// settleNow rereads the stored ring and settles it, whatever the cache's
// age; currentLocked writes a ring that changed back to storage.
func (k *conduitGrantKeys) settleNow(ctx context.Context) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.ring = nil
	_, err := k.currentLocked(ctx)
	return err
}

// conduitGrantKeyRecheckFresh is the oldest a cached ring may be when
// re-check verification uses it; an older one is read again first.
const conduitGrantKeyRecheckFresh = 5 * time.Second

// freshPublicKeys returns every published verification key from a fresh
// read of the stored ring (one secret row), bypassing the cache. The
// hub-direct channel (the grant-key route and the token-refresh response)
// uses it, so a target's fetch reflects every rotation and revocation stored
// before the fetch arrived, whichever hub node serves it. The read and its
// settle run without k.mu, so concurrent fetches never hold up signing;
// the lock is taken only to cache the result, and only if the cache has not
// changed (no install, no invalidate) since the read began. With no stored ring yet, the ring is created
// under the lock as on every other path.
func (k *conduitGrantKeys) freshPublicKeys(ctx context.Context) ([]grant.PublicKey, error) {
	k.mu.Lock()
	gen, read := k.gen, k.nextReadLocked()
	readStart := k.now()
	k.mu.Unlock()
	ring, err := k.loadSettled(ctx)
	if errors.Is(err, store.ErrNotFound) {
		k.mu.Lock()
		defer k.mu.Unlock()
		k.ring = nil
		ring, err := k.currentLocked(ctx)
		if err != nil {
			return nil, err
		}
		return ring.PublicKeys(k.now()), nil
	}
	now := k.now()
	if err != nil {
		if errors.Is(err, errConduitGrantRingUnreadable) {
			k.mu.Lock()
			k.logUnreadableLocked(now, err)
			k.mu.Unlock()
		}
		return nil, fmt.Errorf("conduit grant key ring: %w", err)
	}
	k.mu.Lock()
	if k.gen == gen {
		// The cached ring's age counts from when its read began, not from
		// when a slow read ended.
		k.ring, k.loadedAt, k.ringRead = ring, readStart, read
		k.gen++
	}
	k.mu.Unlock()
	return ring.PublicKeys(now), nil
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

// ConduitGrantKeyRotation describes a completed rotation: kids and
// timestamps only, never key material.
type ConduitGrantKeyRotation struct {
	// KeyID is the kid of the key rotated in.
	KeyID string `json:"kid"`
	// ActivateAt is when the new key starts signing.
	ActivateAt time.Time `json:"activate_at"`
	// Retiring lists the outgoing keys and when each is retired.
	Retiring []ConduitGrantKeyRetirement `json:"retiring"`
}

// ConduitGrantKeyRetirement is one outgoing key in a rotation.
type ConduitGrantKeyRetirement struct {
	KeyID    string    `json:"kid"`
	NotAfter time.Time `json:"not_after"`
}

// rotate adds a new key and reports its kid, activation time and the
// retirement time of every outgoing key. Each attempt rereads the stored
// ring and writes it back with a compare-and-swap on its revision, so a
// concurrent rotation (on this or another node) is never overwritten; after
// conduitGrantKeyRotateAttempts lost races it gives up with an error. It
// refuses with errConduitGrantKeyPending, leaving the ring unchanged, while
// the stored ring holds a key whose activation is still in the future; the
// check runs on each attempt's freshly loaded ring, so it also holds against
// a rotation that landed concurrently.
func (k *conduitGrantKeys) rotate(ctx context.Context, activateAfter, overlap time.Duration) (ConduitGrantKeyRotation, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if activateAfter < conduitGrantKeyRefresh {
		return ConduitGrantKeyRotation{}, fmt.Errorf("activation delay %s must be at least the ring refresh interval %s", activateAfter, conduitGrantKeyRefresh)
	}
	// Make sure a ring exists (bootstrapping it if needed).
	k.ring = nil
	if _, err := k.currentLocked(ctx); err != nil {
		return ConduitGrantKeyRotation{}, err
	}
	now := k.now()
	key, err := grant.NewRingKey(now, now)
	if err != nil {
		return ConduitGrantKeyRotation{}, err
	}
	for range conduitGrantKeyRotateAttempts {
		read := k.nextReadLocked()
		ring, rev, err := k.loadRing(ctx)
		if err != nil {
			return ConduitGrantKeyRotation{}, fmt.Errorf("load conduit grant key ring: %w", err)
		}
		if hasPendingConduitGrantKey(ring, now) {
			return ConduitGrantKeyRotation{}, errConduitGrantKeyPending
		}
		next := cloneKeyRing(ring)
		next.Settle(now, k.retention())
		if err := next.Rotate(now, key, activateAfter, overlap); err != nil {
			return ConduitGrantKeyRotation{}, err
		}
		applied, err := k.store.CompareAndSwap(ctx, next, rev)
		if err != nil {
			return ConduitGrantKeyRotation{}, fmt.Errorf("store rotated conduit grant key ring: %w", err)
		}
		if applied {
			k.ring, k.loadedAt, k.ringRead = next, now, read
			k.gen++
			out := ConduitGrantKeyRotation{KeyID: key.KeyID, Retiring: []ConduitGrantKeyRetirement{}}
			for _, rk := range next.Keys {
				if rk.KeyID == key.KeyID {
					out.ActivateAt = rk.ActivateAt
					continue
				}
				out.Retiring = append(out.Retiring, ConduitGrantKeyRetirement{KeyID: rk.KeyID, NotAfter: rk.NotAfter})
			}
			slog.Info("Conduit grant key rotated", "kid", key.KeyID, "activate_at", out.ActivateAt, "retiring", len(out.Retiring))
			return out, nil
		}
	}
	return ConduitGrantKeyRotation{}, fmt.Errorf("conduit grant key rotation: ring changed concurrently %d times; retry", conduitGrantKeyRotateAttempts)
}

// errConduitGrantKeyRevokeActive refuses revoking the key that signs now
// (the handler maps it to 409); errConduitGrantKeyUnknown is a kid the ring
// does not hold (404).
var (
	errConduitGrantKeyRevokeActive = errors.New("the active signing key cannot be revoked; rotate first")
	errConduitGrantKeyUnknown      = errors.New("no such grant key")
)

// revoke removes kid from the ring at once, from the published keys and
// from the verify-only retained set, so it verifies nothing from then on,
// in admission or in re-check. Like rotate it rereads the stored ring and
// writes it back with a compare-and-swap, retrying a bounded number of
// times.
func (k *conduitGrantKeys) revoke(ctx context.Context, kid string) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.ring = nil
	if _, err := k.currentLocked(ctx); err != nil {
		return err
	}
	now := k.now()
	for range conduitGrantKeyRotateAttempts {
		read := k.nextReadLocked()
		ring, rev, err := k.loadRing(ctx)
		if err != nil {
			return fmt.Errorf("load conduit grant key ring: %w", err)
		}
		next := cloneKeyRing(ring)
		next.Settle(now, k.retention())
		switch err := next.Revoke(now, kid); {
		case errors.Is(err, grant.ErrRevokeActiveSigner):
			return errConduitGrantKeyRevokeActive
		case errors.Is(err, grant.ErrRevokeUnknown):
			return errConduitGrantKeyUnknown
		case err != nil:
			return err
		}
		applied, err := k.store.CompareAndSwap(ctx, next, rev)
		if err != nil {
			return fmt.Errorf("store conduit grant key ring: %w", err)
		}
		if applied {
			k.ring, k.loadedAt, k.ringRead = next, now, read
			k.gen++
			slog.Warn("Conduit grant key revoked", "kid", kid)
			return nil
		}
	}
	return fmt.Errorf("conduit grant key revoke: ring changed concurrently %d times; retry", conduitGrantKeyRotateAttempts)
}

// invalidate drops the cached ring, so the next use reloads it from the
// store.
func (k *conduitGrantKeys) invalidate() {
	k.mu.Lock()
	k.ring = nil
	k.gen++
	k.mu.Unlock()
}

// recheckKeys returns the verify-only key set for re-check verification:
// the ring's keys and its retained public halves (grant.KeyRing.RecheckKeys).
// It is never published and never verifies a stream grant at admission.
//
// A cached ring older than conduitGrantKeyRecheckFresh is read again first,
// so a revocation stored by another node applies here within that time.
func (k *conduitGrantKeys) recheckKeys(ctx context.Context) (grant.KeySet, error) {
	keys, _, err := k.recheckKeysGen(ctx)
	return keys, err
}

// recheckKeysGen is recheckKeys, also returning a snapshot for
// reloadRecheckKeys: the number of ring reads begun before this call. The
// snapshot is taken in the same critical section as the returned keys and
// before this call's own load of a stale or missing ring, so that load,
// which is a fresh read of the store, counts as newer than the snapshot.
func (k *conduitGrantKeys) recheckKeysGen(ctx context.Context) (grant.KeySet, uint64, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	snapshot := k.reads
	if k.ring != nil && k.now().Sub(k.loadedAt) >= conduitGrantKeyRecheckFresh {
		k.ring = nil
	}
	ring, err := k.currentLocked(ctx)
	if err != nil {
		return nil, 0, err
	}
	return ring.RecheckKeys(k.now(), k.retention()), snapshot, nil
}

// reloadRecheckKeys returns the re-check key set after one reload of the
// ring from storage, for a grant whose kid was unknown in the keys of
// snapshot seen (design §3.5, Unknown kid). A cached ring from a read that
// began after the snapshot (another renewal's reload, a key change applied
// here, or a fresh read) is used without another read, so many streams
// meeting the same new kid share one reload. A ring from a read that began
// at or before the snapshot is read again, even when that read installed it
// later, so a slow read never stands in for the reload. A failed reload
// returns its error. A replica that lags the store can still serve the
// reload an older ring; one reload is what the design promises.
func (k *conduitGrantKeys) reloadRecheckKeys(ctx context.Context, seen uint64) (grant.KeySet, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.ring == nil || k.ringRead <= seen {
		k.ring = nil
		k.gen++
	}
	ring, err := k.currentLocked(ctx)
	if err != nil {
		return nil, err
	}
	return ring.RecheckKeys(k.now(), k.retention()), nil
}

// hasPendingConduitGrantKey reports whether ring holds a key that has not
// activated yet at now.
func hasPendingConduitGrantKey(ring *grant.KeyRing, now time.Time) bool {
	for _, k := range ring.Keys {
		if k.ActivateAt.After(now) {
			return true
		}
	}
	return false
}

// conduitGrantKeySet returns this node's ring cache, creating it on first
// use. No storage is touched until a key is needed.
func (s *Server) conduitGrantKeySet() *conduitGrantKeys {
	s.conduitGrantsOnce.Do(func() {
		if s.conduitGrants != nil {
			return
		}
		var st conduitGrantKeyStore
		if db, err := newDBConduitGrantKeyStore(s.store, s.cipher); err == nil {
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
		s.conduitGrants.retain = s.conduitGrantKeyRetention
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

// conduitGrantKeyRetention is how long a retired key's public half is kept
// for re-check verification: the largest stream authorization interval in
// effect (only user streams carry one) plus the targets' 30s allowance.
// Raising the interval later extends retention on the next ring load; it
// never shortens it.
func (s *Server) conduitGrantKeyRetention() time.Duration {
	return s.conduitUserStreamAuthzInterval() + 30*time.Second
}

// RevokeConduitGrantKey removes a grant key from the ring at once, to
// withdraw it before its not_after: it stops verifying anything, and
// streams whose latest grant it signed get no further renewal. The key that
// signs now cannot be revoked; rotate first and wait for the new key to
// activate. POST /api/v1/admin/conduit/grant-keys/revoke calls it.
func (s *Server) RevokeConduitGrantKey(ctx context.Context, kid string) error {
	if !s.experimentEnabled(conduitExperiment) {
		return errConduitDisabled
	}
	if err := s.conduitGrantKeySet().revoke(ctx, kid); err != nil {
		return err
	}
	s.publishConduitGrantKeysChanged(kid)
	return nil
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

// conduitHubDirectGrantKeys returns the grant verification keys for the
// hub-direct channel (GET /api/v1/conduit/grant-keys and the token-refresh
// response), from a fresh read of the stored ring (freshPublicKeys). It
// returns errConduitDisabled when the experiment is off.
func (s *Server) conduitHubDirectGrantKeys(ctx context.Context) ([]grant.PublicKey, error) {
	if !s.experimentEnabled(conduitExperiment) {
		return nil, errConduitDisabled
	}
	return s.conduitGrantKeySet().freshPublicKeys(ctx)
}

// RotateConduitGrantKey adds a new grant signing key, activating after
// ServerConfig.ConduitGrantKeyActivation, and retires every older key one
// conduitGrantKeyDefaultOverlap after that. POST
// /api/v1/admin/conduit/grant-keys/rotate calls it.
func (s *Server) RotateConduitGrantKey(ctx context.Context) (ConduitGrantKeyRotation, error) {
	if !s.experimentEnabled(conduitExperiment) {
		return ConduitGrantKeyRotation{}, errConduitDisabled
	}
	rot, err := s.conduitGrantKeySet().rotate(ctx, s.conduitGrantKeyActivation(), conduitGrantKeyDefaultOverlap)
	if err != nil {
		return ConduitGrantKeyRotation{}, err
	}
	s.publishConduitGrantKeysChanged(rot.KeyID)
	return rot, nil
}

// conduitGrantKeysResponse is the body of GET /api/v1/conduit/grant-keys.
type conduitGrantKeysResponse struct {
	Keys []grant.WireKey `json:"keys"`
	// TargetStreamDeadlines is the enforcement mode of target-side stream
	// deadlines, "off" or "enforce" (design §3.5 Gating). Targets take it
	// only from this hub-direct response.
	TargetStreamDeadlines string `json:"target_stream_deadlines"`
}

// handleConduitGrantKeys serves GET /api/v1/conduit/grant-keys: the public
// verification keys only, from a fresh read of the stored ring
// (conduitHubDirectGrantKeys), so the answer is the same on every hub node.
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
	keys, err := s.conduitHubDirectGrantKeys(r.Context())
	if err != nil {
		slog.Error("conduit grant keys unavailable", "error", err)
		writeError(w, http.StatusServiceUnavailable, ErrCodeInternalError, "grant keys unavailable", nil)
		return
	}
	writeJSON(w, http.StatusOK, conduitGrantKeysResponse{Keys: grant.ToWire(keys), TargetStreamDeadlines: s.conduitTargetStreamDeadlines()})
}

// handleAdminConduitGrantKeyRotate serves POST
// /api/v1/admin/conduit/grant-keys/rotate. Route metadata (RouteHubAdmin,
// permission hub.conduit_grant_keys.execute) authorizes the caller before the
// handler runs. The response carries kids and timestamps only; key material
// is never returned. The experiment is checked per request (404 when off),
// as in handleConduitGrantKeys.
func (s *Server) handleAdminConduitGrantKeyRotate(w http.ResponseWriter, r *http.Request) {
	if !s.experimentEnabled(conduitExperiment) {
		NotFound(w, "route")
		return
	}
	if r.Method != http.MethodPost {
		MethodNotAllowed(w, http.MethodPost)
		return
	}
	out, err := s.RotateConduitGrantKey(r.Context())
	if errors.Is(err, errConduitGrantKeyPending) {
		slog.Info("Conduit grant key rotation refused: previous key not yet active")
		writeError(w, http.StatusConflict, ErrCodeConflict, "previous key not yet active", nil)
		return
	}
	if err != nil {
		slog.Error("conduit grant key rotation failed", "error", err)
		writeError(w, http.StatusServiceUnavailable, ErrCodeInternalError, "grant key rotation failed", nil)
		return
	}
	actor := ""
	if id := GetIdentityFromContext(r.Context()); id != nil {
		actor = id.ID()
	}
	slog.Info("Conduit grant key rotation requested", "actor_id", actor, "kid", out.KeyID)
	writeJSON(w, http.StatusOK, out)
}

// conduitGrantKeyRevokeRequest is the body of POST
// /api/v1/admin/conduit/grant-keys/revoke.
type conduitGrantKeyRevokeRequest struct {
	KeyID string `json:"kid"`
}

// handleAdminConduitGrantKeyRevoke serves POST
// /api/v1/admin/conduit/grant-keys/revoke {"kid": ...}. Route metadata
// (RouteHubAdmin, permission hub.conduit_grant_keys.execute) authorizes the
// caller before the handler runs. The key is removed at once: it no longer
// verifies anything, and streams whose latest grant it signed get no
// further renewal. Responses: 200 {"kid"}; 400 without a kid; 404 for a kid
// the ring does not hold, or when hub.conduit is off; 409 for the key that
// signs now (rotate first and wait for the new key to activate).
func (s *Server) handleAdminConduitGrantKeyRevoke(w http.ResponseWriter, r *http.Request) {
	if !s.experimentEnabled(conduitExperiment) {
		NotFound(w, "route")
		return
	}
	if r.Method != http.MethodPost {
		MethodNotAllowed(w, http.MethodPost)
		return
	}
	var req conduitGrantKeyRevokeRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil || req.KeyID == "" {
		writeError(w, http.StatusBadRequest, ErrCodeValidationError, "kid is required", nil)
		return
	}
	err := s.RevokeConduitGrantKey(r.Context(), req.KeyID)
	switch {
	case errors.Is(err, errConduitGrantKeyUnknown):
		writeError(w, http.StatusNotFound, ErrCodeNotFound, "no such grant key", nil)
		return
	case errors.Is(err, errConduitGrantKeyRevokeActive):
		writeError(w, http.StatusConflict, ErrCodeConflict, "the active signing key cannot be revoked; rotate first", nil)
		return
	case err != nil:
		slog.Error("conduit grant key revocation failed", "error", err)
		writeError(w, http.StatusServiceUnavailable, ErrCodeInternalError, "grant key revocation failed", nil)
		return
	}
	actor := ""
	if id := GetIdentityFromContext(r.Context()); id != nil {
		actor = id.ID()
	}
	slog.Warn("Conduit grant key revocation requested", "actor_id", actor, "kid", req.KeyID)
	writeJSON(w, http.StatusOK, conduitGrantKeyRevokeRequest{KeyID: req.KeyID})
}
