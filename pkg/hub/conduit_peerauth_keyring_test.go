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
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit/relay"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/keyring"
)

// Relay-peer signatures under the signing ring between two hub replicas
// (ptone/scion#1530 Phase 5). Synthetic fixtures only.

// TestConduitPeerAuthKeyring_TwoReplicas: a replica's real internal relay
// API, keyed from its signing ring, accepts a peer replica on another
// active generation, in both signature formats and both peer-auth modes;
// it still refuses a replay, a request for another target, an unknown key
// ID, a generation that is not a legacy reader, and, in OIDC mode, a
// missing or disallowed ID token.
func TestConduitPeerAuthKeyring_TwoReplicas(t *testing.T) {
	for _, mode := range []string{config.ConduitPeerAuthHMAC, config.ConduitPeerAuthOIDC} {
		for _, emit := range []keyring.Emit{keyring.EmitLegacy, keyring.EmitVersioned} {
			t.Run(mode+"/"+string(emit), func(t *testing.T) {
				ka, kb := admRollout(t, emit)
				build := func(self string, k *keyring.Keyring, mod func(*ConduitPeerAuthOptions)) relay.PeerAuth {
					o := ConduitPeerAuthOptions{Mode: mode, SelfID: self, SharedSecret: krLegacySecret, SigningRing: k.Signing()}
					if mode == config.ConduitPeerAuthOIDC {
						o.OwnServiceAccount = "hub@p.iam.gserviceaccount.com"
						o.TokenSource = staticTokenSource{tok: "own"}
						o.Validate = fakeIDTokens(nil)
					}
					if mod != nil {
						mod(&o)
					}
					a, got, err := NewConduitPeerAuth(o)
					require.NoError(t, err)
					require.Equal(t, mode, got)
					return a
				}
				f := newRelayFixture(t, func(o *ConduitRelayOptions) { o.PeerAuth = build("hub-a", ka, nil) })
				internal := httptest.NewServer(f.srv.ConduitInternalHandler())
				t.Cleanup(internal.Close)
				caller := build("hub-b", kb, nil)
				rt := f.srv.conduit.Load()
				require.NotNil(t, rt)
				self := internal.URL + relay.InternalPathPrefix + "self"
				newReq := func(t *testing.T) *http.Request {
					t.Helper()
					req, err := http.NewRequest(http.MethodGet, self, nil)
					require.NoError(t, err)
					relay.SetPeerTarget(req, rt.relay.InstanceID(), rt.relay.Generation())
					return req
				}
				do := func(t *testing.T, req *http.Request) int {
					t.Helper()
					resp, err := internal.Client().Do(req)
					require.NoError(t, err)
					_ = resp.Body.Close()
					return resp.StatusCode
				}

				req := newReq(t)
				require.NoError(t, caller.Sign(req))
				if emit == keyring.EmitVersioned {
					require.Equal(t, "gen2", req.Header.Get(relay.HeaderPeerKeyID))
				} else {
					require.Empty(t, req.Header.Get(relay.HeaderPeerKeyID))
				}
				replay := req.Clone(context.Background())
				assert.Equal(t, http.StatusOK, do(t, req), "B (gen2) signs, A (gen1 active) verifies")
				assert.Equal(t, http.StatusUnauthorized, do(t, replay), "replay refused")

				other := newReq(t)
				relay.SetPeerTarget(other, "hub-c", rt.relay.Generation())
				require.NoError(t, caller.Sign(other))
				assert.Equal(t, http.StatusConflict, do(t, other), "another target")

				if emit == keyring.EmitVersioned {
					unknown := newReq(t)
					require.NoError(t, caller.Sign(unknown))
					unknown.Header.Set(relay.HeaderPeerKeyID, "gen9")
					assert.Equal(t, http.StatusUnauthorized, do(t, unknown), "unknown key ID")
				}

				// A generation the verifier holds but does not read ID-less.
				narrow := admKeyring(t, "gen2", keyring.EmitLegacy, []string{"gen2"},
					admEntry{id: "legacy", secret: krLegacySecret}, admEntry{id: "gen1", secret: krGen1Secret}, admEntry{id: "gen2", secret: krGen2Secret})
				fn := newRelayFixture(t, func(o *ConduitRelayOptions) { o.PeerAuth = build("hub-a", narrow, nil) })
				internalN := httptest.NewServer(fn.srv.ConduitInternalHandler())
				t.Cleanup(internalN.Close)
				rtN := fn.srv.conduit.Load()
				reqN, err := http.NewRequest(http.MethodGet, internalN.URL+relay.InternalPathPrefix+"self", nil)
				require.NoError(t, err)
				relay.SetPeerTarget(reqN, rtN.relay.InstanceID(), rtN.relay.Generation())
				require.NoError(t, build("hub-b", admKeyring(t, "gen1", keyring.EmitLegacy, []string{"gen1"},
					admEntry{id: "legacy", secret: krLegacySecret}, admEntry{id: "gen1", secret: krGen1Secret}), nil).Sign(reqN))
				resp, err := internalN.Client().Do(reqN)
				require.NoError(t, err)
				_ = resp.Body.Close()
				assert.Equal(t, http.StatusUnauthorized, resp.StatusCode, "an ID-less gen1 signature to a verifier reading only gen2")

				if mode == config.ConduitPeerAuthOIDC {
					noToken := newReq(t)
					require.NoError(t, caller.Sign(noToken))
					noToken.Header.Del("Authorization")
					assert.Equal(t, http.StatusUnauthorized, do(t, noToken), "signature without the ID token")

					disallowed := build("hub-b", kb, func(o *ConduitPeerAuthOptions) { o.TokenSource = staticTokenSource{tok: "stranger"} })
					strange := newReq(t)
					require.NoError(t, disallowed.Sign(strange))
					assert.Equal(t, http.StatusUnauthorized, do(t, strange), "an ID token outside the allow-list")
				}
			})
		}
	}
}

