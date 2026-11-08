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
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"

	yamlv3 "gopkg.in/yaml.v3"

	"github.com/GoogleCloudPlatform/scion/pkg/util/privatefs"
)

// instancesPath is the one settings key the instances writers change.
var instancesPath = []string{"server", "broker", "instances"}

// ErrInstancesSettingsJSON refuses a JSON global settings file: the
// instances writers edit YAML only.
var ErrInstancesSettingsJSON = errors.New("the global settings file is settings.json; server.broker.instances is written to a YAML settings file only (settings.yaml): convert the file to YAML first")

// ErrInstancesSettingsNotYAML refuses a settings file named other than
// settings.yaml (settings.yml): the server reads the instances from
// settings.yaml only.
var ErrInstancesSettingsNotYAML = errors.New("the global settings file is not named settings.yaml; the server reads server.broker.instances from settings.yaml only: rename the file first")

// ErrInstancesSettingsMissing refuses a global directory with no settings
// file.
var ErrInstancesSettingsMissing = errors.New("there is no global settings file to write server.broker.instances to")

// ErrInstancesSettingsNotV1 refuses a settings file that is not
// schema_version 1.
var ErrInstancesSettingsNotV1 = errors.New("the global settings file is not schema_version 1; migrate it before writing server.broker.instances")

// InstancesMismatchError is returned when the current
// server.broker.instances is not what the writer expects. It names the
// instance keys only, never a value.
type InstancesMismatchError struct {
	// Keys are the instance keys present on either side whose entries
	// differ (sorted).
	Keys []string
}

func (e *InstancesMismatchError) Error() string {
	if len(e.Keys) == 0 {
		return "server.broker.instances in the global settings is not what was expected"
	}
	return "server.broker.instances in the global settings is not what was expected; the entries that differ are for instance keys: " + strings.Join(e.Keys, ", ")
}

// WriteRuntimeBrokerInstances sets server.broker.instances in the global
// settings file in dir to instances, and changes nothing else in the file.
// It holds the settings lock from its read to its write. It refuses unless
// the current list is empty, or equal to instances (then it writes
// nothing). The result is validated with ValidateRuntimeBrokerInstances
// and the strict instances loader before it is written.
//
// The write never follows a link: a symlink, a non-regular file or a file
// with more than one name at the settings path is refused. The new file is
// written through an owner-only temporary file in the same directory,
// synced, given the original file's mode and renamed over it, and the
// directory is synced. Only a YAML settings file of schema_version 1 is
// written; a settings.json file is refused (ErrInstancesSettingsJSON).
//
// It does not consult internal/writesonread: that switch stops read paths
// from rewriting settings, and this is an explicit, requested write
// (design erratum E1 to the convert-apply design, section 3).
func WriteRuntimeBrokerInstances(dir string, instances []V1RuntimeBrokerInstanceConfig) error {
	if len(instances) == 0 {
		return errors.New("WriteRuntimeBrokerInstances: no instances to write")
	}
	if errs := ValidateRuntimeBrokerInstances(instances); len(errs) > 0 {
		return joinValidationErrors(errs)
	}
	return editRuntimeBrokerInstances(dir, func(current []V1RuntimeBrokerInstanceConfig) (bool, error) {
		switch {
		case len(current) == 0:
			return true, nil
		case InstancesEqual(current, instances):
			return false, nil
		default:
			return false, &InstancesMismatchError{Keys: differingKeys(current, instances)}
		}
	}, instances)
}

// RemoveRuntimeBrokerInstances is the inverse of
// WriteRuntimeBrokerInstances: it removes server.broker.instances from the
// global settings file in dir, and changes nothing else. It refuses, with
// an *InstancesMismatchError naming the differing instance keys, unless
// the current list equals expected. An absent key is a no-op. It writes
// as WriteRuntimeBrokerInstances does.
func RemoveRuntimeBrokerInstances(dir string, expected []V1RuntimeBrokerInstanceConfig) error {
	return editRuntimeBrokerInstances(dir, func(current []V1RuntimeBrokerInstanceConfig) (bool, error) {
		switch {
		case len(current) == 0:
			return false, nil
		case InstancesEqual(current, expected):
			return true, nil
		default:
			return false, &InstancesMismatchError{Keys: differingKeys(current, expected)}
		}
	}, nil)
}

