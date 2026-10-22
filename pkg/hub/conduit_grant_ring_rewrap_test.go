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

//go:build !no_sqlite

package hub

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit/grant"
	"github.com/GoogleCloudPlatform/scion/pkg/keyring"
	"github.com/GoogleCloudPlatform/scion/pkg/secret"
	"github.com/GoogleCloudPlatform/scion/pkg/secret/ciphertest"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// The conduit grant ring's rotation semantics across encryption rewrap
// (ptone/scion#1530 Phase 5b): re-encrypting the stored ring changes only
// its ciphertext, never its keys, activation times or retention metadata,
// and Ed25519 rotation, activation, revocation and retention keep working
// across replicas against the v2-stored ring. Every key is a synthetic
// ciphertest fixture or a freshly generated grant key.

const (
	// rwActivate and rwOverlap are the rotation delays the drill uses
	// (rotate needs activateAfter >= conduitGrantKeyRefresh and an overlap
	// >= grant.MaxValidity).
	rwActivate = 2 * time.Minute
	rwOverlap  = 2 * time.Minute
	// rwRetain is the retention of retired public halves and revoked kids.
	rwRetain = 10 * time.Minute
)

var rwT0 = time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)

// rwV1Cipher is the legacy-material v1 cipher the ring starts under.
func rwV1Cipher(t *testing.T) *secret.ValueCipher {
	t.Helper()
	return ciphertest.Cipher(t, ciphertest.Ring{Active: keyring.LegacyID, IDs: []string{keyring.LegacyID},
		WriteFormat: keyring.WriteV1, V1Readers: []string{keyring.LegacyID}})
}

// rwKeys returns grant keys over the stored ring through cipher c, on
// clock, with the drill's retention.
func rwKeys(t *testing.T, st store.Store, c *secret.ValueCipher, clock *conduitTestClock) *conduitGrantKeys {
	t.Helper()
	db, err := newDBConduitGrantKeyStore(st, c)
	require.NoError(t, err)
	k := newConduitGrantKeys(db, clock.Now)
	k.retain = func() time.Duration { return rwRetain }
	return k
}

// rwKids names the keys of the rotated ring.
type rwKids struct {
	retained, active, revoked, pending string
}

// buildRotatedRing creates, under the v1 cipher, a ring that carries every
// kind of rotation state, and returns its kids. At rwT0+5m the stored ring
// is: Keys {active (signing, not_after set), pending (activates later)},
// Retained {the first key, past its not_after, with retain_until},
// Revoked {a revoked pending key}.
func buildRotatedRing(t *testing.T, st store.Store, clock *conduitTestClock) rwKids {
	t.Helper()
	ctx := context.Background()
	k := rwKeys(t, st, rwV1Cipher(t), clock)
	first, err := k.signer(ctx) // bootstrap
	require.NoError(t, err)
	r1, err := k.rotate(ctx, rwActivate, rwOverlap) // second key, activates at +2m; first retires at +4m
	require.NoError(t, err)
	clock.Advance(5 * time.Minute) // past the first key's not_after
	r2, err := k.rotate(ctx, rwActivate, rwOverlap)
	require.NoError(t, err)
	require.NoError(t, k.revoke(ctx, r2.KeyID)) // revoke the pending key
	r3, err := k.rotate(ctx, rwActivate, rwOverlap)
	require.NoError(t, err)
	kids := rwKids{retained: first.KeyID, active: r1.KeyID, revoked: r2.KeyID, pending: r3.KeyID}

	ring, _, err := k.store.Load(ctx)
	require.NoError(t, err)
	require.Len(t, ring.Keys, 2)
	byKid := map[string]grant.RingKey{}
	for _, rk := range ring.Keys {
		byKid[rk.KeyID] = rk
	}
	require.Contains(t, byKid, kids.active)
	require.Contains(t, byKid, kids.pending)
	assert.False(t, byKid[kids.active].NotAfter.IsZero(), "the outgoing key has a not_after")
	assert.True(t, byKid[kids.pending].ActivateAt.After(clock.Now()), "the successor is pending")
	require.Len(t, ring.Retained, 1)
	assert.Equal(t, kids.retained, ring.Retained[0].KeyID)
	assert.False(t, ring.Retained[0].RetainUntil.IsZero())
	require.Len(t, ring.Revoked, 1)
	assert.Equal(t, kids.revoked, ring.Revoked[0].KeyID)
	return kids
}

