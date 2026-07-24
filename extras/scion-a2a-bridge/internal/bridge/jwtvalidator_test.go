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

package bridge

import (
	"crypto/rand"
	"strings"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
)

// mintTestJWT creates a signed JWT for testing.
func mintTestJWT(t *testing.T, signingKey []byte, claims jwtUserClaims) string {
	t.Helper()
	signer, err := jose.NewSigner(
		jose.SigningKey{Algorithm: jose.HS256, Key: signingKey},
		(&jose.SignerOptions{}).WithType("JWT"),
	)
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}
	token, err := jwt.Signed(signer).Claims(claims).Serialize()
	if err != nil {
		t.Fatalf("Serialize: %v", err)
	}
	return token
}

func testSigningKey(t *testing.T) []byte {
	t.Helper()
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatalf("rand.Read: %v", err)
	}
	return key
}

func validClaims() jwtUserClaims {
	now := time.Now()
	return jwtUserClaims{
		Claims: jwt.Claims{
			Issuer:    jwtIssuer,
			Subject:   "user-1",
			Audience:  jwt.Audience{jwtAudience},
			IssuedAt:  jwt.NewNumericDate(now.Add(-1 * time.Minute)),
			Expiry:    jwt.NewNumericDate(now.Add(15 * time.Minute)),
			NotBefore: jwt.NewNumericDate(now.Add(-1 * time.Minute)),
		},
		UserID:     "user-1",
		Email:      "alice@example.com",
		Role:       "user",
		TokenType:  "access",
		ClientType: "api",
	}
}

func TestJWTValidator_ValidToken(t *testing.T) {
	key := testSigningKey(t)
	v := NewJWTValidator(key)

	claims := validClaims()
	token := mintTestJWT(t, key, claims)

	id, err := v.Validate(token)
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if id.UserID != "user-1" {
		t.Errorf("UserID = %q, want %q", id.UserID, "user-1")
	}
	if id.Email != "alice@example.com" {
		t.Errorf("Email = %q, want %q", id.Email, "alice@example.com")
	}
	if id.Role != "user" {
		t.Errorf("Role = %q, want %q", id.Role, "user")
	}
	if id.TokenType != "jwt" {
		t.Errorf("TokenType = %q, want %q", id.TokenType, "jwt")
	}
	if id.RawToken != token {
		t.Error("RawToken mismatch")
	}
}

func TestJWTValidator_ExpiredToken(t *testing.T) {
	key := testSigningKey(t)
	v := NewJWTValidator(key)

	claims := validClaims()
	claims.Expiry = jwt.NewNumericDate(time.Now().Add(-1 * time.Hour))

	token := mintTestJWT(t, key, claims)
	_, err := v.Validate(token)
	if err == nil {
		t.Fatal("expected error for expired token")
	}
}

func TestJWTValidator_WrongSigningKey(t *testing.T) {
	realKey := testSigningKey(t)
	wrongKey := testSigningKey(t)

	v := NewJWTValidator(realKey)

	claims := validClaims()
	token := mintTestJWT(t, wrongKey, claims)

	_, err := v.Validate(token)
	if err == nil {
		t.Fatal("expected error for wrong signing key")
	}
}

func TestJWTValidator_WrongIssuer(t *testing.T) {
	key := testSigningKey(t)
	v := NewJWTValidator(key)

	claims := validClaims()
	claims.Issuer = "wrong-issuer"

	token := mintTestJWT(t, key, claims)
	_, err := v.Validate(token)
	if err == nil {
		t.Fatal("expected error for wrong issuer")
	}
}

func TestJWTValidator_WrongAudience(t *testing.T) {
	key := testSigningKey(t)
	v := NewJWTValidator(key)

	claims := validClaims()
	claims.Audience = jwt.Audience{"wrong-audience"}

	token := mintTestJWT(t, key, claims)
	_, err := v.Validate(token)
	if err == nil {
		t.Fatal("expected error for wrong audience")
	}
}

func TestJWTValidator_MissingClaims(t *testing.T) {
	key := testSigningKey(t)
	v := NewJWTValidator(key)

	claims := validClaims()
	claims.UserID = "" // missing
	claims.Email = ""  // missing

	token := mintTestJWT(t, key, claims)
	_, err := v.Validate(token)
	if err == nil {
		t.Fatal("expected error for missing required claims")
	}
}

func TestJWTValidator_CLIToken(t *testing.T) {
	key := testSigningKey(t)
	v := NewJWTValidator(key)

	claims := validClaims()
	claims.TokenType = "access"
	claims.ClientType = "cli"

	token := mintTestJWT(t, key, claims)
	id, err := v.Validate(token)
	if err != nil {
		t.Fatalf("CLI token should be valid: %v", err)
	}
	if id.UserID != "user-1" {
		t.Errorf("UserID = %q, want %q", id.UserID, "user-1")
	}
}

func TestJWTValidator_RefreshTokenRejected(t *testing.T) {
	key := testSigningKey(t)
	v := NewJWTValidator(key)

	claims := validClaims()
	claims.TokenType = "refresh"

	token := mintTestJWT(t, key, claims)
	_, err := v.Validate(token)
	if err == nil {
		t.Fatal("expected error for refresh token")
	}
	if !strings.Contains(err.Error(), "refresh tokens") {
		t.Errorf("error should mention refresh tokens, got: %v", err)
	}
}

func TestJWTValidator_MalformedToken(t *testing.T) {
	key := testSigningKey(t)
	v := NewJWTValidator(key)

	_, err := v.Validate("not-a-jwt")
	if err == nil {
		t.Fatal("expected error for malformed token")
	}
}
