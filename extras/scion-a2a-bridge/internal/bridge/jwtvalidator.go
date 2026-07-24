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
	"fmt"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
)

// JWT issuer and audience must match the Hub's UserTokenService constants
// (pkg/hub/usertoken.go). Inlined here to avoid importing pkg/hub which
// would pull in database and server dependencies.
const (
	jwtIssuer   = "scion-hub"
	jwtAudience = "scion-hub-api"
)

// jwtUserClaims mirrors pkg/hub.UserTokenClaims.
type jwtUserClaims struct {
	jwt.Claims
	UserID      string `json:"uid"`
	Email       string `json:"email"`
	DisplayName string `json:"name,omitempty"`
	Role        string `json:"role"`
	TokenType   string `json:"type"`
	ClientType  string `json:"client"`
}

// JWTValidator validates Scion user JWTs locally using the Hub's HS256
// signing key. No network call is required — validation is purely
// cryptographic. This is the bridge-side equivalent of
// pkg/hub.UserTokenService.ValidateUserToken.
type JWTValidator struct {
	signingKey []byte
}

// NewJWTValidator creates a validator from the Hub's raw HS256 signing key.
func NewJWTValidator(signingKey []byte) *JWTValidator {
	return &JWTValidator{signingKey: signingKey}
}

// Validate parses and verifies a JWT, returning the caller identity on
// success. Rejects expired, tampered, or wrongly-issued tokens.
func (v *JWTValidator) Validate(tokenString string) (*CallerIdentity, error) {
	token, err := jwt.ParseSigned(tokenString, []jose.SignatureAlgorithm{jose.HS256})
	if err != nil {
		return nil, fmt.Errorf("parse JWT: %w", err)
	}

	var claims jwtUserClaims
	if err := token.Claims(v.signingKey, &claims); err != nil {
		return nil, fmt.Errorf("verify JWT signature: %w", err)
	}

	// Validate standard claims (issuer, audience, expiry, not-before).
	expected := jwt.Expected{
		Issuer:      jwtIssuer,
		AnyAudience: jwt.Audience{jwtAudience},
		Time:        time.Now(),
	}
	if err := claims.Validate(expected); err != nil {
		return nil, fmt.Errorf("JWT claims validation: %w", err)
	}

	// Reject refresh tokens — only access tokens are valid for API calls.
	if claims.TokenType == "refresh" {
		return nil, fmt.Errorf("refresh tokens are not accepted for API authentication")
	}

	if claims.UserID == "" || claims.Email == "" {
		return nil, fmt.Errorf("JWT missing required claims (uid=%q, email=%q)", claims.UserID, claims.Email)
	}

	return &CallerIdentity{
		UserID:    claims.UserID,
		Email:     claims.Email,
		Role:      claims.Role,
		RawToken:  tokenString,
		TokenType: "jwt",
	}, nil
}
