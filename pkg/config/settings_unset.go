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
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/projectkeys"
	koanfjson "github.com/knadh/koanf/parsers/json"
	"github.com/knadh/koanf/parsers/yaml"
	"github.com/knadh/koanf/providers/confmap"
	"github.com/knadh/koanf/providers/rawbytes"
	"github.com/knadh/koanf/v2"
	yamlv3 "gopkg.in/yaml.v3"
)

// UnsetResult reports what UnsetSetting did.
type UnsetResult struct {
	// Removed is true when the key was in the settings file and was
	// removed; false when the file (or the key) did not exist, and nothing
	// was written.
	Removed bool
	// Path is the settings file of the layer ("" when there is none).
	Path string
	// MigrationWarnings are the deprecation warnings of a legacy-format
	// file that was migrated to the v1 format before the key was removed.
	MigrationWarnings []string
}

// unsetPathFor returns the settings path UnsetSetting removes for key: the
// keys "config set" accepts (and their project ID aliases). Other keys are
// rejected, as config set rejects them.
func unsetPathFor(key string) ([]string, error) {
	if projectkeys.IsProjectIDConfigKey(key) || projectkeys.IsHubProjectIDConfigKey(key) {
		return []string{"hub", "project_id"}, nil
	}
	if k, ok := versionedSettingKeys[key]; ok {
		return k.path, nil
	}
	return nil, fmt.Errorf("unknown or complex setting key: %s (config unset takes the keys config set takes)", key)
}

// UnsetSetting removes key from one settings file: the project's own
// settings (projectPath, resolved like UpdateSetting) or, with global, the
// global settings. The setting then takes its value from the layers below
// (the global settings or the embedded defaults), unlike setting a boolean
// to false. An environment variable that sets the key still applies.
//
// The write policy applies first (ValidateSettingWrite): a key no layer
// reads is refused and nothing is read or written. The file is changed
// under the settings lock, through the locked writer. A key that is not in
// the file changes nothing, and a legacy-format file without the key is not
// migrated. A legacy-format file with the key is migrated to the v1 format
// first, as UpdateSetting migrates it. A YAML file is edited in place
// (comments and other keys survive); a parent mapping left empty is removed
// with the key.
func UnsetSetting(projectPath, key string, global bool) (*UnsetResult, error) {
	layerGlobal := global || (projectPath != "" && IsGlobalProjectDir(projectPath))
	if err := ValidateSettingWrite(key, layerGlobal); err != nil {
		return nil, err
	}
	path, err := unsetPathFor(key)
	if err != nil {
		return nil, err
	}

	var dir string
	if global {
		if dir, err = GetGlobalDir(); err != nil {
			return nil, err
		}
	} else {
		if projectPath == "" {
			return nil, fmt.Errorf("project path required for local settings")
		}
		dir = GetProjectConfigDir(projectPath)
	}

	res := &UnsetResult{}
	settingsPath := GetSettingsPath(dir)
	if settingsPath == "" {
		return res, nil
	}
	if _, err := os.Stat(dir); err != nil {
		return res, nil
	}

	lock, err := LockSettingsDir(dir)
	if err != nil {
		return nil, err
	}
	defer lock.Unlock()

	settingsPath = GetSettingsPath(dir)
	if settingsPath == "" {
		return res, nil
	}
	res.Path = settingsPath
	data, err := os.ReadFile(settingsPath)
	if err != nil {
		return nil, fmt.Errorf("failed to read %s: %w", settingsPath, err)
	}

	if version, _ := DetectSettingsFormat(data); version == "" {
		present, err := legacyFileSets(data, settingsPath, path)
		if err != nil {
			return nil, err
		}
		if !present {
			return res, nil
		}
		migration, err := migrateSettingsFileLocked(lock, dir, false)
		if err != nil {
			return nil, fmt.Errorf("auto-migration of legacy settings failed: %w\n  Run 'scion config migrate' to migrate manually", err)
		}
		res.MigrationWarnings = migration.Warnings
		settingsPath = GetSettingsPath(dir)
		res.Path = settingsPath
		if data, err = os.ReadFile(settingsPath); err != nil {
			return nil, fmt.Errorf("failed to read %s: %w", settingsPath, err)
		}
	}

	if filepath.Ext(settingsPath) == ".json" {
		removed, err := unsetJSONSettingLocked(lock, settingsPath, data, path)
		res.Removed = removed
		return res, err
	}
	removed, err := unsetYAMLSettingLocked(lock, dir, settingsPath, data, path)
	res.Removed = removed
	return res, err
}

