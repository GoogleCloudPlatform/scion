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
	"fmt"
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

	// Google Chat
	SecretGChatSigningKey = "GCHAT_SIGNING_KEY"
)

// IntegrationConfigProvider defines the interface for reading and writing
// per-integration configuration. Implementations handle only non-sensitive
// settings; secrets are managed separately via SecretBackend.
//
// Phase 1 exposes file-based Load/Save only. Phase 2 will extend this to the
// full admin API surface (context-aware, per-integration routing, list, status)
// per design doc §3.3.
type IntegrationConfigProvider interface {
	// Load reads the integration config file and returns its contents as a
	// flat key-value map suitable for merging into a plugin's Config map.
	Load() (map[string]string, error)

	// Save writes the given key-value map to the integration config file.
	Save(config map[string]string) error
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
func (p *YAMLConfigProvider) Load() (map[string]string, error) {
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
func (p *YAMLConfigProvider) Save(config map[string]string) error {
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
	"chat-app": {
		{SecretGChatSigningKey, "signing_key"},
	},
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

	fileConfig, err := provider.Load()
	if err != nil {
		return nil, err
	}

	// Filter out secret keys from file-based config
	for _, secretKey := range []string{
		SecretTelegramBotToken, SecretTelegramWebhookKey,
		SecretDiscordBotToken, SecretDiscordPublicKey,
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
