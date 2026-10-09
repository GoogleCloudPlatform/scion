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
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	yamlv3 "gopkg.in/yaml.v3"
)

// Tests for ptone/scion#3885: every key of an unversioned settings file is
// either converted to its v1 form, deliberately dropped, or (when the legacy
// struct does not decode it) carried into the v1 file unchanged, so a legacy
// key cannot be silently lost on the first settings write.

// legacyEveryKeyYAML is an unversioned settings file holding every key the
// legacy Settings struct decodes (and every hub and cli field), plus the
// v1-only server.broker.instances.
const legacyEveryKeyYAML = `project_id: proj-top-123
active_profile: local
default_template: custom-template
workspace_path: /work/space
bucket:
  provider: GCS
  name: legacy-bucket
  prefix: legacy-prefix
hub:
  enabled: true
  linked: true
  local_only: false
  endpoint: https://hub.example.com
  token: legacy-token
  apiKey: legacy-api-key
  projectId: proj-hub-456
  brokerId: broker-123
  brokerNickname: broker-nick
  brokerToken: broker-token
  lastSyncedAt: "2026-01-01T00:00:00Z"
  transport:
    mode: iap
    audience: client-id.apps.example.com
cli:
  autohelp: true
  mode: assistant
hub_connections:
  hub-prod:
    endpoint: https://hub.prod.example.com
runtimes:
  docker:
    context: default
harnesses:
  claude:
    image: example/claude:latest
    user: scion
profiles:
  local:
    runtime: docker
server:
  broker:
    instances:
      - key: local-docker
        name: local-docker
        runtime_target:
          type: docker
`

// legacyKeyCheck checks how one legacy key (a dotted path in
// legacyEveryKeyYAML) appears in the migrated v1 file m. in is the parsed
// legacy file.
type legacyKeyCheck func(t *testing.T, in, m map[string]interface{}, dir string)

// keptUnchanged checks the legacy value at path is at the same path in m.
func keptUnchanged(path ...string) legacyKeyCheck {
	return func(t *testing.T, in, m map[string]interface{}, _ string) {
		t.Helper()
		want, _ := lookupPath(in, path...)
		got, ok := lookupPath(m, path...)
		if !ok || !reflect.DeepEqual(got, want) {
			t.Errorf("%s = %#v (present %v), want %#v kept unchanged", strings.Join(path, "."), got, ok, want)
		}
	}
}

// convertedTo checks the migrated file holds want at path.
func convertedTo(want interface{}, path ...string) legacyKeyCheck {
	return func(t *testing.T, _, m map[string]interface{}, _ string) {
		t.Helper()
		got, ok := lookupPath(m, path...)
		if !ok || !reflect.DeepEqual(got, want) {
			t.Errorf("%s = %#v (present %v), want %#v", strings.Join(path, "."), got, ok, want)
		}
	}
}

// absent checks path is not in the migrated file.
func absent(path ...string) legacyKeyCheck {
	return func(t *testing.T, _, m map[string]interface{}, _ string) {
		t.Helper()
		if v, ok := lookupPath(m, path...); ok {
			t.Errorf("%s = %#v, want it dropped", strings.Join(path, "."), v)
		}
	}
}

