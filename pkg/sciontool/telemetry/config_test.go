/*
Copyright 2025 The Scion Authors.
*/

package telemetry

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/util/gcp"
)

func TestLoadConfig_Defaults(t *testing.T) {
	// Clear all env vars
	clearTelemetryEnv()

	cfg := LoadConfig()

	if !cfg.Enabled {
		t.Error("Expected Enabled to be true by default")
	}
	if !cfg.CloudEnabled {
		t.Error("Expected CloudEnabled to be true by default")
	}
	if cfg.Protocol != DefaultProtocol {
		t.Errorf("Expected Protocol to be %q, got %q", DefaultProtocol, cfg.Protocol)
	}
	if cfg.GRPCPort != DefaultGRPCPort {
		t.Errorf("Expected GRPCPort to be %d, got %d", DefaultGRPCPort, cfg.GRPCPort)
	}
	if cfg.HTTPPort != DefaultHTTPPort {
		t.Errorf("Expected HTTPPort to be %d, got %d", DefaultHTTPPort, cfg.HTTPPort)
	}
	if cfg.Insecure {
		t.Error("Expected Insecure to be false by default")
	}
	if cfg.CAFile != "" {
		t.Errorf("Expected CAFile to be empty by default, got %q", cfg.CAFile)
	}
	if cfg.CertFile != "" || cfg.KeyFile != "" {
		t.Errorf("Expected CertFile and KeyFile to be empty by default, got %q and %q", cfg.CertFile, cfg.KeyFile)
	}
	if cfg.MetricsDebug {
		t.Error("Expected MetricsDebug to be false by default")
	}
	// Default exclude list should be applied
	if len(cfg.Filter.Exclude) != 1 || cfg.Filter.Exclude[0] != "agent.user.prompt" {
		t.Errorf("Expected default exclude list, got %v", cfg.Filter.Exclude)
	}
}

func TestLoadConfig_EnvOverrides(t *testing.T) {
	clearTelemetryEnv()

	_ = os.Setenv(EnvEnabled, "false")
	_ = os.Setenv(EnvCloudEnabled, "false")
	_ = os.Setenv(EnvEndpoint, "otel.example.com:443")
	_ = os.Setenv(EnvProtocol, "http")
	_ = os.Setenv(EnvInsecure, "true")
	if err := os.Setenv(EnvCAFile, "/etc/ssl/certs/custom-root.pem"); err != nil {
		t.Fatalf("failed to set %s: %v", EnvCAFile, err)
	}
	_ = os.Setenv(EnvCertFile, "/etc/ssl/certs/client.pem")
	_ = os.Setenv(EnvKeyFile, "/etc/ssl/private/client-key.pem")
	_ = os.Setenv(EnvGRPCPort, "14317")
	_ = os.Setenv(EnvHTTPPort, "14318")
	_ = os.Setenv(EnvProjectID, "my-project")
	_ = os.Setenv(EnvFilterExclude, "event.type.a,event.type.b")
	_ = os.Setenv(EnvFilterInclude, "event.type.c")
	_ = os.Setenv(EnvMetricsDebug, "true")
	defer clearTelemetryEnv()

	cfg := LoadConfig()

	if cfg.Enabled {
		t.Error("Expected Enabled to be false")
	}
	if cfg.CloudEnabled {
		t.Error("Expected CloudEnabled to be false")
	}
	if cfg.Endpoint != "otel.example.com:443" {
		t.Errorf("Expected Endpoint to be 'otel.example.com:443', got %q", cfg.Endpoint)
	}
	if cfg.Protocol != "http" {
		t.Errorf("Expected Protocol to be 'http', got %q", cfg.Protocol)
	}
	if !cfg.Insecure {
		t.Error("Expected Insecure to be true")
	}
	if cfg.CAFile != "/etc/ssl/certs/custom-root.pem" {
		t.Errorf("Expected CAFile to be '/etc/ssl/certs/custom-root.pem', got %q", cfg.CAFile)
	}
	if cfg.CertFile != "/etc/ssl/certs/client.pem" {
		t.Errorf("Expected CertFile to be '/etc/ssl/certs/client.pem', got %q", cfg.CertFile)
	}
	if cfg.KeyFile != "/etc/ssl/private/client-key.pem" {
		t.Errorf("Expected KeyFile to be '/etc/ssl/private/client-key.pem', got %q", cfg.KeyFile)
	}
	if cfg.GRPCPort != 14317 {
		t.Errorf("Expected GRPCPort to be 14317, got %d", cfg.GRPCPort)
	}
	if cfg.HTTPPort != 14318 {
		t.Errorf("Expected HTTPPort to be 14318, got %d", cfg.HTTPPort)
	}
	if cfg.ProjectID != "my-project" {
		t.Errorf("Expected ProjectID to be 'my-project', got %q", cfg.ProjectID)
	}
	if len(cfg.Filter.Exclude) != 2 {
		t.Errorf("Expected 2 exclude patterns, got %d", len(cfg.Filter.Exclude))
	}
	if len(cfg.Filter.Include) != 1 {
		t.Errorf("Expected 1 include pattern, got %d", len(cfg.Filter.Include))
	}
	if !cfg.MetricsDebug {
		t.Error("Expected MetricsDebug to be true")
	}
}

