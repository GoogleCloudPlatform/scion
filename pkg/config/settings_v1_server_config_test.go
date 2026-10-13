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

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

// --- Phase 4: V1ServerConfig tests ---

func TestV1ServerConfig_YAMLRoundTrip(t *testing.T) {
	v1 := &V1ServerConfig{
		Env:       "production",
		LogLevel:  "debug",
		LogFormat: "json",
		Hub: &V1ServerHubConfig{
			Port:         9810,
			Host:         "0.0.0.0",
			HubID:        "test-hub-id",
			PublicURL:    "https://hub.example.com",
			ReadTimeout:  "30s",
			WriteTimeout: "60s",
			AdminEmails:  []string{"admin@example.com"},
			CORS: &V1CORSConfig{
				Enabled:        true,
				AllowedOrigins: []string{"*"},
				AllowedMethods: []string{"GET", "POST"},
				AllowedHeaders: []string{"Authorization"},
				MaxAge:         3600,
			},
		},
		Broker: &V1BrokerConfig{
			Enabled:        true,
			Port:           9800,
			Host:           "0.0.0.0",
			BrokerID:       "broker-123",
			BrokerName:     "my-broker",
			BrokerNickname: "broker-nick",
			BrokerToken:    "token-xyz",
			HubEndpoint:    "https://hub.example.com",
		},
		Database: &V1DatabaseConfig{
			Driver: "sqlite",
			URL:    "/tmp/hub.db",
		},
		Auth: &V1AuthConfig{
			DevMode:           true,
			DevToken:          "dev-token",
			AuthorizedDomains: []string{"example.com"},
		},
		Storage: &V1StorageConfig{
			Provider:  "local",
			LocalPath: "/tmp/storage",
		},
		Secrets: &V1SecretsConfig{
			Backend: "local",
		},
	}

	data, err := yaml.Marshal(v1)
	require.NoError(t, err)

	var roundTripped V1ServerConfig
	err = yaml.Unmarshal(data, &roundTripped)
	require.NoError(t, err)

	assert.Equal(t, v1.Env, roundTripped.Env)
	assert.Equal(t, v1.LogLevel, roundTripped.LogLevel)
	assert.Equal(t, v1.Hub.Port, roundTripped.Hub.Port)
	assert.Equal(t, v1.Hub.PublicURL, roundTripped.Hub.PublicURL)
	assert.Equal(t, v1.Broker.BrokerID, roundTripped.Broker.BrokerID)
	assert.Equal(t, v1.Broker.BrokerNickname, roundTripped.Broker.BrokerNickname)
	assert.Equal(t, v1.Database.Driver, roundTripped.Database.Driver)
	assert.Equal(t, v1.Auth.DevMode, roundTripped.Auth.DevMode)
	assert.Equal(t, v1.Storage.Provider, roundTripped.Storage.Provider)
}

func TestConvertV1ServerToGlobalConfig_Basic(t *testing.T) {
	v1 := &V1ServerConfig{
		LogLevel:  "debug",
		LogFormat: "json",
		Hub: &V1ServerHubConfig{
			Port:         9810,
			Host:         "0.0.0.0",
			HubID:        "test-hub-id",
			PublicURL:    "https://hub.example.com",
			ReadTimeout:  "30s",
			WriteTimeout: "60s",
			AdminEmails:  []string{"admin@example.com"},
			CORS: &V1CORSConfig{
				Enabled:        true,
				AllowedOrigins: []string{"*"},
				MaxAge:         3600,
			},
		},
		Broker: &V1BrokerConfig{
			Enabled:              true,
			Port:                 9800,
			BrokerID:             "broker-123",
			BrokerName:           "my-broker",
			BrokerNickname:       "nick",
			HubEndpoint:          "https://hub.example.com",
			ContainerHubEndpoint: "http://host.containers.internal:8080",
		},
		Database: &V1DatabaseConfig{
			Driver: "sqlite",
			URL:    "/tmp/hub.db",
		},
		Auth: &V1AuthConfig{
			DevMode:           true,
			DevToken:          "dev-token",
			AuthorizedDomains: []string{"example.com"},
		},
		Storage: &V1StorageConfig{
			Provider:  "local",
			LocalPath: "/tmp/storage",
		},
		Secrets: &V1SecretsConfig{
			Backend: "local",
		},
	}

	gc := ConvertV1ServerToGlobalConfig(v1)

	assert.Equal(t, "debug", gc.LogLevel)
	assert.Equal(t, 9810, gc.Hub.Port)
	assert.Equal(t, "test-hub-id", gc.Hub.HubID)
	assert.Equal(t, "https://hub.example.com", gc.Hub.Endpoint)
	assert.Equal(t, true, gc.Hub.CORSEnabled)
	assert.Equal(t, 3600, gc.Hub.CORSMaxAge)
	assert.Equal(t, true, gc.RuntimeBroker.Enabled)
	assert.Equal(t, 9800, gc.RuntimeBroker.Port)
	assert.Equal(t, "broker-123", gc.RuntimeBroker.BrokerID)
	// BrokerName takes priority over BrokerNickname when both are set
	assert.Equal(t, "my-broker", gc.RuntimeBroker.BrokerName)
	assert.Equal(t, "https://hub.example.com", gc.RuntimeBroker.HubEndpoint)
	assert.Equal(t, "http://host.containers.internal:8080", gc.RuntimeBroker.ContainerHubEndpoint)
	assert.Equal(t, "sqlite", gc.Database.Driver)
	assert.Equal(t, "/tmp/hub.db", gc.Database.URL)
	assert.Equal(t, true, gc.Auth.Enabled)
	assert.Equal(t, "dev-token", gc.Auth.Token)
	assert.Equal(t, "local", gc.Storage.Provider)
	assert.Equal(t, "/tmp/storage", gc.Storage.LocalPath)
	assert.Equal(t, "local", gc.Secrets.Backend)
}