// legacyKeyChecks has one entry per legacy key: each entry of
// legacyTopLevelKeyHandling, and "<key>.<field>" for each entry of
// legacySubKeyHandling. TestLegacyKeyChecks_CoverHandlingTables keeps it
// in step with those tables.
var legacyKeyChecks = map[string]legacyKeyCheck{
	"project_id":       keptUnchanged("project_id"),
	"active_profile":   convertedTo("local", "active_profile"),
	"default_template": convertedTo("custom-template", "default_template"),
	"workspace_path":   keptUnchanged("workspace_path"),
	"bucket":           absent("bucket"),
	"hub_connections":  keptUnchanged("hub_connections"),
	"runtimes":         convertedTo(map[string]interface{}{"type": "docker", "context": "default"}, "runtimes", "docker"),
	"harnesses": func(t *testing.T, _, m map[string]interface{}, _ string) {
		t.Helper()
		convertedTo("claude", "harness_configs", "claude", "harness")(t, nil, m, "")
		convertedTo("example/claude:latest", "harness_configs", "claude", "image")(t, nil, m, "")
		absent("harnesses")(t, nil, m, "")
	},
	"profiles": convertedTo("docker", "profiles", "local", "runtime"),

	"hub.enabled":        convertedTo(true, "hub", "enabled"),
	"hub.linked":         convertedTo(true, "hub", "linked"),
	"hub.local_only":     convertedTo(false, "hub", "local_only"),
	"hub.endpoint":       convertedTo("https://hub.example.com", "hub", "endpoint"),
	"hub.token":          absent("hub", "token"),
	"hub.apiKey":         absent("hub", "apiKey"),
	"hub.projectId":      convertedTo("proj-hub-456", "hub", "project_id"),
	"hub.brokerId":       convertedTo("broker-123", "server", "broker", "broker_id"),
	"hub.brokerNickname": convertedTo("broker-nick", "server", "broker", "broker_nickname"),
	"hub.brokerToken":    convertedTo("broker-token", "server", "broker", "broker_token"),
	"hub.lastSyncedAt": func(t *testing.T, _, m map[string]interface{}, dir string) {
		t.Helper()
		absent("hub", "lastSyncedAt")(t, nil, m, "")
		state, err := LoadProjectState(dir)
		if err != nil {
			t.Fatalf("LoadProjectState: %v", err)
		}
		if state.LastSyncedAt != "2026-01-01T00:00:00Z" {
			t.Errorf("state.yaml last_synced_at = %q, want it moved from hub.lastSyncedAt", state.LastSyncedAt)
		}
	},
	"hub.transport": keptUnchanged("hub", "transport"),
	"cli.autohelp":  convertedTo(true, "cli", "autohelp"),
	"cli.mode":      keptUnchanged("cli", "mode"),
}

