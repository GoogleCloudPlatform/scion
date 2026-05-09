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

	// Clear SCION_HOST_UID so the agent-container check doesn't interfere
	t.Setenv("SCION_HOST_UID", "")

	// Bypass the git and workspace checks for this specific test
	origGlobal := globalMode
	globalMode = true
	defer func() { globalMode = origGlobal }()

	// Build a fake interactive command for testing rejection
	fakeAttachCmd := &cobra.Command{Use: "attach"}
	fakeAttachCmd.SetArgs([]string{})
	rootCmd.AddCommand(fakeAttachCmd)
	defer rootCmd.RemoveCommand(fakeAttachCmd)

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
			name:        "Json format, non-interactive command",
			cmd:         &cobra.Command{Use: "other"},
			format:      "json",
			expectError: false,
		},
		{
			name:        "Json format, version command",
			cmd:         versionCmd,
			format:      "json",
			expectError: false,
		},
		{
			name:          "Json format, interactive command (attach)",
			cmd:           fakeAttachCmd,
			format:        "json",
			expectError:   true,
			errorContains: "--format json is not supported for 'scion attach'",
		},
	}

	// Test that look command clears outputFormat (json no-op)
	t.Run("Json format, look command (no-op)", func(t *testing.T) {
		outputFormat = "json"
		err := rootCmd.PersistentPreRunE(lookCmd, []string{})
		if err != nil {
			assert.NotContains(t, err.Error(), "format")
		}
		assert.Empty(t, outputFormat, "outputFormat should be cleared for look command")
	})

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

func TestHubAuthLoginDoesNotRequireImageRegistry(t *testing.T) {
	origGlobalMode := globalMode
	origGrovePath := grovePath
	origProfile := profile
	origOutputFormat := outputFormat
	origNoHub := noHub
	origHubEndpoint := hubEndpoint
	origNonInteractive := nonInteractive
	origAutoConfirm := autoConfirm
	defer func() {
		globalMode = origGlobalMode
		grovePath = origGrovePath
		profile = origProfile
		outputFormat = origOutputFormat
		noHub = origNoHub
		hubEndpoint = origHubEndpoint
		nonInteractive = origNonInteractive
		autoConfirm = origAutoConfirm
	}()

	t.Setenv("SCION_HOST_UID", "")

	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	if err := os.MkdirAll(filepath.Join(tmpHome, ".scion"), 0755); err != nil {
		t.Fatalf("failed to create test global scion dir: %v", err)
	}

	globalMode = true
	grovePath = ""
	profile = ""
	outputFormat = ""
	noHub = false
	hubEndpoint = "http://127.0.0.1:8080"
	nonInteractive = true
	autoConfirm = true

	err := rootCmd.PersistentPreRunE(hubAuthLoginCmd, []string{})
	assert.NoError(t, err)
}

