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

	"github.com/knadh/koanf/parsers/yaml"
	"github.com/knadh/koanf/providers/rawbytes"
	"github.com/knadh/koanf/v2"
	yamlv3 "gopkg.in/yaml.v3"
)

// SettingsPathEdit is one edit for PrepareSettingsPathEdits: set Path to
// Value, or remove Path when Delete is true.
//
// Keep (Delete only) lists paths relative to Path that the delete must
// preserve: the mapping at Path is emptied of everything except the kept
// subtrees, recursively, instead of being removed. Used so a null on an
// ancestor of a hub-owned key (server, server.broker) keeps that key.
type SettingsPathEdit struct {
	Path   []string
	Value  interface{}
	Delete bool
	Keep   [][]string
}

// ErrSettingsPathEditUnsupported is returned when an edit cannot be made in
// place: the file is JSON, or a node on an edited path is an alias, carries
// an anchor, or is a mapping with a key the edit cannot match by name. The
// caller should ask for the file to be edited by hand rather than rewrite it
// from a struct and lose comments and unknown keys.
var ErrSettingsPathEditUnsupported = errors.New("settings file cannot be edited in place")

// StagedSettingsEdit is a prepared settings-file update. Changed lists the
// dotted paths of the edits that are written; when it is empty Commit
// writes nothing.
type StagedSettingsEdit struct {
	Changed []string
	target  string
	orig    []byte
	data    []byte
}

// Commit writes the staged file atomically (see writeSettingsFileAtomic).
func (s *StagedSettingsEdit) Commit() error {
	if s == nil || len(s.Changed) == 0 {
		return nil
	}
	return writeSettingsFileAtomic(s.target, s.data)
}

// Original returns the settings file bytes the edit was prepared against.
func (s *StagedSettingsEdit) Original() []byte { return s.orig }

// Result returns the bytes Commit writes (the original bytes when nothing
// changes).
func (s *StagedSettingsEdit) Result() []byte {
	if len(s.Changed) == 0 {
		return s.orig
	}
	return s.data
}

// readSettingsForEdit returns the YAML settings file in dir (nil when there
// is none) and the path a write targets.
func readSettingsForEdit(dir string) (orig []byte, target string, err error) {
	settingsPath := GetSettingsPath(dir)
	switch {
	case settingsPath == "":
		return nil, newSettingsFilePath(dir), nil
	case filepath.Ext(settingsPath) == ".json":
		return nil, "", fmt.Errorf("%w: %s is JSON", ErrSettingsPathEditUnsupported, settingsPath)
	}
	orig, err = os.ReadFile(settingsPath)
	if err != nil {
		return nil, "", fmt.Errorf("failed to read %s: %w", settingsPath, err)
	}
	return orig, settingsPath, nil
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
	orig, target, err := readSettingsForEdit(dir)
	if err != nil {
		return nil, err
	}
	return prepareSettingsEditsBytes(orig, target, edits)
}

// PrepareEffectiveSettingsPathEdits is PrepareSettingsPathEdits that keeps
// only the edits that change the effective settings (see
// SettingsFileEffective): an edit whose removal leaves the effective
// settings of the result unchanged is dropped, so it is not written and not
// reported in Changed. When no edit changes anything effective, nothing is
// written. The caller must hold LockSettingsFile from this call to Commit.
func PrepareEffectiveSettingsPathEdits(dir string, edits []SettingsPathEdit) (*StagedSettingsEdit, error) {
	orig, target, err := readSettingsForEdit(dir)
	if err != nil {
		return nil, err
	}
	before, err := SettingsFileEffective(orig)
	if err != nil {
		return nil, err
	}
	staged, err := prepareSettingsEditsBytes(orig, target, edits)
	if err != nil {
		return nil, err
	}
	after, err := SettingsFileEffective(staged.Result())
	if err != nil {
		return nil, err
	}
	if before.Equal(after) {
		staged.Changed = nil
		return staged, nil
	}
	// Greedily drop edits that contribute nothing given the others.
	kept := append([]SettingsPathEdit{}, edits...)
	for i := 0; i < len(kept); {
		trial := append(append([]SettingsPathEdit{}, kept[:i]...), kept[i+1:]...)
		ts, err := prepareSettingsEditsBytes(orig, target, trial)
		if err != nil {
			return nil, err
		}
		te, err := SettingsFileEffective(ts.Result())
		if err != nil {
			return nil, err
		}
		if te.Equal(after) {
			kept = trial
			continue
		}
		i++
	}
	return prepareSettingsEditsBytes(orig, target, kept)
}

