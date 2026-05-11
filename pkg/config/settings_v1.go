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
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/knadh/koanf/parsers/yaml"
	"github.com/knadh/koanf/providers/confmap"
	"github.com/knadh/koanf/providers/env"
	"github.com/knadh/koanf/providers/file"
	"github.com/knadh/koanf/providers/rawbytes"
	"github.com/knadh/koanf/v2"
	yamlv3 "gopkg.in/yaml.v3"
)

// ResolveHarnessConfig looks up a named harness config and merges profile-level overrides.
// If profileName is empty, ActiveProfile is used. If the config name is not found in the
// settings map, an empty HarnessConfigEntry is used as the base (profile overrides still apply).
func (vs *VersionedSettings) ResolveHarnessConfig(profileName, harnessConfigName string) (HarnessConfigEntry, error) {
	if profileName == "" {
		profileName = vs.ActiveProfile
	}

	baseConfig := vs.HarnessConfigs[harnessConfigName]

	profile, ok := vs.Profiles[profileName]
	if !ok {
		return baseConfig, nil
	}

	result := baseConfig

	// Merge profile-level env
	if profile.Env != nil {
		result.Env = mergeMaps(result.Env, profile.Env)
	}

	// Merge profile-level volumes
	if profile.Volumes != nil {
		result.Volumes = append(result.Volumes, profile.Volumes...)
	}

	// Apply harness overrides from the profile
	if profile.HarnessOverrides != nil {
		if override, ok := profile.HarnessOverrides[harnessConfigName]; ok {
			if override.Image != "" {
				result.Image = override.Image
			}
			if override.User != "" {
				result.User = override.User
			}
			if override.AuthSelectedType != "" {
				result.AuthSelectedType = override.AuthSelectedType
			}
			if override.Env != nil {
				result.Env = mergeMaps(result.Env, override.Env)
			}
			if override.Volumes != nil {
				result.Volumes = append(result.Volumes, override.Volumes...)
			}
		}
	}

	return result, nil
}

// ResolveRuntime resolves the runtime config for a profile.
// Returns the runtime config, the effective runtime type, and any error.
// If profileName is empty, ActiveProfile is used.
// The runtime type is V1RuntimeConfig.Type if set, otherwise the map key name.
func (vs *VersionedSettings) ResolveRuntime(profileName string) (V1RuntimeConfig, string, error) {
	if profileName == "" {
		profileName = vs.ActiveProfile
	}
	profile, ok := vs.Profiles[profileName]
	if !ok {
		return V1RuntimeConfig{}, "", fmt.Errorf("profile %q not found", profileName)
	}
	rtConfig, ok := vs.Runtimes[profile.Runtime]
	if !ok {
		return V1RuntimeConfig{}, "", fmt.Errorf("runtime %q not found for profile %q", profile.Runtime, profileName)
	}

	// Resolve the effective runtime type: explicit Type field, or map key name
	runtimeType := rtConfig.Type
	if runtimeType == "" {
		runtimeType = profile.Runtime
	}

	// Merge profile-level env into runtime config
	if profile.Env != nil {
		rtConfig.Env = mergeMaps(rtConfig.Env, profile.Env)
	}

	return rtConfig, runtimeType, nil
}

// GetHubEndpoint returns the Hub endpoint from settings, or empty string if not configured.
func (vs *VersionedSettings) GetHubEndpoint() string {
	if vs.Hub != nil {
		return vs.Hub.Endpoint
	}
	return ""
}

// IsHubConfigured returns true if Hub settings are configured.
func (vs *VersionedSettings) IsHubConfigured() bool {
	return vs.Hub != nil && vs.Hub.Endpoint != ""
}

// IsHubEnabled returns true if Hub integration is explicitly enabled.
func (vs *VersionedSettings) IsHubEnabled() bool {
	return vs.Hub != nil && vs.Hub.Enabled != nil && *vs.Hub.Enabled
}

// IsHubLinked returns true if this grove has been explicitly linked to the Hub.
func (vs *VersionedSettings) IsHubLinked() bool {
	return vs.Hub != nil && vs.Hub.Linked != nil && *vs.Hub.Linked
}

// IsHubExplicitlyDisabled returns true if Hub integration is explicitly disabled.
func (vs *VersionedSettings) IsHubExplicitlyDisabled() bool {
	return vs.Hub != nil && vs.Hub.Enabled != nil && !*vs.Hub.Enabled
}

// IsHubLocalOnly returns true if the grove is configured for local-only mode.
func (vs *VersionedSettings) IsHubLocalOnly() bool {
	return vs.Hub != nil && vs.Hub.LocalOnly != nil && *vs.Hub.LocalOnly
}

// IsImageRegistryConfigured returns true if image_registry is set at any level
// (top-level or in any profile).
func (vs *VersionedSettings) IsImageRegistryConfigured(profileName string) bool {
	return vs.ResolveImageRegistry(profileName) != ""
}

// ResolveImageRegistry returns the effective image_registry for the given profile.
// Profile-level image_registry takes precedence over the top-level setting.
func (vs *VersionedSettings) ResolveImageRegistry(profileName string) string {
	if profileName == "" {
		profileName = vs.ActiveProfile
	}
	if profile, ok := vs.Profiles[profileName]; ok && profile.ImageRegistry != "" {
		return profile.ImageRegistry
	}
	return vs.ImageRegistry
}

// RequireImageRegistry checks that image_registry is configured and returns an
// actionable error if not. This is called early in command execution to fail fast.
func RequireImageRegistry(grovePath, profileName string) error {
	vs, _, err := LoadEffectiveSettings(grovePath)
	if err != nil {
		// Can't load settings — let other code handle this error
		return nil
	}
	if vs == nil {
		return nil
	}
	if vs.IsImageRegistryConfigured(profileName) {
		return nil
	}
	return fmt.Errorf("image_registry is not configured.\n\n" +
		"Scion requires container images to run agents. To get started:\n\n" +
		"  1. Build your images:  image-build/scripts/build-images.sh --registry <your-registry> --push\n" +
		"     See image-build/README.md for detailed instructions.\n\n" +
		"  2. Configure scion:   scion config set --global image_registry <your-registry>\n" +
		"     Example:           scion config set --global image_registry ghcr.io/myorg")
}

// RewriteImageRegistry replaces the registry prefix of a container image reference
// with newRegistry. Only images whose basename starts with "scion-" are rewritten.
// If newRegistry is empty, the original image is returned unchanged.
func RewriteImageRegistry(fullImage, newRegistry string) string {
	if newRegistry == "" || fullImage == "" {
		return fullImage
	}

	// Extract the basename (last path component, e.g. "scion-claude:latest")
	lastSlash := strings.LastIndex(fullImage, "/")
	var basename string
	if lastSlash >= 0 {
		basename = fullImage[lastSlash+1:]
	} else {
		basename = fullImage
	}

	// Only rewrite images following the scion naming convention
	if !strings.HasPrefix(basename, "scion-") {
		return fullImage
	}

	// Strip trailing slash from registry
	registry := strings.TrimRight(newRegistry, "/")
	return registry + "/" + basename
}

// PrintDeprecationWarnings prints deprecation warnings to stderr.
func PrintDeprecationWarnings(warnings []string) {
	for _, w := range warnings {
		fmt.Fprintf(os.Stderr, "Warning: %s\n", w)
	}
}

// VersionedSettings is the root configuration struct for versioned settings (v1+).
type VersionedSettings struct {
	SchemaVersion        string                        `json:"schema_version" yaml:"schema_version" koanf:"schema_version"`
	ActiveProfile        string                        `json:"active_profile,omitempty" yaml:"active_profile,omitempty" koanf:"active_profile"`
	DefaultTemplate      string                        `json:"default_template,omitempty" yaml:"default_template,omitempty" koanf:"default_template"`
	DefaultHarnessConfig string                        `json:"default_harness_config,omitempty" yaml:"default_harness_config,omitempty" koanf:"default_harness_config"`
	Server               *V1ServerConfig               `json:"server,omitempty" yaml:"server,omitempty" koanf:"server"`
	Hub                  *V1HubClientConfig            `json:"hub,omitempty" yaml:"hub,omitempty" koanf:"hub"`
	CLI                  *V1CLIConfig                  `json:"cli,omitempty" yaml:"cli,omitempty" koanf:"cli"`
	Telemetry            *V1TelemetryConfig            `json:"telemetry,omitempty" yaml:"telemetry,omitempty" koanf:"telemetry"`
	Runtimes             map[string]V1RuntimeConfig    `json:"runtimes,omitempty" yaml:"runtimes,omitempty" koanf:"runtimes"`
	ImageRegistry        string                        `json:"image_registry,omitempty" yaml:"image_registry,omitempty" koanf:"image_registry"`
	WorkspacePath        string                        `json:"workspace_path,omitempty" yaml:"workspace_path,omitempty" koanf:"workspace_path"`
	HarnessConfigs       map[string]HarnessConfigEntry `json:"harness_configs,omitempty" yaml:"harness_configs,omitempty" koanf:"harness_configs"`
	Profiles             map[string]V1ProfileConfig    `json:"profiles,omitempty" yaml:"profiles,omitempty" koanf:"profiles"`
	SharedDirs           []api.SharedDir               `json:"shared_dirs,omitempty" yaml:"shared_dirs,omitempty" koanf:"shared_dirs"`

	// Default agent limits (applied when no explicit value is set)
	DefaultMaxTurns      int               `json:"default_max_turns,omitempty" yaml:"default_max_turns,omitempty" koanf:"default_max_turns"`
	DefaultMaxModelCalls int               `json:"default_max_model_calls,omitempty" yaml:"default_max_model_calls,omitempty" koanf:"default_max_model_calls"`
	DefaultMaxDuration   string            `json:"default_max_duration,omitempty" yaml:"default_max_duration,omitempty" koanf:"default_max_duration"`
	DefaultResources     *api.ResourceSpec `json:"default_resources,omitempty" yaml:"default_resources,omitempty" koanf:"default_resources"`
}

// V1ServerConfig holds server-side configuration in the versioned settings format.
// This mirrors GlobalConfig but uses snake_case koanf/yaml tags.
// Only valid at the global level (~/.scion/settings.yaml), never in grove-level settings.
type V1ServerConfig struct {
	// Mode selects the server operating mode: "workstation" (default) or "production".
	// When set to "production", the server behaves as if --production were passed.
	Mode      string             `json:"mode,omitempty" yaml:"mode,omitempty" koanf:"mode"`
	Env       string             `json:"env,omitempty" yaml:"env,omitempty" koanf:"env"`
	Hub       *V1ServerHubConfig `json:"hub,omitempty" yaml:"hub,omitempty" koanf:"hub"`
	Broker    *V1BrokerConfig    `json:"broker,omitempty" yaml:"broker,omitempty" koanf:"broker"`
	Database  *V1DatabaseConfig  `json:"database,omitempty" yaml:"database,omitempty" koanf:"database"`
	Auth      *V1AuthConfig      `json:"auth,omitempty" yaml:"auth,omitempty" koanf:"auth"`
	OAuth     *V1OAuthConfig     `json:"oauth,omitempty" yaml:"oauth,omitempty" koanf:"oauth"`
	Storage   *V1StorageConfig   `json:"storage,omitempty" yaml:"storage,omitempty" koanf:"storage"`
	Secrets   *V1SecretsConfig   `json:"secrets,omitempty" yaml:"secrets,omitempty" koanf:"secrets"`
	LogLevel  string             `json:"log_level,omitempty" yaml:"log_level,omitempty" koanf:"log_level"`
	LogFormat string             `json:"log_format,omitempty" yaml:"log_format,omitempty" koanf:"log_format"`

	// NotificationChannels configures external notification delivery channels.
	// Secrets (webhook URLs, API tokens) are held in memory only — never persisted to a database.
	NotificationChannels []V1NotificationChannelConfig `json:"notification_channels,omitempty" yaml:"notification_channels,omitempty" koanf:"notification_channels"`

	// MessageBroker configures the message broker for pub/sub message routing.
	// When enabled, messages are routed through the broker instead of direct dispatch,
	// enabling topic-based subscriptions and broadcast fan-out at the Hub level.
	MessageBroker *V1MessageBrokerConfig `json:"message_broker,omitempty" yaml:"message_broker,omitempty" koanf:"message_broker"`

	// Plugins configures external plugin loading for message brokers and harnesses.
	// Plugins run as separate processes using hashicorp/go-plugin.
	Plugins *V1PluginsConfig `json:"plugins,omitempty" yaml:"plugins,omitempty" koanf:"plugins"`

	// GitHubApp configures the Hub's GitHub App integration for agent git authentication.
	GitHubApp *V1GitHubAppConfig `json:"github_app,omitempty" yaml:"github_app,omitempty" koanf:"github_app"`
}

