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
	"encoding/base64"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/keyring"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// SealPosition and OpenPosition: opaque positions that do not fit
// authorizedList's "created,id" cursor (the Starred rail's sort value and
// conversation key), sealed with the list-cursor key under their own AAD
// domain and prefix.

const positionSealTestPosition = `{"value":"mia","key":"dm:user:a:user:b"}`

func TestListCursorSealer_PositionRoundTrip(t *testing.T) {
	sealer := mustNewListCursorSealer(t)
	sealed, err := sealer.SealPosition(positionSealTestPosition, "binding-x")
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(sealed, listPositionPrefix))
	assert.NotContains(t, sealed, "mia", "the sealed form carries no plaintext")

	opened, err := sealer.OpenPosition(sealed, "binding-x")
	require.NoError(t, err)
	assert.Equal(t, positionSealTestPosition, opened)
}

// A position cursor never opens as a list cursor, and a list cursor never
// opens as a position cursor, under the same key and binding.
func TestListCursorSealer_PositionAndListCursorsDoNotCross(t *testing.T) {
	sealer := mustNewListCursorSealer(t)
	position, err := sealer.SealPosition(positionSealTestPosition, "binding-x")
	require.NoError(t, err)
	_, err = sealer.Open(position, "binding-x")
	assert.ErrorIs(t, err, errInvalidCursor, "Open refuses a position cursor")
	_, err = sealer.Open(listCursorPrefix+strings.TrimPrefix(position, listPositionPrefix), "binding-x")
	assert.ErrorIs(t, err, errInvalidCursor, "a position body under the list prefix fails the list AAD")

	inner := authorizedListCursor(time.Now(), "11111111-1111-1111-1111-111111111111", "binding-x")
	list, err := sealer.Seal(inner, "binding-x")
	require.NoError(t, err)
	_, err = sealer.OpenPosition(list, "binding-x")
	assert.ErrorIs(t, err, errInvalidCursor, "OpenPosition refuses a list cursor")
	_, err = sealer.OpenPosition(listPositionPrefix+strings.TrimPrefix(list, listCursorPrefix), "binding-x")
	assert.ErrorIs(t, err, errInvalidCursor, "a list body under the position prefix fails the position AAD")
}

func TestListCursorSealer_PositionRejections(t *testing.T) {
	sealer := mustNewListCursorSealer(t)
	sealed, err := sealer.SealPosition(positionSealTestPosition, "binding-x")
	require.NoError(t, err)
	body := strings.TrimPrefix(sealed, listPositionPrefix)
	raw, err := base64.RawURLEncoding.DecodeString(body)
	require.NoError(t, err)
	flipped := append([]byte(nil), raw...)
	flipped[len(flipped)-1] ^= 0x01

	for name, c := range map[string]struct{ cursor, binding string }{
		"wrong binding":   {sealed, "binding-y"},
		"tampered byte":   {listPositionPrefix + base64.RawURLEncoding.EncodeToString(flipped), "binding-x"},
		"truncated":       {sealed[:len(sealed)-4], "binding-x"},
		"nonce only":      {listPositionPrefix + base64.RawURLEncoding.EncodeToString(raw[:4]), "binding-x"},
		"no prefix":       {body, "binding-x"},
		"not base64":      {listPositionPrefix + "!!!", "binding-x"},
		"empty":           {"", "binding-x"},
		"plaintext":       {positionSealTestPosition, "binding-x"},
		"unknown version": {"p3." + body, "binding-x"},
		"v2 without kid":  {listPositionPrefixV2 + body, "binding-x"},
	} {
		_, err := sealer.OpenPosition(c.cursor, c.binding)
		assert.ErrorIs(t, err, errInvalidCursor, name)
	}

	other := mustNewListCursorSealer(t)
	_, err = other.OpenPosition(sealed, "binding-x")
	assert.ErrorIs(t, err, errInvalidCursor, "another key")
}

