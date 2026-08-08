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
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/secret"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- Mock secret backend for OIDC tests ---

// oidcMockSecretBackend implements secret.SecretBackend for tests.
// It stores secrets in-memory and allows controlling Get/Set behavior.
type oidcMockSecretBackend struct {
	secrets map[string]*secret.SecretWithValue // keyed by "name/scope/scopeID"
	setErr  error                              // if set, Set() returns this error
	getErr  error                              // if set, Get() returns this error
}

func newOIDCMockSecretBackend() *oidcMockSecretBackend {
	return &oidcMockSecretBackend{
		secrets: make(map[string]*secret.SecretWithValue),
	}
}

func (m *oidcMockSecretBackend) secretKey(name, scope, scopeID string) string {
	return name + "/" + scope + "/" + scopeID
}

func (m *oidcMockSecretBackend) Get(_ context.Context, name, scope, scopeID string) (*secret.SecretWithValue, error) {
	if m.getErr != nil {
		return nil, m.getErr
	}
	sv, ok := m.secrets[m.secretKey(name, scope, scopeID)]
	if !ok {
		return nil, store.ErrNotFound
	}
	return sv, nil
}

func (m *oidcMockSecretBackend) Set(_ context.Context, input *secret.SetSecretInput) (bool, *secret.SecretMeta, error) {
	if m.setErr != nil {
		return false, nil, m.setErr
	}
	key := m.secretKey(input.Name, input.Scope, input.ScopeID)
	_, existed := m.secrets[key]
	m.secrets[key] = &secret.SecretWithValue{
		Value: input.Value,
	}
	return !existed, &secret.SecretMeta{}, nil
}

func (m *oidcMockSecretBackend) Delete(_ context.Context, name, scope, scopeID string) error {
	delete(m.secrets, m.secretKey(name, scope, scopeID))
	return nil
}

func (m *oidcMockSecretBackend) List(_ context.Context, _ secret.Filter) ([]secret.SecretMeta, error) {
	return nil, nil
}

func (m *oidcMockSecretBackend) GetMeta(_ context.Context, _, _, _ string) (*secret.SecretMeta, error) {
	return nil, nil
}

func (m *oidcMockSecretBackend) Resolve(_ context.Context, _, _, _ string, _ *secret.ResolveOpts) ([]secret.SecretWithValue, error) {
	return nil, nil
}

func (m *oidcMockSecretBackend) HubID() string { return "test-hub" }

// --- Tests ---

func TestGenerateRSAKeyPair(t *testing.T) {
	tests := []struct {
		name string
	}{
		{name: "generates valid RSA-2048 key"},
		{name: "generates different key each call"},
	}

	var firstKey *rsa.PrivateKey
	for i, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			key, err := generateRSAKeyPair()
			require.NoError(t, err)
			require.NotNil(t, key)

			// Verify key size is 2048 bits
			assert.Equal(t, 2048, key.N.BitLen(), "RSA key should be 2048 bits")

			// Verify public key is extractable
			assert.NotNil(t, key.N)
			assert.NotNil(t, key.E)

			// Verify the key validates
			err = key.Validate()
			assert.NoError(t, err, "Generated RSA key should be valid")

			if i == 0 {
				firstKey = key
			} else {
				// Keys should be unique
				assert.NotEqual(t, firstKey.D.Bytes(), key.D.Bytes(),
					"Each call should generate a unique key")
			}
		})
	}
}

func TestPEMEncodingRoundTrip(t *testing.T) {
	tests := []struct {
		name string
	}{
		{name: "round-trip preserves private key"},
		{name: "round-trip preserves public key"},
	}

	key, err := generateRSAKeyPair()
	require.NoError(t, err)

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// Encode to PEM
			pemData, err := encodePEMPrivateKey(key)
			require.NoError(t, err)
			require.NotEmpty(t, pemData)

			// Verify PEM block type
			block, _ := pem.Decode(pemData)
			require.NotNil(t, block, "PEM decode should produce a block")
			assert.Equal(t, "PRIVATE KEY", block.Type, "PEM block type should be PRIVATE KEY")

			// Decode from PEM
			decoded, err := decodePEMPrivateKey(pemData)
			require.NoError(t, err)
			require.NotNil(t, decoded)

			if tc.name == "round-trip preserves private key" {
				// Private key should match
				assert.Equal(t, key.D.Bytes(), decoded.D.Bytes(),
					"Private key exponent should be preserved")
				assert.Equal(t, key.N.Bytes(), decoded.N.Bytes(),
					"Modulus should be preserved")
			} else {
				// Public key should match
				assert.Equal(t, key.N.Bytes(), decoded.N.Bytes(),
					"Public key modulus should be preserved")
				assert.Equal(t, key.E, decoded.E,
					"Public key exponent should be preserved")
			}
		})
	}
}

