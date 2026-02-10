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

// Package credentials manages CLI authentication credentials for Scion Hub.
package credentials

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/ptone/scion-agent/pkg/config"
)

const (
	// CredentialsFile is the name of the credentials file.
	CredentialsFile = "credentials.json"
	// FileMode is the file permissions for the credentials file (owner read/write only).
	FileMode = 0600
	// RefreshThreshold is how early before expiration to refresh tokens.
	RefreshThreshold = 5 * time.Minute
)

var (
	// ErrNotAuthenticated is returned when no credentials are found.
	ErrNotAuthenticated = errors.New("not authenticated")
	// ErrTokenExpired is returned when the token has expired and refresh failed.
	ErrTokenExpired = errors.New("token expired")
)

// Credentials holds all hub credentials.
type Credentials struct {
	Version int                        `json:"version"`
	Hubs    map[string]*HubCredentials `json:"hubs"`
}

// HubCredentials holds credentials for a specific hub.
type HubCredentials struct {
	AccessToken  string    `json:"accessToken"`
	RefreshToken string    `json:"refreshToken,omitempty"`
	ExpiresAt    time.Time `json:"expiresAt"`
	User         *User     `json:"user,omitempty"`
}

// User represents the authenticated user.
type User struct {
	ID          string `json:"id"`
	Email       string `json:"email"`
	DisplayName string `json:"displayName"`
	Role        string `json:"role,omitempty"`
}

// TokenResponse represents the response from token exchange or refresh.
type TokenResponse struct {
	AccessToken  string        `json:"accessToken"`
	RefreshToken string        `json:"refreshToken,omitempty"`
	ExpiresIn    time.Duration `json:"expiresIn"`
	User         *User         `json:"user,omitempty"`
}

// TokenRefresher is a function type for refreshing tokens.
type TokenRefresher func(refreshToken string) (*TokenResponse, error)

var (
	// mu protects file operations
	mu sync.Mutex
	// refresher can be set to enable automatic token refresh
	refresher TokenRefresher
)

// SetRefresher sets the function used to refresh tokens.
func SetRefresher(fn TokenRefresher) {
	mu.Lock()
	defer mu.Unlock()
	refresher = fn
}

// Store saves credentials for a hub.
func Store(hubURL string, token *TokenResponse) error {
	mu.Lock()
	defer mu.Unlock()

	path := credentialsPath()

	creds, _ := loadFile(path)
	if creds == nil {
		creds = &Credentials{Version: 1, Hubs: make(map[string]*HubCredentials)}
	}

	creds.Hubs[hubURL] = &HubCredentials{
		AccessToken:  token.AccessToken,
		RefreshToken: token.RefreshToken,
		ExpiresAt:    time.Now().Add(token.ExpiresIn),
		User:         token.User,
	}

	return saveFile(path, creds)
}

// Load retrieves credentials for a hub, refreshing if necessary.
func Load(hubURL string) (*HubCredentials, error) {
	mu.Lock()
	defer mu.Unlock()

	path := credentialsPath()
	creds, err := loadFile(path)
	if err != nil {
		return nil, err
	}

	hubCreds, ok := creds.Hubs[hubURL]
	if !ok {
		return nil, ErrNotAuthenticated
	}

	// Check if token needs refresh
	if time.Now().After(hubCreds.ExpiresAt.Add(-RefreshThreshold)) {
		if hubCreds.RefreshToken != "" && refresher != nil {
			newToken, err := refresher(hubCreds.RefreshToken)
			if err != nil {
				// If refresh fails but token is still valid, return it anyway
				if time.Now().Before(hubCreds.ExpiresAt) {
					return hubCreds, nil
				}
				return nil, ErrTokenExpired
			}

			// Update credentials with new tokens
			hubCreds.AccessToken = newToken.AccessToken
			if newToken.RefreshToken != "" {
				hubCreds.RefreshToken = newToken.RefreshToken
			}
			hubCreds.ExpiresAt = time.Now().Add(newToken.ExpiresIn)
			if newToken.User != nil {
				hubCreds.User = newToken.User
			}

			// Save updated credentials
			if err := saveFile(path, creds); err != nil {
				// Log but don't fail - we still have valid credentials in memory
				fmt.Fprintf(os.Stderr, "Warning: failed to save refreshed credentials: %v\n", err)
			}
		} else if time.Now().After(hubCreds.ExpiresAt) {
			return nil, ErrTokenExpired
		}
	}

	return hubCreds, nil
}

// Remove deletes credentials for a hub.
func Remove(hubURL string) error {
	mu.Lock()
	defer mu.Unlock()

	path := credentialsPath()
	creds, err := loadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil // Nothing to remove
		}
		return err
	}

	delete(creds.Hubs, hubURL)

	// If no more hubs, remove the file
	if len(creds.Hubs) == 0 {
		return os.Remove(path)
	}

	return saveFile(path, creds)
}

// GetAccessToken returns just the access token for a hub, or empty string if not authenticated.
func GetAccessToken(hubURL string) string {
	creds, err := Load(hubURL)
	if err != nil {
		return ""
	}
	return creds.AccessToken
}

// IsAuthenticated checks if credentials exist for a hub.
func IsAuthenticated(hubURL string) bool {
	creds, err := Load(hubURL)
	return err == nil && creds.AccessToken != ""
}

// credentialsPath is a function variable that returns the path to the credentials file.
// It can be overridden in tests.
var credentialsPath = func() string {
	scionDir, err := config.GetGlobalDir()
	if err != nil {
		// Fallback to home directory
		home, _ := os.UserHomeDir()
		scionDir = filepath.Join(home, ".scion")
	}
	return filepath.Join(scionDir, CredentialsFile)
}

// loadFile reads and parses the credentials file.
func loadFile(path string) (*Credentials, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, ErrNotAuthenticated
		}
		return nil, fmt.Errorf("failed to read credentials file: %w", err)
	}

	var creds Credentials
	if err := json.Unmarshal(data, &creds); err != nil {
		return nil, fmt.Errorf("failed to parse credentials file: %w", err)
	}

	if creds.Hubs == nil {
		creds.Hubs = make(map[string]*HubCredentials)
	}

	return &creds, nil
}

// saveFile writes credentials to the file with proper permissions.
func saveFile(path string, creds *Credentials) error {
	// Ensure directory exists
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return fmt.Errorf("failed to create credentials directory: %w", err)
	}

	data, err := json.MarshalIndent(creds, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal credentials: %w", err)
	}

	if err := os.WriteFile(path, data, FileMode); err != nil {
		return fmt.Errorf("failed to write credentials file: %w", err)
	}

	return nil
}
