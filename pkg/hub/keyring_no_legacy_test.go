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
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit/relay"
	"github.com/GoogleCloudPlatform/scion/pkg/keyring"
)

// Keyring mode with no "legacy" signing entry (the old generation retired),
// with the release check relaxed as lifting the release limits will.

// rolloutWithoutLegacy makes the shared two-replica rollout fixtures
// (capRollout, admRollout, krRollout) build their rings with no "legacy"
// signing entry and no "legacy" reader. Set only through
// withRolloutWithoutLegacy; tests using it do not run in parallel.
var rolloutWithoutLegacy bool

// withoutLegacyID returns ids without keyring.LegacyID.
func withoutLegacyID(ids []string) []string {
	return slices.DeleteFunc(slices.Clone(ids), func(id string) bool { return id == keyring.LegacyID })
}

// withRolloutWithoutLegacy relaxes the release check and switches the
// rollout fixtures to rings without the "legacy" entry for the test.
func withRolloutWithoutLegacy(t *testing.T) {
	t.Helper()
	relaxKeyringReleaseCheck(t)
	rolloutWithoutLegacy = true
	t.Cleanup(func() { rolloutWithoutLegacy = false })
}

// TestNoLegacyRing_TwoInstancesServeEveryPurpose is the two-instance smoke
// over the real store: with rings that have no "legacy" entry, every
// purpose a replica issues is accepted by the other (and back), through the
// same cross-instance tests that cover the rollout with the entry: web
// session cookies, skill file URLs, artifact views, list cursors,
// attachment delivery, conduit admission tokens and relay-peer signatures.
// Agent and user JWTs: TestNoLegacyRing_JWTAcrossInstancesAndRestart.
func TestNoLegacyRing_TwoInstancesServeEveryPurpose(t *testing.T) {
	withRolloutWithoutLegacy(t)
	// Control: the fixtures really build rings without the entry.
	ca, cb := capRollout(t, "legacy")
	aa, ab := admRollout(t, keyring.EmitVersioned)
	ka, kb := krRollout(t)
	for _, k := range []*keyring.Keyring{ca, cb, aa, ab, ka, kb} {
		_, ok := k.Signing().LegacySecret()
		require.False(t, ok, "a rollout ring still has the legacy entry")
		require.NotContains(t, k.Signing().IDs(), keyring.LegacyID)
	}
	for _, tc := range []struct {
		name string
		run  func(*testing.T)
	}{
		{"web session cookies: callback state", TestWebKeyring_CallbackStateSurvivesCrossReplicaHop},
		{"web session cookies: login", TestWebKeyring_OldCookieLoginSurvivesSecondReplica},
		{"skill file URLs", TestCapabilityKeyring_SkillURLCrossInstance},
		{"artifact views", TestCapabilityKeyring_ArtifactViewCrossInstance},
		{"list cursors", TestCapabilityKeyring_ListCursorCrossInstance},
		{"attachment delivery", TestCapabilityKeyring_DeliveryCrossReplica},
		{"conduit admission tokens", TestAdmissionKeyring_CrossReplicaMixedActive},
		{"relay-peer signatures", TestConduitPeerAuthKeyring_TwoReplicas},
	} {
		t.Run(tc.name, tc.run)
	}
}

// noLegacyJWTRings is a rollout without the "legacy" entry over the P3
// fixture's clock: A issues with gen1; B has activated gen2 and keeps gen1
// as the outgoing generation. Both list both generations as legacy readers
// (emit "legacy").
func (f *p3Fixture) noLegacyJWTRings(t *testing.T) (a, b *keyring.Keyring) {
	t.Helper()
	readers := []string{"gen1", "gen2"}
	a = jkRing(t, "legacy", "gen1", readers, jkEntry{id: "gen1", secret: jkGen1Secret}, jkEntry{id: "gen2", secret: jkGen2Secret})
	b = jkRing(t, "legacy", "gen2", readers,
		jkEntry{id: "gen1", secret: jkGen1Secret, signUntil: f.at(time.Hour), verifyUntl: f.at(3 * time.Hour)},
		jkEntry{id: "gen2", secret: jkGen2Secret})
	return a, b
}

