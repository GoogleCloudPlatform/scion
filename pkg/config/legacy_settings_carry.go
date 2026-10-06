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
// AdaptLegacySettings and are not returned.
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
		if legacySettingsTopLevelKeys[k] || k == "schema_version" {
			continue
		}
		carried[k] = v
	}
	return carried, nil
}

// mergeCarriedSettings adds the carried top-level entries to the mapping
// node root, which holds the converted settings. A key root does not have is
// appended with the carried value. When both hold a mapping under the same
// key the mappings are merged the same way, recursively, so converted fields
// (e.g. server.broker.broker_id from a legacy hub.brokerId) win and carried
// siblings (e.g. server.broker.port) are kept. Any other clash keeps the
// converted value.
func mergeCarriedSettings(root *yamlv3.Node, carried map[string]interface{}) error {
	keys := make([]string, 0, len(carried))
	for k := range carried {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		var v yamlv3.Node
		if err := v.Encode(carried[k]); err != nil {
			return fmt.Errorf("failed to encode settings key %q: %w", k, err)
		}
		mergeYAMLMappingEntry(root, k, &v)
	}
	return nil
}

// mergeYAMLMappingEntry sets key to value in mapping unless mapping already
// has key, in which case two mappings are merged recursively and any other
// existing value is kept.
func mergeYAMLMappingEntry(mapping *yamlv3.Node, key string, value *yamlv3.Node) {
	_, existing := findMapKey(mapping, key)
	if existing == nil {
		mapping.Content = append(mapping.Content, newYAMLStringScalar(key), value)
		return
	}
	if existing.Kind != yamlv3.MappingNode || value.Kind != yamlv3.MappingNode {
		return
	}
	for i := 0; i+1 < len(value.Content); i += 2 {
		mergeYAMLMappingEntry(existing, value.Content[i].Value, value.Content[i+1])
	}
}

// marshalMigratedSettings returns the YAML for vs with the carried top-level
// entries merged in (see mergeCarriedSettings).
func marshalMigratedSettings(vs *VersionedSettings, carried map[string]interface{}) ([]byte, error) {
	var root yamlv3.Node
	if err := root.Encode(vs); err != nil {
		return nil, err
	}
	if err := mergeCarriedSettings(&root, carried); err != nil {
		return nil, err
	}
	return encodeYAMLDocument(&yamlv3.Node{Kind: yamlv3.DocumentNode, Content: []*yamlv3.Node{&root}}, 4)
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
func deleteHubConnectionFromFile(dir, name string) error {
	unlock := LockSettingsFile()
	defer unlock()

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
