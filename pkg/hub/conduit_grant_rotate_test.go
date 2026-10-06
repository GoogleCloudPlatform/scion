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
	"context"
	"crypto/ed25519"
	"encoding/json"
	"net/http"
	"sort"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit/grant"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const conduitGrantKeyRotatePath = "/api/v1/admin/conduit/grant-keys/rotate"

// rotateConduitGrantKeyAsAdmin rotates through the registered route as the
// dev admin and decodes the response.
func rotateConduitGrantKeyAsAdmin(t *testing.T, srv *Server) ConduitGrantKeyRotation {
	t.Helper()
	rec := doRequest(t, srv, http.MethodPost, conduitGrantKeyRotatePath, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var out ConduitGrantKeyRotation
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	return out
}

// verifyWithKeys plays a target holding keys (for example a set it fetched
// before the hub's latest refresh).
func (f *conduitFixture) verifyWithKeys(t *testing.T, tok []byte, h grant.StreamHeader, pubs []grant.PublicKey) error {
	t.Helper()
	keys, err := grant.NewKeySet(pubs...)
	require.NoError(t, err)
	_, err = grant.Verify(context.Background(), tok, keys, grant.Expectation{
		Target: f.target(), Header: h, ProjectID: f.agent.ProjectID, Issuer: conduitGrantIssuer,
	}, grant.NewMemoryReplayCache(f.clock.Now, 0), f.clock.Now())
	return err
}

// TestAdminConduitGrantKeyRotate covers the route itself: who may call it,
// the experiment gate, the method, and the response shape.
func TestAdminConduitGrantKeyRotate(t *testing.T) {
	srv, _, hubAdmin, member := setupScopedAdminTest(t)
	clock := &conduitTestClock{now: time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)}
	srv.conduitGrants = newConduitGrantKeys(&memoryConduitGrantKeyStore{}, clock.Now)
	setConduitExperiment(t, srv, true)
	ctx := context.Background()
	before, err := srv.conduitGrants.signer(ctx)
	require.NoError(t, err)

	ringLen := func() int {
		ring, _, err := srv.conduitGrants.store.Load(ctx)
		require.NoError(t, err)
		return len(ring.Keys)
	}

	t.Run("refused callers leave the ring unchanged", func(t *testing.T) {
		for _, tc := range []struct {
			name string
			do   func() int
			want int
		}{
			{"unauthenticated", func() int {
				return doRequestNoAuth(t, srv, http.MethodPost, conduitGrantKeyRotatePath, nil).Code
			}, http.StatusUnauthorized},
			{"member", func() int {
				return doRequestAsUser(t, srv, member, http.MethodPost, conduitGrantKeyRotatePath, nil).Code
			}, http.StatusForbidden},
			{"scoped hub-admin", func() int {
				return doRequestAsUser(t, srv, hubAdmin, http.MethodPost, conduitGrantKeyRotatePath, nil).Code
			}, http.StatusForbidden},
		} {
			t.Run(tc.name, func(t *testing.T) {
				assert.Equal(t, tc.want, tc.do())
				assert.Equal(t, 1, ringLen())
			})
		}
	})

	t.Run("method and experiment gate", func(t *testing.T) {
		rec := doRequest(t, srv, http.MethodGet, conduitGrantKeyRotatePath, nil)
		assert.Equal(t, http.StatusMethodNotAllowed, rec.Code)
		setConduitExperiment(t, srv, false)
		rec = doRequest(t, srv, http.MethodPost, conduitGrantKeyRotatePath, nil)
		assert.Equal(t, http.StatusNotFound, rec.Code)
		setConduitExperiment(t, srv, true)
		assert.Equal(t, 1, ringLen())
	})

	t.Run("admin rotates; response has kids and timestamps only", func(t *testing.T) {
		rec := doRequest(t, srv, http.MethodPost, conduitGrantKeyRotatePath, nil)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

		var raw map[string]json.RawMessage
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &raw))
		assert.ElementsMatch(t, []string{"kid", "activate_at", "retiring"}, jsonFieldNames(raw))
		var retiring []map[string]json.RawMessage
		require.NoError(t, json.Unmarshal(raw["retiring"], &retiring))
		require.Len(t, retiring, 1)
		assert.ElementsMatch(t, []string{"kid", "not_after"}, jsonFieldNames(retiring[0]))

		ring, _, err := srv.conduitGrants.store.Load(ctx)
		require.NoError(t, err)
		require.Len(t, ring.Keys, 2)
		assertNoKeyMaterial(t, rec.Body.String(), ring)
		for _, k := range ring.Keys {
			pub := ed25519.NewKeyFromSeed(k.Seed).Public().(ed25519.PublicKey)
			assert.NotContains(t, rec.Body.String(), string(mustJSON(t, []byte(pub))), "no public key bytes either")
		}

		var out ConduitGrantKeyRotation
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
		assert.NotEqual(t, before.KeyID, out.KeyID)
		assert.Equal(t, ring.Keys[1].KeyID, out.KeyID)
		assert.True(t, out.ActivateAt.Equal(clock.Now().Add(srv.conduitGrantKeyActivation())))
		assert.Equal(t, before.KeyID, out.Retiring[0].KeyID)
		assert.True(t, out.Retiring[0].NotAfter.Equal(out.ActivateAt.Add(conduitGrantKeyDefaultOverlap)),
			"the outgoing key retires one overlap after the new key activates")
		assert.True(t, ring.Keys[0].NotAfter.Equal(out.Retiring[0].NotAfter), "not_after is persisted on the outgoing key")
	})
}

