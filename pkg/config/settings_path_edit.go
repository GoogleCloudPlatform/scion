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
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"

	yamlv3 "gopkg.in/yaml.v3"
)

// SettingsPathEdit is one edit for PrepareSettingsPathEdits: set Path to
// Value, or remove Path when Delete is true.
type SettingsPathEdit struct {
	Path   []string
	Value  interface{}
	Delete bool
}

// ErrSettingsPathEditUnsupported is returned when an edit cannot be made in
// place: the file is JSON, or a node on an edited path is an alias, carries
// an anchor, or is a mapping with a key the edit cannot match by name. The
// caller should ask for the file to be edited by hand rather than rewrite it
// from a struct and lose comments and unknown keys.
var ErrSettingsPathEditUnsupported = errors.New("settings file cannot be edited in place")

// StagedSettingsEdit is a prepared settings-file update. Changed lists the
// dotted paths whose value changes; when it is empty Commit writes nothing.
type StagedSettingsEdit struct {
	Changed []string
	target  string
	data    []byte
}

// Commit writes the staged file atomically (see writeSettingsFileAtomic).
func (s *StagedSettingsEdit) Commit() error {
	if s == nil || len(s.Changed) == 0 {
		return nil
	}
	return writeSettingsFileAtomic(s.target, s.data)
}

// PrepareSettingsPathEdits applies edits to the YAML settings file in dir in
// memory, editing nodes in place so comments, key order and unknown keys
// survive, and returns the result for Commit. Edits whose value is already
// in the file (or deletes of absent keys) are skipped and not reported in
// Changed. The caller must hold LockSettingsFile from this call to Commit.
//
// It keeps the guards UpdateVersionedSetting relies on: a file the struct
// loader rejects is refused, an edit through an alias/anchor/opaque key is
// refused before anything changes, and the output must decode to the edited
// tree and load as VersionedSettings. Several edits cannot use the
// byte-level splice, so the document is re-encoded: blank lines are not
// kept and only the first YAML document survives.
func PrepareSettingsPathEdits(dir string, edits []SettingsPathEdit) (*StagedSettingsEdit, error) {
	settingsPath := GetSettingsPath(dir)
	var orig []byte
	target := settingsPath
	switch {
	case settingsPath == "":
		target = newSettingsFilePath(dir)
	case filepath.Ext(settingsPath) == ".json":
		return nil, fmt.Errorf("%w: %s is JSON", ErrSettingsPathEditUnsupported, settingsPath)
	default:
		var err error
		if orig, err = os.ReadFile(settingsPath); err != nil {
			return nil, fmt.Errorf("failed to read %s: %w", settingsPath, err)
		}
	}

	doc, err := parseYAMLMappingDocument(orig)
	if err != nil {
		return nil, fmt.Errorf("failed to parse YAML settings at %s: %w", target, err)
	}
	var vs VersionedSettings
	if err := doc.Decode(&vs); err != nil {
		return nil, fmt.Errorf("failed to parse YAML settings at %s: %w", target, err)
	}
	root := doc.Content[0]
	indent := detectYAMLIndent(root)

	// All-or-nothing: refuse before the first edit if any path is shared.
	for _, e := range edits {
		if err := checkYAMLPathUnshared(root, e.Path); err != nil {
			return nil, fmt.Errorf("%w: %s: %v", ErrSettingsPathEditUnsupported, strings.Join(e.Path, "."), err)
		}
	}

	staged := &StagedSettingsEdit{target: target}
	for _, e := range edits {
		cur, found := yamlValueAtPath(root, e.Path)
		if e.Delete {
			if !found {
				continue
			}
			if _, err := deleteYAMLPath(root, e.Path); err != nil {
				return nil, fmt.Errorf("failed to update %s: %w", target, err)
			}
			staged.Changed = append(staged.Changed, strings.Join(e.Path, "."))
			continue
		}
		var n yamlv3.Node
		if err := n.Encode(e.Value); err != nil {
			return nil, fmt.Errorf("failed to encode %s: %w", strings.Join(e.Path, "."), err)
		}
		if found {
			var want interface{}
			if err := n.Decode(&want); err == nil && reflect.DeepEqual(cur, want) {
				continue
			}
		}
		if _, err := setYAMLPath(root, e.Path, &n); err != nil {
			return nil, fmt.Errorf("failed to update %s: %w", target, err)
		}
		staged.Changed = append(staged.Changed, strings.Join(e.Path, "."))
	}
	if len(staged.Changed) == 0 {
		return staged, nil
	}

	if _, sv := findMapKey(root, "schema_version"); sv == nil || isYAMLNull(sv) || (sv.Kind == yamlv3.ScalarNode && sv.Value == "") {
		if sv != nil {
			deleteMapKey(root, "schema_version")
		}
		svKey := newYAMLStringScalar("schema_version")
		if len(root.Content) > 0 {
			svKey.HeadComment, root.Content[0].HeadComment = root.Content[0].HeadComment, ""
		}
		root.Content = append([]*yamlv3.Node{svKey, newYAMLStringScalar("1")}, root.Content...)
	}

	var want interface{}
	if err := doc.Decode(&want); err != nil {
		return nil, fmt.Errorf("failed to decode edited settings: %w", err)
	}
	out, err := encodeSettingsYAML(doc, indent)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal settings: %w", err)
	}
	if !yamlDecodesTo(out, want) {
		return nil, fmt.Errorf("refusing to write %s: the re-encoded settings do not round-trip", target)
	}
	if err := decodeVersionedSettingsYAML(out); err != nil {
		return nil, fmt.Errorf("refusing to write %s: the updated settings would not load: %w", target, err)
	}
	if bytes.Equal(out, orig) {
		staged.Changed = nil
		return staged, nil
	}
	staged.data = out
	return staged, nil
}

// yamlValueAtPath decodes the value at path within the mapping root.
func yamlValueAtPath(root *yamlv3.Node, path []string) (interface{}, bool) {
	m := root
	for _, k := range path {
		m = resolveAlias(m)
		if m == nil || m.Kind != yamlv3.MappingNode {
			return nil, false
		}
		_, v := findMapKey(m, k)
		if v == nil {
			return nil, false
		}
		m = v
	}
	var out interface{}
	if err := m.Decode(&out); err != nil {
		return nil, false
	}
	return out, true
}