// prepareSettingsEditsBytes is PrepareSettingsPathEdits on the file bytes.
func prepareSettingsEditsBytes(orig []byte, target string, edits []SettingsPathEdit) (*StagedSettingsEdit, error) {
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

	staged := &StagedSettingsEdit{target: target, orig: orig}
	for _, e := range edits {
		cur, found := yamlValueAtPath(root, e.Path)
		if e.Delete {
			if !found {
				continue
			}
			changed, err := deleteYAMLPathKeeping(root, e.Path, e.Keep)
			if err != nil {
				return nil, fmt.Errorf("failed to update %s: %w", target, err)
			}
			if changed {
				staged.Changed = append(staged.Changed, strings.Join(e.Path, "."))
			}
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

// deleteYAMLPathKeeping deletes path, or, when keep names subtrees under it
// that exist, deletes everything at path except them (recursively).
func deleteYAMLPathKeeping(root *yamlv3.Node, path []string, keep [][]string) (bool, error) {
	if len(keep) == 0 {
		return deleteYAMLPath(root, path)
	}
	node := root
	for _, k := range path {
		_, v := findMapKey(resolveAlias(node), k)
		if v == nil {
			return false, nil
		}
		node = resolveAlias(v)
	}
	if node.Kind != yamlv3.MappingNode {
		return deleteYAMLPath(root, path)
	}
	// Split the keep paths by their first element.
	byChild := map[string][][]string{}
	for _, kp := range keep {
		if len(kp) == 0 {
			return false, nil // keep the whole subtree
		}
		byChild[kp[0]] = append(byChild[kp[0]], kp[1:])
	}
	changed := false
	var names []string
	for i := 0; i+1 < len(node.Content); i += 2 {
		names = append(names, node.Content[i].Value)
	}
	for _, name := range names {
		sub, ok := byChild[name]
		childPath := append(append([]string{}, path...), name)
		if !ok {
			c, err := deleteYAMLPath(root, childPath)
			if err != nil {
				return false, err
			}
			changed = changed || c
			continue
		}
		keepWhole := false
		for _, s := range sub {
			if len(s) == 0 {
				keepWhole = true
			}
		}
		if keepWhole {
			continue
		}
		c, err := deleteYAMLPathKeeping(root, childPath, sub)
		if err != nil {
			return false, err
		}
		changed = changed || c
	}
	return changed, nil
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

// SettingsEffective is what the server derives from a global settings file:
//
//   - Server: the hub's server config, built as the hub builds it from a
//     settings.yaml that has a server key (typed decode of the server block,
//     ConvertV1ServerToGlobalConfig, then the top-level hub sections). When
//     settings.yaml has no server key the hub falls back to the deprecated
//     server.yaml (loadGlobalConfigLegacy), which this does not model; the
//     workstation server-config PUT refuses to create a server block in that
//     case instead (409 legacy_server_yaml);
//   - Typed: the typed VersionedSettings decode, normalised so an absent
//     key, a null and the zero value compare equal (nil pointers to structs
//     and nil slices/maps become their zero/empty values). It covers fields
//     the converter does not map; where the converter tells absent from
//     empty (a missing cors block, a nil list), Server tells them apart;
//   - ActiveProfile / WorkspacePath: these two load through koanf on top of
//     the embedded defaults (LoadEffectiveSettings), where a present "" does
//     override the default, so they are taken from that merge.
type SettingsEffective struct {
	Server        *GlobalConfig
	Typed         VersionedSettings
	ActiveProfile string
	WorkspacePath string
}

// SettingsFileEffective computes SettingsEffective for settings file bytes.
func SettingsFileEffective(data []byte) (*SettingsEffective, error) {
	var raw map[string]interface{}
	if err := yamlv3.Unmarshal(data, &raw); err != nil {
		return nil, err
	}
	var v1Server V1ServerConfig
	if srv, ok := raw["server"]; ok && srv != nil {
		b, err := yamlv3.Marshal(srv)
		if err != nil {
			return nil, err
		}
		if err := yamlv3.Unmarshal(b, &v1Server); err != nil {
			return nil, err
		}
	}
	gc := ConvertV1ServerToGlobalConfig(&v1Server)
	applyTopLevelSettingsSections(gc, raw)

	var typed VersionedSettings
	if err := yamlv3.Unmarshal(data, &typed); err != nil {
		return nil, err
	}
	normalizeZero(reflect.ValueOf(&typed).Elem())

	k := koanf.New(".")
	if defaults, err := GetDefaultSettingsDataYAML(); err == nil {
		_ = k.Load(rawbytes.Provider(defaults), yaml.Parser())
	}
	if len(bytes.TrimSpace(data)) > 0 {
		if err := k.Load(rawbytes.Provider(data), yaml.Parser()); err != nil {
			return nil, err
		}
	}
	return &SettingsEffective{
		Server:        gc,
		Typed:         typed,
		ActiveProfile: k.String("active_profile"),
		WorkspacePath: k.String("workspace_path"),
	}, nil
}

// Equal reports whether two effective settings are the same.
func (e *SettingsEffective) Equal(o *SettingsEffective) bool {
	return reflect.DeepEqual(e, o)
}

// normalizeZero replaces nil pointers to structs with pointers to zero
// structs and nil slices/maps with empty ones, recursively, so absent and
// zero compare equal.
func normalizeZero(v reflect.Value) {
	switch v.Kind() {
	case reflect.Pointer:
		if v.IsNil() {
			if v.Type().Elem().Kind() == reflect.Struct && v.CanSet() {
				v.Set(reflect.New(v.Type().Elem()))
			} else {
				return
			}
		}
		normalizeZero(v.Elem())
	case reflect.Struct:
		for i := 0; i < v.NumField(); i++ {
			if v.Type().Field(i).IsExported() {
				normalizeZero(v.Field(i))
			}
		}
	case reflect.Slice:
		if v.IsNil() {
			if v.CanSet() {
				v.Set(reflect.MakeSlice(v.Type(), 0, 0))
			}
			return
		}
		for i := 0; i < v.Len(); i++ {
			normalizeZero(v.Index(i))
		}
	case reflect.Map:
		if v.IsNil() {
			if v.CanSet() {
				v.Set(reflect.MakeMap(v.Type()))
			}
			return
		}
		for _, k := range v.MapKeys() {
			e := reflect.New(v.Type().Elem()).Elem()
			e.Set(v.MapIndex(k))
			normalizeZero(e)
			v.SetMapIndex(k, e)
		}
	}
}
