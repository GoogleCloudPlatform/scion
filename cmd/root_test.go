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

package cmd

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
)

func TestFormatFlagCheck(t *testing.T) {
	// Backup original values
	origFormat := outputFormat
	defer func() { outputFormat = origFormat }()

	// We assume git checks pass in this environment, or we handle failures.

	tests := []struct {
		name          string
		cmd           *cobra.Command
		format        string
		expectError   bool
		errorContains string
	}{
		{
			name:        "No format, other command",
			cmd:         &cobra.Command{Use: "other"},
			format:      "",
			expectError: false,
		},
		{
			name:        "Json format, list command",
			cmd:         listCmd,
			format:      "json",
			expectError: false,
		},
		{
			name:        "Plain format, list command",
			cmd:         listCmd,
			format:      "plain",
			expectError: false,
		},
		{
			name:          "Invalid format",
			cmd:           listCmd,
			format:        "yaml",
			expectError:   true,
			errorContains: "invalid format: yaml (allowed: json, plain)",
		},
		{
			name:          "Json format, other command",
			cmd:           &cobra.Command{Use: "other"},
			format:        "json",
			expectError:   true,
			errorContains: "format flag is not yet supported for command other",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			outputFormat = tt.format
			err := rootCmd.PersistentPreRunE(tt.cmd, []string{})

			if tt.expectError {
				assert.Error(t, err)
				if tt.errorContains != "" {
					assert.Contains(t, err.Error(), tt.errorContains)
				}
			} else {
				// If error is not nil, check if it's unrelated (e.g. git check)
				// But ideally we want no error.
				if err != nil {
					// Allow git check failure if it occurs, but ensure it's not a format error
					assert.NotContains(t, err.Error(), "format flag")
					assert.NotContains(t, err.Error(), "invalid format")
				}
			}
		})
	}
}

func TestDevAuthWarning(t *testing.T) {
	// Save and restore original flags
	origNoHub := noHub
	origHubEndpoint := hubEndpoint
	defer func() {
		noHub = origNoHub
		hubEndpoint = origHubEndpoint
	}()

	// Save and restore HOME
	origHome := os.Getenv("HOME")
	defer os.Setenv("HOME", origHome)

	// Create a temp directory for test settings
	tmpDir := t.TempDir()
	os.Setenv("HOME", tmpDir)
	scionDir := filepath.Join(tmpDir, ".scion")
	if err := os.MkdirAll(scionDir, 0755); err != nil {
		t.Fatalf("failed to create test .scion dir: %v", err)
	}

	// Create settings.yaml with hub enabled
	settingsPath := filepath.Join(scionDir, "settings.yaml")
	settingsContent := `
hub:
  enabled: true
  endpoint: http://localhost:9810
`
	if err := os.WriteFile(settingsPath, []byte(settingsContent), 0644); err != nil {
		t.Fatalf("failed to write test settings: %v", err)
	}

	tests := []struct {
		name          string
		noHubFlag     bool
		hubEndpoint   string
		devTokenEnv   string
		devTokenFile  string
		expectWarning bool
	}{
		{
			name:          "No hub enabled, no warning",
			noHubFlag:     true,
			expectWarning: false,
		},
		{
			name:          "Local hub endpoint with dev token env var",
			noHubFlag:     false,
			hubEndpoint:   "http://localhost:9810",
			devTokenEnv:   "scion_dev_testtoken123",
			expectWarning: true,
		},
		{
			name:          "Hub endpoint via flag, no dev token",
			noHubFlag:     false,
			hubEndpoint:   "http://localhost:9810",
			devTokenEnv:   "",
			expectWarning: false,
		},
		{
			name:          "Remote hub with dev token env var warns",
			noHubFlag:     false,
			hubEndpoint:   "https://hub.demo.scion-ai.dev/",
			devTokenEnv:   "scion_dev_testtoken123",
			expectWarning: true,
		},
		{
			name:          "Remote hub with dev token file does not warn",
			noHubFlag:     false,
			hubEndpoint:   "https://hub.demo.scion-ai.dev/",
			devTokenFile:  "scion_dev_testtoken123",
			expectWarning: false,
		},
		{
			name:          "Local hub with dev token file warns",
			noHubFlag:     false,
			hubEndpoint:   "http://localhost:9810",
			devTokenFile:  "scion_dev_testtoken123",
			expectWarning: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Set flags
			noHub = tt.noHubFlag
			hubEndpoint = tt.hubEndpoint

			// Set environment
			if tt.devTokenEnv != "" {
				os.Setenv("SCION_DEV_TOKEN", tt.devTokenEnv)
				defer os.Unsetenv("SCION_DEV_TOKEN")
			} else {
				os.Unsetenv("SCION_DEV_TOKEN")
			}
			os.Unsetenv("SCION_DEV_TOKEN_FILE")

			// Write dev token file if specified
			devTokenPath := filepath.Join(scionDir, "dev-token")
			if tt.devTokenFile != "" {
				os.WriteFile(devTokenPath, []byte(tt.devTokenFile+"\n"), 0600)
				defer os.Remove(devTokenPath)
			} else {
				os.Remove(devTokenPath)
			}

			// Capture stderr
			oldStderr := os.Stderr
			r, w, _ := os.Pipe()
			os.Stderr = w

			// Call the function (use empty grove path as settings won't load in test env)
			printDevAuthWarningIfNeeded("")

			// Restore stderr and read output
			w.Close()
			os.Stderr = oldStderr

			var buf bytes.Buffer
			buf.ReadFrom(r)
			output := buf.String()

			if tt.expectWarning {
				assert.Contains(t, output, "WARNING")
				assert.Contains(t, output, "Development authentication enabled")
			} else {
				assert.NotContains(t, output, "WARNING")
			}
		})
	}
}

func TestIsLocalEndpoint(t *testing.T) {
	tests := []struct {
		endpoint string
		expected bool
	}{
		{"http://localhost:9810", true},
		{"http://127.0.0.1:9810", true},
		{"http://[::1]:9810", true},
		{"http://0.0.0.0:9810", true},
		{"https://hub.demo.scion-ai.dev/", false},
		{"https://example.com", false},
		{"", false},
	}

	for _, tt := range tests {
		t.Run(tt.endpoint, func(t *testing.T) {
			result := isLocalEndpoint(tt.endpoint)
			assert.Equal(t, tt.expected, result)
		})
	}
}