func TestDecodePEMPrivateKey_Errors(t *testing.T) {
	tests := []struct {
		name    string
		input   []byte
		wantErr string
	}{
		{
			name:    "no PEM block",
			input:   []byte("not a PEM block"),
			wantErr: "no PEM block found",
		},
		{
			name: "wrong PEM block type",
			input: pem.EncodeToMemory(&pem.Block{
				Type:  "RSA PRIVATE KEY",
				Bytes: []byte("dummy"),
			}),
			wantErr: "unexpected PEM block type",
		},
		{
			name: "invalid DER data",
			input: pem.EncodeToMemory(&pem.Block{
				Type:  "PRIVATE KEY",
				Bytes: []byte("invalid-der"),
			}),
			wantErr: "failed to parse PKCS#8 private key",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := decodePEMPrivateKey(tc.input)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)
		})
	}
}

func TestComputeKeyID(t *testing.T) {
	key1, err := generateRSAKeyPair()
	require.NoError(t, err)
	key2, err := generateRSAKeyPair()
	require.NoError(t, err)

	tests := []struct {
		name       string
		pub        *rsa.PublicKey
		wantPrefix string
	}{
		{
			name:       "deterministic for same key",
			pub:        &key1.PublicKey,
			wantPrefix: oidcKIDPrefix,
		},
		{
			name:       "unique across different keys",
			pub:        &key2.PublicKey,
			wantPrefix: oidcKIDPrefix,
		},
	}

	var firstKID string
	for i, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			kid := computeKeyID(tc.pub)

			// Should start with prefix
			assert.Contains(t, kid, tc.wantPrefix, "kid should start with scion-oidc- prefix")

			// Should be deterministic: calling again produces the same result
			kid2 := computeKeyID(tc.pub)
			assert.Equal(t, kid, kid2, "kid should be deterministic for the same key")

			// Should have correct length: prefix (11) + 12 hex chars = 23
			assert.Len(t, kid, len(oidcKIDPrefix)+12,
				"kid should be prefix + 12 hex chars")

			if i == 0 {
				firstKID = kid
			} else {
				// Different keys should produce different KIDs
				assert.NotEqual(t, firstKID, kid,
					"Different keys should produce different KIDs")
			}
		})
	}
}

