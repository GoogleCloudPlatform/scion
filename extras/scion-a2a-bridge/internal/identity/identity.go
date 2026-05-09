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

package identity

import (
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
)

const (
	tokenIssuer   = "scion-hub"
	tokenAudience = "scion-hub-api"
)

type tokenClaims struct {
	jwt.Claims
	UserID     string `json:"uid"`
	Email      string `json:"email"`
	Role       string `json:"role"`
	TokenType  string `json:"type"`
	ClientType string `json:"client"`
}

// TokenMinter creates signed user JWTs using the hub's signing key.
type TokenMinter struct {
	signer jose.Signer
}

// NewTokenMinter creates a minter from a raw signing key (HS256).
func NewTokenMinter(signingKey []byte) (*TokenMinter, error) {
	signer, err := jose.NewSigner(
		jose.SigningKey{Algorithm: jose.HS256, Key: signingKey},
		(&jose.SignerOptions{}).WithType("JWT"),
	)
	if err != nil {
		return nil, fmt.Errorf("creating signer: %w", err)
	}
	return &TokenMinter{signer: signer}, nil
}

// MintToken creates a signed JWT for the given user identity.
func (m *TokenMinter) MintToken(userID, email, role string, duration time.Duration) (string, error) {
	now := time.Now()
	claims := tokenClaims{
		Claims: jwt.Claims{
			Issuer:    tokenIssuer,
			Subject:   userID,
			Audience:  jwt.Audience{tokenAudience},
			IssuedAt:  jwt.NewNumericDate(now),
			Expiry:    jwt.NewNumericDate(now.Add(duration)),
			NotBefore: jwt.NewNumericDate(now),
		},
		UserID:     userID,
		Email:      email,
		Role:       role,
		TokenType:  "access",
		ClientType: "api",
	}

	token, err := jwt.Signed(m.signer).Claims(claims).Serialize()
	if err != nil {
		return "", fmt.Errorf("signing token: %w", err)
	}
	return token, nil
}

// MintingAuth implements hubclient.Authenticator by minting fresh JWTs on demand.
type MintingAuth struct {
	minter   *TokenMinter
	userID   string
	email    string
	role     string
	duration time.Duration

	mu      sync.Mutex
	token   string
	expires time.Time
}

// NewMintingAuth creates an authenticator that mints tokens for the given user.
func NewMintingAuth(minter *TokenMinter, userID, email, role string, duration time.Duration) *MintingAuth {
	return &MintingAuth{
		minter:   minter,
		userID:   userID,
		email:    email,
		role:     role,
		duration: duration,
	}
}

// ApplyAuth sets the Authorization header with a fresh or cached token.
// Holds a.mu across the JWT signing call on refresh, which serializes Hub
// requests during that window. Acceptable: this is an admin-only client
// with a 1-minute pre-expiry refresh window, so contention is rare.
func (a *MintingAuth) ApplyAuth(req *http.Request) error {
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.token == "" || time.Now().After(a.expires.Add(-1*time.Minute)) {
		token, err := a.minter.MintToken(a.userID, a.email, a.role, a.duration)
		if err != nil {
			return fmt.Errorf("minting auth token: %w", err)
		}
		a.token = token
		a.expires = time.Now().Add(a.duration)
	}

	req.Header.Set("Authorization", "Bearer "+a.token)
	return nil
}

// Refresh is a no-op; MintingAuth handles refresh transparently in ApplyAuth.
func (a *MintingAuth) Refresh() (bool, error) { return false, nil }