// grantRow returns the stored grant ring row.
func grantRow(t *testing.T, st store.Store) *store.Secret {
	t.Helper()
	row, err := st.GetSecret(context.Background(), conduitGrantKeySecretName, store.ScopeHub, conduitGrantKeyScopeID)
	require.NoError(t, err)
	return row
}

// rewrapGrantRow re-encrypts the grant ring row with to (which must read
// the current ciphertext) through ReplaceSecretCiphertext, as the
// migration does, and returns the row before the rewrap.
func rewrapGrantRow(t *testing.T, st store.Store, to *secret.ValueCipher) store.SecretPayload {
	t.Helper()
	ctx := context.Background()
	page, err := st.ScanSecretPayloads(ctx, "", store.MaxSecretPayloadScan)
	require.NoError(t, err)
	for _, p := range page {
		if p.Key != conduitGrantKeySecretName || p.Scope != store.ScopeHub || p.ScopeID != conduitGrantKeyScopeID {
			continue
		}
		plain, err := to.Decrypt(p.EncryptedValue)
		require.NoError(t, err)
		repl, err := to.Encrypt(plain)
		require.NoError(t, err)
		applied, err := st.ReplaceSecretCiphertext(ctx, p.ID, p.Version, p.EncryptedValue, repl)
		require.NoError(t, err)
		require.True(t, applied)
		return p
	}
	t.Fatal("no grant ring row to rewrap")
	return store.SecretPayload{}
}

// publicKeyBytes maps each published kid to its key bytes.
func publicKeyBytes(pubs []grant.PublicKey) map[string][]byte {
	out := map[string][]byte{}
	for _, p := range pubs {
		out[p.KeyID] = append([]byte(nil), p.Key...)
	}
	return out
}

func assertSameKeyBytes(t *testing.T, want, got map[string][]byte, what string) {
	t.Helper()
	require.Len(t, got, len(want), what)
	for kid, b := range want {
		assert.True(t, bytes.Equal(b, got[kid]), "%s: key %s differs or is missing", what, kid)
	}
}

// recheckLookup reports whether keys holds kid and returns its key bytes.
func recheckLookup(keys grant.KeySet, kid string) ([]byte, bool) {
	pk, ok := keys.Lookup(kid)
	if !ok {
		return nil, false
	}
	return append([]byte(nil), pk.Key...), true
}

// rwClaims is a minimal valid stream grant at now.
func rwClaims(now time.Time) grant.Claims {
	return grant.Claims{
		Issuer:    conduitGrantIssuer,
		Subject:   "user-rw",
		ProjectID: "proj-rw",
		Target:    grant.Target{Kind: grant.TargetKindAgent, ID: "agent-rw", EndpointIncarnation: "inc-1", SessionID: "sess-1", ConnectionEpoch: 1},
		Stream:    grant.StreamHeader{Kind: grant.StreamKindTCP, Params: map[string]string{grant.ParamHost: "127.0.0.1", grant.ParamPort: "3000"}},
		NotBefore: now,
		Expiry:    now.Add(30 * time.Second),
	}
}

// verifyGrant verifies tok against pubs as a target would.
func verifyGrant(t *testing.T, tok []byte, pubs []grant.PublicKey, now time.Time) error {
	t.Helper()
	keys, err := grant.NewKeySet(pubs...)
	require.NoError(t, err)
	c := rwClaims(now)
	_, err = grant.Verify(context.Background(), tok, keys, grant.Expectation{
		Target: c.Target, Header: c.Stream, ProjectID: c.ProjectID, Issuer: conduitGrantIssuer,
	}, grant.NewMemoryReplayCache(func() time.Time { return now }, 0), now)
	return err
}

