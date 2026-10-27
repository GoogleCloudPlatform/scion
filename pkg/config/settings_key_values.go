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

	"github.com/knadh/koanf/maps"
	"github.com/knadh/koanf/v2"
)

// DecodeSettingsKey decodes the single settings key from k into a fresh
// VersionedSettings, with the settings loader's decode options. A list key
// whose value arrives as a comma-separated string (as SCION_SERVER_* list
// variables do) is split first, exactly as the bootstrap merge splits it
// (commaSplitKoanfKeys), so the decoded value is the one the hub uses. It
// fails when k has no value for key, when the value does not decode, or when
// any key of k matches no VersionedSettings field, so a caller never takes a
// value that did not land in a known field.
func DecodeSettingsKey(k *koanf.Koanf, key string) (*VersionedSettings, error) {
	if k == nil || !k.Exists(key) {
		return nil, fmt.Errorf("settings key %q is not set", key)
	}
	single := koanf.New(".")
	if err := single.Set(key, k.Get(key)); err != nil {
		return nil, err
	}
	splitKoanfListKeys(single, commaSplitKoanfKeys)
	vs, ok := settingsProbeStruct(true).(*VersionedSettings)
	if !ok {
		return nil, fmt.Errorf("unexpected settings probe type")
	}
	unused, err := decodeCollectingUnused(single, vs)
	if err != nil {
		return nil, err
	}
	if len(unused) > 0 {
		return nil, fmt.Errorf("settings key %q matches no settings field", key)
	}
	return vs, nil
}

// FlattenSettingsJSON returns the JSON form of v (a settings struct or an
// admin API view of one) as leaves keyed by their dotted path, objects
// flattened and lists kept whole. Settings structs carry the same names in
// their json and koanf tags, so the paths are settings keys. Fields that
// encode as absent (omitempty zero values, json:"-") have no entry.
func FlattenSettingsJSON(v any) (map[string]any, error) {
	data, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, err
	}
	flat, _ := maps.Flatten(m, nil, ".")
	return flat, nil
}

// IsZeroSettingScalar reports whether v is a scalar zero value as a settings
// source gives it (false, 0, or the strings "false" and "0"): the values a
// settings struct field may hold while encoding as absent under omitempty.
func IsZeroSettingScalar(v any) bool {
	switch t := v.(type) {
	case bool:
		return !t
	case string:
		return t == "false" || t == "0"
	case int:
		return t == 0
	case int64:
		return t == 0
	case float64:
		return t == 0
	}
	return false
}