func TestListCursorSealer_PositionNilReceiverFailsClosed(t *testing.T) {
	var sealer *listCursorSealer
	_, err := sealer.SealPosition(positionSealTestPosition, "binding-x")
	assert.ErrorIs(t, err, errListCursorSealerUnavailable)
	_, err = sealer.OpenPosition(listPositionPrefix+"AAAA", "binding-x")
	assert.ErrorIs(t, err, errInvalidCursor)
}

// positionRingSealer is a ring-mode sealer over keyring k at clock clk.
func positionRingSealer(k *keyring.Keyring, clk *krClock) *listCursorSealer {
	return newRingListCursorSealer(capabilityKeys{ring: k.Signing(), purpose: sharedSecretPurpose(SecretKeyListCursorKey), now: clk.now})
}

// In ring mode with emit "versioned" a position cursor carries its
// generation (p2.<kid>.), opens on a replica that still verifies that
// generation until its verify_until and not from that instant, and never
// opens with another or an unknown generation.
func TestListCursorSealer_PositionRingRotationAndRetirement(t *testing.T) {
	clk := &krClock{t: krT0}
	ka, kb := capRollout(t, "versioned")
	a, b := positionRingSealer(ka, clk), positionRingSealer(kb, clk)

	sealed, err := a.SealPosition(positionSealTestPosition, "binding-x")
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(sealed, listPositionPrefixV2+"gen1."), sealed)
	for name, s := range map[string]*listCursorSealer{"issuing replica": a, "replica with gen1 outgoing": b} {
		got, err := s.OpenPosition(sealed, "binding-x")
		require.NoError(t, err, name)
		assert.Equal(t, positionSealTestPosition, got, name)
	}
	fromB, err := b.SealPosition(positionSealTestPosition, "binding-x")
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(fromB, listPositionPrefixV2+"gen2."), fromB)

	body := strings.TrimPrefix(sealed, listPositionPrefixV2+"gen1.")
	for name, c := range map[string]string{
		"unknown kid":       listPositionPrefixV2 + "gen9." + body,
		"swapped kid":       listPositionPrefixV2 + "gen2." + body,
		"stripped to p1":    listPositionPrefix + body,
		"no kid":            listPositionPrefixV2 + body,
		"empty kid":         listPositionPrefixV2 + "." + body,
		"bad kid character": listPositionPrefixV2 + "gen%211." + body,
		"as a list cursor":  listCursorPrefixV2 + "gen1." + body,
	} {
		_, err := b.OpenPosition(c, "binding-x")
		assert.ErrorIs(t, err, errInvalidCursor, name)
	}
	_, err = b.Open(sealed, "binding-x")
	assert.ErrorIs(t, err, errInvalidCursor, "Open refuses a p2 cursor")
	_, err = b.Open(listCursorPrefixV2+"gen1."+body, "binding-x")
	assert.ErrorIs(t, err, errInvalidCursor, "a p2 body under c2 fails the list AAD")
	list, err := a.Seal(authorizedListCursor(time.Now(), "11111111-1111-1111-1111-111111111111", "binding-x"), "binding-x")
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(list, listCursorPrefixV2+"gen1."), list)
	_, err = b.OpenPosition(list, "binding-x")
	assert.ErrorIs(t, err, errInvalidCursor, "OpenPosition refuses a c2 cursor")
	_, err = b.OpenPosition(listPositionPrefixV2+strings.TrimPrefix(list, listCursorPrefixV2), "binding-x")
	assert.ErrorIs(t, err, errInvalidCursor, "a c2 body under p2 fails the position AAD")

	clk.t = krT0.Add(48*time.Hour - time.Second)
	_, err = b.OpenPosition(sealed, "binding-x")
	require.NoError(t, err, "gen1 still verifies before its verify_until")
	clk.t = krT0.Add(48 * time.Hour)
	_, err = b.OpenPosition(sealed, "binding-x")
	assert.ErrorIs(t, err, errInvalidCursor, "a retired generation's position cursor fails")
}