// TestAdminConduitGrantKeyRotate_ReadSurfaceUnchanged: rotation does not
// change what GET /api/v1/conduit/grant-keys returns (the same fields, public
// halves only) or who may read it.
func TestAdminConduitGrantKeyRotate_ReadSurfaceUnchanged(t *testing.T) {
	srv, _, _, member := setupScopedAdminTest(t)
	clock := &conduitTestClock{now: time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)}
	srv.conduitGrants = newConduitGrantKeys(&memoryConduitGrantKeyStore{}, clock.Now)
	setConduitExperiment(t, srv, true)

	rec := doRequestAsUser(t, srv, member, http.MethodGet, "/api/v1/conduit/grant-keys", nil)
	require.Equal(t, http.StatusOK, rec.Code, "any signed-in user may read, as before")
	rotateConduitGrantKeyAsAdmin(t, srv)
	rec = doRequestAsUser(t, srv, member, http.MethodGet, "/api/v1/conduit/grant-keys", nil)
	require.Equal(t, http.StatusOK, rec.Code)

	var raw map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &raw))
	assert.Equal(t, []string{"keys"}, jsonFieldNames(raw))
	var keys []map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(raw["keys"], &keys))
	require.Len(t, keys, 2, "both keys are published during the overlap")
	for _, k := range keys {
		assert.Equal(t, []string{"kid", "not_after_unix", "public_key"}, jsonFieldNames(k))
	}
	ring, _, err := srv.conduitGrants.store.Load(context.Background())
	require.NoError(t, err)
	assertNoKeyMaterial(t, rec.Body.String(), ring)

	assert.Equal(t, http.StatusUnauthorized, doRequestNoAuth(t, srv, http.MethodGet, "/api/v1/conduit/grant-keys", nil).Code)
	assert.Equal(t, http.StatusMethodNotAllowed, doRequest(t, srv, http.MethodPost, "/api/v1/conduit/grant-keys", nil).Code)
}

