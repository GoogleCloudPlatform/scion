/*
Copyright 2025 The Scion Authors.
*/

package telemetry

import (
	"os"
	"path/filepath"
	"testing"
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
	if cfg.CertFile != "" {
		t.Errorf("Expected CertFile to be empty by default, got %q", cfg.CertFile)
	}
	if cfg.KeyFile != "" {
		t.Errorf("Expected KeyFile to be empty by default, got %q", cfg.KeyFile)
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

	os.Setenv(EnvEnabled, "false")
	os.Setenv(EnvCloudEnabled, "false")
	os.Setenv(EnvEndpoint, "otel.example.com:443")
	os.Setenv(EnvProtocol, "http")
	os.Setenv(EnvInsecure, "true")
	if err := os.Setenv(EnvCAFile, "/etc/ssl/certs/custom-root.pem"); err != nil {
		t.Fatalf("failed to set %s: %v", EnvCAFile, err)
	}
	if err := os.Setenv(EnvCertFile, "/etc/ssl/certs/client.pem"); err != nil {
		t.Fatalf("failed to set %s: %v", EnvCertFile, err)
	}
	if err := os.Setenv(EnvKeyFile, "/etc/ssl/private/client-key.pem"); err != nil {
		t.Fatalf("failed to set %s: %v", EnvKeyFile, err)
	}
	os.Setenv(EnvGRPCPort, "14317")
	os.Setenv(EnvHTTPPort, "14318")
	os.Setenv(EnvProjectID, "my-project")
	os.Setenv(EnvFilterExclude, "event.type.a,event.type.b")
	os.Setenv(EnvFilterInclude, "event.type.c")
	os.Setenv(EnvMetricsDebug, "true")
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

func TestParseBoolEnv(t *testing.T) {
	tests := []struct {
		value      string
		defaultVal bool
		expected   bool
	}{
		{"", true, true},
		{"", false, false},
		{"true", false, true},
		{"True", false, true},
		{"TRUE", false, true},
		{"1", false, true},
		{"yes", false, true},
		{"on", false, true},
		{"false", true, false},
		{"False", true, false},
		{"0", true, false},
		{"no", true, false},
		{"off", true, false},
		{"invalid", true, true},
		{"invalid", false, false},
	}

	for _, tt := range tests {
		os.Setenv("TEST_BOOL", tt.value)
		got := parseBoolEnv("TEST_BOOL", tt.defaultVal)
		if got != tt.expected {
			t.Errorf("parseBoolEnv(%q, %v) = %v, want %v", tt.value, tt.defaultVal, got, tt.expected)
		}
	}
	os.Unsetenv("TEST_BOOL")
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
		os.Setenv("TEST_CSV", tt.value)
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
	os.Unsetenv("TEST_CSV")
}

func TestIsCloudConfigured_GCP(t *testing.T) {
	tests := []struct {
		name     string
		config   *Config
		expected bool
	}{
		{
			name: "gcp with credentials",
			config: &Config{
				CloudEnabled:       true,
				CloudProvider:      "gcp",
				GCPCredentialsFile: "/path/to/creds.json",
			},
			expected: true,
		},
		{
			name: "gcp without credentials (ADC fallback)",
			config: &Config{
				CloudEnabled:  true,
				CloudProvider: "gcp",
			},
			expected: true,
		},
		{
			name: "gcp disabled",
			config: &Config{
				CloudEnabled:       false,
				CloudProvider:      "gcp",
				GCPCredentialsFile: "/path/to/creds.json",
			},
			expected: false,
		},
		{
			name: "gcp with credentials and no endpoint is OK",
			config: &Config{
				CloudEnabled:       true,
				CloudProvider:      "gcp",
				GCPCredentialsFile: "/path/to/creds.json",
				Endpoint:           "", // no endpoint needed for GCP
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

func TestReadProjectIDFromCredentials(t *testing.T) {
	// Write a temp credentials file
	tmpFile, err := os.CreateTemp("", "gcp-creds-*.json")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(tmpFile.Name())

	credJSON := `{"type":"service_account","project_id":"test-project-123","private_key_id":"key"}`
	if _, err := tmpFile.WriteString(credJSON); err != nil {
		t.Fatal(err)
	}
	tmpFile.Close()

	got := readProjectIDFromCredentials(tmpFile.Name())
	if got != "test-project-123" {
		t.Errorf("readProjectIDFromCredentials() = %q, want %q", got, "test-project-123")
	}

	// Non-existent file
	got = readProjectIDFromCredentials("/nonexistent/path.json")
	if got != "" {
		t.Errorf("readProjectIDFromCredentials(nonexistent) = %q, want empty", got)
	}

	// Invalid JSON
	badFile, _ := os.CreateTemp("", "bad-creds-*.json")
	defer os.Remove(badFile.Name())
	badFile.WriteString("not json")
	badFile.Close()
	got = readProjectIDFromCredentials(badFile.Name())
	if got != "" {
		t.Errorf("readProjectIDFromCredentials(invalid) = %q, want empty", got)
	}
}

func TestLoadConfig_ProjectIDFromCredentials(t *testing.T) {
	clearTelemetryEnv()

	// Write a temp credentials file
	tmpFile, err := os.CreateTemp("", "gcp-creds-*.json")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(tmpFile.Name())

	credJSON := `{"type":"service_account","project_id":"creds-project"}`
	tmpFile.WriteString(credJSON)
	tmpFile.Close()

	// Set credentials file but NOT project ID or provider
	os.Setenv(EnvGCPCredentials, tmpFile.Name())
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
	defer os.Remove(tmpFile.Name())

	credJSON := `{"type":"service_account","project_id":"auto-project"}`
	tmpFile.WriteString(credJSON)
	tmpFile.Close()

	// Only set credentials file - no provider, no project ID, no endpoint
	os.Setenv(EnvGCPCredentials, tmpFile.Name())
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
	defer os.Remove(tmpFile.Name())

	credJSON := `{"type":"service_account","project_id":"creds-project"}`
	tmpFile.WriteString(credJSON)
	tmpFile.Close()

	// Set both env var and credentials file
	os.Setenv(EnvProjectID, "env-project")
	os.Setenv(EnvGCPCredentials, tmpFile.Name())
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

	// Note: GCPCredentialsFile may be non-empty if the well-known path exists
	// in the test environment's home directory. Only assert CloudProvider is
	// empty when no credentials are present.
	if cfg.GCPCredentialsFile == "" && cfg.CloudProvider != "" {
		t.Errorf("Expected CloudProvider to be empty when no credentials, got %q", cfg.CloudProvider)
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
	os.Setenv("HOME", tmpHome)
	defer os.Setenv("HOME", origHome)

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
	os.MkdirAll(scionDir, 0755)
	wellKnownPath := filepath.Join(scionDir, "telemetry-gcp-credentials.json")
	os.WriteFile(wellKnownPath, []byte(`{"type":"service_account","project_id":"wk-project"}`), 0600)

	// Also create a separate file for the env var
	envFile, _ := os.CreateTemp("", "gcp-creds-*.json")
	defer os.Remove(envFile.Name())
	envFile.WriteString(`{"type":"service_account","project_id":"env-project"}`)
	envFile.Close()

	origHome := os.Getenv("HOME")
	os.Setenv("HOME", tmpHome)
	os.Setenv(EnvGCPCredentials, envFile.Name())
	defer func() {
		os.Setenv("HOME", origHome)
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
		Endpoint:      "cloudtrace.googleapis.com:443",
	}
	if !cfg.IsGCP() {
		t.Error("IsGCP() should return true when CloudProvider=gcp, even without credentials file")
	}
	if !cfg.IsCloudConfigured() {
		t.Error("IsCloudConfigured() should return true when CloudProvider=gcp, even without credentials file")
	}
}

func TestLoadConfig_GCPEnvOverrides(t *testing.T) {
	clearTelemetryEnv()

	os.Setenv(EnvGCPCredentials, "/etc/gcp/sa.json")
	os.Setenv(EnvCloudProvider, "gcp")
	defer clearTelemetryEnv()

	cfg := LoadConfig()

	if cfg.GCPCredentialsFile != "/etc/gcp/sa.json" {
		t.Errorf("Expected GCPCredentialsFile to be '/etc/gcp/sa.json', got %q", cfg.GCPCredentialsFile)
	}
	if cfg.CloudProvider != "gcp" {
		t.Errorf("Expected CloudProvider to be 'gcp', got %q", cfg.CloudProvider)
	}
}

func clearTelemetryEnv() {
	os.Unsetenv(EnvEnabled)
	os.Unsetenv(EnvCloudEnabled)
	os.Unsetenv(EnvEndpoint)
	os.Unsetenv(EnvProtocol)
	os.Unsetenv(EnvInsecure)
	if err := os.Unsetenv(EnvCAFile); err != nil {
		panic(err)
	}
	if err := os.Unsetenv(EnvCertFile); err != nil {
		panic(err)
	}
	if err := os.Unsetenv(EnvKeyFile); err != nil {
		panic(err)
	}
	os.Unsetenv(EnvGRPCPort)
	os.Unsetenv(EnvHTTPPort)
	os.Unsetenv(EnvFilterExclude)
	os.Unsetenv(EnvFilterInclude)
	os.Unsetenv(EnvProjectID)
	os.Unsetenv(EnvGCPCredentials)
	os.Unsetenv(EnvCloudProvider)
	os.Unsetenv(EnvMetricsDebug)
}
