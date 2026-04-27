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
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/knadh/koanf/parsers/yaml"
	"github.com/knadh/koanf/providers/confmap"
	"github.com/knadh/koanf/providers/env"
	"github.com/knadh/koanf/providers/file"
	"github.com/knadh/koanf/v2"
	yamlv3 "gopkg.in/yaml.v3"
)

// HubServerConfig holds configuration for the Hub API server.
type HubServerConfig struct {
	Port         int           `json:"port" yaml:"port" koanf:"port"`
	Host         string        `json:"host" yaml:"host" koanf:"host"`
	ReadTimeout  time.Duration `json:"readTimeout" yaml:"readTimeout" koanf:"readTimeout"`
	WriteTimeout time.Duration `json:"writeTimeout" yaml:"writeTimeout" koanf:"writeTimeout"`

	// Endpoint is the public-facing URL for this Hub (e.g., "https://hub.example.com").
	// This is passed to agents so they know where to report status updates.
	// If empty, agents won't be able to call back to the Hub.
	Endpoint string `json:"endpoint" yaml:"endpoint" koanf:"endpoint"`

	// CORS settings
	CORSEnabled        bool     `json:"corsEnabled" yaml:"corsEnabled" koanf:"corsEnabled"`
	CORSAllowedOrigins []string `json:"corsAllowedOrigins" yaml:"corsAllowedOrigins" koanf:"corsAllowedOrigins"`
	CORSAllowedMethods []string `json:"corsAllowedMethods" yaml:"corsAllowedMethods" koanf:"corsAllowedMethods"`
	CORSAllowedHeaders []string `json:"corsAllowedHeaders" yaml:"corsAllowedHeaders" koanf:"corsAllowedHeaders"`
	CORSMaxAge         int      `json:"corsMaxAge" yaml:"corsMaxAge" koanf:"corsMaxAge"`

	// AdminEmails is a list of email addresses to auto-promote to admin role.
	AdminEmails []string `json:"adminEmails" yaml:"adminEmails" koanf:"adminEmails"`

	// SoftDeleteRetention is how long soft-deleted agents are retained before purging.
	// Zero means soft-delete is disabled (hard-delete immediately).
	SoftDeleteRetention time.Duration `json:"softDeleteRetention" yaml:"softDeleteRetention" koanf:"softDeleteRetention"`

	// SoftDeleteRetainFiles controls whether workspace files are preserved during soft-delete.
	// When true, the broker skips file cleanup for soft-deleted agents.
	SoftDeleteRetainFiles bool `json:"softDeleteRetainFiles" yaml:"softDeleteRetainFiles" koanf:"softDeleteRetainFiles"`

	// HubID is a unique identifier for this hub instance.
	// Used to namespace secrets and other hub-scoped resources in shared GCP projects.
	// Defaults to sha256(hostname)[:12] if not set.
	HubID string `json:"hubId" yaml:"hubId" koanf:"hubId"`

	// GCPProjectID is the GCP project ID used for minting service accounts.
	// If empty, auto-detected from the metadata server when running on GCE/Cloud Run.
	GCPProjectID string `json:"gcpProjectId,omitempty" yaml:"gcpProjectId,omitempty" koanf:"gcpProjectId"`
}

// DefaultHubID generates a deterministic hub instance ID from the machine hostname.
// The ID is a 12-character hex string derived from SHA-256 of the hostname.
func DefaultHubID() string {
	hostname, err := os.Hostname()
	if err != nil {
		hostname = "unknown"
	}
	h := sha256.Sum256([]byte(hostname))
	return hex.EncodeToString(h[:6]) // 12 hex chars
}

// ResolveHubID returns the configured HubID if set, otherwise generates one from hostname.
func (c *HubServerConfig) ResolveHubID() string {
	if c.HubID != "" {
		return c.HubID
	}
	return DefaultHubID()
}