func TestServerStartDoesNotRequireImageRegistry(t *testing.T) {
	origGlobalMode := globalMode
	origGrovePath := grovePath
	origProfile := profile
	origOutputFormat := outputFormat
	origNoHub := noHub
	origNonInteractive := nonInteractive
	origAutoConfirm := autoConfirm
	defer func() {
		globalMode = origGlobalMode
		grovePath = origGrovePath
		profile = origProfile
		outputFormat = origOutputFormat
		noHub = origNoHub
		nonInteractive = origNonInteractive
		autoConfirm = origAutoConfirm
	}()

	t.Setenv("SCION_HOST_UID", "")

	// Create a temp home with a global .scion dir but NO image_registry
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	if err := os.MkdirAll(filepath.Join(tmpHome, ".scion"), 0755); err != nil {
		t.Fatalf("failed to create test global scion dir: %v", err)
	}

	globalMode = true
	grovePath = ""
	profile = ""
	outputFormat = ""
	noHub = true
	nonInteractive = true
	autoConfirm = true

	// serverStartCmd is a subcommand of serverCmd — its Name() is "start",
	// not "server". The PersistentPreRunE should still skip the image_registry
	// check because it's in the server subtree.
	err := rootCmd.PersistentPreRunE(serverStartCmd, []string{})
	assert.NoError(t, err, "server start should not require image_registry")
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
			// Clear v1 settings env var to prevent it from leaking dev auth
			origServerAuthToken := os.Getenv("SCION_AUTH_TOKEN")
			os.Unsetenv("SCION_AUTH_TOKEN")
			defer os.Setenv("SCION_AUTH_TOKEN", origServerAuthToken)

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

func TestNonInteractiveImpliesAutoConfirm(t *testing.T) {
	// Backup original values
	origAutoConfirm := autoConfirm
	origNonInteractive := nonInteractive
	origFormat := outputFormat
	defer func() {
		autoConfirm = origAutoConfirm
		nonInteractive = origNonInteractive
		outputFormat = origFormat
	}()

	// Ensure agent-mode auto-enable doesn't interfere with flag-level tests
	t.Setenv("SCION_CLI_MODE", "human")

	t.Run("nonInteractive sets autoConfirm true", func(t *testing.T) {
		autoConfirm = false
		nonInteractive = true
		outputFormat = ""

		// Run PersistentPreRunE - it should set autoConfirm = true
		_ = rootCmd.PersistentPreRunE(&cobra.Command{Use: "scion"}, []string{})

		assert.True(t, autoConfirm, "autoConfirm should be true when nonInteractive is set")
		assert.True(t, IsAutoConfirm(), "IsAutoConfirm() should return true")
		assert.True(t, IsNonInteractive(), "IsNonInteractive() should return true")
	})

	t.Run("autoConfirm without nonInteractive", func(t *testing.T) {
		autoConfirm = true
		nonInteractive = false
		outputFormat = ""

		_ = rootCmd.PersistentPreRunE(&cobra.Command{Use: "scion"}, []string{})

		assert.True(t, autoConfirm, "autoConfirm should remain true")
		assert.True(t, IsAutoConfirm(), "IsAutoConfirm() should return true")
		assert.False(t, IsNonInteractive(), "IsNonInteractive() should return false")
	})

	t.Run("neither flag set", func(t *testing.T) {
		autoConfirm = false
		nonInteractive = false
		outputFormat = ""

		_ = rootCmd.PersistentPreRunE(&cobra.Command{Use: "scion"}, []string{})

		assert.False(t, autoConfirm, "autoConfirm should remain false")
		assert.False(t, IsAutoConfirm(), "IsAutoConfirm() should return false")
		assert.False(t, IsNonInteractive(), "IsNonInteractive() should return false")
	})
}

func TestAgentModeImpliesNonInteractive(t *testing.T) {
	origAutoConfirm := autoConfirm
	origNonInteractive := nonInteractive
	origFormat := outputFormat
	defer func() {
		autoConfirm = origAutoConfirm
		nonInteractive = origNonInteractive
		outputFormat = origFormat
	}()

	t.Run("agent mode auto-enables non-interactive", func(t *testing.T) {
		t.Setenv("SCION_CLI_MODE", "agent")
		autoConfirm = false
		nonInteractive = false
		outputFormat = ""

		_ = rootCmd.PersistentPreRunE(&cobra.Command{Use: "scion"}, []string{})

		assert.True(t, nonInteractive, "nonInteractive should be auto-enabled in agent mode")
		assert.True(t, autoConfirm, "autoConfirm should be implied by nonInteractive")
	})

	t.Run("assistant mode does not auto-enable non-interactive", func(t *testing.T) {
		t.Setenv("SCION_CLI_MODE", "assistant")
		autoConfirm = false
		nonInteractive = false
		outputFormat = ""

		_ = rootCmd.PersistentPreRunE(&cobra.Command{Use: "scion"}, []string{})

		assert.False(t, nonInteractive, "nonInteractive should not be auto-enabled in assistant mode")
		assert.False(t, autoConfirm, "autoConfirm should remain false in assistant mode")
	})

	t.Run("skipped when already non-interactive", func(t *testing.T) {
		t.Setenv("SCION_CLI_MODE", "agent")
		autoConfirm = false
		nonInteractive = true
		outputFormat = ""

		_ = rootCmd.PersistentPreRunE(&cobra.Command{Use: "scion"}, []string{})

		assert.True(t, nonInteractive, "nonInteractive should remain true")
		assert.True(t, autoConfirm, "autoConfirm should be set by the flag-level check")
	})
}

func TestNonInteractiveFlagRegistered(t *testing.T) {
	// Verify the --non-interactive flag exists on the root command
	flag := rootCmd.PersistentFlags().Lookup("non-interactive")
	assert.NotNil(t, flag, "--non-interactive flag should be registered")
	assert.Equal(t, "false", flag.DefValue, "default value should be false")

	// Verify --yes flag still exists
	yesFlag := rootCmd.PersistentFlags().Lookup("yes")
	assert.NotNil(t, yesFlag, "--yes flag should be registered")
}

func TestHarnessConfigAliasRegistered(t *testing.T) {
	// Verify --harness-config and --harness flags exist on startCmd
	hcFlag := startCmd.Flags().Lookup("harness-config")
	assert.NotNil(t, hcFlag, "--harness-config flag should be registered on start")

	hFlag := startCmd.Flags().Lookup("harness")
	assert.NotNil(t, hFlag, "--harness flag should be registered on start")

	// Verify --harness-config and --harness flags exist on createCmd
	hcFlag = createCmd.Flags().Lookup("harness-config")
	assert.NotNil(t, hcFlag, "--harness-config flag should be registered on create")

	hFlag = createCmd.Flags().Lookup("harness")
	assert.NotNil(t, hFlag, "--harness flag should be registered on create")
}

func TestTelemetryFlagsRegistered(t *testing.T) {
	// Verify --enable-telemetry and --disable-telemetry flags exist on startCmd
	etFlag := startCmd.Flags().Lookup("enable-telemetry")
	assert.NotNil(t, etFlag, "--enable-telemetry flag should be registered on start")

	dtFlag := startCmd.Flags().Lookup("disable-telemetry")
	assert.NotNil(t, dtFlag, "--disable-telemetry flag should be registered on start")

	// Verify flags exist on resumeCmd
	etFlag = resumeCmd.Flags().Lookup("enable-telemetry")
	assert.NotNil(t, etFlag, "--enable-telemetry flag should be registered on resume")

	dtFlag = resumeCmd.Flags().Lookup("disable-telemetry")
	assert.NotNil(t, dtFlag, "--disable-telemetry flag should be registered on resume")
}

func TestTelemetryFlagsMutualExclusion(t *testing.T) {
	// Save and restore flag state
	origEnable := enableTelemetry
	origDisable := disableTelemetry
	defer func() {
		enableTelemetry = origEnable
		disableTelemetry = origDisable
	}()

	enableTelemetry = true
	disableTelemetry = true

	err := RunAgent(&cobra.Command{Use: "start"}, []string{"test-agent"}, false)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "--enable-telemetry and --disable-telemetry are mutually exclusive")
}