func TestConvertV1ServerToGlobalConfig_Mode(t *testing.T) {
	v1 := &V1ServerConfig{
		Mode: "hosted",
	}
	gc := ConvertV1ServerToGlobalConfig(v1)
	assert.Equal(t, "hosted", gc.Mode)

	// Legacy "production" value should pass through (backward compat)
	v1Legacy := &V1ServerConfig{
		Mode: "production",
	}
	gcLegacy := ConvertV1ServerToGlobalConfig(v1Legacy)
	assert.Equal(t, "production", gcLegacy.Mode)

	// Empty mode should leave it empty (workstation default)
	v1Empty := &V1ServerConfig{}
	gcEmpty := ConvertV1ServerToGlobalConfig(v1Empty)
	assert.Equal(t, "", gcEmpty.Mode)
}

func TestConvertGlobalToV1ServerConfig_Mode(t *testing.T) {
	gc := DefaultGlobalConfig()
	gc.Mode = "hosted"

	v1 := ConvertGlobalToV1ServerConfig(&gc)
	assert.Equal(t, "hosted", v1.Mode)

	// Round-trip
	gc2 := ConvertV1ServerToGlobalConfig(v1)
	assert.Equal(t, "hosted", gc2.Mode)
}

func TestConvertV1ServerToGlobalConfig_Nil(t *testing.T) {
	gc := ConvertV1ServerToGlobalConfig(nil)
	assert.NotNil(t, gc)
	// Should be defaults
	assert.Equal(t, "info", gc.LogLevel)
}

func TestConvertV1ServerToGlobalConfig_AllowContainerScriptHarnesses(t *testing.T) {
	boolPtr := func(b bool) *bool { return &b }

	t.Run("no broker section defaults to true", func(t *testing.T) {
		v1 := &V1ServerConfig{}
		gc := ConvertV1ServerToGlobalConfig(v1)
		assert.True(t, gc.RuntimeBroker.AllowContainerScriptHarnesses)
	})

	t.Run("broker section without field defaults to true", func(t *testing.T) {
		v1 := &V1ServerConfig{
			Broker: &V1BrokerConfig{Enabled: true},
		}
		gc := ConvertV1ServerToGlobalConfig(v1)
		assert.True(t, gc.RuntimeBroker.AllowContainerScriptHarnesses)
	})

	t.Run("broker section with explicit false", func(t *testing.T) {
		v1 := &V1ServerConfig{
			Broker: &V1BrokerConfig{
				AllowContainerScriptHarnesses: boolPtr(false),
			},
		}
		gc := ConvertV1ServerToGlobalConfig(v1)
		assert.False(t, gc.RuntimeBroker.AllowContainerScriptHarnesses)
	})

	t.Run("broker section with explicit true", func(t *testing.T) {
		v1 := &V1ServerConfig{
			Broker: &V1BrokerConfig{
				AllowContainerScriptHarnesses: boolPtr(true),
			},
		}
		gc := ConvertV1ServerToGlobalConfig(v1)
		assert.True(t, gc.RuntimeBroker.AllowContainerScriptHarnesses)
	})

	t.Run("nil config defaults to true", func(t *testing.T) {
		gc := ConvertV1ServerToGlobalConfig(nil)
		assert.True(t, gc.RuntimeBroker.AllowContainerScriptHarnesses)
	})
}

func TestConvertGlobalToV1ServerConfig_RoundTrip(t *testing.T) {
	gc := DefaultGlobalConfig()
	gc.LogLevel = "debug"
	gc.Hub.Port = 9999
	gc.Hub.HubID = "roundtrip-hub-id"
	gc.RuntimeBroker.Enabled = true
	gc.RuntimeBroker.BrokerID = "broker-abc"
	gc.RuntimeBroker.BrokerName = "test-broker"
	gc.Database.Driver = "sqlite"
	gc.Auth.Enabled = true
	gc.Auth.Token = "test-token"

	v1 := ConvertGlobalToV1ServerConfig(&gc)

	assert.Equal(t, "debug", v1.LogLevel)
	assert.Equal(t, 9999, v1.Hub.Port)
	assert.Equal(t, "roundtrip-hub-id", v1.Hub.HubID)
	assert.Equal(t, true, v1.Broker.Enabled)
	assert.Equal(t, "broker-abc", v1.Broker.BrokerID)
	assert.Equal(t, "test-broker", v1.Broker.BrokerName)
	assert.Equal(t, "sqlite", v1.Database.Driver)
	assert.Equal(t, true, v1.Auth.DevMode)
	assert.Equal(t, "test-token", v1.Auth.DevToken)

	// Round-trip back
	gc2 := ConvertV1ServerToGlobalConfig(v1)
	assert.Equal(t, gc.LogLevel, gc2.LogLevel)
	assert.Equal(t, gc.Hub.Port, gc2.Hub.Port)
	assert.Equal(t, gc.Hub.HubID, gc2.Hub.HubID)
	assert.Equal(t, gc.RuntimeBroker.Enabled, gc2.RuntimeBroker.Enabled)
	assert.Equal(t, gc.RuntimeBroker.BrokerID, gc2.RuntimeBroker.BrokerID)
	assert.Equal(t, gc.RuntimeBroker.BrokerName, gc2.RuntimeBroker.BrokerName)
}