// V1GitHubAppConfig holds the GitHub App configuration in settings.yaml format.
type V1GitHubAppConfig struct {
	AppID           int64  `json:"app_id,omitempty" yaml:"app_id,omitempty" koanf:"app_id"`
	PrivateKeyPath  string `json:"private_key_path,omitempty" yaml:"private_key_path,omitempty" koanf:"private_key_path"`
	PrivateKey      string `json:"private_key,omitempty" yaml:"private_key,omitempty" koanf:"private_key"`
	WebhookSecret   string `json:"webhook_secret,omitempty" yaml:"webhook_secret,omitempty" koanf:"webhook_secret"`
	APIBaseURL      string `json:"api_base_url,omitempty" yaml:"api_base_url,omitempty" koanf:"api_base_url"`
	WebhooksEnabled bool   `json:"webhooks_enabled,omitempty" yaml:"webhooks_enabled,omitempty" koanf:"webhooks_enabled"`
	InstallationURL string `json:"installation_url,omitempty" yaml:"installation_url,omitempty" koanf:"installation_url"`
}

// V1NotificationChannelConfig holds configuration for an external notification channel.
type V1NotificationChannelConfig struct {
	Type             string            `json:"type" yaml:"type" koanf:"type"`
	Params           map[string]string `json:"params,omitempty" yaml:"params,omitempty" koanf:"params"`
	FilterTypes      []string          `json:"filter_types,omitempty" yaml:"filter_types,omitempty" koanf:"filter_types"`
	FilterUrgentOnly bool              `json:"filter_urgent_only,omitempty" yaml:"filter_urgent_only,omitempty" koanf:"filter_urgent_only"`
}

// V1MessageBrokerConfig configures the message broker adapter.
type V1MessageBrokerConfig struct {
	// Enabled controls whether the message broker is active. Default false.
	Enabled bool `json:"enabled,omitempty" yaml:"enabled,omitempty" koanf:"enabled"`
	// Type selects the broker adapter: "inprocess" (default). Future: "nats", "redis".
	Type string `json:"type,omitempty" yaml:"type,omitempty" koanf:"type"`
}

// V1PluginsConfig configures external plugin loading for message brokers and harnesses.
type V1PluginsConfig struct {
	// Broker maps plugin names to their configuration for message broker plugins.
	Broker map[string]V1PluginEntry `json:"broker,omitempty" yaml:"broker,omitempty" koanf:"broker"`
	// Harness maps plugin names to their configuration for harness plugins.
	Harness map[string]V1PluginEntry `json:"harness,omitempty" yaml:"harness,omitempty" koanf:"harness"`
}

// V1PluginEntry holds configuration for a single plugin.
type V1PluginEntry struct {
	// Path is the explicit filesystem path to the plugin binary.
	// If empty, discovery will attempt to find it automatically.
	// Ignored when SelfManaged is true.
	Path string `json:"path,omitempty" yaml:"path,omitempty" koanf:"path"`
	// Config is an opaque key-value map passed to the plugin via Configure().
	Config map[string]string `json:"config,omitempty" yaml:"config,omitempty" koanf:"config"`
	// SelfManaged indicates the plugin manages its own process lifecycle.
	// The Hub connects to the plugin's RPC server rather than starting it.
	SelfManaged bool `json:"self_managed,omitempty" yaml:"self_managed,omitempty" koanf:"self_managed"`
	// Address is the RPC address for self-managed plugins (e.g. "localhost:9090").
	// Required when SelfManaged is true.
	Address string `json:"address,omitempty" yaml:"address,omitempty" koanf:"address"`
}

// V1ServerHubConfig holds the Hub API server settings (when running scion-server).
type V1ServerHubConfig struct {
	Port         int           `json:"port,omitempty" yaml:"port,omitempty" koanf:"port"`
	Host         string        `json:"host,omitempty" yaml:"host,omitempty" koanf:"host"`
	HubID        string        `json:"hub_id,omitempty" yaml:"hub_id,omitempty" koanf:"hub_id"`
	PublicURL    string        `json:"public_url,omitempty" yaml:"public_url,omitempty" koanf:"public_url"`
	ReadTimeout  string        `json:"read_timeout,omitempty" yaml:"read_timeout,omitempty" koanf:"read_timeout"`
	WriteTimeout string        `json:"write_timeout,omitempty" yaml:"write_timeout,omitempty" koanf:"write_timeout"`
	CORS         *V1CORSConfig `json:"cors,omitempty" yaml:"cors,omitempty" koanf:"cors"`
	AdminEmails  []string      `json:"admin_emails,omitempty" yaml:"admin_emails,omitempty" koanf:"admin_emails"`

	// SoftDeleteRetention is how long soft-deleted agents are retained (e.g., "72h").
	SoftDeleteRetention string `json:"soft_delete_retention,omitempty" yaml:"soft_delete_retention,omitempty" koanf:"soft_delete_retention"`
	// SoftDeleteRetainFiles controls whether workspace files are preserved during soft-delete.
	SoftDeleteRetainFiles *bool `json:"soft_delete_retain_files,omitempty" yaml:"soft_delete_retain_files,omitempty" koanf:"soft_delete_retain_files"`
}

// V1BrokerConfig holds Runtime Broker configuration.
// Includes broker identity fields previously in hub client config.
type V1BrokerConfig struct {
	Enabled              bool          `json:"enabled,omitempty" yaml:"enabled,omitempty" koanf:"enabled"`
	Port                 int           `json:"port,omitempty" yaml:"port,omitempty" koanf:"port"`
	Host                 string        `json:"host,omitempty" yaml:"host,omitempty" koanf:"host"`
	ReadTimeout          string        `json:"read_timeout,omitempty" yaml:"read_timeout,omitempty" koanf:"read_timeout"`
	WriteTimeout         string        `json:"write_timeout,omitempty" yaml:"write_timeout,omitempty" koanf:"write_timeout"`
	HubEndpoint          string        `json:"hub_endpoint,omitempty" yaml:"hub_endpoint,omitempty" koanf:"hub_endpoint"`
	ContainerHubEndpoint string        `json:"container_hub_endpoint,omitempty" yaml:"container_hub_endpoint,omitempty" koanf:"container_hub_endpoint"`
	BrokerID             string        `json:"broker_id,omitempty" yaml:"broker_id,omitempty" koanf:"broker_id"`
	BrokerName           string        `json:"broker_name,omitempty" yaml:"broker_name,omitempty" koanf:"broker_name"`
	BrokerNickname       string        `json:"broker_nickname,omitempty" yaml:"broker_nickname,omitempty" koanf:"broker_nickname"`
	BrokerToken          string        `json:"broker_token,omitempty" yaml:"broker_token,omitempty" koanf:"broker_token"`
	AutoProvide          *bool         `json:"auto_provide,omitempty" yaml:"auto_provide,omitempty" koanf:"auto_provide"`
	CORS                 *V1CORSConfig `json:"cors,omitempty" yaml:"cors,omitempty" koanf:"cors"`
	// AllowContainerScriptHarnesses controls whether this broker will
	// dispatch agents whose harness-config declares container-script
	// provisioning. Defaults to true; set false to block container-script dispatches.
	AllowContainerScriptHarnesses *bool `json:"allow_container_script_harnesses,omitempty" yaml:"allow_container_script_harnesses,omitempty" koanf:"allow_container_script_harnesses"`
}

// V1DatabaseConfig holds database settings.
type V1DatabaseConfig struct {
	Driver string `json:"driver,omitempty" yaml:"driver,omitempty" koanf:"driver"`
	URL    string `json:"url,omitempty" yaml:"url,omitempty" koanf:"url"`
}

// V1AuthConfig holds development authentication settings.
type V1AuthConfig struct {
	DevMode           bool     `json:"dev_mode,omitempty" yaml:"dev_mode,omitempty" koanf:"dev_mode"`
	DevToken          string   `json:"dev_token,omitempty" yaml:"dev_token,omitempty" koanf:"dev_token"`
	DevTokenFile      string   `json:"dev_token_file,omitempty" yaml:"dev_token_file,omitempty" koanf:"dev_token_file"`
	AuthorizedDomains []string `json:"authorized_domains,omitempty" yaml:"authorized_domains,omitempty" koanf:"authorized_domains"`
	UserAccessMode    string   `json:"user_access_mode,omitempty" yaml:"user_access_mode,omitempty" koanf:"user_access_mode"`
}

// V1OAuthConfig holds OAuth provider configurations.
type V1OAuthConfig struct {
	Web    *V1OAuthClientConfig `json:"web,omitempty" yaml:"web,omitempty" koanf:"web"`
	CLI    *V1OAuthClientConfig `json:"cli,omitempty" yaml:"cli,omitempty" koanf:"cli"`
	Device *V1OAuthClientConfig `json:"device,omitempty" yaml:"device,omitempty" koanf:"device"`
}

// V1OAuthClientConfig holds OAuth provider settings for a specific client type.
type V1OAuthClientConfig struct {
	Google *V1OAuthProviderConfig `json:"google,omitempty" yaml:"google,omitempty" koanf:"google"`
	GitHub *V1OAuthProviderConfig `json:"github,omitempty" yaml:"github,omitempty" koanf:"github"`
}

// V1OAuthProviderConfig holds OAuth credentials for a single provider.
type V1OAuthProviderConfig struct {
	ClientID     string `json:"client_id,omitempty" yaml:"client_id,omitempty" koanf:"client_id"`
	ClientSecret string `json:"client_secret,omitempty" yaml:"client_secret,omitempty" koanf:"client_secret"`
}

// V1StorageConfig holds storage settings.
type V1StorageConfig struct {
	Provider  string `json:"provider,omitempty" yaml:"provider,omitempty" koanf:"provider"`
	Bucket    string `json:"bucket,omitempty" yaml:"bucket,omitempty" koanf:"bucket"`
	LocalPath string `json:"local_path,omitempty" yaml:"local_path,omitempty" koanf:"local_path"`
}

// V1SecretsConfig holds secrets backend settings.
type V1SecretsConfig struct {
	Backend        string `json:"backend,omitempty" yaml:"backend,omitempty" koanf:"backend"`
	GCPProjectID   string `json:"gcp_project_id,omitempty" yaml:"gcp_project_id,omitempty" koanf:"gcp_project_id"`
	GCPCredentials string `json:"gcp_credentials,omitempty" yaml:"gcp_credentials,omitempty" koanf:"gcp_credentials"`
}

// V1CORSConfig holds CORS settings for server endpoints.
type V1CORSConfig struct {
	Enabled        bool     `json:"enabled,omitempty" yaml:"enabled,omitempty" koanf:"enabled"`
	AllowedOrigins []string `json:"allowed_origins,omitempty" yaml:"allowed_origins,omitempty" koanf:"allowed_origins"`
	AllowedMethods []string `json:"allowed_methods,omitempty" yaml:"allowed_methods,omitempty" koanf:"allowed_methods"`
	AllowedHeaders []string `json:"allowed_headers,omitempty" yaml:"allowed_headers,omitempty" koanf:"allowed_headers"`
	MaxAge         int      `json:"max_age,omitempty" yaml:"max_age,omitempty" koanf:"max_age"`
}

