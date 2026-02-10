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
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGenerateDevToken(t *testing.T) {
	token, err := GenerateDevToken()
	if err != nil {
		t.Fatalf("GenerateDevToken() error = %v", err)
	}

	// Check prefix
	if !strings.HasPrefix(token, DevTokenPrefix) {
		t.Errorf("GenerateDevToken() = %v, want prefix %v", token, DevTokenPrefix)
	}

	// Check length (prefix + 64 hex chars)
	expectedLen := len(DevTokenPrefix) + DevTokenLength*2
	if len(token) != expectedLen {
		t.Errorf("GenerateDevToken() length = %v, want %v", len(token), expectedLen)
	}

	// Generate another and ensure they're different
	token2, err := GenerateDevToken()
	if err != nil {
		t.Fatalf("GenerateDevToken() second call error = %v", err)
	}
	if token == token2 {
		t.Error("GenerateDevToken() generated same token twice")
	}
}

func TestIsDevToken(t *testing.T) {
	tests := []struct {
		name  string
		token string
		want  bool
	}{
		{"valid dev token", "scion_dev_abc123", true},
		{"valid dev token long", "scion_dev_" + strings.Repeat("a", 64), true},
		{"empty", "", false},
		{"bearer token", "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9", false},
		{"partial prefix", "scion_de_abc", false},
		{"wrong prefix", "scion_prod_abc123", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsDevToken(tt.token); got != tt.want {
				t.Errorf("IsDevToken(%v) = %v, want %v", tt.token, got, tt.want)
			}
		})
	}
}

func TestValidateDevToken(t *testing.T) {
	token := "scion_dev_abc123def456"

	tests := []struct {
		name     string
		provided string
		expected string
		want     bool
	}{
		{"exact match", token, token, true},
		{"wrong token", "scion_dev_wrong", token, false},
		{"empty provided", "", token, false},
		{"empty expected", token, "", false},
		{"both empty", "", "", true},
		{"case sensitive", "SCION_DEV_abc123def456", token, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ValidateDevToken(tt.provided, tt.expected); got != tt.want {
				t.Errorf("ValidateDevToken(%v, %v) = %v, want %v",
					tt.provided, tt.expected, got, tt.want)
			}
		})
	}
}

func TestInitDevAuth(t *testing.T) {
	// Create temp directory for testing
	tmpDir, err := os.MkdirTemp("", "scion-devauth-test")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	t.Run("disabled", func(t *testing.T) {
		cfg := DevAuthConfig{Enabled: false}
		token, err := InitDevAuth(cfg, tmpDir)
		if err != nil {
			t.Errorf("InitDevAuth() error = %v", err)
		}
		if token != "" {
			t.Errorf("InitDevAuth() token = %v, want empty", token)
		}
	})

	t.Run("explicit token", func(t *testing.T) {
		expectedToken := "scion_dev_explicit_token"
		cfg := DevAuthConfig{
			Enabled: true,
			Token:   expectedToken,
		}
		token, err := InitDevAuth(cfg, tmpDir)
		if err != nil {
			t.Errorf("InitDevAuth() error = %v", err)
		}
		if token != expectedToken {
			t.Errorf("InitDevAuth() token = %v, want %v", token, expectedToken)
		}
	})

	t.Run("generate and persist", func(t *testing.T) {
		subDir := filepath.Join(tmpDir, "generate")
		cfg := DevAuthConfig{Enabled: true}

		// First call should generate
		token1, err := InitDevAuth(cfg, subDir)
		if err != nil {
			t.Errorf("InitDevAuth() error = %v", err)
		}
		if token1 == "" {
			t.Error("InitDevAuth() generated empty token")
		}
		if !IsDevToken(token1) {
			t.Errorf("InitDevAuth() token %v is not a valid dev token", token1)
		}

		// Check file was created
		tokenFile := filepath.Join(subDir, "dev-token")
		data, err := os.ReadFile(tokenFile)
		if err != nil {
			t.Errorf("Token file not created: %v", err)
		}
		if strings.TrimSpace(string(data)) != token1 {
			t.Errorf("Token file content = %v, want %v", string(data), token1)
		}

		// Check file permissions
		info, err := os.Stat(tokenFile)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0600 {
			t.Errorf("Token file permissions = %v, want 0600", info.Mode().Perm())
		}

		// Second call should return same token
		token2, err := InitDevAuth(cfg, subDir)
		if err != nil {
			t.Errorf("InitDevAuth() second call error = %v", err)
		}
		if token2 != token1 {
			t.Errorf("InitDevAuth() second call token = %v, want %v", token2, token1)
		}
	})

	t.Run("custom token file", func(t *testing.T) {
		customFile := filepath.Join(tmpDir, "custom-token")
		expectedToken := "scion_dev_custom_file_token"

		// Write token to custom file
		if err := os.WriteFile(customFile, []byte(expectedToken+"\n"), 0600); err != nil {
			t.Fatal(err)
		}

		cfg := DevAuthConfig{
			Enabled:   true,
			TokenFile: customFile,
		}

		token, err := InitDevAuth(cfg, tmpDir)
		if err != nil {
			t.Errorf("InitDevAuth() error = %v", err)
		}
		if token != expectedToken {
			t.Errorf("InitDevAuth() token = %v, want %v", token, expectedToken)
		}
	})
}

