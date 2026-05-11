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

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/util"
	"gopkg.in/yaml.v3"
)

// Note: Settings files support YAML (preferred) and JSONC formats.
// YAML files (.yaml/.yml) are checked first, then JSON (.json).
// Environment variables with SCION_ prefix override top-level settings.

type RuntimeConfig struct {
	Host      string            `json:"broker,omitempty" yaml:"host,omitempty" koanf:"host"`
	Context   string            `json:"context,omitempty" yaml:"context,omitempty" koanf:"context"`
	Namespace string            `json:"namespace,omitempty" yaml:"namespace,omitempty" koanf:"namespace"`
	Env       map[string]string `json:"env,omitempty" yaml:"env,omitempty" koanf:"env"`
	Sync      string            `json:"sync,omitempty" yaml:"sync,omitempty" koanf:"sync"`
}

type HarnessConfig struct {
	Image            string            `json:"image" yaml:"image" koanf:"image"`
	User             string            `json:"user" yaml:"user" koanf:"user"`
	Env              map[string]string `json:"env,omitempty" yaml:"env,omitempty" koanf:"env"`
	Volumes          []api.VolumeMount `json:"volumes,omitempty" yaml:"volumes,omitempty" koanf:"volumes"`
	AuthSelectedType string            `json:"auth_selectedType,omitempty" yaml:"auth_selectedType,omitempty" koanf:"auth_selectedType"`
}

type HarnessOverride struct {
	Image            string            `json:"image,omitempty" yaml:"image,omitempty" koanf:"image"`
	User             string            `json:"user,omitempty" yaml:"user,omitempty" koanf:"user"`
	Env              map[string]string `json:"env,omitempty" yaml:"env,omitempty" koanf:"env"`
	Volumes          []api.VolumeMount `json:"volumes,omitempty" yaml:"volumes,omitempty" koanf:"volumes"`
	AuthSelectedType string            `json:"auth_selectedType,omitempty" yaml:"auth_selectedType,omitempty" koanf:"auth_selectedType"`
	Resources        *api.ResourceSpec `json:"resources,omitempty" yaml:"resources,omitempty" koanf:"resources"`
}

type ProfileConfig struct {
	Runtime          string                     `json:"runtime" yaml:"runtime" koanf:"runtime"`
	Env              map[string]string          `json:"env,omitempty" yaml:"env,omitempty" koanf:"env"`
	Volumes          []api.VolumeMount          `json:"volumes,omitempty" yaml:"volumes,omitempty" koanf:"volumes"`
	Resources        *api.ResourceSpec          `json:"resources,omitempty" yaml:"resources,omitempty" koanf:"resources"`
	HarnessOverrides map[string]HarnessOverride `json:"harness_overrides,omitempty" yaml:"harness_overrides,omitempty" koanf:"harness_overrides"`
}

// BucketConfig defines settings for cloud storage bucket persistence.
// These settings can be set via environment variables:
//   - SCION_BUCKET_PROVIDER: The cloud provider (e.g., "GCS")
//   - SCION_BUCKET_NAME: The bucket name
//   - SCION_BUCKET_PREFIX: The prefix/path within the bucket
type BucketConfig struct {
	Provider string `json:"provider,omitempty" yaml:"provider,omitempty" koanf:"provider"` // Cloud provider: "GCS", etc.
	Name     string `json:"name,omitempty" yaml:"name,omitempty" koanf:"name"`             // Bucket name
	Prefix   string `json:"prefix,omitempty" yaml:"prefix,omitempty" koanf:"prefix"`       // Prefix/path within the bucket
}