// V1HubClientConfig defines hub client connection settings for versioned config.
// Legacy fields (Token, APIKey, BrokerID, BrokerToken, LastSyncedAt) are removed.
type V1HubClientConfig struct {
	Enabled   *bool  `json:"enabled,omitempty" yaml:"enabled,omitempty" koanf:"enabled"`
	Linked    *bool  `json:"linked,omitempty" yaml:"linked,omitempty" koanf:"linked"`
	Endpoint  string `json:"endpoint,omitempty" yaml:"endpoint,omitempty" koanf:"endpoint"`
	ProjectID   string `json:"grove_id,omitempty" yaml:"grove_id,omitempty" koanf:"grove_id"`
	LocalOnly *bool  `json:"local_only,omitempty" yaml:"local_only,omitempty" koanf:"local_only"`
}

// V1CLIConfig defines CLI behavior settings for versioned config.
type V1CLIConfig struct {
	AutoHelp            *bool `json:"autohelp,omitempty" yaml:"autohelp,omitempty" koanf:"autohelp"`
	InteractiveDisabled *bool `json:"interactive_disabled,omitempty" yaml:"interactive_disabled,omitempty" koanf:"interactive_disabled"`
}

// V1TelemetryConfig holds telemetry/observability settings.
// Configurable at global or grove scope in settings.yaml, and overridable per-template/agent
// in scion-agent.yaml. See design doc section 10.2 for the full reference.
type V1TelemetryConfig struct {
	Enabled  *bool                    `json:"enabled,omitempty" yaml:"enabled,omitempty" koanf:"enabled"`
	Cloud    *V1TelemetryCloudConfig  `json:"cloud,omitempty" yaml:"cloud,omitempty" koanf:"cloud"`
	Hub      *V1TelemetryHubConfig    `json:"hub,omitempty" yaml:"hub,omitempty" koanf:"hub"`
	Local    *V1TelemetryLocalConfig  `json:"local,omitempty" yaml:"local,omitempty" koanf:"local"`
	Filter   *V1TelemetryFilterConfig `json:"filter,omitempty" yaml:"filter,omitempty" koanf:"filter"`
	Resource map[string]string        `json:"resource,omitempty" yaml:"resource,omitempty" koanf:"resource"`
}

// V1TelemetryCloudConfig holds cloud OTLP forwarding settings.
type V1TelemetryCloudConfig struct {
	Enabled  *bool                   `json:"enabled,omitempty" yaml:"enabled,omitempty" koanf:"enabled"`
	Endpoint string                  `json:"endpoint,omitempty" yaml:"endpoint,omitempty" koanf:"endpoint"`
	Protocol string                  `json:"protocol,omitempty" yaml:"protocol,omitempty" koanf:"protocol"`
	Headers  map[string]string       `json:"headers,omitempty" yaml:"headers,omitempty" koanf:"headers"`
	TLS      *V1TelemetryTLSConfig   `json:"tls,omitempty" yaml:"tls,omitempty" koanf:"tls"`
	Batch    *V1TelemetryBatchConfig `json:"batch,omitempty" yaml:"batch,omitempty" koanf:"batch"`
	Provider string                  `json:"provider,omitempty" yaml:"provider,omitempty" koanf:"provider"`
}

// V1TelemetryTLSConfig holds TLS settings for cloud OTLP export.
type V1TelemetryTLSConfig struct {
	Enabled            *bool  `json:"enabled,omitempty" yaml:"enabled,omitempty" koanf:"enabled"`
	InsecureSkipVerify *bool  `json:"insecure_skip_verify,omitempty" yaml:"insecure_skip_verify,omitempty" koanf:"insecure_skip_verify"`
	CAFile             string `json:"ca_file,omitempty" yaml:"ca_file,omitempty" koanf:"ca_file"`
}

// V1TelemetryBatchConfig holds batch export settings.
type V1TelemetryBatchConfig struct {
	MaxSize int    `json:"max_size,omitempty" yaml:"max_size,omitempty" koanf:"max_size"`
	Timeout string `json:"timeout,omitempty" yaml:"timeout,omitempty" koanf:"timeout"`
}

// V1TelemetryHubConfig holds Hub telemetry reporting settings.
type V1TelemetryHubConfig struct {
	Enabled        *bool  `json:"enabled,omitempty" yaml:"enabled,omitempty" koanf:"enabled"`
	ReportInterval string `json:"report_interval,omitempty" yaml:"report_interval,omitempty" koanf:"report_interval"`
}

// V1TelemetryLocalConfig holds local debug telemetry output settings.
type V1TelemetryLocalConfig struct {
	Enabled *bool  `json:"enabled,omitempty" yaml:"enabled,omitempty" koanf:"enabled"`
	File    string `json:"file,omitempty" yaml:"file,omitempty" koanf:"file"`
	Console *bool  `json:"console,omitempty" yaml:"console,omitempty" koanf:"console"`
}

// V1TelemetryFilterConfig holds event filtering and sampling settings.
type V1TelemetryFilterConfig struct {
	Enabled          *bool                        `json:"enabled,omitempty" yaml:"enabled,omitempty" koanf:"enabled"`
	RespectDebugMode *bool                        `json:"respect_debug_mode,omitempty" yaml:"respect_debug_mode,omitempty" koanf:"respect_debug_mode"`
	Events           *V1TelemetryEventsConfig     `json:"events,omitempty" yaml:"events,omitempty" koanf:"events"`
	Attributes       *V1TelemetryAttributesConfig `json:"attributes,omitempty" yaml:"attributes,omitempty" koanf:"attributes"`
	Sampling         *V1TelemetrySamplingConfig   `json:"sampling,omitempty" yaml:"sampling,omitempty" koanf:"sampling"`
}

// V1TelemetryEventsConfig holds event include/exclude lists.
type V1TelemetryEventsConfig struct {
	Include []string `json:"include,omitempty" yaml:"include,omitempty" koanf:"include"`
	Exclude []string `json:"exclude,omitempty" yaml:"exclude,omitempty" koanf:"exclude"`
}

// V1TelemetryAttributesConfig holds attribute redaction and hashing lists.
type V1TelemetryAttributesConfig struct {
	Redact []string `json:"redact,omitempty" yaml:"redact,omitempty" koanf:"redact"`
	Hash   []string `json:"hash,omitempty" yaml:"hash,omitempty" koanf:"hash"`
}

// V1TelemetrySamplingConfig holds sampling rate settings.
type V1TelemetrySamplingConfig struct {
	Default *float64           `json:"default,omitempty" yaml:"default,omitempty" koanf:"default"`
	Rates   map[string]float64 `json:"rates,omitempty" yaml:"rates,omitempty" koanf:"rates"`
}

// V1RuntimeConfig extends RuntimeConfig with a Type field.
type V1RuntimeConfig struct {
	Type              string            `json:"type,omitempty" yaml:"type,omitempty" koanf:"type"`
	Host              string            `json:"host,omitempty" yaml:"host,omitempty" koanf:"host"`
	Context           string            `json:"context,omitempty" yaml:"context,omitempty" koanf:"context"`
	Namespace         string            `json:"namespace,omitempty" yaml:"namespace,omitempty" koanf:"namespace"`
	Env               map[string]string `json:"env,omitempty" yaml:"env,omitempty" koanf:"env"`
	Sync              string            `json:"sync,omitempty" yaml:"sync,omitempty" koanf:"sync"`
	GKE               bool              `json:"gke,omitempty" yaml:"gke,omitempty" koanf:"gke"`
	ListAllNamespaces bool              `json:"list_all_namespaces,omitempty" yaml:"list_all_namespaces,omitempty" koanf:"list_all_namespaces"`
}

// HarnessConfigEntry defines a harness configuration entry in versioned settings.
// The Harness field is required and specifies the harness type this config applies to.
type HarnessConfigEntry struct {
	Harness          string               `json:"harness" yaml:"harness" koanf:"harness"`
	Image            string               `json:"image,omitempty" yaml:"image,omitempty" koanf:"image"`
	User             string               `json:"user,omitempty" yaml:"user,omitempty" koanf:"user"`
	Model            string               `json:"model,omitempty" yaml:"model,omitempty" koanf:"model"`
	TaskFlag         string               `json:"task_flag,omitempty" yaml:"task_flag,omitempty" koanf:"task_flag"`
	Args             []string             `json:"args,omitempty" yaml:"args,omitempty" koanf:"args"`
	Env              map[string]string    `json:"env,omitempty" yaml:"env,omitempty" koanf:"env"`
	Volumes          []api.VolumeMount    `json:"volumes,omitempty" yaml:"volumes,omitempty" koanf:"volumes"`
	AuthSelectedType string               `json:"auth_selected_type,omitempty" yaml:"auth_selected_type,omitempty" koanf:"auth_selected_type"`
	Secrets          []api.RequiredSecret `json:"secrets,omitempty" yaml:"secrets,omitempty" koanf:"secrets"`

	Provisioner      *HarnessProvisionerConfig        `json:"provisioner,omitempty" yaml:"provisioner,omitempty" koanf:"provisioner"`
	ConfigDir        string                           `json:"config_dir,omitempty" yaml:"config_dir,omitempty" koanf:"config_dir"`
	SkillsDir        string                           `json:"skills_dir,omitempty" yaml:"skills_dir,omitempty" koanf:"skills_dir"`
	InterruptKey     string                           `json:"interrupt_key,omitempty" yaml:"interrupt_key,omitempty" koanf:"interrupt_key"`
	InstructionsFile string                           `json:"instructions_file,omitempty" yaml:"instructions_file,omitempty" koanf:"instructions_file"`
	SystemPromptFile string                           `json:"system_prompt_file,omitempty" yaml:"system_prompt_file,omitempty" koanf:"system_prompt_file"`
	SystemPromptMode string                           `json:"system_prompt_mode,omitempty" yaml:"system_prompt_mode,omitempty" koanf:"system_prompt_mode"`
	Command          *HarnessCommandConfig            `json:"command,omitempty" yaml:"command,omitempty" koanf:"command"`
	EnvTemplate      map[string]string                `json:"env_template,omitempty" yaml:"env_template,omitempty" koanf:"env_template"`
	Capabilities     *api.HarnessAdvancedCapabilities `json:"capabilities,omitempty" yaml:"capabilities,omitempty" koanf:"capabilities"`
	Auth             *HarnessAuthMetadata             `json:"auth,omitempty" yaml:"auth,omitempty" koanf:"auth"`
	MCP              *HarnessMCPConfig                `json:"mcp,omitempty" yaml:"mcp,omitempty" koanf:"mcp"`
	Dialect          map[string]interface{}           `json:"dialect,omitempty" yaml:"dialect,omitempty" koanf:"dialect"`
}

// HarnessProvisionerConfig declares how a harness-config is provisioned.
// "builtin" keeps the current compiled Go harness behavior; "container-script"
// is the explicit opt-in for the decoupled container-side script path.
type HarnessProvisionerConfig struct {
	Type               string   `json:"type,omitempty" yaml:"type,omitempty" koanf:"type"`
	InterfaceVersion   int      `json:"interface_version,omitempty" yaml:"interface_version,omitempty" koanf:"interface_version"`
	Command            []string `json:"command,omitempty" yaml:"command,omitempty" koanf:"command"`
	Timeout            string   `json:"timeout,omitempty" yaml:"timeout,omitempty" koanf:"timeout"`
	LifecycleEvents    []string `json:"lifecycle_events,omitempty" yaml:"lifecycle_events,omitempty" koanf:"lifecycle_events"`
	RequiredImageTools []string `json:"required_image_tools,omitempty" yaml:"required_image_tools,omitempty" koanf:"required_image_tools"`
}

// HarnessCommandConfig describes harness CLI command construction.
type HarnessCommandConfig struct {
	Base             []string `json:"base,omitempty" yaml:"base,omitempty" koanf:"base"`
	ResumeFlag       string   `json:"resume_flag,omitempty" yaml:"resume_flag,omitempty" koanf:"resume_flag"`
	TaskFlag         string   `json:"task_flag,omitempty" yaml:"task_flag,omitempty" koanf:"task_flag"`
	TaskPosition     string   `json:"task_position,omitempty" yaml:"task_position,omitempty" koanf:"task_position"`
	SystemPromptFlag string   `json:"system_prompt_flag,omitempty" yaml:"system_prompt_flag,omitempty" koanf:"system_prompt_flag"`
}

