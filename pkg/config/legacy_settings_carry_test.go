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
	"reflect"
	"strings"
	"testing"

	yamlv3 "gopkg.in/yaml.v3"
)

// Tests for ptone/scion#3497: v1-only top-level keys in an unversioned
// settings file must survive every settings write path.

// legacyWithV1KeysYAML is an unversioned settings file (no schema_version,
// no harnesses) holding v1-only top-level keys. server.broker.instances is
// the flat Runtime Broker config; it is checked as raw YAML so the test does
// not depend on the struct that models it.
const legacyWithV1KeysYAML = `active_profile: local
image_registry: ghcr.io/example/scion
server:
  broker:
    port: 19800
    instances:
      - key: local-docker
        name: local-docker
        runtime_target:
          type: docker
hub_connections:
  hub-prod:
    endpoint: https://hub.prod.example.com
  hub-staging:
    endpoint: https://hub.staging.example.com
`

const legacyWithV1KeysJSON = `{
  "active_profile": "local",
  "image_registry": "ghcr.io/example/scion",
  "server": {
    "broker": {
      "port": 19800,
      "instances": [
        {"key": "local-docker", "name": "local-docker", "runtime_target": {"type": "docker"}}
      ]
    }
  }
}
`

// versionedWithV1KeysYAML is the same data in a versioned (schema_version
// "1") file, with a comment that an in-place edit keeps.
const versionedWithV1KeysYAML = `schema_version: "1"
# broker settings
active_profile: local
image_registry: ghcr.io/example/scion
server:
  broker:
    port: 19800
    instances:
      - key: local-docker
        name: local-docker
        runtime_target:
          type: docker
`

func carryTestDir(t *testing.T, name, content string) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, ".scion")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func readSettingsMap(t *testing.T, path string) map[string]interface{} {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]interface{}
	if err := yamlv3.Unmarshal(data, &m); err != nil {
		t.Fatalf("parse %s: %v\n%s", path, err, data)
	}
	return m
}

func lookupPath(m map[string]interface{}, path ...string) (interface{}, bool) {
	var cur interface{} = m
	for _, p := range path {
		mm, ok := cur.(map[string]interface{})
		if !ok {
			return nil, false
		}
		if cur, ok = mm[p]; !ok {
			return nil, false
		}
	}
	return cur, true
}

// wantInstances is server.broker.instances as decoded from the fixtures.
func wantInstances(t *testing.T) interface{} {
	t.Helper()
	var m map[string]interface{}
	if err := yamlv3.Unmarshal([]byte(legacyWithV1KeysYAML), &m); err != nil {
		t.Fatal(err)
	}
	v, _ := lookupPath(m, "server", "broker", "instances")
	return v
}

// assertV1KeysKept checks the v1-only keys of the fixtures survived in m.
func assertV1KeysKept(t *testing.T, m map[string]interface{}) {
	t.Helper()
	if port, _ := lookupPath(m, "server", "broker", "port"); port != 19800 {
		t.Errorf("server.broker.port = %v (%T), want 19800", port, port)
	}
	if reg, _ := lookupPath(m, "image_registry"); reg != "ghcr.io/example/scion" {
		t.Errorf("image_registry = %v, want ghcr.io/example/scion", reg)
	}
	got, _ := lookupPath(m, "server", "broker", "instances")
	if want := wantInstances(t); !reflect.DeepEqual(got, want) {
		t.Errorf("server.broker.instances = %#v, want %#v", got, want)
	}
}

func TestUpdateSetting_LegacyKeepsV1OnlyTopLevelKeys(t *testing.T) {
	dir := carryTestDir(t, "settings.yaml", legacyWithV1KeysYAML)

	if err := UpdateSetting(dir, "default_template", "custom", false); err != nil {
		t.Fatalf("UpdateSetting: %v", err)
	}

	m := readSettingsMap(t, filepath.Join(dir, "settings.yaml"))
	assertV1KeysKept(t, m)
	if m["schema_version"] != "1" {
		t.Errorf("schema_version = %v, want \"1\"", m["schema_version"])
	}
	if m["default_template"] != "custom" {
		t.Errorf("default_template = %v, want custom", m["default_template"])
	}
	if m["active_profile"] != "local" {
		t.Errorf("active_profile = %v, want local", m["active_profile"])
	}

	// The effective (struct-loaded) broker port is the file's, not the default.
	vs, err := LoadSingleFileVersioned(dir)
	if err != nil {
		t.Fatalf("LoadSingleFileVersioned: %v", err)
	}
	if vs.Server == nil || vs.Server.Broker == nil || vs.Server.Broker.Port != 19800 {
		t.Errorf("loaded broker port: got %+v, want 19800", vs.Server)
	}
}