// RuntimeBrokerConfig holds configuration for the Runtime Broker API server.
type RuntimeBrokerConfig struct {
	// Enabled indicates whether the Runtime Broker API is enabled
	Enabled bool `json:"enabled" yaml:"enabled" koanf:"enabled"`
	// Port is the HTTP port to listen on (default 9800)
	Port int `json:"port" yaml:"port" koanf:"port"`
	// Host is the address to bind to (e.g., "0.0.0.0" or "127.0.0.1")
	Host string `json:"host" yaml:"host" koanf:"host"`
	// ReadTimeout is the maximum duration for reading the entire request
	ReadTimeout time.Duration `json:"readTimeout" yaml:"readTimeout" koanf:"readTimeout"`
	// WriteTimeout is the maximum duration before timing out writes
	WriteTimeout time.Duration `json:"writeTimeout" yaml:"writeTimeout" koanf:"writeTimeout"`

	// HubEndpoint is the Hub API endpoint for status reporting (when Hub not co-located)
	HubEndpoint string `json:"hubEndpoint" yaml:"hubEndpoint" koanf:"hubEndpoint"`

	// ContainerHubEndpoint overrides HubEndpoint when injecting the Hub URL
	// into agent containers. Use this when agents inside containers cannot
	// reach the Hub at the same address as the broker (e.g. localhost vs
	// host.containers.internal for local development).
	ContainerHubEndpoint string `json:"containerHubEndpoint" yaml:"containerHubEndpoint" koanf:"containerHubEndpoint"`

	// BrokerID is a unique identifier for this runtime broker (auto-generated if empty)
	BrokerID string `json:"brokerId" yaml:"brokerId" koanf:"brokerId"`
	// BrokerName is a human-readable name for this runtime broker
	BrokerName string `json:"brokerName" yaml:"brokerName" koanf:"brokerName"`

	// CORS settings
	CORSEnabled        bool     `json:"corsEnabled" yaml:"corsEnabled" koanf:"corsEnabled"`
	CORSAllowedOrigins []string `json:"corsAllowedOrigins" yaml:"corsAllowedOrigins" koanf:"corsAllowedOrigins"`
	CORSAllowedMethods []string `json:"corsAllowedMethods" yaml:"corsAllowedMethods" koanf:"corsAllowedMethods"`
	CORSAllowedHeaders []string `json:"corsAllowedHeaders" yaml:"corsAllowedHeaders" koanf:"corsAllowedHeaders"`
	CORSMaxAge         int      `json:"corsMaxAge" yaml:"corsMaxAge" koanf:"corsMaxAge"`

	// AllowContainerScriptHarnesses controls whether the broker accepts
	// dispatches whose harness-config declares
	// `provisioner.type: container-script`. Defaults to false; the broker
	// rejects container-script dispatches with 403 until enabled. See
	// .design/decoupled-harness-implementation.md (Phase 3) for the policy
	// rationale.
	AllowContainerScriptHarnesses bool `json:"allowContainerScriptHarnesses" yaml:"allowContainerScriptHarnesses" koanf:"allowContainerScriptHarnesses"`
}

// DatabaseConfig holds database connection settings.
type DatabaseConfig struct {
	Driver string `json:"driver" yaml:"driver" koanf:"driver"` // sqlite, postgres
	URL    string `json:"url" yaml:"url" koanf:"url"`          // Connection URL/path
}

// DevAuthConfig holds development authentication settings.
type DevAuthConfig struct {
	// Enabled indicates whether development authentication is enabled.
	// WARNING: Not for production use.
	Enabled bool `json:"devMode" yaml:"devMode" koanf:"devMode"`
	// Token is an explicitly configured development token.
	// If empty and Enabled=true, a token is auto-generated and persisted.
	Token string `json:"devToken" yaml:"devToken" koanf:"devToken"`
	// TokenFile is the path to the token file (default: ~/.scion/dev-token).
	TokenFile string `json:"devTokenFile" yaml:"devTokenFile" koanf:"devTokenFile"`
	// AuthorizedDomains is a list of email domains allowed to authenticate.
	// If empty, all domains are allowed.
	AuthorizedDomains []string `json:"authorizedDomains" yaml:"authorizedDomains" koanf:"authorizedDomains"`
}

// OAuthProviderConfig holds OAuth credentials for a single provider.
type OAuthProviderConfig struct {
	// ClientID is the OAuth application client ID.
	ClientID string `json:"clientId" yaml:"clientId" koanf:"clientId"`
	// ClientSecret is the OAuth application client secret.
	ClientSecret string `json:"clientSecret" yaml:"clientSecret" koanf:"clientSecret"`
}