func TestIsCloudConfigured(t *testing.T) {
	tests := []struct {
		name     string
		config   *Config
		expected bool
	}{
		{
			name:     "nil config",
			config:   nil,
			expected: false,
		},
		{
			name: "cloud disabled",
			config: &Config{
				CloudEnabled: false,
				Endpoint:     "otel.example.com",
			},
			expected: false,
		},
		{
			name: "no endpoint",
			config: &Config{
				CloudEnabled: true,
				Endpoint:     "",
			},
			expected: false,
		},
		{
			name: "properly configured",
			config: &Config{
				CloudEnabled: true,
				Endpoint:     "otel.example.com:443",
			},
			expected: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.config.IsCloudConfigured(); got != tt.expected {
				t.Errorf("IsCloudConfigured() = %v, want %v", got, tt.expected)
			}
		})
	}
}

func TestParseCSVEnv(t *testing.T) {
	tests := []struct {
		value    string
		expected []string
	}{
		{"", nil},
		{"a", []string{"a"}},
		{"a,b,c", []string{"a", "b", "c"}},
		{"a, b, c", []string{"a", "b", "c"}},
		{" a , b , c ", []string{"a", "b", "c"}},
		{"a,,b", []string{"a", "b"}},
	}

	for _, tt := range tests {
		_ = os.Setenv("TEST_CSV", tt.value)
		got := parseCSVEnv("TEST_CSV")
		if len(got) != len(tt.expected) {
			t.Errorf("parseCSVEnv(%q) = %v, want %v", tt.value, got, tt.expected)
			continue
		}
		for i := range got {
			if got[i] != tt.expected[i] {
				t.Errorf("parseCSVEnv(%q)[%d] = %q, want %q", tt.value, i, got[i], tt.expected[i])
			}
		}
	}
	_ = os.Unsetenv("TEST_CSV")
}

