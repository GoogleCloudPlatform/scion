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
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	goruntime "runtime"
	"strings"

	"github.com/knadh/koanf/parsers/json"
	"github.com/knadh/koanf/parsers/yaml"
	"github.com/knadh/koanf/providers/confmap"
	"github.com/knadh/koanf/providers/env"
	"github.com/knadh/koanf/providers/file"
	"github.com/knadh/koanf/providers/rawbytes"
	"github.com/knadh/koanf/v2"
)

// LoadSettingsKoanf loads settings using Koanf with provider priority:
// 1. Embedded defaults (YAML) with OS-specific runtime adjustment
// 2. Global settings file (~/.scion/settings.yaml or .json)
// 3. Project settings file (.scion/settings.yaml or .json)
// 4. Environment variables (SCION_ prefix, top-level only)
func LoadSettingsKoanf(projectPath string) (*Settings, error) {
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

	// 3. Load project settings
	effectiveProjectPath := resolveEffectiveProjectPath(projectPath)
	// Only load project settings if it's different from global (avoid double-loading)
	if effectiveProjectPath != "" && effectiveProjectPath != globalDir {
		if err := loadSettingsFile(k, effectiveProjectPath); err != nil {
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
		// Handle legacy grove_id
		if key == "grove_id" {
			return "project_id"
		}
		// Handle nested hub keys
		if strings.HasPrefix(key, "hub_") {
			subkey := strings.TrimPrefix(key, "hub_")
			// Convert snake_case to camelCase for specific keys
			switch subkey {
			case "grove_id", "project_id":
				// SCION_HUB_GROVE_ID or SCION_HUB_PROJECT_ID maps to top-level project_id, not hub.projectId
				return "project_id"
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

	// Normalize v1 settings keys to legacy keyspace.
	// In v1 format, project_id (legacy grove_id) is stored as hub.grove_id (snake_case),
	// but the legacy Settings struct expects it at the top level (project_id). The
	// HubClientConfig struct uses koanf tag "projectId" (camelCase), so the
	// v1 key hub.grove_id doesn't match either location without remapping.
	// Always remap (unconditionally) because after the koanf merge chain,
	// hub.grove_id reflects the most specific (project-level) value and must
	// take precedence over any top-level project_id inherited from global.
	// Support both hub.grove_id and hub.project_id from v1 settings.
	hubProjectID := ""
	if k.Exists("hub.project_id") {
		hubProjectID = k.String("hub.project_id")
	} else if k.Exists("hub.grove_id") {
		hubProjectID = k.String("hub.grove_id")
	}

	if hubProjectID != "" {
		_ = k.Load(confmap.Provider(map[string]interface{}{
			"project_id": hubProjectID,
		}, "."), nil)
		// Also remap to hub.projectId (camelCase) so the legacy
		// HubClientConfig.ProjectID field (koanf tag "projectId") is populated.
		// Without this, GetHubProjectID() returns "" for V1 settings, causing
		// EnsureHubReady to fall back to the local project_id and loop on
		// project registration when the hub project ID differs from the local ID.
		if !k.Exists("hub.projectId") {
			_ = k.Load(confmap.Provider(map[string]interface{}{
				"hub.projectId": hubProjectID,
			}, "."), nil)
		}
	}

	// For git projects, the project_id is stored in a project-id file inside the
	// .scion directory rather than in the settings file. Read it here so that
	// it overrides any project_id inherited from global settings. The original
	// projectPath points to the .scion directory (before resolveEffectiveProjectPath
	// redirects to the external config dir).
	if projectPath != "" && projectPath != globalDir {
		if projectID, err := ReadProjectID(projectPath); err == nil && projectID != "" {
			_ = k.Load(confmap.Provider(map[string]interface{}{
				"project_id": projectID,
			}, "."), nil)
		}
	}

	// In v1 format, broker identity fields are stored under server.broker.*
	// (snake_case), but the legacy Settings struct expects them at hub.brokerId
	// (camelCase). Remap so LoadSettingsKoanf produces correct HubClientConfig.
	if k.Exists("server.broker.broker_id") && !k.Exists("hub.brokerId") {
		_ = k.Load(confmap.Provider(map[string]interface{}{
			"hub.brokerId": k.String("server.broker.broker_id"),
		}, "."), nil)
	}
	if k.Exists("server.broker.broker_token") && !k.Exists("hub.brokerToken") {
		_ = k.Load(confmap.Provider(map[string]interface{}{
			"hub.brokerToken": k.String("server.broker.broker_token"),
		}, "."), nil)
	}
	if k.Exists("server.broker.broker_nickname") && !k.Exists("hub.brokerNickname") {
		_ = k.Load(confmap.Provider(map[string]interface{}{
			"hub.brokerNickname": k.String("server.broker.broker_nickname"),
		}, "."), nil)
	}

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

// LoadSettingsFromDir loads settings from a single directory's settings file
// without applying embedded defaults, global settings, or environment variables.
// This is useful when you need to read just one project's settings file in isolation,
// for example to get the project's hub.endpoint without the broker's own env vars
// overriding it.
func LoadSettingsFromDir(dir string) (*Settings, error) {
	k := koanf.New(".")
	if err := loadSettingsFile(k, dir); err != nil {
		return nil, err
	}
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

// getDefaultSettingsYAMLForRuntime generates the default settings YAML with the
// specified runtime for the local profile. The embedded template defaults to
// "container"; if a different runtime is specified, the template is adjusted.
func getDefaultSettingsYAMLForRuntime(targetRuntime string) ([]byte, error) {
	data, err := EmbedsFS.ReadFile("embeds/default_settings.yaml")
	if err != nil {
		return nil, err
	}

	if targetRuntime != "container" {
		data = bytes.Replace(data,
			[]byte("runtime: container  # Auto-adjusted by OS"),
			[]byte(fmt.Sprintf("runtime: %s  # Auto-detected", targetRuntime)),
			1)
	}

	return data, nil
}

// GetDefaultSettingsDataYAML returns the embedded default settings in YAML format.
// This function adjusts the local profile runtime based on the OS. It is used as
// a fallback default for settings loaders; during init, DetectLocalRuntime is used
// instead for actual runtime probing.
func GetDefaultSettingsDataYAML() ([]byte, error) {
	if goruntime.GOOS != "darwin" {
		return getDefaultSettingsYAMLForRuntime("docker")
	}
	return getDefaultSettingsYAMLForRuntime("container")
}

// GetProjectDefaultSettingsYAML returns the embedded project-level default settings YAML.
// Unlike the full default settings, project settings do not include profiles or runtimes;
// those are managed at the global/broker level (~/.scion/settings.yaml).
func GetProjectDefaultSettingsYAML() ([]byte, error) {

	return EmbedsFS.ReadFile("embeds/default_grove_settings.yaml")
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
