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
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/keyring"
	"github.com/GoogleCloudPlatform/scion/pkg/secret"
	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Legacy (no keyring file) mode holds one key: the keyring's "legacy"
// generation. It accepts the ID-less legacy forms and the versioned forms
// whose key ID is "legacy" (the same key, for rollback from a ring that
// emits versioned values with "legacy" active). Every other key ID is
// refused, even when the value is genuinely sealed or MAC'd with that same
// key. Synthetic fixtures only.

// legacyKIDSecret is 32 bytes, so it is also a valid AES-256 list cursor
// key and a valid non-legacy keyring signing secret.
const legacyKIDSecret = "test-only-legacy-kid-secret-0123"

// legacyKIDKeyring parses a keyring file whose "legacy" and "k2"
// generations hold the same key material, emitting versioned values and
// writing enc:v2: under active (for signing and encryption alike).
func legacyKIDKeyring(t *testing.T, active string) *keyring.Keyring {
	t.Helper()
	encKey := base64.StdEncoding.EncodeToString(keyring.DeriveLegacyEncryptionKey(legacyKIDSecret))
	doc := map[string]any{
		"schema":   1,
		"revision": "test-rev-legacy-kid",
		"signing": map[string]any{
			"active": active, "emit": "versioned", "legacy_read_ids": []any{"legacy"},
			"keys": []any{
				map[string]any{"id": "legacy", "secret": legacyKIDSecret, "sign_until": nil, "verify_until": nil},
				map[string]any{"id": "k2", "secret": legacyKIDSecret, "sign_until": nil, "verify_until": nil},
			},
		},
		"encryption": map[string]any{
			"active": active, "write_format": "v2", "legacy_v1_read_ids": []any{"legacy"},
			"keys": []any{
				map[string]any{"id": "legacy", "key_b64": encKey},
				map[string]any{"id": "k2", "key_b64": encKey},
			},
		},
	}
	data, err := json.Marshal(doc)
	require.NoError(t, err)
	k, err := keyring.Parse(data)
	require.NoError(t, err)
	return k
}

// ringFixedKeys are capability keys over kr's signing ring with the purpose
// fixedCapabilityKeys uses, so every generation's key is legacyKIDSecret.
func ringFixedKeys(kr *keyring.Keyring) capabilityKeys {
	return capabilityKeys{ring: kr.Signing(), purpose: fixedCapabilityKeyPurpose}
}

// legacyKIDCases are the key IDs a versioned value may carry and whether
// legacy mode accepts it.
var legacyKIDCases = []struct {
	kid    string
	accept bool
}{
	{"legacy", true},
	{"k2", false},
}

func TestLegacyMode_VersionedValueCipherAcceptsOnlyLegacyKID(t *testing.T) {
	legacyCipher := secret.NewLegacyValueCipher(legacyKIDSecret)
	for _, tc := range legacyKIDCases {
		t.Run(tc.kid, func(t *testing.T) {
			writer, err := secret.NewValueCipher(legacyKIDKeyring(t, tc.kid))
			require.NoError(t, err)
			sealed, err := writer.Encrypt("payload")
			require.NoError(t, err)
			require.True(t, strings.HasPrefix(sealed, "enc:v2:"+tc.kid+":"), "fixture must be enc:v2:%s:", tc.kid)

			got, err := legacyCipher.Decrypt(sealed)
			if tc.accept {
				require.NoError(t, err)
				assert.Equal(t, "payload", got)
				return
			}
			assert.ErrorIs(t, err, secret.ErrUnknownKeyID)
			assert.Empty(t, got)
		})
	}
}

func TestLegacyMode_AttachmentDeliveryAcceptsOnlyLegacyKID(t *testing.T) {
	legacySrv := &Server{attachmentDeliveryKeys: fixedCapabilityKeys([]byte(legacyKIDSecret))}
	now := time.Now()
	c := deliveryCapability{ArtifactID: "art", MessageID: "msg", AgentID: "agent", BrokerID: "broker",
		Exp: now.Add(time.Minute).Unix(), Nonce: "nonce"}

	legacyTok, err := legacySrv.signDeliveryCapability(c)
	require.NoError(t, err)
	_, ok := legacySrv.verifyDeliveryCapability(legacyTok, now)
	assert.True(t, ok, "the ID-less legacy form must be accepted")

	for _, tc := range legacyKIDCases {
		t.Run(tc.kid, func(t *testing.T) {
			minter := &Server{attachmentDeliveryKeys: ringFixedKeys(legacyKIDKeyring(t, tc.kid))}
			tok, err := minter.signDeliveryCapability(c)
			require.NoError(t, err)
			require.True(t, strings.HasPrefix(tok, "ad2."+tc.kid+"."), "fixture must be ad2.%s.", tc.kid)
			_, ok := minter.verifyDeliveryCapability(tok, now)
			require.True(t, ok, "fixture must verify under its own ring")

			got, ok := legacySrv.verifyDeliveryCapability(tok, now)
			assert.Equal(t, tc.accept, ok)
			if tc.accept {
				assert.Equal(t, c, got)
			}
		})
	}
}

