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
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	yamlv3 "gopkg.in/yaml.v3"
)

// Well-known secret keys for chat integration credentials stored via the secrets system.
const (
	// Telegram
	SecretTelegramBotToken   = "TELEGRAM_BOT_TOKEN"
	SecretTelegramWebhookKey = "TELEGRAM_WEBHOOK_SECRET"

	// Discord
	SecretDiscordBotToken  = "DISCORD_BOT_TOKEN"
	SecretDiscordPublicKey = "DISCORD_PUBLIC_KEY"

	// Slack
	SecretSlackBotToken      = "SLACK_BOT_TOKEN"
	SecretSlackAppToken      = "SLACK_APP_TOKEN"
	SecretSlackSigningSecret = "SLACK_SIGNING_SECRET"

	// Google Chat
	SecretGChatSigningKey = "GCHAT_SIGNING_KEY"
)

// IntegrationConfigProvider defines the interface for reading and writing
// per-integration configuration. Implementations handle only non-sensitive
// settings; secrets are managed separately via SecretBackend.
type IntegrationConfigProvider interface {
	// Load reads the integration configuration and returns its contents as a
	// flat key-value map suitable for merging into a plugin's Config map.
	Load(ctx context.Context) (map[string]string, error)

	// Save writes the given key-value map to the integration configuration store.
	Save(ctx context.Context, config map[string]string) error
}

// YAMLConfigProvider reads and writes per-integration YAML config files.
// The YAML file is a flat map[string]string at the top level.
type YAMLConfigProvider struct {
	path string
}

// NewYAMLConfigProvider creates a new YAMLConfigProvider for the given file path.
// If path is relative, it is resolved relative to the global scion config dir (~/.scion/).
func NewYAMLConfigProvider(path string) (*YAMLConfigProvider, error) {
	if path == "" {
		return nil, fmt.Errorf("config file path is required")
	}

	resolved := path
	if strings.HasPrefix(resolved, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("resolve home dir: %w", err)
		}
		resolved = filepath.Join(home, resolved[2:])
	}
	if !filepath.IsAbs(resolved) {
		globalDir, err := GetGlobalDir()
		if err != nil {
			return nil, fmt.Errorf("resolve config dir: %w", err)
		}
		resolved = filepath.Join(globalDir, resolved)
	}

	return &YAMLConfigProvider{path: resolved}, nil
}

// Path returns the resolved absolute path to the config file.
func (p *YAMLConfigProvider) Path() string {
	return p.path
}

// Load reads the YAML config file and returns its contents as a flat key-value map.
// Returns an empty map (not an error) if the file does not exist.
// The context parameter is accepted for interface compliance but unused by
// the file-based provider.
func (p *YAMLConfigProvider) Load(_ context.Context) (map[string]string, error) {
	data, err := os.ReadFile(p.path)
	if err != nil {
		if os.IsNotExist(err) {
			return make(map[string]string), nil
		}
		return nil, fmt.Errorf("read config file %s: %w", p.path, err)
	}

	var raw map[string]interface{}
	if err := yamlv3.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("parse config file %s: %w", p.path, err)
	}

	result := make(map[string]string, len(raw))
	for k, v := range raw {
		result[k] = fmt.Sprintf("%v", v)
	}
	return result, nil
}

// Save writes the given key-value map to the YAML config file.
// The parent directory is created if it does not exist.
// The context parameter is accepted for interface compliance but unused by
// the file-based provider.
func (p *YAMLConfigProvider) Save(_ context.Context, config map[string]string) error {
	dir := filepath.Dir(p.path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return fmt.Errorf("create config dir %s: %w", dir, err)
	}

	data, err := yamlv3.Marshal(config)
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}

	return os.WriteFile(p.path, data, 0600)
}

// IntegrationSecretMapping maps a secret backend key to the config key that a
// plugin's Configure() method expects.
type IntegrationSecretMapping struct {
	SecretKey string
	ConfigKey string
}