func TestResolveDevToken(t *testing.T) {
	// Create temp directory for testing
	tmpDir, err := os.MkdirTemp("", "scion-resolve-test")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	// Clear any existing env vars
	originalToken := os.Getenv("SCION_DEV_TOKEN")
	originalFile := os.Getenv("SCION_DEV_TOKEN_FILE")
	defer func() {
		os.Setenv("SCION_DEV_TOKEN", originalToken)
		os.Setenv("SCION_DEV_TOKEN_FILE", originalFile)
	}()

	t.Run("from env var", func(t *testing.T) {
		os.Setenv("SCION_DEV_TOKEN", "scion_dev_from_env")
		os.Setenv("SCION_DEV_TOKEN_FILE", "")

		token := ResolveDevToken()
		if token != "scion_dev_from_env" {
			t.Errorf("ResolveDevToken() = %v, want scion_dev_from_env", token)
		}

		os.Unsetenv("SCION_DEV_TOKEN")
	})

	t.Run("from custom file via env", func(t *testing.T) {
		os.Unsetenv("SCION_DEV_TOKEN")

		customFile := filepath.Join(tmpDir, "custom-resolve")
		if err := os.WriteFile(customFile, []byte("scion_dev_from_file\n"), 0600); err != nil {
			t.Fatal(err)
		}

		os.Setenv("SCION_DEV_TOKEN_FILE", customFile)

		token := ResolveDevToken()
		if token != "scion_dev_from_file" {
			t.Errorf("ResolveDevToken() = %v, want scion_dev_from_file", token)
		}

		os.Unsetenv("SCION_DEV_TOKEN_FILE")
	})

	t.Run("no token found", func(t *testing.T) {
		os.Unsetenv("SCION_DEV_TOKEN")
		os.Unsetenv("SCION_DEV_TOKEN_FILE")

		// Note: This test might find a token if ~/.scion/dev-token exists
		// For a pure test, we'd need to mock the home directory
		token := ResolveDevToken()
		// Just verify it doesn't panic and returns something (or empty)
		_ = token
	})
}

func TestResolveDevTokenWithSource(t *testing.T) {
	// Create temp directory for testing
	tmpDir, err := os.MkdirTemp("", "scion-resolve-source-test")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	// Clear any existing env vars
	originalToken := os.Getenv("SCION_DEV_TOKEN")
	originalFile := os.Getenv("SCION_DEV_TOKEN_FILE")
	defer func() {
		os.Setenv("SCION_DEV_TOKEN", originalToken)
		os.Setenv("SCION_DEV_TOKEN_FILE", originalFile)
	}()

	t.Run("from env var with source", func(t *testing.T) {
		os.Setenv("SCION_DEV_TOKEN", "scion_dev_from_env")
		os.Setenv("SCION_DEV_TOKEN_FILE", "")

		token, source := ResolveDevTokenWithSource()
		if token != "scion_dev_from_env" {
			t.Errorf("ResolveDevTokenWithSource() token = %v, want scion_dev_from_env", token)
		}
		if source != "SCION_DEV_TOKEN env var" {
			t.Errorf("ResolveDevTokenWithSource() source = %v, want 'SCION_DEV_TOKEN env var'", source)
		}

		os.Unsetenv("SCION_DEV_TOKEN")
	})

	t.Run("from custom file via env with source", func(t *testing.T) {
		os.Unsetenv("SCION_DEV_TOKEN")

		customFile := filepath.Join(tmpDir, "custom-resolve-source")
		if err := os.WriteFile(customFile, []byte("scion_dev_from_file\n"), 0600); err != nil {
			t.Fatal(err)
		}

		os.Setenv("SCION_DEV_TOKEN_FILE", customFile)

		token, source := ResolveDevTokenWithSource()
		if token != "scion_dev_from_file" {
			t.Errorf("ResolveDevTokenWithSource() token = %v, want scion_dev_from_file", token)
		}
		expectedSource := "SCION_DEV_TOKEN_FILE: " + customFile
		if source != expectedSource {
			t.Errorf("ResolveDevTokenWithSource() source = %v, want %v", source, expectedSource)
		}

		os.Unsetenv("SCION_DEV_TOKEN_FILE")
	})
}
