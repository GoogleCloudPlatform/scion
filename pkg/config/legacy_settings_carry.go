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
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/util"
	koanfyaml "github.com/knadh/koanf/parsers/yaml"
	"github.com/knadh/koanf/providers/rawbytes"
	"github.com/knadh/koanf/v2"
	yamlv3 "gopkg.in/yaml.v3"
)

// legacySettingsTopLevelKeys is the set of top-level keys the legacy Settings
// struct decodes. It is derived from the struct's yaml tags so it cannot
// drift from the struct.
var legacySettingsTopLevelKeys = func() map[string]bool {
	keys := make(map[string]bool)
	t := reflect.TypeOf(Settings{})
	for i := 0; i < t.NumField(); i++ {
		name, _, _ := strings.Cut(t.Field(i).Tag.Get("yaml"), ",")
		if name != "" && name != "-" {
			keys[name] = true
		}
	}
	return keys
}()

// legacyCarriedTopLevelKeys returns the top-level entries of an unversioned
// settings file that the legacy Settings struct does not decode, such as the
// v1-only server and image_registry keys. The legacy conversion
// (AdaptLegacySettings) never sees these, so MigrateSettingsFile carries them
// into the migrated file unchanged instead of dropping them
// (ptone/scion#3497). Keys the legacy struct decodes are converted by
// AdaptLegacySettings and are not returned. For JSON the match is
// case-insensitive, as json.Unmarshal matches struct fields, so a "Hub" key
// that the legacy decode converted is not carried a second time.
func legacyCarriedTopLevelKeys(data []byte, isJSON bool) (map[string]interface{}, error) {
	var raw map[string]interface{}
	var err error
	if isJSON {
		err = json.Unmarshal(data, &raw)
	} else {
		err = yamlv3.Unmarshal(data, &raw)
	}
	if err != nil {
		return nil, err
	}
	carried := make(map[string]interface{})
	for k, v := range raw {
		if k == "schema_version" || isLegacySettingsTopLevelKey(k, isJSON) {
			continue
		}
		carried[k] = v
	}
	return carried, nil
}

// isLegacySettingsTopLevelKey reports whether the legacy Settings decode
// consumes the top-level key k: an exact match for YAML, a case-insensitive
// one for JSON.
func isLegacySettingsTopLevelKey(k string, isJSON bool) bool {
	if legacySettingsTopLevelKeys[k] {
		return true
	}
	if isJSON {
		for lk := range legacySettingsTopLevelKeys {
			if strings.EqualFold(k, lk) {
				return true
			}
		}
	}
	return false
}

// mergeCarriedSettings adds the carried top-level entries to the mapping
// node root, which holds the converted settings. A key root does not have is
// appended with the carried value. When both hold a mapping under the same
// key the mappings are merged the same way, recursively, so converted fields
// (e.g. server.broker.broker_id from a legacy hub.brokerId) win and carried
// siblings (e.g. server.broker.port) are kept. Any other clash keeps the
// converted value; the dotted path of each carried value dropped that way
// (when it differs from the converted one) is returned, sorted, so the
// caller can report it.
func mergeCarriedSettings(root *yamlv3.Node, carried map[string]interface{}) ([]string, error) {
	keys := make([]string, 0, len(carried))
	for k := range carried {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var dropped []string
	for _, k := range keys {
		var v yamlv3.Node
		if err := v.Encode(carried[k]); err != nil {
			return nil, fmt.Errorf("failed to encode settings key %q: %w", k, err)
		}
		dropped = mergeYAMLMappingEntry(root, k, k, &v, dropped)
	}
	sort.Strings(dropped)
	return dropped, nil
}

// mergeYAMLMappingEntry sets key to value in mapping unless mapping already
// has key, in which case two mappings are merged recursively and any other
// existing value is kept. path is the dotted path of key; the path of a
// discarded value that differs from the kept one is appended to dropped.
func mergeYAMLMappingEntry(mapping *yamlv3.Node, key, path string, value *yamlv3.Node, dropped []string) []string {
	_, existing := findMapKey(mapping, key)
	if existing == nil {
		mapping.Content = append(mapping.Content, newYAMLStringScalar(key), value)
		return dropped
	}
	if existing.Kind != yamlv3.MappingNode || value.Kind != yamlv3.MappingNode {
		if !yamlNodesEqual(existing, value) {
			dropped = append(dropped, path)
		}
		return dropped
	}
	for i := 0; i+1 < len(value.Content); i += 2 {
		k := value.Content[i].Value
		dropped = mergeYAMLMappingEntry(existing, k, path+"."+k, value.Content[i+1], dropped)
	}
	return dropped
}

// yamlNodesEqual reports whether a and b decode to the same data.
func yamlNodesEqual(a, b *yamlv3.Node) bool {
	var av, bv interface{}
	if a.Decode(&av) != nil || b.Decode(&bv) != nil {
		return false
	}
	return reflect.DeepEqual(av, bv)
}

// marshalMigratedSettings returns the YAML for vs with the carried top-level
// entries merged in, and the paths of carried values the merge dropped (see
// mergeCarriedSettings).
func marshalMigratedSettings(vs *VersionedSettings, carried map[string]interface{}) ([]byte, []string, error) {
	var root yamlv3.Node
	if err := root.Encode(vs); err != nil {
		return nil, nil, err
	}
	dropped, err := mergeCarriedSettings(&root, carried)
	if err != nil {
		return nil, nil, err
	}
	data, err := encodeYAMLDocument(&yamlv3.Node{Kind: yamlv3.DocumentNode, Content: []*yamlv3.Node{&root}}, 4)
	return data, dropped, err
}

// checkMigratedSettingsDecode reports whether data, migrated v1 settings
// YAML, loads the way the settings loaders read it: a yaml.v3 decode into
// VersionedSettings (LoadSingleFileVersioned, UpdateVersionedSetting) and
// the koanf/mapstructure decode (LoadSettingsKoanf, LoadGlobalSettings).
// Unknown keys are not errors for either; a wrongly typed value (for
// example "server: hello") is.
func checkMigratedSettingsDecode(data []byte) error {
	var vs VersionedSettings
	if err := yamlv3.Unmarshal(data, &vs); err != nil {
		return err
	}
	k := koanf.New(".")
	if err := k.Load(rawbytes.Provider(data), koanfyaml.Parser()); err != nil {
		return err
	}
	_, err := decodeCollectingUnused(k, &VersionedSettings{})
	return err
}

// checkCarriedSettingsDecode returns an error when the migrated output
// (vs with the carried keys merged in) would not load as v1 settings. The
// error names each carried key that fails on its own, so the user knows
// what to fix in the original file.
func checkCarriedSettingsDecode(vs *VersionedSettings, carried map[string]interface{}, merged []byte) error {
	mergedErr := checkMigratedSettingsDecode(merged)
	if mergedErr == nil {
		return nil
	}
	var bad []string
	for k, v := range carried {
		data, _, err := marshalMigratedSettings(vs, map[string]interface{}{k: v})
		if err != nil || checkMigratedSettingsDecode(data) != nil {
			bad = append(bad, k)
		}
	}
	sort.Strings(bad)
	if len(bad) == 0 {
		return mergedErr
	}
	return fmt.Errorf("top-level key(s) %s would not load as v1 settings: %w", strings.Join(bad, ", "), mergedErr)
}

// saveVersionedSettingsData writes already-marshalled v1 settings YAML to dir
// the way SaveVersionedSettings writes a struct: to newSettingsFilePath(dir),
// atomically, under the settings-file lock, and not at all when the bytes
// would not change.
func saveVersionedSettingsData(dir string, data []byte) error {
	unlock := LockSettingsFile()
	defer unlock()
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("failed to create directory %s: %w", dir, err)
	}
	targetPath := newSettingsFilePath(dir)
	if existing, err := os.ReadFile(targetPath); err == nil && bytes.Equal(existing, data) {
		return nil
	}
	return writeSettingsFileAtomic(targetPath, data)
}