// PluginSecretKeyMap maps plugin names to their well-known secret keys and the
// corresponding plugin config keys.
var PluginSecretKeyMap = map[string][]IntegrationSecretMapping{
	"telegram": {
		{SecretTelegramBotToken, "bot_token"},
		{SecretTelegramWebhookKey, "webhook_secret"},
	},
	"discord": {
		{SecretDiscordBotToken, "bot_token"},
		{SecretDiscordPublicKey, "public_key"},
	},
	"slack": {
		{SecretSlackBotToken, "bot_token"},
		{SecretSlackAppToken, "app_token"},
		{SecretSlackSigningSecret, "signing_secret"},
	},
	"chat-app": {
		{SecretGChatSigningKey, "signing_key"},
	},
}

// wiringKeys are config keys that always come from settings.yaml inline config,
// never from the per-plugin YAML file.
var wiringKeys = map[string]bool{
	"mode":            true,
	"path":            true,
	"address":         true,
	"tls_cert_file":   true,
	"tls_key_file":    true,
	"tls_ca_file":     true,
	"tls_skip_verify": true,
	"config_file":     true,
}

// secretConfigKeys collects all config-level key names that map to secret
// backend entries (e.g. "bot_token", "signing_key"). Built once from
// PluginSecretKeyMap.
var secretConfigKeys map[string]bool

func init() {
	secretConfigKeys = make(map[string]bool)
	for _, mappings := range PluginSecretKeyMap {
		for _, m := range mappings {
			secretConfigKeys[m.ConfigKey] = true
		}
	}
}

// IsSecretConfigKey reports whether key is a plugin config key that maps to a
// secret backend entry (e.g. "bot_token", "signing_secret").
func IsSecretConfigKey(key string) bool {
	return secretConfigKeys[key]
}

// backendSecretKeys lists the well-known secret backend key names
// (TELEGRAM_BOT_TOKEN, etc.) for stripping from config maps.
var backendSecretKeys = []string{
	SecretTelegramBotToken, SecretTelegramWebhookKey,
	SecretDiscordBotToken, SecretDiscordPublicKey,
	SecretSlackBotToken, SecretSlackAppToken, SecretSlackSigningSecret,
	SecretGChatSigningKey,
}

// ResolvePluginConfig builds the config map for a plugin with consistent precedence.
// If configFile is set, the file is the source of truth for non-wiring keys.
// Inline config is only used for wiring keys or as fallback when no config_file exists.
// Secret config keys (bot_token, signing_key, etc.) are always stripped from inline
// config so that the secret backend takes precedence.
func ResolvePluginConfig(configFile string, inlineConfig map[string]string) (map[string]string, error) {
	// Build a clean copy of inline config with secret keys stripped.
	cleanedInline := make(map[string]string, len(inlineConfig))
	for k, v := range inlineConfig {
		if secretConfigKeys[k] {
			continue
		}
		cleanedInline[k] = v
	}
	// Also strip backend key names (same filtering applied to file config).
	for _, sk := range backendSecretKeys {
		delete(cleanedInline, sk)
		delete(cleanedInline, strings.ToLower(sk))
	}

	if configFile == "" {
		return cleanedInline, nil
	}

	provider, err := NewYAMLConfigProvider(configFile)
	if err != nil {
		return nil, err
	}

	fileConfig, err := provider.Load(context.Background())
	if err != nil {
		return nil, err
	}

	// Filter backend secret key names from file config.
	for _, sk := range backendSecretKeys {
		delete(fileConfig, sk)
		delete(fileConfig, strings.ToLower(sk))
	}

	// File is the base for non-wiring keys.
	merged := make(map[string]string, len(fileConfig)+len(cleanedInline))
	for k, v := range fileConfig {
		merged[k] = v
	}

	// Add only wiring keys from inline; log a deprecation warning for
	// any non-wiring keys that would be silently ignored.
	var deprecated []string
	for k, v := range cleanedInline {
		if wiringKeys[k] {
			merged[k] = v
		} else if v != "" {
			deprecated = append(deprecated, k)
		}
	}
	if len(deprecated) > 0 {
		slog.Warn("inline config keys ignored because config_file is set — move them to the config file",
			"keys", deprecated, "config_file", configFile)
	}

	return merged, nil
}