// OAuthClientConfig holds OAuth provider configurations for a specific client type.
type OAuthClientConfig struct {
	// Google OAuth settings for this client type.
	Google OAuthProviderConfig `json:"google" yaml:"google" koanf:"google"`
	// GitHub OAuth settings for this client type.
	GitHub OAuthProviderConfig `json:"github" yaml:"github" koanf:"github"`
}

// OAuthConfig holds OAuth provider configurations.
// Web, CLI, and Device use separate OAuth clients due to different redirect URI requirements.
type OAuthConfig struct {
	// Web OAuth client settings (for web frontend flows).
	Web OAuthClientConfig `json:"web" yaml:"web" koanf:"web"`
	// CLI OAuth client settings (for CLI localhost callback flows).
	CLI OAuthClientConfig `json:"cli" yaml:"cli" koanf:"cli"`
	// Device OAuth client settings (for device authorization grant / headless flows).
	Device OAuthClientConfig `json:"device" yaml:"device" koanf:"device"`
}

// GlobalConfig holds the complete server configuration.
// This is distinct from hub.ServerConfig which only holds HTTP server settings.
type GlobalConfig struct {
	// Mode selects the server operating mode: "workstation" (default) or "production".
	// When set to "production" in settings.yaml, the server behaves as if --production were passed.
	Mode string `json:"mode,omitempty" yaml:"mode,omitempty" koanf:"mode"`

	// Hub API server settings
	Hub HubServerConfig `json:"hub" yaml:"hub" koanf:"hub"`

	// Runtime Broker API server settings
	RuntimeBroker RuntimeBrokerConfig `json:"runtimeBroker" yaml:"runtimeBroker" koanf:"runtimeBroker"`

	// Database settings
	Database DatabaseConfig `json:"database" yaml:"database" koanf:"database"`

	// Authentication settings
	Auth DevAuthConfig `json:"auth" yaml:"auth" koanf:"auth"`

	// OAuth provider settings
	OAuth OAuthConfig `json:"oauth" yaml:"oauth" koanf:"oauth"`

	// Storage settings
	Storage StorageConfig `json:"storage" yaml:"storage" koanf:"storage"`

	// Secrets backend settings
	Secrets SecretsConfig `json:"secrets" yaml:"secrets" koanf:"secrets"`

	// Logging settings
	LogLevel  string `json:"logLevel" yaml:"logLevel" koanf:"logLevel"`
	LogFormat string `json:"logFormat" yaml:"logFormat" koanf:"logFormat"` // text, json

	// Admin mode settings
	AdminMode          bool   `json:"adminMode" yaml:"adminMode" koanf:"adminMode"`
	MaintenanceMessage string `json:"maintenanceMessage" yaml:"maintenanceMessage" koanf:"maintenanceMessage"`

	// Telemetry default — when set, the Hub exposes this as the default telemetry opt-in
	// state for new agents via GET /api/v1/settings/public.
	TelemetryEnabled *bool `json:"telemetryEnabled,omitempty" yaml:"telemetryEnabled,omitempty" koanf:"telemetryEnabled"`

	// TelemetryConfig holds the full telemetry configuration from settings.yaml.
	// Used to populate default telemetry config on new agents.
	TelemetryConfig *V1TelemetryConfig `json:"-" yaml:"-" koanf:"-"`

	// GitHub App settings
	GitHubApp GitHubAppConfig `json:"githubApp" yaml:"githubApp" koanf:"githubApp"`
}

// GitHubAppConfig holds configuration for the Hub's GitHub App integration.
type GitHubAppConfig struct {
	AppID           int64  `json:"appId" yaml:"appId" koanf:"appId"`
	PrivateKeyPath  string `json:"privateKeyPath,omitempty" yaml:"privateKeyPath,omitempty" koanf:"privateKeyPath"`
	PrivateKey      string `json:"privateKey,omitempty" yaml:"privateKey,omitempty" koanf:"privateKey"`
	WebhookSecret   string `json:"webhookSecret,omitempty" yaml:"webhookSecret,omitempty" koanf:"webhookSecret"`
	APIBaseURL      string `json:"apiBaseUrl,omitempty" yaml:"apiBaseUrl,omitempty" koanf:"apiBaseUrl"`
	WebhooksEnabled bool   `json:"webhooksEnabled,omitempty" yaml:"webhooksEnabled,omitempty" koanf:"webhooksEnabled"`
	InstallationURL string `json:"installationUrl,omitempty" yaml:"installationUrl,omitempty" koanf:"installationUrl"`
}

