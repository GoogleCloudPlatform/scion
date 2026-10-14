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
	"testing"
	"time"
)

func TestUserTokenService_GenerateAndValidate(t *testing.T) {
	cfg := UserTokenConfig{
		AccessTokenDuration:    15 * time.Minute,
		CLIAccessTokenDuration: 24 * time.Hour,
		RefreshTokenDuration:   7 * 24 * time.Hour,
	}
	svc, err := NewUserTokenService(cfg)
	if err != nil {
		t.Fatalf("failed to create service: %v", err)
	}

	// Generate token pair
	accessToken, refreshToken, expiresIn, err := svc.GenerateTokenPair(
		"user-123", "test@example.com", "Test User", "member", ClientTypeWeb,
	)
	if err != nil {
		t.Fatalf("failed to generate tokens: %v", err)
	}

	if accessToken == "" {
		t.Error("access token should not be empty")
	}
	if refreshToken == "" {
		t.Error("refresh token should not be empty")
	}
	if expiresIn <= 0 {
		t.Error("expiresIn should be positive")
	}

	// Validate access token
	claims, err := svc.ValidateUserToken(accessToken)
	if err != nil {
		t.Fatalf("failed to validate access token: %v", err)
	}

	if claims.UserID != "user-123" {
		t.Errorf("expected user ID 'user-123', got %q", claims.UserID)
	}
	if claims.Email != "test@example.com" {
		t.Errorf("expected email 'test@example.com', got %q", claims.Email)
	}
	if claims.Role != "member" {
		t.Errorf("expected role 'member', got %q", claims.Role)
	}
	if claims.TokenType != TokenTypeAccess {
		t.Errorf("expected token type 'access', got %q", claims.TokenType)
	}
	if claims.ClientType != ClientTypeWeb {
		t.Errorf("expected client type 'web', got %q", claims.ClientType)
	}

	// Validate refresh token
	refreshClaims, err := svc.ValidateRefreshToken(refreshToken)
	if err != nil {
		t.Fatalf("failed to validate refresh token: %v", err)
	}

	if refreshClaims.TokenType != TokenTypeRefresh {
		t.Errorf("expected token type 'refresh', got %q", refreshClaims.TokenType)
	}
}

func TestUserTokenService_CLITokenDuration(t *testing.T) {
	cfg := UserTokenConfig{
		AccessTokenDuration:    15 * time.Minute,
		CLIAccessTokenDuration: 30 * 24 * time.Hour, // 30 days
	}
	svc, err := NewUserTokenService(cfg)
	if err != nil {
		t.Fatalf("failed to create service: %v", err)
	}

	// Generate CLI token
	accessToken, _, expiresIn, err := svc.GenerateTokenPair(
		"user-123", "test@example.com", "Test User", "member", ClientTypeCLI,
	)
	if err != nil {
		t.Fatalf("failed to generate tokens: %v", err)
	}

	// CLI tokens should have 30 day expiry (30 * 24 * 3600 = 2592000 seconds)
	expectedExpiry := int64(30 * 24 * 3600)
	if expiresIn != expectedExpiry {
		t.Errorf("expected expiresIn %d, got %d", expectedExpiry, expiresIn)
	}

	// Validate the token type is CLI
	claims, err := svc.ValidateUserToken(accessToken)
	if err != nil {
		t.Fatalf("failed to validate token: %v", err)
	}

	if claims.TokenType != TokenTypeCLI {
		t.Errorf("expected token type 'cli', got %q", claims.TokenType)
	}
}

func TestUserTokenService_RefreshTokens(t *testing.T) {
	svc, err := NewUserTokenService(UserTokenConfig{})
	if err != nil {
		t.Fatalf("failed to create service: %v", err)
	}

	// Generate initial tokens
	_, refreshToken, _, err := svc.GenerateTokenPair(
		"user-123", "test@example.com", "Test User", "admin", ClientTypeWeb,
	)
	if err != nil {
		t.Fatalf("failed to generate tokens: %v", err)
	}

	// Refresh the tokens
	newAccessToken, newRefreshToken, expiresIn, err := svc.RefreshTokens(refreshToken)
	if err != nil {
		t.Fatalf("failed to refresh tokens: %v", err)
	}

	if newAccessToken == "" {
		t.Error("new access token should not be empty")
	}
	if newRefreshToken == "" {
		t.Error("new refresh token should not be empty")
	}
	if expiresIn <= 0 {
		t.Error("expiresIn should be positive")
	}

	// Validate the new access token
	claims, err := svc.ValidateUserToken(newAccessToken)
	if err != nil {
		t.Fatalf("failed to validate new access token: %v", err)
	}

	if claims.UserID != "user-123" {
		t.Errorf("expected user ID 'user-123', got %q", claims.UserID)
	}
	if claims.Role != "admin" {
		t.Errorf("expected role 'admin', got %q", claims.Role)
	}
}

func TestUserTokenService_ExpiredToken(t *testing.T) {
	cfg := UserTokenConfig{
		AccessTokenDuration: -1 * time.Hour, // Expired immediately
	}
	svc, err := NewUserTokenService(cfg)
	if err != nil {
		t.Fatalf("failed to create service: %v", err)
	}

	accessToken, _, _, err := svc.GenerateTokenPair(
		"user-123", "test@example.com", "Test User", "member", ClientTypeWeb,
	)
	if err != nil {
		t.Fatalf("failed to generate tokens: %v", err)
	}

	// Token should be invalid (expired)
	_, err = svc.ValidateUserToken(accessToken)
	if err == nil {
		t.Error("expected error for expired token, got nil")
	}
}

func TestUserTokenService_InvalidSignature(t *testing.T) {
	// Create two services with different keys
	svc1, err := NewUserTokenService(UserTokenConfig{})
	if err != nil {
		t.Fatalf("failed to create service 1: %v", err)
	}

	svc2, err := NewUserTokenService(UserTokenConfig{})
	if err != nil {
		t.Fatalf("failed to create service 2: %v", err)
	}

	// Generate token with service 1
	accessToken, _, _, err := svc1.GenerateTokenPair(
		"user-123", "test@example.com", "Test User", "member", ClientTypeWeb,
	)
	if err != nil {
		t.Fatalf("failed to generate tokens: %v", err)
	}

	// Try to validate with service 2 (different key)
	_, err = svc2.ValidateUserToken(accessToken)
	if err == nil {
		t.Error("expected error for token with invalid signature, got nil")
	}
}

func TestUserTokenService_RefreshWithAccessToken(t *testing.T) {
	svc, err := NewUserTokenService(UserTokenConfig{})
	if err != nil {
		t.Fatalf("failed to create service: %v", err)
	}

	// Generate tokens
	accessToken, _, _, err := svc.GenerateTokenPair(
		"user-123", "test@example.com", "Test User", "member", ClientTypeWeb,
	)
	if err != nil {
		t.Fatalf("failed to generate tokens: %v", err)
	}

	// Try to use access token as refresh token - should fail
	_, _, _, err = svc.RefreshTokens(accessToken)
	if err == nil {
		t.Error("expected error when using access token as refresh token, got nil")
	}
}