func TestIsCloudConfigured_GCP(t *testing.T) {
	tests := []struct {
		name     string
		config   *Config
		expected bool
	}{
		{
			name: "gcp with credentials and project ID",
			config: &Config{
				CloudEnabled:       true,
				CloudProvider:      "gcp",
				GCPCredentialsFile: "/path/to/creds.json",
				ProjectID:          "my-project",
			},
			expected: true,
		},
		{
			name: "gcp without project ID returns false",
			config: &Config{
				CloudEnabled:       true,
				CloudProvider:      "gcp",
				GCPCredentialsFile: "/path/to/creds.json",
			},
			expected: false,
		},
		{
			name: "gcp without credentials but with project ID (ADC fallback)",
			config: &Config{
				CloudEnabled:  true,
				CloudProvider: "gcp",
				ProjectID:     "my-project",
			},
			expected: true,
		},
		{
			name: "gcp disabled",
			config: &Config{
				CloudEnabled:       false,
				CloudProvider:      "gcp",
				GCPCredentialsFile: "/path/to/creds.json",
				ProjectID:          "my-project",
			},
			expected: false,
		},
		{
			name: "gcp with credentials and project ID, no endpoint is OK",
			config: &Config{
				CloudEnabled:       true,
				CloudProvider:      "gcp",
				GCPCredentialsFile: "/path/to/creds.json",
				ProjectID:          "my-project",
				Endpoint:           "", // no endpoint needed for GCP
			},
			expected: true,
		},
		{
			name: "gcp without credentials in metadata mode block returns false",
			config: &Config{
				CloudEnabled:  true,
				CloudProvider: "gcp",
				ProjectID:     "my-project",
				MetadataMode:  "block",
			},
			expected: false,
		},
		{
			name: "gcp with credentials in metadata mode block returns true",
			config: &Config{
				CloudEnabled:       true,
				CloudProvider:      "gcp",
				GCPCredentialsFile: "/path/to/creds.json",
				ProjectID:          "my-project",
				MetadataMode:       "block",
			},
			expected: true,
		},
		{
			name: "gcp without credentials in metadata mode assign returns true",
			config: &Config{
				CloudEnabled:  true,
				CloudProvider: "gcp",
				ProjectID:     "my-project",
				MetadataMode:  "assign",
			},
			expected: true,
		},
		{
			name: "gcp without credentials in metadata mode passthrough returns true",
			config: &Config{
				CloudEnabled:  true,
				CloudProvider: "gcp",
				ProjectID:     "my-project",
				MetadataMode:  "passthrough",
			},
			expected: true,
		},
		{
			name: "generic without endpoint",
			config: &Config{
				CloudEnabled: true,
			},
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.config.IsCloudConfigured(); got != tt.expected {
				t.Errorf("IsCloudConfigured() = %v, want %v", got, tt.expected)
			}
		})
	}
}

func TestIsGCP(t *testing.T) {
	tests := []struct {
		name     string
		config   *Config
		expected bool
	}{
		{
			name:     "nil config",
			config:   nil,
			expected: false,
		},
		{
			name: "gcp with creds",
			config: &Config{
				CloudProvider:      "gcp",
				GCPCredentialsFile: "/path/to/creds.json",
			},
			expected: true,
		},
		{
			name: "gcp without creds (ADC fallback)",
			config: &Config{
				CloudProvider: "gcp",
			},
			expected: true,
		},
		{
			name: "not gcp",
			config: &Config{
				CloudProvider:      "other",
				GCPCredentialsFile: "/path/to/creds.json",
			},
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.config.IsGCP(); got != tt.expected {
				t.Errorf("IsGCP() = %v, want %v", got, tt.expected)
			}
		})
	}
}

func TestExtractProjectIDFromCredentials(t *testing.T) {
	// Write a temp credentials file
	tmpFile, err := os.CreateTemp("", "gcp-creds-*.json")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Remove(tmpFile.Name()) }()

	credJSON := `{"type":"service_account","project_id":"test-project-123","private_key_id":"key"}`
	if _, err := tmpFile.WriteString(credJSON); err != nil {
		t.Fatal(err)
	}
	_ = tmpFile.Close()

	got := gcp.ExtractProjectID(tmpFile.Name())
	if got != "test-project-123" {
		t.Errorf("ExtractProjectID() = %q, want %q", got, "test-project-123")
	}

	// Non-existent file
	got = gcp.ExtractProjectID("/nonexistent/path.json")
	if got != "" {
		t.Errorf("ExtractProjectID(nonexistent) = %q, want empty", got)
	}

	// Invalid JSON
	badFile, _ := os.CreateTemp("", "bad-creds-*.json")
	defer func() { _ = os.Remove(badFile.Name()) }()
	_, _ = badFile.WriteString("not json")
	_ = badFile.Close()
	got = gcp.ExtractProjectID(badFile.Name())
	if got != "" {
		t.Errorf("ExtractProjectID(invalid) = %q, want empty", got)
	}
}

func TestLoadConfig_ProjectIDFromCredentials(t *testing.T) {
	clearTelemetryEnv()

	// Write a temp credentials file
	tmpFile, err := os.CreateTemp("", "gcp-creds-*.json")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Remove(tmpFile.Name()) }()

	credJSON := `{"type":"service_account","project_id":"creds-project"}`
	_, _ = tmpFile.WriteString(credJSON)
	_ = tmpFile.Close()

	// Set credentials file but NOT project ID or provider
	_ = os.Setenv(EnvGCPCredentials, tmpFile.Name())
	defer clearTelemetryEnv()

	cfg := LoadConfig()

	if cfg.ProjectID != "creds-project" {
		t.Errorf("Expected ProjectID auto-resolved from credentials, got %q", cfg.ProjectID)
	}

	// Provider should also be auto-detected
	if cfg.CloudProvider != "gcp" {
		t.Errorf("Expected CloudProvider auto-detected as 'gcp', got %q", cfg.CloudProvider)
	}
}