// HarnessAuthMetadata contains declarative auth preflight metadata.
type HarnessAuthMetadata struct {
	DefaultType string                             `json:"default_type,omitempty" yaml:"default_type,omitempty" koanf:"default_type"`
	Types       map[string]HarnessAuthTypeMetadata `json:"types,omitempty" yaml:"types,omitempty" koanf:"types"`
	Autodetect  HarnessAuthAutodetect              `json:"autodetect,omitempty" yaml:"autodetect,omitempty" koanf:"autodetect"`
}

type HarnessAuthTypeMetadata struct {
	RequiredEnv   []HarnessAuthEnvRequirement  `json:"required_env,omitempty" yaml:"required_env,omitempty" koanf:"required_env"`
	RequiredFiles []HarnessAuthFileRequirement `json:"required_files,omitempty" yaml:"required_files,omitempty" koanf:"required_files"`
}

type HarnessAuthEnvRequirement struct {
	AnyOf []string `json:"any_of,omitempty" yaml:"any_of,omitempty" koanf:"any_of"`
}

type HarnessAuthFileRequirement struct {
	Name        string `json:"name,omitempty" yaml:"name,omitempty" koanf:"name"`
	Type        string `json:"type,omitempty" yaml:"type,omitempty" koanf:"type"`
	Description string `json:"description,omitempty" yaml:"description,omitempty" koanf:"description"`
	// TargetSuffix is the in-container projection target suffix. Used
	// together with the broker's home dir resolution, e.g. "/.claude/.credentials.json".
	TargetSuffix string `json:"target_suffix,omitempty" yaml:"target_suffix,omitempty" koanf:"target_suffix"`
	// AlternativeEnvKeys lists env vars that satisfy this file requirement
	// in lieu of the file itself (e.g. GOOGLE_APPLICATION_CREDENTIALS for
	// gcloud-adc).
	AlternativeEnvKeys []string `json:"alternative_env_keys,omitempty" yaml:"alternative_env_keys,omitempty" koanf:"alternative_env_keys"`
	// SkippedWhenGCPServiceAccountAssigned drops this requirement when a
	// GCP workload identity is attached, because the metadata server stands
	// in for the credential file.
	SkippedWhenGCPServiceAccountAssigned bool `json:"skipped_when_gcp_service_account_assigned,omitempty" yaml:"skipped_when_gcp_service_account_assigned,omitempty" koanf:"skipped_when_gcp_service_account_assigned"`
	// Required marks the file as a broker-side required secret: the broker
	// must locate it (via Hub secrets, CLI gather, or alternatives) before
	// dispatching the agent. When false the file is documentary — the auth
	// type uses it, but the broker is not responsible for sourcing it (the
	// user mounts a locally-resolved file). This preserves legacy parity:
	// vertex-ai's gcloud-adc was the only "must-supply" secret in the
	// compiled tables; auth-file types described their files but did not
	// enforce them at preflight.
	Required bool `json:"required,omitempty" yaml:"required,omitempty" koanf:"required"`
}

type HarnessAuthAutodetect struct {
	Env   map[string]string `json:"env,omitempty" yaml:"env,omitempty" koanf:"env"`
	Files map[string]string `json:"files,omitempty" yaml:"files,omitempty" koanf:"files"`
}

// HarnessMCPConfig is the declarative mapping that lets a harness's
// container-side provisioner translate the universal mcp_servers map into the
// harness's native MCP config without bespoke per-harness Python. Used by
// the scion_harness.apply_mcp_servers_simple helper. Harnesses whose native
// format does not fit the simple-merge pattern (e.g. OpenCode) leave this
// empty and translate themselves in provision.py.
type HarnessMCPConfig struct {
	// GlobalConfigFile is the agent-home-relative path to the file that holds
	// global-scope MCP entries (e.g. ".claude.json").
	GlobalConfigFile string `json:"global_config_file,omitempty" yaml:"global_config_file,omitempty" koanf:"global_config_file"`
	// GlobalConfigPath is the dotted JSON path inside GlobalConfigFile where
	// the per-name MCP map lives (e.g. "mcpServers").
	GlobalConfigPath string `json:"global_config_path,omitempty" yaml:"global_config_path,omitempty" koanf:"global_config_path"`
	// ProjectConfigFile and ProjectConfigPath are the same for project-scope
	// servers; the path may include "{workspace}" which is substituted with
	// the agent_workspace path.
	ProjectConfigFile string `json:"project_config_file,omitempty" yaml:"project_config_file,omitempty" koanf:"project_config_file"`
	ProjectConfigPath string `json:"project_config_path,omitempty" yaml:"project_config_path,omitempty" koanf:"project_config_path"`
	// TransportField is the field name in the native server object that holds
	// the transport identifier (e.g. "type" for Claude/Gemini).
	TransportField string `json:"transport_field,omitempty" yaml:"transport_field,omitempty" koanf:"transport_field"`
	// TransportMap maps universal transport values to the native value, e.g.
	//   stdio: stdio
	//   sse: sse
	//   streamable-http: streamable-http
	TransportMap map[string]string `json:"transport_map,omitempty" yaml:"transport_map,omitempty" koanf:"transport_map"`
}

// V1HarnessOverride defines a harness override entry in versioned settings.
// Uses snake_case tags, unlike the legacy HarnessOverride (which uses camelCase auth_selectedType).
type V1HarnessOverride struct {
	Image            string            `json:"image,omitempty" yaml:"image,omitempty" koanf:"image"`
	User             string            `json:"user,omitempty" yaml:"user,omitempty" koanf:"user"`
	Env              map[string]string `json:"env,omitempty" yaml:"env,omitempty" koanf:"env"`
	Volumes          []api.VolumeMount `json:"volumes,omitempty" yaml:"volumes,omitempty" koanf:"volumes"`
	AuthSelectedType string            `json:"auth_selected_type,omitempty" yaml:"auth_selected_type,omitempty" koanf:"auth_selected_type"`
	Resources        *api.ResourceSpec `json:"resources,omitempty" yaml:"resources,omitempty" koanf:"resources"`
}

// V1ProfileConfig extends ProfileConfig with new fields for versioned settings.
type V1ProfileConfig struct {
	Runtime              string                       `json:"runtime" yaml:"runtime" koanf:"runtime"`
	DefaultTemplate      string                       `json:"default_template,omitempty" yaml:"default_template,omitempty" koanf:"default_template"`
	DefaultHarnessConfig string                       `json:"default_harness_config,omitempty" yaml:"default_harness_config,omitempty" koanf:"default_harness_config"`
	ImageRegistry        string                       `json:"image_registry,omitempty" yaml:"image_registry,omitempty" koanf:"image_registry"`
	Env                  map[string]string            `json:"env,omitempty" yaml:"env,omitempty" koanf:"env"`
	Volumes              []api.VolumeMount            `json:"volumes,omitempty" yaml:"volumes,omitempty" koanf:"volumes"`
	Resources            *api.ResourceSpec            `json:"resources,omitempty" yaml:"resources,omitempty" koanf:"resources"`
	HarnessOverrides     map[string]V1HarnessOverride `json:"harness_overrides,omitempty" yaml:"harness_overrides,omitempty" koanf:"harness_overrides"`
	Secrets              []api.RequiredSecret         `json:"secrets,omitempty" yaml:"secrets,omitempty" koanf:"secrets"`
}

// resolveEffectiveProjectPath resolves the effective project path for settings loading.
// Shared by both LoadSettingsKoanf and LoadVersionedSettings.
// For git projects with split storage, this redirects to the external config dir
// so that settings are loaded from ~/.scion/project-configs/<slug>__<uuid>/.scion/.
func resolveEffectiveProjectPath(projectPath string) string {
	effectiveProjectPath := projectPath
	if effectiveProjectPath == "" {
		if projectPath, ok := FindProjectRoot(); ok {
			effectiveProjectPath = projectPath
		}
	} else if effectiveProjectPath == "global" || effectiveProjectPath == "home" {
		effectiveProjectPath = ""
	}
	if effectiveProjectPath != "" {
		effectiveProjectPath = GetProjectConfigDir(effectiveProjectPath)
	}
	return effectiveProjectPath
}

// LoadVersionedSettings loads settings using Koanf into VersionedSettings.
// Provider priority:
// 1. Embedded defaults (YAML) with OS-specific runtime adjustment
// 2. Global settings file (~/.scion/settings.yaml or .json)
// 3. Project settings file (.scion/settings.yaml or .json)
// 4. Environment variables (SCION_ prefix)
func LoadVersionedSettings(projectPath string) (*VersionedSettings, error) {
	k := koanf.New(".")

	// 1. Load embedded defaults (YAML)
	if defaultData, err := GetDefaultSettingsDataYAML(); err == nil {
		_ = k.Load(rawbytes.Provider(defaultData), yaml.Parser())
	}

	// 2. Load global settings (~/.scion/settings.yaml or .json)
	globalDir, _ := GetGlobalDir()
	if globalDir != "" {
		if err := loadSettingsFile(k, globalDir); err != nil {
			return nil, err
		}
	}

	// 3. Load project settings
	effectiveProjectPath := resolveEffectiveProjectPath(projectPath)
	if effectiveProjectPath != "" && effectiveProjectPath != globalDir {
		if err := loadSettingsFile(k, effectiveProjectPath); err != nil {
			return nil, err
		}
	}

	// 4. Load environment variables (SCION_ prefix)
	_ = k.Load(env.Provider("SCION_", ".", versionedEnvKeyMapper), nil)

	// For git projects, the project_id is stored in a project-id file inside the
	// .scion directory rather than in the settings file. Read it here so that
	// it overrides any hub.project_id inherited from global settings.
	if projectPath != "" {
		globalDir, _ := GetGlobalDir()
		if projectPath != globalDir {
			if projectID, err := ReadProjectID(projectPath); err == nil && projectID != "" {
				_ = k.Load(confmap.Provider(map[string]interface{}{
					"hub.grove_id": projectID,
				}, "."), nil)
			}
		}
	}

	// Remap hub.project_id to hub.grove_id for backward compatibility with V1 structs.
	// SCION_HUB_PROJECT_ID maps to hub.project_id via versionedEnvKeyMapper.
	if k.Exists("hub.project_id") && !k.Exists("hub.grove_id") {
		_ = k.Load(confmap.Provider(map[string]interface{}{
			"hub.grove_id": k.String("hub.project_id"),
		}, "."), nil)
	}

	// Unmarshal into VersionedSettings struct
	settings := &VersionedSettings{
		Runtimes:       make(map[string]V1RuntimeConfig),
		HarnessConfigs: make(map[string]HarnessConfigEntry),
		Profiles:       make(map[string]V1ProfileConfig),
	}

	if err := k.Unmarshal("", settings); err != nil {
		return nil, err
	}

	return settings, nil
}

// versionedEnvKeyMapper maps SCION_* environment variables to versioned settings keys.
// All keys are snake_case so no camelCase conversion is needed.
func versionedEnvKeyMapper(s string) string {
	key := strings.ToLower(strings.TrimPrefix(s, "SCION_"))

	// Handle nested hub keys (single level: hub.endpoint, hub.grove_id, etc.)
	if strings.HasPrefix(key, "hub_") {
		return "hub." + strings.TrimPrefix(key, "hub_")
	}
	// Handle nested cli keys (single level: cli.autohelp, cli.interactive_disabled)
	if strings.HasPrefix(key, "cli_") {
		return "cli." + strings.TrimPrefix(key, "cli_")
	}
	// Handle nested server keys — deep nesting requires mapping compound field names
	if strings.HasPrefix(key, "server_") {
		rest := strings.TrimPrefix(key, "server_")
		return "server." + mapServerEnvKey(rest)
	}
	// Handle nested telemetry keys — deep nesting with compound field names
	if strings.HasPrefix(key, "telemetry_") {
		rest := strings.TrimPrefix(key, "telemetry_")
		return "telemetry." + mapTelemetryEnvKey(rest)
	}
	// Handle SCION_OTEL_* → telemetry.cloud.* mappings
	if strings.HasPrefix(key, "otel_") {
		return mapOtelEnvKey(strings.TrimPrefix(key, "otel_"))
	}

	return key
}

