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

package cmd

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit/relay"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/hub"
	"github.com/GoogleCloudPlatform/scion/pkg/store/relaydb"
)

// Relay-peer auth options under a keyring file (ptone/scion#1530 Phase 5).

func conduitPeerReq(t *testing.T, signer relay.PeerAuth) *http.Request {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/internal/v1/conduit/self", nil)
	relay.SetPeerTarget(req, "hub-x", 1)
	require.NoError(t, signer.Sign(req))
	return req
}

// TestConduitPeerAuthOptions_KeyringRing: the in-process hub relay's
// options carry the keyring file's signing ring in ring mode and none in
// legacy mode, where the shared secret keys the signatures as before.
func TestConduitPeerAuthOptions_KeyringRing(t *testing.T) {
	cfg := &config.GlobalConfig{}

	t.Run("legacy mode", func(t *testing.T) {
		isolateKeyring(t)
		webSessionSecret = "test-only-conduit-legacy-secret-0123456789"
		require.NoError(t, loadServerKeyring(&config.GlobalConfig{}))
		o := conduitPeerAuthOptions(context.Background(), cfg, "hub-a", false)
		assert.Nil(t, o.SigningRing)
		assert.Equal(t, "test-only-conduit-legacy-secret-0123456789", o.SharedSecret)
	})

	t.Run("ring mode", func(t *testing.T) {
		isolateKeyring(t)
		path := writeCmdKeyring(t, func(doc map[string]any) {
			doc["signing"].(map[string]any)["legacy_read_ids"] = []any{"legacy", "gen1"}
		})
		require.NoError(t, loadServerKeyring(&config.GlobalConfig{KeyringFile: path}))
		o := conduitPeerAuthOptions(context.Background(), cfg, "hub-a", false)
		require.NotNil(t, o.SigningRing)
		assert.Equal(t, "gen1", o.SigningRing.ActiveID())

		o.Mode = config.ConduitPeerAuthHMAC
		signer, _, err := hub.NewConduitPeerAuth(o)
		require.NoError(t, err)
		// The active gen1 signs: a verifier keyed only from the legacy
		// entry (the pre-ring wiring) refuses, the ring accepts.
		legacyOnly, _, err := hub.NewConduitPeerAuth(hub.ConduitPeerAuthOptions{Mode: config.ConduitPeerAuthHMAC, SelfID: "hub-x", SharedSecret: cmdKRLegacySecret})
		require.NoError(t, err)
		_, err = legacyOnly.Verify(conduitPeerReq(t, signer))
		assert.ErrorIs(t, err, relay.ErrPeerUnauthenticated)
		o.SelfID = "hub-x"
		verifier, _, err := hub.NewConduitPeerAuth(o)
		require.NoError(t, err)
		_, err = verifier.Verify(conduitPeerReq(t, signer))
		assert.NoError(t, err)
	})
}

// TestRelayRoleIgnoresLoadedKeyring: even with a keyring loaded in the
// process, the relay role keys relay-peer auth from peer_secret alone; its
// signatures verify under a peer-secret-only authenticator and not under
// the ring.
func TestRelayRoleIgnoresLoadedKeyring(t *testing.T) {
	isolateKeyring(t)
	path := writeCmdKeyring(t, func(doc map[string]any) {
		doc["signing"].(map[string]any)["legacy_read_ids"] = []any{"legacy", "gen1"}
	})
	require.NoError(t, loadServerKeyring(&config.GlobalConfig{KeyringFile: path}))
	require.NotNil(t, serverSigningRingFromFile())

	rcfg, err := relayServerConfig(context.Background(), relayRoleTestConfig(), &relaydb.DB{Settings: &relaydb.SettingsReader{}})
	require.NoError(t, err)
	peerOnly, _, err := relay.NewConduitPeerAuth(relay.ConduitPeerAuthOptions{Mode: config.ConduitPeerAuthHMAC, SelfID: "hub-x", PeerSecret: relayRoleTestPeerSecret})
	require.NoError(t, err)
	ringOnly, _, err := relay.NewConduitPeerAuth(relay.ConduitPeerAuthOptions{Mode: config.ConduitPeerAuthHMAC, SelfID: "hub-x", SigningRing: serverSigningRingFromFile()})
	require.NoError(t, err)

	req := conduitPeerReq(t, rcfg.PeerAuth)
	assert.Empty(t, req.Header.Get(relay.HeaderPeerKeyID))
	_, err = ringOnly.Verify(req.Clone(context.Background()))
	assert.ErrorIs(t, err, relay.ErrPeerUnauthenticated, "the relay role did not sign with the ring")
	_, err = peerOnly.Verify(req)
	assert.NoError(t, err, "the relay role signs with peer_secret")
}
