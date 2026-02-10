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

package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestDefaultGlobalConfig(t *testing.T) {
	cfg := DefaultGlobalConfig()

	if cfg.Hub.Port != 9810 {
		t.Errorf("expected Hub port 9810, got %d", cfg.Hub.Port)
	}

	if cfg.Hub.Host != "0.0.0.0" {
		t.Errorf("expected Hub host '0.0.0.0', got %q", cfg.Hub.Host)
	}

	if cfg.Hub.ReadTimeout != 30*time.Second {
		t.Errorf("expected ReadTimeout 30s, got %v", cfg.Hub.ReadTimeout)
	}

	if cfg.Hub.WriteTimeout != 60*time.Second {
		t.Errorf("expected WriteTimeout 60s, got %v", cfg.Hub.WriteTimeout)
	}

	if !cfg.Hub.CORSEnabled {
		t.Error("expected CORS to be enabled by default")
	}

	if cfg.Database.Driver != "sqlite" {
		t.Errorf("expected database driver 'sqlite', got %q", cfg.Database.Driver)
	}

	if cfg.LogLevel != "info" {
		t.Errorf("expected log level 'info', got %q", cfg.LogLevel)
	}
}

func TestLoadGlobalConfigDefaults(t *testing.T) {
	// Load config without any config file
	cfg, err := LoadGlobalConfig("")
	if err != nil {
		t.Fatalf("failed to load config: %v", err)
	}

	// Should have default values
	if cfg.Hub.Port != 9810 {
		t.Errorf("expected Hub port 9810, got %d", cfg.Hub.Port)
	}

	if cfg.Database.Driver != "sqlite" {
		t.Errorf("expected database driver 'sqlite', got %q", cfg.Database.Driver)
	}

	// Database URL should be set to default path
	if cfg.Database.URL == "" {
		t.Error("expected database URL to be set")
	}
}

func TestLoadGlobalConfigFromFile(t *testing.T) {
	// Create a temporary config file
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "server.yaml")

	configContent := `
hub:
  port: 8080
  host: "127.0.0.1"
  corsEnabled: false

database:
  driver: postgres
  url: "postgres://localhost:5432/scion"

logLevel: debug
logFormat: json
`
	if err := os.WriteFile(configPath, []byte(configContent), 0644); err != nil {
		t.Fatalf("failed to write config file: %v", err)
	}

	cfg, err := LoadGlobalConfig(configPath)
	if err != nil {
		t.Fatalf("failed to load config: %v", err)
	}

	if cfg.Hub.Port != 8080 {
		t.Errorf("expected Hub port 8080, got %d", cfg.Hub.Port)
	}

	if cfg.Hub.Host != "127.0.0.1" {
		t.Errorf("expected Hub host '127.0.0.1', got %q", cfg.Hub.Host)
	}

	if cfg.Hub.CORSEnabled {
		t.Error("expected CORS to be disabled")
	}

	if cfg.Database.Driver != "postgres" {
		t.Errorf("expected database driver 'postgres', got %q", cfg.Database.Driver)
	}

	if cfg.Database.URL != "postgres://localhost:5432/scion" {
		t.Errorf("expected database URL 'postgres://localhost:5432/scion', got %q", cfg.Database.URL)
	}

	if cfg.LogLevel != "debug" {
		t.Errorf("expected log level 'debug', got %q", cfg.LogLevel)
	}

	if cfg.LogFormat != "json" {
		t.Errorf("expected log format 'json', got %q", cfg.LogFormat)
	}
}

func TestLoadGlobalConfigFromDirectory(t *testing.T) {
	// Create a temporary directory with config file
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "server.yaml")

	configContent := `
hub:
  port: 9999
`
	if err := os.WriteFile(configPath, []byte(configContent), 0644); err != nil {
		t.Fatalf("failed to write config file: %v", err)
	}

	// Load from directory (not file path)
	cfg, err := LoadGlobalConfig(tmpDir)
	if err != nil {
		t.Fatalf("failed to load config: %v", err)
	}

	if cfg.Hub.Port != 9999 {
		t.Errorf("expected Hub port 9999, got %d", cfg.Hub.Port)
	}
}

