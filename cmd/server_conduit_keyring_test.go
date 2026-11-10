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

// cmdKRStagedSecret is a synthetic staged-generation secret (test only).
const cmdKRStagedSecret = "test-only-cmd-keyring-staged-generation-0123456789"

// cmdKRPeerSecret is a synthetic relay-peer secret that differs from every
// keyring generation (test only). This file builds in every tag set, so it
// declares its own value instead of using one from a !no_sqlite file.
const cmdKRPeerSecret = "test-only-cmd-distinct-relay-peer-secret-0123456789"

// writeCmdKeyringWithStaged is the cmd keyring with a staged gen2 that is
// neither active nor a legacy reader.
func writeCmdKeyringWithStaged(t *testing.T) string {
	t.Helper()
	return writeCmdKeyring(t, func(doc map[string]any) {
		s := doc["signing"].(map[string]any)
		s["keys"] = append(s["keys"].([]any), map[string]any{"id": "gen2", "secret": cmdKRStagedSecret, "sign_until": nil, "verify_until": nil})
	})
}

// TestValidateServerPreflight_PeerSecretAgainstEveryGeneration: in ring
// mode the hub's startup preflight refuses a peer secret equal to the
// secret of any signing generation (the staged one included), not only
// the session secret; a distinct one starts. The external relay mode's
// peer auth applies the same rule. Errors name the setting, never a value.
func TestValidateServerPreflight_PeerSecretAgainstEveryGeneration(t *testing.T) {
	t.Cleanup(resetServerFlags)
	isolateKeyring(t)
	require.NoError(t, loadServerKeyring(&config.GlobalConfig{KeyringFile: writeCmdKeyringWithStaged(t)}))
	ring := serverSigningRingFromFile()
	require.NotNil(t, ring)
	require.Len(t, ring.IDs(), 3)

	external := config.HubConduitConfig{RelayMode: config.ConduitRelayModeExternal}
	for name, secret := range map[string]string{
		"active generation": cmdKRGen1Secret,
		"staged generation": cmdKRStagedSecret,
	} {
		t.Run(name, func(t *testing.T) {
			c := external
			c.PeerSecret = secret
			err := validateServerPreflight(&config.GlobalConfig{Hub: config.HubServerConfig{Conduit: c}})
			require.Error(t, err)
			assert.Contains(t, err.Error(), "every signing generation")
			assertNoCmdKRSecret(t, err.Error())
			assert.NotContains(t, err.Error(), cmdKRStagedSecret)

			cfg := &config.GlobalConfig{}
			cfg.Hub.Conduit = c
			cfg.Hub.Conduit.PeerAuth = config.ConduitPeerAuthHMAC
			_, _, err = hub.NewConduitPeerAuth(externalPeerAuthOptions(context.Background(), cfg, "hub-a", false))
			require.Error(t, err)
			assert.Contains(t, err.Error(), "every signing generation")
			assert.NotContains(t, err.Error(), cmdKRStagedSecret)
		})
	}
	// The "legacy" entry is a generation too (checked here below the
	// strength rule, which this short fixture secret would fail first).
	err := validateConduitPeerSecret(config.HubConduitConfig{PeerSecret: cmdKRLegacySecret}, false, "", ring)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "every signing generation")

	c := external
	c.PeerSecret = cmdKRPeerSecret
	assert.NoError(t, validateServerPreflight(&config.GlobalConfig{Hub: config.HubServerConfig{Conduit: c}}))
	// The relay role passes no ring: only the session-secret rule applies.
	assert.NoError(t, validateConduitPeerSecret(config.HubConduitConfig{PeerSecret: cmdKRStagedSecret}, true, "", nil))
}
