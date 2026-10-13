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
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

// --- Telemetry settings tests ---

func TestLoadVersionedSettings_TelemetryRoundTrip(t *testing.T) {
	// Verify that a full telemetry config YAML can be marshaled, validated, and unmarshaled.
	data := []byte(`
schema_version: "1"
telemetry:
  enabled: true
  cloud:
    enabled: true
    endpoint: "https://otel.example.com"
    protocol: grpc
    headers:
      Authorization: "Bearer test-key"
    tls:
      enabled: true
      insecure_skip_verify: false
      ca_file: "/etc/ssl/certs/custom-root.pem"
      cert_file: "/etc/ssl/certs/client.pem"
      key_file: "/etc/ssl/private/client-key.pem"
    batch:
      max_size: 256
      timeout: "10s"
  hub:
    enabled: true
    report_interval: "60s"
  local:
    enabled: false
    file: "/tmp/telemetry.jsonl"
    console: true
  filter:
    enabled: true
    respect_debug_mode: true
    events:
      include: []
      exclude:
        - "agent.user.prompt"
    attributes:
      redact:
        - "prompt"
        - "user.email"
      hash:
        - "session_id"
    sampling:
      default: 0.5
      rates:
        "agent.tool.call": 0.1
  resource:
    service.name: "scion-agent"
    deployment.env: "staging"
`)
	// Validate against schema
	validationErrors, err := ValidateSettings(data, "1")
	require.NoError(t, err)
	assert.Empty(t, validationErrors, "full telemetry config should be valid, got: %v", validationErrors)

	// Unmarshal into struct
	var vs VersionedSettings
	require.NoError(t, yaml.Unmarshal(data, &vs))

	require.NotNil(t, vs.Telemetry)
	require.NotNil(t, vs.Telemetry.Enabled)
	assert.True(t, *vs.Telemetry.Enabled)

	require.NotNil(t, vs.Telemetry.Cloud)
	require.NotNil(t, vs.Telemetry.Cloud.Enabled)
	assert.True(t, *vs.Telemetry.Cloud.Enabled)
	assert.Equal(t, "https://otel.example.com", vs.Telemetry.Cloud.Endpoint)
	assert.Equal(t, "grpc", vs.Telemetry.Cloud.Protocol)
	assert.Equal(t, "Bearer test-key", vs.Telemetry.Cloud.Headers["Authorization"])

	require.NotNil(t, vs.Telemetry.Cloud.TLS)
	require.NotNil(t, vs.Telemetry.Cloud.TLS.Enabled)
	assert.True(t, *vs.Telemetry.Cloud.TLS.Enabled)
	require.NotNil(t, vs.Telemetry.Cloud.TLS.InsecureSkipVerify)
	assert.False(t, *vs.Telemetry.Cloud.TLS.InsecureSkipVerify)
	assert.Equal(t, "/etc/ssl/certs/custom-root.pem", vs.Telemetry.Cloud.TLS.CAFile)
	assert.Equal(t, "/etc/ssl/certs/client.pem", vs.Telemetry.Cloud.TLS.CertFile)
	assert.Equal(t, "/etc/ssl/private/client-key.pem", vs.Telemetry.Cloud.TLS.KeyFile)

	require.NotNil(t, vs.Telemetry.Cloud.Batch)
	assert.Equal(t, 256, vs.Telemetry.Cloud.Batch.MaxSize)
	assert.Equal(t, "10s", vs.Telemetry.Cloud.Batch.Timeout)

	require.NotNil(t, vs.Telemetry.Hub)
	require.NotNil(t, vs.Telemetry.Hub.Enabled)
	assert.True(t, *vs.Telemetry.Hub.Enabled)
	assert.Equal(t, "60s", vs.Telemetry.Hub.ReportInterval)

	require.NotNil(t, vs.Telemetry.Local)
	require.NotNil(t, vs.Telemetry.Local.Enabled)
	assert.False(t, *vs.Telemetry.Local.Enabled)
	assert.Equal(t, "/tmp/telemetry.jsonl", vs.Telemetry.Local.File)
	require.NotNil(t, vs.Telemetry.Local.Console)
	assert.True(t, *vs.Telemetry.Local.Console)

	require.NotNil(t, vs.Telemetry.Filter)
	require.NotNil(t, vs.Telemetry.Filter.Enabled)
	assert.True(t, *vs.Telemetry.Filter.Enabled)
	require.NotNil(t, vs.Telemetry.Filter.RespectDebugMode)
	assert.True(t, *vs.Telemetry.Filter.RespectDebugMode)

	require.NotNil(t, vs.Telemetry.Filter.Events)
	assert.Equal(t, []string{"agent.user.prompt"}, vs.Telemetry.Filter.Events.Exclude)

	require.NotNil(t, vs.Telemetry.Filter.Attributes)
	assert.Equal(t, []string{"prompt", "user.email"}, vs.Telemetry.Filter.Attributes.Redact)
	assert.Equal(t, []string{"session_id"}, vs.Telemetry.Filter.Attributes.Hash)

	require.NotNil(t, vs.Telemetry.Filter.Sampling)
	require.NotNil(t, vs.Telemetry.Filter.Sampling.Default)
	assert.Equal(t, 0.5, *vs.Telemetry.Filter.Sampling.Default)
	assert.Equal(t, 0.1, vs.Telemetry.Filter.Sampling.Rates["agent.tool.call"])

	assert.Equal(t, "scion-agent", vs.Telemetry.Resource["service.name"])
	assert.Equal(t, "staging", vs.Telemetry.Resource["deployment.env"])
}

