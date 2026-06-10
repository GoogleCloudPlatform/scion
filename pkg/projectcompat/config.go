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
	ConfigGroveIDKey       = "grove_id"
	ConfigHubProjectIDKey  = "hub.project_id"
	ConfigHubProjectIDJSON = "hub.projectId"
	ConfigHubGroveIDKey    = "hub.grove_id"
	ConfigHubGroveIDJSON   = "hub.groveId"

	EnvProjectID    = "SCION_PROJECT_ID"
	EnvGroveID      = "SCION_GROVE_ID"
	EnvHubProjectID = "SCION_HUB_PROJECT_ID"
	EnvHubGroveID   = "SCION_HUB_GROVE_ID"

	ProjectIDFile = "project-id"
	GroveIDFile   = "grove-id"

	ProjectConfigsDir = "project-configs"
	GroveConfigsDir   = "grove-configs"
	ProjectsDir       = "projects"
	GrovesDir         = "groves"
)

func IsProjectIDConfigKey(key string) bool {
	return key == ConfigProjectIDKey || key == ConfigGroveIDKey
}

func IsHubProjectIDConfigKey(key string) bool {
	switch key {
	case ConfigHubProjectIDKey, ConfigHubProjectIDJSON, ConfigHubGroveIDKey, ConfigHubGroveIDJSON:
		return true
	default:
		return false
	}
}

func CanonicalConfigKey(key string) (canonical string, legacy bool) {
	switch {
	case IsProjectIDConfigKey(key):
		return ConfigProjectIDKey, key == ConfigGroveIDKey
	case IsHubProjectIDConfigKey(key):
		return ConfigHubProjectIDKey, key == ConfigHubGroveIDKey || key == ConfigHubGroveIDJSON
	default:
		return CanonicalFieldAliases(key)
	}
}

func EnvProjectIDConfigKey(envName string, hubProjectAsTopLevel bool) (string, bool) {
	switch envName {
	case EnvProjectID, EnvGroveID:
		if envName == EnvGroveID && !hubProjectAsTopLevel {
			return ConfigGroveIDKey, true
		}
		return ConfigProjectIDKey, true
	case EnvHubProjectID, EnvHubGroveID:
		if hubProjectAsTopLevel {
			return ConfigProjectIDKey, true
		}
		if envName == EnvHubGroveID {
			return ConfigHubGroveIDKey, true
		}
		return ConfigHubProjectIDKey, true
	default:
		return "", false
	}
}