func TestConvertGlobalToV1ServerConfig_Nil(t *testing.T) {
	v1 := ConvertGlobalToV1ServerConfig(nil)
	assert.NotNil(t, v1)
}

// TestAgentEndpointRoundTrip verifies server.hub.agent_endpoint survives the
// V1<->GlobalConfig conversion in both directions, independently of
// public_url / Hub.Endpoint.
func TestAgentEndpointRoundTrip(t *testing.T) {
	gc := DefaultGlobalConfig()
	gc.Hub.Endpoint = "https://hub.example.com"
	gc.Hub.AgentEndpoint = "http://192.0.2.10:8080"

	v1 := ConvertGlobalToV1ServerConfig(&gc)
	assert.Equal(t, "https://hub.example.com", v1.Hub.PublicURL)
	assert.Equal(t, "http://192.0.2.10:8080", v1.Hub.AgentEndpoint)

	gc2 := ConvertV1ServerToGlobalConfig(v1)
	assert.Equal(t, gc.Hub.Endpoint, gc2.Hub.Endpoint)
	assert.Equal(t, gc.Hub.AgentEndpoint, gc2.Hub.AgentEndpoint)
}

func TestLoadGlobalConfig_FromSettingsYAML(t *testing.T) {
	tmpDir := t.TempDir()

	originalHome := os.Getenv("HOME")
	defer func() { _ = os.Setenv("HOME", originalHome) }()
	_ = os.Setenv("HOME", tmpDir)

	globalDir := filepath.Join(tmpDir, ".scion")
	require.NoError(t, os.MkdirAll(globalDir, 0755))

	// Write settings.yaml with server key
	settingsContent := `
schema_version: "1"
server:
  log_level: debug
  log_format: json
  hub:
    port: 9999
    host: "0.0.0.0"
    hub_id: "settings-hub-id"
  broker:
    enabled: true
    port: 8888
    broker_id: "test-broker-id"
    broker_nickname: "test-broker-nick"
  database:
    driver: sqlite
  auth:
    dev_mode: true
    dev_token: "test-dev-token"
`
	require.NoError(t, os.WriteFile(filepath.Join(globalDir, "settings.yaml"), []byte(settingsContent), 0644))

	gc, err := LoadGlobalConfig(globalDir)
	require.NoError(t, err)

	assert.Equal(t, "debug", gc.LogLevel)
	// server.log_format is accepted but ignored (ptone/scion#4103).
	assert.Equal(t, 9999, gc.Hub.Port)
	assert.Equal(t, "settings-hub-id", gc.Hub.HubID)
	assert.Equal(t, true, gc.RuntimeBroker.Enabled)
	assert.Equal(t, 8888, gc.RuntimeBroker.Port)
	assert.Equal(t, "test-broker-id", gc.RuntimeBroker.BrokerID)
	assert.Equal(t, "test-broker-nick", gc.RuntimeBroker.BrokerName)
	assert.Equal(t, true, gc.Auth.Enabled)
	assert.Equal(t, "test-dev-token", gc.Auth.Token)
}

func TestLoadGlobalConfig_TelemetryEnabledFromSettingsYAML(t *testing.T) {
	tmpDir := t.TempDir()

	originalHome := os.Getenv("HOME")
	defer func() { _ = os.Setenv("HOME", originalHome) }()
	_ = os.Setenv("HOME", tmpDir)

	globalDir := filepath.Join(tmpDir, ".scion")
	require.NoError(t, os.MkdirAll(globalDir, 0755))

	// Write settings.yaml with top-level telemetry section (separate from server)
	settingsContent := `
schema_version: "1"
server:
  mode: production
  broker:
    broker_id: "test-broker-id"
telemetry:
  enabled: true
  cloud:
    enabled: true
    endpoint: "cloudtrace.googleapis.com:443"
`
	require.NoError(t, os.WriteFile(filepath.Join(globalDir, "settings.yaml"), []byte(settingsContent), 0644))

	gc, err := LoadGlobalConfig(globalDir)
	require.NoError(t, err)

	require.NotNil(t, gc.TelemetryEnabled, "TelemetryEnabled should be populated from top-level telemetry.enabled")
	assert.True(t, *gc.TelemetryEnabled)
}

func TestLoadGlobalConfig_TelemetryDisabledFromSettingsYAML(t *testing.T) {
	tmpDir := t.TempDir()

	originalHome := os.Getenv("HOME")
	defer func() { _ = os.Setenv("HOME", originalHome) }()
	_ = os.Setenv("HOME", tmpDir)

	globalDir := filepath.Join(tmpDir, ".scion")
	require.NoError(t, os.MkdirAll(globalDir, 0755))

	settingsContent := `
schema_version: "1"
server:
  mode: production
telemetry:
  enabled: false
`
	require.NoError(t, os.WriteFile(filepath.Join(globalDir, "settings.yaml"), []byte(settingsContent), 0644))

	gc, err := LoadGlobalConfig(globalDir)
	require.NoError(t, err)

	require.NotNil(t, gc.TelemetryEnabled)
	assert.False(t, *gc.TelemetryEnabled)
}