// yamlTagNames returns the yaml tag names of the fields of struct type typ.
func yamlTagNames(typ reflect.Type) []string {
	var names []string
	for i := 0; i < typ.NumField(); i++ {
		name, _, _ := strings.Cut(typ.Field(i).Tag.Get("yaml"), ",")
		if name != "" && name != "-" {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// The handling tables cover every key the legacy structs decode: a new
// field on Settings, HubClientConfig or CLIConfig must be given a handling,
// or this fails.
func TestLegacyKeyHandling_CoversLegacyStructs(t *testing.T) {
	settingsType := reflect.TypeOf(Settings{})
	var wantTop []string
	for _, k := range yamlTagNames(settingsType) {
		_, top := legacyTopLevelKeyHandling[k]
		_, sub := legacySubKeyHandling[k]
		switch {
		case top && sub:
			t.Errorf("legacy key %q is in both legacyTopLevelKeyHandling and legacySubKeyHandling", k)
		case !top && !sub:
			t.Errorf("legacy key %q has no handling in legacyTopLevelKeyHandling or legacySubKeyHandling", k)
		}
		if !sub {
			wantTop = append(wantTop, k)
		}
	}
	if got := sortedKeys(legacyTopLevelKeyHandling); !reflect.DeepEqual(got, wantTop) {
		t.Errorf("legacyTopLevelKeyHandling keys = %v, want %v", got, wantTop)
	}
	for k, fields := range legacySubKeyHandling {
		var typ reflect.Type
		for i := 0; i < settingsType.NumField(); i++ {
			if tag, _, _ := strings.Cut(settingsType.Field(i).Tag.Get("yaml"), ","); tag == k {
				typ = settingsType.Field(i).Type
			}
		}
		if typ == nil {
			t.Errorf("legacySubKeyHandling key %q is not a Settings field", k)
			continue
		}
		if typ.Kind() == reflect.Pointer {
			typ = typ.Elem()
		}
		if got, want := sortedKeys(fields), yamlTagNames(typ); !reflect.DeepEqual(got, want) {
			t.Errorf("legacySubKeyHandling[%q] keys = %v, want the %s fields %v", k, got, typ.Name(), want)
		}
	}
	for k, h := range legacyTopLevelKeyHandling {
		if h == 0 {
			t.Errorf("legacy key %q has no handling", k)
		}
	}
}

// legacyKeyChecks, and the fixture, cover every entry of the handling
// tables, so a key added to the tables is checked end to end.
func TestLegacyKeyChecks_CoverHandlingTables(t *testing.T) {
	var in map[string]interface{}
	if err := yamlv3.Unmarshal([]byte(legacyEveryKeyYAML), &in); err != nil {
		t.Fatal(err)
	}
	var want []string
	for k := range legacyTopLevelKeyHandling {
		want = append(want, k)
		if _, ok := in[k]; !ok {
			t.Errorf("legacyEveryKeyYAML has no %q", k)
		}
	}
	for k, fields := range legacySubKeyHandling {
		for f := range fields {
			want = append(want, k+"."+f)
			if _, ok := lookupPath(in, k, f); !ok {
				t.Errorf("legacyEveryKeyYAML has no %s.%s", k, f)
			}
		}
	}
	sort.Strings(want)
	if got := sortedKeys(legacyKeyChecks); !reflect.DeepEqual(got, want) {
		t.Errorf("legacyKeyChecks keys = %v, want %v", got, want)
	}
}

// Migrating a legacy file with every legacy key keeps each one as its
// handling says, for YAML and JSON files.
func TestMigrateSettingsFile_EveryLegacyKey(t *testing.T) {
	var in map[string]interface{}
	if err := yamlv3.Unmarshal([]byte(legacyEveryKeyYAML), &in); err != nil {
		t.Fatal(err)
	}
	jsonData, err := json.MarshalIndent(in, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, file, content string
	}{
		{"yaml", "settings.yaml", legacyEveryKeyYAML},
		{"json", "settings.json", string(jsonData)},
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
			for _, k := range sortedKeys(legacyKeyChecks) {
				t.Run(k, func(t *testing.T) {
					legacyKeyChecks[k](t, in, m, dir)
				})
			}
			keptUnchanged("server", "broker", "instances")(t, in, m, dir)
		})
	}
}

// scion config set against a legacy file migrates it first; the converted
// keys survive that write and are read back by the settings loader.
func TestUpdateSetting_LegacyKeepsConvertedV1Keys(t *testing.T) {
	dir := carryTestDir(t, "settings.yaml", legacyEveryKeyYAML)
	if err := UpdateSetting(dir, "default_template", "other", false); err != nil {
		t.Fatalf("UpdateSetting: %v", err)
	}
	m := readSettingsMap(t, filepath.Join(dir, "settings.yaml"))
	if m["schema_version"] != "1" {
		t.Fatalf("schema_version = %v, want \"1\"", m["schema_version"])
	}
	if m["default_template"] != "other" {
		t.Errorf("default_template = %v, want other", m["default_template"])
	}

	s, err := LoadSettingsFromDir(dir)
	if err != nil {
		t.Fatalf("LoadSettingsFromDir: %v", err)
	}
	if s.ProjectID != "proj-top-123" {
		t.Errorf("project_id = %q, want proj-top-123", s.ProjectID)
	}
	if s.WorkspacePath != "/work/space" {
		t.Errorf("workspace_path = %q, want /work/space", s.WorkspacePath)
	}
	if s.CLI == nil || s.CLI.Mode != "assistant" {
		t.Errorf("cli = %+v, want mode assistant", s.CLI)
	}
	if s.Hub == nil || s.Hub.Transport == nil || s.Hub.Transport.Mode != "iap" || s.Hub.Transport.Audience != "client-id.apps.example.com" {
		t.Errorf("hub = %+v, want transport iap with its audience", s.Hub)
	}
	if got := s.HubConnections["hub-prod"].Endpoint; got != "https://hub.prod.example.com" {
		t.Errorf("hub_connections.hub-prod.endpoint = %q, want https://hub.prod.example.com", got)
	}
}

// In a JSON file the legacy keys match case-insensitively; a converted
// legacy key or field is written under its canonical name, once.
func TestMigrateSettingsFile_JSONLegacyKeyCanonicalName(t *testing.T) {
	dir := carryTestDir(t, "settings.json", `{"Workspace_Path": "/w", "CLI": {"Mode": "agent", "autohelp": true}}`)
	if _, err := MigrateSettingsFile(dir, false); err != nil {
		t.Fatalf("MigrateSettingsFile: %v", err)
	}
	m := readSettingsMap(t, filepath.Join(dir, "settings.yaml"))
	if m["workspace_path"] != "/w" {
		t.Errorf("workspace_path = %v, want /w", m["workspace_path"])
	}
	if v, _ := lookupPath(m, "cli", "mode"); v != "agent" {
		t.Errorf("cli.mode = %v, want agent", v)
	}
	if v, _ := lookupPath(m, "cli", "autohelp"); v != true {
		t.Errorf("cli.autohelp = %v, want true", v)
	}
	for _, k := range []string{"Workspace_Path", "CLI"} {
		if _, ok := m[k]; ok {
			t.Errorf("key %q written under its original case", k)
		}
	}
}

// After migration, the converted keys survive every later settings write:
// the in-place YAML edit (UpdateSetting) and the struct round-trip writers
// (LoadModifySaveVersionedSettings, SaveVersionedSettings and the
// updateVersionedSettingStruct fallback), and the settings loader still
// reads them.
func TestMigratedLegacyKeys_SurviveLaterWrites(t *testing.T) {
	kept := []string{"workspace_path", "project_id", "hub_connections", "hub.transport", "cli.mode"}
	dir := carryTestDir(t, "settings.yaml", legacyEveryKeyYAML)
	path := filepath.Join(dir, "settings.yaml")
	var in map[string]interface{}
	if err := yamlv3.Unmarshal([]byte(legacyEveryKeyYAML), &in); err != nil {
		t.Fatal(err)
	}

	for _, w := range []struct {
		name  string
		write func() error
	}{
		{"UpdateSetting migrating", func() error { return UpdateSetting(dir, "default_template", "other", false) }},
		{"UpdateSetting in place", func() error { return UpdateSetting(dir, "active_profile", "remote", false) }},
		{"LoadModifySaveVersionedSettings", func() error {
			return LoadModifySaveVersionedSettings(dir, func(vs *VersionedSettings) error {
				vs.DefaultTemplate = "third"
				return nil
			})
		}},
		{"SaveVersionedSettings", func() error {
			vs, err := LoadSingleFileVersioned(dir)
			if err != nil {
				return err
			}
			vs.DefaultTemplate = "fourth"
			return SaveVersionedSettings(dir, vs)
		}},
		{"updateVersionedSettingStruct", func() error { return updateVersionedSettingStruct(dir, "default_template", "fifth") }},
	} {
		if err := w.write(); err != nil {
			t.Fatalf("%s: %v", w.name, err)
		}
		m := readSettingsMap(t, path)
		for _, k := range kept {
			keptUnchanged(strings.Split(k, ".")...)(t, in, m, dir)
		}
		if t.Failed() {
			t.Fatalf("kept keys lost after %s", w.name)
		}
	}

	s, err := LoadSettingsFromDir(dir)
	if err != nil {
		t.Fatalf("LoadSettingsFromDir: %v", err)
	}
	if s.DefaultTemplate != "fifth" {
		t.Errorf("default_template = %q, want fifth", s.DefaultTemplate)
	}
	if s.CLI == nil || s.CLI.Mode != "assistant" {
		t.Errorf("cli = %+v, want mode assistant", s.CLI)
	}
	if s.Hub == nil || s.Hub.Transport == nil || s.Hub.Transport.Mode != "iap" {
		t.Errorf("hub = %+v, want transport mode iap", s.Hub)
	}
	if got := s.HubConnections["hub-prod"].Endpoint; got != "https://hub.prod.example.com" {
		t.Errorf("hub_connections.hub-prod.endpoint = %q", got)
	}
	if s.WorkspacePath != "/work/space" {
		t.Errorf("workspace_path = %q, want /work/space", s.WorkspacePath)
	}
}

// AdaptLegacySettings (also used to load a legacy file in memory) maps the
// legacy keys that have a v1 field. The top-level project_id is kept as is;
// hub.project_id stays the hub value.
func TestAdaptLegacySettings_KeysWithV1Fields(t *testing.T) {
	var legacy Settings
	if err := yamlv3.Unmarshal([]byte(legacyEveryKeyYAML), &legacy); err != nil {
		t.Fatal(err)
	}
	vs, _ := AdaptLegacySettings(&legacy)
	if vs.WorkspacePath != "/work/space" {
		t.Errorf("WorkspacePath = %q, want /work/space", vs.WorkspacePath)
	}
	if vs.ProjectID != "proj-top-123" {
		t.Errorf("ProjectID = %q, want proj-top-123", vs.ProjectID)
	}
	if vs.Hub == nil || vs.Hub.ProjectID != "proj-hub-456" {
		t.Errorf("Hub = %+v, want project_id proj-hub-456", vs.Hub)
	}
	if vs.Hub == nil || !reflect.DeepEqual(vs.Hub.Transport, &V1HubTransportConfig{Mode: "iap", Audience: "client-id.apps.example.com"}) {
		t.Errorf("Hub.Transport = %+v, want iap with its audience", vs.Hub)
	}
	if vs.CLI == nil || vs.CLI.Mode != "assistant" {
		t.Errorf("CLI = %+v, want mode assistant", vs.CLI)
	}
	want := map[string]V1HubConnectionConfig{"hub-prod": {Endpoint: "https://hub.prod.example.com"}}
	if !reflect.DeepEqual(vs.HubConnections, want) {
		t.Errorf("HubConnections = %+v, want %+v", vs.HubConnections, want)
	}
}

// A JSON legacy file may spell nested keys in any case (json.Unmarshal
// matches struct fields case-insensitively). Each such key is written once,
// under its canonical name, so the migrated file passes config validate and
// keeps every value.
func TestMigrateSettingsFile_JSONNestedKeyCaseCanonical(t *testing.T) {
	dir := carryTestDir(t, "settings.json", `{
  "Project_ID": "proj-top",
  "Hub": {"Endpoint": "https://h", "Transport": {"Mode": "iap", "Audience": "a"}},
  "Hub_Connections": {"p": {"Endpoint": "https://p"}},
  "CLI": {"Mode": "agent"}
}`)
	result, err := MigrateSettingsFile(dir, false)
	if err != nil {
		t.Fatalf("MigrateSettingsFile: %v", err)
	}
	for _, w := range result.Warnings {
		if strings.Contains(w, "kept setting") {
			t.Errorf("unexpected warning: %s", w)
		}
	}
	path := filepath.Join(dir, "settings.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	verrs, err := ValidateSettings(data, "1")
	if err != nil {
		t.Fatalf("ValidateSettings: %v", err)
	}
	for _, ve := range verrs {
		t.Errorf("migrated file fails validation: %s", ve.Error())
	}
	m := readSettingsMap(t, path)
	want := map[string]interface{}{
		"schema_version":  "1",
		"project_id":      "proj-top",
		"hub":             map[string]interface{}{"endpoint": "https://h", "transport": map[string]interface{}{"mode": "iap", "audience": "a"}},
		"hub_connections": map[string]interface{}{"p": map[string]interface{}{"endpoint": "https://p"}},
		"cli":             map[string]interface{}{"mode": "agent"},
	}
	if !reflect.DeepEqual(m, want) {
		t.Errorf("migrated settings = %#v, want %#v", m, want)
	}
}

// Legacy keys are converted through the v1 types, so a field the legacy
// struct never read (here an extra field in hub.transport and in a hub
// connection) is dropped by the migration, as by every later struct
// round-trip write; the migrated file stays valid. Only top-level keys the
// legacy struct does not decode (here the v1-only image_registry) are
// carried unchanged.
func TestMigrateSettingsFile_ConvertedKeysDropUnmodelledFields(t *testing.T) {
	dir := carryTestDir(t, "settings.yaml", `hub:
  endpoint: https://hub.example.com
  transport:
    mode: iap
    extra: dropped-transport
hub_connections:
  prod:
    endpoint: https://hub.prod.example.com
    extra: dropped-connection
image_registry: registry.example.com/team
`)
	if _, err := MigrateSettingsFile(dir, false); err != nil {
		t.Fatalf("MigrateSettingsFile: %v", err)
	}
	path := filepath.Join(dir, "settings.yaml")
	m := readSettingsMap(t, path)
	for p, want := range map[string]string{
		"hub.transport.mode":            "iap",
		"hub_connections.prod.endpoint": "https://hub.prod.example.com",
		"hub.endpoint":                  "https://hub.example.com",
		"image_registry":                "registry.example.com/team",
	} {
		if got, _ := lookupPath(m, strings.Split(p, ".")...); got != want {
			t.Errorf("%s = %v, want %q", p, got, want)
		}
	}
	absent("hub", "transport", "extra")(t, nil, m, "")
	absent("hub_connections", "prod", "extra")(t, nil, m, "")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	verrs, err := ValidateSettings(data, "1")
	if err != nil {
		t.Fatalf("ValidateSettings: %v", err)
	}
	for _, ve := range verrs {
		t.Errorf("migrated file fails validation: %s", ve.Error())
	}
}
