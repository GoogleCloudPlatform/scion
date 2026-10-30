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

// loadSettingValues loads projectPath's settings with their sources,
// leaving out the SCION_* environment variables named in skipEnv.
func loadSettingValues(projectPath string, skipEnv ...string) (*VersionedSettings, *versionedLoadResult, error) {
	opts := versionedLoadOptions{withDefaults: true, trackSources: true}
	if len(skipEnv) > 0 {
		opts.skipEnv = make(map[string]bool, len(skipEnv))
		for _, name := range skipEnv {
			opts.skipEnv[name] = true
		}
	}
	vs, res, err := loadVersionedSettings(projectPath, opts)
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
		sv.Source = lookupSource(res.sources, settingSourceKey(key))
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

// ResolveSettingValueIgnoringEnv is ResolveSettingValue as if the
// environment variable envName were not set: the value and source the key
// would have without it. config unset uses it to say what a key falls
// back to when the variable that sets it applies to one command only.
func ResolveSettingValueIgnoringEnv(projectPath, key, envName string) (SettingValue, error) {
	vs, res, err := loadSettingValues(projectPath, envName)
	if err != nil {
		return SettingValue{}, err
	}
	return settingValueOf(vs, res, key)
}

// lookupSource returns the source recorded for path. The settings decode
// matches key names case-insensitively, so a key spelt in another case in a
// file (hub.Endpoint) is looked up case-insensitively when the exact match
// misses. That lookup only answers when it is unambiguous: when layers spell
// the key differently, which spelling the decode used is not known here,
// so "" is returned rather than a guess.
func lookupSource(sources map[string]string, path string) string {
	if src, ok := sources[path]; ok {
		return src
	}
	found := ""
	for k, src := range sources {
		if !strings.EqualFold(k, path) {
			continue
		}
		if found != "" && found != src {
			return ""
		}
		found = src
	}
	return found
}
