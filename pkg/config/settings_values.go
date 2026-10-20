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
	"sort"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/projectkeys"
)

// SettingValue is one setting's effective value and where it came from, as
// "scion config list" and "scion config get" show it.
type SettingValue struct {
	// Key is the setting's name, as config get and config set take it.
	Key string `json:"key"`
	// Value is the effective value, masked for a credential-like key
	// (IsSecretSettingKey). Empty when the setting is not set.
	Value string `json:"value"`
	// Source is the layer that set the value: "default", "global <file>",
	// "project <file>", "env <VARIABLE>" or "project-id file <file>".
	// Empty when the setting is not set.
	Source string `json:"source"`
}

// secretMask replaces the value of a credential-like setting.
const secretMask = "********"

// settingListKeys are the keys "config list" shows: every key "config set"
// accepts, the hub transport settings, and each configured hub connection's
// endpoint. Sorted.
func settingListKeys(vs *VersionedSettings) []string {
	keys := make([]string, 0, len(versionedSettingKeys)+4)
	for k := range versionedSettingKeys {
		keys = append(keys, k)
	}
	keys = append(keys, projectkeys.ConfigHubProjectIDKey, "hub.transport.mode", "hub.transport.audience")
	for name := range vs.HubConnections {
		keys = append(keys, "hub_connections."+name+".endpoint")
	}
	sort.Strings(keys)
	return keys
}

// settingSourceKey returns the settings path a setting key is stored at,
// the key of versionedLoadResult.sources.
func settingSourceKey(key string) string {
	if projectkeys.IsProjectIDConfigKey(key) || projectkeys.IsHubProjectIDConfigKey(key) {
		return projectkeys.ConfigHubProjectIDKey
	}
	if k, ok := versionedSettingKeys[key]; ok {
		return strings.Join(k.path, ".")
	}
	return key
}

// loadSettingValues loads projectPath's settings with their sources.
func loadSettingValues(projectPath string) (*VersionedSettings, *versionedLoadResult, error) {
	vs, res, err := loadVersionedSettings(projectPath, versionedLoadOptions{withDefaults: true, trackSources: true})
	if err != nil {
		return nil, nil, err
	}
	if o := globalOverlay; o != nil {
		o.Apply(vs)
	}
	return vs, res, nil
}

// settingValueOf returns key's value and source in vs. A setting with no
// value has no source.
func settingValueOf(vs *VersionedSettings, res *versionedLoadResult, key string) (SettingValue, error) {
	value, err := GetVersionedSettingValue(vs, key)
	if err != nil {
		return SettingValue{}, err
	}
	sv := SettingValue{Key: key, Value: value}
	if value != "" {
		sv.Source = res.sources[settingSourceKey(key)]
		if IsSecretSettingKey(key) {
			sv.Value = secretMask
		}
	}
	return sv, nil
}

// ResolveSettingValues returns the value and source of every setting
// "config list" shows (settingListKeys), for projectPath ("" for the
// current directory's project, or the global settings outside a project).
// It reads the same settings, through the same function, as
// ResolveSettingValue, so the two always agree.
func ResolveSettingValues(projectPath string) ([]SettingValue, error) {
	vs, res, err := loadSettingValues(projectPath)
	if err != nil {
		return nil, err
	}
	keys := settingListKeys(vs)
	out := make([]SettingValue, 0, len(keys))
	for _, key := range keys {
		sv, err := settingValueOf(vs, res, key)
		if err != nil {
			return nil, err
		}
		out = append(out, sv)
	}
	return out, nil
}

// ResolveSettingValue returns one setting's value and source, as
// "config get" shows it. key is any key GetVersionedSettingValue accepts.
func ResolveSettingValue(projectPath, key string) (SettingValue, error) {
	vs, res, err := loadSettingValues(projectPath)
	if err != nil {
		return SettingValue{}, err
	}
	return settingValueOf(vs, res, key)
}