func TestCheckAgentContainerContext(t *testing.T) {
	// Save and restore original flag state
	origHubEndpoint := hubEndpoint
	defer func() { hubEndpoint = origHubEndpoint }()

	tests := []struct {
		name        string
		hostUID     string
		hubEndpoint string // flag value
		hubEnv      string // SCION_HUB_ENDPOINT env var
		networkMode string // SCION_NETWORK_MODE env var
		cmdName     string
		expectError bool
		errContains string
	}{
		{
			name:        "not in container — no error",
			hostUID:     "",
			cmdName:     "list",
			expectError: false,
		},
		{
			name:        "in container, no hub endpoint — error",
			hostUID:     "1000",
			cmdName:     "list",
			expectError: true,
			errContains: "no Hub endpoint is configured",
		},
		{
			name:        "in container, localhost hub — error",
			hostUID:     "1000",
			hubEnv:      "http://localhost:9810",
			cmdName:     "list",
			expectError: true,
			errContains: "points to localhost",
		},
		{
			name:        "in container, 127.0.0.1 hub — error",
			hostUID:     "1000",
			hubEnv:      "http://127.0.0.1:9810",
			cmdName:     "start",
			expectError: true,
			errContains: "points to localhost",
		},
		{
			name:        "in container, localhost hub with host networking — no error",
			hostUID:     "1000",
			hubEnv:      "http://localhost:8080",
			networkMode: "host",
			cmdName:     "list",
			expectError: false,
		},
		{
			name:        "in container, remote hub — no error",
			hostUID:     "1000",
			hubEnv:      "https://hub.scion.dev",
			cmdName:     "list",
			expectError: false,
		},
		{
			name:        "in container, remote hub via flag — no error",
			hostUID:     "1000",
			hubEndpoint: "https://hub.scion.dev",
			cmdName:     "list",
			expectError: false,
		},
		{
			name:        "in container, version command exempt",
			hostUID:     "1000",
			cmdName:     "version",
			expectError: false,
		},
		{
			name:        "in container, help command exempt",
			hostUID:     "1000",
			cmdName:     "help",
			expectError: false,
		},
		{
			name:        "in container, doctor command exempt",
			hostUID:     "1000",
			cmdName:     "doctor",
			expectError: false,
		},
		{
			name:        "in container, config command exempt",
			hostUID:     "1000",
			cmdName:     "config",
			expectError: false,
		},
		{
			name:        "in container, root command exempt",
			hostUID:     "1000",
			cmdName:     "scion",
			expectError: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Set env vars
			if tt.hostUID != "" {
				t.Setenv("SCION_HOST_UID", tt.hostUID)
			} else {
				t.Setenv("SCION_HOST_UID", "")
				os.Unsetenv("SCION_HOST_UID")
			}
			if tt.hubEnv != "" {
				t.Setenv("SCION_HUB_ENDPOINT", tt.hubEnv)
			} else {
				t.Setenv("SCION_HUB_ENDPOINT", "")
				os.Unsetenv("SCION_HUB_ENDPOINT")
			}
			t.Setenv("SCION_HUB_URL", "")
			os.Unsetenv("SCION_HUB_URL")
			if tt.networkMode != "" {
				t.Setenv("SCION_NETWORK_MODE", tt.networkMode)
			} else {
				t.Setenv("SCION_NETWORK_MODE", "")
				os.Unsetenv("SCION_NETWORK_MODE")
			}

			// Set flag
			hubEndpoint = tt.hubEndpoint

			cmd := &cobra.Command{Use: tt.cmdName}
			err := checkAgentContainerContext(cmd)

			if tt.expectError {
				assert.Error(t, err)
				if tt.errContains != "" {
					assert.Contains(t, err.Error(), tt.errContains)
				}
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

func TestCheckAgentContainerContextConfigSubcommand(t *testing.T) {
	t.Setenv("SCION_HOST_UID", "1000")
	t.Setenv("SCION_HUB_ENDPOINT", "")
	os.Unsetenv("SCION_HUB_ENDPOINT")
	t.Setenv("SCION_HUB_URL", "")
	os.Unsetenv("SCION_HUB_URL")

	origHubEndpoint := hubEndpoint
	hubEndpoint = ""
	defer func() { hubEndpoint = origHubEndpoint }()

	// config subcommand (e.g., "config set") should be exempt
	parentCmd := &cobra.Command{Use: "config"}
	childCmd := &cobra.Command{Use: "set"}
	parentCmd.AddCommand(childCmd)

	err := checkAgentContainerContext(childCmd)
	assert.NoError(t, err)
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
