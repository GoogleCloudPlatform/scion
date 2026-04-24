/*
Copyright 2025 The Scion Authors.
*/

// Package telemetry provides OTLP telemetry collection and forwarding for sciontool.
// It enables agents to collect and forward traces to Google Cloud backend.
package telemetry

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Environment variable names for telemetry configuration.
const (
	// EnvEnabled controls whether telemetry collection is enabled.
	EnvEnabled = "SCION_TELEMETRY_ENABLED"
	// EnvCloudEnabled controls whether telemetry is forwarded to cloud backend.
	EnvCloudEnabled = "SCION_TELEMETRY_CLOUD_ENABLED"
	// EnvEndpoint is the cloud OTLP endpoint (required for cloud forwarding).
	EnvEndpoint = "SCION_OTEL_ENDPOINT"
	// EnvProtocol is the OTLP protocol to use (grpc or http).
	EnvProtocol = "SCION_OTEL_PROTOCOL"
	// EnvInsecure controls whether TLS verification is skipped.
	EnvInsecure = "SCION_OTEL_INSECURE"
	// EnvCAFile is the path to a PEM-encoded CA bundle for OTLP TLS.
	EnvCAFile = "SCION_OTEL_CA_FILE"
	// EnvCertFile is the path to a PEM-encoded client certificate for OTLP mTLS.
	EnvCertFile = "SCION_OTEL_CERT_FILE"
	// EnvKeyFile is the path to a PEM-encoded client private key for OTLP mTLS.
	EnvKeyFile = "SCION_OTEL_KEY_FILE"
	// EnvGRPCPort is the local gRPC receiver port.
	EnvGRPCPort = "SCION_OTEL_GRPC_PORT"
	// EnvHTTPPort is the local HTTP receiver port.
	EnvHTTPPort = "SCION_OTEL_HTTP_PORT"
	// EnvFilterExclude is a comma-separated list of event types to exclude.
	EnvFilterExclude = "SCION_TELEMETRY_FILTER_EXCLUDE"
	// EnvFilterInclude is a comma-separated list of event types to include.
	EnvFilterInclude = "SCION_TELEMETRY_FILTER_INCLUDE"
	// EnvProjectID is the GCP project ID for the exporter.
	EnvProjectID = "SCION_GCP_PROJECT_ID"
	// EnvRedactFields is a comma-separated list of fields to redact.
	EnvRedactFields = "SCION_TELEMETRY_REDACT"
	// EnvHashFields is a comma-separated list of fields to hash.
	EnvHashFields = "SCION_TELEMETRY_HASH"
	// EnvGCPCredentials is the path to a GCP service account key file
	// for authenticating OTLP exports. Injected by the broker runtime.
	EnvGCPCredentials = "SCION_OTEL_GCP_CREDENTIALS"
	// EnvCloudProvider identifies the cloud telemetry backend (e.g. "gcp").
	EnvCloudProvider = "SCION_TELEMETRY_CLOUD_PROVIDER"
	// EnvMetricsDebug enables verbose metrics normalization/export debugging.
	EnvMetricsDebug = "SCION_TELEMETRY_METRICS_DEBUG"
)

// Default configuration values.
const (
	DefaultGRPCPort = 4317
	DefaultHTTPPort = 4318
	DefaultProtocol = "grpc"
)

// Default event types to exclude for privacy.
var DefaultFilterExclude = []string{"agent.user.prompt"}

// Default fields to redact for privacy.
var DefaultRedactFields = []string{"prompt", "user.email", "tool_output", "tool_input"}

// Default fields to hash for privacy while maintaining correlation.
var DefaultHashFields = []string{"session_id"}