// editRuntimeBrokerInstances runs one locked read-modify-write of
// server.broker.instances. decide sees the current list and says whether
// to write; set is the new list (nil removes the key).
func editRuntimeBrokerInstances(dir string, decide func([]V1RuntimeBrokerInstanceConfig) (bool, error), set []V1RuntimeBrokerInstanceConfig) error {
	lock, err := LockSettingsDir(dir)
	if err != nil {
		return err
	}
	defer lock.Unlock()

	// The server reads server.broker.instances from settings.yaml only
	// (LoadRuntimeBrokerInstances), so that is the only file written.
	path := GetSettingsPath(dir)
	switch {
	case path == "":
		return ErrInstancesSettingsMissing
	case strings.HasSuffix(path, ".json"):
		return ErrInstancesSettingsJSON
	case filepath.Base(path) != "settings.yaml":
		return ErrInstancesSettingsNotYAML
	}
	if err := lock.checkFor(path); err != nil {
		return err
	}
	tree, err := privatefs.OpenTree(dir, privatefs.TreeOptions{})
	if err != nil {
		return err
	}
	name := filepath.Base(path)
	mode, before, err := checkInstancesWriteTarget(path)
	if err != nil {
		return err
	}
	orig, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("reading %s: %w", path, err)
	}
	// The file read must be the one checked: a link or another file
	// swapped in between is refused.
	if _, after, err := checkInstancesWriteTarget(path); err != nil || !os.SameFile(before, after) {
		return fmt.Errorf("refusing to write %s: it was replaced while it was being read", path)
	}

	current, err := strictInstancesFromYAML(orig)
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	write, err := decide(current)
	if err != nil || !write {
		return err
	}

	out, err := editInstancesYAML(orig, set)
	if err != nil {
		return fmt.Errorf("refusing to write %s: %w", path, err)
	}
	got, err := strictInstancesFromYAML(out)
	if err != nil {
		return fmt.Errorf("refusing to write %s: the result does not load: %w", path, err)
	}
	if !InstancesEqual(got, set) {
		return fmt.Errorf("refusing to write %s: the result does not read back as the intended server.broker.instances", path)
	}
	if bytes.Equal(out, orig) {
		return nil
	}
	tmpRel, err := writeSettingsTempKeepingMode(tree, name, out, mode)
	if err != nil {
		return err
	}
	// The one settings-file write (listed in settingsBypassAllowed): a
	// rename within the directory, which replaces whatever is at path
	// itself and never writes through a link.
	if err := os.Rename(filepath.Join(dir, tmpRel), path); err != nil {
		_ = tree.Remove(tmpRel)
		return err
	}
	if err := syncDir(dir); err != nil {
		return fmt.Errorf("%w: %w", errAtomicNotDurable, err)
	}
	return nil
}

// checkInstancesWriteTarget refuses a settings path that is a symlink, not
// a regular file or has more than one name, and returns its mode and
// file info.
func checkInstancesWriteTarget(path string) (fs.FileMode, fs.FileInfo, error) {
	fi, err := os.Lstat(path)
	if err != nil {
		return 0, nil, err
	}
	if fi.Mode()&fs.ModeSymlink != 0 {
		return 0, nil, fmt.Errorf("refusing to write %s: it is a symbolic link", path)
	}
	if !fi.Mode().IsRegular() {
		return 0, nil, fmt.Errorf("refusing to write %s: it is not a regular file", path)
	}
	// The one-name rule (more than one name is refused); the leaf is not
	// a link, so nothing is resolved here.
	if err := privatefs.CheckReplaceTarget(path); err != nil {
		return 0, nil, fmt.Errorf("refusing to write %s: %w", path, err)
	}
	return fi.Mode().Perm(), fi, nil
}

