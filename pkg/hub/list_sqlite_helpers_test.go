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
	"crypto/rand"
	"encoding/base64"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func mustNewListCursorSealer(t *testing.T) *listCursorSealer {
	t.Helper()
	key := make([]byte, 32)
	_, err := rand.Read(key)
	require.NoError(t, err)
	sealer, err := newListCursorSealer(key)
	require.NoError(t, err)
	return sealer
}

// assertNoRecoverableCursorPayload asserts that sealed's raw bytes (the
// version prefix stripped, then base64-decoded) contain no recoverable trace
// of the position it carries: neither the encoded inner cursor itself, nor
// that encoding's own decoded "created,id,binding" payload, nor the item ID
// or binding as bare substrings. This is stronger than tamper-evidence: an
// authenticated-but-unencrypted (signature-only) cursor format is
// tamper-evident and key-dependent, but still fails this check, because the
// payload sits in the clear. Confidentiality, not just authentication, is
// the property under test.
func assertNoRecoverableCursorPayload(t *testing.T, sealed, inner string) {
	t.Helper()
	require.True(t, strings.HasPrefix(sealed, listCursorPrefix), "sealed cursor %q must start with %q", sealed, listCursorPrefix)
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(sealed, listCursorPrefix))
	require.NoError(t, err)
	rawStr := string(raw)

	assert.NotContains(t, rawStr, inner, "raw cursor bytes must not contain the encoded inner cursor")

	decodedInner, err := base64.URLEncoding.DecodeString(inner)
	require.NoError(t, err)
	parts := strings.SplitN(string(decodedInner), ",", 3)
	require.Len(t, parts, 3, "inner must decode to created,id,binding")
	created, id, binding := parts[0], parts[1], parts[2]

	assert.NotContains(t, rawStr, string(decodedInner), "raw cursor bytes must not contain the decoded created,id,binding payload")
	assert.NotContains(t, rawStr, id, "raw cursor bytes must not contain the item ID")
	assert.NotContains(t, rawStr, created, "raw cursor bytes must not contain the created time")
	assert.NotContains(t, rawStr, binding, "raw cursor bytes must not contain the binding")
}