func TestLoadGlobalConfigEnvOverride(t *testing.T) {
	// Set environment variables
	// Note: Env vars use underscores which map to dots for nesting
	os.Setenv("SCION_SERVER_HUB_PORT", "7777")
	os.Setenv("SCION_SERVER_DATABASE_DRIVER", "postgres")
	defer func() {
		os.Unsetenv("SCION_SERVER_HUB_PORT")
		os.Unsetenv("SCION_SERVER_DATABASE_DRIVER")
	}()

	cfg, err := LoadGlobalConfig("")
	if err != nil {
		t.Fatalf("failed to load config: %v", err)
	}

	if cfg.Hub.Port != 7777 {
		t.Errorf("expected Hub port 7777 from env, got %d", cfg.Hub.Port)
	}

	if cfg.Database.Driver != "postgres" {
		t.Errorf("expected database driver 'postgres' from env, got %q", cfg.Database.Driver)
	}
}

func TestLoadGlobalConfigAdminEmailsEnvOverride(t *testing.T) {
	// Test standard SCION_SERVER_HUB_ADMINEMAILS
	os.Setenv("SCION_SERVER_HUB_ADMINEMAILS", "admin1@example.com,admin2@example.com")
	defer os.Unsetenv("SCION_SERVER_HUB_ADMINEMAILS")

	cfg, err := LoadGlobalConfig("")
	if err != nil {
		t.Fatalf("failed to load config: %v", err)
	}

	expected := []string{"admin1@example.com", "admin2@example.com"}
	if len(cfg.Hub.AdminEmails) != len(expected) {
		t.Errorf("expected %d admin emails, got %d. Values: %v", len(expected), len(cfg.Hub.AdminEmails), cfg.Hub.AdminEmails)
	} else {
		for i, email := range cfg.Hub.AdminEmails {
			if email != expected[i] {
				t.Errorf("expected admin email %d to be %q, got %q", i, expected[i], email)
			}
		}
	}

	// Unset to test shorthand removal
	os.Unsetenv("SCION_SERVER_HUB_ADMINEMAILS")

	// Verify that the old SCION_ADMIN_EMAILS no longer works
	os.Setenv("SCION_ADMIN_EMAILS", "old@example.com")
	defer os.Unsetenv("SCION_ADMIN_EMAILS")

	cfg, err = LoadGlobalConfig("")
	if err != nil {
		t.Fatalf("failed to load config: %v", err)
	}

	for _, email := range cfg.Hub.AdminEmails {
		if email == "old@example.com" {
			t.Errorf("SCION_ADMIN_EMAILS should no longer be supported")
		}
	}
}

func TestLoadGlobalConfigAuthorizedDomainsEnvOverride(t *testing.T) {
	// Test standard SCION_SERVER_AUTH_AUTHORIZEDDOMAINS
	os.Setenv("SCION_SERVER_AUTH_AUTHORIZEDDOMAINS", "example.com,test.org")
	defer os.Unsetenv("SCION_SERVER_AUTH_AUTHORIZEDDOMAINS")

	cfg, err := LoadGlobalConfig("")
	if err != nil {
		t.Fatalf("failed to load config: %v", err)
	}

	expected := []string{"example.com", "test.org"}
	if len(cfg.Auth.AuthorizedDomains) != len(expected) {
		t.Errorf("expected %d domains, got %d. Values: %v", len(expected), len(cfg.Auth.AuthorizedDomains), cfg.Auth.AuthorizedDomains)
	} else {
		for i, domain := range cfg.Auth.AuthorizedDomains {
			if domain != expected[i] {
				t.Errorf("expected domain %d to be %q, got %q", i, expected[i], domain)
			}
		}
	}

	// Unset to test shorthand removal
	os.Unsetenv("SCION_SERVER_AUTH_AUTHORIZEDDOMAINS")

	// Verify that the old SCION_AUTHORIZED_DOMAINS no longer works
	os.Setenv("SCION_AUTHORIZED_DOMAINS", "old.com")
	defer os.Unsetenv("SCION_AUTHORIZED_DOMAINS")

	cfg, err = LoadGlobalConfig("")
	if err != nil {
		t.Fatalf("failed to load config: %v", err)
	}

	for _, domain := range cfg.Auth.AuthorizedDomains {
		if domain == "old.com" {
			t.Errorf("SCION_AUTHORIZED_DOMAINS should no longer be supported")
		}
	}
}