// HubClientConfig defines settings for connecting to a Scion Hub.
// These settings can be set via environment variables:
//   - SCION_HUB_ENDPOINT: The Hub API endpoint URL (e.g., "https://hub.scion.dev")
//   - SCION_HUB_TOKEN: Bearer token for Hub authentication
//   - SCION_HUB_API_KEY: API key for Hub authentication (alternative to token)
//   - SCION_HUB_ENABLED: Set to "true" to enable Hub integration
type HubClientConfig struct {
	// Enabled indicates whether Hub integration is enabled.
	// When enabled and configured, agent operations are routed through the Hub.
	Enabled *bool `json:"enabled,omitempty" yaml:"enabled,omitempty" koanf:"enabled"`
	// Linked indicates whether this project has been explicitly linked to the Hub
	// via 'scion hub link'. This is separate from Enabled: a project can have hub
	// enabled for routing without being linked (status should not report linked
	// until the user explicitly runs 'hub link').
	Linked *bool `json:"linked,omitempty" yaml:"linked,omitempty" koanf:"linked"`
	// LocalOnly indicates that this project should operate in local-only mode.
	// When set to true, Hub sync checks will error with guidance to use --no-hub.
	// This is different from Enabled=false: LocalOnly=true means Hub IS configured
	// but the user has explicitly opted out of sync requirements for this project.
	LocalOnly *bool `json:"local_only,omitempty" yaml:"local_only,omitempty" koanf:"local_only"`
	// Endpoint is the Hub API endpoint URL
	Endpoint string `json:"endpoint,omitempty" yaml:"endpoint,omitempty" koanf:"endpoint"`
	// Token is a bearer token for authentication
	Token string `json:"token,omitempty" yaml:"token,omitempty" koanf:"token"`
	// APIKey is an API key for authentication (alternative to Token)
	APIKey string `json:"apiKey,omitempty" yaml:"apiKey,omitempty" koanf:"apiKey"`
	// ProjectID is the unique identifier for the project when registered with the Hub
	ProjectID string `json:"projectId,omitempty" yaml:"projectId,omitempty" koanf:"projectId"`
	// BrokerID is the unique identifier for this broker when registered with the Hub.
	// This is a durable UUID that persists across server restarts.
	BrokerID string `json:"brokerId,omitempty" yaml:"brokerId,omitempty" koanf:"brokerId"`
	// BrokerNickname is a human-readable name for this broker.
	// If not set, defaults to the system hostname.
	BrokerNickname string `json:"brokerNickname,omitempty" yaml:"brokerNickname,omitempty" koanf:"brokerNickname"`
	// BrokerToken is the token received when registering this broker with the Hub
	BrokerToken string `json:"brokerToken,omitempty" yaml:"brokerToken,omitempty" koanf:"brokerToken"`
	// LastSyncedAt is the RFC3339 timestamp of the last successful Hub sync.
	// Used to determine whether hub-only agents were created by other brokers
	// (after this timestamp) or were locally deleted (before this timestamp).
	LastSyncedAt string `json:"lastSyncedAt,omitempty" yaml:"lastSyncedAt,omitempty" koanf:"lastSyncedAt"`
}

type CLIConfig struct {
	// AutoHelp indicates whether to print usage help on every error.
	AutoHelp *bool  `json:"autohelp,omitempty" yaml:"autohelp,omitempty" koanf:"autohelp"`
	Mode     string `json:"mode,omitempty" yaml:"mode,omitempty" koanf:"mode"`
}

// HubConnectionConfig defines settings for a named hub connection.
// These are written during broker registration and used to track
// the hub connections this broker has registered with.
type HubConnectionConfig struct {
	Endpoint string `json:"endpoint,omitempty" yaml:"endpoint,omitempty" koanf:"endpoint"`
}

type Settings struct {
	ProjectID         string                         `json:"project_id,omitempty" yaml:"project_id,omitempty" koanf:"project_id"`
	ActiveProfile   string                         `json:"active_profile" yaml:"active_profile" koanf:"active_profile"`
	DefaultTemplate string                         `json:"default_template,omitempty" yaml:"default_template,omitempty" koanf:"default_template"`
	WorkspacePath   string                         `json:"workspace_path,omitempty" yaml:"workspace_path,omitempty" koanf:"workspace_path"`
	Bucket          *BucketConfig                  `json:"bucket,omitempty" yaml:"bucket,omitempty" koanf:"bucket"`
	Hub             *HubClientConfig               `json:"hub,omitempty" yaml:"hub,omitempty" koanf:"hub"`
	CLI             *CLIConfig                     `json:"cli,omitempty" yaml:"cli,omitempty" koanf:"cli"`
	HubConnections  map[string]HubConnectionConfig `json:"hub_connections,omitempty" yaml:"hub_connections,omitempty" koanf:"hub_connections"`
	Runtimes        map[string]RuntimeConfig       `json:"runtimes" yaml:"runtimes" koanf:"runtimes"`
	Harnesses       map[string]HarnessConfig       `json:"harnesses" yaml:"harnesses" koanf:"harnesses"`
	Profiles        map[string]ProfileConfig       `json:"profiles" yaml:"profiles" koanf:"profiles"`
}

func (s *Settings) ResolveRuntime(profileName string) (RuntimeConfig, string, error) {
	if profileName == "" {
		profileName = s.ActiveProfile
	}
	profile, ok := s.Profiles[profileName]
	if !ok {
		return RuntimeConfig{}, "", fmt.Errorf("profile %q not found", profileName)
	}
	runtime, ok := s.Runtimes[profile.Runtime]
	if !ok {
		return RuntimeConfig{}, "", fmt.Errorf("runtime %q not found for profile %q", profile.Runtime, profileName)
	}

	// Merge profile-level env into runtime config
	if profile.Env != nil {
		runtime.Env = mergeMaps(runtime.Env, profile.Env)
	}

	return runtime, profile.Runtime, nil
}

