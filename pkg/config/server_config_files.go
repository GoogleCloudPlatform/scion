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

// ServerConfigFiles names the files LoadGlobalConfig(configPath) reads the
// server configuration from: the settings file whose server key it uses,
// else the legacy server.yaml files. It is for startup logs; it reads no
// values.
func ServerConfigFiles(configPath string) []string {
	src := resolveServerSettingsSource(configPath)
	if !src.legacy && src.settingsDir != "" {
		if p := GetSettingsPath(src.settingsDir); p != "" {
			return []string{p}
		}
	}
	return serverConfigSources(configPath)
}