func TestValidateSettings_TelemetryInvalidProtocol(t *testing.T) {
	data := []byte(`
schema_version: "1"
telemetry:
  cloud:
    protocol: "invalid"
`)
	errors, err := ValidateSettings(data, "1")
	require.NoError(t, err)
	assert.NotEmpty(t, errors, "invalid protocol should produce validation error")
}

func TestValidateSettings_TelemetrySamplingOutOfRange(t *testing.T) {
	data := []byte(`
schema_version: "1"
telemetry:
  filter:
    sampling:
      default: 1.5
`)
	errors, err := ValidateSettings(data, "1")
	require.NoError(t, err)
	assert.NotEmpty(t, errors, "sampling rate > 1.0 should produce validation error")
}

func TestValidateSettings_TelemetryUnknownField(t *testing.T) {
	data := []byte(`
schema_version: "1"
telemetry:
  unknown_field: true
`)
	errors, err := ValidateSettings(data, "1")
	require.NoError(t, err)
	assert.NotEmpty(t, errors, "unknown field in telemetry should produce validation error")
}

func TestLoadVersionedSettings_TelemetryHierarchyMerge(t *testing.T) {
	// Unset all SCION_ environment variables to avoid pollution
	for _, e := range os.Environ() {
		if strings.HasPrefix(e, "SCION_") {
			key := strings.SplitN(e, "=", 2)[0]
			val := os.Getenv(key)
			_ = os.Unsetenv(key)
			defer func() { _ = os.Setenv(key, val) }()
		}
	}

	// Test that telemetry settings merge across global → project (last write wins).
	tmpDir := t.TempDir()

	originalHome := os.Getenv("HOME")
	defer func() { _ = os.Setenv("HOME", originalHome) }()
	_ = os.Setenv("HOME", tmpDir)

	originalWd, _ := os.Getwd()
	defer func() { _ = os.Chdir(originalWd) }()

	// Create global settings with telemetry defaults
	globalScionDir := filepath.Join(tmpDir, ".scion")
	require.NoError(t, os.MkdirAll(globalScionDir, 0755))

	globalSettings := `schema_version: "1"
telemetry:
  enabled: true
  cloud:
    enabled: true
    endpoint: "https://global-otel.example.com"
    protocol: grpc
  hub:
    enabled: true
    report_interval: "30s"
  filter:
    events:
      exclude:
        - "agent.user.prompt"
`
	require.NoError(t, os.WriteFile(filepath.Join(globalScionDir, "settings.yaml"), []byte(globalSettings), 0644))

	// Create project with overrides
	projectDir := filepath.Join(tmpDir, "myproject")
	projectScionDir := filepath.Join(projectDir, ".scion")
	require.NoError(t, os.MkdirAll(projectScionDir, 0755))
	_ = os.Chdir(projectDir)

	projectSettings := `schema_version: "1"
telemetry:
  cloud:
    endpoint: "https://project-otel.example.com"
  hub:
    enabled: false
`
	require.NoError(t, os.WriteFile(filepath.Join(projectScionDir, "settings.yaml"), []byte(projectSettings), 0644))

	// Load merged settings
	vs, err := LoadVersionedSettings(projectScionDir)
	require.NoError(t, err)
	require.NotNil(t, vs.Telemetry)

	// Telemetry.enabled should come from global (not overridden by project)
	require.NotNil(t, vs.Telemetry.Enabled)
	assert.True(t, *vs.Telemetry.Enabled)

	// Cloud endpoint should be overridden by project
	require.NotNil(t, vs.Telemetry.Cloud)
	assert.Equal(t, "https://project-otel.example.com", vs.Telemetry.Cloud.Endpoint)

	// Cloud protocol should come from global
	assert.Equal(t, "grpc", vs.Telemetry.Cloud.Protocol)

	// Hub.enabled should be overridden by project
	require.NotNil(t, vs.Telemetry.Hub)
	require.NotNil(t, vs.Telemetry.Hub.Enabled)
	assert.False(t, *vs.Telemetry.Hub.Enabled)

	// Hub report_interval should come from global
	assert.Equal(t, "30s", vs.Telemetry.Hub.ReportInterval)
}