// writeSettingsTempKeepingMode writes data to a new owner-only temporary
// file next to name in tree, syncs it, gives it mode and returns its name.
// The file stays 0600 until it is complete. On error nothing is left.
func writeSettingsTempKeepingMode(tree *privatefs.Tree, name string, data []byte, mode fs.FileMode) (string, error) {
	f, tmpRel, err := tree.CreateTemp("", "."+name+".tmp-*", privatefs.Private)
	if err != nil {
		return "", err
	}
	ok := false
	defer func() {
		_ = f.Close()
		if !ok {
			_ = tree.Remove(tmpRel)
		}
	}()
	if _, err := f.Write(data); err != nil {
		return "", err
	}
	if err := f.Sync(); err != nil {
		return "", err
	}
	if err := f.Chmod(mode); err != nil {
		return "", err
	}
	if err := f.Close(); err != nil {
		return "", err
	}
	ok = true
	return tmpRel, nil
}

// editInstancesYAML returns orig with server.broker.instances set to set
// (removed when set is nil) and every other byte kept where the splice
// can keep it. The file must be schema_version 1.
func editInstancesYAML(orig []byte, set []V1RuntimeBrokerInstanceConfig) ([]byte, error) {
	doc, err := parseYAMLMappingDocument(orig)
	if err != nil {
		return nil, fmt.Errorf("the settings are not valid YAML: %w", err)
	}
	var vs VersionedSettings
	if err := decodeSettingsYAMLNode(doc, &vs); err != nil {
		return nil, fmt.Errorf("the settings do not load: %w", err)
	}
	root := doc.Content[0]
	if _, sv := findMapKey(root, "schema_version"); sv == nil || sv.Kind != yamlv3.ScalarNode || sv.Value != "1" {
		return nil, ErrInstancesSettingsNotV1
	}
	indent := detectYAMLIndent(root)

	edit := versionedSettingEdit{path: instancesPath}
	if set != nil {
		var node yamlv3.Node
		if err := node.Encode(set); err != nil {
			return nil, err
		}
		edit.value = &node
	}
	if edit.value == nil {
		_, err = deleteYAMLPath(root, edit.path)
	} else {
		_, err = setYAMLPath(root, edit.path, edit.value)
	}
	if err != nil {
		return nil, err
	}
	var want interface{}
	if err := doc.Decode(&want); err != nil {
		return nil, err
	}
	out, err := encodeSettingsYAML(doc, indent)
	if err != nil {
		return nil, err
	}
	if spliced, ok := spliceVersionedSettingEdit(orig, edit, indent); ok && yamlDecodesTo(spliced, want) && decodeVersionedSettingsYAML(spliced) == nil {
		out = spliced
	}
	if !yamlDecodesTo(out, want) {
		return nil, errors.New("the edited settings do not round-trip")
	}
	if err := decodeVersionedSettingsYAML(out); err != nil {
		return nil, fmt.Errorf("the edited settings would not load: %w", err)
	}
	return out, nil
}

// strictInstancesFromYAML strictly loads server.broker.instances from the
// settings file content data, as LoadRuntimeBrokerInstances does from the
// file.
func strictInstancesFromYAML(data []byte) ([]V1RuntimeBrokerInstanceConfig, error) {
	raw, found, err := parseServerSectionStrict(data, "settings.yaml")
	if err != nil || !found {
		return nil, err
	}
	return instancesFromServerSection(raw)
}

// InstancesEqual reports whether a and b are the same instances list.
func InstancesEqual(a, b []V1RuntimeBrokerInstanceConfig) bool {
	if len(a) == 0 && len(b) == 0 {
		return true
	}
	return reflect.DeepEqual(a, b)
}

// differingKeys lists the instance keys whose entries differ between a
// and b, or that only one side has.
func differingKeys(a, b []V1RuntimeBrokerInstanceConfig) []string {
	am := map[string]V1RuntimeBrokerInstanceConfig{}
	for _, inst := range a {
		am[inst.Key] = inst
	}
	bm := map[string]V1RuntimeBrokerInstanceConfig{}
	for _, inst := range b {
		bm[inst.Key] = inst
	}
	seen := map[string]bool{}
	var keys []string
	for k, av := range am {
		if bv, ok := bm[k]; !ok || !reflect.DeepEqual(av, bv) {
			keys = append(keys, k)
		}
		seen[k] = true
	}
	for k := range bm {
		if !seen[k] {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	return keys
}