// knownCompoundFields lists multi-word snake_case field names used in server config.
// These must be recognized as single fields rather than split into nested keys.
// IMPORTANT: Sorted longest-first so that "dev_token_file" matches before "dev_token".
var knownCompoundFields = []string{
	"soft_delete_retain_files",
	"soft_delete_retention",
	"authorized_domains",
	"broker_nickname",
	"allowed_origins",
	"allowed_methods",
	"allowed_headers",
	"dev_token_file",
	"gcp_project_id",
	"gcp_credentials",
	"client_secret",
	"write_timeout",
	"read_timeout",
	"broker_token",
	"admin_emails",
	"hub_endpoint",
	"container_hub_endpoint",
	"broker_name",
	"public_url",
	"local_path",
	"log_format",
	"broker_id",
	"client_id",
	"dev_token",
	"log_level",
	"dev_mode",
	"max_age",
}

// mapServerEnvKey maps the portion after "server_" to a dotted path, recognizing
// known multi-word snake_case fields so they are not incorrectly split.
// For example: "hub_read_timeout" -> "hub.read_timeout"
//
//	"broker_broker_id" -> "broker.broker_id"
//	"oauth_web_google_client_id" -> "oauth.web.google.client_id"
func mapServerEnvKey(key string) string {
	return mapEnvKeyRecursive(key)
}

// mapEnvKeyRecursive splits an env key fragment into dotted segments, recognizing
// compound field names at each position.
func mapEnvKeyRecursive(key string) string {
	if key == "" {
		return ""
	}

	// Check if the entire remaining key is a known compound field
	for _, compound := range knownCompoundFields {
		if key == compound {
			return key
		}
	}

	// Try to match the longest known compound field at the end,
	// treating the prefix as nesting segments.
	// We scan for underscores and try to split at each one.
	for i := 0; i < len(key); i++ {
		if key[i] == '_' {
			prefix := key[:i]
			rest := key[i+1:]

			// Check if the rest starts with a known compound field
			matched := false
			for _, compound := range knownCompoundFields {
				if strings.HasPrefix(rest, compound) {
					if len(rest) == len(compound) {
						// Exact match for the rest
						return prefix + "." + compound
					}
					if rest[len(compound)] == '_' {
						// Compound field followed by more segments
						return prefix + "." + compound + "." + mapEnvKeyRecursive(rest[len(compound)+1:])
					}
				}
			}

			if !matched {
				// The prefix could be a section name (hub, broker, database, auth, oauth, storage, secrets, cors)
				// Try recursively
				subResult := mapEnvKeyRecursive(rest)
				if subResult != rest || isSectionName(prefix) {
					return prefix + "." + subResult
				}
			}
		}
	}

	// No compound match found — the key is a simple single-word field
	return key
}

// isSectionName checks if a name is a known section in the server config hierarchy.
func isSectionName(name string) bool {
	switch name {
	case "hub", "broker", "database", "auth", "oauth", "storage", "secrets", "cors",
		"web", "cli", "device", "google", "github":
		return true
	}
	return false
}

// knownTelemetryCompoundFields lists multi-word snake_case field names used in telemetry config.
// Sorted longest-first so longer matches take priority.
var knownTelemetryCompoundFields = []string{
	"insecure_skip_verify",
	"respect_debug_mode",
	"report_interval",
	"max_size",
}

// telemetrySectionNames are sub-sections within the telemetry config tree.
var telemetrySectionNames = map[string]bool{
	"cloud": true, "hub": true, "local": true, "filter": true,
	"tls": true, "batch": true, "events": true, "attributes": true, "sampling": true,
}

// mapTelemetryEnvKey maps the portion after "telemetry_" to a dotted path.
// Examples:
//
//	"enabled" -> "enabled"
//	"cloud_enabled" -> "cloud.enabled"
//	"cloud_tls_insecure_skip_verify" -> "cloud.tls.insecure_skip_verify"
//	"filter_respect_debug_mode" -> "filter.respect_debug_mode"
//	"hub_report_interval" -> "hub.report_interval"
func mapTelemetryEnvKey(key string) string {
	if key == "" {
		return ""
	}

	// Check if the entire key is a known compound field
	for _, compound := range knownTelemetryCompoundFields {
		if key == compound {
			return key
		}
	}

	// Try splitting at underscores
	for i := 0; i < len(key); i++ {
		if key[i] == '_' {
			prefix := key[:i]
			rest := key[i+1:]

			// Check if the rest starts with a known compound field
			for _, compound := range knownTelemetryCompoundFields {
				if strings.HasPrefix(rest, compound) {
					if len(rest) == len(compound) {
						return prefix + "." + compound
					}
					if rest[len(compound)] == '_' {
						return prefix + "." + compound + "." + mapTelemetryEnvKey(rest[len(compound)+1:])
					}
				}
			}

			// If prefix is a known section, recurse on the rest
			if telemetrySectionNames[prefix] {
				return prefix + "." + mapTelemetryEnvKey(rest)
			}
		}
	}

	return key
}

// mapOtelEnvKey maps OTEL_* env keys to telemetry.cloud.* settings paths.
// Examples:
//
//	"endpoint" -> "telemetry.cloud.endpoint"
//	"protocol" -> "telemetry.cloud.protocol"
//	"headers" -> "telemetry.cloud.headers"
//	"insecure" -> "telemetry.cloud.tls.insecure_skip_verify"
//	"ca_file" -> "telemetry.cloud.tls.ca_file"
func mapOtelEnvKey(key string) string {
	switch key {
	case "endpoint":
		return "telemetry.cloud.endpoint"
	case "protocol":
		return "telemetry.cloud.protocol"
	case "headers":
		return "telemetry.cloud.headers"
	case "insecure":
		return "telemetry.cloud.tls.insecure_skip_verify"
	case "ca_file":
		return "telemetry.cloud.tls.ca_file"
	default:
		return "telemetry.cloud." + key
	}
}

// ConvertV1ServerToGlobalConfig maps a V1ServerConfig (snake_case) to a GlobalConfig (camelCase).
// This allows server/broker commands to continue operating on GlobalConfig internally
// while loading from the versioned settings.yaml format.
func ConvertV1ServerToGlobalConfig(v1 *V1ServerConfig) *GlobalConfig {
	if v1 == nil {
		gc := DefaultGlobalConfig()
		return &gc
	}

	gc := DefaultGlobalConfig()

	// Mode
	if v1.Mode != "" {
		gc.Mode = v1.Mode
	}

	// Top-level fields
	if v1.LogLevel != "" {
		gc.LogLevel = v1.LogLevel
	}
	if v1.LogFormat != "" {
		gc.LogFormat = v1.LogFormat
	}

	// Hub server config
	if v1.Hub != nil {
		if v1.Hub.Port != 0 {
			gc.Hub.Port = v1.Hub.Port
		}
		if v1.Hub.Host != "" {
			gc.Hub.Host = v1.Hub.Host
		}
		if v1.Hub.HubID != "" {
			gc.Hub.HubID = v1.Hub.HubID
		}
		if v1.Hub.PublicURL != "" {
			gc.Hub.Endpoint = v1.Hub.PublicURL
		}
		if v1.Hub.ReadTimeout != "" {
			if d, err := time.ParseDuration(v1.Hub.ReadTimeout); err == nil {
				gc.Hub.ReadTimeout = d
			}
		}
		if v1.Hub.WriteTimeout != "" {
			if d, err := time.ParseDuration(v1.Hub.WriteTimeout); err == nil {
				gc.Hub.WriteTimeout = d
			}
		}
		if v1.Hub.CORS != nil {
			gc.Hub.CORSEnabled = v1.Hub.CORS.Enabled
			if v1.Hub.CORS.AllowedOrigins != nil {
				gc.Hub.CORSAllowedOrigins = v1.Hub.CORS.AllowedOrigins
			}
			if v1.Hub.CORS.AllowedMethods != nil {
				gc.Hub.CORSAllowedMethods = v1.Hub.CORS.AllowedMethods
			}
			if v1.Hub.CORS.AllowedHeaders != nil {
				gc.Hub.CORSAllowedHeaders = v1.Hub.CORS.AllowedHeaders
			}
			if v1.Hub.CORS.MaxAge != 0 {
				gc.Hub.CORSMaxAge = v1.Hub.CORS.MaxAge
			}
		}
		if v1.Hub.AdminEmails != nil {
			gc.Hub.AdminEmails = v1.Hub.AdminEmails
		}
		if v1.Hub.SoftDeleteRetention != "" {
			if d, err := time.ParseDuration(v1.Hub.SoftDeleteRetention); err == nil {
				gc.Hub.SoftDeleteRetention = d
			}
		}
		if v1.Hub.SoftDeleteRetainFiles != nil {
			gc.Hub.SoftDeleteRetainFiles = *v1.Hub.SoftDeleteRetainFiles
		}
	}

	// Broker config
	if v1.Broker != nil {
		gc.RuntimeBroker.Enabled = v1.Broker.Enabled
		if v1.Broker.Port != 0 {
			gc.RuntimeBroker.Port = v1.Broker.Port
		}
		if v1.Broker.Host != "" {
			gc.RuntimeBroker.Host = v1.Broker.Host
		}
		if v1.Broker.ReadTimeout != "" {
			if d, err := time.ParseDuration(v1.Broker.ReadTimeout); err == nil {
				gc.RuntimeBroker.ReadTimeout = d
			}
		}
		if v1.Broker.WriteTimeout != "" {
			if d, err := time.ParseDuration(v1.Broker.WriteTimeout); err == nil {
				gc.RuntimeBroker.WriteTimeout = d
			}
		}
		if v1.Broker.HubEndpoint != "" {
			gc.RuntimeBroker.HubEndpoint = v1.Broker.HubEndpoint
		}
		if v1.Broker.ContainerHubEndpoint != "" {
			gc.RuntimeBroker.ContainerHubEndpoint = v1.Broker.ContainerHubEndpoint
		}
		if v1.Broker.BrokerID != "" {
			gc.RuntimeBroker.BrokerID = v1.Broker.BrokerID
		}
		// Map BrokerName and BrokerNickname to BrokerName in GlobalConfig
		if v1.Broker.BrokerName != "" {
			gc.RuntimeBroker.BrokerName = v1.Broker.BrokerName
		} else if v1.Broker.BrokerNickname != "" {
			gc.RuntimeBroker.BrokerName = v1.Broker.BrokerNickname
		}
		if v1.Broker.CORS != nil {
			gc.RuntimeBroker.CORSEnabled = v1.Broker.CORS.Enabled
			if v1.Broker.CORS.AllowedOrigins != nil {
				gc.RuntimeBroker.CORSAllowedOrigins = v1.Broker.CORS.AllowedOrigins
			}
			if v1.Broker.CORS.AllowedMethods != nil {
				gc.RuntimeBroker.CORSAllowedMethods = v1.Broker.CORS.AllowedMethods
			}
			if v1.Broker.CORS.AllowedHeaders != nil {
				gc.RuntimeBroker.CORSAllowedHeaders = v1.Broker.CORS.AllowedHeaders
			}
			if v1.Broker.CORS.MaxAge != 0 {
				gc.RuntimeBroker.CORSMaxAge = v1.Broker.CORS.MaxAge
			}
		}
		if v1.Broker.AllowContainerScriptHarnesses != nil {
			gc.RuntimeBroker.AllowContainerScriptHarnesses = *v1.Broker.AllowContainerScriptHarnesses
		} else {
			gc.RuntimeBroker.AllowContainerScriptHarnesses = true
		}
	}

	// Database config
	if v1.Database != nil {
		if v1.Database.Driver != "" {
			gc.Database.Driver = v1.Database.Driver
		}
		if v1.Database.URL != "" {
			gc.Database.URL = v1.Database.URL
		}
	}

	// Auth config
	if v1.Auth != nil {
		gc.Auth.Enabled = v1.Auth.DevMode
		gc.Auth.Token = v1.Auth.DevToken
		gc.Auth.TokenFile = v1.Auth.DevTokenFile
		if v1.Auth.AuthorizedDomains != nil {
			gc.Auth.AuthorizedDomains = v1.Auth.AuthorizedDomains
		}
		if v1.Auth.UserAccessMode != "" {
			gc.Auth.UserAccessMode = v1.Auth.UserAccessMode
		}
	}

	// OAuth config
	if v1.OAuth != nil {
		if v1.OAuth.Web != nil {
			if v1.OAuth.Web.Google != nil {
				gc.OAuth.Web.Google.ClientID = v1.OAuth.Web.Google.ClientID
				gc.OAuth.Web.Google.ClientSecret = v1.OAuth.Web.Google.ClientSecret
			}
			if v1.OAuth.Web.GitHub != nil {
				gc.OAuth.Web.GitHub.ClientID = v1.OAuth.Web.GitHub.ClientID
				gc.OAuth.Web.GitHub.ClientSecret = v1.OAuth.Web.GitHub.ClientSecret
			}
		}
		if v1.OAuth.CLI != nil {
			if v1.OAuth.CLI.Google != nil {
				gc.OAuth.CLI.Google.ClientID = v1.OAuth.CLI.Google.ClientID
				gc.OAuth.CLI.Google.ClientSecret = v1.OAuth.CLI.Google.ClientSecret
			}
			if v1.OAuth.CLI.GitHub != nil {
				gc.OAuth.CLI.GitHub.ClientID = v1.OAuth.CLI.GitHub.ClientID
				gc.OAuth.CLI.GitHub.ClientSecret = v1.OAuth.CLI.GitHub.ClientSecret
			}
		}
		if v1.OAuth.Device != nil {
			if v1.OAuth.Device.Google != nil {
				gc.OAuth.Device.Google.ClientID = v1.OAuth.Device.Google.ClientID
				gc.OAuth.Device.Google.ClientSecret = v1.OAuth.Device.Google.ClientSecret
			}
			if v1.OAuth.Device.GitHub != nil {
				gc.OAuth.Device.GitHub.ClientID = v1.OAuth.Device.GitHub.ClientID
				gc.OAuth.Device.GitHub.ClientSecret = v1.OAuth.Device.GitHub.ClientSecret
			}
		}
	}

	// Storage config
	if v1.Storage != nil {
		if v1.Storage.Provider != "" {
			gc.Storage.Provider = v1.Storage.Provider
		}
		if v1.Storage.Bucket != "" {
			gc.Storage.Bucket = v1.Storage.Bucket
		}
		if v1.Storage.LocalPath != "" {
			gc.Storage.LocalPath = v1.Storage.LocalPath
		}
	}

	// Secrets config
	if v1.Secrets != nil {
		if v1.Secrets.Backend != "" {
			gc.Secrets.Backend = v1.Secrets.Backend
		}
		if v1.Secrets.GCPProjectID != "" {
			gc.Secrets.GCPProjectID = v1.Secrets.GCPProjectID
		}
		if v1.Secrets.GCPCredentials != "" {
			gc.Secrets.GCPCredentials = v1.Secrets.GCPCredentials
		}
	}

	// GitHub App
	if v1.GitHubApp != nil {
		gc.GitHubApp.AppID = v1.GitHubApp.AppID
		gc.GitHubApp.PrivateKeyPath = v1.GitHubApp.PrivateKeyPath
		gc.GitHubApp.PrivateKey = v1.GitHubApp.PrivateKey
		gc.GitHubApp.WebhookSecret = v1.GitHubApp.WebhookSecret
		gc.GitHubApp.APIBaseURL = v1.GitHubApp.APIBaseURL
		gc.GitHubApp.WebhooksEnabled = v1.GitHubApp.WebhooksEnabled
		gc.GitHubApp.InstallationURL = v1.GitHubApp.InstallationURL
	}

	return &gc
}