func TestVersionedEnvKeyMapper_Telemetry(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"SCION_TELEMETRY_ENABLED", "telemetry.enabled"},
		{"SCION_TELEMETRY_CLOUD_ENABLED", "telemetry.cloud.enabled"},
		{"SCION_TELEMETRY_CLOUD_ENDPOINT", "telemetry.cloud.endpoint"},
		{"SCION_TELEMETRY_CLOUD_PROTOCOL", "telemetry.cloud.protocol"},
		{"SCION_TELEMETRY_CLOUD_TLS_INSECURE_SKIP_VERIFY", "telemetry.cloud.tls.insecure_skip_verify"},
		{"SCION_TELEMETRY_CLOUD_BATCH_MAX_SIZE", "telemetry.cloud.batch.max_size"},
		{"SCION_TELEMETRY_CLOUD_BATCH_TIMEOUT", "telemetry.cloud.batch.timeout"},
		{"SCION_TELEMETRY_HUB_ENABLED", "telemetry.hub.enabled"},
		{"SCION_TELEMETRY_HUB_REPORT_INTERVAL", "telemetry.hub.report_interval"},
		{"SCION_TELEMETRY_LOCAL_ENABLED", "telemetry.local.enabled"},
		{"SCION_TELEMETRY_LOCAL_FILE", "telemetry.local.file"},
		{"SCION_TELEMETRY_LOCAL_CONSOLE", "telemetry.local.console"},
		{"SCION_TELEMETRY_FILTER_ENABLED", "telemetry.filter.enabled"},
		{"SCION_TELEMETRY_FILTER_RESPECT_DEBUG_MODE", "telemetry.filter.respect_debug_mode"},
		// OTEL env vars map to telemetry.cloud.*
		{"SCION_OTEL_ENDPOINT", "telemetry.cloud.endpoint"},
		{"SCION_OTEL_PROTOCOL", "telemetry.cloud.protocol"},
		{"SCION_OTEL_HEADERS", "telemetry.cloud.headers"},
		{"SCION_OTEL_INSECURE", "telemetry.cloud.tls.enabled"},
		{"SCION_OTEL_SKIP_TLS_VERIFY", "telemetry.cloud.tls.insecure_skip_verify"},
		{"SCION_OTEL_CA_FILE", "telemetry.cloud.tls.ca_file"},
		{"SCION_OTEL_CERT_FILE", "telemetry.cloud.tls.cert_file"},
		{"SCION_OTEL_KEY_FILE", "telemetry.cloud.tls.key_file"},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			result := versionedEnvKeyMapper(tt.input)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestLoadVersionedSettings_TelemetryEnvOverride(t *testing.T) {
	// Unset all SCION_ environment variables to avoid pollution
	for _, e := range os.Environ() {
		if strings.HasPrefix(e, "SCION_") {
			key := strings.SplitN(e, "=", 2)[0]
			val := os.Getenv(key)
			_ = os.Unsetenv(key)
			defer func() { _ = os.Setenv(key, val) }()
		}
	}

	tmpDir := t.TempDir()

	originalHome := os.Getenv("HOME")
	defer func() { _ = os.Setenv("HOME", originalHome) }()
	_ = os.Setenv("HOME", tmpDir)

	originalWd, _ := os.Getwd()
	defer func() { _ = os.Chdir(originalWd) }()
	_ = os.Chdir(tmpDir)

	// Create global settings with telemetry
	globalScionDir := filepath.Join(tmpDir, ".scion")
	require.NoError(t, os.MkdirAll(globalScionDir, 0755))

	globalSettings := `schema_version: "1"
telemetry:
  enabled: true
  cloud:
    endpoint: "https://yaml-endpoint.example.com"
`
	require.NoError(t, os.WriteFile(filepath.Join(globalScionDir, "settings.yaml"), []byte(globalSettings), 0644))

	// Set env var override
	t.Setenv("SCION_TELEMETRY_ENABLED", "false")

	vs, err := LoadVersionedSettings("")
	require.NoError(t, err)
	require.NotNil(t, vs.Telemetry)

	// Env var should override the YAML setting
	require.NotNil(t, vs.Telemetry.Enabled)
	assert.False(t, *vs.Telemetry.Enabled)

	// YAML value should be preserved for non-overridden fields
	require.NotNil(t, vs.Telemetry.Cloud)
	assert.Equal(t, "https://yaml-endpoint.example.com", vs.Telemetry.Cloud.Endpoint)
}