func TestLoadGlobalConfig_NoTelemetrySectionLeavesNil(t *testing.T) {
	tmpDir := t.TempDir()

	originalHome := os.Getenv("HOME")
	defer func() { _ = os.Setenv("HOME", originalHome) }()
	_ = os.Setenv("HOME", tmpDir)

	globalDir := filepath.Join(tmpDir, ".scion")
	require.NoError(t, os.MkdirAll(globalDir, 0755))

	settingsContent := `
schema_version: "1"
server:
  mode: production
`
	require.NoError(t, os.WriteFile(filepath.Join(globalDir, "settings.yaml"), []byte(settingsContent), 0644))

	gc, err := LoadGlobalConfig(globalDir)
	require.NoError(t, err)

	assert.Nil(t, gc.TelemetryEnabled, "TelemetryEnabled should be nil when no telemetry section exists")
}

func TestLoadGlobalConfig_SettingsYAMLPreferredOverServerYAML(t *testing.T) {
	tmpDir := t.TempDir()

	originalHome := os.Getenv("HOME")
	defer func() { _ = os.Setenv("HOME", originalHome) }()
	_ = os.Setenv("HOME", tmpDir)

	globalDir := filepath.Join(tmpDir, ".scion")
	require.NoError(t, os.MkdirAll(globalDir, 0755))

	// Write settings.yaml with server key
	settingsContent := `
schema_version: "1"
server:
  log_level: debug
  hub:
    port: 9999
`
	require.NoError(t, os.WriteFile(filepath.Join(globalDir, "settings.yaml"), []byte(settingsContent), 0644))

	// Write server.yaml (legacy) — should NOT be used
	serverContent := `
logLevel: warn
hub:
  port: 1111
`
	require.NoError(t, os.WriteFile(filepath.Join(globalDir, "server.yaml"), []byte(serverContent), 0644))

	gc, err := LoadGlobalConfig(globalDir)
	require.NoError(t, err)

	// settings.yaml should win
	assert.Equal(t, "debug", gc.LogLevel)
	assert.Equal(t, 9999, gc.Hub.Port)
}

func TestLoadGlobalConfig_FallsBackToServerYAML(t *testing.T) {
	tmpDir := t.TempDir()

	originalHome := os.Getenv("HOME")
	defer func() { _ = os.Setenv("HOME", originalHome) }()
	_ = os.Setenv("HOME", tmpDir)

	globalDir := filepath.Join(tmpDir, ".scion")
	require.NoError(t, os.MkdirAll(globalDir, 0755))

	// Write settings.yaml WITHOUT server key
	settingsContent := `
schema_version: "1"
active_profile: local
`
	require.NoError(t, os.WriteFile(filepath.Join(globalDir, "settings.yaml"), []byte(settingsContent), 0644))

	// Write server.yaml
	serverContent := `
logLevel: warn
hub:
  port: 7777
`
	require.NoError(t, os.WriteFile(filepath.Join(globalDir, "server.yaml"), []byte(serverContent), 0644))

	gc, err := LoadGlobalConfig(globalDir)
	require.NoError(t, err)

	// server.yaml should be used
	assert.Equal(t, "warn", gc.LogLevel)
	assert.Equal(t, 7777, gc.Hub.Port)
}