// Config holds the telemetry configuration.
type Config struct {
	// Enabled controls whether telemetry collection is active.
	Enabled bool
	// CloudEnabled controls whether data is forwarded to cloud backend.
	CloudEnabled bool
	// Endpoint is the cloud OTLP endpoint.
	Endpoint string
	// Protocol is the OTLP protocol ("grpc" or "http").
	Protocol string
	// Insecure skips TLS verification if true.
	Insecure bool
	// CAFile is the path to a PEM-encoded CA bundle for OTLP TLS.
	CAFile string
	// CertFile is the path to a PEM-encoded client certificate for OTLP mTLS.
	CertFile string
	// KeyFile is the path to a PEM-encoded client private key for OTLP mTLS.
	KeyFile string
	// GRPCPort is the local gRPC receiver port.
	GRPCPort int
	// HTTPPort is the local HTTP receiver port.
	HTTPPort int
	// ProjectID is the GCP project ID for the exporter.
	ProjectID string
	// Filter contains the filtering configuration.
	Filter FilterConfig
	// Redaction contains the redaction/hashing configuration.
	Redaction RedactionConfig
	// GCPCredentialsFile is the path to a GCP service account key file.
	GCPCredentialsFile string
	// CloudProvider identifies the cloud telemetry backend (e.g. "gcp").
	CloudProvider string
	// MetricsDebug enables verbose metric normalization and export logging.
	MetricsDebug bool
}

// FilterConfig holds include/exclude patterns for event filtering.
type FilterConfig struct {
	// Include is a list of event types to include (if empty, include all).
	Include []string
	// Exclude is a list of event types to exclude (always applied after include).
	Exclude []string
}

// WellKnownGCPCredentialsPath is the conventional path inside agent containers
// for the GCP telemetry service account key file. The broker writes this file
// via the secrets system and sets SCION_OTEL_GCP_CREDENTIALS to point to it.
// As a fallback, LoadConfig probes this path when the env var is absent.
const WellKnownGCPCredentialsPath = ".scion/telemetry-gcp-credentials.json"

// LoadConfig loads telemetry configuration from environment variables.
func LoadConfig() *Config {
	cfg := &Config{
		Enabled:      parseBoolEnv(EnvEnabled, true),
		CloudEnabled: parseBoolEnv(EnvCloudEnabled, true),
		Endpoint:     os.Getenv(EnvEndpoint),
		Protocol:     getEnvOrDefault(EnvProtocol, DefaultProtocol),
		Insecure:     parseBoolEnv(EnvInsecure, false),
		CAFile:       os.Getenv(EnvCAFile),
		CertFile:     os.Getenv(EnvCertFile),
		KeyFile:      os.Getenv(EnvKeyFile),
		GRPCPort:     parseIntEnv(EnvGRPCPort, DefaultGRPCPort),
		HTTPPort:     parseIntEnv(EnvHTTPPort, DefaultHTTPPort),
		ProjectID:    os.Getenv(EnvProjectID),
		Filter: FilterConfig{
			Include: parseCSVEnv(EnvFilterInclude),
			Exclude: parseCSVEnv(EnvFilterExclude),
		},
		Redaction: RedactionConfig{
			Redact: parseCSVEnv(EnvRedactFields),
			Hash:   parseCSVEnv(EnvHashFields),
		},
		GCPCredentialsFile: os.Getenv(EnvGCPCredentials),
		CloudProvider:      os.Getenv(EnvCloudProvider),
		MetricsDebug:       parseBoolEnv(EnvMetricsDebug, false),
	}

	// Fallback: if SCION_OTEL_GCP_CREDENTIALS is not set, probe the
	// well-known path where the broker mounts the telemetry SA key file.
	// This handles cases where the file is present (e.g. mounted via a
	// volume or template) but the env var was not injected — for example,
	// when the secret is not in ResolvedSecrets but the file exists from
	// a prior provisioning step or manual placement.
	if cfg.GCPCredentialsFile == "" {
		if home, err := os.UserHomeDir(); err == nil {
			wellKnown := filepath.Join(home, WellKnownGCPCredentialsPath)
			if _, err := os.Stat(wellKnown); err == nil {
				cfg.GCPCredentialsFile = wellKnown
			}
		}
	}

	// Auto-detect GCP provider when credentials file is present but provider
	// is not explicitly set. The presence of SCION_OTEL_GCP_CREDENTIALS is a
	// strong signal that GCP-native export should be used.
	if cfg.CloudProvider == "" && cfg.GCPCredentialsFile != "" {
		cfg.CloudProvider = "gcp"
	}

	// Auto-resolve project ID from GCP credentials file if not explicitly set
	if cfg.ProjectID == "" && cfg.GCPCredentialsFile != "" {
		cfg.ProjectID = readProjectIDFromCredentials(cfg.GCPCredentialsFile)
	}

	// Apply default exclude list if not explicitly set
	if len(cfg.Filter.Exclude) == 0 && os.Getenv(EnvFilterExclude) == "" {
		cfg.Filter.Exclude = DefaultFilterExclude
	}

	// Apply default redact fields if not explicitly set
	if len(cfg.Redaction.Redact) == 0 && os.Getenv(EnvRedactFields) == "" {
		cfg.Redaction.Redact = DefaultRedactFields
	}

	// Apply default hash fields if not explicitly set
	if len(cfg.Redaction.Hash) == 0 && os.Getenv(EnvHashFields) == "" {
		cfg.Redaction.Hash = DefaultHashFields
	}

	return cfg
}

