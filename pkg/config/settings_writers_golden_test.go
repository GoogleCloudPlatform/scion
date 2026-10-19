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
	"os"
	"path/filepath"
	"testing"
)

// Golden tests for every whole-file settings writer: each writer's output for
// a fixture with comments and unusual key order is pinned byte for byte. The
// goldens were produced by the writers as they were before the locked
// writer, so these tests show that routing every write through it changed no
// output: writers that kept comments and key order still do, and writers
// that rewrite from a struct or map (comments lost) still produce exactly
// what they did. Regenerate with SCION_UPDATE_SETTINGS_GOLDEN=1 only for an
// intended output change.

// goldenSettingsFixture has comments, blank lines, an inline comment and a
// key order no struct would produce.
const goldenSettingsFixture = `# Operator notes: keep this header.
schema_version: "1"

# Hub connections, newest first.
hub_connections:
  hub-staging:
    endpoint: https://staging.example.com # staging
  hub-prod:
    endpoint: https://prod.example.com

zz_unknown_key: kept # not a known key
default_harness_config: claude
active_profile: local
server:
  broker:
    broker_id: b-1 # registered
  hub:
    port: 9810
`

// goldenLegacyFixture is a legacy (unversioned) file with comments.
const goldenLegacyFixture = `# Legacy settings
active_profile: local # chosen
default_template: claude
hub:
  endpoint: https://hub.example.com
  lastSyncedAt: "2026-01-02T03:04:05Z"
image_registry: ghcr.io/example
`

type settingsWriterGolden struct {
	name    string
	file    string // fixture file name ("" for none)
	fixture string
	write   func(t *testing.T, dir string) error
	output  string // file whose bytes are compared
}

var settingsWriterGoldens = []settingsWriterGolden{
	{"update_versioned_existing_key", "settings.yaml", goldenSettingsFixture,
		func(t *testing.T, dir string) error { return UpdateVersionedSetting(dir, "active_profile", "remote") }, "settings.yaml"},
	{"update_versioned_new_nested_key", "settings.yaml", goldenSettingsFixture,
		func(t *testing.T, dir string) error {
			return UpdateVersionedSetting(dir, "server.auth.display_name", "Dev")
		}, "settings.yaml"},
	{"update_setting_auto_migrates_legacy", "settings.yaml", goldenLegacyFixture,
		func(t *testing.T, dir string) error { return UpdateSetting(dir, "default_template", "gemini", false) }, "settings.yaml"},
	{"update_versioned_alias_struct_fallback", "settings.yaml", "# header\nschema_version: \"1\"\nbase: &b\n  broker_id: b-1\nserver:\n  broker: *b\nactive_profile: local\n",
		func(t *testing.T, dir string) error { return UpdateVersionedSetting(dir, "hub.brokerId", "b-2") }, "settings.yaml"},
	{"delete_hub_connection_versioned", "settings.yaml", goldenSettingsFixture,
		func(t *testing.T, dir string) error { return DeleteHubConnection(dir, "hub-prod", false) }, "settings.yaml"},
	{"staged_path_edits_commit", "settings.yaml", goldenSettingsFixture,
		func(t *testing.T, dir string) error {
			return commitGoldenPathEdits(t, dir, []SettingsPathEdit{
				{Path: []string{"server", "hub", "port"}, Value: 9999},
				{Path: []string{"server", "broker", "hub_endpoint"}, Value: "https://hub.example.com"},
				{Path: []string{"hub_connections", "hub-staging"}, Delete: true},
			})
		}, "settings.yaml"},
	{"load_modify_save_struct_rewrite", "settings.yaml", goldenSettingsFixture,
		func(t *testing.T, dir string) error {
			return LoadModifySaveVersionedSettings(dir, func(vs *VersionedSettings) error {
				vs.DefaultTemplate = "gemini"
				return nil
			})
		}, "settings.yaml"},
	{"migrate_legacy_settings", "settings.yaml", goldenLegacyFixture,
		func(t *testing.T, dir string) error { _, err := MigrateSettingsFile(dir, false); return err }, "settings.yaml"},
	{"migrate_legacy_state_yaml", "settings.yaml", goldenLegacyFixture,
		func(t *testing.T, dir string) error { _, err := MigrateSettingsFile(dir, false); return err }, "state.yaml"},
	{"save_project_state", "", "",
		func(t *testing.T, dir string) error {
			return SaveProjectState(dir, &ProjectState{LastSyncedAt: "2026-01-02T03:04:05Z"})
		}, "state.yaml"},
	{"init_write_project_settings", "", "",
		func(t *testing.T, dir string) error {
			return writeProjectSettings(dir, "/work/space", "11111111-2222-3333-4444-555555555555", InitProjectOpts{SkipRuntimeCheck: true})
		}, "settings.yaml"},
	{"init_seed_project_settings", "", "",
		func(t *testing.T, dir string) error {
			return ensureProjectSettingsFile(dir, InitProjectOpts{SkipRuntimeCheck: true})
		}, "settings.yaml"},
}