func TestUpdateSetting_VersionedFileUnaffected(t *testing.T) {
	dir := carryTestDir(t, "settings.yaml", versionedWithV1KeysYAML)

	if err := UpdateSetting(dir, "default_template", "custom", false); err != nil {
		t.Fatalf("UpdateSetting: %v", err)
	}

	path := filepath.Join(dir, "settings.yaml")
	m := readSettingsMap(t, path)
	assertV1KeysKept(t, m)
	if m["default_template"] != "custom" {
		t.Errorf("default_template = %v, want custom", m["default_template"])
	}
	data, _ := os.ReadFile(path)
	if !strings.Contains(string(data), "# broker settings") {
		t.Errorf("versioned file was rewritten, not edited in place:\n%s", data)
	}
	if _, err := os.Stat(path + ".bak"); err == nil {
		t.Error("versioned file was migrated (backup created)")
	}
}

func TestMigrateSettingsFile_KeepsV1OnlyTopLevelKeys(t *testing.T) {
	for _, tc := range []struct {
		name, file, content string
	}{
		{"yaml", "settings.yaml", legacyWithV1KeysYAML},
		{"json", "settings.json", legacyWithV1KeysJSON},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := carryTestDir(t, tc.file, tc.content)

			result, err := MigrateSettingsFile(dir, false)
			if err != nil {
				t.Fatalf("MigrateSettingsFile: %v", err)
			}
			if result.Skipped {
				t.Fatalf("migration skipped: %s", result.SkipReason)
			}

			m := readSettingsMap(t, filepath.Join(dir, "settings.yaml"))
			assertV1KeysKept(t, m)
			if m["schema_version"] != "1" {
				t.Errorf("schema_version = %v, want \"1\"", m["schema_version"])
			}
			// A key the legacy struct decodes is still converted.
			if m["active_profile"] != "local" {
				t.Errorf("active_profile = %v, want local", m["active_profile"])
			}
			// hub_connections is legacy-only and is not carried.
			if _, ok := m["hub_connections"]; ok {
				t.Error("legacy hub_connections was carried into the v1 file")
			}
		})
	}
}

func TestMigrateSettingsFile_DryRunKeepsFile(t *testing.T) {
	dir := carryTestDir(t, "settings.yaml", legacyWithV1KeysYAML)
	if _, err := MigrateSettingsFile(dir, true); err != nil {
		t.Fatalf("MigrateSettingsFile dry run: %v", err)
	}
	data, _ := os.ReadFile(filepath.Join(dir, "settings.yaml"))
	if string(data) != legacyWithV1KeysYAML {
		t.Errorf("dry run changed the file:\n%s", data)
	}
}

// A converted field and a carried sibling under the same key are merged:
// the legacy hub.brokerId becomes server.broker.broker_id next to the
// file's own server.broker.port.
func TestMigrateSettingsFile_MergesCarriedWithConverted(t *testing.T) {
	dir := carryTestDir(t, "settings.yaml", `hub:
  brokerId: broker-123
server:
  broker:
    port: 19800
    broker_id: stale-id
`)
	if _, err := MigrateSettingsFile(dir, false); err != nil {
		t.Fatalf("MigrateSettingsFile: %v", err)
	}
	m := readSettingsMap(t, filepath.Join(dir, "settings.yaml"))
	if port, _ := lookupPath(m, "server", "broker", "port"); port != 19800 {
		t.Errorf("server.broker.port = %v, want 19800", port)
	}
	if id, _ := lookupPath(m, "server", "broker", "broker_id"); id != "broker-123" {
		t.Errorf("server.broker.broker_id = %v, want the converted broker-123", id)
	}
}

// A carried key the v1 schema does not know is kept, with a warning,
// rather than dropped or failing the migration.
func TestMigrateSettingsFile_UnknownKeyKeptWithWarning(t *testing.T) {
	dir := carryTestDir(t, "settings.yaml", "active_profile: local\ncustom_extension:\n  enabled: true\n")
	result, err := MigrateSettingsFile(dir, false)
	if err != nil {
		t.Fatalf("MigrateSettingsFile: %v", err)
	}
	m := readSettingsMap(t, filepath.Join(dir, "settings.yaml"))
	if v, _ := lookupPath(m, "custom_extension", "enabled"); v != true {
		t.Errorf("custom_extension.enabled = %v, want true", v)
	}
	var warned bool
	for _, w := range result.Warnings {
		if strings.Contains(w, "custom_extension") {
			warned = true
		}
	}
	if !warned {
		t.Errorf("no warning for the kept unknown key; warnings: %v", result.Warnings)
	}
}