// SecretsConfig holds configuration for the secrets backend.
type SecretsConfig struct {
	// Backend selects the secret storage backend: "local" (default) or "gcpsm".
	Backend string `json:"backend" yaml:"backend" koanf:"backend"`
	// GCPProjectID is the GCP project ID for the GCP Secret Manager backend.
	GCPProjectID string `json:"gcpProjectId" yaml:"gcpProjectId" koanf:"gcpProjectId"`
	// GCPCredentials is the path to GCP credentials JSON or the JSON itself.
	GCPCredentials string `json:"gcpCredentials" yaml:"gcpCredentials" koanf:"gcpCredentials"`
}

// StorageConfig holds storage settings.
type StorageConfig struct {
	Provider  string `json:"provider" yaml:"provider" koanf:"provider"`
	Bucket    string `json:"bucket" yaml:"bucket" koanf:"bucket"`
	LocalPath string `json:"localPath" yaml:"localPath" koanf:"localPath"`
}

// DefaultGlobalConfig returns the default global configuration.
func DefaultGlobalConfig() GlobalConfig {
	return GlobalConfig{
		Hub: HubServerConfig{
			Port:               9810,
			Host:               "0.0.0.0",
			ReadTimeout:        30 * time.Second,
			WriteTimeout:       60 * time.Second,
			CORSEnabled:        true,
			CORSAllowedOrigins: []string{"*"},
			CORSAllowedMethods: []string{"GET", "POST", "PUT", "PATCH", "DELETE", "OPTIONS"},
			CORSAllowedHeaders: []string{"Authorization", "Content-Type", "X-Scion-Broker-Token", "X-Scion-Agent-Token", "X-API-Key"},
			CORSMaxAge:         3600,
			AdminEmails:        []string{},
		},
		RuntimeBroker: RuntimeBrokerConfig{
			Enabled:            false,
			Port:               9800,
			Host:               "0.0.0.0",
			ReadTimeout:        30 * time.Second,
			WriteTimeout:       120 * time.Second, // Longer for agent operations
			CORSEnabled:        true,
			CORSAllowedOrigins: []string{"*"},
			CORSAllowedMethods: []string{"GET", "POST", "PUT", "PATCH", "DELETE", "OPTIONS"},
			CORSAllowedHeaders: []string{"Authorization", "Content-Type", "X-Scion-Broker-Token", "X-API-Key"},
			CORSMaxAge:         3600,
		},
		Database: DatabaseConfig{
			Driver: "sqlite",
			URL:    "", // Will be set to default path if empty
		},
		Auth: DevAuthConfig{
			Enabled:   false,
			Token:     "",
			TokenFile: "", // Will default to ~/.scion/dev-token
		},
		Storage: StorageConfig{
			Provider: "local",
		},
		Secrets: SecretsConfig{
			Backend: "local",
		},
		LogLevel:  "info",
		LogFormat: "text",
	}
}

// LoadGlobalConfig loads global configuration using Koanf with priority:
// 1. Embedded defaults
// 2. Global config: settings.yaml (server key) OR server.yaml (~/.scion/)
// 3. Local config: settings.yaml (server key) OR server.yaml (./server.yaml or specified path)
// 4. Environment variables (SCION_SERVER_ prefix)
//
// If settings.yaml contains a "server" key, it is preferred over server.yaml.
// If both exist in the same directory, a deprecation warning is emitted to stderr.
func LoadGlobalConfig(configPath string) (*GlobalConfig, error) {
	// Try loading from settings.yaml first (versioned path)
	if gc, ok := loadGlobalConfigFromSettings(configPath); ok {
		return gc, nil
	}

	// Fall back to legacy server.yaml path
	return loadGlobalConfigLegacy(configPath)
}