func TestLoadConfig_GCPAutoDetect(t *testing.T) {
	clearTelemetryEnv()

	// Write a temp credentials file
	tmpFile, err := os.CreateTemp("", "gcp-creds-*.json")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Remove(tmpFile.Name()) }()

	credJSON := `{"type":"service_account","project_id":"auto-project"}`
	_, _ = tmpFile.WriteString(credJSON)
	_ = tmpFile.Close()

	// Only set credentials file - no provider, no project ID, no endpoint
	_ = os.Setenv(EnvGCPCredentials, tmpFile.Name())
	defer clearTelemetryEnv()

	cfg := LoadConfig()

	// Should auto-detect GCP mode
	if !cfg.IsGCP() {
		t.Error("Expected IsGCP() = true when credentials file is present")
	}
	if !cfg.IsCloudConfigured() {
		t.Error("Expected IsCloudConfigured() = true in auto-detected GCP mode")
	}
	if cfg.ProjectID != "auto-project" {
		t.Errorf("Expected ProjectID = 'auto-project', got %q", cfg.ProjectID)
	}
}

func TestLoadConfig_ProjectIDEnvTakesPriority(t *testing.T) {
	clearTelemetryEnv()

	// Write a temp credentials file
	tmpFile, err := os.CreateTemp("", "gcp-creds-*.json")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Remove(tmpFile.Name()) }()

	credJSON := `{"type":"service_account","project_id":"creds-project"}`
	_, _ = tmpFile.WriteString(credJSON)
	_ = tmpFile.Close()

	// Set both env var and credentials file
	_ = os.Setenv(EnvProjectID, "env-project")
	_ = os.Setenv(EnvGCPCredentials, tmpFile.Name())
	defer clearTelemetryEnv()

	cfg := LoadConfig()

	// Explicit env var should win
	if cfg.ProjectID != "env-project" {
		t.Errorf("Expected ProjectID from env to take priority, got %q", cfg.ProjectID)
	}
}

func TestLoadConfig_GCPDefaults(t *testing.T) {
	clearTelemetryEnv()

	cfg := LoadConfig()

	// CloudProvider is auto-detected when a credentials file exists at the
	// well-known path. Only assert it is empty when no credentials are found.
	if cfg.GCPCredentialsFile == "" && cfg.CloudProvider != "" {
		t.Errorf("Expected CloudProvider to be empty when no GCP credentials, got %q", cfg.CloudProvider)
	}
}

func TestLoadConfig_WellKnownPathFallback(t *testing.T) {
	clearTelemetryEnv()

	// Create a temp home dir with the well-known credentials path
	tmpHome := t.TempDir()
	scionDir := filepath.Join(tmpHome, ".scion")
	if err := os.MkdirAll(scionDir, 0755); err != nil {
		t.Fatal(err)
	}
	credPath := filepath.Join(scionDir, "telemetry-gcp-credentials.json")
	credJSON := `{"type":"service_account","project_id":"wellknown-project"}`
	if err := os.WriteFile(credPath, []byte(credJSON), 0600); err != nil {
		t.Fatal(err)
	}

	// Override HOME so LoadConfig finds the well-known path
	origHome := os.Getenv("HOME")
	_ = os.Setenv("HOME", tmpHome)
	defer func() { _ = os.Setenv("HOME", origHome) }()

	cfg := LoadConfig()

	if cfg.GCPCredentialsFile != credPath {
		t.Errorf("Expected GCPCredentialsFile = %q (well-known fallback), got %q", credPath, cfg.GCPCredentialsFile)
	}
	if cfg.CloudProvider != "gcp" {
		t.Errorf("Expected CloudProvider auto-detected as 'gcp', got %q", cfg.CloudProvider)
	}
	if cfg.ProjectID != "wellknown-project" {
		t.Errorf("Expected ProjectID = 'wellknown-project', got %q", cfg.ProjectID)
	}
}

