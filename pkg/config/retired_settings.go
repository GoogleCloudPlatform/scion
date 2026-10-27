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
	"strings"
)

// RetiredAsyncAgentLaunchWarning is logged at hub startup when the retired
// server.hub.async_agent_launch setting is still set. Async agent create is
// now the hub.async_agent_launch experiment (ptone/scion#4991); the setting
// is ignored, so without this warning a hub that set it would silently go
// synchronous after an upgrade.
const RetiredAsyncAgentLaunchWarning = "server.hub.async_agent_launch is retired; enable the hub.async_agent_launch experiment instead"

// retiredAsyncAgentLaunchEnvVar is the environment variable the retired
// setting was read from.
const retiredAsyncAgentLaunchEnvVar = "SCION_SERVER_HUB_ASYNCAGENTLAUNCH"

// RetiredServerSettingWarnings returns one warning for each retired server
// setting that is still set, whatever its value, in the server config that
// LoadGlobalConfig(configPath) reads (the settings.yaml server block, or the
// legacy server.yaml files when no settings.yaml has one) or in the
// environment. The settings themselves are ignored by the loader.
func RetiredServerSettingWarnings(configPath string) []string {
	if retiredAsyncAgentLaunchSet(configPath) {
		return []string{RetiredAsyncAgentLaunchWarning}
	}
	return nil
}

func retiredAsyncAgentLaunchSet(configPath string) bool {
	if os.Getenv(retiredAsyncAgentLaunchEnvVar) != "" {
		return true
	}
	src := resolveServerSettingsSource(configPath)
	if !src.legacy {
		raw, ok := readSettingsFileRaw(src.settingsDir)
		return ok && hasNestedKey(raw, "server", "hub", "async_agent_launch")
	}
	for _, path := range serverConfigSources(configPath) {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var raw map[string]interface{}
		if unmarshalSettingsYAML(data, &raw) != nil {
			continue
		}
		if hasNestedKey(raw, "hub", "asyncAgentLaunch") {
			return true
		}
	}
	return false
}

// hasNestedKey reports whether the YAML map m has the key path, matching
// each key case-insensitively (the legacy loader's keys are not
// case-sensitive in practice).
func hasNestedKey(m map[string]interface{}, path ...string) bool {
	cur := m
	for i, want := range path {
		var next interface{}
		found := false
		for k, v := range cur {
			if strings.EqualFold(k, want) {
				next, found = v, true
				break
			}
		}
		if !found {
			return false
		}
		if i == len(path)-1 {
			return true
		}
		nm, ok := next.(map[string]interface{})
		if !ok {
			return false
		}
		cur = nm
	}
	return false
}