// loadGlobalConfigFromSettings attempts to load server config from settings.yaml files.
// Returns (config, true) if settings.yaml had a server key, (nil, false) otherwise.
func loadGlobalConfigFromSettings(configPath string) (*GlobalConfig, bool) {
	// Check global settings.yaml
	globalDir, err := GetGlobalDir()
	if err != nil {
		return nil, false
	}

	gc, found := loadServerFromSettingsFile(globalDir)
	if !found {
		// Also check local path
		if configPath != "" {
			info, err := os.Stat(configPath)
			if err == nil {
				dir := configPath
				if !info.IsDir() {
					dir = filepath.Dir(configPath)
				}
				gc, found = loadServerFromSettingsFile(dir)
			}
		}
	}

	if !found {
		return nil, false
	}

	// Emit deprecation warning if server.yaml also exists
	if hasServerYAML(globalDir) {
		fmt.Fprintf(os.Stderr, "Warning: Both settings.yaml (server key) and server.yaml exist in %s. Using settings.yaml. server.yaml is deprecated; run 'scion config migrate --server' to consolidate.\n", globalDir)
	}
	if configPath != "" {
		info, err := os.Stat(configPath)
		if err == nil {
			dir := configPath
			if !info.IsDir() {
				dir = filepath.Dir(configPath)
			}
			if dir != globalDir && hasServerYAML(dir) {
				fmt.Fprintf(os.Stderr, "Warning: Both settings.yaml (server key) and server.yaml exist in %s. Using settings.yaml. server.yaml is deprecated.\n", dir)
			}
		}
	}

	// Apply environment variable overrides (SCION_SERVER_ prefix).
	// Without this, env vars are ignored when config comes from settings.yaml.
	if err := applyEnvOverrides(gc); err != nil {
		return nil, false
	}

	// Apply database URL default if needed
	if gc.Database.URL == "" && gc.Database.Driver == "sqlite" {
		gc.Database.URL = filepath.Join(globalDir, "hub.db")
	}

	return gc, true
}