func TestVersionedEnvKeyMapper_DeepServerNesting(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		// Basic server keys
		{"SCION_SERVER_ENV", "server.env"},
		{"SCION_SERVER_LOG_LEVEL", "server.log_level"},
		{"SCION_SERVER_LOG_FORMAT", "server.log_format"},
		// Hub server keys
		{"SCION_SERVER_HUB_PORT", "server.hub.port"},
		{"SCION_SERVER_HUB_HOST", "server.hub.host"},
		{"SCION_SERVER_HUB_PUBLIC_URL", "server.hub.public_url"},
		{"SCION_SERVER_HUB_READ_TIMEOUT", "server.hub.read_timeout"},
		{"SCION_SERVER_HUB_WRITE_TIMEOUT", "server.hub.write_timeout"},
		// Broker keys
		{"SCION_SERVER_BROKER_PORT", "server.broker.port"},
		{"SCION_SERVER_BROKER_HOST", "server.broker.host"},
		{"SCION_SERVER_BROKER_BROKER_ID", "server.broker.broker_id"},
		{"SCION_SERVER_BROKER_BROKER_NAME", "server.broker.broker_name"},
		{"SCION_SERVER_BROKER_BROKER_NICKNAME", "server.broker.broker_nickname"},
		{"SCION_SERVER_BROKER_BROKER_TOKEN", "server.broker.broker_token"},
		{"SCION_SERVER_BROKER_HUB_ENDPOINT", "server.broker.hub_endpoint"},
		// Database keys
		{"SCION_SERVER_DATABASE_DRIVER", "server.database.driver"},
		{"SCION_SERVER_DATABASE_URL", "server.database.url"},
		// Auth keys
		{"SCION_SERVER_AUTH_DEV_MODE", "server.auth.dev_mode"},
		{"SCION_SERVER_AUTH_DEV_TOKEN", "server.auth.dev_token"},
		{"SCION_SERVER_AUTH_DEV_TOKEN_FILE", "server.auth.dev_token_file"},
		{"SCION_SERVER_AUTH_AUTHORIZED_DOMAINS", "server.auth.authorized_domains"},
		// OAuth keys
		{"SCION_SERVER_OAUTH_WEB_GOOGLE_CLIENT_ID", "server.oauth.web.google.client_id"},
		{"SCION_SERVER_OAUTH_WEB_GOOGLE_CLIENT_SECRET", "server.oauth.web.google.client_secret"},
		{"SCION_SERVER_OAUTH_CLI_GITHUB_CLIENT_ID", "server.oauth.cli.github.client_id"},
		// Storage keys
		{"SCION_SERVER_STORAGE_PROVIDER", "server.storage.provider"},
		{"SCION_SERVER_STORAGE_LOCAL_PATH", "server.storage.local_path"},
		// Secrets keys
		{"SCION_SERVER_SECRETS_BACKEND", "server.secrets.backend"},
		{"SCION_SERVER_SECRETS_GCP_PROJECT_ID", "server.secrets.gcp_project_id"},
		{"SCION_SERVER_SECRETS_GCP_CREDENTIALS", "server.secrets.gcp_credentials"},
		// CORS keys (nested under hub or broker)
		{"SCION_SERVER_HUB_CORS_ENABLED", "server.hub.cors.enabled"},
		{"SCION_SERVER_HUB_CORS_ALLOWED_ORIGINS", "server.hub.cors.allowed_origins"},
		{"SCION_SERVER_HUB_CORS_MAX_AGE", "server.hub.cors.max_age"},
		{"SCION_SERVER_BROKER_CORS_ENABLED", "server.broker.cors.enabled"},
		// JWT proxy auth provider keys
		{"SCION_SERVER_AUTH_PROXY_PROVIDER", "server.auth.proxy.provider"},
		{"SCION_SERVER_AUTH_PROXY_JWT_HEADER", "server.auth.proxy.jwt.header"},
		{"SCION_SERVER_AUTH_PROXY_JWT_ALGORITHM", "server.auth.proxy.jwt.algorithm"},
		{"SCION_SERVER_AUTH_PROXY_JWT_ISSUER", "server.auth.proxy.jwt.issuer"},
		{"SCION_SERVER_AUTH_PROXY_JWT_AUDIENCE", "server.auth.proxy.jwt.audience"},
		{"SCION_SERVER_AUTH_PROXY_JWT_JWKS_URL", "server.auth.proxy.jwt.jwks_url"},
		{"SCION_SERVER_AUTH_PROXY_JWT_JWKS_FILE", "server.auth.proxy.jwt.jwks_file"},
		{"SCION_SERVER_AUTH_PROXY_JWT_PUBLIC_KEY_FILE", "server.auth.proxy.jwt.public_key_file"},
		{"SCION_SERVER_AUTH_PROXY_JWT_CLAIMS_EMAIL", "server.auth.proxy.jwt.claims.email"},
		{"SCION_SERVER_AUTH_PROXY_JWT_CLAIMS_SUBJECT", "server.auth.proxy.jwt.claims.subject"},
		{"SCION_SERVER_AUTH_PROXY_JWT_CLAIMS_DISPLAY_NAME", "server.auth.proxy.jwt.claims.display_name"},
		{"SCION_SERVER_AUTH_PROXY_JWT_CLAIMS_DOMAIN", "server.auth.proxy.jwt.claims.domain"},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			result := versionedEnvKeyMapper(tt.input)
			assert.Equal(t, tt.expected, result, "input: %s", tt.input)
		})
	}
}

func TestMergeServerIntoSettings(t *testing.T) {
	tmpDir := t.TempDir()

	// Write existing settings.yaml
	existingContent := `
schema_version: "1"
active_profile: local
default_template: gemini
`
	require.NoError(t, os.WriteFile(filepath.Join(tmpDir, "settings.yaml"), []byte(existingContent), 0644))

	v1 := &V1ServerConfig{
		LogLevel: "debug",
		Hub: &V1ServerHubConfig{
			Port: 9999,
		},
	}

	err := MergeServerIntoSettings(tmpDir, v1)
	require.NoError(t, err)

	// Re-read and verify
	data, err := os.ReadFile(filepath.Join(tmpDir, "settings.yaml"))
	require.NoError(t, err)

	content := string(data)
	assert.Contains(t, content, "server:")
	assert.Contains(t, content, "active_profile")
	assert.Contains(t, content, "schema_version")
}

// ============================================================================
// Scheduler Config Tests
// ============================================================================

func TestConvertV1ServerToGlobalConfig_Scheduler(t *testing.T) {
	v1 := &V1ServerConfig{
		Scheduler: &V1SchedulerConfig{
			IntervalSeconds: 120,
			MaxConcurrency:  intPtr(3),
		},
	}
	gc := ConvertV1ServerToGlobalConfig(v1)
	assert.Equal(t, 120, gc.Scheduler.IntervalSeconds)
	require.NotNil(t, gc.Scheduler.MaxConcurrency)
	assert.Equal(t, 3, *gc.Scheduler.MaxConcurrency)
}

func TestConvertV1ServerToGlobalConfig_SchedulerNil(t *testing.T) {
	v1 := &V1ServerConfig{}
	gc := ConvertV1ServerToGlobalConfig(v1)
	assert.Equal(t, 0, gc.Scheduler.IntervalSeconds, "nil scheduler should leave defaults (zero)")
	assert.Nil(t, gc.Scheduler.MaxConcurrency, "nil scheduler should leave MaxConcurrency nil (use scheduler default)")
}

func TestConvertGlobalToV1ServerConfig_Scheduler(t *testing.T) {
	gc := DefaultGlobalConfig()
	gc.Scheduler.IntervalSeconds = 180
	gc.Scheduler.MaxConcurrency = intPtr(2)
	v1 := ConvertGlobalToV1ServerConfig(&gc)
	require.NotNil(t, v1.Scheduler)
	assert.Equal(t, 180, v1.Scheduler.IntervalSeconds)
	require.NotNil(t, v1.Scheduler.MaxConcurrency)
	assert.Equal(t, 2, *v1.Scheduler.MaxConcurrency)
}

