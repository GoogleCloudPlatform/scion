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

package relay

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/keyring"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Synthetic fixtures only; none of these is or was a real credential.
const (
	psLegacySecret = "ps-test-only-legacy-generation-secret-0123456789"
	psPrevSecret   = "ps-test-only-previous-generation-secret-012345678"
	psActiveSecret = "ps-test-only-active-generation-secret-0123456789"
	psStagedSecret = "ps-test-only-staged-generation-secret-0123456789"
	psPeerSecret   = "ps-test-only-dedicated-relay-peer-secret-01234567"
)

type psEntry struct {
	id, secret string
	signUntil  *time.Time
	verify     *time.Time
}

// psRing parses a keyring file with the given signing entries (file
// order), active generation and legacy readers, and returns its signing
// ring.
func psRing(t *testing.T, active string, readers []string, entries ...psEntry) *keyring.SigningRing {
	t.Helper()
	keys := make([]any, 0, len(entries))
	for _, e := range entries {
		m := map[string]any{"id": e.id, "secret": e.secret, "sign_until": nil, "verify_until": nil}
		if e.signUntil != nil {
			m["sign_until"] = e.signUntil.UTC().Format(time.RFC3339)
		}
		if e.verify != nil {
			m["verify_until"] = e.verify.UTC().Format(time.RFC3339)
		}
		keys = append(keys, m)
	}
	doc := map[string]any{
		"schema":   1,
		"revision": "peer-secret-ring-test",
		"signing": map[string]any{
			"active":          active,
			"emit":            "versioned",
			"keys":            keys,
			"legacy_read_ids": readers,
		},
		"encryption": map[string]any{
			"active":             "e1",
			"write_format":       "v1",
			"keys":               []any{map[string]any{"id": "e1", "key_b64": base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32))}},
			"legacy_v1_read_ids": []any{"e1"},
		},
	}
	raw, err := json.Marshal(doc)
	require.NoError(t, err)
	k, err := keyring.Parse(raw)
	require.NoError(t, err)
	return k.Signing()
}

func psTime(d time.Duration) *time.Time {
	at := time.Now().Add(d)
	return &at
}

// psFullRing has a retired previous generation (issuance and verification
// over), the active one and a staged successor that is neither active nor
// a legacy reader. withLegacy makes the previous generation the imported
// "legacy" entry.
func psFullRing(t *testing.T, withLegacy bool) *keyring.SigningRing {
	prev := psEntry{id: "gen1", secret: psPrevSecret, signUntil: psTime(-48 * time.Hour), verify: psTime(-time.Hour)}
	readers := []string{"gen2"}
	if withLegacy {
		prev = psEntry{id: "legacy", secret: psLegacySecret, signUntil: psTime(-48 * time.Hour), verify: psTime(-time.Hour)}
		readers = []string{"legacy", "gen2"}
	}
	return psRing(t, "gen2", readers, prev,
		psEntry{id: "gen2", secret: psActiveSecret},
		psEntry{id: "gen3", secret: psStagedSecret},
	)
}

func psRequireRefused(t *testing.T, err error, want error, values ...string) {
	t.Helper()
	require.Error(t, err)
	assert.ErrorIs(t, err, want)
	for _, v := range values {
		assert.NotContains(t, err.Error(), v)
	}
	for _, id := range []string{"gen1", "gen2", "gen3"} {
		assert.NotContains(t, err.Error(), id, "the error names no key ID")
	}
}

// A peer secret equal to the secret of any generation is refused, whatever
// the generation's role or deadlines: the previous (retired) one, whether
// or not it is the "legacy" entry, the active one and the staged one.
func TestCheckPeerSecretNotInRing_RefusesEveryGeneration(t *testing.T) {
	all := []string{psLegacySecret, psPrevSecret, psActiveSecret, psStagedSecret}
	for _, withLegacy := range []bool{true, false} {
		ring := psFullRing(t, withLegacy)
		_, hasLegacy := ring.LegacySecret()
		require.Equal(t, withLegacy, hasLegacy)
		prev := psPrevSecret
		if withLegacy {
			prev = psLegacySecret
		}
		for name, secret := range map[string]string{
			"previous generation (retired)":   prev,
			"active generation":               psActiveSecret,
			"staged generation":               psStagedSecret,
			"staged, with a trailing newline": psStagedSecret + "\n",
		} {
			t.Run(fmt.Sprintf("%s/legacy entry %v", name, withLegacy), func(t *testing.T) {
				psRequireRefused(t, CheckPeerSecretNotInRing(secret, ring), errPeerSecretInRing, all...)
			})
		}
		// A secret that differs from every generation is accepted.
		assert.NoError(t, CheckPeerSecretNotInRing(psPeerSecret, ring))
		// A prefix of a generation's secret differs from it.
		assert.NoError(t, CheckPeerSecretNotInRing(prev[:len(prev)-1], ring))
	}
}

// A ring whose staged slot is unset (a single generation) is a legitimate
// ring: a distinct peer secret is accepted, and an equal one is refused.
// Only an empty secret in a populated generation refuses as such.
func TestCheckPeerSecretNotInRing_UnsetStagedSlotIsNotRefused(t *testing.T) {
	ring := psRing(t, "gen1", []string{"gen1"}, psEntry{id: "gen1", secret: psActiveSecret})
	require.Len(t, ring.IDs(), 1)
	assert.NoError(t, CheckPeerSecretNotInRing(psPeerSecret, ring))
	psRequireRefused(t, CheckPeerSecretNotInRing(psActiveSecret, ring), errPeerSecretInRing)
}