func TestMigrateSettingsFile_VersionedFileUnaffected(t *testing.T) {
	dir := carryTestDir(t, "settings.yaml", versionedWithV1KeysYAML)
	result, err := MigrateSettingsFile(dir, false)
	if err != nil {
		t.Fatalf("MigrateSettingsFile: %v", err)
	}
	if !result.Skipped {
		t.Error("versioned file was not skipped")
	}
	data, _ := os.ReadFile(filepath.Join(dir, "settings.yaml"))
	if string(data) != versionedWithV1KeysYAML {
		t.Errorf("versioned file changed:\n%s", data)
	}
}

func TestDeleteHubConnection_LegacyKeepsV1OnlyTopLevelKeys(t *testing.T) {
	dir := carryTestDir(t, "settings.yaml", legacyWithV1KeysYAML)

	if err := DeleteHubConnection(dir, "hub-prod", false); err != nil {
		t.Fatalf("DeleteHubConnection: %v", err)
	}

	m := readSettingsMap(t, filepath.Join(dir, "settings.yaml"))
	assertV1KeysKept(t, m)
	if _, ok := lookupPath(m, "hub_connections", "hub-prod"); ok {
		t.Error("hub-prod was not deleted")
	}
	if ep, _ := lookupPath(m, "hub_connections", "hub-staging", "endpoint"); ep != "https://hub.staging.example.com" {
		t.Errorf("hub-staging endpoint = %v, want it kept", ep)
	}
	if m["active_profile"] != "local" {
		t.Errorf("active_profile = %v, want local", m["active_profile"])
	}
	if _, ok := m["schema_version"]; ok {
		t.Error("delete path changed the file's format (schema_version added)")
	}
}

func TestDeleteHubConnection_JSONKeepsV1OnlyTopLevelKeys(t *testing.T) {
	dir := carryTestDir(t, "settings.json", strings.Replace(legacyWithV1KeysJSON,
		`"active_profile": "local",`,
		`"active_profile": "local", "hub_connections": {"hub-prod": {"endpoint": "https://hub.prod.example.com"}},`, 1))

	if err := DeleteHubConnection(dir, "hub-prod", false); err != nil {
		t.Fatalf("DeleteHubConnection: %v", err)
	}

	m := readSettingsMap(t, filepath.Join(dir, "settings.yaml"))
	assertV1KeysKept(t, m)
	if _, ok := m["hub_connections"]; ok {
		t.Error("empty hub_connections was not removed")
	}
	if _, err := os.Stat(filepath.Join(dir, "settings.json")); !os.IsNotExist(err) {
		t.Errorf("settings.json not removed after conversion: %v", err)
	}
}

func TestDeleteHubConnection_VersionedFileUnaffected(t *testing.T) {
	dir := carryTestDir(t, "settings.yaml", versionedWithV1KeysYAML)
	if err := DeleteHubConnection(dir, "hub-prod", false); err != nil {
		t.Fatalf("DeleteHubConnection: %v", err)
	}
	data, _ := os.ReadFile(filepath.Join(dir, "settings.yaml"))
	if string(data) != versionedWithV1KeysYAML {
		t.Errorf("versioned file changed:\n%s", data)
	}
}

func TestDeleteHubConnection_NoFileCreatesNothing(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, ".scion")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := DeleteHubConnection(dir, "hub-prod", false); err != nil {
		t.Fatalf("DeleteHubConnection: %v", err)
	}
	if p := GetSettingsPath(dir); p != "" {
		t.Errorf("settings file %s created", p)
	}
}

// legacySettingsTopLevelKeys must track the legacy struct's yaml tags.
func TestLegacySettingsTopLevelKeys(t *testing.T) {
	for _, k := range []string{"project_id", "active_profile", "hub", "hub_connections", "runtimes", "harnesses", "profiles"} {
		if !legacySettingsTopLevelKeys[k] {
			t.Errorf("legacy key %q missing", k)
		}
	}
	for _, k := range []string{"server", "image_registry", "schema_version"} {
		if legacySettingsTopLevelKeys[k] {
			t.Errorf("v1-only key %q treated as legacy", k)
		}
	}
}