// AddPluginToSettings adds a broker plugin entry to the global settings.yaml.
func AddPluginToSettings(pluginName, configFilePath string) error {
	globalDir, err := GetGlobalDir()
	if err != nil {
		return fmt.Errorf("resolve global dir: %w", err)
	}

	settingsPath := filepath.Join(globalDir, "settings.yaml")

	var raw map[string]interface{}
	data, err := os.ReadFile(settingsPath)
	if err != nil {
		if !os.IsNotExist(err) {
			return fmt.Errorf("read settings file: %w", err)
		}
		raw = make(map[string]interface{})
	} else {
		if err := yamlv3.Unmarshal(data, &raw); err != nil {
			return fmt.Errorf("parse settings file: %w", err)
		}
		if raw == nil {
			raw = make(map[string]interface{})
		}
	}

	server, _ := raw["server"].(map[string]interface{})
	if server == nil {
		server = make(map[string]interface{})
		raw["server"] = server
	}

	plugins, _ := server["plugins"].(map[string]interface{})
	if plugins == nil {
		plugins = make(map[string]interface{})
		server["plugins"] = plugins
	}

	broker, _ := plugins["broker"].(map[string]interface{})
	if broker == nil {
		broker = make(map[string]interface{})
		plugins["broker"] = broker
	}

	broker[pluginName] = map[string]interface{}{
		"config_file": configFilePath,
	}

	out, err := yamlv3.Marshal(raw)
	if err != nil {
		return fmt.Errorf("marshal settings: %w", err)
	}

	return os.WriteFile(settingsPath, out, 0644)
}

// CreatePluginConfigFile creates a default config file for a newly installed plugin.
func CreatePluginConfigFile(pluginName, configFilePath string) error {
	resolved := configFilePath
	if strings.HasPrefix(resolved, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return fmt.Errorf("resolve home dir: %w", err)
		}
		resolved = filepath.Join(home, resolved[2:])
	}

	dir := filepath.Dir(resolved)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return fmt.Errorf("create config dir: %w", err)
	}

	// Write a minimal config file with only non-secret settings.
	// Secret keys (bot_token, public_key, etc.) are managed via the
	// secrets backend and should not appear in the config file.
	content := "# Scion plugin configuration for " + pluginName + "\n"
	switch pluginName {
	case "discord":
		content += "application_id: \"\"\n"
	case "slack":
		content += "socket_mode: \"false\"\n"
		content += "listen_address: \":3000\"\n"
		content += "db_path: \"~/.scion/scion-slack.db\"\n"
		content += "agent_cache_ttl: \"5m\"\n"
	}

	return os.WriteFile(resolved, []byte(content), 0600)
}

// LoadPluginConfigFile reads a standalone YAML config file for a plugin and
// returns its contents merged with any existing inline config. The inline
// config takes precedence (allows overrides). Secret keys are excluded from
// the file-based config to prevent accidental leakage.
func LoadPluginConfigFile(configFile string, inlineConfig map[string]string) (map[string]string, error) {
	if configFile == "" {
		return inlineConfig, nil
	}

	provider, err := NewYAMLConfigProvider(configFile)
	if err != nil {
		return nil, err
	}

	fileConfig, err := provider.Load(context.Background())
	if err != nil {
		return nil, err
	}

	// Filter out secret keys from file-based config
	for _, secretKey := range []string{
		SecretTelegramBotToken, SecretTelegramWebhookKey,
		SecretDiscordBotToken, SecretDiscordPublicKey,
		SecretSlackBotToken, SecretSlackAppToken, SecretSlackSigningSecret,
		SecretGChatSigningKey,
	} {
		delete(fileConfig, strings.ToLower(secretKey))
		delete(fileConfig, secretKey)
	}

	// Merge: file config is the base, inline config overrides
	merged := make(map[string]string, len(fileConfig)+len(inlineConfig))
	for k, v := range fileConfig {
		merged[k] = v
	}
	for k, v := range inlineConfig {
		merged[k] = v
	}

	return merged, nil
}