func TestJoseSignerProducesValidRS256(t *testing.T) {
	key, err := generateRSAKeyPair()
	require.NoError(t, err)

	kid := computeKeyID(&key.PublicKey)

	signer, err := jose.NewSigner(
		jose.SigningKey{Algorithm: jose.RS256, Key: key},
		(&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", kid),
	)
	require.NoError(t, err)

	tests := []struct {
		name   string
		claims map[string]interface{}
	}{
		{
			name: "signs basic claims",
			claims: map[string]interface{}{
				"sub": "agent-123",
				"iss": "https://hub.example.com",
				"aud": "https://vault.example.com",
			},
		},
		{
			name: "signs claims with nested fields",
			claims: map[string]interface{}{
				"sub":        "agent-456",
				"iss":        "https://hub.example.com",
				"project_id": "proj-789",
				"scopes":     []string{"identity"},
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// Sign claims
			token, err := jwt.Signed(signer).Claims(tc.claims).Serialize()
			require.NoError(t, err)
			assert.NotEmpty(t, token)

			// Parse and verify with the public key
			parsed, err := jwt.ParseSigned(token, []jose.SignatureAlgorithm{jose.RS256})
			require.NoError(t, err)

			var result map[string]interface{}
			err = parsed.Claims(&key.PublicKey, &result)
			require.NoError(t, err)

			assert.Equal(t, tc.claims["sub"], result["sub"])
			assert.Equal(t, tc.claims["iss"], result["iss"])

			// Verify that verifying with a different key fails
			wrongKey, err := generateRSAKeyPair()
			require.NoError(t, err)
			var wrongResult map[string]interface{}
			err = parsed.Claims(&wrongKey.PublicKey, &wrongResult)
			assert.Error(t, err, "Verification with wrong key should fail")
		})
	}
}

func TestOIDCKeyManager_JWKS(t *testing.T) {
	s := createOIDCTestStore(t)
	ctx := context.Background()

	mgr, err := NewOIDCKeyManager(ctx, OIDCKeyManagerConfig{
		Store:     s,
		HubID:     "test-hub",
		IssuerURL: "https://hub.example.com",
	})
	require.NoError(t, err)

	tests := []struct {
		name  string
		check func(t *testing.T, jwks jose.JSONWebKeySet)
	}{
		{
			name: "returns non-empty key set",
			check: func(t *testing.T, jwks jose.JSONWebKeySet) {
				assert.NotEmpty(t, jwks.Keys, "JWKS should contain at least one key")
			},
		},
		{
			name: "key has correct algorithm and use",
			check: func(t *testing.T, jwks jose.JSONWebKeySet) {
				require.Len(t, jwks.Keys, 1)
				key := jwks.Keys[0]
				assert.Equal(t, string(jose.RS256), key.Algorithm)
				assert.Equal(t, "sig", key.Use)
			},
		},
		{
			name: "key has correct kid",
			check: func(t *testing.T, jwks jose.JSONWebKeySet) {
				require.Len(t, jwks.Keys, 1)
				key := jwks.Keys[0]
				assert.Contains(t, key.KeyID, oidcKIDPrefix)
				assert.Len(t, key.KeyID, len(oidcKIDPrefix)+12)
			},
		},
		{
			name: "key is an RSA public key",
			check: func(t *testing.T, jwks jose.JSONWebKeySet) {
				require.Len(t, jwks.Keys, 1)
				key := jwks.Keys[0]
				_, ok := key.Key.(*rsa.PublicKey)
				assert.True(t, ok, "JWKS key should be an RSA public key")
			},
		},
		{
			name: "JWKS key validates tokens from signer",
			check: func(t *testing.T, jwks jose.JSONWebKeySet) {
				require.Len(t, jwks.Keys, 1)

				// Sign a token with the manager's signer
				claims := map[string]interface{}{
					"sub": "agent-test",
					"iss": "https://hub.example.com",
				}
				token, err := jwt.Signed(mgr.Signer()).Claims(claims).Serialize()
				require.NoError(t, err)

				// Verify with the JWKS public key
				parsed, err := jwt.ParseSigned(token, []jose.SignatureAlgorithm{jose.RS256})
				require.NoError(t, err)

				var result map[string]interface{}
				err = parsed.Claims(jwks.Keys[0].Key, &result)
				require.NoError(t, err)
				assert.Equal(t, "agent-test", result["sub"])
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			jwks := mgr.JWKS()
			tc.check(t, jwks)
		})
	}
}

func TestOIDCKeyManager_LoadFromStore(t *testing.T) {
	s := createOIDCTestStore(t)
	ctx := context.Background()
	hubID := "test-hub-store"

	// First initialization: generates and stores a key
	mgr1, err := NewOIDCKeyManager(ctx, OIDCKeyManagerConfig{
		Store:     s,
		HubID:     hubID,
		IssuerURL: "https://hub.example.com",
	})
	require.NoError(t, err)
	kid1 := mgr1.JWKS().Keys[0].KeyID

	// Second initialization with the same store: should load the same key
	mgr2, err := NewOIDCKeyManager(ctx, OIDCKeyManagerConfig{
		Store:     s,
		HubID:     hubID,
		IssuerURL: "https://hub.example.com",
	})
	require.NoError(t, err)
	kid2 := mgr2.JWKS().Keys[0].KeyID

	assert.Equal(t, kid1, kid2, "Second initialization should load the same key from store")

	// Verify cross-validation: token signed by mgr1 can be verified with mgr2's JWKS
	claims := map[string]interface{}{"sub": "agent-cross"}
	token, err := jwt.Signed(mgr1.Signer()).Claims(claims).Serialize()
	require.NoError(t, err)

	parsed, err := jwt.ParseSigned(token, []jose.SignatureAlgorithm{jose.RS256})
	require.NoError(t, err)

	var result map[string]interface{}
	err = parsed.Claims(mgr2.JWKS().Keys[0].Key, &result)
	require.NoError(t, err)
	assert.Equal(t, "agent-cross", result["sub"])
}

func TestOIDCKeyManager_LoadFromBackend(t *testing.T) {
	s := createOIDCTestStore(t)
	ctx := context.Background()
	hubID := "test-hub-backend"
	backend := newOIDCMockSecretBackend()

	// First initialization with backend: generates and stores to both backend and store
	mgr1, err := NewOIDCKeyManager(ctx, OIDCKeyManagerConfig{
		Store:     s,
		Backend:   backend,
		HubID:     hubID,
		IssuerURL: "https://hub.example.com",
	})
	require.NoError(t, err)
	kid1 := mgr1.JWKS().Keys[0].KeyID

	// Verify the key was stored in the backend
	sv, err := backend.Get(ctx, SecretKeyOIDCSigningKey, store.ScopeHub, hubID)
	require.NoError(t, err)
	assert.NotEmpty(t, sv.Value, "Key should be stored in backend")

	// Create a new store (simulating fresh start) but same backend
	s2 := createOIDCTestStore(t)
	mgr2, err := NewOIDCKeyManager(ctx, OIDCKeyManagerConfig{
		Store:     s2,
		Backend:   backend,
		HubID:     hubID,
		IssuerURL: "https://hub.example.com",
	})
	require.NoError(t, err)
	kid2 := mgr2.JWKS().Keys[0].KeyID

	assert.Equal(t, kid1, kid2, "Should load the same key from backend")
}

func TestOIDCKeyManager_GenerateWhenNoKey(t *testing.T) {
	s := createOIDCTestStore(t)
	backend := newOIDCMockSecretBackend()
	ctx := context.Background()

	tests := []struct {
		name    string
		backend secret.SecretBackend
	}{
		{
			name:    "generates key with no backend",
			backend: nil,
		},
		{
			name:    "generates key with empty backend",
			backend: backend,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			mgr, err := NewOIDCKeyManager(ctx, OIDCKeyManagerConfig{
				Store:     s,
				Backend:   tc.backend,
				HubID:     "test-hub-gen-" + tc.name,
				IssuerURL: "https://hub.example.com",
			})
			require.NoError(t, err)
			require.NotNil(t, mgr)

			// Verify signer works
			signer := mgr.Signer()
			require.NotNil(t, signer)

			// Verify JWKS is populated
			jwks := mgr.JWKS()
			require.Len(t, jwks.Keys, 1)

			// Verify IssuerURL
			assert.Equal(t, "https://hub.example.com", mgr.IssuerURL())
		})
	}
}