func TestLoadConfig_EnvTakesPriorityOverWellKnown(t *testing.T) {
	clearTelemetryEnv()

	// Create well-known path
	tmpHome := t.TempDir()
	scionDir := filepath.Join(tmpHome, ".scion")
	if err := os.MkdirAll(scionDir, 0755); err != nil {
		t.Fatal(err)
	}
	wellKnownPath := filepath.Join(scionDir, "telemetry-gcp-credentials.json")
	if err := os.WriteFile(wellKnownPath, []byte(`{"type":"service_account","project_id":"wk-project"}`), 0600); err != nil {
		t.Fatal(err)
	}

	// Also create a separate file for the env var
	envFile, err := os.CreateTemp("", "gcp-creds-*.json")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Remove(envFile.Name()) }()
	if _, err := envFile.WriteString(`{"type":"service_account","project_id":"env-project"}`); err != nil {
		t.Fatal(err)
	}
	_ = envFile.Close()

	origHome := os.Getenv("HOME")
	_ = os.Setenv("HOME", tmpHome)
	_ = os.Setenv(EnvGCPCredentials, envFile.Name())
	defer func() {
		_ = os.Setenv("HOME", origHome)
		clearTelemetryEnv()
	}()

	cfg := LoadConfig()

	// Env var should take priority over well-known path
	if cfg.GCPCredentialsFile != envFile.Name() {
		t.Errorf("Expected env var path %q to take priority, got %q", envFile.Name(), cfg.GCPCredentialsFile)
	}
	if cfg.ProjectID != "env-project" {
		t.Errorf("Expected ProjectID from env creds, got %q", cfg.ProjectID)
	}
}

func TestIsGCP_WithEndpointNoCreds(t *testing.T) {
	// Reproduces the scenario where provider=gcp and endpoint is set but no
	// credentials file is present. IsGCP must still return true so the code
	// uses GCP-native SDKs (which support ADC) instead of falling through to
	// generic OTLP against a non-OTLP endpoint like cloudtrace.googleapis.com.
	cfg := &Config{
		CloudEnabled:  true,
		CloudProvider: "gcp",
		ProjectID:     "my-project",
		Endpoint:      "cloudtrace.googleapis.com:443",
	}
	if !cfg.IsGCP() {
		t.Error("IsGCP() should return true when CloudProvider=gcp, even without credentials file")
	}
	if !cfg.IsCloudConfigured() {
		t.Error("IsCloudConfigured() should return true when CloudProvider=gcp with project ID, even without credentials file")
	}
}

func TestIsCloudConfigured_GCPRequiresProjectID(t *testing.T) {
	// GCP mode without a ProjectID should not be considered configured,
	// preventing indefinite retry loops when credentials exist but project
	// ID is missing.
	cfg := &Config{
		CloudEnabled:       true,
		CloudProvider:      "gcp",
		GCPCredentialsFile: "/path/to/creds.json",
	}
	if cfg.IsCloudConfigured() {
		t.Error("IsCloudConfigured() should return false when CloudProvider=gcp but ProjectID is empty")
	}
	cfg.ProjectID = "my-project"
	if !cfg.IsCloudConfigured() {
		t.Error("IsCloudConfigured() should return true when CloudProvider=gcp and ProjectID is set")
	}
}

func TestLoadConfig_GCPEnvOverrides(t *testing.T) {
	clearTelemetryEnv()

	_ = os.Setenv(EnvGCPCredentials, "/etc/gcp/sa.json")
	_ = os.Setenv(EnvCloudProvider, "gcp")
	defer clearTelemetryEnv()

	cfg := LoadConfig()

	if cfg.GCPCredentialsFile != "/etc/gcp/sa.json" {
		t.Errorf("Expected GCPCredentialsFile to be '/etc/gcp/sa.json', got %q", cfg.GCPCredentialsFile)
	}
	if cfg.CloudProvider != "gcp" {
		t.Errorf("Expected CloudProvider to be 'gcp', got %q", cfg.CloudProvider)
	}
}