// loadGlobalConfigLegacy loads global configuration from server.yaml files using the legacy path.
func loadGlobalConfigLegacy(configPath string) (*GlobalConfig, error) {
	k := koanf.New(".")

	// 1. Load embedded defaults
	defaults := DefaultGlobalConfig()
	if err := k.Load(confmap.Provider(map[string]interface{}{
		"hub.port":               defaults.Hub.Port,
		"hub.host":               defaults.Hub.Host,
		"hub.readTimeout":        defaults.Hub.ReadTimeout,
		"hub.writeTimeout":       defaults.Hub.WriteTimeout,
		"hub.corsEnabled":        defaults.Hub.CORSEnabled,
		"hub.corsAllowedOrigins": defaults.Hub.CORSAllowedOrigins,
		"hub.corsAllowedMethods": defaults.Hub.CORSAllowedMethods,
		"hub.corsAllowedHeaders": defaults.Hub.CORSAllowedHeaders,
		"hub.corsMaxAge":         defaults.Hub.CORSMaxAge,
		// RuntimeBroker defaults
		"runtimeBroker.enabled":            defaults.RuntimeBroker.Enabled,
		"runtimeBroker.port":               defaults.RuntimeBroker.Port,
		"runtimeBroker.host":               defaults.RuntimeBroker.Host,
		"runtimeBroker.readTimeout":        defaults.RuntimeBroker.ReadTimeout,
		"runtimeBroker.writeTimeout":       defaults.RuntimeBroker.WriteTimeout,
		"runtimeBroker.corsEnabled":        defaults.RuntimeBroker.CORSEnabled,
		"runtimeBroker.corsAllowedOrigins": defaults.RuntimeBroker.CORSAllowedOrigins,
		"runtimeBroker.corsAllowedMethods": defaults.RuntimeBroker.CORSAllowedMethods,
		"runtimeBroker.corsAllowedHeaders": defaults.RuntimeBroker.CORSAllowedHeaders,
		"runtimeBroker.corsMaxAge":         defaults.RuntimeBroker.CORSMaxAge,
		// Database defaults
		"database.driver": defaults.Database.Driver,
		"database.url":    defaults.Database.URL,
		// Auth defaults
		"auth.devMode":           defaults.Auth.Enabled,
		"auth.devToken":          defaults.Auth.Token,
		"auth.devTokenFile":      defaults.Auth.TokenFile,
		"auth.authorizedDomains": []string{},
		// OAuth defaults (empty by default, loaded from env/config)
		// Web OAuth client config
		"oauth.web.google.clientId":     "",
		"oauth.web.google.clientSecret": "",
		"oauth.web.github.clientId":     "",
		"oauth.web.github.clientSecret": "",
		// CLI OAuth client config
		"oauth.cli.google.clientId":     "",
		"oauth.cli.google.clientSecret": "",
		"oauth.cli.github.clientId":     "",
		"oauth.cli.github.clientSecret": "",
		// Device OAuth client config
		"oauth.device.google.clientId":     "",
		"oauth.device.google.clientSecret": "",
		"oauth.device.github.clientId":     "",
		"oauth.device.github.clientSecret": "",
		// Storage defaults
		"storage.provider":  defaults.Storage.Provider,
		"storage.bucket":    defaults.Storage.Bucket,
		"storage.localPath": defaults.Storage.LocalPath,
		// Secrets backend defaults
		"secrets.backend":        defaults.Secrets.Backend,
		"secrets.gcpProjectId":   defaults.Secrets.GCPProjectID,
		"secrets.gcpCredentials": defaults.Secrets.GCPCredentials,
		"logLevel":               defaults.LogLevel,
		"logFormat":              defaults.LogFormat,
		"adminMode":              defaults.AdminMode,
		"maintenanceMessage":     defaults.MaintenanceMessage,
	}, "."), nil); err != nil {
		return nil, err
	}

	// 2. Load global config (~/.scion/server.yaml)
	if globalDir, err := GetGlobalDir(); err == nil {
		loadServerConfigFile(k, globalDir)
	}

	// 3. Load local config
	if configPath != "" {
		// Check if configPath is a file or directory
		info, err := os.Stat(configPath)
		if err == nil {
			if info.IsDir() {
				loadServerConfigFile(k, configPath)
			} else {
				_ = k.Load(file.Provider(configPath), yaml.Parser())
			}
		}
	} else {
		// Try current directory
		loadServerConfigFile(k, ".")
	}

	// 4. Load environment variables (SCION_SERVER_ prefix)
	// Maps: SCION_SERVER_HUB_PORT -> hub.port
	//       SCION_SERVER_DATABASE_DRIVER -> database.driver
	//       SCION_SERVER_LOG_LEVEL -> logLevel
	//       SCION_SERVER_OAUTH_CLI_GOOGLE_CLIENTID -> oauth.cli.google.clientId
	_ = k.Load(env.Provider("SCION_SERVER_", ".", func(s string) string {
		key := strings.TrimPrefix(s, "SCION_SERVER_")
		// Replace underscores with dots for nested keys and handle camelCase
		key = envKeyToConfigKey(key)
		return key
	}), nil)

	// Unmarshal into GlobalConfig struct
	config := &GlobalConfig{
		Hub: HubServerConfig{
			CORSAllowedOrigins: make([]string, 0),
			CORSAllowedMethods: make([]string, 0),
			CORSAllowedHeaders: make([]string, 0),
		},
		RuntimeBroker: RuntimeBrokerConfig{
			CORSAllowedOrigins: make([]string, 0),
			CORSAllowedMethods: make([]string, 0),
			CORSAllowedHeaders: make([]string, 0),
		},
	}

	if err := k.Unmarshal("", config); err != nil {
		return nil, err
	}

	// Apply defaults for database path if not set
	if config.Database.URL == "" && config.Database.Driver == "sqlite" {
		if globalDir, err := GetGlobalDir(); err == nil {
			config.Database.URL = filepath.Join(globalDir, "hub.db")
		} else {
			config.Database.URL = "hub.db"
		}
	}

	// Fixup for list fields that might be loaded as a single comma-separated string from env vars.
	// This happens because koanf's env provider doesn't automatically split strings for slice fields.
	if len(config.Hub.AdminEmails) == 1 && strings.Contains(config.Hub.AdminEmails[0], ",") {
		config.Hub.AdminEmails = parseCommaSeparatedList(config.Hub.AdminEmails[0])
	}
	if len(config.Auth.AuthorizedDomains) == 1 && strings.Contains(config.Auth.AuthorizedDomains[0], ",") {
		config.Auth.AuthorizedDomains = parseCommaSeparatedList(config.Auth.AuthorizedDomains[0])
	}

	return config, nil
}

// parseCommaSeparatedList parses a comma-separated string into a slice.
func parseCommaSeparatedList(s string) []string {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	result := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			result = append(result, p)
		}
	}
	return result
}