func TestOIDCKeyManager_RequireStableSigningKey(t *testing.T) {
	s := createOIDCTestStore(t)
	backend := newOIDCMockSecretBackend()
	ctx := context.Background()

	tests := []struct {
		name    string
		backend secret.SecretBackend
		wantErr string
	}{
		{
			name:    "fails with no backend and no existing key",
			backend: nil,
			wantErr: "RequireStableSigningKey is set",
		},
		{
			name:    "fails with empty backend and no existing key",
			backend: backend,
			wantErr: "RequireStableSigningKey is set",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewOIDCKeyManager(ctx, OIDCKeyManagerConfig{
				Store:                   s,
				Backend:                 tc.backend,
				HubID:                   "test-hub-require-stable-" + tc.name,
				IssuerURL:               "https://hub.example.com",
				RequireStableSigningKey: true,
			})
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)
		})
	}

	// Verify that RequireStableSigningKey succeeds when a key exists
	t.Run("succeeds when key exists in backend", func(t *testing.T) {
		hubID := "test-hub-require-stable-exists"
		be := newOIDCMockSecretBackend()

		// First: create a key normally
		mgr1, err := NewOIDCKeyManager(ctx, OIDCKeyManagerConfig{
			Store:     s,
			Backend:   be,
			HubID:     hubID,
			IssuerURL: "https://hub.example.com",
		})
		require.NoError(t, err)
		kid1 := mgr1.JWKS().Keys[0].KeyID

		// Second: load with RequireStableSigningKey — should succeed
		mgr2, err := NewOIDCKeyManager(ctx, OIDCKeyManagerConfig{
			Store:                   s,
			Backend:                 be,
			HubID:                   hubID,
			IssuerURL:               "https://hub.example.com",
			RequireStableSigningKey: true,
		})
		require.NoError(t, err)
		kid2 := mgr2.JWKS().Keys[0].KeyID
		assert.Equal(t, kid1, kid2)
	})
}