// ConvertGlobalToV1ServerConfig maps a GlobalConfig (camelCase) to a V1ServerConfig (snake_case).
// Used by AdaptLegacySettings and migration tooling.
func ConvertGlobalToV1ServerConfig(gc *GlobalConfig) *V1ServerConfig {
	if gc == nil {
		return &V1ServerConfig{}
	}

	v1 := &V1ServerConfig{
		Mode:      gc.Mode,
		LogLevel:  gc.LogLevel,
		LogFormat: gc.LogFormat,
	}

	// Hub server config
	v1Hub := &V1ServerHubConfig{
		Port:         gc.Hub.Port,
		Host:         gc.Hub.Host,
		HubID:        gc.Hub.HubID,
		PublicURL:    gc.Hub.Endpoint,
		ReadTimeout:  gc.Hub.ReadTimeout.String(),
		WriteTimeout: gc.Hub.WriteTimeout.String(),
		AdminEmails:  gc.Hub.AdminEmails,
		CORS: &V1CORSConfig{
			Enabled:        gc.Hub.CORSEnabled,
			AllowedOrigins: gc.Hub.CORSAllowedOrigins,
			AllowedMethods: gc.Hub.CORSAllowedMethods,
			AllowedHeaders: gc.Hub.CORSAllowedHeaders,
			MaxAge:         gc.Hub.CORSMaxAge,
		},
	}
	if gc.Hub.SoftDeleteRetention > 0 {
		v1Hub.SoftDeleteRetention = gc.Hub.SoftDeleteRetention.String()
	}
	if gc.Hub.SoftDeleteRetainFiles {
		retainFiles := true
		v1Hub.SoftDeleteRetainFiles = &retainFiles
	}
	v1.Hub = v1Hub

	// Broker config
	v1.Broker = &V1BrokerConfig{
		Enabled:                       gc.RuntimeBroker.Enabled,
		Port:                          gc.RuntimeBroker.Port,
		Host:                          gc.RuntimeBroker.Host,
		ReadTimeout:                   gc.RuntimeBroker.ReadTimeout.String(),
		WriteTimeout:                  gc.RuntimeBroker.WriteTimeout.String(),
		HubEndpoint:                   gc.RuntimeBroker.HubEndpoint,
		ContainerHubEndpoint:          gc.RuntimeBroker.ContainerHubEndpoint,
		BrokerID:                      gc.RuntimeBroker.BrokerID,
		BrokerName:                    gc.RuntimeBroker.BrokerName,
		AllowContainerScriptHarnesses: &gc.RuntimeBroker.AllowContainerScriptHarnesses,
		CORS: &V1CORSConfig{
			Enabled:        gc.RuntimeBroker.CORSEnabled,
			AllowedOrigins: gc.RuntimeBroker.CORSAllowedOrigins,
			AllowedMethods: gc.RuntimeBroker.CORSAllowedMethods,
			AllowedHeaders: gc.RuntimeBroker.CORSAllowedHeaders,
			MaxAge:         gc.RuntimeBroker.CORSMaxAge,
		},
	}

	// Database config
	v1.Database = &V1DatabaseConfig{
		Driver: gc.Database.Driver,
		URL:    gc.Database.URL,
	}

	// Auth config
	v1.Auth = &V1AuthConfig{
		DevMode:           gc.Auth.Enabled,
		DevToken:          gc.Auth.Token,
		DevTokenFile:      gc.Auth.TokenFile,
		AuthorizedDomains: gc.Auth.AuthorizedDomains,
		UserAccessMode:    gc.Auth.UserAccessMode,
	}

	// OAuth config
	v1.OAuth = &V1OAuthConfig{
		Web: &V1OAuthClientConfig{
			Google: &V1OAuthProviderConfig{ClientID: gc.OAuth.Web.Google.ClientID, ClientSecret: gc.OAuth.Web.Google.ClientSecret},
			GitHub: &V1OAuthProviderConfig{ClientID: gc.OAuth.Web.GitHub.ClientID, ClientSecret: gc.OAuth.Web.GitHub.ClientSecret},
		},
		CLI: &V1OAuthClientConfig{
			Google: &V1OAuthProviderConfig{ClientID: gc.OAuth.CLI.Google.ClientID, ClientSecret: gc.OAuth.CLI.Google.ClientSecret},
			GitHub: &V1OAuthProviderConfig{ClientID: gc.OAuth.CLI.GitHub.ClientID, ClientSecret: gc.OAuth.CLI.GitHub.ClientSecret},
		},
		Device: &V1OAuthClientConfig{
			Google: &V1OAuthProviderConfig{ClientID: gc.OAuth.Device.Google.ClientID, ClientSecret: gc.OAuth.Device.Google.ClientSecret},
			GitHub: &V1OAuthProviderConfig{ClientID: gc.OAuth.Device.GitHub.ClientID, ClientSecret: gc.OAuth.Device.GitHub.ClientSecret},
		},
	}

	// Storage config
	v1.Storage = &V1StorageConfig{
		Provider:  gc.Storage.Provider,
		Bucket:    gc.Storage.Bucket,
		LocalPath: gc.Storage.LocalPath,
	}

	// Secrets config
	v1.Secrets = &V1SecretsConfig{
		Backend:        gc.Secrets.Backend,
		GCPProjectID:   gc.Secrets.GCPProjectID,
		GCPCredentials: gc.Secrets.GCPCredentials,
	}

	// GitHub App config
	if gc.GitHubApp.AppID != 0 {
		v1.GitHubApp = &V1GitHubAppConfig{
			AppID:           gc.GitHubApp.AppID,
			PrivateKeyPath:  gc.GitHubApp.PrivateKeyPath,
			PrivateKey:      gc.GitHubApp.PrivateKey,
			WebhookSecret:   gc.GitHubApp.WebhookSecret,
			APIBaseURL:      gc.GitHubApp.APIBaseURL,
			WebhooksEnabled: gc.GitHubApp.WebhooksEnabled,
			InstallationURL: gc.GitHubApp.InstallationURL,
		}
	}

	return v1
}