// envKeyToConfigKey converts an environment variable key to a config key.
// Handles camelCase conversion for known fields like clientId, clientSecret.
// Example: OAUTH_CLI_GOOGLE_CLIENTID -> oauth.cli.google.clientId
func envKeyToConfigKey(envKey string) string {
	// Known camelCase field mappings
	camelCaseFields := map[string]string{
		"clientid":             "clientId",
		"clientsecret":         "clientSecret",
		"readtimeout":          "readTimeout",
		"writetimeout":         "writeTimeout",
		"brokerid":             "brokerId",
		"brokername":           "brokerName",
		"hubendpoint":          "hubEndpoint",
		"containerhubendpoint": "containerHubEndpoint",
		"devmode":              "devMode",
		"devtoken":             "devToken",
		"devtokenfile":         "devTokenFile",
		"loglevel":             "logLevel",
		"logformat":            "logFormat",
		"localpath":            "localPath",
		"authorizeddomains":    "authorizedDomains",
		"adminemails":          "adminEmails",
		"gcpprojectid":         "gcpProjectId",
		"gcpcredentials":       "gcpCredentials",
		"hubid":                "hubId",
		"adminmode":            "adminMode",
		"maintenancemessage":   "maintenanceMessage",
	}

	// Split by underscore, convert each part
	parts := strings.Split(strings.ToLower(envKey), "_")
	for i, part := range parts {
		if replacement, ok := camelCaseFields[part]; ok {
			parts[i] = replacement
		}
	}

	return strings.Join(parts, ".")
}

// applyEnvOverrides loads SCION_SERVER_ environment variables and merges them
// into an existing GlobalConfig. This ensures env vars override config file
// values regardless of which config loading path was used (settings.yaml or
// legacy server.yaml).
func applyEnvOverrides(gc *GlobalConfig) error {
	k := koanf.New(".")
	_ = k.Load(env.Provider("SCION_SERVER_", ".", func(s string) string {
		key := strings.TrimPrefix(s, "SCION_SERVER_")
		return envKeyToConfigKey(key)
	}), nil)

	if err := k.Unmarshal("", gc); err != nil {
		return err
	}

	// Fixup for list fields that might be loaded as a single comma-separated
	// string from env vars (koanf's env provider doesn't auto-split slices).
	if len(gc.Hub.AdminEmails) == 1 && strings.Contains(gc.Hub.AdminEmails[0], ",") {
		gc.Hub.AdminEmails = parseCommaSeparatedList(gc.Hub.AdminEmails[0])
	}
	if len(gc.Auth.AuthorizedDomains) == 1 && strings.Contains(gc.Auth.AuthorizedDomains[0], ",") {
		gc.Auth.AuthorizedDomains = parseCommaSeparatedList(gc.Auth.AuthorizedDomains[0])
	}

	return nil
}

// LoadServerMode reads just the server mode from settings.yaml without loading the full config.
// Returns "production" if mode is explicitly set to "production", empty string otherwise.
func LoadServerMode() string {
	globalDir, err := GetGlobalDir()
	if err != nil {
		return ""
	}

	settingsPath := filepath.Join(globalDir, "settings.yaml")
	data, err := os.ReadFile(settingsPath)
	if err != nil {
		return ""
	}

	var raw map[string]interface{}
	if err := yamlv3.Unmarshal(data, &raw); err != nil {
		return ""
	}

	serverRaw, ok := raw["server"]
	if !ok || serverRaw == nil {
		return ""
	}

	serverMap, ok := serverRaw.(map[string]interface{})
	if !ok {
		return ""
	}

	mode, _ := serverMap["mode"].(string)
	return mode
}

// GetServerConfigPath returns the path to server.yaml (or server.yml) in the given directory,
// or empty string if neither exists.
func GetServerConfigPath(dir string) string {
	yamlPath := filepath.Join(dir, "server.yaml")
	if _, err := os.Stat(yamlPath); err == nil {
		return yamlPath
	}
	ymlPath := filepath.Join(dir, "server.yml")
	if _, err := os.Stat(ymlPath); err == nil {
		return ymlPath
	}
	return ""
}

