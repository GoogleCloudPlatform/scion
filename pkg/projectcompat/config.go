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

	EnvProjectID    = "SCION_PROJECT_ID"
	EnvHubProjectID = "SCION_HUB_PROJECT_ID"

	ProjectIDFile = "project-id"

	ProjectConfigsDir = "project-configs"
	ProjectsDir       = "projects"
)

// IsProjectIDConfigKey reports whether key is the canonical top-level
// project-id config key name. The legacy grove_id key name is no longer
// accepted as CLI input.
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
	case EnvProjectID:
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
// lookup.
func ProjectIDFromEnv(getenv func(string) string) string {
	if getenv == nil {
		return ""
	}
	return getenv(EnvProjectID)
}