// TestNoLegacyRing_JWTAcrossInstancesAndRestart: with no "legacy" entry,
// agent JWTs issued by either replica authenticate on the other, from the
// keyring source (never a shared-secret or empty default); and a restart
// of A on the same store (start, then restart) keeps the same keys: tokens
// issued before the restart authenticate after it, and the restarted
// replica's tokens authenticate on B.
func TestNoLegacyRing_JWTAcrossInstancesAndRestart(t *testing.T) {
	relaxKeyringReleaseCheck(t)
	f := newP3Fixture(t)
	ka, kb := f.noLegacyJWTRings(t)
	srvA, err := f.start(t, f.config(ka, true))
	require.NoError(t, err)
	srvB, err := f.start(t, f.config(kb, true))
	require.NoError(t, err)
	for _, srv := range []*Server{srvA, srvB} {
		assert.Equal(t, SigningKeySourceKeyring, srv.signingKeySource(SecretKeyAgentSigningKey))
		assert.Equal(t, SigningKeySourceKeyring, srv.signingKeySource(SecretKeyUserSigningKey))
		assert.Empty(t, srv.config.SharedSigningSecret)
	}
	_, _, user, project := setupCredentialTestServerOn(t, srvA, f.raw)
	agentID := tid("agent-no-legacy")
	createCredTestAgent(t, f.raw, agentID, project.ID, user.ID)

	tokA, err := srvA.GenerateAgentToken(agentID, project.ID, nil, AgentRoleFull, nil)
	require.NoError(t, err)
	tokB, err := srvB.GenerateAgentToken(agentID, project.ID, nil, AgentRoleFull, nil)
	require.NoError(t, err)
	assert.Equal(t, []string{"gen1"}, verifiesOnlyUnder(t, tokA, SecretKeyAgentSigningKey))
	assert.Equal(t, []string{"gen2"}, verifiesOnlyUnder(t, tokB, SecretKeyAgentSigningKey))
	authOK := func(t *testing.T, srv *Server, tok string) {
		t.Helper()
		rec := p3AgentAuth(t, srv, map[string]string{"X-Scion-Agent-Token": tok})
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		assert.Equal(t, agentID, rec.Body.String())
	}
	authOK(t, srvB, tokA)
	authOK(t, srvA, tokB)

	// Restart A on the same store.
	srvA2, err := f.start(t, f.config(ka, true))
	require.NoError(t, err)
	assert.Equal(t, SigningKeySourceKeyring, srvA2.signingKeySource(SecretKeyAgentSigningKey))
	authOK(t, srvA2, tokA)
	authOK(t, srvA2, tokB)
	tokA2, err := srvA2.GenerateAgentToken(agentID, project.ID, nil, AgentRoleFull, nil)
	require.NoError(t, err)
	assert.Equal(t, []string{"gen1"}, verifiesOnlyUnder(t, tokA2, SecretKeyAgentSigningKey))
	authOK(t, srvB, tokA2)
}

// TestKeyringLegacyGeneration_ByteIdenticalPerPurpose pins that, for a ring
// WITH the "legacy" entry, every signing purpose derives from that
// generation exactly the key legacy mode derives from the same secret (the
// legacy adapter, which a legacy-mode replica uses), and, where the
// legacy-mode derivation is reachable here, the bytes of that derivation
// itself. SharedSigningSecret is an input to none of them, so emptying it
// in keyring mode changes no key: values minted before and after the
// change, by a keyring replica or a legacy-mode one, verify on both (the
// cross-replica LegacyReplicaUpgrade tests exercise that end to end).
func TestKeyringLegacyGeneration_ByteIdenticalPerPurpose(t *testing.T) {
	withLegacy, _ := capRollout(t, "legacy")
	legacyMode := keyring.Legacy(krLegacySecret)
	legacyGen := func(t *testing.T, p keyring.Purpose) []byte {
		t.Helper()
		for _, k := range withLegacy.Signing().Generations(p) {
			if k.ID == keyring.LegacyID {
				return k.Bytes()
			}
		}
		t.Fatalf("purpose %s: no legacy generation", p.Name())
		return nil
	}
	legacyModeKey := func(t *testing.T, p keyring.Purpose) []byte {
		t.Helper()
		k, err := legacyMode.Signing().Active(p, time.Time{})
		require.NoError(t, err)
		return k.Bytes()
	}
	for _, tc := range []struct {
		name    string
		purpose keyring.Purpose
		direct  []byte // the legacy-mode derivation, where reachable; nil otherwise
	}{
		{"agent JWT", agentJWTPurpose, deriveSharedSigningKey(krLegacySecret, SecretKeyAgentSigningKey)},
		{"user JWT", userJWTPurpose, deriveSharedSigningKey(krLegacySecret, SecretKeyUserSigningKey)},
		{"web session hash", webSessionHashPurpose, deriveSessionKey(krLegacySecret, "scion-session-hash")},
		{"web session block", webSessionBlockPurpose, nil},
		{"skill file URLs", sharedSecretPurpose(SecretKeyDownloadSigningKey), deriveSharedSigningKey(krLegacySecret, SecretKeyDownloadSigningKey)},
		{"artifact views", sharedSecretPurpose(SecretKeyArtifactViewKey), deriveSharedSigningKey(krLegacySecret, SecretKeyArtifactViewKey)},
		{"list cursors", sharedSecretPurpose(SecretKeyListCursorKey), deriveSharedSigningKey(krLegacySecret, SecretKeyListCursorKey)},
		{"attachment delivery", sharedSecretPurpose(SecretKeyAttachmentDeliveryKey), deriveSharedSigningKey(krLegacySecret, SecretKeyAttachmentDeliveryKey)},
		{"port views", sharedSecretPurpose(SecretKeyPortViewKey), deriveSharedSigningKey(krLegacySecret, SecretKeyPortViewKey)},
		{"conduit admission tokens", conduitAdmissionPurpose, nil},
		{"relay-peer signatures", relay.PeerHMACPurpose, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := legacyGen(t, tc.purpose)
			require.Len(t, got, 32, "a derived key of the expected length")
			assert.Equal(t, legacyModeKey(t, tc.purpose), got, "the legacy generation derives the legacy-mode key")
			if tc.direct != nil {
				assert.Equal(t, tc.direct, got, "the legacy-mode derivation, byte for byte")
			}
		})
	}
}
