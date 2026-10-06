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
	"time"

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
	result, err := MigrateSettingsFile(dir, false)
	if err != nil {
		t.Fatalf("MigrateSettingsFile: %v", err)
	}
	m := readSettingsMap(t, filepath.Join(dir, "settings.yaml"))
	if port, _ := lookupPath(m, "server", "broker", "port"); port != 19800 {
		t.Errorf("server.broker.port = %v, want 19800", port)
	}
	if id, _ := lookupPath(m, "server", "broker", "broker_id"); id != "broker-123" {
		t.Errorf("server.broker.broker_id = %v, want the converted broker-123", id)
	}
	// The dropped carried value is reported by path only, never by value
	// (settings can hold secrets such as broker tokens).
	want := "kept setting server.broker.broker_id dropped: it conflicts with the value converted from the legacy settings, which is used instead"
	assertWarnings(t, result.Warnings, []string{want}, []string{"server.broker.port", "stale-id"})
}

// assertWarnings checks every want string is one of warnings, and that no
// warning mentions any of notMentioned.
func assertWarnings(t *testing.T, warnings, want, notMentioned []string) {
	t.Helper()
	for _, w := range want {
		found := false
		for _, got := range warnings {
			if got == w {
				found = true
			}
		}
		if !found {
			t.Errorf("missing warning %q; warnings: %q", w, warnings)
		}
	}
	for _, got := range warnings {
		for _, n := range notMentioned {
			if strings.Contains(got, n) {
				t.Errorf("warning %q mentions %q", got, n)
			}
		}
	}
}

