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

	"github.com/knadh/koanf/parsers/json"
	"github.com/knadh/koanf/parsers/yaml"
	"github.com/knadh/koanf/providers/env"
	"github.com/knadh/koanf/providers/file"
	"github.com/knadh/koanf/providers/rawbytes"
	"github.com/knadh/koanf/v2"
)

// LoadSettingsKoanf loads settings using Koanf with provider priority:
// 1. Embedded defaults (YAML) with OS-specific runtime adjustment
// 2. Global settings file (~/.scion/settings.yaml or .json)
// 3. Grove settings file (.scion/settings.yaml or .json)
// 4. Environment variables (SCION_ prefix, top-level only)
func LoadSettingsKoanf(grovePath string) (*Settings, error) {
	k := koanf.New(".")

	// 1. Load embedded defaults (YAML with fallback to JSON)
	// GetDefaultSettingsData applies OS-specific runtime adjustments
	if defaultData, err := GetDefaultSettingsData(); err == nil {
		_ = k.Load(rawbytes.Provider(defaultData), json.Parser())
	}

	// 2. Load global settings (~/.scion/settings.yaml or .json)
	globalDir, _ := GetGlobalDir()
	if globalDir != "" {
		if err := loadSettingsFile(k, globalDir); err != nil {
			return nil, err
		}
	}

	// 3. Load grove settings
	// If grovePath is empty, try to find the project grove
	effectiveGrovePath := grovePath
	if effectiveGrovePath == "" {
		if projectPath, ok := FindProjectRoot(); ok {
			effectiveGrovePath = projectPath
		}
	} else if effectiveGrovePath == "global" || effectiveGrovePath == "home" {
		// For global/home, we already loaded global settings above
		effectiveGrovePath = ""
	}
	// Only load grove settings if it's different from global (avoid double-loading)
	if effectiveGrovePath != "" && effectiveGrovePath != globalDir {
		if err := loadSettingsFile(k, effectiveGrovePath); err != nil {
			return nil, err
		}
	}

	// 4. Load environment variables (SCION_ prefix, top-level only)
	// Maps: SCION_ACTIVE_PROFILE -> active_profile
	//       SCION_DEFAULT_TEMPLATE -> default_template
	//       SCION_BUCKET_PROVIDER -> bucket.provider
	//       SCION_BUCKET_NAME -> bucket.name
	//       SCION_BUCKET_PREFIX -> bucket.prefix
	//       SCION_HUB_ENDPOINT -> hub.endpoint
	//       SCION_HUB_TOKEN -> hub.token
	//       SCION_HUB_API_KEY -> hub.apiKey
	//       SCION_HUB_BROKER_ID -> hub.brokerId
	//       SCION_HUB_BROKER_TOKEN -> hub.brokerToken
	_ = k.Load(env.Provider("SCION_", ".", func(s string) string {
		key := strings.ToLower(strings.TrimPrefix(s, "SCION_"))
		// Handle nested bucket keys
		if strings.HasPrefix(key, "bucket_") {
			return "bucket." + strings.TrimPrefix(key, "bucket_")
		}
		// Handle nested hub keys
		if strings.HasPrefix(key, "hub_") {
			subkey := strings.TrimPrefix(key, "hub_")
			// Convert snake_case to camelCase for specific keys
			switch subkey {
			case "api_key":
				return "hub.apiKey"
			case "broker_id":
				return "hub.brokerId"
			case "broker_token":
				return "hub.brokerToken"
			default:
				return "hub." + subkey
			}
		}
		return key
	}), nil)

	// Unmarshal into Settings struct
	settings := &Settings{
		Runtimes:  make(map[string]RuntimeConfig),
		Harnesses: make(map[string]HarnessConfig),
		Profiles:  make(map[string]ProfileConfig),
	}

	if err := k.Unmarshal("", settings); err != nil {
		return nil, err
	}

	return settings, nil
}

// loadSettingsFile loads settings from a directory, preferring YAML over JSON
func loadSettingsFile(k *koanf.Koanf, dir string) error {
	yamlPath := filepath.Join(dir, "settings.yaml")
	ymlPath := filepath.Join(dir, "settings.yml")
	jsonPath := filepath.Join(dir, "settings.json")

	// Try YAML first (.yaml then .yml)
	if _, err := os.Stat(yamlPath); err == nil {
		return k.Load(file.Provider(yamlPath), yaml.Parser())
	}
	if _, err := os.Stat(ymlPath); err == nil {
		return k.Load(file.Provider(ymlPath), yaml.Parser())
	}
	// Fall back to JSON
	if _, err := os.Stat(jsonPath); err == nil {
		return k.Load(file.Provider(jsonPath), json.Parser())
	}
	return nil
}

// GetDefaultSettingsDataYAML returns the embedded default settings in YAML format.
// This function adjusts the local profile runtime based on the OS.
func GetDefaultSettingsDataYAML() ([]byte, error) {
	return EmbedsFS.ReadFile("embeds/default_settings.yaml")
}

// GetSettingsPath returns the path to the settings file in a directory,
// preferring YAML over JSON. Returns empty string if no settings file exists.
func GetSettingsPath(dir string) string {
	yamlPath := filepath.Join(dir, "settings.yaml")
	ymlPath := filepath.Join(dir, "settings.yml")
	jsonPath := filepath.Join(dir, "settings.json")

	if _, err := os.Stat(yamlPath); err == nil {
		return yamlPath
	}
	if _, err := os.Stat(ymlPath); err == nil {
		return ymlPath
	}
	if _, err := os.Stat(jsonPath); err == nil {
		return jsonPath
	}
	return ""
}

// GetScionAgentConfigPath returns the path to the scion-agent config file,
// preferring YAML over JSON. Returns empty string if no config file exists.
func GetScionAgentConfigPath(dir string) string {
	yamlPath := filepath.Join(dir, "scion-agent.yaml")
	ymlPath := filepath.Join(dir, "scion-agent.yml")
	jsonPath := filepath.Join(dir, "scion-agent.json")

	if _, err := os.Stat(yamlPath); err == nil {
		return yamlPath
	}
	if _, err := os.Stat(ymlPath); err == nil {
		return ymlPath
	}
	if _, err := os.Stat(jsonPath); err == nil {
		return jsonPath
	}
	return ""
}

// SettingsFileExists checks if a settings file exists in a directory (YAML or JSON)
func SettingsFileExists(dir string) bool {
	return GetSettingsPath(dir) != ""
}

// ScionAgentConfigExists checks if a scion-agent config file exists (YAML or JSON)
func ScionAgentConfigExists(dir string) bool {
	return GetScionAgentConfigPath(dir) != ""
}