// TestConduitGrantRing_RewrapPreservesKeysActivationRetention (P5
// acceptance): re-encrypting a ring that carries rotation state (an
// outgoing signing key with not_after, a pending successor, a retained
// retired key, a revoked kid) from v1 to v2 changes only the ciphertext.
// A reader still holding the old key and one holding only the new key both
// see the identical ring (seeds, public keys, created/activate/not_after,
// retained retain_until, revoked_at), the same row Version, the same
// published and re-check key sets, and the same signer, whose grants
// verify under the key set published before the rewrap.
func TestConduitGrantRing_RewrapPreservesKeysActivationRetention(t *testing.T) {
	st := encStore(t)
	ctx := context.Background()
	clock := &conduitTestClock{now: rwT0}
	kids := buildRotatedRing(t, st, clock)

	orig := rwKeys(t, st, rwV1Cipher(t), clock)
	ringBefore, _, err := orig.store.Load(ctx)
	require.NoError(t, err)
	pubsBefore, err := orig.publicKeys(ctx)
	require.NoError(t, err)
	recheckBefore, err := orig.recheckKeys(ctx)
	require.NoError(t, err)
	signerBefore, err := orig.signer(ctx)
	require.NoError(t, err)
	require.Equal(t, kids.active, signerBefore.KeyID)
	rowBefore := grantRow(t, st)
	require.True(t, strings.HasPrefix(rowBefore.EncryptedValue, "enc:v1:"))

	both := v2Cipher(t, "k2", keyring.LegacyID, "k2")
	p := rewrapGrantRow(t, st, both)
	assert.Equal(t, rowBefore.Version, p.Version)

	for _, reader := range []struct {
		name string
		c    *secret.ValueCipher
	}{
		{"reader holding the old and the new key", both},
		{"reader holding only the new key", v2Cipher(t, "k2", "k2")},
	} {
		t.Run(reader.name, func(t *testing.T) {
			row := grantRow(t, st)
			assert.True(t, strings.HasPrefix(row.EncryptedValue, "enc:v2:k2:"), "the row is v2 under k2")
			assert.Equal(t, rowBefore.Version, row.Version, "rewrap keeps the logical Version")
			assert.Equal(t, rowBefore.SecretRef, row.SecretRef)

			k := rwKeys(t, st, reader.c, clock)
			ringAfter, rev, err := k.store.Load(ctx)
			require.NoError(t, err)
			assert.Equal(t, rowBefore.Version, rev)
			assertSameGrantRing(t, ringBefore, ringAfter)

			pubs, err := k.publicKeys(ctx)
			require.NoError(t, err)
			assertSameKeyBytes(t, publicKeyBytes(pubsBefore), publicKeyBytes(pubs), "published keys")

			recheck, err := k.recheckKeys(ctx)
			require.NoError(t, err)
			for _, kid := range []string{kids.retained, kids.active, kids.pending} {
				want, ok := recheckLookup(recheckBefore, kid)
				require.True(t, ok, "re-check key %s before the rewrap", kid)
				got, ok := recheckLookup(recheck, kid)
				require.True(t, ok, "re-check key %s after the rewrap", kid)
				assert.True(t, bytes.Equal(want, got), "re-check key %s changed", kid)
			}
			_, ok := recheckLookup(recheck, kids.revoked)
			assert.False(t, ok, "the revoked kid stays out of the re-check set")

			signer, err := k.signer(ctx)
			require.NoError(t, err)
			assert.Equal(t, signerBefore.KeyID, signer.KeyID)
			assert.True(t, bytes.Equal(signerBefore.Key, signer.Key), "the signing key changed")
			tok, err := grant.Mint(signer, rwClaims(clock.Now()))
			require.NoError(t, err)
			assert.NoError(t, verifyGrant(t, tok, pubsBefore, clock.Now()), "a grant signed after the rewrap verifies under the keys published before it")

			// Reading settled nothing new: the row is untouched.
			after := grantRow(t, st)
			assert.True(t, row.EncryptedValue == after.EncryptedValue, "reading rewrote the ring row")
			assert.Equal(t, row.Version, after.Version)
		})
	}
}

// rwReplica is a real hub Server over st with cipher c whose grant keys
// use the Server's own cipher, the shared clock and the drill's retention.
func rwReplica(t *testing.T, st store.Store, c *secret.ValueCipher, clock *conduitTestClock) *Server {
	t.Helper()
	srv := encServer(t, st, c, false)
	require.Same(t, c, srv.cipher, "the Server keeps the injected cipher")
	srv.conduitGrants = rwKeys(t, st, srv.cipher, clock)
	return srv
}