func TestOIDCKeyManager_IssuerURL(t *testing.T) {
	s := createOIDCTestStore(t)
	ctx := context.Background()

	tests := []struct {
		name      string
		issuerURL string
	}{
		{name: "https URL", issuerURL: "https://hub.example.com"},
		{name: "localhost URL", issuerURL: "http://localhost:9810"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			mgr, err := NewOIDCKeyManager(ctx, OIDCKeyManagerConfig{
				Store:     s,
				HubID:     "test-hub-issuer-" + tc.name,
				IssuerURL: tc.issuerURL,
			})
			require.NoError(t, err)
			assert.Equal(t, tc.issuerURL, mgr.IssuerURL())
		})
	}
}

func TestOIDCSigningKeySecretID(t *testing.T) {
	tests := []struct {
		name  string
		hubID string
	}{
		{name: "deterministic", hubID: "hub-1"},
		{name: "different hub IDs produce different IDs", hubID: "hub-2"},
	}

	var firstID string
	for i, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			id := oidcSigningKeySecretID(tc.hubID)
			assert.NotEmpty(t, id)

			// Deterministic
			id2 := oidcSigningKeySecretID(tc.hubID)
			assert.Equal(t, id, id2, "Should be deterministic")

			if i == 0 {
				firstID = id
			} else {
				assert.NotEqual(t, firstID, id, "Different hub IDs should produce different secret IDs")
			}
		})
	}
}

func TestEncodePEMPrivateKey_PKCS8Format(t *testing.T) {
	key, err := generateRSAKeyPair()
	require.NoError(t, err)

	pemData, err := encodePEMPrivateKey(key)
	require.NoError(t, err)

	// Should be valid PKCS#8
	block, _ := pem.Decode(pemData)
	require.NotNil(t, block)

	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	require.NoError(t, err)

	rsaKey, ok := parsed.(*rsa.PrivateKey)
	require.True(t, ok, "Parsed key should be RSA")
	assert.Equal(t, key.D.Bytes(), rsaKey.D.Bytes())
}

// --- Key Rotation and Cleanup Tests ---