func TestEnvKeyToConfigKey(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"HUB_PORT", "hub.port"},
		{"DATABASE_DRIVER", "database.driver"},
		{"OAUTH_CLI_GOOGLE_CLIENTID", "oauth.cli.google.clientId"},
		{"OAUTH_CLI_GOOGLE_CLIENTSECRET", "oauth.cli.google.clientSecret"},
		{"OAUTH_WEB_GITHUB_CLIENTID", "oauth.web.github.clientId"},
		{"OAUTH_WEB_GITHUB_CLIENTSECRET", "oauth.web.github.clientSecret"},
		{"RUNTIMEBROKER_READTIMEOUT", "runtimebroker.readTimeout"},
		{"RUNTIMEBROKER_WRITETIMEOUT", "runtimebroker.writeTimeout"},
		{"RUNTIMEBROKER_BROKERID", "runtimebroker.brokerId"},
		{"RUNTIMEBROKER_BROKERNAME", "runtimebroker.brokerName"},
		{"AUTH_DEVMODE", "auth.devMode"},
		{"AUTH_DEVTOKEN", "auth.devToken"},
		{"LOGLEVEL", "logLevel"},
		{"LOGFORMAT", "logFormat"},
	}

	for _, tc := range tests {
		t.Run(tc.input, func(t *testing.T) {
			got := envKeyToConfigKey(tc.input)
			if got != tc.expected {
				t.Errorf("envKeyToConfigKey(%q) = %q, want %q", tc.input, got, tc.expected)
			}
		})
	}
}

func TestLoadGlobalConfigOAuthEnvOverride(t *testing.T) {
	// Set OAuth environment variables
	os.Setenv("SCION_SERVER_OAUTH_CLI_GOOGLE_CLIENTID", "test-cli-client-id")
	os.Setenv("SCION_SERVER_OAUTH_CLI_GOOGLE_CLIENTSECRET", "test-cli-secret")
	os.Setenv("SCION_SERVER_OAUTH_WEB_GITHUB_CLIENTID", "test-web-gh-id")
	os.Setenv("SCION_SERVER_OAUTH_WEB_GITHUB_CLIENTSECRET", "test-web-gh-secret")
	defer func() {
		os.Unsetenv("SCION_SERVER_OAUTH_CLI_GOOGLE_CLIENTID")
		os.Unsetenv("SCION_SERVER_OAUTH_CLI_GOOGLE_CLIENTSECRET")
		os.Unsetenv("SCION_SERVER_OAUTH_WEB_GITHUB_CLIENTID")
		os.Unsetenv("SCION_SERVER_OAUTH_WEB_GITHUB_CLIENTSECRET")
	}()

	cfg, err := LoadGlobalConfig("")
	if err != nil {
		t.Fatalf("failed to load config: %v", err)
	}

	if cfg.OAuth.CLI.Google.ClientID != "test-cli-client-id" {
		t.Errorf("expected CLI Google ClientID 'test-cli-client-id', got %q", cfg.OAuth.CLI.Google.ClientID)
	}

	if cfg.OAuth.CLI.Google.ClientSecret != "test-cli-secret" {
		t.Errorf("expected CLI Google ClientSecret 'test-cli-secret', got %q", cfg.OAuth.CLI.Google.ClientSecret)
	}

	if cfg.OAuth.Web.GitHub.ClientID != "test-web-gh-id" {
		t.Errorf("expected Web GitHub ClientID 'test-web-gh-id', got %q", cfg.OAuth.Web.GitHub.ClientID)
	}

	if cfg.OAuth.Web.GitHub.ClientSecret != "test-web-gh-secret" {
		t.Errorf("expected Web GitHub ClientSecret 'test-web-gh-secret', got %q", cfg.OAuth.Web.GitHub.ClientSecret)
	}
}

