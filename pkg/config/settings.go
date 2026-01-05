package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/ptone/scion-agent/pkg/api"
)

type RuntimeConfig struct {
	Host      string            `json:"host,omitempty"`
	Context   string            `json:"context,omitempty"`
	Namespace string            `json:"namespace,omitempty"`
	Tmux      *bool             `json:"tmux,omitempty"`
	Env       map[string]string `json:"env,omitempty"`
	Sync      string            `json:"sync,omitempty"`
}

type HarnessConfig struct {
	Image            string            `json:"image"`
	User             string            `json:"user"`
	Env              map[string]string `json:"env,omitempty"`
	Volumes          []api.VolumeMount `json:"volumes,omitempty"`
	AuthSelectedType string            `json:"auth_selectedType,omitempty"`
}

type HarnessOverride struct {
	Image            string            `json:"image,omitempty"`
	User             string            `json:"user,omitempty"`
	Env              map[string]string `json:"env,omitempty"`
	Volumes          []api.VolumeMount `json:"volumes,omitempty"`
	AuthSelectedType string            `json:"auth_selectedType,omitempty"`
}

type ProfileConfig struct {
	Runtime          string                     `json:"runtime"`
	Tmux             *bool                      `json:"tmux,omitempty"`
	Env              map[string]string          `json:"env,omitempty"`
	Volumes          []api.VolumeMount          `json:"volumes,omitempty"`
	HarnessOverrides map[string]HarnessOverride `json:"harness_overrides,omitempty"`
}

type Settings struct {
	ActiveProfile   string                   `json:"active_profile"`
	DefaultTemplate string                   `json:"default_template,omitempty"`
	Runtimes        map[string]RuntimeConfig `json:"runtimes"`
	Harnesses       map[string]HarnessConfig `json:"harnesses"`
	Profiles        map[string]ProfileConfig `json:"profiles"`
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

// LoadSettings loads and merges settings from the hierarchy.
// Priority: Grove > Global > Defaults
func LoadSettings(grovePath string) (*Settings, error) {
	// 1. Start with App Defaults from embedded JSON
	settings := &Settings{
		Runtimes:  make(map[string]RuntimeConfig),
		Harnesses: make(map[string]HarnessConfig),
		Profiles:  make(map[string]ProfileConfig),
	}

	if defaultData, err := GetDefaultSettingsData(); err == nil {
		if err := MergeSettings(settings, defaultData); err != nil {
			// This should not happen with embedded defaults
		}
	} else {
		// Fallback to minimal hardcoded defaults if embed fails
		settings.ActiveProfile = "local"
		settings.DefaultTemplate = "gemini"
	}

	// 2. Merge Global (~/.scion/settings.json)
	globalDir, err := GetGlobalDir()
	if err == nil {
		globalSettingsPath := filepath.Join(globalDir, "settings.json")
		if err := mergeSettingsFromFile(settings, globalSettingsPath); err != nil {
			if !os.IsNotExist(err) {
				// We still return settings but maybe we should log this
			}
		}
	}

	// 3. Merge Grove settings
	if grovePath != "" {
		groveSettingsPath := filepath.Join(grovePath, "settings.json")

		if err := mergeSettingsFromFile(settings, groveSettingsPath); err != nil {
			if !os.IsNotExist(err) {
				// We still return settings
			}
		}
	}

	return settings, nil
}

func mergeSettingsFromFile(base *Settings, path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}

	return MergeSettings(base, data)
}

func MergeSettings(base *Settings, data []byte) error {
	var override Settings
	if err := json.Unmarshal(data, &override); err != nil {
		return err
	}

	if override.ActiveProfile != "" {
		base.ActiveProfile = override.ActiveProfile
	}
	if override.DefaultTemplate != "" {
		base.DefaultTemplate = override.DefaultTemplate
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
			if v.Tmux != nil {
				existing.Tmux = v.Tmux
			}
			if v.Env != nil {
				existing.Env = mergeMaps(existing.Env, v.Env)
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
				existing.Env = mergeMaps(existing.Env, v.Env)
			}
			if v.Volumes != nil {
				existing.Volumes = append(existing.Volumes, v.Volumes...)
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
			if v.Tmux != nil {
				existing.Tmux = v.Tmux
			}
			if v.Env != nil {
				existing.Env = mergeMaps(existing.Env, v.Env)
			}
			if v.Volumes != nil {
				existing.Volumes = append(existing.Volumes, v.Volumes...)
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
						hov.Env = mergeMaps(hov.Env, hv.Env)
					}
					if hv.Volumes != nil {
						hov.Volumes = append(hov.Volumes, hv.Volumes...)
					}
					existing.HarnessOverrides[hk] = hov
				}
			}
			base.Profiles[k] = existing
		}
	}

	return nil
}

// SaveSettings saves the settings to the specified location.
func SaveSettings(grovePath string, settings *Settings, global bool) error {
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
func UpdateSetting(grovePath string, key string, value string, global bool) error {
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

	// Load existing file specifically (not merged)
	var current Settings
	data, err := os.ReadFile(targetPath)
	if err == nil {
		if err := json.Unmarshal(data, &current); err != nil {
			return fmt.Errorf("failed to parse existing settings at %s: %w", targetPath, err)
		}
	} else if !os.IsNotExist(err) {
		return err
	}

	// Update the field - this logic needs to be more flexible for the new structure
	// For now, support some basic ones
	switch key {
	case "active_profile":
		current.ActiveProfile = value
	case "default_template":
		current.DefaultTemplate = value
	default:
		return fmt.Errorf("unknown or complex setting key: %s (manual edit recommended for registries)", key)
	}

	// Save
	if err := os.MkdirAll(filepath.Dir(targetPath), 0755); err != nil {
		return err
	}
	newData, err := json.MarshalIndent(current, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(targetPath, newData, 0644)
}

func GetSettingValue(s *Settings, key string) (string, error) {
	switch key {
	case "active_profile":
		return s.ActiveProfile, nil
	case "default_template":
		return s.DefaultTemplate, nil
	}
	return "", fmt.Errorf("unknown or complex setting key: %s", key)
}

func GetSettingsMap(s *Settings) map[string]string {
	m := make(map[string]string)
	m["active_profile"] = s.ActiveProfile
	m["default_template"] = s.DefaultTemplate
	return m
}