func TestOIDCKeyManager_RotateKey(t *testing.T) {
	s := createOIDCTestStore(t)
	ctx := context.Background()

	mgr, err := NewOIDCKeyManager(ctx, OIDCKeyManagerConfig{
		Store:     s,
		HubID:     "test-hub-rotate",
		IssuerURL: "https://hub.example.com",
	})
	require.NoError(t, err)

	originalKID := mgr.JWKS().Keys[0].KeyID
	originalSigner := mgr.Signer()

	// Sign a token with the original key before rotation.
	preRotationClaims := map[string]interface{}{
		"sub": "agent-pre-rotate",
		"iss": "https://hub.example.com",
	}
	preRotationToken, err := jwt.Signed(originalSigner).Claims(preRotationClaims).Serialize()
	require.NoError(t, err)

	// Rotate.
	err = mgr.RotateKey(ctx)
	require.NoError(t, err)

	tests := []struct {
		name  string
		check func(t *testing.T)
	}{
		{
			name: "rotation produces new kid",
			check: func(t *testing.T) {
				jwks := mgr.JWKS()
				require.NotEmpty(t, jwks.Keys)
				// Active key (first in JWKS) should have a different kid.
				newKID := jwks.Keys[0].KeyID
				assert.NotEqual(t, originalKID, newKID,
					"After rotation, active key should have a different kid")
			},
		},
		{
			name: "old key remains in JWKS",
			check: func(t *testing.T) {
				jwks := mgr.JWKS()
				require.Len(t, jwks.Keys, 2, "JWKS should contain both old and new keys")
				kids := []string{jwks.Keys[0].KeyID, jwks.Keys[1].KeyID}
				assert.Contains(t, kids, originalKID, "Old key should still be in JWKS")
			},
		},
		{
			name: "signing uses new key",
			check: func(t *testing.T) {
				newClaims := map[string]interface{}{
					"sub": "agent-post-rotate",
					"iss": "https://hub.example.com",
				}
				token, signErr := jwt.Signed(mgr.Signer()).Claims(newClaims).Serialize()
				require.NoError(t, signErr)

				parsed, parseErr := jwt.ParseSigned(token, []jose.SignatureAlgorithm{jose.RS256})
				require.NoError(t, parseErr)

				// The token's kid header should match the new active key.
				jwks := mgr.JWKS()
				newKID := jwks.Keys[0].KeyID
				assert.NotEqual(t, originalKID, newKID)

				// Token should verify with the new key.
				var result map[string]interface{}
				verifyErr := parsed.Claims(jwks.Keys[0].Key, &result)
				require.NoError(t, verifyErr)
				assert.Equal(t, "agent-post-rotate", result["sub"])
			},
		},
		{
			name: "old tokens still verifiable via JWKS",
			check: func(t *testing.T) {
				parsed, parseErr := jwt.ParseSigned(preRotationToken, []jose.SignatureAlgorithm{jose.RS256})
				require.NoError(t, parseErr)

				// Find the old key in JWKS.
				jwks := mgr.JWKS()
				var oldKey interface{}
				for _, k := range jwks.Keys {
					if k.KeyID == originalKID {
						oldKey = k.Key
						break
					}
				}
				require.NotNil(t, oldKey, "Old key must still be in JWKS")

				var result map[string]interface{}
				verifyErr := parsed.Claims(oldKey, &result)
				require.NoError(t, verifyErr, "Token signed before rotation should still verify with old key from JWKS")
				assert.Equal(t, "agent-pre-rotate", result["sub"])
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tc.check(t)
		})
	}
}

func TestOIDCKeyManager_CleanupExpiredKeys(t *testing.T) {
	s := createOIDCTestStore(t)
	ctx := context.Background()

	mgr, err := NewOIDCKeyManager(ctx, OIDCKeyManagerConfig{
		Store:     s,
		HubID:     "test-hub-cleanup",
		IssuerURL: "https://hub.example.com",
	})
	require.NoError(t, err)

	// Rotate to get a second key.
	err = mgr.RotateKey(ctx)
	require.NoError(t, err)
	require.Len(t, mgr.JWKS().Keys, 2, "Should have 2 keys after rotation")

	tests := []struct {
		name  string
		setup func()
		check func(t *testing.T)
	}{
		{
			name: "keeps recent inactive keys",
			setup: func() {
				// Inactive key is recent (created moments ago) — should be kept.
			},
			check: func(t *testing.T) {
				mgr.CleanupExpiredKeys()
				jwks := mgr.JWKS()
				assert.Len(t, jwks.Keys, 2, "Recent inactive key should not be removed")
			},
		},
		{
			name: "removes old inactive keys",
			setup: func() {
				// Artificially age the inactive key's deactivation time past the overlap window.
				mgr.mu.Lock()
				for _, k := range mgr.allKeys {
					if !k.Active {
						k.DeactivatedAt = time.Now().Add(-25 * time.Hour) // 25h ago
					}
				}
				mgr.mu.Unlock()
			},
			check: func(t *testing.T) {
				mgr.CleanupExpiredKeys()
				jwks := mgr.JWKS()
				assert.Len(t, jwks.Keys, 1, "Old inactive key should be removed")
				assert.True(t, jwks.Keys[0].KeyID != "", "Remaining key should have a kid")
			},
		},
		{
			name: "never removes active key regardless of age",
			setup: func() {
				// Age the active key past the overlap window.
				mgr.mu.Lock()
				for _, k := range mgr.allKeys {
					if k.Active {
						k.CreatedAt = time.Now().Add(-48 * time.Hour) // 48h ago
					}
				}
				mgr.mu.Unlock()
			},
			check: func(t *testing.T) {
				mgr.CleanupExpiredKeys()
				jwks := mgr.JWKS()
				assert.Len(t, jwks.Keys, 1, "Active key should never be removed")
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tc.setup()
			tc.check(t)
		})
	}
}

