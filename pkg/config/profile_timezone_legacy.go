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
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/knadh/koanf/parsers/json"
	"github.com/knadh/koanf/parsers/yaml"
	"github.com/knadh/koanf/v2"
)

// LegacyProfileTimezoneKey is the key the removed runtime-profile timezone
// field used inside a profile object (profiles.<name>.timezone). The field
// is gone from V1ProfileConfig; the key is only read to retire stored values.
const LegacyProfileTimezoneKey = "timezone"

// LegacyProfileTimezone is one profiles.<name>.timezone value found in raw
// settings material.
type LegacyProfileTimezone struct {
	Profile string
	// Timezone is the stored value. A non-string value is rendered with
	// fmt's %v so it can still be reported and stripped.
	Timezone string
}

// LegacyProfileTimezonesFromMap walks profiles.<name>.timezone in a raw,
// unflattened settings document (the top-level map, which holds a
// "profiles" key) and returns every key it finds, including empty values,
// sorted by profile name.
//
// It walks the map rather than using dotted koanf paths because a profile
// name may contain '.', which koanf's "." delimiter would split.
func LegacyProfileTimezonesFromMap(raw map[string]any) []LegacyProfileTimezone {
	profiles, ok := raw["profiles"].(map[string]any)
	if !ok {
		return nil
	}
	return LegacyProfileTimezonesFromProfiles(profiles)
}

// LegacyProfileTimezonesFromProfiles is LegacyProfileTimezonesFromMap for a
// map that is already the profiles section (keys are profile names), such
// as the hub_settings "profiles" row.
func LegacyProfileTimezonesFromProfiles(profiles map[string]any) []LegacyProfileTimezone {
	var found []LegacyProfileTimezone
	for name, v := range profiles {
		p, ok := v.(map[string]any)
		if !ok {
			continue
		}
		tz, ok := p[LegacyProfileTimezoneKey]
		if !ok {
			continue
		}
		s, isString := tz.(string)
		if !isString {
			if tz == nil {
				s = ""
			} else {
				s = fmt.Sprintf("%v", tz)
			}
		}
		found = append(found, LegacyProfileTimezone{Profile: name, Timezone: s})
	}
	sort.Slice(found, func(i, j int) bool { return found[i].Profile < found[j].Profile })
	return found
}

// SettingsFileProfileTimezoneScan is the result of ScanSettingsFileProfileTimezones.
type SettingsFileProfileTimezoneScan struct {
	// Path is the settings file that was read, or "" if dir has none.
	Path string
	// DefaultTimezone is the file's top-level default_timezone value (the
	// settings.yaml form of agent_defaults.default_timezone).
	DefaultTimezone string
	// ProfileTimezones lists every profiles.<name>.timezone key in the file.
	ProfileTimezones []LegacyProfileTimezone
}

// ScanSettingsFileProfileTimezones reads the global settings file in dir
// (the same file and parser loadSettingsFile picks: settings.yaml, then
// settings.yml, then settings.json) into a raw map and reports its legacy
// profiles.<name>.timezone keys. The file is never modified.
func ScanSettingsFileProfileTimezones(dir string) (SettingsFileProfileTimezoneScan, error) {
	candidates := []struct {
		name   string
		parser koanf.Parser
	}{
		{"settings.yaml", yaml.Parser()},
		{"settings.yml", yaml.Parser()},
		{"settings.json", json.Parser()},
	}
	for _, c := range candidates {
		path := filepath.Join(dir, c.name)
		data, err := os.ReadFile(path)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return SettingsFileProfileTimezoneScan{Path: path}, err
		}
		raw, err := c.parser.Unmarshal(data)
		if err != nil {
			return SettingsFileProfileTimezoneScan{Path: path}, fmt.Errorf("parsing %s: %w", path, err)
		}
		scan := SettingsFileProfileTimezoneScan{
			Path:             path,
			ProfileTimezones: LegacyProfileTimezonesFromMap(raw),
		}
		if s, ok := raw["default_timezone"].(string); ok {
			scan.DefaultTimezone = s
		}
		return scan, nil
	}
	return SettingsFileProfileTimezoneScan{}, nil
}

// DeleteLegacyProfileTimezones removes each found profiles.<name>.timezone
// key from k, so seed material extracted from k no longer carries it. The
// names come from a raw map walk; koanf splits a dotted profile name into
// nested maps on load, and Delete splits the path the same way, so the
// matching nested key is the one removed. A profile left with no keys is
// dropped by koanf.
func DeleteLegacyProfileTimezones(k *koanf.Koanf, found []LegacyProfileTimezone) {
	if k == nil {
		return
	}
	for _, f := range found {
		k.Delete("profiles." + f.Profile + "." + LegacyProfileTimezoneKey)
	}
}