// A carried value whose shape clashes with a converted one (a scalar where
// the converted settings have a mapping) is dropped, as the converted value
// wins, and reported. An equal carried value is not reported.
func TestMigrateSettingsFile_DroppedCarriedValueWarnings(t *testing.T) {
	for _, tc := range []struct {
		name, content string
		want          []string
		notMentioned  []string
	}{
		{
			name:         "scalar against converted mapping",
			content:      "hub:\n  brokerId: broker-123\nserver: secret-hello-value\n",
			want:         []string{"kept setting server dropped: it conflicts with the value converted from the legacy settings, which is used instead"},
			notMentioned: []string{"secret-hello-value"},
		},
		{
			name:         "list against converted mapping",
			content:      "hub:\n  brokerId: broker-123\nserver:\n  broker: [secret-list-value]\n",
			want:         []string{"kept setting server.broker dropped: it conflicts with the value converted from the legacy settings, which is used instead"},
			notMentioned: []string{"secret-list-value"},
		},
		{
			name:         "equal value",
			content:      "hub:\n  brokerId: broker-123\nserver:\n  broker:\n    broker_id: broker-123\n    port: 19800\n",
			notMentioned: []string{"kept setting"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := carryTestDir(t, "settings.yaml", tc.content)
			result, err := MigrateSettingsFile(dir, false)
			if err != nil {
				t.Fatalf("MigrateSettingsFile: %v", err)
			}
			assertWarnings(t, result.Warnings, tc.want, tc.notMentioned)
			// The converted value is what is written.
			m := readSettingsMap(t, filepath.Join(dir, "settings.yaml"))
			if id, _ := lookupPath(m, "server", "broker", "broker_id"); id != "broker-123" {
				t.Errorf("server.broker.broker_id = %v, want broker-123", id)
			}
		})
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
	// No hub_connections to delete: the file is not rewritten.
	dir := carryTestDir(t, "settings.yaml", versionedWithV1KeysYAML)
	if err := DeleteHubConnection(dir, "hub-prod", false); err != nil {
		t.Fatalf("DeleteHubConnection: %v", err)
	}
	data, _ := os.ReadFile(filepath.Join(dir, "settings.yaml"))
	if string(data) != versionedWithV1KeysYAML {
		t.Errorf("versioned file changed:\n%s", data)
	}
}

// A real delete in a versioned file edits it in place: schema_version, the
// v1 keys, comments and key order survive. Before ptone/scion#3497 the file
// was rewritten as a legacy file, losing all of them.
func TestDeleteHubConnection_VersionedFileEditedInPlace(t *testing.T) {
	const content = `schema_version: "1"
# broker settings
image_registry: ghcr.io/example/scion
hub_connections:
  hub-prod:
    endpoint: https://hub.prod.example.com # prod hub
  hub-staging:
    endpoint: https://hub.staging.example.com
server:
  broker:
    port: 19800
    instances:
      - key: local-docker
        name: local-docker
        runtime_target:
          type: docker
`
	const want = `schema_version: "1"
# broker settings
image_registry: ghcr.io/example/scion
hub_connections:
  hub-staging:
    endpoint: https://hub.staging.example.com
server:
  broker:
    port: 19800
    instances:
      - key: local-docker
        name: local-docker
        runtime_target:
          type: docker
`
	dir := carryTestDir(t, "settings.yaml", content)
	if err := DeleteHubConnection(dir, "hub-prod", false); err != nil {
		t.Fatalf("DeleteHubConnection: %v", err)
	}
	path := filepath.Join(dir, "settings.yaml")
	data, _ := os.ReadFile(path)
	if string(data) != want {
		t.Errorf("got:\n%s\nwant:\n%s", data, want)
	}
	assertV1KeysKept(t, readSettingsMap(t, path))

	// Deleting the last connection removes hub_connections entirely.
	if err := DeleteHubConnection(dir, "hub-staging", false); err != nil {
		t.Fatalf("DeleteHubConnection: %v", err)
	}
	m := readSettingsMap(t, path)
	if _, ok := m["hub_connections"]; ok {
		t.Error("empty hub_connections was not removed")
	}
	if m["schema_version"] != "1" {
		t.Errorf("schema_version = %v, want \"1\"", m["schema_version"])
	}
	assertV1KeysKept(t, m)
}

// A wrongly typed v1-only key is not the delete path's to fix: the file
// stays unversioned and the key is kept as it was, so it still loads the
// way it did before.
func TestDeleteHubConnection_UndecodableV1KeyKept(t *testing.T) {
	const content = `active_profile: local
server: hello
hub_connections:
  hub-prod:
    endpoint: https://hub.prod.example.com
`
	dir := carryTestDir(t, "settings.yaml", content)
	if err := DeleteHubConnection(dir, "hub-prod", false); err != nil {
		t.Fatalf("DeleteHubConnection: %v", err)
	}
	m := readSettingsMap(t, filepath.Join(dir, "settings.yaml"))
	if m["server"] != "hello" {
		t.Errorf("server = %v, want hello kept", m["server"])
	}
	if _, ok := m["schema_version"]; ok {
		t.Error("delete path changed the file's format (schema_version added)")
	}
	if _, ok := m["hub_connections"]; ok {
		t.Error("hub_connections not removed")
	}
}

// When the delete cannot be made safely (hub_connections is an anchored
// node another key aliases), it fails and the file is left byte-for-byte
// unchanged.
func TestDeleteHubConnection_FailureLeavesFileUntouched(t *testing.T) {
	const content = `image_registry: ghcr.io/example/scion
server:
  broker:
    port: 19800
hub_connections: &conns
  hub-prod:
    endpoint: https://hub.prod.example.com
backup_connections: *conns
`
	dir := carryTestDir(t, "settings.yaml", content)
	if err := DeleteHubConnection(dir, "hub-prod", false); err == nil {
		t.Fatal("DeleteHubConnection through an anchored node: want error, got nil")
	}
	data, _ := os.ReadFile(filepath.Join(dir, "settings.yaml"))
	if string(data) != content {
		t.Errorf("file changed after a failed delete:\n%s", data)
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

// undecodableLegacyFiles are unversioned settings files whose v1-only keys
// have a type the v1 loaders cannot decode. hub.lastSyncedAt checks the
// state.yaml side effect is not written either.
var undecodableLegacyFiles = []struct {
	name, content, key string
}{
	{"server scalar", "active_profile: local\nhub:\n  lastSyncedAt: \"2026-01-01T00:00:00Z\"\nserver: hello\n", "server"},
	{"broker port string", "active_profile: local\nhub:\n  lastSyncedAt: \"2026-01-01T00:00:00Z\"\nserver:\n  broker:\n    port: abc\n", "server"},
	{"image_registry list", "active_profile: local\nimage_registry: [a, b]\nserver:\n  broker:\n    port: 19800\n", "image_registry"},
}

// assertDirUntouched checks dir still holds only settings.yaml, unchanged.
func assertDirUntouched(t *testing.T, dir, content string) {
	t.Helper()
	data, _ := os.ReadFile(filepath.Join(dir, "settings.yaml"))
	if string(data) != content {
		t.Errorf("settings.yaml changed:\n%s", data)
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if e.Name() != "settings.yaml" {
			t.Errorf("unexpected file %s written", e.Name())
		}
	}
}

func TestMigrateSettingsFile_UndecodableCarriedKeyLeavesFileUntouched(t *testing.T) {
	for _, tc := range undecodableLegacyFiles {
		t.Run(tc.name, func(t *testing.T) {
			dir := carryTestDir(t, "settings.yaml", tc.content)
			_, err := MigrateSettingsFile(dir, false)
			if err == nil {
				t.Fatal("want error, got nil")
			}
			if !strings.Contains(err.Error(), tc.key) {
				t.Errorf("error %q does not name key %q", err, tc.key)
			}
			assertDirUntouched(t, dir, tc.content)

			// A dry run reports the same error.
			if _, err := MigrateSettingsFile(dir, true); err == nil {
				t.Error("dry run: want error, got nil")
			}
		})
	}
}

func TestUpdateSetting_UndecodableCarriedKeyLeavesFileUntouched(t *testing.T) {
	for _, tc := range undecodableLegacyFiles {
		t.Run(tc.name, func(t *testing.T) {
			// Global scope: LoadGlobalSettings must load the same way
			// before and after the failed write.
			dir := carryTestDir(t, "settings.yaml", tc.content)
			if _, _, err := LoadGlobalSettings(); err != nil {
				t.Fatalf("LoadGlobalSettings before: %v", err)
			}
			err := UpdateSetting("", "default_template", "custom", true)
			if err == nil {
				t.Fatal("want error, got nil")
			}
			if !strings.Contains(err.Error(), tc.key) {
				t.Errorf("error %q does not name key %q", err, tc.key)
			}
			assertDirUntouched(t, dir, tc.content)
			if _, _, err := LoadGlobalSettings(); err != nil {
				t.Errorf("LoadGlobalSettings after the failed write: %v", err)
			}
		})
	}
}

// json.Unmarshal matches legacy fields case-insensitively, so a "Hub" key
// is converted and must not also be carried. A key the legacy struct does
// not have, in any case, is carried.
func TestMigrateSettingsFile_JSONKeyCaseNotCarriedTwice(t *testing.T) {
	dir := carryTestDir(t, "settings.json", `{"Active_Profile": "local", "Hub": {"endpoint": "https://hub.example.com"}, "Image_Registry": "r"}`)
	if _, err := MigrateSettingsFile(dir, false); err != nil {
		t.Fatalf("MigrateSettingsFile: %v", err)
	}
	m := readSettingsMap(t, filepath.Join(dir, "settings.yaml"))
	for _, k := range []string{"Hub", "Active_Profile"} {
		if _, ok := m[k]; ok {
			t.Errorf("legacy key %q carried as well as converted", k)
		}
	}
	if ep, _ := lookupPath(m, "hub", "endpoint"); ep != "https://hub.example.com" {
		t.Errorf("hub.endpoint = %v, want it converted", ep)
	}
	if m["Image_Registry"] != "r" {
		t.Errorf("Image_Registry = %v, want carried", m["Image_Registry"])
	}
}

// legacySettingsTopLevelKeys reads only direct yaml tags; an embedded or
// inline field would hide its keys from it.
func TestLegacySettingsTopLevelKeys_NoEmbeddedFields(t *testing.T) {
	typ := reflect.TypeOf(Settings{})
	for i := 0; i < typ.NumField(); i++ {
		f := typ.Field(i)
		tag := f.Tag.Get("yaml")
		name, opts, _ := strings.Cut(tag, ",")
		if f.Anonymous || strings.Contains(opts, "inline") || name == "" || name == "-" {
			t.Errorf("Settings.%s (yaml %q) is embedded, inline or untagged; legacySettingsTopLevelKeys must handle it", f.Name, tag)
		}
	}
	if len(legacySettingsTopLevelKeys) != typ.NumField() {
		t.Errorf("legacySettingsTopLevelKeys has %d keys, Settings has %d fields", len(legacySettingsTopLevelKeys), typ.NumField())
	}
}

// scion broker deregister holds LockSettingsFile while it calls
// DeleteHubConnection for a legacy file, so DeleteHubConnection must not
// take the (non-reentrant) lock itself.
func TestDeleteHubConnection_CallableUnderSettingsLock(t *testing.T) {
	dir := carryTestDir(t, "settings.yaml", legacyWithV1KeysYAML)
	unlock := LockSettingsFile()
	defer unlock()
	done := make(chan error, 1)
	go func() { done <- DeleteHubConnection(dir, "hub-prod", false) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("DeleteHubConnection: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("DeleteHubConnection blocked on the settings lock its caller holds")
	}
	assertV1KeysKept(t, readSettingsMap(t, filepath.Join(dir, "settings.yaml")))
}