func (s *Settings) ResolveHarness(profileName, harnessName string) (HarnessConfig, error) {
	if profileName == "" {
		profileName = s.ActiveProfile
	}
	baseHarness, ok := s.Harnesses[harnessName]
	if !ok {
		// Try to fallback to common harnesses if not found?
		// For now, return error if not in registry
		return HarnessConfig{}, fmt.Errorf("harness %q not found in registry", harnessName)
	}

	profile, ok := s.Profiles[profileName]
	if !ok {
		return baseHarness, nil
	}

	result := baseHarness

	// Merge profile-level env
	if profile.Env != nil {
		result.Env = mergeMaps(result.Env, profile.Env)
	}

	// Merge profile-level volumes
	if profile.Volumes != nil {
		result.Volumes = append(result.Volumes, profile.Volumes...)
	}

	if profile.HarnessOverrides != nil {
		if override, ok := profile.HarnessOverrides[harnessName]; ok {
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

// MergeResourceSpec merges resource specs field-by-field.
// Override values take precedence over base values.
func MergeResourceSpec(base, override *api.ResourceSpec) *api.ResourceSpec {
	if override == nil {
		return base
	}
	if base == nil {
		cpy := *override
		return &cpy
	}
	result := *base
	if override.Requests.CPU != "" {
		result.Requests.CPU = override.Requests.CPU
	}
	if override.Requests.Memory != "" {
		result.Requests.Memory = override.Requests.Memory
	}
	if override.Limits.CPU != "" {
		result.Limits.CPU = override.Limits.CPU
	}
	if override.Limits.Memory != "" {
		result.Limits.Memory = override.Limits.Memory
	}
	if override.Disk != "" {
		result.Disk = override.Disk
	}
	return &result
}

func mergeMaps(base, override map[string]string) map[string]string {
	if override == nil {
		return base
	}
	result := make(map[string]string)
	for k, v := range base {
		result[k] = v
	}
	for k, v := range override {
		result[k] = v
	}
	return result
}

// LoadSettings loads and merges settings from the hierarchy using Koanf.
// Priority: Env vars > Grove > Global > Defaults
// Supports both YAML (.yaml/.yml) and JSON (.json) files, preferring YAML.
func LoadSettings(grovePath string) (*Settings, error) {
	return LoadSettingsKoanf(grovePath)
}

func mergeSettingsFromFile(base *Settings, path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}

	return MergeSettings(base, data)
}

func expandEnvMap(m map[string]string) map[string]string {
	if m == nil {
		return nil
	}
	expanded := make(map[string]string)
	for k, v := range m {
		ek, _ := util.ExpandEnv(k)
		if ek == "" {
			continue
		}
		val, _ := util.ExpandEnv(v)
		expanded[ek] = val
	}
	return expanded
}

func expandVolumeMounts(volumes []api.VolumeMount) []api.VolumeMount {
	if volumes == nil {
		return nil
	}
	expanded := make([]api.VolumeMount, len(volumes))
	for i, v := range volumes {
		s, _ := util.ExpandEnv(v.Source)
		t, _ := util.ExpandEnv(v.Target)
		expanded[i] = api.VolumeMount{
			Source:   s,
			Target:   t,
			ReadOnly: v.ReadOnly,
			Type:     v.Type,
			Bucket:   v.Bucket,
			Prefix:   v.Prefix,
			Mode:     v.Mode,
		}
	}
	return expanded
}

func MergeSettings(base *Settings, data []byte) error {
	var override Settings
	if err := util.UnmarshalJSONC(data, &override); err != nil {
		return err
	}

	if override.ProjectID != "" {
		base.ProjectID = override.ProjectID
	}
	if override.ActiveProfile != "" {
		base.ActiveProfile = override.ActiveProfile
	}
	if override.DefaultTemplate != "" {
		base.DefaultTemplate = override.DefaultTemplate
	}

	// Merge bucket config with env var expansion
	if override.Bucket != nil {
		if base.Bucket == nil {
			base.Bucket = &BucketConfig{}
		}
		if override.Bucket.Provider != "" {
			p, _ := util.ExpandEnv(override.Bucket.Provider)
			base.Bucket.Provider = p
		}
		if override.Bucket.Name != "" {
			n, _ := util.ExpandEnv(override.Bucket.Name)
			base.Bucket.Name = n
		}
		if override.Bucket.Prefix != "" {
			pf, _ := util.ExpandEnv(override.Bucket.Prefix)
			base.Bucket.Prefix = pf
		}
	}

	if override.CLI != nil {
		if base.CLI == nil {
			base.CLI = &CLIConfig{}
		}
		if override.CLI.AutoHelp != nil {
			base.CLI.AutoHelp = override.CLI.AutoHelp
		}
	}

	// Merge hub_connections map
	if override.HubConnections != nil {
		if base.HubConnections == nil {
			base.HubConnections = make(map[string]HubConnectionConfig)
		}
		for k, v := range override.HubConnections {
			existing := base.HubConnections[k]
			if v.Endpoint != "" {
				existing.Endpoint = v.Endpoint
			}
			base.HubConnections[k] = existing
		}
	}

	if override.Runtimes != nil {
		if base.Runtimes == nil {
			base.Runtimes = make(map[string]RuntimeConfig)
		}
		for k, v := range override.Runtimes {
			existing := base.Runtimes[k]
			if v.Host != "" {
				existing.Host = v.Host
			}
			if v.Context != "" {
				existing.Context = v.Context
			}
			if v.Namespace != "" {
				existing.Namespace = v.Namespace
			}
			if v.Env != nil {
				existing.Env = mergeMaps(existing.Env, expandEnvMap(v.Env))
			}
			if v.Sync != "" {
				existing.Sync = v.Sync
			}
			base.Runtimes[k] = existing
		}
	}
	if override.Harnesses != nil {
		if base.Harnesses == nil {
			base.Harnesses = make(map[string]HarnessConfig)
		}
		for k, v := range override.Harnesses {
			existing := base.Harnesses[k]
			if v.Image != "" {
				existing.Image = v.Image
			}
			if v.User != "" {
				existing.User = v.User
			}
			if v.AuthSelectedType != "" {
				existing.AuthSelectedType = v.AuthSelectedType
			}
			if v.Env != nil {
				existing.Env = mergeMaps(existing.Env, expandEnvMap(v.Env))
			}
			if v.Volumes != nil {
				existing.Volumes = append(existing.Volumes, expandVolumeMounts(v.Volumes)...)
			}
			base.Harnesses[k] = existing
		}
	}
	if override.Profiles != nil {
		if base.Profiles == nil {
			base.Profiles = make(map[string]ProfileConfig)
		}
		for k, v := range override.Profiles {
			existing := base.Profiles[k]
			if v.Runtime != "" {
				existing.Runtime = v.Runtime
			}
			if v.Env != nil {
				existing.Env = mergeMaps(existing.Env, expandEnvMap(v.Env))
			}
			if v.Volumes != nil {
				existing.Volumes = append(existing.Volumes, expandVolumeMounts(v.Volumes)...)
			}
			if v.Resources != nil {
				existing.Resources = MergeResourceSpec(existing.Resources, v.Resources)
			}
			if v.HarnessOverrides != nil {
				if existing.HarnessOverrides == nil {
					existing.HarnessOverrides = make(map[string]HarnessOverride)
				}
				for hk, hv := range v.HarnessOverrides {
					hov := existing.HarnessOverrides[hk]
					if hv.Image != "" {
						hov.Image = hv.Image
					}
					if hv.User != "" {
						hov.User = hv.User
					}
					if hv.AuthSelectedType != "" {
						hov.AuthSelectedType = hv.AuthSelectedType
					}
					if hv.Env != nil {
						hov.Env = mergeMaps(hov.Env, expandEnvMap(hv.Env))
					}
					if hv.Volumes != nil {
						hov.Volumes = append(hov.Volumes, expandVolumeMounts(hv.Volumes)...)
					}
					if hv.Resources != nil {
						hov.Resources = MergeResourceSpec(hov.Resources, hv.Resources)
					}
					existing.HarnessOverrides[hk] = hov
				}
			}
			base.Profiles[k] = existing
		}
	}

	return nil
}

// SaveSettings saves the settings to the specified location in YAML format.
func SaveSettings(grovePath string, settings *Settings, global bool) error {
	var targetPath string
	if global {
		globalDir, err := GetGlobalDir()
		if err != nil {
			return err
		}
		targetPath = filepath.Join(globalDir, "settings.yaml")
	} else {
		if grovePath == "" {
			return fmt.Errorf("grove path required for local settings")
		}
		targetPath = filepath.Join(grovePath, "settings.yaml")
	}

	dir := filepath.Dir(targetPath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}

	data, err := yaml.Marshal(settings)
	if err != nil {
		return err
	}

	return os.WriteFile(targetPath, data, 0644)
}

// SaveSettingsJSON saves the settings to the specified location in JSON format.
// This is provided for backward compatibility.
func SaveSettingsJSON(grovePath string, settings *Settings, global bool) error {
	var targetPath string
	if global {
		globalDir, err := GetGlobalDir()
		if err != nil {
			return err
		}
		targetPath = filepath.Join(globalDir, "settings.json")
	} else {
		if grovePath == "" {
			return fmt.Errorf("grove path required for local settings")
		}
		targetPath = filepath.Join(grovePath, "settings.json")
	}

	dir := filepath.Dir(targetPath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}

	data, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return err
	}

	return os.WriteFile(targetPath, data, 0644)
}

// UpdateSetting updates a specific setting key in the specified scope (global or local).
// It reads from existing settings file (YAML or JSON) and writes to YAML format.
// If the existing file is in v1 versioned format (has schema_version), it delegates
// to UpdateVersionedSetting to preserve the format.
func UpdateSetting(grovePath string, key string, value string, global bool) error {
	var dir string
	if global {
		globalDir, err := GetGlobalDir()
		if err != nil {
			return err
		}
		dir = globalDir
	} else {
		if grovePath == "" {
			return fmt.Errorf("grove path required for local settings")
		}
		// Resolve through GetProjectConfigDir so that git groves with split
		// storage write to the external config dir (~/.scion/project-configs/…)
		// — the same location LoadSettingsKoanf reads from.
		dir = GetProjectConfigDir(grovePath)

		// Phase 5: Migrate .scion/grove-id to project-id if it exists.
		// This ensures that subsequent reads prefer the new filename.
		if grovePath != "" {
			groveIDFile := filepath.Join(grovePath, "grove-id")
			projectIDFile := filepath.Join(grovePath, "project-id")
			if _, err := os.Stat(groveIDFile); err == nil {
				if _, err := os.Stat(projectIDFile); os.IsNotExist(err) {
					_ = os.Rename(groveIDFile, projectIDFile)
				}
			}
		}
	}

	// Find existing settings file (YAML or JSON)
	existingPath := GetSettingsPath(dir)

	// Check if the existing file is in v1 versioned format
	if existingPath != "" {
		data, err := os.ReadFile(existingPath)
		if err == nil {
			if version, _ := DetectSettingsFormat(data); version != "" {
				// Delegate to versioned handler to preserve v1 format
				return UpdateVersionedSetting(dir, key, value)
			}
			// Legacy format detected — auto-migrate to v1 before updating
			fmt.Fprintf(os.Stderr, "Warning: settings file %s uses legacy format. Auto-migrating to v1 schema.\n", existingPath)
			fmt.Fprintf(os.Stderr, "  You can also run 'scion config migrate' to migrate manually.\n")
			if _, err := MigrateSettingsFile(dir, false); err != nil {
				return fmt.Errorf("auto-migration of legacy settings failed: %w\n  Run 'scion config migrate' to migrate manually", err)
			}
			return UpdateVersionedSetting(dir, key, value)
		}
	}

	// No existing file — create a new v1 settings file via the versioned handler
	return UpdateVersionedSetting(dir, key, value)
}

// updateSettingLegacy is the old legacy settings update path, retained only for reference.
// All settings updates now go through UpdateVersionedSetting after auto-migration.
func updateSettingLegacy(dir string, key string, value string) error {
	existingPath := GetSettingsPath(dir)
	targetPath := filepath.Join(dir, "settings.yaml")

	// Legacy path: load existing file specifically (not merged)
	var current Settings
	if existingPath != "" {
		data, err := os.ReadFile(existingPath)
		if err != nil && !os.IsNotExist(err) {
			return err
		}
		if err == nil {
			// Parse based on file extension
			if filepath.Ext(existingPath) == ".json" {
				if err := util.UnmarshalJSONC(data, &current); err != nil {
					return fmt.Errorf("failed to parse existing settings at %s: %w", existingPath, err)
				}
			} else {
				if err := yaml.Unmarshal(data, &current); err != nil {
					return fmt.Errorf("failed to parse existing settings at %s: %w", existingPath, err)
				}
			}
		}
	}

	// Update the field
	switch key {
	case "project_id", "grove_id":
		current.ProjectID = value
	case "active_profile":
		current.ActiveProfile = value
	case "default_template":
		current.DefaultTemplate = value
	case "workspace_path":
		current.WorkspacePath = value
	case "bucket.provider":
		if current.Bucket == nil {
			current.Bucket = &BucketConfig{}
		}
		current.Bucket.Provider = value
	case "bucket.name":
		if current.Bucket == nil {
			current.Bucket = &BucketConfig{}
		}
		current.Bucket.Name = value
	case "bucket.prefix":
		if current.Bucket == nil {
			current.Bucket = &BucketConfig{}
		}
		current.Bucket.Prefix = value
	case "hub.endpoint":
		if current.Hub == nil {
			current.Hub = &HubClientConfig{}
		}
		current.Hub.Endpoint = value
	case "hub.token":
		if current.Hub == nil {
			current.Hub = &HubClientConfig{}
		}
		current.Hub.Token = value
	case "hub.apiKey":
		if current.Hub == nil {
			current.Hub = &HubClientConfig{}
		}
		current.Hub.APIKey = value
	case "hub.projectId", "hub.groveId":
		if current.Hub == nil {
			current.Hub = &HubClientConfig{}
		}
		current.Hub.ProjectID = value
	case "hub.brokerId":
		if current.Hub == nil {
			current.Hub = &HubClientConfig{}
		}
		current.Hub.BrokerID = value
	case "hub.brokerToken":
		if current.Hub == nil {
			current.Hub = &HubClientConfig{}
		}
		current.Hub.BrokerToken = value
	case "hub.brokerNickname":
		if current.Hub == nil {
			current.Hub = &HubClientConfig{}
		}
		current.Hub.BrokerNickname = value
	case "hub.enabled":
		if current.Hub == nil {
			current.Hub = &HubClientConfig{}
		}
		enabled := value == "true"
		current.Hub.Enabled = &enabled
	case "hub.linked":
		if current.Hub == nil {
			current.Hub = &HubClientConfig{}
		}
		linked := value == "true"
		current.Hub.Linked = &linked
	case "hub.local_only":
		if current.Hub == nil {
			current.Hub = &HubClientConfig{}
		}
		localOnly := value == "true"
		current.Hub.LocalOnly = &localOnly
	case "hub.lastSyncedAt":
		if current.Hub == nil {
			current.Hub = &HubClientConfig{}
		}
		current.Hub.LastSyncedAt = value
	case "cli.autohelp":
		if current.CLI == nil {
			current.CLI = &CLIConfig{}
		}
		autohelp := value == "true"
		current.CLI.AutoHelp = &autohelp
	case "cli.mode":
		if current.CLI == nil {
			current.CLI = &CLIConfig{}
		}
		current.CLI.Mode = value
	default:
		// Handle hub_connections.<name>.endpoint keys
		if strings.HasPrefix(key, "hub_connections.") {
			parts := strings.SplitN(key, ".", 3)
			if len(parts) != 3 {
				return fmt.Errorf("invalid hub_connections key: %s (expected hub_connections.<name>.<field>)", key)
			}
			connName := parts[1]
			field := parts[2]

			if field != "endpoint" {
				return fmt.Errorf("unknown hub_connections field: %s (supported: endpoint)", field)
			}

			if current.HubConnections == nil {
				current.HubConnections = make(map[string]HubConnectionConfig)
			}
			conn := current.HubConnections[connName]
			conn.Endpoint = value
			current.HubConnections[connName] = conn
		} else {
			return fmt.Errorf("unknown or complex setting key: %s (manual edit recommended for registries)", key)
		}
	}

	// Save as YAML
	if err := os.MkdirAll(filepath.Dir(targetPath), 0755); err != nil {
		return err
	}
	newData, err := yaml.Marshal(current)
	if err != nil {
		return err
	}
	if err := os.WriteFile(targetPath, newData, 0644); err != nil {
		return err
	}

	// If we migrated from JSON, remove the old JSON file
	if existingPath != "" && existingPath != targetPath && filepath.Ext(existingPath) == ".json" {
		_ = os.Remove(existingPath)
	}

	return nil
}

func GetSettingValue(s *Settings, key string) (string, error) {
	switch key {
	case "project_id", "grove_id":
		return s.ProjectID, nil
	case "active_profile":
		return s.ActiveProfile, nil
	case "default_template":
		return s.DefaultTemplate, nil
	case "bucket.provider":
		if s.Bucket != nil {
			return s.Bucket.Provider, nil
		}
		return "", nil
	case "bucket.name":
		if s.Bucket != nil {
			return s.Bucket.Name, nil
		}
		return "", nil
	case "bucket.prefix":
		if s.Bucket != nil {
			return s.Bucket.Prefix, nil
		}
		return "", nil
	case "hub.endpoint":
		if s.Hub != nil {
			return s.Hub.Endpoint, nil
		}
		return "", nil
	case "hub.token":
		if s.Hub != nil {
			return s.Hub.Token, nil
		}
		return "", nil
	case "hub.apiKey":
		if s.Hub != nil {
			return s.Hub.APIKey, nil
		}
		return "", nil
	case "hub.projectId", "hub.groveId":
		if s.Hub != nil {
			return s.Hub.ProjectID, nil
		}
		return "", nil
	case "hub.brokerId":
		if s.Hub != nil {
			return s.Hub.BrokerID, nil
		}
		return "", nil
	case "hub.brokerToken":
		if s.Hub != nil {
			return s.Hub.BrokerToken, nil
		}
		return "", nil
	case "hub.brokerNickname":
		if s.Hub != nil {
			return s.Hub.BrokerNickname, nil
		}
		return "", nil
	case "hub.enabled":
		if s.Hub != nil && s.Hub.Enabled != nil {
			if *s.Hub.Enabled {
				return "true", nil
			}
			return "false", nil
		}
		return "", nil
	case "hub.linked":
		if s.Hub != nil && s.Hub.Linked != nil {
			if *s.Hub.Linked {
				return "true", nil
			}
			return "false", nil
		}
		return "", nil
	case "hub.local_only":
		if s.Hub != nil && s.Hub.LocalOnly != nil {
			if *s.Hub.LocalOnly {
				return "true", nil
			}
			return "false", nil
		}
		return "", nil
	case "hub.lastSyncedAt":
		if s.Hub != nil {
			return s.Hub.LastSyncedAt, nil
		}
		return "", nil
	case "cli.autohelp":
		if s.CLI != nil && s.CLI.AutoHelp != nil {
			if *s.CLI.AutoHelp {
				return "true", nil
			}
			return "false", nil
		}
		return "", nil
	case "cli.mode":
		if s.CLI != nil {
			return s.CLI.Mode, nil
		}
		return "", nil
	}

	// Handle hub_connections.<name>.endpoint keys
	if strings.HasPrefix(key, "hub_connections.") {
		parts := strings.SplitN(key, ".", 3)
		if len(parts) == 3 && parts[2] == "endpoint" {
			if s.HubConnections != nil {
				if conn, ok := s.HubConnections[parts[1]]; ok {
					return conn.Endpoint, nil
				}
			}
			return "", nil
		}
	}

	return "", fmt.Errorf("unknown or complex setting key: %s", key)
}

func GetSettingsMap(s *Settings) map[string]string {
	m := make(map[string]string)
	m["project_id"] = s.ProjectID
	m["active_profile"] = s.ActiveProfile
	m["default_template"] = s.DefaultTemplate
	if s.Bucket != nil {
		m["bucket.provider"] = s.Bucket.Provider
		m["bucket.name"] = s.Bucket.Name
		m["bucket.prefix"] = s.Bucket.Prefix
	}
	if s.Hub != nil {
		if s.Hub.Enabled != nil {
			if *s.Hub.Enabled {
				m["hub.enabled"] = "true"
			} else {
				m["hub.enabled"] = "false"
			}
		}
		if s.Hub.Linked != nil {
			if *s.Hub.Linked {
				m["hub.linked"] = "true"
			} else {
				m["hub.linked"] = "false"
			}
		}
		if s.Hub.LocalOnly != nil {
			if *s.Hub.LocalOnly {
				m["hub.local_only"] = "true"
			} else {
				m["hub.local_only"] = "false"
			}
		}
		m["hub.endpoint"] = s.Hub.Endpoint
		// Don't include secrets in the map by default
		if s.Hub.Token != "" {
			m["hub.token"] = "********" // Mask token
		}
		if s.Hub.APIKey != "" {
			m["hub.apiKey"] = "********" // Mask API key
		}
		m["hub.projectId"] = s.Hub.ProjectID
		m["hub.brokerId"] = s.Hub.BrokerID
		m["hub.brokerNickname"] = s.Hub.BrokerNickname
		if s.Hub.BrokerToken != "" {
			m["hub.brokerToken"] = "********" // Mask broker token
		}
		m["hub.lastSyncedAt"] = s.Hub.LastSyncedAt
	}
	if s.CLI != nil {
		if s.CLI.AutoHelp != nil {
			if *s.CLI.AutoHelp {
				m["cli.autohelp"] = "true"
			} else {
				m["cli.autohelp"] = "false"
			}
		}
		if s.CLI.Mode != "" {
			m["cli.mode"] = s.CLI.Mode
		}
	}
	for name, conn := range s.HubConnections {
		m["hub_connections."+name+".endpoint"] = conn.Endpoint
	}
	return m
}

// GetHubEndpoint returns the Hub endpoint from settings, or empty string if not configured.
func (s *Settings) GetHubEndpoint() string {
	if s.Hub != nil {
		return s.Hub.Endpoint
	}
	return ""
}

// GetHubProjectID returns the hub-side project ID if configured.
// This is the ID of the project on the Hub, which may differ from the local
// deterministic project_id (especially for git-based projects where project_id
// is a UUID v5 hash of the git remote).
func (s *Settings) GetHubProjectID() string {
	if s.Hub != nil {
		return s.Hub.ProjectID
	}
	return ""
}

// IsHubConfigured returns true if Hub settings are configured.
func (s *Settings) IsHubConfigured() bool {
	return s.Hub != nil && s.Hub.Endpoint != ""
}

// IsHubEnabled returns true if Hub integration is enabled.
// Hub is considered enabled when:
//  1. Hub credentials (token or apiKey) AND an endpoint are present.
//     Credentials imply intent to use the hub and override any
//     hub.enabled setting from config files. OR
//  2. hub.enabled is explicitly set to true (without credentials).
//
// This allows users with SCION_HUB_TOKEN and SCION_HUB_ENDPOINT env vars
// to interact with the hub without requiring an explicit hub.enabled=true,
// even if a stale hub.enabled=false exists in a settings file.
func (s *Settings) IsHubEnabled() bool {
	if s.Hub == nil {
		return false
	}
	// Credentials + endpoint present: always enabled (env vars override config)
	if s.Hub.Endpoint != "" && (s.Hub.Token != "" || s.Hub.APIKey != "") {
		return true
	}
	// Fall back to explicit hub.enabled setting
	return s.Hub.Enabled != nil && *s.Hub.Enabled
}

// IsHubLinked returns true if this grove has been explicitly linked to the Hub
// via 'scion hub link'. A grove can be hub-enabled without being linked.
func (s *Settings) IsHubLinked() bool {
	return s.Hub != nil && s.Hub.Linked != nil && *s.Hub.Linked
}

// IsHubExplicitlyDisabled returns true if Hub integration is explicitly disabled.
// Returns false if not configured (nil) or enabled.
func (s *Settings) IsHubExplicitlyDisabled() bool {
	return s.Hub != nil && s.Hub.Enabled != nil && !*s.Hub.Enabled
}

// IsHubLocalOnly returns true if the grove is configured for local-only mode.
// When true, Hub sync checks will error with guidance to use --no-hub.
func (s *Settings) IsHubLocalOnly() bool {
	return s.Hub != nil && s.Hub.LocalOnly != nil && *s.Hub.LocalOnly
}

// DeleteHubConnection removes a hub connection entry from settings at the specified scope.
// It loads the existing settings file, removes the named connection, and saves.
func DeleteHubConnection(grovePath string, name string, global bool) error {
	var dir string
	if global {
		globalDir, err := GetGlobalDir()
		if err != nil {
			return err
		}
		dir = globalDir
	} else {
		if grovePath == "" {
			return fmt.Errorf("grove path required for local settings")
		}
		// Resolve through GetProjectConfigDir so that git groves with split
		// storage write to the external config dir (~/.scion/project-configs/…)
		// — the same location LoadSettingsKoanf reads from.
		dir = GetProjectConfigDir(grovePath)
	}

	existingPath := GetSettingsPath(dir)
	targetPath := filepath.Join(dir, "settings.yaml")

	var current Settings
	if existingPath != "" {
		data, err := os.ReadFile(existingPath)
		if err != nil && !os.IsNotExist(err) {
			return err
		}
		if err == nil {
			if filepath.Ext(existingPath) == ".json" {
				if err := util.UnmarshalJSONC(data, &current); err != nil {
					return fmt.Errorf("failed to parse existing settings at %s: %w", existingPath, err)
				}
			} else {
				if err := yaml.Unmarshal(data, &current); err != nil {
					return fmt.Errorf("failed to parse existing settings at %s: %w", existingPath, err)
				}
			}
		}
	}

	if current.HubConnections != nil {
		delete(current.HubConnections, name)
		// Clean up empty map
		if len(current.HubConnections) == 0 {
			current.HubConnections = nil
		}
	}

	if err := os.MkdirAll(filepath.Dir(targetPath), 0755); err != nil {
		return err
	}
	newData, err := yaml.Marshal(current)
	if err != nil {
		return err
	}
	if err := os.WriteFile(targetPath, newData, 0644); err != nil {
		return err
	}

	// If we migrated from JSON, remove the old JSON file
	if existingPath != "" && existingPath != targetPath && filepath.Ext(existingPath) == ".json" {
		_ = os.Remove(existingPath)
	}

	return nil
}
