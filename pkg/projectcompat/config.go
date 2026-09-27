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

package projectcompat

const (
	ConfigProjectIDKey     = "project_id"
	ConfigHubProjectIDKey  = "hub.project_id"
	ConfigHubProjectIDJSON = "hub.projectId"
	// ConfigGroveIDKey is the legacy top-level grove_id key name. It is no
	// longer accepted as CLI key-name input (see IsProjectIDConfigKey), but
	// it is still used to map SCION_GROVE_ID onto the versioned settings
	// loader's raw field name (see EnvProjectIDConfigKey).
	ConfigGroveIDKey = "grove_id"
	// ConfigHubGroveIDKey is the legacy hub.grove_id settings-FILE key. It
	// keeps working as a read-only fallback when a settings file still has
	// a `hub: grove_id:` entry; it is not accepted as `config get/set`
	// key-name input. Do not delete: pkg/config/koanf.go and
	// pkg/config/settings_v1.go still read it, and a follow-up change
	// migrates settings files that still use it.
	ConfigHubGroveIDKey = "hub.grove_id"

	EnvProjectID    = "SCION_PROJECT_ID"
	EnvGroveID      = "SCION_GROVE_ID"
	EnvHubProjectID = "SCION_HUB_PROJECT_ID"

	ProjectIDFile = "project-id"

	ProjectConfigsDir = "project-configs"
	GroveConfigsDir   = "grove-configs"
	ProjectsDir       = "projects"
	GrovesDir         = "groves"
)

// IsProjectIDConfigKey reports whether key is the canonical top-level
// project-id config key name. The legacy grove_id key name is no longer
// accepted as CLI input (see ConfigGroveIDKey for the settings-file fallback
// that still exists).
func IsProjectIDConfigKey(key string) bool {
	return key == ConfigProjectIDKey
}

// IsHubProjectIDConfigKey reports whether key is a canonical hub project-id
// config key name. The legacy hub.grove_id / hub.groveId key names are no
// longer accepted as CLI input.
func IsHubProjectIDConfigKey(key string) bool {
	switch key {
	case ConfigHubProjectIDKey, ConfigHubProjectIDJSON:
		return true
	default:
		return false
	}
}

func EnvProjectIDConfigKey(envName string, hubProjectAsTopLevel bool) (string, bool) {
	switch envName {
	case EnvProjectID, EnvGroveID:
		if envName == EnvGroveID && !hubProjectAsTopLevel {
			return ConfigGroveIDKey, true
		}
		return ConfigProjectIDKey, true
	case EnvHubProjectID:
		if hubProjectAsTopLevel {
			return ConfigProjectIDKey, true
		}
		return ConfigHubProjectIDKey, true
	default:
		return "", false
	}
}

// ProjectIDFromEnv returns the canonical project identity from an environment
// lookup. The canonical name wins when both canonical and legacy aliases exist.
func ProjectIDFromEnv(getenv func(string) string) string {
	if getenv == nil {
		return ""
	}
	if projectID := getenv(EnvProjectID); projectID != "" {
		return projectID
	}
	return getenv(EnvGroveID)
}