// AdaptLegacySettings converts a legacy Settings struct to VersionedSettings.
// Returns the adapted settings and a slice of deprecation warnings.
// This is a pure function with no I/O.
func AdaptLegacySettings(legacy *Settings) (*VersionedSettings, []string) {
	if legacy == nil {
		return &VersionedSettings{SchemaVersion: "1"}, nil
	}

	var warnings []string

	vs := &VersionedSettings{
		SchemaVersion:   "1",
		ActiveProfile:   legacy.ActiveProfile,
		DefaultTemplate: legacy.DefaultTemplate,
	}

	// Adapt Hub config
	if legacy.Hub != nil {
		vs.Hub = &V1HubClientConfig{
			Enabled:   legacy.Hub.Enabled,
			Linked:    legacy.Hub.Linked,
			Endpoint:  legacy.Hub.Endpoint,
			ProjectID:   legacy.Hub.ProjectID,
			LocalOnly: legacy.Hub.LocalOnly,
		}
		if legacy.Hub.Token != "" {
			warnings = append(warnings, "hub.token is deprecated; use server.auth.dev_token for dev mode authentication")
		}
		if legacy.Hub.APIKey != "" {
			warnings = append(warnings, "hub.apiKey is deprecated; API key authentication is no longer supported")
		}
		if legacy.Hub.BrokerID != "" || legacy.Hub.BrokerNickname != "" || legacy.Hub.BrokerToken != "" {
			if vs.Server == nil {
				vs.Server = &V1ServerConfig{}
			}
			if vs.Server.Broker == nil {
				vs.Server.Broker = &V1BrokerConfig{}
			}
			if legacy.Hub.BrokerID != "" {
				vs.Server.Broker.BrokerID = legacy.Hub.BrokerID
				warnings = append(warnings, "hub.brokerId is deprecated; moved to server.broker.broker_id")
			}
			if legacy.Hub.BrokerNickname != "" {
				vs.Server.Broker.BrokerNickname = legacy.Hub.BrokerNickname
				warnings = append(warnings, "hub.brokerNickname is deprecated; moved to server.broker.broker_nickname")
			}
			if legacy.Hub.BrokerToken != "" {
				vs.Server.Broker.BrokerToken = legacy.Hub.BrokerToken
				warnings = append(warnings, "hub.brokerToken is deprecated; moved to server.broker.broker_token")
			}
		}
		if legacy.Hub.LastSyncedAt != "" {
			warnings = append(warnings, "hub.lastSyncedAt is deprecated; moved to state.yaml")
		}
	}

	// Adapt CLI config
	if legacy.CLI != nil {
		vs.CLI = &V1CLIConfig{
			AutoHelp: legacy.CLI.AutoHelp,
		}
	}

	// Adapt Runtimes — set Type from map key
	if legacy.Runtimes != nil {
		vs.Runtimes = make(map[string]V1RuntimeConfig, len(legacy.Runtimes))
		for name, rc := range legacy.Runtimes {
			vs.Runtimes[name] = V1RuntimeConfig{
				Type:      name,
				Host:      rc.Host,
				Context:   rc.Context,
				Namespace: rc.Namespace,
				Env:       rc.Env,
				Sync:      rc.Sync,
			}
		}
	}

	// Adapt Harnesses → HarnessConfigs — set Harness from map key
	if legacy.Harnesses != nil {
		vs.HarnessConfigs = make(map[string]HarnessConfigEntry, len(legacy.Harnesses))
		for name, hc := range legacy.Harnesses {
			vs.HarnessConfigs[name] = HarnessConfigEntry{
				Harness:          name,
				Image:            hc.Image,
				User:             hc.User,
				Env:              hc.Env,
				Volumes:          hc.Volumes,
				AuthSelectedType: hc.AuthSelectedType,
			}
		}
		warnings = append(warnings, "harnesses is deprecated; renamed to harness_configs with a required 'harness' field")
	}

	// Adapt Profiles
	if legacy.Profiles != nil {
		vs.Profiles = make(map[string]V1ProfileConfig, len(legacy.Profiles))
		for name, pc := range legacy.Profiles {
			profile := V1ProfileConfig{
				Runtime:   pc.Runtime,
				Env:       pc.Env,
				Volumes:   pc.Volumes,
				Resources: pc.Resources,
			}
			// Convert HarnessOverride → V1HarnessOverride (camelCase → snake_case tags)
			if pc.HarnessOverrides != nil {
				profile.HarnessOverrides = make(map[string]V1HarnessOverride, len(pc.HarnessOverrides))
				for hk, ho := range pc.HarnessOverrides {
					profile.HarnessOverrides[hk] = V1HarnessOverride(ho)
				}
			}
			vs.Profiles[name] = profile
		}
	}

	// Warn about Bucket config
	if legacy.Bucket != nil && (legacy.Bucket.Provider != "" || legacy.Bucket.Name != "" || legacy.Bucket.Prefix != "") {
		warnings = append(warnings, "bucket config is deprecated; will consolidate into server.storage")
	}

	return vs, warnings
}

// convertVersionedToLegacy maps VersionedSettings back to legacy Settings.
// Used by GetDefaultSettingsData() so the legacy Koanf loader receives valid data
// after the default file changes format.
func convertVersionedToLegacy(vs *VersionedSettings) *Settings {
	if vs == nil {
		return &Settings{}
	}

	s := &Settings{
		ActiveProfile:   vs.ActiveProfile,
		DefaultTemplate: vs.DefaultTemplate,
		WorkspacePath:   vs.WorkspacePath,
	}

	// Convert Hub
	if vs.Hub != nil {
		s.Hub = &HubClientConfig{
			Enabled:   vs.Hub.Enabled,
			Linked:    vs.Hub.Linked,
			Endpoint:  vs.Hub.Endpoint,
			ProjectID:   vs.Hub.ProjectID,
			LocalOnly: vs.Hub.LocalOnly,
		}
	}

	// Map broker identity fields from v1 Server.Broker back to legacy Hub
	if vs.Server != nil && vs.Server.Broker != nil {
		if s.Hub == nil {
			s.Hub = &HubClientConfig{}
		}
		if vs.Server.Broker.BrokerID != "" {
			s.Hub.BrokerID = vs.Server.Broker.BrokerID
		}
		if vs.Server.Broker.BrokerToken != "" {
			s.Hub.BrokerToken = vs.Server.Broker.BrokerToken
		}
		if vs.Server.Broker.BrokerNickname != "" {
			s.Hub.BrokerNickname = vs.Server.Broker.BrokerNickname
		}
	}

	// Convert CLI
	if vs.CLI != nil {
		s.CLI = &CLIConfig{
			AutoHelp: vs.CLI.AutoHelp,
		}
	}

	// Convert Runtimes — drop Type field
	if vs.Runtimes != nil {
		s.Runtimes = make(map[string]RuntimeConfig, len(vs.Runtimes))
		for name, rc := range vs.Runtimes {
			s.Runtimes[name] = RuntimeConfig{
				Host:      rc.Host,
				Context:   rc.Context,
				Namespace: rc.Namespace,
				Env:       rc.Env,
				Sync:      rc.Sync,
			}
		}
	}

	// Convert HarnessConfigs → Harnesses — drop new fields (Model, Args, Harness)
	if vs.HarnessConfigs != nil {
		s.Harnesses = make(map[string]HarnessConfig, len(vs.HarnessConfigs))
		for name, hc := range vs.HarnessConfigs {
			s.Harnesses[name] = HarnessConfig{
				Image:            hc.Image,
				User:             hc.User,
				Env:              hc.Env,
				Volumes:          hc.Volumes,
				AuthSelectedType: hc.AuthSelectedType,
			}
		}
	}

	// Convert Profiles — drop new fields (DefaultTemplate, DefaultHarnessConfig)
	// Convert V1HarnessOverride → HarnessOverride (snake_case → camelCase tags)
	if vs.Profiles != nil {
		s.Profiles = make(map[string]ProfileConfig, len(vs.Profiles))
		for name, pc := range vs.Profiles {
			profile := ProfileConfig{
				Runtime:   pc.Runtime,
				Env:       pc.Env,
				Volumes:   pc.Volumes,
				Resources: pc.Resources,
			}
			if pc.HarnessOverrides != nil {
				profile.HarnessOverrides = make(map[string]HarnessOverride, len(pc.HarnessOverrides))
				for hk, ho := range pc.HarnessOverrides {
					profile.HarnessOverrides[hk] = HarnessOverride(ho)
				}
			}
			s.Profiles[name] = profile
		}
	}

	return s
}

// detectHierarchyFormat checks settings files in the global and project directories
// to determine if any user file uses the versioned format.
// Returns true if any user file is versioned (has schema_version).
func detectHierarchyFormat(projectPath string) (hasVersioned bool) {
	// Check global settings
	globalDir, _ := GetGlobalDir()
	if globalDir != "" {
		if path := GetSettingsPath(globalDir); path != "" {
			if data, err := os.ReadFile(path); err == nil {
				if version, _ := DetectSettingsFormat(data); version != "" {
					return true
				}
			}
		}
	}

	// Check project settings
	effectiveProjectPath := resolveEffectiveProjectPath(projectPath)
	if effectiveProjectPath != "" && effectiveProjectPath != globalDir {
		if path := GetSettingsPath(effectiveProjectPath); path != "" {
			if data, err := os.ReadFile(path); err == nil {
				if version, _ := DetectSettingsFormat(data); version != "" {
					return true
				}
			}
		}
	}

	return false
}

// LoadEffectiveSettings is a unified entry point that detects the settings format
// and loads using the appropriate path.
// - If any user file is versioned → uses LoadVersionedSettings
// - If all user files are legacy or absent → uses LoadSettingsKoanf + AdaptLegacySettings
// Returns (settings, deprecation_warnings, error).
func LoadEffectiveSettings(projectPath string) (*VersionedSettings, []string, error) {
	if detectHierarchyFormat(projectPath) {
		vs, err := LoadVersionedSettings(projectPath)
		if err != nil {
			return nil, nil, fmt.Errorf("loading versioned settings: %w", err)
		}
		return vs, nil, nil
	}

	// Legacy path: load via existing loader, then adapt
	legacy, err := LoadSettingsKoanf(projectPath)
	if err != nil {
		return nil, nil, fmt.Errorf("loading legacy settings: %w", err)
	}
	vs, warnings := AdaptLegacySettings(legacy)
	return vs, warnings, nil
}

// MigrationResult reports what happened during a migration.
type MigrationResult struct {
	Path             string   `json:"path"`               // settings file that was migrated
	BackupPath       string   `json:"backup_path"`        // path of backup file created
	Format           string   `json:"format"`             // "legacy" or "versioned" (already up-to-date)
	Warnings         []string `json:"warnings"`           // deprecation warnings from AdaptLegacySettings
	StateMigrated    bool     `json:"state_migrated"`     // true if hub.lastSyncedAt was moved to state.yaml
	ServerMigrated   bool     `json:"server_migrated"`    // true if server.yaml was merged into settings
	ServerBackupPath string   `json:"server_backup_path"` // path of server.yaml backup created
	WasJSON          bool     `json:"was_json"`           // true if source was .json format
	Skipped          bool     `json:"skipped"`            // true if file was already versioned or missing
	SkipReason       string   `json:"skip_reason"`        // reason for skipping
}

// loadSingleFileVersioned loads a single settings file from dir into a VersionedSettings struct.
// Unlike LoadVersionedSettings, this does NOT merge defaults, global settings, or env vars.
// It reads only the file at the given directory, which is needed for UpdateVersionedSetting
// to avoid clobbering other layers.
func LoadSingleFileVersioned(dir string) (*VersionedSettings, error) {
	settingsPath := GetSettingsPath(dir)
	if settingsPath == "" {
		// No file exists yet — return empty versioned settings
		return &VersionedSettings{SchemaVersion: "1"}, nil
	}

	data, err := os.ReadFile(settingsPath)
	if err != nil {
		return nil, fmt.Errorf("failed to read %s: %w", settingsPath, err)
	}

	var vs VersionedSettings
	ext := filepath.Ext(settingsPath)
	if ext == ".json" {
		if err := json.Unmarshal(data, &vs); err != nil {
			return nil, fmt.Errorf("failed to parse JSON settings at %s: %w", settingsPath, err)
		}
	} else {
		if err := yamlv3.Unmarshal(data, &vs); err != nil {
			return nil, fmt.Errorf("failed to parse YAML settings at %s: %w", settingsPath, err)
		}
	}

	// Ensure schema_version is set
	if vs.SchemaVersion == "" {
		vs.SchemaVersion = "1"
	}

	return &vs, nil
}