// deleteHubConnectionFromFile removes hub_connections.<name> from the
// settings file in dir, editing the parsed YAML tree so every other key
// (including v1-only keys such as server and image_registry, and the whole
// content of a versioned file) is kept. hub_connections is removed when it
// becomes empty. A YAML file is rewritten in place only when it changes; a
// JSON file is converted to YAML in newSettingsFilePath(dir) and removed, as
// before. A missing file is left missing.
//
// It does not take LockSettingsFile: its caller (scion broker deregister,
// via DeleteHubConnection) already holds it from its own read to this
// write, and the lock is not reentrant.
func deleteHubConnectionFromFile(dir, name string) error {
	existingPath := GetSettingsPath(dir)
	if existingPath == "" {
		return nil
	}
	data, err := os.ReadFile(existingPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	isJSON := filepath.Ext(existingPath) == ".json"

	var doc *yamlv3.Node
	if isJSON {
		var raw map[string]interface{}
		if err := util.UnmarshalJSONC(data, &raw); err != nil {
			return fmt.Errorf("failed to parse existing settings at %s: %w", existingPath, err)
		}
		var root yamlv3.Node
		if raw == nil {
			raw = map[string]interface{}{}
		}
		if err := root.Encode(raw); err != nil {
			return fmt.Errorf("failed to convert settings at %s: %w", existingPath, err)
		}
		doc = &yamlv3.Node{Kind: yamlv3.DocumentNode, Content: []*yamlv3.Node{&root}}
	} else {
		if doc, err = parseYAMLMappingDocument(data); err != nil {
			return fmt.Errorf("failed to parse existing settings at %s: %w", existingPath, err)
		}
	}
	root := doc.Content[0]

	changed, err := deleteYAMLPath(root, []string{"hub_connections", name})
	if err != nil {
		return fmt.Errorf("failed to update %s: %w", existingPath, err)
	}
	if _, conns := findMapKey(root, "hub_connections"); conns != nil &&
		(isYAMLNull(conns) || (conns.Kind == yamlv3.MappingNode && len(conns.Content) == 0)) {
		deleteMapKey(root, "hub_connections")
		changed = true
	}
	if !changed && !isJSON {
		return nil
	}

	out, err := encodeYAMLDocument(doc, detectYAMLIndent(root))
	if err != nil {
		return fmt.Errorf("failed to marshal settings: %w", err)
	}
	if !isJSON {
		return writeSettingsFileAtomic(existingPath, out)
	}
	if err := writeSettingsFileAtomic(newSettingsFilePath(dir), out); err != nil {
		return err
	}
	_ = os.Remove(existingPath)
	return nil
}