// Fail closed: a non-nil ring with no usable generation (zero, or a
// dangling active ID) refuses rather than comparing against nothing; so
// does a populated generation with an empty secret (the loader refuses
// one; this guards the comparison itself). A nil ring (also what a zero
// Keyring returns) is legacy mode or the relay role: nothing to compare.
func TestCheckPeerSecretNotInRing_FailsClosed(t *testing.T) {
	psRequireRefused(t, CheckPeerSecretNotInRing(psPeerSecret, &keyring.SigningRing{}), errPeerSecretRingNoUsable)
	psRequireRefused(t, checkPeerSecretAgainst(psPeerSecret, nil), errPeerSecretRingNoUsable)

	psRequireRefused(t, checkPeerSecretAgainst(psPeerSecret, [][]byte{[]byte(psActiveSecret), {}}), errPeerSecretRingEmptyEntry)
	psRequireRefused(t, checkPeerSecretAgainst(psPeerSecret, [][]byte{{}, []byte(psActiveSecret)}), errPeerSecretRingEmptyEntry)
	// An empty generation never hides a match elsewhere behind a pass.
	err := checkPeerSecretAgainst(psActiveSecret, [][]byte{{}, []byte(psActiveSecret)})
	require.Error(t, err)

	assert.NoError(t, CheckPeerSecretNotInRing(psPeerSecret, nil))
	assert.NoError(t, CheckPeerSecretNotInRing(psActiveSecret, nil))
}

// Planted defects: a check that compares only the active generation, or
// only the "legacy" entry (the behaviour before every generation was
// compared), misses the staged or previous generation; this test detects
// both.
func TestCheckPeerSecretNotInRing_DetectsActiveOrLegacyOnlyCompare(t *testing.T) {
	ring := psFullRing(t, true)
	activeOnly := func(peer string, r *keyring.SigningRing) error {
		k, err := r.Active(peerSecretRingCheckPurpose, time.Now())
		if err != nil {
			return err
		}
		return checkPeerSecretAgainst(peer, [][]byte{k.Bytes()})
	}
	legacyOnly := func(peer string, r *keyring.SigningRing) error {
		l, ok := r.LegacySecret()
		if !ok {
			return nil
		}
		return checkPeerSecretAgainst(peer, [][]byte{[]byte(l)})
	}
	for name, check := range map[string]func(string, *keyring.SigningRing) error{
		"active only": activeOnly,
		"legacy only": legacyOnly,
	} {
		assert.NoError(t, check(psStagedSecret, ring), "%s: the planted defect accepts the staged generation", name)
	}
	assert.Error(t, CheckPeerSecretNotInRing(psStagedSecret, ring), "the real check refuses it")
	// Without the legacy entry the legacy-only check compares nothing.
	noLegacy := psFullRing(t, false)
	assert.NoError(t, legacyOnly(psPrevSecret, noLegacy), "legacy only: the planted defect accepts the previous generation")
	assert.Error(t, CheckPeerSecretNotInRing(psPrevSecret, noLegacy), "the real check refuses it")
}

// NewConduitPeerAuth applies the check when both a peer secret and the
// signing ring are set (a hub in external relay mode), and keeps the relay
// role (no ring) unchanged.
func TestNewConduitPeerAuth_PeerSecretAgainstEveryGeneration(t *testing.T) {
	ring := psFullRing(t, true)
	for _, secret := range []string{psLegacySecret, psActiveSecret, psStagedSecret} {
		_, _, err := NewConduitPeerAuth(ConduitPeerAuthOptions{Mode: config.ConduitPeerAuthHMAC, SelfID: "hub-1", PeerSecret: secret, SigningRing: ring})
		require.Error(t, err)
		var se *SettingError
		require.True(t, errors.As(err, &se))
		assert.Equal(t, peerSecretSetting, se.Key)
		assert.ErrorIs(t, err, errPeerSecretInRing)
		assert.NotContains(t, err.Error(), secret)
	}
	a, mode, err := NewConduitPeerAuth(ConduitPeerAuthOptions{Mode: config.ConduitPeerAuthHMAC, SelfID: "hub-1", PeerSecret: psPeerSecret, SigningRing: ring})
	require.NoError(t, err)
	assert.Equal(t, config.ConduitPeerAuthHMAC, mode)
	assert.NotNil(t, a)

	// The relay role carries no ring: only the peer secret's own rules.
	_, _, err = NewConduitPeerAuth(ConduitPeerAuthOptions{Mode: config.ConduitPeerAuthHMAC, SelfID: "relay-1", PeerSecret: psActiveSecret})
	assert.NoError(t, err)
	// A non-nil ring with no usable generation refuses with a peer secret.
	_, _, err = NewConduitPeerAuth(ConduitPeerAuthOptions{Mode: config.ConduitPeerAuthHMAC, SelfID: "hub-1", PeerSecret: psPeerSecret, SigningRing: &keyring.SigningRing{}})
	require.Error(t, err)
	assert.ErrorIs(t, err, errPeerSecretRingNoUsable)
}