func TestLoadConfig_GCPMetadataModeBlock(t *testing.T) {
	t.Run("block mode without credentials disables cloud export", func(t *testing.T) {
		clearTelemetryEnv()
		t.Cleanup(clearTelemetryEnv)
		t.Setenv("HOME", t.TempDir())
		t.Setenv(EnvCloudProvider, "gcp")
		t.Setenv(EnvProjectID, "my-project")
		t.Setenv(EnvMetadataMode, "block")

		cfg := LoadConfig()
		if cfg.IsCloudConfigured() {
			t.Error("IsCloudConfigured() = true, want false when metadata mode is block and no credentials file is present")
		}
		if AnalyticsEnabledForHooks(cfg) {
			t.Error("AnalyticsEnabledForHooks() = true, want false when cloud export is disabled in block mode")
		}
	})

	t.Run("block mode with explicit credentials env keeps cloud export enabled", func(t *testing.T) {
		clearTelemetryEnv()
		t.Cleanup(clearTelemetryEnv)
		t.Setenv("HOME", t.TempDir())
		t.Setenv(EnvCloudProvider, "gcp")
		t.Setenv(EnvProjectID, "my-project")
		t.Setenv(EnvGCPCredentials, "/etc/gcp/sa.json")
		t.Setenv(EnvMetadataMode, "block")

		cfg := LoadConfig()
		if !cfg.IsCloudConfigured() {
			t.Error("IsCloudConfigured() = false, want true when explicit credentials file is set in block mode")
		}
	})

	t.Run("block mode with well-known credentials file keeps cloud export enabled", func(t *testing.T) {
		clearTelemetryEnv()
		t.Cleanup(clearTelemetryEnv)
		tmpHome := t.TempDir()
		scionDir := filepath.Join(tmpHome, ".scion")
		if err := os.MkdirAll(scionDir, 0755); err != nil {
			t.Fatal(err)
		}
		credPath := filepath.Join(scionDir, "telemetry-gcp-credentials.json")
		if err := os.WriteFile(credPath, []byte(`{"type":"service_account","project_id":"wk-project"}`), 0600); err != nil {
			t.Fatal(err)
		}
		t.Setenv("HOME", tmpHome)
		t.Setenv(EnvCloudProvider, "gcp")
		t.Setenv(EnvMetadataMode, "block")

		cfg := LoadConfig()
		if !cfg.IsCloudConfigured() {
			t.Error("IsCloudConfigured() = false, want true when well-known credentials file exists in block mode")
		}
	})

	for _, mode := range []string{"assign", "passthrough", ""} {
		name := mode
		if name == "" {
			name = "unset"
		}
		t.Run("metadata mode "+name+" without credentials keeps ADC cloud export enabled", func(t *testing.T) {
			clearTelemetryEnv()
			t.Cleanup(clearTelemetryEnv)
			t.Setenv("HOME", t.TempDir())
			t.Setenv(EnvCloudProvider, "gcp")
			t.Setenv(EnvProjectID, "my-project")
			if mode != "" {
				t.Setenv(EnvMetadataMode, mode)
			} else {
				_ = os.Unsetenv(EnvMetadataMode)
			}

			cfg := LoadConfig()
			if !cfg.IsCloudConfigured() {
				t.Errorf("IsCloudConfigured() = false, want true for metadata mode %q with project ID", mode)
			}
		})
	}
}

func clearTelemetryEnv() {
	_ = os.Unsetenv(EnvEnabled)
	_ = os.Unsetenv(EnvCloudEnabled)
	_ = os.Unsetenv(EnvEndpoint)
	_ = os.Unsetenv(EnvProtocol)
	_ = os.Unsetenv(EnvInsecure)
	if err := os.Unsetenv(EnvCAFile); err != nil {
		panic(err)
	}
	_ = os.Unsetenv(EnvCertFile)
	_ = os.Unsetenv(EnvKeyFile)
	_ = os.Unsetenv(EnvGRPCPort)
	_ = os.Unsetenv(EnvHTTPPort)
	_ = os.Unsetenv(EnvFilterExclude)
	_ = os.Unsetenv(EnvFilterInclude)
	_ = os.Unsetenv(EnvProjectID)
	_ = os.Unsetenv(EnvGCPCredentials)
	_ = os.Unsetenv(EnvCloudProvider)
	_ = os.Unsetenv(EnvMetricsDebug)
	_ = os.Unsetenv(EnvMetadataMode)
	// Mark as sandboxed so LoadConfig's test guard doesn't force-disable cloud.
	telemetryTestSandboxed = true
}