// TestAdminConduitGrantKeyRotate_OldKidUntilExpiry: after a rotation, a grant
// signed with the old kid verifies until its exp and is refused from exp on;
// once the new key activates, new grants carry the new kid and verify.
func TestAdminConduitGrantKeyRotate_OldKidUntilExpiry(t *testing.T) {
	f := newConduitFixture(t)
	h := tcpHeader("3000")

	oldTok, oldClaims, err := f.mint(f.owner, h)
	require.NoError(t, err)
	rot := rotateConduitGrantKeyAsAdmin(t, f.srv)
	require.NotEqual(t, oldClaims.KeyID, rot.KeyID)

	f.clock.Advance(oldClaims.Expiry.Sub(f.clock.Now()) - time.Second)
	assert.NoError(t, f.targetVerify(t, oldTok, h), "old-kid grant accepted before exp")
	f.clock.Advance(time.Second)
	assert.ErrorIs(t, f.targetVerify(t, oldTok, h), grant.ErrExpired, "old-kid grant refused at exp")

	// Before activation the hub still signs with the old kid.
	tok, claims, err := f.mint(f.owner, h)
	require.NoError(t, err)
	assert.Equal(t, oldClaims.KeyID, claims.KeyID)
	assert.NoError(t, f.targetVerify(t, tok, h))

	f.clock.Advance(rot.ActivateAt.Sub(f.clock.Now()))
	tok, claims, err = f.mint(f.owner, h)
	require.NoError(t, err)
	assert.Equal(t, rot.KeyID, claims.KeyID, "new kid signs from activation")
	assert.NoError(t, f.targetVerify(t, tok, h), "new-kid grant accepted")
}

// TestAdminConduitGrantKeyRotate_OldKidUntilNotAfter: an old-kid grant whose
// exp lies past the outgoing key's not_after is accepted just before
// not_after and refused from not_after on, both by a target still holding
// the key set it fetched earlier (not_after check) and by one that has
// refreshed (the retired kid is no longer published). The old key is pruned
// from storage by the next rotation after not_after.
func TestAdminConduitGrantKeyRotate_OldKidUntilNotAfter(t *testing.T) {
	f := newConduitFixture(t)
	ctx := context.Background()
	h := tcpHeader("3000")

	_, template, err := f.mint(f.owner, h)
	require.NoError(t, err)
	ring, _, err := f.srv.conduitGrants.store.Load(ctx)
	require.NoError(t, err)
	require.Len(t, ring.Keys, 1)
	oldKey := ring.Keys[0]

	rot := rotateConduitGrantKeyAsAdmin(t, f.srv)
	require.Len(t, rot.Retiring, 1)
	notAfter := rot.Retiring[0].NotAfter
	require.False(t, notAfter.IsZero(), "rotation sets not_after on the outgoing key")

	// A target refreshed during the overlap holds both keys, old one with
	// its not_after.
	f.clock.Advance(notAfter.Sub(f.clock.Now()) - 30*time.Second)
	staleKeys, err := f.srv.ConduitGrantPublicKeys(ctx)
	require.NoError(t, err)
	require.Len(t, staleKeys, 2)

	// Sign directly with the old seed so exp outlives not_after; the hub's
	// own signer never does this, which isolates the not_after check.
	claims := *template
	claims.JTI, err = grant.NewJTI()
	require.NoError(t, err)
	claims.NotBefore = f.clock.Now()
	claims.Expiry = f.clock.Now().Add(grant.MaxValidity)
	tok, err := grant.Mint(&grant.Signer{KeyID: oldKey.KeyID, Key: ed25519.NewKeyFromSeed(oldKey.Seed)}, claims)
	require.NoError(t, err)

	f.clock.Advance(29 * time.Second)
	assert.NoError(t, f.verifyWithKeys(t, tok, h, staleKeys), "accepted just before not_after")
	assert.NoError(t, f.targetVerify(t, tok, h), "still published just before not_after")

	f.clock.Advance(time.Second)
	require.True(t, f.clock.Now().Equal(notAfter))
	require.True(t, f.clock.Now().Before(claims.Expiry), "exp has not passed")
	assert.ErrorIs(t, f.verifyWithKeys(t, tok, h, staleKeys), grant.ErrKeyExpired, "refused at not_after by a stale key set")
	assert.ErrorIs(t, f.targetVerify(t, tok, h), grant.ErrUnknownKey, "retired kid is no longer published")

	// The next rotation prunes the retired key from storage.
	rotateConduitGrantKeyAsAdmin(t, f.srv)
	ring, _, err = f.srv.conduitGrants.store.Load(ctx)
	require.NoError(t, err)
	for _, k := range ring.Keys {
		assert.NotEqual(t, oldKey.KeyID, k.KeyID, "retired key pruned")
	}
	assert.Len(t, ring.Keys, 2, "current key plus the key just rotated in")
}

func jsonFieldNames[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