// MetricsDebugEnabled returns true when verbose metrics debugging is enabled.
func MetricsDebugEnabled() bool {
	return parseBoolEnv(EnvMetricsDebug, false)
}

// IsCloudConfigured returns true if cloud forwarding is properly configured.
// For GCP provider, no endpoint is required — GCP-native SDKs handle routing
// and support Application Default Credentials (ADC) when no explicit
// credentials file is provided.
// For generic OTLP, an endpoint must be specified.
func (c *Config) IsCloudConfigured() bool {
	if c == nil {
		return false
	}
	if !c.CloudEnabled {
		return false
	}
	// GCP mode: SDKs handle endpoints and auth (ADC or explicit credentials)
	if c.CloudProvider == "gcp" {
		return true
	}
	// Generic OTLP mode: endpoint is required
	return c.Endpoint != ""
}

// IsGCP returns true if the cloud provider is configured for GCP-native export.
// GCP-native mode uses Cloud Trace, Cloud Monitoring, and Cloud Logging SDKs
// directly, which support both explicit credentials files and Application
// Default Credentials (ADC). An explicit credentials file is not required.
func (c *Config) IsGCP() bool {
	return c != nil && c.CloudProvider == "gcp"
}

// readProjectIDFromCredentials reads the project_id field from a GCP service
// account credentials JSON file. Returns empty string on any error.
func readProjectIDFromCredentials(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	var creds struct {
		ProjectID string `json:"project_id"`
	}
	if err := json.Unmarshal(data, &creds); err != nil {
		return ""
	}
	return creds.ProjectID
}

// parseBoolEnv parses a boolean environment variable with a default value.
func parseBoolEnv(key string, defaultVal bool) bool {
	val := os.Getenv(key)
	if val == "" {
		return defaultVal
	}
	val = strings.ToLower(val)
	switch val {
	case "true", "1", "yes", "on":
		return true
	case "false", "0", "no", "off":
		return false
	default:
		return defaultVal
	}
}

// parseIntEnv parses an integer environment variable with a default value.
func parseIntEnv(key string, defaultVal int) int {
	val := os.Getenv(key)
	if val == "" {
		return defaultVal
	}
	intVal, err := strconv.Atoi(val)
	if err != nil {
		return defaultVal
	}
	return intVal
}

// getEnvOrDefault returns the environment variable value or a default.
func getEnvOrDefault(key, defaultVal string) string {
	val := os.Getenv(key)
	if val == "" {
		return defaultVal
	}
	return val
}

// parseCSVEnv parses a comma-separated environment variable into a slice.
func parseCSVEnv(key string) []string {
	val := os.Getenv(key)
	if val == "" {
		return nil
	}
	parts := strings.Split(val, ",")
	result := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			result = append(result, p)
		}
	}
	return result
}