func TestOIDCKeyManager_RotateKey_MultipleRotations(t *testing.T) {
	s := createOIDCTestStore(t)
	ctx := context.Background()

	mgr, err := NewOIDCKeyManager(ctx, OIDCKeyManagerConfig{
		Store:     s,
		HubID:     "test-hub-multi-rotate",
		IssuerURL: "https://hub.example.com",
	})
	require.NoError(t, err)

	// Rotate three times.
	kids := make([]string, 0, 4)
	kids = append(kids, mgr.JWKS().Keys[0].KeyID)

	for i := 0; i < 3; i++ {
		err = mgr.RotateKey(ctx)
		require.NoError(t, err)
		jwks := mgr.JWKS()
		kids = append(kids, jwks.Keys[0].KeyID)
	}

	t.Run("all kids are unique", func(t *testing.T) {
		seen := make(map[string]bool)
		for _, kid := range kids {
			assert.False(t, seen[kid], "kid %s should be unique", kid)
			seen[kid] = true
		}
	})

	t.Run("JWKS contains all keys", func(t *testing.T) {
		jwks := mgr.JWKS()
		assert.Len(t, jwks.Keys, 4, "JWKS should contain original + 3 rotated keys")
	})

	t.Run("only one key is active", func(t *testing.T) {
		mgr.mu.RLock()
		defer mgr.mu.RUnlock()
		activeCount := 0
		for _, k := range mgr.allKeys {
			if k.Active {
				activeCount++
			}
		}
		assert.Equal(t, 1, activeCount, "Exactly one key should be active")
	})
}

func TestOIDCKeyManager_RotateKey_WithBackend(t *testing.T) {
	s := createOIDCTestStore(t)
	ctx := context.Background()
	backend := newOIDCMockSecretBackend()

	mgr, err := NewOIDCKeyManager(ctx, OIDCKeyManagerConfig{
		Store:     s,
		Backend:   backend,
		HubID:     "test-hub-rotate-backend",
		IssuerURL: "https://hub.example.com",
	})
	require.NoError(t, err)

	err = mgr.RotateKey(ctx)
	require.NoError(t, err)

	// Verify the rotated key was persisted to the backend.
	sv, err := backend.Get(ctx, SecretKeyOIDCSigningKey, store.ScopeHub, "test-hub-rotate-backend")
	require.NoError(t, err)
	assert.NotEmpty(t, sv.Value, "Rotated key should be persisted to backend")

	// The persisted key should be the new active key.
	privKey, err := decodePEMPrivateKey([]byte(sv.Value))
	require.NoError(t, err)
	newKID := computeKeyID(&privKey.PublicKey)
	assert.Equal(t, mgr.JWKS().Keys[0].KeyID, newKID,
		"Persisted key should match the new active key")
}

