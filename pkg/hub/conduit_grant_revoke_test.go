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
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Grant key retention, seed removal and revocation on the hub (design §3.5,
// Target-side deadlines, Re-check).

const conduitGrantKeyRevokePath = "/api/v1/admin/conduit/grant-keys/revoke"

// TestAdminConduitGrantKeyRevoke covers the revoke route: who may call it,
// the experiment gate, the method, the refusals, and the effect on the
// published and re-check key sets.
func TestAdminConduitGrantKeyRevoke(t *testing.T) {
	srv, _, hubAdmin, member := setupScopedAdminTest(t)
	clock := &conduitTestClock{now: time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)}
	srv.conduitGrants = newConduitGrantKeys(&memoryConduitGrantKeyStore{}, clock.Now)
	setConduitExperiment(t, srv, true)
	ctx := context.Background()
	first, err := srv.conduitGrants.signer(ctx)
	require.NoError(t, err)
	body := map[string]string{"kid": first.KeyID}

	t.Run("refused callers", func(t *testing.T) {
		assert.Equal(t, http.StatusUnauthorized, doRequestNoAuth(t, srv, http.MethodPost, conduitGrantKeyRevokePath, body).Code)
		assert.Equal(t, http.StatusForbidden, doRequestAsUser(t, srv, member, http.MethodPost, conduitGrantKeyRevokePath, body).Code)
		assert.Equal(t, http.StatusForbidden, doRequestAsUser(t, srv, hubAdmin, http.MethodPost, conduitGrantKeyRevokePath, body).Code)
	})
	t.Run("method and experiment gate", func(t *testing.T) {
		assert.Equal(t, http.StatusMethodNotAllowed, doRequest(t, srv, http.MethodGet, conduitGrantKeyRevokePath, nil).Code)
		setConduitExperiment(t, srv, false)
		assert.Equal(t, http.StatusNotFound, doRequest(t, srv, http.MethodPost, conduitGrantKeyRevokePath, body).Code)
		setConduitExperiment(t, srv, true)
	})
	t.Run("missing and unknown kid", func(t *testing.T) {
		assert.Equal(t, http.StatusBadRequest, doRequest(t, srv, http.MethodPost, conduitGrantKeyRevokePath, map[string]string{}).Code)
		assert.Equal(t, http.StatusNotFound, doRequest(t, srv, http.MethodPost, conduitGrantKeyRevokePath, map[string]string{"kid": "cg-unknown"}).Code)
	})
	t.Run("the active signing key is refused", func(t *testing.T) {
		rec := doRequest(t, srv, http.MethodPost, conduitGrantKeyRevokePath, body)
		require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
		pubs, err := srv.ConduitGrantPublicKeys(ctx)
		require.NoError(t, err)
		require.Len(t, pubs, 1)
		assert.Equal(t, first.KeyID, pubs[0].KeyID)
	})
	t.Run("a rotated-out key is revoked at once", func(t *testing.T) {
		rot := rotateConduitGrantKeyAsAdmin(t, srv)
		clock.Advance(rot.ActivateAt.Sub(clock.Now()) + time.Second)
		rec := doRequest(t, srv, http.MethodPost, conduitGrantKeyRevokePath, body)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		var raw map[string]json.RawMessage
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &raw))
		assert.ElementsMatch(t, []string{"kid"}, jsonFieldNames(raw))

		pubs, err := srv.ConduitGrantPublicKeys(ctx)
		require.NoError(t, err)
		for _, pk := range pubs {
			assert.NotEqual(t, first.KeyID, pk.KeyID, "revoked key still published")
		}
		recheck, err := srv.conduitGrants.recheckKeys(ctx)
		require.NoError(t, err)
		_, ok := recheck.Lookup(first.KeyID)
		assert.False(t, ok, "revoked key still in the re-check key set")
		_, ok = recheck.Lookup(rot.KeyID)
		assert.True(t, ok, "the current key is in the re-check key set")
	})
}

// TestConduitGrantRingDropsOutgoingSeedOnLoad: once the rotated-in key has
// activated, the next ring load stores the ring without the outgoing key's
// seed; the outgoing key is still published until its not_after and stays
// in the re-check key set, but never signs again.
func TestConduitGrantRingDropsOutgoingSeedOnLoad(t *testing.T) {
	srv, _ := testServer(t)
	clock := &conduitTestClock{now: time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)}
	mem := &memoryConduitGrantKeyStore{}
	srv.conduitGrants = newConduitGrantKeys(mem, clock.Now)
	setConduitExperiment(t, srv, true)
	ctx := context.Background()
	first, err := srv.conduitGrants.signer(ctx)
	require.NoError(t, err)

	rot := rotateConduitGrantKeyAsAdmin(t, srv)
	clock.Advance(rot.ActivateAt.Sub(clock.Now()) + conduitGrantKeyRefresh)
	s, err := srv.conduitGrants.signer(ctx)
	require.NoError(t, err)
	assert.Equal(t, rot.KeyID, s.KeyID)

	ring, _, err := mem.Load(ctx)
	require.NoError(t, err)
	var found bool
	for _, k := range ring.Keys {
		if k.KeyID == first.KeyID {
			found = true
			assert.Empty(t, k.Seed, "the stored ring still holds the outgoing key's seed")
			assert.Len(t, k.PublicKey, 32)
		}
	}
	require.True(t, found, "the outgoing key is still in the ring within the overlap")
	pubs, err := srv.ConduitGrantPublicKeys(ctx)
	require.NoError(t, err)
	assert.Len(t, pubs, 2, "the outgoing key is still published within the overlap")
	recheck, err := srv.conduitGrants.recheckKeys(ctx)
	require.NoError(t, err)
	_, ok := recheck.Lookup(first.KeyID)
	assert.True(t, ok)
}

// TestConduitGrantKeyRetentionFollowsInterval: retention is the user stream
// authorization interval plus 30s, with the 8h default.
func TestConduitGrantKeyRetentionFollowsInterval(t *testing.T) {
	srv, _ := testServer(t)
	assert.Equal(t, defaultConduitUserStreamAuthzMax+30*time.Second, srv.conduitGrantKeyRetention())
	srv.config.ConduitUserStreamAuthzMax = 2 * time.Hour
	assert.Equal(t, 2*time.Hour+30*time.Second, srv.conduitGrantKeyRetention())
}