// TestHubEndpointConfiguration tests the Hub endpoint configuration from file and env.
// This verifies Fix 2 from progress-report.md: Hub config includes endpoint field.
func TestHubEndpointConfiguration(t *testing.T) {
	t.Run("default is empty", func(t *testing.T) {
		cfg := DefaultGlobalConfig()
		if cfg.Hub.Endpoint != "" {
			t.Errorf("expected Hub.Endpoint to be empty by default, got %q", cfg.Hub.Endpoint)
		}
	})

	t.Run("from config file", func(t *testing.T) {
		tmpDir := t.TempDir()
		configPath := filepath.Join(tmpDir, "server.yaml")

		configContent := `
hub:
  endpoint: "https://hub.example.com"
`
		if err := os.WriteFile(configPath, []byte(configContent), 0644); err != nil {
			t.Fatalf("failed to write config file: %v", err)
		}

		cfg, err := LoadGlobalConfig(configPath)
		if err != nil {
			t.Fatalf("failed to load config: %v", err)
		}

		if cfg.Hub.Endpoint != "https://hub.example.com" {
			t.Errorf("expected Hub.Endpoint 'https://hub.example.com', got %q", cfg.Hub.Endpoint)
		}
	})

	t.Run("from environment variable", func(t *testing.T) {
		os.Setenv("SCION_SERVER_HUB_ENDPOINT", "https://env-hub.example.com")
		defer os.Unsetenv("SCION_SERVER_HUB_ENDPOINT")

		cfg, err := LoadGlobalConfig("")
		if err != nil {
			t.Fatalf("failed to load config: %v", err)
		}

		if cfg.Hub.Endpoint != "https://env-hub.example.com" {
			t.Errorf("expected Hub.Endpoint 'https://env-hub.example.com', got %q", cfg.Hub.Endpoint)
		}
	})

	t.Run("env overrides config file", func(t *testing.T) {
		tmpDir := t.TempDir()
		configPath := filepath.Join(tmpDir, "server.yaml")

		configContent := `
hub:
  endpoint: "https://file-hub.example.com"
`
		if err := os.WriteFile(configPath, []byte(configContent), 0644); err != nil {
			t.Fatalf("failed to write config file: %v", err)
		}

		os.Setenv("SCION_SERVER_HUB_ENDPOINT", "https://env-hub.example.com")
		defer os.Unsetenv("SCION_SERVER_HUB_ENDPOINT")

		cfg, err := LoadGlobalConfig(configPath)
		if err != nil {
			t.Fatalf("failed to load config: %v", err)
		}

		if cfg.Hub.Endpoint != "https://env-hub.example.com" {
			t.Errorf("expected Hub.Endpoint 'https://env-hub.example.com' (env override), got %q", cfg.Hub.Endpoint)
		}
	})
}

// TestRuntimeBrokerHubEndpointConfiguration tests RuntimeBroker hubEndpoint config.
// This relates to Fix 4/6 in progress-report.md: RuntimeBroker hub endpoint configuration.
func TestRuntimeBrokerHubEndpointConfiguration(t *testing.T) {
	t.Run("from config file", func(t *testing.T) {
		tmpDir := t.TempDir()
		configPath := filepath.Join(tmpDir, "server.yaml")

		configContent := `
runtimeBroker:
  hubEndpoint: "https://rh-hub.example.com"
`
		if err := os.WriteFile(configPath, []byte(configContent), 0644); err != nil {
			t.Fatalf("failed to write config file: %v", err)
		}

		cfg, err := LoadGlobalConfig(configPath)
		if err != nil {
			t.Fatalf("failed to load config: %v", err)
		}

		if cfg.RuntimeBroker.HubEndpoint != "https://rh-hub.example.com" {
			t.Errorf("expected RuntimeBroker.HubEndpoint 'https://rh-hub.example.com', got %q", cfg.RuntimeBroker.HubEndpoint)
		}
	})

	t.Run("default is empty", func(t *testing.T) {
		cfg := DefaultGlobalConfig()
		if cfg.RuntimeBroker.HubEndpoint != "" {
			t.Errorf("expected RuntimeBroker.HubEndpoint to be empty by default, got %q", cfg.RuntimeBroker.HubEndpoint)
		}
	})

	// Note: Env var override for runtimeBroker.hubEndpoint doesn't work due to case sensitivity
	// in koanf. The env var SCION_SERVER_RUNTIMEBROKER_HUBENDPOINT maps to "runtimebroker.hubEndpoint"
	// but the config expects "runtimeBroker.hubEndpoint" (camelCase). This is a known limitation.
	// For RuntimeBroker hubEndpoint, use config file or the settings.yaml fallback (Fix 6).
}