// legacyFileSets reports whether the legacy-format settings data sets the
// v1 key at path once converted (loadLegacySettingsLayer's conversion).
func legacyFileSets(data []byte, settingsPath string, path []string) (bool, error) {
	fk := koanf.New(".")
	var parser koanf.Parser = yaml.Parser()
	if filepath.Ext(settingsPath) == ".json" {
		parser = koanfjson.Parser()
	}
	if err := fk.Load(rawbytes.Provider(data), parser); err != nil {
		return false, fmt.Errorf("failed to parse %s: %w", settingsPath, err)
	}
	converted, _, err := convertLegacySettingsLayer(fk, nil)
	if err != nil {
		return false, fmt.Errorf("%s: %w", settingsPath, err)
	}
	ck := koanf.New(".")
	if err := ck.Load(confmap.Provider(converted, ""), nil); err != nil {
		return false, err
	}
	return ck.Exists(strings.Join(path, ".")), nil
}

// unsetPrunePath returns the path to delete for removing path from root:
// path itself, or its highest ancestor below the top level that holds
// nothing else, so no empty mapping is left behind.
func unsetPrunePath(root *yamlv3.Node, path []string) []string {
	del := path
	for i := len(path) - 1; i > 0; i-- {
		node := root
		for _, k := range path[:i] {
			_, v := findMapKey(resolveAlias(node), k)
			if v == nil {
				return del
			}
			node = resolveAlias(v)
		}
		if node.Kind != yamlv3.MappingNode || len(node.Content) != 2 {
			return del
		}
		del = path[:i]
	}
	return del
}

// yamlPathExists reports whether root has a key at path.
func yamlPathExists(root *yamlv3.Node, path []string) bool {
	node := root
	for _, k := range path {
		node = resolveAlias(node)
		if node == nil || node.Kind != yamlv3.MappingNode {
			return false
		}
		_, v := findMapKey(node, k)
		if v == nil {
			return false
		}
		node = v
	}
	return true
}

// unsetYAMLSettingLocked removes path from the YAML settings file, in place
// where it can (updateVersionedSettingYAML), else by rewriting the file
// from its data (comments are then lost, as for a struct round trip).
func unsetYAMLSettingLocked(lock *SettingsLock, dir, settingsPath string, data []byte, path []string) (bool, error) {
	doc, err := parseYAMLMappingDocument(data)
	if err != nil {
		return false, fmt.Errorf("failed to parse YAML settings at %s: %w", settingsPath, err)
	}
	root := doc.Content[0]
	if !yamlPathExists(root, path) {
		return false, nil
	}
	edit := versionedSettingEdit{path: unsetPrunePath(root, path)}
	err = updateVersionedSettingYAML(lock, dir, settingsPath, edit)
	if !errors.Is(err, errYAMLEditThroughAlias) {
		return err == nil, err
	}
	// An alias, anchor or key the in-place edit cannot match is on the
	// path: rewrite the file from its data (comments are lost). Read it
	// again first, still under the lock: the edit attempt may already have
	// migrated a legacy key in it.
	if data, err = os.ReadFile(settingsPath); err != nil {
		return false, fmt.Errorf("failed to read %s: %w", settingsPath, err)
	}
	var m map[string]interface{}
	if err := unmarshalSettingsYAML(data, &m); err != nil {
		return false, fmt.Errorf("failed to parse YAML settings at %s: %w", settingsPath, err)
	}
	if !deleteMapPath(m, path) {
		return false, nil
	}
	out, err := yamlv3.Marshal(m)
	if err != nil {
		return false, err
	}
	if err := decodeVersionedSettingsYAML(out); err != nil {
		return false, fmt.Errorf("refusing to write %s: the settings would not load without %s: %w; remove it by editing the file", settingsPath, strings.Join(path, "."), err)
	}
	return true, writeSettingsFileAtomic(lock, settingsPath, out)
}

// unsetJSONSettingLocked removes path from the JSON settings file.
func unsetJSONSettingLocked(lock *SettingsLock, settingsPath string, data []byte, path []string) (bool, error) {
	var m map[string]interface{}
	if err := json.Unmarshal(data, &m); err != nil {
		return false, fmt.Errorf("failed to parse JSON settings at %s: %w", settingsPath, err)
	}
	if !deleteMapPath(m, path) {
		return false, nil
	}
	// Keys come out sorted, as for every struct round trip of a JSON
	// settings file; values are written as given (no HTML escaping of &,
	// < and >).
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(m); err != nil {
		return false, err
	}
	return true, writeSettingsFileAtomic(lock, settingsPath, buf.Bytes())
}

// deleteMapPath deletes path from m, and every parent map left empty by it
// (below the top level). It reports whether the key was there.
func deleteMapPath(m map[string]interface{}, path []string) bool {
	if len(path) == 0 {
		return false
	}
	if len(path) == 1 {
		if _, ok := m[path[0]]; !ok {
			return false
		}
		delete(m, path[0])
		return true
	}
	child, ok := m[path[0]].(map[string]interface{})
	if !ok || !deleteMapPath(child, path[1:]) {
		return false
	}
	if len(child) == 0 {
		delete(m, path[0])
	}
	return true
}
