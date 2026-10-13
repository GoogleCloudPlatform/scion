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
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
)

// TestIAPAuthenticator_RejectsOtherAlgorithms: the IAP authenticator
// accepts ES256 only. An unsigned (alg none) assertion, an RS256 one and an
// HS256 one keyed with the ES256 public key (algorithm confusion) are each
// refused, even with the expected kid and valid claims.
func TestIAPAuthenticator_RejectsOtherAlgorithms(t *testing.T) {
	kp := newTestKeyPair(t, "iap-kid")
	jwks := startJWKSServer(t, kp.jwksJSON(t))
	const aud = "/projects/123/global/backendServices/456"
	now := time.Now()
	claims := makeTestClaims("sub", "user@example.com", DefaultIAPIssuer, aud, now.Add(-time.Minute), now.Add(5*time.Minute))
	auth := &IAPAuthenticator{Audience: aud, JWKSURL: jwks.URL}
	check := func(name, assertion string) {
		t.Helper()
		req := httptest.NewRequest("GET", "/", nil)
		req.Header.Set(IAPAssertionHeader, assertion)
		info, err := auth.Authenticate(req)
		if err == nil || info != nil {
			t.Errorf("%s: accepted (%+v, %v)", name, info, err)
		}
	}

	// The positive control: the same claims signed ES256 are accepted.
	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set(IAPAssertionHeader, kp.signJWT(t, claims))
	if info, err := auth.Authenticate(req); err != nil || info == nil {
		t.Fatalf("ES256 control refused: %v", err)
	}

	header, _ := json.Marshal(map[string]string{"alg": "none", "typ": "JWT", "kid": "iap-kid"})
	payload, _ := json.Marshal(claims)
	check("alg none", base64.RawURLEncoding.EncodeToString(header)+"."+base64.RawURLEncoding.EncodeToString(payload)+".")

	rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	check("RS256", signWith(t, jose.SigningKey{Algorithm: jose.RS256, Key: rsaKey}, claims))

	pub, err := x509.MarshalPKIXPublicKey(&kp.privateKey.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	check("HS256 keyed with the public key", signWith(t, jose.SigningKey{Algorithm: jose.HS256, Key: pub}, claims))
}

func signWith(t *testing.T, key jose.SigningKey, claims any) string {
	t.Helper()
	signer, err := jose.NewSigner(key, (&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", "iap-kid"))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := jwt.Signed(signer).Claims(claims).Serialize()
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