// TestConduitGrantRing_Ed25519RotationAcrossReplicasOnV2 (P5: existing
// Ed25519 rotation semantics against a v2-stored ring): two real hub
// Servers over one store, after the rewrap, rotate, publish, activate,
// revoke and retire grant keys exactly as before. A replica whose cipher
// cannot read a ring rewritten under a newer writer key refuses: it
// neither signs, nor rotates, nor overwrites or recreates the ring.
func TestConduitGrantRing_Ed25519RotationAcrossReplicasOnV2(t *testing.T) {
	st := encStore(t)
	ctx := context.Background()
	clock := &conduitTestClock{now: rwT0}
	kids := buildRotatedRing(t, st, clock)
	cAB := v2Cipher(t, "k2", keyring.LegacyID, "k2")
	rewrapGrantRow(t, st, cAB)

	a := rwReplica(t, st, cAB, clock)
	b := rwReplica(t, st, v2Cipher(t, "k2", keyring.LegacyID, "k2"), clock)
	ka, kb := a.conduitGrantKeySet(), b.conduitGrantKeySet()

	// The pending key activates at +7m; the outgoing key retires at +9m.
	clock.Advance(3 * time.Minute) // +8m
	sa, err := ka.signer(ctx)
	require.NoError(t, err)
	assert.Equal(t, kids.pending, sa.KeyID, "the pending successor activated")

	// Past the outgoing key's not_after. B warms its cache, then A rotates.
	clock.Advance(2 * time.Minute) // +10m
	_, err = kb.publicKeys(ctx)
	require.NoError(t, err)
	before := grantRow(t, st)
	rot, err := ka.rotate(ctx, rwActivate, rwOverlap)
	require.NoError(t, err)
	row := grantRow(t, st)
	assert.True(t, strings.HasPrefix(row.EncryptedValue, "enc:v2:k2:"), "the rotation is stored v2 under the active writer")
	assert.Greater(t, row.Version, before.Version)

	// B's cached ring does not have the new key until it refreshes; then B
	// publishes it before it signs.
	pubsB, err := kb.publicKeys(ctx)
	require.NoError(t, err)
	assert.NotContains(t, publicKeyBytes(pubsB), rot.KeyID, "B's warm cache predates the rotation")
	clock.Advance(conduitGrantKeyRefresh + time.Second) // +11m01s, before activation at +12m
	pubsB, err = kb.publicKeys(ctx)
	require.NoError(t, err)
	assert.Contains(t, publicKeyBytes(pubsB), rot.KeyID, "B publishes the rotated-in key")
	sb, err := kb.signer(ctx)
	require.NoError(t, err)
	assert.Equal(t, kids.pending, sb.KeyID, "B does not sign with it yet")

	// After activation a grant signed on A verifies with B's published keys.
	clock.Advance(time.Minute) // +12m01s
	sa, err = ka.signer(ctx)
	require.NoError(t, err)
	require.Equal(t, rot.KeyID, sa.KeyID)
	tok, err := grant.Mint(sa, rwClaims(clock.Now()))
	require.NoError(t, err)
	pubsB, err = kb.freshPublicKeys(ctx)
	require.NoError(t, err)
	assert.NoError(t, verifyGrant(t, tok, pubsB, clock.Now()), "A's grant verifies on B")
	recheckB, err := kb.recheckKeys(ctx)
	require.NoError(t, err)
	pubA, ok := recheckLookup(recheckB, rot.KeyID)
	require.True(t, ok)
	assert.True(t, bytes.Equal(sa.Key.Public().(ed25519.PublicKey), pubA), "B's re-check key is A's signer's public half")

	// B revokes the outgoing key; A drops it within the re-check freshness.
	require.NoError(t, kb.revoke(ctx, kids.pending))
	assert.True(t, strings.HasPrefix(grantRow(t, st).EncryptedValue, "enc:v2:k2:"))
	clock.Advance(conduitGrantKeyRecheckFresh + time.Second)
	recheckA, err := ka.recheckKeys(ctx)
	require.NoError(t, err)
	_, ok = recheckLookup(recheckA, kids.pending)
	assert.False(t, ok, "A no longer verifies the revoked key")
	ring, _, err := ka.store.Load(ctx)
	require.NoError(t, err)
	revoked := map[string]bool{}
	for _, r := range ring.Revoked {
		revoked[r.KeyID] = true
	}
	assert.True(t, revoked[kids.pending], "the revoked kid is remembered")

	// Retention: the first key's public half (retired at +4m, retained
	// until +14m) verifies re-checks until then, on both replicas.
	retainUntil := rwT0.Add(4*time.Minute + rwRetain)
	clock.Advance(retainUntil.Sub(clock.Now()) - time.Second)
	for name, k := range map[string]*conduitGrantKeys{"A": ka, "B": kb} {
		k.invalidate()
		rk, err := k.recheckKeys(ctx)
		require.NoError(t, err)
		_, ok := recheckLookup(rk, kids.retained)
		assert.True(t, ok, "%s keeps the retained key just before retain_until", name)
	}
	clock.Advance(time.Second)
	for name, k := range map[string]*conduitGrantKeys{"A": ka, "B": kb} {
		k.invalidate()
		rk, err := k.recheckKeys(ctx)
		require.NoError(t, err)
		_, ok := recheckLookup(rk, kids.retained)
		assert.False(t, ok, "%s drops the retained key at retain_until", name)
	}

	// A third replica on a newer writer key rotates: the ring is rewritten
	// under k3. Nodes holding only k2 cannot read it and refuse, without
	// overwriting or recreating it.
	clock.Advance(rwActivate) // the last rotation's key is active, nothing pending
	c := rwReplica(t, st, v2Cipher(t, "k3", "k2", "k3"), clock)
	_, err = c.conduitGrantKeySet().rotate(ctx, rwActivate, rwOverlap)
	require.NoError(t, err)
	k3Row := grantRow(t, st)
	require.True(t, strings.HasPrefix(k3Row.EncryptedValue, "enc:v2:k3:"), "the newer writer stored the ring under k3")

	// Two nodes without k3 must refuse:
	//   - replica A, already running, whose cached ring (last read at +14m)
	//     is past its refresh: it must not serve the stale cache;
	//   - a cold node (grant keys over a k2-only cipher; a new Server's
	//     startup audit would flag the unreadable row before these paths).
	k2 := rwKeys(t, st, v2Cipher(t, "k2", "k2"), clock)
	_, _, err = k2.store.Load(ctx)
	assert.ErrorIs(t, err, errConduitGrantRingUnreadable)
	assert.ErrorIs(t, err, secret.ErrUnknownKeyID)
	// Precondition: A really holds a cached ring, older than the refresh,
	// so its refusals below prove the stale cache is not served.
	ka.mu.Lock()
	cached, age := ka.ring != nil, clock.Now().Sub(ka.loadedAt)
	ka.mu.Unlock()
	require.True(t, cached, "replica A holds a cached ring")
	require.GreaterOrEqual(t, age, conduitGrantKeyRefresh, "replica A's cached ring is past its refresh")
	// For A, signer, publicKeys and freshPublicKeys carry the stale-cache
	// check; recheckKeys (and rotate, revoke) clear or bypass the cache.
	for name, k := range map[string]*conduitGrantKeys{"running replica A": ka, "cold k2-only node": k2} {
		_, err := k.signer(ctx)
		assert.ErrorIs(t, err, errConduitGrantRingUnreadable, "%s: no signer from an unreadable ring (not a cached one)", name)
		_, err = k.publicKeys(ctx)
		assert.ErrorIs(t, err, errConduitGrantRingUnreadable, "%s: nothing published", name)
		_, err = k.freshPublicKeys(ctx)
		assert.Error(t, err, "%s: nothing freshly published", name)
		_, err = k.recheckKeys(ctx)
		assert.ErrorIs(t, err, errConduitGrantRingUnreadable, "%s: no re-check keys", name)
		_, err = k.rotate(ctx, rwActivate, rwOverlap)
		assert.Error(t, err, "%s: no rotation over an unreadable ring", name)
		// kids.active is retained until +19m: revocable on a readable ring.
		err = k.revoke(ctx, kids.active)
		assert.Error(t, err, "%s: no revocation over an unreadable ring", name)
	}
	after := grantRow(t, st)
	assert.True(t, k3Row.EncryptedValue == after.EncryptedValue, "the unreadable ring was overwritten")
	assert.Equal(t, k3Row.Version, after.Version)
	ringK3, _, err := c.conduitGrantKeySet().store.Load(ctx)
	require.NoError(t, err)
	assertNoKeyMaterial(t, err2String(k2.signer(ctx)), ringK3)
}

// err2String is the error text of a (value, error) pair ("" for nil).
func err2String[T any](_ T, err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