// MarshalV1ServerConfig marshals a V1ServerConfig to YAML bytes.
func MarshalV1ServerConfig(v1 *V1ServerConfig) ([]byte, error) {
	return yamlv3.Marshal(v1)
}

// MergeServerIntoSettings merges a V1ServerConfig into the settings.yaml file
// in the given directory under the "server" key.
func MergeServerIntoSettings(dir string, v1 *V1ServerConfig) error {
	settingsPath := filepath.Join(dir, "settings.yaml")

	// Load existing settings.yaml if it exists
	var raw map[string]interface{}
	if data, err := os.ReadFile(settingsPath); err == nil {
		if err := yamlv3.Unmarshal(data, &raw); err != nil {
			return fmt.Errorf("failed to parse existing settings.yaml: %w", err)
		}
	}
	if raw == nil {
		raw = make(map[string]interface{})
	}

	// Marshal the V1ServerConfig to get it as a map
	serverData, err := yamlv3.Marshal(v1)
	if err != nil {
		return fmt.Errorf("failed to marshal server config: %w", err)
	}

	var serverMap interface{}
	if err := yamlv3.Unmarshal(serverData, &serverMap); err != nil {
		return fmt.Errorf("failed to unmarshal server config: %w", err)
	}

	raw["server"] = serverMap

	// Ensure schema_version is set
	if _, ok := raw["schema_version"]; !ok {
		raw["schema_version"] = "1"
	}

	// Write back
	newData, err := yamlv3.Marshal(raw)
	if err != nil {
		return fmt.Errorf("failed to marshal settings: %w", err)
	}

	return os.WriteFile(settingsPath, newData, 0644)
}

// loadServerConfigFile loads server config from a directory's server.yaml file.
func loadServerConfigFile(k *koanf.Koanf, dir string) {
	yamlPath := filepath.Join(dir, "server.yaml")
	ymlPath := filepath.Join(dir, "server.yml")

	if _, err := os.Stat(yamlPath); err == nil {
		_ = k.Load(file.Provider(yamlPath), yaml.Parser())
		return
	}
	if _, err := os.Stat(ymlPath); err == nil {
		_ = k.Load(file.Provider(ymlPath), yaml.Parser())
	}
}

// loadServerFromSettingsFile checks if settings.yaml in the given directory
// contains a "server" key. If found, it loads the server section as a
// V1ServerConfig and converts it to a GlobalConfig.
// Returns (config, true) if settings.yaml had a server key, (nil, false) otherwise.
func loadServerFromSettingsFile(dir string) (*GlobalConfig, bool) {
	settingsPath := filepath.Join(dir, "settings.yaml")
	data, err := os.ReadFile(settingsPath)
	if err != nil {
		return nil, false
	}

	// Parse the YAML to check if it has a "server" key
	var raw map[string]interface{}
	if err := yamlv3.Unmarshal(data, &raw); err != nil {
		return nil, false
	}

	serverRaw, ok := raw["server"]
	if !ok || serverRaw == nil {
		return nil, false
	}

	// Re-marshal just the server section, then unmarshal into V1ServerConfig
	serverData, err := yamlv3.Marshal(serverRaw)
	if err != nil {
		return nil, false
	}

	var v1Server V1ServerConfig
	if err := yamlv3.Unmarshal(serverData, &v1Server); err != nil {
		return nil, false
	}

	gc := ConvertV1ServerToGlobalConfig(&v1Server)

	// Also check for top-level "telemetry" section — it lives outside "server"
	// in settings.yaml but controls the default telemetry opt-in for the Hub.
	if telRaw, ok := raw["telemetry"]; ok && telRaw != nil {
		telData, err := yamlv3.Marshal(telRaw)
		if err == nil {
			var telCfg V1TelemetryConfig
			if err := yamlv3.Unmarshal(telData, &telCfg); err == nil {
				if telCfg.Enabled != nil {
					gc.TelemetryEnabled = telCfg.Enabled
				}
				gc.TelemetryConfig = &telCfg
			}
		}
	}

	return gc, true
}

// hasServerYAML checks if a directory has a server.yaml or server.yml file.
func hasServerYAML(dir string) bool {
	if _, err := os.Stat(filepath.Join(dir, "server.yaml")); err == nil {
		return true
	}
	if _, err := os.Stat(filepath.Join(dir, "server.yml")); err == nil {
		return true
	}
	return false
}