func TestConvertGlobalToV1ServerConfig_SchedulerZeroOmitted(t *testing.T) {
	gc := DefaultGlobalConfig()
	// Zero/nil values — scheduler block should not be emitted
	v1 := ConvertGlobalToV1ServerConfig(&gc)
	assert.Nil(t, v1.Scheduler, "zero-value scheduler should be omitted in V1")
}

func TestSchedulerMaxConcurrency_ExplicitZeroRoundTrips(t *testing.T) {
	// Regression: explicit max_concurrency=0 (unlimited) must survive the
	// V1 → Global → V1 round-trip and not be confused with "unset".
	v1 := &V1ServerConfig{
		Scheduler: &V1SchedulerConfig{
			MaxConcurrency: intPtr(0),
		},
	}
	gc := ConvertV1ServerToGlobalConfig(v1)
	require.NotNil(t, gc.Scheduler.MaxConcurrency, "explicit 0 must not be nil")
	assert.Equal(t, 0, *gc.Scheduler.MaxConcurrency)

	v1Out := ConvertGlobalToV1ServerConfig(gc)
	require.NotNil(t, v1Out.Scheduler, "scheduler block must be emitted for explicit 0")
	require.NotNil(t, v1Out.Scheduler.MaxConcurrency)
	assert.Equal(t, 0, *v1Out.Scheduler.MaxConcurrency)
}