// TestConduitPeerAuth_PeerSecretIgnoresRing: with the dedicated relay-peer
// secret set, the signing ring is not used at all. Signatures are those of
// a peer-secret-only authenticator, carry no versioned headers, and a
// ring-keyed verifier refuses them; the peer-secret verifier refuses
// ring-keyed signatures.
func TestConduitPeerAuth_PeerSecretIgnoresRing(t *testing.T) {
	_, kb := admRollout(t, keyring.EmitVersioned)
	withRing := hmacPeerAuthFor(t, ConduitPeerAuthOptions{SelfID: "relay-a", PeerSecret: independencePeerSecret, SharedSecret: krLegacySecret, SigningRing: kb.Signing()})
	peerOnly := hmacPeerAuthFor(t, ConduitPeerAuthOptions{SelfID: "hub", PeerSecret: independencePeerSecret})
	ringOnly := hmacPeerAuthFor(t, ConduitPeerAuthOptions{SelfID: "hub", SharedSecret: krLegacySecret, SigningRing: kb.Signing()})

	req := peerSignedRequest(t, withRing)
	assert.Empty(t, req.Header.Get(relay.HeaderPeerKeyID))
	assert.Empty(t, req.Header.Get(relay.HeaderPeerSignatureVersion))
	_, err := ringOnly.Verify(req.Clone(context.Background()))
	assert.ErrorIs(t, err, relay.ErrPeerUnauthenticated, "the ring does not accept a peer-secret signature")
	_, err = peerOnly.Verify(req)
	require.NoError(t, err, "the peer-secret verifier accepts it")

	_, err = peerOnly.Verify(peerSignedRequest(t, ringOnly))
	assert.ErrorIs(t, err, relay.ErrPeerUnauthenticated, "the peer secret does not accept a ring signature")

	// A refused issuance never falls back to another key: a ring whose
	// active generation is past sign_until signs nothing.
	closing := admKeyring(t, "gen1", keyring.EmitVersioned, []string{"gen1"},
		admEntry{id: "legacy", secret: krLegacySecret}, admEntry{id: "gen1", secret: krGen1Secret, bounded: true})
	a, err := relay.NewHMACPeerAuthFromRing(relay.HMACPeerAuthConfig{SelfID: "hub", Now: func() time.Time { return admSignUntil }}, closing.Signing())
	require.NoError(t, err)
	r := httptest.NewRequest(http.MethodPost, "/internal/v1/hub/admit", nil)
	assert.Error(t, a.Sign(r))
	assert.Empty(t, r.Header.Get(relay.HeaderPeerSignature))
}
