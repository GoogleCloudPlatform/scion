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
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit/relay"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	independenceSessionSecret = "hub-session-secret-0123456789abcdefghij"
	independencePeerSecret    = "relay-peer-secret-0123456789abcdefghijk"
)

func hmacPeerAuthFor(t *testing.T, o ConduitPeerAuthOptions) relay.PeerAuth {
	t.Helper()
	o.Mode = config.ConduitPeerAuthHMAC
	a, _, err := NewConduitPeerAuth(o)
	require.NoError(t, err)
	return a
}

func peerSignedRequest(t *testing.T, signer relay.PeerAuth) *http.Request {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/internal/v1/hub/admit", nil)
	relay.SetPeerTarget(req, "hub", 0)
	require.NoError(t, signer.Sign(req))
	return req
}

// TestPeerSecretIndependentOfSessionTokens (design §3.9): a holder of
// peer_secret can neither mint nor verify a session token, and the session
// secret can neither produce nor verify a relay-peer signature made with
// the peer secret.
func TestPeerSecretIndependentOfSessionTokens(t *testing.T) {
	newTokens := func(secret string) *UserTokenService {
		t.Helper()
		s, err := NewUserTokenService(UserTokenConfig{
			SigningKey:          deriveSharedSigningKey(secret, SecretKeyUserSigningKey),
			AccessTokenDuration: time.Hour, CLIAccessTokenDuration: time.Hour, RefreshTokenDuration: time.Hour,
		})
		require.NoError(t, err)
		return s
	}
	hubTokens := newTokens(independenceSessionSecret)
	token, _, _, err := hubTokens.GenerateTokenPair("u1", "u1@example.com", "U1", "member", ClientTypeWeb)
	require.NoError(t, err)
	_, err = hubTokens.ValidateUserToken(token)
	require.NoError(t, err, "the hub verifies its own token")

	// A peer-secret holder, using the peer secret in place of the session
	// secret, or as a raw signing key.
	peerTokens := newTokens(independencePeerSecret)
	_, err = peerTokens.ValidateUserToken(token)
	assert.Error(t, err, "peer secret verified a session token")
	rawPeer, err := NewUserTokenService(UserTokenConfig{SigningKey: []byte(independencePeerSecret), AccessTokenDuration: time.Hour, CLIAccessTokenDuration: time.Hour, RefreshTokenDuration: time.Hour})
	require.NoError(t, err)
	_, err = rawPeer.ValidateUserToken(token)
	assert.Error(t, err, "peer secret as a raw key verified a session token")
	forged, _, _, err := peerTokens.GenerateTokenPair("u1", "u1@example.com", "U1", "admin", ClientTypeWeb)
	require.NoError(t, err)
	_, err = hubTokens.ValidateUserToken(forged)
	assert.Error(t, err, "a token minted from the peer secret verified at the hub")

	// Relay-peer signatures: the peer-secret verifier refuses one made with
	// the session secret, and the reverse.
	peerVerifier := hmacPeerAuthFor(t, ConduitPeerAuthOptions{SelfID: "hub", PeerSecret: independencePeerSecret})
	sessionSigner := hmacPeerAuthFor(t, ConduitPeerAuthOptions{SelfID: "relay-a", SharedSecret: independenceSessionSecret})
	_, err = peerVerifier.Verify(peerSignedRequest(t, sessionSigner))
	assert.True(t, errors.Is(err, relay.ErrPeerUnauthenticated), "session-secret signature accepted: %v", err)
	sessionVerifier := hmacPeerAuthFor(t, ConduitPeerAuthOptions{SelfID: "hub", SharedSecret: independenceSessionSecret})
	peerSigner := hmacPeerAuthFor(t, ConduitPeerAuthOptions{SelfID: "relay-a", PeerSecret: independencePeerSecret})
	_, err = sessionVerifier.Verify(peerSignedRequest(t, peerSigner))
	assert.True(t, errors.Is(err, relay.ErrPeerUnauthenticated), "peer-secret signature accepted by the session-secret verifier: %v", err)
}

// TestConduitPeerAuthUsesNormalizedPeerSecret: the HMAC key is derived from
// the normalized peer secret, so a secret file's single trailing newline
// does not change it, and a peer secret equal to the session secret once
// normalized is refused.
func TestConduitPeerAuthUsesNormalizedPeerSecret(t *testing.T) {
	verifier := hmacPeerAuthFor(t, ConduitPeerAuthOptions{SelfID: "hub", PeerSecret: independencePeerSecret})
	withNewline := hmacPeerAuthFor(t, ConduitPeerAuthOptions{SelfID: "relay-a", PeerSecret: independencePeerSecret + "\n"})
	_, err := verifier.Verify(peerSignedRequest(t, withNewline))
	assert.NoError(t, err)

	_, _, err = NewConduitPeerAuth(ConduitPeerAuthOptions{Mode: config.ConduitPeerAuthHMAC, SelfID: "hub",
		PeerSecret: independenceSessionSecret + "\n", SharedSecret: independenceSessionSecret})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "server.hub.conduit.peer_secret must differ from the session secret")
	assert.NotContains(t, err.Error(), independenceSessionSecret)
}

// TestConduitPeerAuthAppliesPeerSecretRules: hub peer auth applies the same
// peer_secret rules as the startup check (one rule, one minimum).
func TestConduitPeerAuthAppliesPeerSecretRules(t *testing.T) {
	for name, secret := range map[string]string{
		"31 octets":           independencePeerSecret[:31],
		"few distinct octets": "abababababababababababababababababab",
	} {
		t.Run(name, func(t *testing.T) {
			require.Error(t, config.ValidatePeerSecret(secret), "the startup check refuses it")
			_, _, err := NewConduitPeerAuth(ConduitPeerAuthOptions{Mode: config.ConduitPeerAuthHMAC, SelfID: "hub", PeerSecret: secret})
			require.Error(t, err, "peer auth refuses what the startup check refuses")
			assert.Contains(t, err.Error(), "server.hub.conduit.peer_secret")
		})
	}
	require.NoError(t, config.ValidatePeerSecret(independencePeerSecret[:32]))
	_, _, err := NewConduitPeerAuth(ConduitPeerAuthOptions{Mode: config.ConduitPeerAuthHMAC, SelfID: "hub", PeerSecret: independencePeerSecret[:32]})
	assert.NoError(t, err, "and accepts what it accepts")
}