// settingsWriterGlobalGoldens write the global settings file: the fixture
// is written to $HOME/.scion with HOME set to a temporary directory.
var settingsWriterGlobalGoldens = []settingsWriterGolden{
	{"integration_self_managed_plugin", "settings.yaml", goldenSettingsFixture,
		func(t *testing.T, dir string) error {
			return AddSelfManagedPluginWithBrokerType(SelfManagedPluginEntry{Name: "a2a-bridge", Address: "localhost:9090", ConfigFile: "/cfg/a2a.yaml"})
		}, "settings.yaml"},
	{"integration_add_plugin", "settings.yaml", goldenSettingsFixture,
		func(t *testing.T, dir string) error { return AddPluginToSettings("slack", "/cfg/slack.yaml") }, "settings.yaml"},
	{"integration_message_broker_type", "settings.yaml", goldenSettingsFixture,
		func(t *testing.T, dir string) error { return AddPluginToMessageBrokerTypes("slack") }, "settings.yaml"},
	{"integration_self_managed_plugin_new_file", "", "",
		func(t *testing.T, dir string) error {
			return AddSelfManagedPluginWithBrokerType(SelfManagedPluginEntry{Name: "a2a-bridge", Address: "localhost:9090", ConfigFile: "/cfg/a2a.yaml"})
		}, "settings.yaml"},
}

func TestSettingsWriters_Golden(t *testing.T) {
	update := os.Getenv("SCION_UPDATE_SETTINGS_GOLDEN") == "1"
	global := map[string]bool{}
	all := append([]settingsWriterGolden{}, settingsWriterGoldens...)
	for _, g := range settingsWriterGlobalGoldens {
		global[g.name] = true
		all = append(all, g)
	}
	for _, g := range all {
		t.Run(g.name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "proj")
			if global[g.name] {
				home := t.TempDir()
				t.Setenv("HOME", home)
				dir = filepath.Join(home, ".scion")
			}
			if err := os.MkdirAll(dir, 0755); err != nil {
				t.Fatal(err)
			}
			if g.file != "" {
				if err := os.WriteFile(filepath.Join(dir, g.file), []byte(g.fixture), 0644); err != nil {
					t.Fatal(err)
				}
			}
			if err := g.write(t, dir); err != nil {
				t.Fatalf("write: %v", err)
			}
			got, err := os.ReadFile(filepath.Join(dir, g.output))
			if err != nil {
				t.Fatal(err)
			}
			goldenPath := filepath.Join("testdata", "settings_writers", g.name+".golden")
			if update {
				if err := os.MkdirAll(filepath.Dir(goldenPath), 0755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(goldenPath, got, 0644); err != nil {
					t.Fatal(err)
				}
				return
			}
			want, err := os.ReadFile(goldenPath)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, want) {
				t.Errorf("%s differs from %s\n--- got ---\n%s\n--- want ---\n%s", g.output, goldenPath, got, want)
			}
		})
	}
}
