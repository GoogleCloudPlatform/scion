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
	"path/filepath"
	"testing"
)

// The EntryName shadowing rule: unless the overlay supplies the runtimes
// section, a project-local runtimes.<name> makes the build record's
// EntryName "" (ProjectSettingsDefinesRuntime); the clause is skipped only
// while the overlay holds at least one stored runtime (SuppliesRuntimes).

func TestSettingsOverlaySuppliesRuntimes(t *testing.T) {
	var nilOverlay *SettingsOverlay
	if nilOverlay.SuppliesRuntimes() {
		t.Error("nil overlay supplies runtimes")
	}
	o := NewSettingsOverlay()
	if o.SuppliesRuntimes() {
		t.Error("inactive overlay supplies runtimes")
	}
	o.Update(nil, map[string]V1ProfileConfig{"p": {Runtime: "k8s"}}, nil)
	if o.SuppliesRuntimes() {
		t.Error("active overlay with no stored runtimes supplies runtimes")
	}
	o.Update(map[string]V1RuntimeConfig{"k8s": {Type: "kubernetes"}}, nil, nil)
	if !o.SuppliesRuntimes() {
		t.Error("overlay with one stored runtime does not supply runtimes")
	}
	o.Update(map[string]V1RuntimeConfig{}, nil, nil)
	if o.SuppliesRuntimes() {
		t.Error("overlay whose stored runtimes were cleared still supplies runtimes")
	}
}

func TestProjectSettingsDefinesRuntime(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	globalDir := filepath.Join(home, ".scion")
	if err := os.MkdirAll(globalDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(globalDir, "settings.yaml"), []byte("schema_version: \"1\"\nruntimes:\n  gke:\n    type: kubernetes\n"), 0644); err != nil {
		t.Fatal(err)
	}

	projectDir := func(t *testing.T, name, content string) string {
		t.Helper()
		dir := filepath.Join(t.TempDir(), ".scion")
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatal(err)
		}
		if name != "" {
			if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0644); err != nil {
				t.Fatal(err)
			}
		}
		return dir
	}

	t.Run("project file defines the same name", func(t *testing.T) {
		dir := projectDir(t, "settings.yaml", "schema_version: \"1\"\nruntimes:\n  gke:\n    namespace: other\n")
		if !ProjectSettingsDefinesRuntime(dir, "gke") {
			t.Error("want true")
		}
	})
	t.Run("project JSON file defines the same name", func(t *testing.T) {
		dir := projectDir(t, "settings.json", `{"schema_version":"1","runtimes":{"gke":{"namespace":"other"}}}`)
		if !ProjectSettingsDefinesRuntime(dir, "gke") {
			t.Error("want true")
		}
	})
	t.Run("project file defines only other names", func(t *testing.T) {
		dir := projectDir(t, "settings.yaml", "schema_version: \"1\"\nruntimes:\n  docker:\n    type: docker\n")
		if ProjectSettingsDefinesRuntime(dir, "gke") {
			t.Error("want false")
		}
	})
	t.Run("no project file", func(t *testing.T) {
		if ProjectSettingsDefinesRuntime(projectDir(t, "", ""), "gke") {
			t.Error("want false")
		}
	})
	t.Run("the global directory is not a project layer", func(t *testing.T) {
		if ProjectSettingsDefinesRuntime(globalDir, "gke") {
			t.Error("want false: the global file defines gke, but it is not a project layer")
		}
	})
	t.Run("empty name", func(t *testing.T) {
		dir := projectDir(t, "settings.yaml", "runtimes:\n  \"\": {}\n")
		if ProjectSettingsDefinesRuntime(dir, "") {
			t.Error("want false")
		}
	})
	t.Run("unparsable project file fails closed", func(t *testing.T) {
		dir := projectDir(t, "settings.yaml", "runtimes: [unclosed\n")
		if !ProjectSettingsDefinesRuntime(dir, "gke") {
			t.Error("want true (fail closed)")
		}
	})
	t.Run("project file with a non-map runtimes section fails closed", func(t *testing.T) {
		dir := projectDir(t, "settings.yaml", "runtimes: just-a-string\n")
		if !ProjectSettingsDefinesRuntime(dir, "gke") {
			t.Error("want true (fail closed)")
		}
	})
	t.Run("unreadable project file fails closed", func(t *testing.T) {
		// A directory named settings.yaml exists but cannot be read as a
		// file, also when the tests run as root.
		dir := projectDir(t, "", "")
		if err := os.Mkdir(filepath.Join(dir, "settings.yaml"), 0755); err != nil {
			t.Fatal(err)
		}
		if !ProjectSettingsDefinesRuntime(dir, "gke") {
			t.Error("want true (fail closed)")
		}
	})
}
