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

package apiclient

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const (
	// DevTokenPrefix is the prefix for development tokens.
	DevTokenPrefix = "scion_dev_"
	// DevTokenLength is the number of random bytes in a dev token (32 bytes = 64 hex chars).
	DevTokenLength = 32
)

// DevAuthConfig holds development authentication settings.
type DevAuthConfig struct {
	// Enabled indicates whether development authentication is enabled.
	Enabled bool `json:"devMode" yaml:"devMode" koanf:"devMode"`
	// Token is an explicitly configured development token.
	Token string `json:"devToken" yaml:"devToken" koanf:"devToken"`
	// TokenFile is the path to the token file (default: ~/.scion/dev-token).
	TokenFile string `json:"devTokenFile" yaml:"devTokenFile" koanf:"devTokenFile"`
}

// InitDevAuth initializes development authentication.
// Returns the token to use and any error encountered.
// If dev auth is not enabled, returns empty string and nil error.
func InitDevAuth(cfg DevAuthConfig, scionDir string) (string, error) {
	if !cfg.Enabled {
		return "", nil
	}

	// Priority 1: Explicit token in config
	if cfg.Token != "" {
		return cfg.Token, nil
	}

	// Determine token file path
	tokenFile := cfg.TokenFile
	if tokenFile == "" {
		tokenFile = filepath.Join(scionDir, "dev-token")
	}

	// Priority 2: Existing token file
	if data, err := os.ReadFile(tokenFile); err == nil {
		token := strings.TrimSpace(string(data))
		if token != "" {
			return token, nil
		}
	}

	// Priority 3: Generate new token
	token, err := GenerateDevToken()
	if err != nil {
		return "", fmt.Errorf("failed to generate dev token: %w", err)
	}

	// Ensure directory exists
	if err := os.MkdirAll(filepath.Dir(tokenFile), 0755); err != nil {
		return "", fmt.Errorf("failed to create token directory: %w", err)
	}

	// Persist token with secure permissions
	if err := os.WriteFile(tokenFile, []byte(token+"\n"), 0600); err != nil {
		return "", fmt.Errorf("failed to write dev token file: %w", err)
	}

	return token, nil
}

// GenerateDevToken creates a new cryptographically secure development token.
func GenerateDevToken() (string, error) {
	bytes := make([]byte, DevTokenLength)
	if _, err := rand.Read(bytes); err != nil {
		return "", err
	}
	return DevTokenPrefix + hex.EncodeToString(bytes), nil
}

// IsDevToken returns true if the token appears to be a development token.
func IsDevToken(token string) bool {
	return strings.HasPrefix(token, DevTokenPrefix)
}

// ValidateDevToken performs a constant-time comparison of the provided token
// against the expected token. Returns true if they match.
func ValidateDevToken(provided, expected string) bool {
	return subtle.ConstantTimeCompare([]byte(provided), []byte(expected)) == 1
}

// ResolveDevToken finds a development token from environment or file.
// It checks in order:
// 1. SCION_DEV_TOKEN environment variable
// 2. SCION_DEV_TOKEN_FILE environment variable (path to token file)
// 3. Default token file (~/.scion/dev-token)
func ResolveDevToken() string {
	token, _ := ResolveDevTokenWithSource()
	return token
}

// ResolveDevTokenWithSource finds a development token and returns both
// the token and the source it was loaded from.
// It checks in order:
// 1. SCION_DEV_TOKEN environment variable
// 2. SCION_DEV_TOKEN_FILE environment variable (path to token file)
// 3. Default token file (~/.scion/dev-token)
func ResolveDevTokenWithSource() (string, string) {
	// Priority 1: Environment variable
	if token := os.Getenv("SCION_DEV_TOKEN"); token != "" {
		return token, "SCION_DEV_TOKEN env var"
	}

	// Priority 2: Custom token file from env
	if tokenFile := os.Getenv("SCION_DEV_TOKEN_FILE"); tokenFile != "" {
		if data, err := os.ReadFile(tokenFile); err == nil {
			token := strings.TrimSpace(string(data))
			if token != "" {
				return token, "SCION_DEV_TOKEN_FILE: " + tokenFile
			}
		}
	}

	// Priority 3: Default token file
	home, err := os.UserHomeDir()
	if err != nil {
		return "", ""
	}

	tokenFile := filepath.Join(home, ".scion", "dev-token")
	if data, err := os.ReadFile(tokenFile); err == nil {
		token := strings.TrimSpace(string(data))
		if token != "" {
			return token, "~/.scion/dev-token"
		}
	}

	return "", ""
}
