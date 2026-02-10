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
	"encoding/base64"
	"fmt"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
)

const (
	// UserTokenIssuer is the issuer claim for user tokens.
	UserTokenIssuer = "scion-hub"
	// UserTokenAudience is the audience claim for user tokens.
	UserTokenAudience = "scion-hub-api"
	// DefaultAccessTokenDuration is the default validity for access tokens.
	DefaultAccessTokenDuration = 15 * time.Minute
	// DefaultCLIAccessTokenDuration is the longer validity for CLI access tokens.
	DefaultCLIAccessTokenDuration = 30 * 24 * time.Hour // 30 days
	// DefaultRefreshTokenDuration is the default validity for refresh tokens.
	DefaultRefreshTokenDuration = 7 * 24 * time.Hour // 7 days
)

// UserTokenType represents the type of user token.
type UserTokenType string

const (
	// TokenTypeAccess is a short-lived access token.
	TokenTypeAccess UserTokenType = "access"
	// TokenTypeRefresh is a longer-lived refresh token.
	TokenTypeRefresh UserTokenType = "refresh"
	// TokenTypeCLI is a long-lived CLI token.
	TokenTypeCLI UserTokenType = "cli"
)

// ClientType represents the type of client.
type ClientType string

const (
	// ClientTypeWeb represents a web browser client.
	ClientTypeWeb ClientType = "web"
	// ClientTypeCLI represents a CLI client.
	ClientTypeCLI ClientType = "cli"
	// ClientTypeAPI represents a programmatic API client.
	ClientTypeAPI ClientType = "api"
)

// UserTokenClaims represents the custom claims in a user JWT.
type UserTokenClaims struct {
	jwt.Claims
	UserID      string        `json:"uid"`
	Email       string        `json:"email"`
	DisplayName string        `json:"name,omitempty"`
	Role        string        `json:"role"`
	TokenType   UserTokenType `json:"type"`
	ClientType  ClientType    `json:"client"`
}

// UserTokenConfig holds configuration for user token generation.
type UserTokenConfig struct {
	// SigningKey is the secret key used for HS256 signing.
	// In production, use RS256 with a proper key pair.
	SigningKey []byte
	// AccessTokenDuration is how long access tokens remain valid.
	AccessTokenDuration time.Duration
	// CLIAccessTokenDuration is how long CLI access tokens remain valid.
	CLIAccessTokenDuration time.Duration
	// RefreshTokenDuration is how long refresh tokens remain valid.
	RefreshTokenDuration time.Duration
}

// UserTokenService handles user token generation and validation.
type UserTokenService struct {
	config UserTokenConfig
	signer jose.Signer
}

// NewUserTokenService creates a new user token service.
// If signingKey is empty, a random key is generated (suitable for development).
func NewUserTokenService(config UserTokenConfig) (*UserTokenService, error) {
	if len(config.SigningKey) == 0 {
		// Generate a random key for development/testing
		config.SigningKey = make([]byte, 32)
		if _, err := rand.Read(config.SigningKey); err != nil {
			return nil, fmt.Errorf("failed to generate signing key: %w", err)
		}
	}

	if config.AccessTokenDuration == 0 {
		config.AccessTokenDuration = DefaultAccessTokenDuration
	}
	if config.CLIAccessTokenDuration == 0 {
		config.CLIAccessTokenDuration = DefaultCLIAccessTokenDuration
	}
	if config.RefreshTokenDuration == 0 {
		config.RefreshTokenDuration = DefaultRefreshTokenDuration
	}

	// Create signer using HS256 (symmetric)
	// In production, consider RS256 (asymmetric) for better security
	signer, err := jose.NewSigner(
		jose.SigningKey{Algorithm: jose.HS256, Key: config.SigningKey},
		(&jose.SignerOptions{}).WithType("JWT"),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create signer: %w", err)
	}

	return &UserTokenService{
		config: config,
		signer: signer,
	}, nil
}

// GenerateTokenPair generates an access token and refresh token for a user.
func (s *UserTokenService) GenerateTokenPair(userID, email, displayName, role string, clientType ClientType) (accessToken, refreshToken string, expiresIn int64, err error) {
	// Determine access token duration based on client type
	var accessDuration time.Duration
	var tokenType UserTokenType
	if clientType == ClientTypeCLI {
		accessDuration = s.config.CLIAccessTokenDuration
		tokenType = TokenTypeCLI
	} else {
		accessDuration = s.config.AccessTokenDuration
		tokenType = TokenTypeAccess
	}

	// Generate access token
	accessToken, err = s.generateToken(userID, email, displayName, role, tokenType, clientType, accessDuration)
	if err != nil {
		return "", "", 0, fmt.Errorf("failed to generate access token: %w", err)
	}

	// Generate refresh token
	refreshToken, err = s.generateToken(userID, email, displayName, role, TokenTypeRefresh, clientType, s.config.RefreshTokenDuration)
	if err != nil {
		return "", "", 0, fmt.Errorf("failed to generate refresh token: %w", err)
	}

	expiresIn = int64(accessDuration.Seconds())
	return accessToken, refreshToken, expiresIn, nil
}