// UpdateVersionedSetting updates a specific setting key in a v1 versioned settings file.
// It loads only the single file at dir (not merged settings), maps legacy key names to
// their v1 equivalents, updates the appropriate field, and saves via SaveVersionedSettings.
func UpdateVersionedSetting(dir string, key string, value string) error {
	vs, err := LoadSingleFileVersioned(dir)
	if err != nil {
		return err
	}

	switch key {
	// --- Direct mappings (same in both formats) ---
	case "active_profile":
		vs.ActiveProfile = value
	case "default_template":
		vs.DefaultTemplate = value
	case "default_harness_config":
		vs.DefaultHarnessConfig = value
	case "workspace_path":
		vs.WorkspacePath = value
	case "image_registry":
		vs.ImageRegistry = value
	case "cli.autohelp":
		if vs.CLI == nil {
			vs.CLI = &V1CLIConfig{}
		}
		autohelp := value == "true"
		vs.CLI.AutoHelp = &autohelp

	// --- grove_id: top-level in legacy, hub.grove_id in v1 ---
	case "project_id", "grove_id":
		if vs.Hub == nil {
			vs.Hub = &V1HubClientConfig{}
		}
		vs.Hub.ProjectID = value

	// --- Hub client settings ---
	case "hub.enabled":
		if vs.Hub == nil {
			vs.Hub = &V1HubClientConfig{}
		}
		enabled := value == "true"
		vs.Hub.Enabled = &enabled
	case "hub.linked":
		if vs.Hub == nil {
			vs.Hub = &V1HubClientConfig{}
		}
		linked := value == "true"
		vs.Hub.Linked = &linked
	case "hub.endpoint":
		if vs.Hub == nil {
			vs.Hub = &V1HubClientConfig{}
		}
		vs.Hub.Endpoint = value
	case "hub.project_id", "hub.grove_id", "hub.projectId", "hub.groveId":
		if vs.Hub == nil {
			vs.Hub = &V1HubClientConfig{}
		}
		vs.Hub.ProjectID = value
	case "hub.local_only":
		if vs.Hub == nil {
			vs.Hub = &V1HubClientConfig{}
		}
		localOnly := value == "true"
		vs.Hub.LocalOnly = &localOnly

	// --- Broker identity: legacy hub.broker* → v1 server.broker.* ---
	case "hub.brokerId":
		if vs.Server == nil {
			vs.Server = &V1ServerConfig{}
		}
		if vs.Server.Broker == nil {
			vs.Server.Broker = &V1BrokerConfig{}
		}
		vs.Server.Broker.BrokerID = value
	case "hub.brokerToken":
		if vs.Server == nil {
			vs.Server = &V1ServerConfig{}
		}
		if vs.Server.Broker == nil {
			vs.Server.Broker = &V1BrokerConfig{}
		}
		vs.Server.Broker.BrokerToken = value
	case "hub.brokerNickname":
		if vs.Server == nil {
			vs.Server = &V1ServerConfig{}
		}
		if vs.Server.Broker == nil {
			vs.Server.Broker = &V1BrokerConfig{}
		}
		vs.Server.Broker.BrokerNickname = value

	// --- Deprecated keys: skip silently in v1 ---
	case "hub.token", "hub.apiKey", "hub.lastSyncedAt":
		// These fields don't exist in v1 — skip without error
		return nil

	// --- Bucket: not present in v1 ---
	case "bucket.provider", "bucket.name", "bucket.prefix":
		// Bucket config is deprecated in v1 — skip without error
		return nil

	default:
		// Handle hub_connections.* keys: skip in v1 format (not supported)
		if strings.HasPrefix(key, "hub_connections.") {
			return nil
		}
		return fmt.Errorf("unknown or complex setting key: %s (manual edit recommended for registries)", key)
	}

	return SaveVersionedSettings(dir, vs)
}

// GetVersionedSettingValue retrieves a specific setting value from a VersionedSettings struct.
// It mirrors the keys supported by UpdateVersionedSetting for read access.
func GetVersionedSettingValue(vs *VersionedSettings, key string) (string, error) {
	switch key {
	case "active_profile":
		return vs.ActiveProfile, nil
	case "default_template":
		return vs.DefaultTemplate, nil
	case "default_harness_config":
		return vs.DefaultHarnessConfig, nil
	case "workspace_path":
		return vs.WorkspacePath, nil
	case "image_registry":
		return vs.ImageRegistry, nil
	case "cli.autohelp":
		if vs.CLI != nil && vs.CLI.AutoHelp != nil {
			if *vs.CLI.AutoHelp {
				return "true", nil
			}
			return "false", nil
		}
		return "", nil
	case "project_id", "grove_id":
		if vs.Hub != nil {
			return vs.Hub.ProjectID, nil
		}
		return "", nil
	case "hub.enabled":
		if vs.Hub != nil && vs.Hub.Enabled != nil {
			if *vs.Hub.Enabled {
				return "true", nil
			}
			return "false", nil
		}
		return "", nil
	case "hub.linked":
		if vs.Hub != nil && vs.Hub.Linked != nil {
			if *vs.Hub.Linked {
				return "true", nil
			}
			return "false", nil
		}
		return "", nil
	case "hub.endpoint":
		if vs.Hub != nil {
			return vs.Hub.Endpoint, nil
		}
		return "", nil
	case "hub.project_id", "hub.grove_id", "hub.projectId", "hub.groveId":
		if vs.Hub != nil {
			return vs.Hub.ProjectID, nil
		}
		return "", nil
	case "hub.local_only":
		if vs.Hub != nil && vs.Hub.LocalOnly != nil {
			if *vs.Hub.LocalOnly {
				return "true", nil
			}
			return "false", nil
		}
		return "", nil
	case "hub.brokerId":
		if vs.Server != nil && vs.Server.Broker != nil {
			return vs.Server.Broker.BrokerID, nil
		}
		return "", nil
	case "hub.brokerToken":
		if vs.Server != nil && vs.Server.Broker != nil {
			return vs.Server.Broker.BrokerToken, nil
		}
		return "", nil
	case "hub.brokerNickname":
		if vs.Server != nil && vs.Server.Broker != nil {
			return vs.Server.Broker.BrokerNickname, nil
		}
		return "", nil
	}

	return "", fmt.Errorf("unknown or complex setting key: %s", key)
}

// SaveVersionedSettings writes a VersionedSettings struct as YAML to settings.yaml in dir.
func SaveVersionedSettings(dir string, vs *VersionedSettings) error {
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("failed to create directory %s: %w", dir, err)
	}

	data, err := yamlv3.Marshal(vs)
	if err != nil {
		return fmt.Errorf("failed to marshal versioned settings: %w", err)
	}

	targetPath := filepath.Join(dir, "settings.yaml")
	return os.WriteFile(targetPath, data, 0644)
}

// MigrateSettingsFile migrates a single legacy settings file in dir to versioned format.
// If a server.yaml exists in the same directory, it is also merged into the settings
// under the "server" key and backed up.
// If dryRun is true, no files are written.
// Returns MigrationResult describing what was (or would be) done.
func MigrateSettingsFile(dir string, dryRun bool) (*MigrationResult, error) {
	result := &MigrationResult{}

	// 1. Find settings file
	settingsPath := GetSettingsPath(dir)
	if settingsPath == "" {
		result.Skipped = true
		result.SkipReason = "no settings file found"
		return result, nil
	}
	result.Path = settingsPath

	// 2. Read and detect format
	data, err := os.ReadFile(settingsPath)
	if err != nil {
		return nil, fmt.Errorf("failed to read %s: %w", settingsPath, err)
	}

	version, isLegacy := DetectSettingsFormat(data)
	if version != "" {
		result.Skipped = true
		result.SkipReason = fmt.Sprintf("already versioned (schema_version: %s)", version)
		result.Format = "versioned"
		return result, nil
	}

	result.Format = "legacy"
	result.WasJSON = filepath.Ext(settingsPath) == ".json"

	// 3. Parse legacy settings
	var legacy Settings
	if result.WasJSON {
		if err := json.Unmarshal(data, &legacy); err != nil {
			return nil, fmt.Errorf("failed to parse JSON settings: %w", err)
		}
	} else {
		if err := yamlv3.Unmarshal(data, &legacy); err != nil {
			return nil, fmt.Errorf("failed to parse YAML settings: %w", err)
		}
	}

	// If file has no legacy indicators and is effectively empty, still migrate it
	// (add schema_version to make it versioned)
	if !isLegacy && version == "" {
		// Minimal or empty file — still convert
	}

	// 4. Convert via AdaptLegacySettings
	vs, warnings := AdaptLegacySettings(&legacy)
	result.Warnings = warnings

	// 4b. Check for server.yaml and merge if present
	serverPath := GetServerConfigPath(dir)
	if serverPath != "" {
		serverGC, err := loadServerConfigOnly(serverPath)
		if err != nil {
			result.Warnings = append(result.Warnings,
				fmt.Sprintf("failed to load %s: %v (skipping server migration)", serverPath, err))
		} else {
			v1Server := ConvertGlobalToV1ServerConfig(serverGC)
			// Merge: preserve broker identity from legacy hub settings if the
			// server.yaml doesn't already have them.
			legacyBroker := vs.Server
			vs.Server = v1Server
			if legacyBroker != nil && legacyBroker.Broker != nil {
				if vs.Server.Broker == nil {
					vs.Server.Broker = legacyBroker.Broker
				} else {
					if vs.Server.Broker.BrokerID == "" {
						vs.Server.Broker.BrokerID = legacyBroker.Broker.BrokerID
					}
					if vs.Server.Broker.BrokerNickname == "" {
						vs.Server.Broker.BrokerNickname = legacyBroker.Broker.BrokerNickname
					}
					if vs.Server.Broker.BrokerToken == "" {
						vs.Server.Broker.BrokerToken = legacyBroker.Broker.BrokerToken
					}
				}
			}
			result.ServerMigrated = true
			result.Warnings = append(result.Warnings,
				fmt.Sprintf("server.yaml merged into settings.yaml under 'server' key (source: %s)", serverPath))
		}
	}

	// 5. Handle hub.lastSyncedAt: migrate to state.yaml
	if legacy.Hub != nil && legacy.Hub.LastSyncedAt != "" {
		result.StateMigrated = true
		if !dryRun {
			state, err := LoadProjectState(dir)
			if err != nil {
				return nil, fmt.Errorf("failed to load grove state: %w", err)
			}
			state.LastSyncedAt = legacy.Hub.LastSyncedAt
			if err := SaveProjectState(dir, state); err != nil {
				return nil, fmt.Errorf("failed to save grove state: %w", err)
			}
		}
	}

	// 6. Validate the output
	outputData, err := yamlv3.Marshal(vs)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal converted settings: %w", err)
	}

	validationErrors, err := ValidateSettings(outputData, "1")
	if err != nil {
		return nil, fmt.Errorf("validation error: %w", err)
	}
	if len(validationErrors) > 0 {
		var errMsgs []string
		for _, ve := range validationErrors {
			errMsgs = append(errMsgs, ve.Error())
		}
		return nil, fmt.Errorf("migrated settings failed validation: %s", strings.Join(errMsgs, "; "))
	}

	// 7. If dryRun, return result without writing
	if dryRun {
		return result, nil
	}

	// 8. Back up the original settings file
	backupPath := getBackupPath(settingsPath)
	if err := os.Rename(settingsPath, backupPath); err != nil {
		return nil, fmt.Errorf("failed to create backup %s: %w", backupPath, err)
	}
	result.BackupPath = backupPath

	// 8b. Back up server.yaml if it was merged
	if result.ServerMigrated && serverPath != "" {
		serverBackup := getBackupPath(serverPath)
		if err := os.Rename(serverPath, serverBackup); err != nil {
			// Non-fatal: settings migration succeeded, server.yaml backup failed
			result.Warnings = append(result.Warnings,
				fmt.Sprintf("warning: failed to back up %s: %v", serverPath, err))
		} else {
			result.ServerBackupPath = serverBackup
		}
	}

	// 9. Write versioned settings
	if err := SaveVersionedSettings(dir, vs); err != nil {
		// Attempt to restore backups on failure
		_ = os.Rename(backupPath, settingsPath)
		if result.ServerBackupPath != "" {
			_ = os.Rename(result.ServerBackupPath, serverPath)
		}
		return nil, fmt.Errorf("failed to write versioned settings: %w", err)
	}

	return result, nil
}

// loadServerConfigOnly loads a GlobalConfig from a single server.yaml file
// without applying defaults or environment variable overrides.
func loadServerConfigOnly(path string) (*GlobalConfig, error) {
	k := koanf.New(".")
	if err := k.Load(file.Provider(path), yaml.Parser()); err != nil {
		return nil, fmt.Errorf("failed to load %s: %w", path, err)
	}
	var gc GlobalConfig
	if err := k.Unmarshal("", &gc); err != nil {
		return nil, fmt.Errorf("failed to unmarshal server config: %w", err)
	}
	return &gc, nil
}

// getBackupPath returns a backup file path that does not already exist.
// Uses <path>.bak, <path>.bak.1, <path>.bak.2, etc.
func getBackupPath(path string) string {
	backup := path + ".bak"
	if _, err := os.Stat(backup); os.IsNotExist(err) {
		return backup
	}
	for i := 1; ; i++ {
		numbered := fmt.Sprintf("%s.bak.%d", path, i)
		if _, err := os.Stat(numbered); os.IsNotExist(err) {
			return numbered
		}
	}
}