func TestOIDCKeyManager_StartCleanupLoop(t *testing.T) {
	s := createOIDCTestStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	mgr, err := NewOIDCKeyManager(ctx, OIDCKeyManagerConfig{
		Store:     s,
		HubID:     "test-hub-cleanup-loop",
		IssuerURL: "https://hub.example.com",
	})
	require.NoError(t, err)

	// Rotate and age the old key past the overlap window.
	err = mgr.RotateKey(ctx)
	require.NoError(t, err)
	require.Len(t, mgr.JWKS().Keys, 2)

	mgr.mu.Lock()
	for _, k := range mgr.allKeys {
		if !k.Active {
			k.DeactivatedAt = time.Now().Add(-25 * time.Hour)
		}
	}
	mgr.mu.Unlock()

	// Manually call CleanupExpiredKeys to verify (we don't wait for the ticker
	// since the 1-hour interval is too long for a unit test).
	mgr.CleanupExpiredKeys()
	assert.Len(t, mgr.JWKS().Keys, 1, "Expired key should be cleaned up")

	// Verify the cleanup loop can be started and stopped via context cancellation
	// without panicking.
	mgr.StartCleanupLoop(ctx)
	cancel()
}

func TestOIDCKeyManager_CleanupUsesDeactivationTime(t *testing.T) {
	// Regression test for R1: CleanupExpiredKeys must use DeactivatedAt, not CreatedAt.
	// A key created 48h ago but deactivated just now should NOT be cleaned up.
	s := createOIDCTestStore(t)
	ctx := context.Background()

	mgr, err := NewOIDCKeyManager(ctx, OIDCKeyManagerConfig{
		Store:     s,
		HubID:     "test-hub-deactivation-time",
		IssuerURL: "https://hub.example.com",
	})
	require.NoError(t, err)

	// Artificially age the initial key's CreatedAt to 48 hours ago.
	mgr.mu.Lock()
	mgr.activeKey.CreatedAt = time.Now().Add(-48 * time.Hour)
	mgr.mu.Unlock()

	// Rotate — the old key is now inactive with DeactivatedAt = now.
	err = mgr.RotateKey(ctx)
	require.NoError(t, err)
	require.Len(t, mgr.JWKS().Keys, 2, "Should have 2 keys after rotation")

	// Run cleanup — old key should NOT be removed because DeactivatedAt is recent.
	mgr.CleanupExpiredKeys()
	jwks := mgr.JWKS()
	assert.Len(t, jwks.Keys, 2,
		"Old key created 48h ago but deactivated just now should still be in JWKS")
}

func TestOIDCKeyManager_RotateKey_PersistFailure(t *testing.T) {
	// Regression test for R2: RotateKey should return an error and leave
	// in-memory state unchanged when all persistence paths fail.
	ctx := context.Background()
	backend := newOIDCMockSecretBackend()

	// Initialize with a backend (no store) so there is only one persistence path.
	mgr, err := NewOIDCKeyManager(ctx, OIDCKeyManagerConfig{
		Backend:   backend,
		HubID:     "test-hub-persist-fail",
		IssuerURL: "https://hub.example.com",
	})
	require.NoError(t, err)

	originalKID := mgr.JWKS().Keys[0].KeyID
	originalSigner := mgr.Signer()

	// Inject error on the backend — the only persistence path.
	backend.setErr = fmt.Errorf("backend unavailable")

	// Attempt rotation — should fail because all persistence paths fail.
	err = mgr.RotateKey(ctx)
	require.Error(t, err, "RotateKey should return an error when all persistence fails")
	assert.Contains(t, err.Error(), "all persistence paths failed")

	// Verify in-memory state is unchanged.
	assert.Equal(t, originalKID, mgr.JWKS().Keys[0].KeyID,
		"Active key should be unchanged after persist failure")
	assert.Len(t, mgr.JWKS().Keys, 1,
		"No new key should be added after persist failure")

	currentSigner := mgr.Signer()
	assert.Equal(t, originalSigner, currentSigner,
		"Signer should be unchanged after persist failure")

	// Verify the old key is still active.
	mgr.mu.RLock()
	assert.True(t, mgr.activeKey.Active, "Active key should still be marked active")
	mgr.mu.RUnlock()
}

// createOIDCTestStore creates an in-memory SQLite store for OIDC tests.
func createOIDCTestStore(t *testing.T) store.Store {
	t.Helper()
	s, err := newTestStore(":memory:")
	if err != nil {
		t.Fatalf("failed to create test store: %v", err)
	}
	if err := s.Migrate(context.Background()); err != nil {
		t.Fatalf("failed to migrate test store: %v", err)
	}
	return s
}