// GenerateAccessToken generates just an access token for a user.
func (s *UserTokenService) GenerateAccessToken(userID, email, displayName, role string, clientType ClientType) (string, int64, error) {
	var duration time.Duration
	var tokenType UserTokenType
	if clientType == ClientTypeCLI {
		duration = s.config.CLIAccessTokenDuration
		tokenType = TokenTypeCLI
	} else {
		duration = s.config.AccessTokenDuration
		tokenType = TokenTypeAccess
	}

	token, err := s.generateToken(userID, email, displayName, role, tokenType, clientType, duration)
	if err != nil {
		return "", 0, err
	}
	return token, int64(duration.Seconds()), nil
}

// generateToken creates a signed JWT token.
func (s *UserTokenService) generateToken(userID, email, displayName, role string, tokenType UserTokenType, clientType ClientType, duration time.Duration) (string, error) {
	now := time.Now()

	claims := UserTokenClaims{
		Claims: jwt.Claims{
			Issuer:    UserTokenIssuer,
			Subject:   userID,
			Audience:  jwt.Audience{UserTokenAudience},
			IssuedAt:  jwt.NewNumericDate(now),
			Expiry:    jwt.NewNumericDate(now.Add(duration)),
			NotBefore: jwt.NewNumericDate(now),
			ID:        generateUserTokenID(),
		},
		UserID:      userID,
		Email:       email,
		DisplayName: displayName,
		Role:        role,
		TokenType:   tokenType,
		ClientType:  clientType,
	}

	token, err := jwt.Signed(s.signer).Claims(claims).Serialize()
	if err != nil {
		return "", fmt.Errorf("failed to sign token: %w", err)
	}

	return token, nil
}

// ValidateUserToken validates a JWT and returns the claims if valid.
func (s *UserTokenService) ValidateUserToken(tokenString string) (*UserTokenClaims, error) {
	token, err := jwt.ParseSigned(tokenString, []jose.SignatureAlgorithm{jose.HS256})
	if err != nil {
		return nil, fmt.Errorf("failed to parse token: %w", err)
	}

	var claims UserTokenClaims
	if err := token.Claims(s.config.SigningKey, &claims); err != nil {
		return nil, fmt.Errorf("failed to verify token: %w", err)
	}

	// Validate standard claims
	expected := jwt.Expected{
		Issuer:      UserTokenIssuer,
		AnyAudience: jwt.Audience{UserTokenAudience},
		Time:        time.Now(),
	}

	if err := claims.Claims.Validate(expected); err != nil {
		return nil, fmt.Errorf("token validation failed: %w", err)
	}

	return &claims, nil
}

// ValidateRefreshToken validates a refresh token and returns the claims.
func (s *UserTokenService) ValidateRefreshToken(tokenString string) (*UserTokenClaims, error) {
	claims, err := s.ValidateUserToken(tokenString)
	if err != nil {
		return nil, err
	}

	if claims.TokenType != TokenTypeRefresh {
		return nil, fmt.Errorf("not a refresh token")
	}

	return claims, nil
}

// RefreshTokens uses a refresh token to generate a new token pair.
func (s *UserTokenService) RefreshTokens(refreshToken string) (accessToken, newRefreshToken string, expiresIn int64, err error) {
	claims, err := s.ValidateRefreshToken(refreshToken)
	if err != nil {
		return "", "", 0, fmt.Errorf("invalid refresh token: %w", err)
	}

	// Generate new token pair
	return s.GenerateTokenPair(claims.UserID, claims.Email, claims.DisplayName, claims.Role, claims.ClientType)
}

// GetTokenExpiry extracts the expiry time from a token without full validation.
func (s *UserTokenService) GetTokenExpiry(tokenString string) (time.Time, error) {
	token, err := jwt.ParseSigned(tokenString, []jose.SignatureAlgorithm{jose.HS256})
	if err != nil {
		return time.Time{}, fmt.Errorf("failed to parse token: %w", err)
	}

	var claims UserTokenClaims
	if err := token.Claims(s.config.SigningKey, &claims); err != nil {
		return time.Time{}, fmt.Errorf("failed to verify token: %w", err)
	}

	if claims.Expiry == nil {
		return time.Time{}, fmt.Errorf("token has no expiry")
	}

	return claims.Expiry.Time(), nil
}

// generateUserTokenID generates a unique token ID.
func generateUserTokenID() string {
	b := make([]byte, 16)
	rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}