func TestVersionedEnvKeyMapper_Scheduler(t *testing.T) {
	tests := []struct {
		env  string
		want string
	}{
		{"SCION_SERVER_SCHEDULER_INTERVAL_SECONDS", "server.scheduler.interval_seconds"},
		{"SCION_SERVER_SCHEDULER_MAX_CONCURRENCY", "server.scheduler.max_concurrency"},
	}
	for _, tt := range tests {
		t.Run(tt.env, func(t *testing.T) {
			got := versionedEnvKeyMapper(tt.env)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestConvertV1ServerToGlobalConfig_StalledThresholdValid(t *testing.T) {
	v1 := &V1ServerConfig{
		Hub: &V1ServerHubConfig{
			StalledThreshold: "10m",
		},
	}
	gc := ConvertV1ServerToGlobalConfig(v1)
	assert.Equal(t, 10*time.Minute, gc.Hub.StalledThreshold)
}

func TestConvertV1ServerToGlobalConfig_StalledThresholdInvalidIgnored(t *testing.T) {
	v1 := &V1ServerConfig{
		Hub: &V1ServerHubConfig{
			StalledThreshold: "not-a-duration",
		},
	}
	gc := ConvertV1ServerToGlobalConfig(v1)
	// Invalid duration string should be ignored; StalledThreshold stays at zero value.
	assert.Equal(t, time.Duration(0), gc.Hub.StalledThreshold)
}

// --- Federation conversion tests ---

func TestConvertV1FederationConfig_RoundTrip(t *testing.T) {
	enabled := true
	v1 := &V1ServerConfig{
		Federation: &V1FederationConfig{
			Enabled: &enabled,
			TrustedIssuers: []V1TrustedIssuerConfig{
				{
					IssuerURL:        "https://hub-a.example.com",
					JWKSURL:          "https://hub-a.example.com/.well-known/jwks.json",
					ExpectedAudience: "https://hub-b.example.com",
					AllowedProjects:  []string{"proj1", "proj2"},
					AllowedRootUsers: []string{"user@example.com"},
					DefaultScopes:    []string{"agent:status:update", "agent:message:send"},
					IssuerType:       "hub",
					DefaultRole:      "",
					AllowedEmails:    nil,
					// AllowVisitorGrants applies to hub and user issuers.
					AllowVisitorGrants: true,
				},
				{
					IssuerURL:        "https://accounts.google.com",
					JWKSURL:          "",
					ExpectedAudience: "https://hub-b.example.com",
					AllowedProjects:  nil,
					AllowedRootUsers: nil,
					DefaultScopes:    []string{"agent:status:update"},
					IssuerType:       "service_account",
					DefaultRole:      "",
					AllowedEmails:    []string{"sa@proj.iam.gserviceaccount.com"},
				},
				{
					// AllowedGCPProjects and AllowedDomains only do anything
					// on an active Google user issuer: issuer_type "user" and
					// a non-empty ExpectedAudience, unlike the
					// service_account entry above, which must not set them
					// (that combination is a config validation error).
					IssuerURL:          "https://accounts.google.com/",
					ExpectedAudience:   "client-id.apps.googleusercontent.com",
					IssuerType:         "user",
					AllowedGCPProjects: []string{"gcp-proj-1", "gcp-proj-2"},
					AllowedDomains:     []string{"Example.com", "other.example"},
				},
			},
			Algorithms:       []string{"RS256", "ES256"},
			RefreshInterval:  "1h",
			DebounceInterval: "5s",
		},
	}

	// V1 -> GlobalConfig
	gc := ConvertV1ServerToGlobalConfig(v1)
	assert.True(t, gc.Federation.Enabled)
	require.Len(t, gc.Federation.TrustedIssuers, 3)

	ti0 := gc.Federation.TrustedIssuers[0]
	assert.Equal(t, "https://hub-a.example.com", ti0.IssuerURL)
	assert.Equal(t, "https://hub-a.example.com/.well-known/jwks.json", ti0.JWKSURL)
	assert.Equal(t, "https://hub-b.example.com", ti0.ExpectedAudience)
	assert.Equal(t, []string{"proj1", "proj2"}, ti0.AllowedProjects)
	assert.Equal(t, []string{"user@example.com"}, ti0.AllowedRootUsers)
	assert.Equal(t, []string{"agent:status:update", "agent:message:send"}, ti0.DefaultScopes)
	assert.Equal(t, "hub", ti0.IssuerType)
	assert.True(t, ti0.AllowVisitorGrants)

	ti1 := gc.Federation.TrustedIssuers[1]
	assert.Equal(t, "https://accounts.google.com", ti1.IssuerURL)
	assert.Equal(t, "service_account", ti1.IssuerType)
	assert.Equal(t, []string{"sa@proj.iam.gserviceaccount.com"}, ti1.AllowedEmails)
	assert.False(t, ti1.AllowVisitorGrants)

	ti2 := gc.Federation.TrustedIssuers[2]
	assert.Equal(t, "https://accounts.google.com/", ti2.IssuerURL)
	assert.Equal(t, "user", ti2.IssuerType)
	assert.Equal(t, []string{"gcp-proj-1", "gcp-proj-2"}, ti2.AllowedGCPProjects)
	assert.Equal(t, []string{"Example.com", "other.example"}, ti2.AllowedDomains)

	assert.Equal(t, []string{"RS256", "ES256"}, gc.Federation.Algorithms)
	assert.Equal(t, time.Hour, gc.Federation.Cache.RefreshInterval)
	assert.Equal(t, 5*time.Second, gc.Federation.Cache.DebounceInterval)

	// GlobalConfig -> V1 (round-trip back)
	v1Back := ConvertGlobalToV1ServerConfig(gc)
	require.NotNil(t, v1Back.Federation)
	assert.Equal(t, &enabled, v1Back.Federation.Enabled)
	assert.Equal(t, []string{"RS256", "ES256"}, v1Back.Federation.Algorithms)
	assert.Equal(t, "1h0m0s", v1Back.Federation.RefreshInterval)
	assert.Equal(t, "5s", v1Back.Federation.DebounceInterval)

	require.Len(t, v1Back.Federation.TrustedIssuers, 3)
	vi0 := v1Back.Federation.TrustedIssuers[0]
	assert.Equal(t, "https://hub-a.example.com", vi0.IssuerURL)
	assert.Equal(t, "https://hub-a.example.com/.well-known/jwks.json", vi0.JWKSURL)
	assert.Equal(t, "https://hub-b.example.com", vi0.ExpectedAudience)
	assert.Equal(t, []string{"proj1", "proj2"}, vi0.AllowedProjects)
	assert.Equal(t, []string{"user@example.com"}, vi0.AllowedRootUsers)
	assert.Equal(t, []string{"agent:status:update", "agent:message:send"}, vi0.DefaultScopes)
	assert.Equal(t, "hub", vi0.IssuerType)
	assert.True(t, vi0.AllowVisitorGrants)

	vi1 := v1Back.Federation.TrustedIssuers[1]
	assert.Equal(t, "https://accounts.google.com", vi1.IssuerURL)
	assert.Equal(t, "service_account", vi1.IssuerType)
	assert.Equal(t, []string{"sa@proj.iam.gserviceaccount.com"}, vi1.AllowedEmails)

	vi2 := v1Back.Federation.TrustedIssuers[2]
	assert.Equal(t, "https://accounts.google.com/", vi2.IssuerURL)
	assert.Equal(t, "user", vi2.IssuerType)
	assert.Equal(t, []string{"gcp-proj-1", "gcp-proj-2"}, vi2.AllowedGCPProjects)
	assert.Equal(t, []string{"Example.com", "other.example"}, vi2.AllowedDomains)
}

func TestConvertV1FederationConfig_NilFederation(t *testing.T) {
	v1 := &V1ServerConfig{}
	gc := ConvertV1ServerToGlobalConfig(v1)
	assert.False(t, gc.Federation.Enabled)
	assert.Empty(t, gc.Federation.TrustedIssuers)
}

func TestConvertGlobalToV1_FederationDisabledNoIssuers(t *testing.T) {
	gc := DefaultGlobalConfig()
	gc.Federation.Enabled = false
	gc.Federation.TrustedIssuers = nil
	v1 := ConvertGlobalToV1ServerConfig(&gc)
	// When federation is disabled with no issuers, Federation should be nil in V1
	assert.Nil(t, v1.Federation)
}

func TestConvertV1FederationConfig_EnabledNilDereference(t *testing.T) {
	// Test that nil Enabled is handled safely (defaults to false)
	v1 := &V1ServerConfig{
		Federation: &V1FederationConfig{
			Enabled: nil,
			TrustedIssuers: []V1TrustedIssuerConfig{
				{
					IssuerURL: "https://hub-a.example.com",
				},
			},
		},
	}
	gc := ConvertV1ServerToGlobalConfig(v1)
	assert.False(t, gc.Federation.Enabled)
	require.Len(t, gc.Federation.TrustedIssuers, 1)
	assert.Equal(t, "https://hub-a.example.com", gc.Federation.TrustedIssuers[0].IssuerURL)
}

// --- OIDC Login Config Tests ---

func TestConvertV1ServerToGlobalConfig_OIDCLogin(t *testing.T) {
	enabled := true
	v1 := &V1ServerConfig{
		OIDCLogin: &V1OIDCLoginConfig{
			Enabled:      &enabled,
			DisplayName:  "Corporate SSO",
			IssuerURL:    "https://sso.example.com/auth/realms/main",
			ClientID:     "scion-client",
			ClientSecret: "secret-value",
			Scopes:       []string{"openid", "email", "custom-scope"},
		},
	}

	gc := ConvertV1ServerToGlobalConfig(v1)

	assert.True(t, gc.OIDCLogin.Enabled)
	assert.Equal(t, "Corporate SSO", gc.OIDCLogin.DisplayName)
	assert.Equal(t, "https://sso.example.com/auth/realms/main", gc.OIDCLogin.IssuerURL)
	assert.Equal(t, "scion-client", gc.OIDCLogin.ClientID)
	assert.Equal(t, "secret-value", gc.OIDCLogin.ClientSecret)
	assert.Equal(t, []string{"openid", "email", "custom-scope"}, gc.OIDCLogin.Scopes)
}

func TestConvertV1ServerToGlobalConfig_OIDCLoginNil(t *testing.T) {
	v1 := &V1ServerConfig{}
	gc := ConvertV1ServerToGlobalConfig(v1)

	assert.False(t, gc.OIDCLogin.Enabled)
	assert.Empty(t, gc.OIDCLogin.DisplayName)
	assert.Empty(t, gc.OIDCLogin.IssuerURL)
	assert.Empty(t, gc.OIDCLogin.ClientID)
	assert.Empty(t, gc.OIDCLogin.ClientSecret)
	assert.Nil(t, gc.OIDCLogin.Scopes)
}

func TestConvertV1ServerToGlobalConfig_OIDCLoginDefaultScopes(t *testing.T) {
	enabled := true
	v1 := &V1ServerConfig{
		OIDCLogin: &V1OIDCLoginConfig{
			Enabled:   &enabled,
			IssuerURL: "https://sso.example.com",
			ClientID:  "client-id",
			// Scopes not set — should remain nil (defaults applied at usage time)
		},
	}

	gc := ConvertV1ServerToGlobalConfig(v1)
	assert.True(t, gc.OIDCLogin.Enabled)
	assert.Nil(t, gc.OIDCLogin.Scopes)
}

func TestConvertGlobalToV1ServerConfig_OIDCLogin(t *testing.T) {
	gc := &GlobalConfig{
		OIDCLogin: OIDCLoginConfig{
			Enabled:      true,
			DisplayName:  "JB Hunt SSO",
			IssuerURL:    "https://sso.example.com/auth/realms/security360",
			ClientID:     "scion-test",
			ClientSecret: "test-secret",
			Scopes:       []string{"openid", "email"},
		},
	}

	v1 := ConvertGlobalToV1ServerConfig(gc)

	require.NotNil(t, v1.OIDCLogin)
	assert.Equal(t, boolPtr(true), v1.OIDCLogin.Enabled)
	assert.Equal(t, "JB Hunt SSO", v1.OIDCLogin.DisplayName)
	assert.Equal(t, "https://sso.example.com/auth/realms/security360", v1.OIDCLogin.IssuerURL)
	assert.Equal(t, "scion-test", v1.OIDCLogin.ClientID)
	assert.Equal(t, "test-secret", v1.OIDCLogin.ClientSecret)
	assert.Equal(t, []string{"openid", "email"}, v1.OIDCLogin.Scopes)
}

func TestConvertGlobalToV1ServerConfig_OIDCLoginDisabledOmitted(t *testing.T) {
	gc := &GlobalConfig{
		OIDCLogin: OIDCLoginConfig{
			Enabled: false,
			// IssuerURL is empty too, so it should be omitted
		},
	}

	v1 := ConvertGlobalToV1ServerConfig(gc)
	assert.Nil(t, v1.OIDCLogin)
}

func TestConvertV1OIDCLoginConfig_RoundTrip(t *testing.T) {
	original := &GlobalConfig{
		OIDCLogin: OIDCLoginConfig{
			Enabled:      true,
			DisplayName:  "Test SSO",
			IssuerURL:    "https://idp.example.com",
			ClientID:     "client-123",
			ClientSecret: "secret-456",
			Scopes:       []string{"openid", "email", "profile"},
		},
	}

	v1 := ConvertGlobalToV1ServerConfig(original)
	roundTripped := ConvertV1ServerToGlobalConfig(v1)

	assert.Equal(t, original.OIDCLogin.Enabled, roundTripped.OIDCLogin.Enabled)
	assert.Equal(t, original.OIDCLogin.DisplayName, roundTripped.OIDCLogin.DisplayName)
	assert.Equal(t, original.OIDCLogin.IssuerURL, roundTripped.OIDCLogin.IssuerURL)
	assert.Equal(t, original.OIDCLogin.ClientID, roundTripped.OIDCLogin.ClientID)
	assert.Equal(t, original.OIDCLogin.ClientSecret, roundTripped.OIDCLogin.ClientSecret)
	assert.Equal(t, original.OIDCLogin.Scopes, roundTripped.OIDCLogin.Scopes)
}

// server.log_format is accepted and ignored (ptone/scion#4103): a settings
// file that still sets it validates and decodes, and nothing is carried into
// the hub's GlobalConfig.
func TestServerLogFormat_AcceptedAndIgnored(t *testing.T) {
	data := []byte(`
schema_version: "1"
server:
  log_level: info
  log_format: json
`)
	_, err := ValidateSettings(data, "1")
	require.NoError(t, err)

	var vs VersionedSettings
	require.NoError(t, yaml.Unmarshal(data, &vs))
	require.NotNil(t, vs.Server)
	assert.Equal(t, "json", vs.Server.LogFormat)

	gc := ConvertV1ServerToGlobalConfig(vs.Server)
	assert.Equal(t, "info", gc.LogLevel)
	assert.Empty(t, ConvertGlobalToV1ServerConfig(gc).LogFormat)
}
