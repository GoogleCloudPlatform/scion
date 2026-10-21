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
	"os"

	yamlv3 "gopkg.in/yaml.v3"
)

// Helpers for RuntimeBuildRecord.EntryName (Kubernetes placement defaults,
// ptone/scion#5085). A runtime records the global runtimes key it was built
// from only when that key exists in the overlay-inclusive global view and,
// unless the settings overlay supplies the runtimes section, the project's
// own settings do not define the same key: a project-local same-name entry
// records "". When the overlay supplies runtimes it replaces the section in
// both the global and the project-merged views, so project shadowing cannot
// occur and the project check does not apply. Both helpers only read.

// SuppliesRuntimes reports whether the overlay currently replaces the
// runtimes section of loaded settings: it is active and holds at least one
// stored runtime (the condition Apply replaces vs.Runtimes under). With an
// active overlay but no stored runtimes the settings files' runtimes stay.
func (o *SettingsOverlay) SuppliesRuntimes() bool {
	if o == nil {
		return false
	}
	o.mu.RLock()
	defer o.mu.RUnlock()
	return o.active && len(o.runtimes) > 0
}

// ProjectSettingsDefinesRuntime reports whether a project settings layer
// that LoadEffectiveSettings(projectDir) merges over the global file
// defines runtimes.<name>. projectDir is the resolved project directory
// passed to LoadEffectiveSettings (it may be "", in which case the
// cwd-resolved project is checked, as the loader does). The layers are the
// same as the loader's: projectDir's own settings file, then the
// effective (split-storage) project directory when it differs; the global
// directory is never a project layer.
//
// It fails closed: a settings file that exists but cannot be read or
// parsed counts as defining the key, so the caller records no entry name.
// A missing file defines nothing.
func ProjectSettingsDefinesRuntime(projectDir, name string) bool {
	if name == "" {
		return false
	}
	globalDir, _ := GetGlobalDir()
	effective := resolveEffectiveProjectPath(projectDir)
	var dirs []string
	if projectDir != "" && projectDir != globalDir {
		dirs = append(dirs, projectDir)
	}
	if effective != "" && effective != globalDir && effective != projectDir {
		dirs = append(dirs, effective)
	}
	for _, dir := range dirs {
		if settingsFileDefinesRuntime(GetSettingsPath(dir), name) {
			return true
		}
	}
	return false
}

// settingsFileDefinesRuntime reports whether the settings file at path has
// a runtimes.<name> key. An empty path (no file) defines nothing; a read or
// parse error counts as defining it (fail closed).
func settingsFileDefinesRuntime(path, name string) bool {
	if path == "" {
		return false
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		// Even if not-exist: the file existed at stat time, so the runtime may have been built from it.
		return true
	}
	var probe struct {
		Runtimes map[string]any `yaml:"runtimes"`
	}
	if err := yamlv3.Unmarshal(raw, &probe); err != nil {
		return true
	}
	_, ok := probe.Runtimes[name]
	return ok
}
