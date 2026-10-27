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
	"encoding/base64"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// SealPosition and OpenPosition: opaque positions that do not fit
// authorizedList's "created,id" cursor (the Starred rail's sort value and
// conversation key), sealed with the list-cursor key under their own AAD
// domain and prefix.

const testPosition = `{"value":"mia","key":"dm:user:a:user:b"}`

func TestListCursorSealer_PositionRoundTrip(t *testing.T) {
	sealer := mustNewListCursorSealer(t)
	sealed, err := sealer.SealPosition(testPosition, "binding-x")
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(sealed, listPositionPrefix))
	assert.NotContains(t, sealed, "mia", "the sealed form carries no plaintext")

	opened, err := sealer.OpenPosition(sealed, "binding-x")
	require.NoError(t, err)
	assert.Equal(t, testPosition, opened)
}

// A position cursor never opens as a list cursor, and a list cursor never
// opens as a position cursor, under the same key and binding.
func TestListCursorSealer_PositionAndListCursorsDoNotCross(t *testing.T) {
	sealer := mustNewListCursorSealer(t)
	position, err := sealer.SealPosition(testPosition, "binding-x")
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
	sealed, err := sealer.SealPosition(testPosition, "binding-x")
	require.NoError(t, err)
	body := strings.TrimPrefix(sealed, listPositionPrefix)
	raw, err := base64.RawURLEncoding.DecodeString(body)
	require.NoError(t, err)
	flipped := append([]byte(nil), raw...)
	flipped[len(flipped)-1] ^= 0x01

	for name, c := range map[string]struct{ cursor, binding string }{
		"wrong binding":  {sealed, "binding-y"},
		"tampered byte":  {listPositionPrefix + base64.RawURLEncoding.EncodeToString(flipped), "binding-x"},
		"truncated":      {sealed[:len(sealed)-4], "binding-x"},
		"nonce only":     {listPositionPrefix + base64.RawURLEncoding.EncodeToString(raw[:4]), "binding-x"},
		"no prefix":      {body, "binding-x"},
		"not base64":     {listPositionPrefix + "!!!", "binding-x"},
		"empty":          {"", "binding-x"},
		"plaintext":      {testPosition, "binding-x"},
		"another prefix": {"p2." + body, "binding-x"},
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
	_, err := sealer.SealPosition(testPosition, "binding-x")
	assert.ErrorIs(t, err, errListCursorSealerUnavailable)
	_, err = sealer.OpenPosition(listPositionPrefix+"AAAA", "binding-x")
	assert.ErrorIs(t, err, errInvalidCursor)
}
