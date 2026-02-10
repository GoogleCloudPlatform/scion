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

package credentials

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestStoreAndLoad(t *testing.T) {
	// Create a temporary directory for test credentials
	tmpDir, err := os.MkdirTemp("", "credentials-test")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	// Override the credentials path for testing
	origPath := credentialsPath
	credentialsPath = func() string {
		return filepath.Join(tmpDir, CredentialsFile)
	}
	defer func() { credentialsPath = origPath }()

	hubURL := "https://hub.example.com"
	token := &TokenResponse{
		AccessToken:  "test-access-token",
		RefreshToken: "test-refresh-token",
		ExpiresIn:    1 * time.Hour,
		User: &User{
			ID:          "user-123",
			Email:       "test@example.com",
			DisplayName: "Test User",
		},
	}

	// Store credentials
	if err := Store(hubURL, token); err != nil {
		t.Fatalf("Store failed: %v", err)
	}

	// Verify file was created with correct permissions
	info, err := os.Stat(filepath.Join(tmpDir, CredentialsFile))
	if err != nil {
		t.Fatalf("credentials file not found: %v", err)
	}
	if info.Mode().Perm() != FileMode {
		t.Errorf("wrong file permissions: got %o, want %o", info.Mode().Perm(), FileMode)
	}

	// Load credentials
	creds, err := Load(hubURL)
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}

	if creds.AccessToken != token.AccessToken {
		t.Errorf("AccessToken mismatch: got %q, want %q", creds.AccessToken, token.AccessToken)
	}
	if creds.RefreshToken != token.RefreshToken {
		t.Errorf("RefreshToken mismatch: got %q, want %q", creds.RefreshToken, token.RefreshToken)
	}
	if creds.User == nil {
		t.Fatal("User is nil")
	}
	if creds.User.Email != token.User.Email {
		t.Errorf("User.Email mismatch: got %q, want %q", creds.User.Email, token.User.Email)
	}
}

func TestLoadNotAuthenticated(t *testing.T) {
	// Create a temporary directory for test credentials
	tmpDir, err := os.MkdirTemp("", "credentials-test")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	// Override the credentials path for testing
	origPath := credentialsPath
	credentialsPath = func() string {
		return filepath.Join(tmpDir, CredentialsFile)
	}
	defer func() { credentialsPath = origPath }()

	// Try to load non-existent credentials
	_, err = Load("https://nonexistent.hub.com")
	if err != ErrNotAuthenticated {
		t.Errorf("expected ErrNotAuthenticated, got %v", err)
	}
}

func TestRemove(t *testing.T) {
	// Create a temporary directory for test credentials
	tmpDir, err := os.MkdirTemp("", "credentials-test")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	// Override the credentials path for testing
	origPath := credentialsPath
	credentialsPath = func() string {
		return filepath.Join(tmpDir, CredentialsFile)
	}
	defer func() { credentialsPath = origPath }()

	hubURL := "https://hub.example.com"
	token := &TokenResponse{
		AccessToken: "test-access-token",
		ExpiresIn:   1 * time.Hour,
	}

	// Store credentials
	if err := Store(hubURL, token); err != nil {
		t.Fatalf("Store failed: %v", err)
	}

	// Remove credentials
	if err := Remove(hubURL); err != nil {
		t.Fatalf("Remove failed: %v", err)
	}

	// Verify credentials are gone
	_, err = Load(hubURL)
	if err != ErrNotAuthenticated {
		t.Errorf("expected ErrNotAuthenticated after Remove, got %v", err)
	}
}

func TestMultipleHubs(t *testing.T) {
	// Create a temporary directory for test credentials
	tmpDir, err := os.MkdirTemp("", "credentials-test")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	// Override the credentials path for testing
	origPath := credentialsPath
	credentialsPath = func() string {
		return filepath.Join(tmpDir, CredentialsFile)
	}
	defer func() { credentialsPath = origPath }()

	hub1 := "https://hub1.example.com"
	hub2 := "https://hub2.example.com"

	token1 := &TokenResponse{
		AccessToken: "token-hub1",
		ExpiresIn:   1 * time.Hour,
	}
	token2 := &TokenResponse{
		AccessToken: "token-hub2",
		ExpiresIn:   1 * time.Hour,
	}

	// Store credentials for both hubs
	if err := Store(hub1, token1); err != nil {
		t.Fatalf("Store hub1 failed: %v", err)
	}
	if err := Store(hub2, token2); err != nil {
		t.Fatalf("Store hub2 failed: %v", err)
	}

	// Load and verify hub1
	creds1, err := Load(hub1)
	if err != nil {
		t.Fatalf("Load hub1 failed: %v", err)
	}
	if creds1.AccessToken != token1.AccessToken {
		t.Errorf("hub1 token mismatch: got %q, want %q", creds1.AccessToken, token1.AccessToken)
	}

	// Load and verify hub2
	creds2, err := Load(hub2)
	if err != nil {
		t.Fatalf("Load hub2 failed: %v", err)
	}
	if creds2.AccessToken != token2.AccessToken {
		t.Errorf("hub2 token mismatch: got %q, want %q", creds2.AccessToken, token2.AccessToken)
	}

	// Remove hub1, hub2 should still work
	if err := Remove(hub1); err != nil {
		t.Fatalf("Remove hub1 failed: %v", err)
	}

	creds2, err = Load(hub2)
	if err != nil {
		t.Fatalf("Load hub2 after removing hub1 failed: %v", err)
	}
	if creds2.AccessToken != token2.AccessToken {
		t.Errorf("hub2 token mismatch after removing hub1: got %q, want %q", creds2.AccessToken, token2.AccessToken)
	}
}

func TestIsAuthenticated(t *testing.T) {
	// Create a temporary directory for test credentials
	tmpDir, err := os.MkdirTemp("", "credentials-test")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	// Override the credentials path for testing
	origPath := credentialsPath
	credentialsPath = func() string {
		return filepath.Join(tmpDir, CredentialsFile)
	}
	defer func() { credentialsPath = origPath }()

	hubURL := "https://hub.example.com"

	// Not authenticated initially
	if IsAuthenticated(hubURL) {
		t.Error("IsAuthenticated returned true for unauthenticated hub")
	}

	// Store credentials
	token := &TokenResponse{
		AccessToken: "test-token",
		ExpiresIn:   1 * time.Hour,
	}
	if err := Store(hubURL, token); err != nil {
		t.Fatalf("Store failed: %v", err)
	}

	// Now authenticated
	if !IsAuthenticated(hubURL) {
		t.Error("IsAuthenticated returned false for authenticated hub")
	}
}

func TestGetAccessToken(t *testing.T) {
	// Create a temporary directory for test credentials
	tmpDir, err := os.MkdirTemp("", "credentials-test")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	// Override the credentials path for testing
	origPath := credentialsPath
	credentialsPath = func() string {
		return filepath.Join(tmpDir, CredentialsFile)
	}
	defer func() { credentialsPath = origPath }()

	hubURL := "https://hub.example.com"
	expectedToken := "my-access-token"

	// Empty when not authenticated
	if got := GetAccessToken(hubURL); got != "" {
		t.Errorf("GetAccessToken returned %q for unauthenticated hub", got)
	}

	// Store credentials
	token := &TokenResponse{
		AccessToken: expectedToken,
		ExpiresIn:   1 * time.Hour,
	}
	if err := Store(hubURL, token); err != nil {
		t.Fatalf("Store failed: %v", err)
	}

	// Returns token when authenticated
	if got := GetAccessToken(hubURL); got != expectedToken {
		t.Errorf("GetAccessToken returned %q, want %q", got, expectedToken)
	}
}