// A p2 cursor opens only under its own AAD (the v2 domain, its kid and the
// binding): each other domain, a missing kid or another kid fails.
func TestListCursorSealer_PositionRingAAD(t *testing.T) {
	clk := &krClock{t: krT0}
	ka, _ := capRollout(t, "versioned")
	sealer := positionRingSealer(ka, clk)
	gen1 := deriveSharedSigningKey(krGen1Secret, SecretKeyListCursorKey)
	sealCustom := func(prefix, aad string) string {
		aead, err := listCursorAEAD(gen1)
		require.NoError(t, err)
		nonce := bytes.Repeat([]byte{7}, aead.NonceSize())
		return prefix + base64.RawURLEncoding.EncodeToString(append(nonce, aead.Seal(nil, nonce, []byte(positionSealTestPosition), []byte(aad))...))
	}
	got, err := sealer.OpenPosition(sealCustom("p2.gen1.", "scion-list-position-v2:gen1:binding-x"), "binding-x")
	require.NoError(t, err)
	assert.Equal(t, positionSealTestPosition, got)
	for name, c := range map[string]string{
		"v1 AAD under p2":  sealCustom("p2.gen1.", "scion-list-position-v1:binding-x"),
		"AAD without kid":  sealCustom("p2.gen1.", "scion-list-position-v2:binding-x"),
		"other kid in AAD": sealCustom("p2.gen1.", "scion-list-position-v2:legacy:binding-x"),
		"list cursor AAD":  sealCustom("p2.gen1.", "scion-list-cursor-v2:gen1:binding-x"),
	} {
		_, err := sealer.OpenPosition(c, "binding-x")
		assert.ErrorIs(t, err, errInvalidCursor, name)
	}

	// A p1 cursor opens with the legacy readers, which on the versioned
	// ring above do not include gen1, so a gen1 p1 cursor would fail on
	// the key alone. On the legacy-emit ring gen1 is a legacy reader: the
	// p1 form under the v1 AAD opens, and the same body under the v2 AAD
	// (the kid in the AAD but not on the wire) fails on the AAD alone.
	lka, _ := capRollout(t, "legacy")
	legacyReads := positionRingSealer(lka, clk)
	got, err = legacyReads.OpenPosition(sealCustom("p1.", "scion-list-position-v1:binding-x"), "binding-x")
	require.NoError(t, err, "gen1 opens a p1 cursor on this ring")
	assert.Equal(t, positionSealTestPosition, got)
	_, err = legacyReads.OpenPosition(sealCustom("p1.", "scion-list-position-v2:gen1:binding-x"), "binding-x")
	assert.ErrorIs(t, err, errInvalidCursor, "kid in AAD, not in wire")
}

// With emit "legacy" the ring seals the key-ID-less p1 form, which opens
// with the ring's legacy readers.
func TestListCursorSealer_PositionRingLegacyEmit(t *testing.T) {
	clk := &krClock{t: krT0}
	ka, kb := capRollout(t, "legacy")
	sealed, err := positionRingSealer(ka, clk).SealPosition(positionSealTestPosition, "binding-x")
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(sealed, listPositionPrefix), sealed)
	got, err := positionRingSealer(kb, clk).OpenPosition(sealed, "binding-x")
	require.NoError(t, err)
	assert.Equal(t, positionSealTestPosition, got)
}

// At the active generation's sign_until no position cursor is issued
// (errCapabilityIssuanceClosed), so the rail answers a page that needs a
// continuation with 500 and no cursor.
func TestListCursorSealer_PositionIssuanceClosed(t *testing.T) {
	clk := &krClock{t: krT0.Add(time.Hour)}
	sealer := positionRingSealer(capClosing(t), clk)
	_, err := sealer.SealPosition(positionSealTestPosition, "binding-x")
	assert.ErrorIs(t, err, errCapabilityIssuanceClosed)
	srv := &Server{listCursorSealer: sealer}
	_, err = srv.sealStarredConversationsCursor(StarredConversationPosition{Value: "mia", Key: "dm:user:a:user:b"}, "binding-x")
	assert.ErrorIs(t, err, errCapabilityIssuanceClosed, "the rail's seal fails, and its handler answers 500")
}