func TestLegacyMode_ListCursorAcceptsOnlyLegacyKID(t *testing.T) {
	legacySealer, err := newListCursorSealer([]byte(legacyKIDSecret))
	require.NoError(t, err)
	const binding = "binding"
	// Open checks the plaintext's shape: a real "created,id,binding" cursor.
	inner := base64.URLEncoding.EncodeToString([]byte(
		time.Now().UTC().Format(time.RFC3339Nano) + "," + uuid.NewString() + "," + binding))

	legacyCursor, err := legacySealer.Seal(inner, binding)
	require.NoError(t, err)
	got, err := legacySealer.Open(legacyCursor, binding)
	require.NoError(t, err, "the ID-less legacy form must be accepted")
	assert.Equal(t, inner, got)

	for _, tc := range legacyKIDCases {
		t.Run(tc.kid, func(t *testing.T) {
			minter := newRingListCursorSealer(ringFixedKeys(legacyKIDKeyring(t, tc.kid)))
			cursor, err := minter.Seal(inner, binding)
			require.NoError(t, err)
			require.True(t, strings.HasPrefix(cursor, "c2."+tc.kid+"."), "fixture must be c2.%s.", tc.kid)
			_, err = minter.Open(cursor, binding)
			require.NoError(t, err, "fixture must open under its own ring")

			got, err := legacySealer.Open(cursor, binding)
			if tc.accept {
				require.NoError(t, err)
				assert.Equal(t, inner, got)
				return
			}
			assert.ErrorIs(t, err, errInvalidCursor)
		})
	}
}

func TestLegacyMode_ConduitAdmissionAcceptsOnlyLegacyKID(t *testing.T) {
	legacySrv := &Server{config: ServerConfig{SharedSigningSecret: legacyKIDSecret}}
	claims := conduitAdmissionClaims{SessionID: "sess", Relay: "relay"}

	legacyTok, err := legacySrv.mintAdmissionToken(claims)
	require.NoError(t, err)
	_, ok := legacySrv.openAdmissionToken(legacyTok)
	assert.True(t, ok, "the ID-less legacy form must be accepted")

	for _, tc := range legacyKIDCases {
		t.Run(tc.kid, func(t *testing.T) {
			minter := &Server{config: ServerConfig{Keyring: legacyKIDKeyring(t, tc.kid)}}
			tok, err := minter.mintAdmissionToken(claims)
			require.NoError(t, err)
			require.True(t, strings.HasPrefix(tok, "ca3."+tc.kid+"."), "fixture must be ca3.%s.", tc.kid)
			_, ok := minter.openAdmissionToken(tok)
			require.True(t, ok, "fixture must open under its own ring")

			_, ok = legacySrv.openAdmissionToken(tok)
			assert.Equal(t, tc.accept, ok)
		})
	}
}

// legacyJWT signs claims with key, setting the protected kid header when
// kid is non-empty.
func legacyJWT(t *testing.T, key []byte, kid string, claims any) string {
	t.Helper()
	opts := (&jose.SignerOptions{}).WithType("JWT")
	if kid != "" {
		opts = opts.WithHeader(jose.HeaderKey("kid"), kid)
	}
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.HS256, Key: key}, opts)
	require.NoError(t, err)
	tok, err := jwt.Signed(signer).Claims(claims).Serialize()
	require.NoError(t, err)
	return tok
}

// legacyJWTCases is the legacy-mode JWT table: an absent kid or "legacy"
// is accepted; any other kid is refused although the token is MAC'd with
// the service's own key.
var legacyJWTCases = []struct {
	name   string
	kid    string
	accept bool
}{
	{"absent kid", "", true},
	{"kid legacy", "legacy", true},
	{"ring kid", "k2", false},
	{"case variant of legacy", "LEGACY", false},
	{"other kid", "gen1", false},
}

func TestLegacyMode_AgentJWTKeyID(t *testing.T) {
	key := []byte(legacyKIDSecret)
	svc, err := NewAgentTokenService(AgentTokenConfig{SigningKey: key})
	require.NoError(t, err)
	now := time.Now()
	claims := AgentTokenClaims{
		Claims: jwt.Claims{
			Subject: "agent-1", Issuer: AgentTokenIssuer, Audience: jwt.Audience{AgentTokenAudience},
			IssuedAt: jwt.NewNumericDate(now), Expiry: jwt.NewNumericDate(now.Add(time.Hour)),
		},
		ScopeSchema: CurrentAgentScopeSchema,
	}
	for _, tc := range legacyJWTCases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := svc.ValidateAgentToken(legacyJWT(t, key, tc.kid, claims))
			if tc.accept {
				require.NoError(t, err)
				assert.Equal(t, "agent-1", got.Subject)
				return
			}
			require.ErrorIs(t, err, errJWTLegacyKeyID, "want the legacy key ID refusal")
			assert.Nil(t, got)
			assert.NotContains(t, err.Error(), tc.kid, "the refusal must not echo the key ID")
		})
	}
}

func TestLegacyMode_UserJWTKeyID(t *testing.T) {
	key := []byte(legacyKIDSecret)
	svc, err := NewUserTokenService(UserTokenConfig{SigningKey: key})
	require.NoError(t, err)
	now := time.Now()
	claims := UserTokenClaims{
		Claims: jwt.Claims{
			Subject: "user-1", Issuer: UserTokenIssuer, Audience: jwt.Audience{UserTokenAudience},
			IssuedAt: jwt.NewNumericDate(now), Expiry: jwt.NewNumericDate(now.Add(time.Hour)),
		},
		UserID: "user-1", TokenType: TokenTypeAccess,
	}
	for _, tc := range legacyJWTCases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := svc.ValidateUserToken(legacyJWT(t, key, tc.kid, claims))
			if tc.accept {
				require.NoError(t, err)
				assert.Equal(t, "user-1", got.UserID)
				return
			}
			require.ErrorIs(t, err, errJWTLegacyKeyID, "want the legacy key ID refusal")
			assert.Nil(t, got)
			assert.NotContains(t, err.Error(), tc.kid, "the refusal must not echo the key ID")
		})
	}
}
