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

// fromDirTestDir returns a project dir holding settings.yaml with content,
// under a HOME whose global settings set other values, so a result that
// came from the global layer would show.
func fromDirTestDir(t *testing.T, content string) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("SCION_PROJECT_ID", "")
	t.Setenv("SCION_HUB_PROJECT_ID", "")
	t.Setenv("SCION_HUB_ENDPOINT", "")
	t.Setenv("SCION_HUB_TOKEN", "")
	t.Setenv("SCION_HUB_API_KEY", "")
	global := filepath.Join(home, ".scion")
	if err := os.MkdirAll(global, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(global, "settings.yaml"),
		[]byte("schema_version: \"1\"\nhub:\n  endpoint: https://global.example.com\n  project_id: global-hub-id\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), ".scion")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if content != "" {
		if err := os.WriteFile(filepath.Join(dir, "settings.yaml"), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// A v1 file's hub.project_id reaches Hub.ProjectID (ptone/scion#5427).
func TestLoadSettingsFromDir_V1HubProjectID(t *testing.T) {
	dir := fromDirTestDir(t, "schema_version: \"1\"\nhub:\n  endpoint: https://hub.example.com\n  project_id: hub-proj-1\n  linked: true\nserver:\n  broker:\n    broker_nickname: nick\n")
	s, err := LoadSettingsFromDir(dir)
	if err != nil {
		t.Fatalf("LoadSettingsFromDir: %v", err)
	}
	if got := s.GetHubProjectID(); got != "hub-proj-1" {
		t.Errorf("GetHubProjectID() = %q, want hub-proj-1", got)
	}
	if got := s.GetHubEndpoint(); got != "https://hub.example.com" {
		t.Errorf("GetHubEndpoint() = %q, want https://hub.example.com", got)
	}
	if s.Hub == nil || s.Hub.Linked == nil || !*s.Hub.Linked {
		t.Errorf("Hub.Linked = %+v, want true", s.Hub)
	}
	if s.Hub == nil || s.Hub.BrokerNickname != "nick" {
		t.Errorf("Hub.BrokerNickname = %+v, want nick", s.Hub)
	}
}

// A legacy file's hub.projectId still maps to Hub.ProjectID, and only
// there: it named the hub project, never the project's own ID.
func TestLoadSettingsFromDir_LegacyHubProjectID(t *testing.T) {
	dir := fromDirTestDir(t, "hub:\n  endpoint: https://hub.example.com\n  projectId: hub-proj-legacy\nproject_id: own-id\n")
	s, err := LoadSettingsFromDir(dir)
	if err != nil {
		t.Fatalf("LoadSettingsFromDir: %v", err)
	}
	if got := s.GetHubProjectID(); got != "hub-proj-legacy" {
		t.Errorf("GetHubProjectID() = %q, want hub-proj-legacy", got)
	}
	if s.ProjectID != "own-id" {
		t.Errorf("ProjectID = %q, want own-id", s.ProjectID)
	}
}

// No environment variable and no other layer reaches the result.
func TestLoadSettingsFromDir_IgnoresEnvironmentAndGlobal(t *testing.T) {
	dir := fromDirTestDir(t, "schema_version: \"1\"\nhub:\n  endpoint: https://hub.example.com\n  project_id: hub-proj-1\n")
	t.Setenv("SCION_HUB_PROJECT_ID", "env-hub-id")
	t.Setenv("SCION_PROJECT_ID", "env-id")
	t.Setenv("SCION_HUB_ENDPOINT", "https://env.example.com")
	t.Setenv("SCION_HUB_TOKEN", "env-token")
	t.Setenv("SCION_HUB_API_KEY", "env-key")
	s, err := LoadSettingsFromDir(dir)
	if err != nil {
		t.Fatalf("LoadSettingsFromDir: %v", err)
	}
	if got := s.GetHubProjectID(); got != "hub-proj-1" {
		t.Errorf("GetHubProjectID() = %q, want hub-proj-1", got)
	}
	if got := s.GetHubEndpoint(); got != "https://hub.example.com" {
		t.Errorf("GetHubEndpoint() = %q, want https://hub.example.com", got)
	}
	if s.Hub.Token != "" || s.Hub.APIKey != "" {
		t.Errorf("Hub.Token/APIKey = %q/%q, want empty", s.Hub.Token, s.Hub.APIKey)
	}
	if s.ProjectID == "env-id" || s.ProjectID == "env-hub-id" {
		t.Errorf("ProjectID = %q, came from the environment", s.ProjectID)
	}
}

// Without a settings file the result is empty: no hub section, and no
// value from the global settings or the embedded defaults.
func TestLoadSettingsFromDir_NoFile(t *testing.T) {
	dir := fromDirTestDir(t, "")
	s, err := LoadSettingsFromDir(dir)
	if err != nil {
		t.Fatalf("LoadSettingsFromDir: %v", err)
	}
	if s.Hub != nil {
		t.Errorf("Hub = %+v, want nil", s.Hub)
	}
	if s.DefaultTemplate != "" || s.ActiveProfile != "" {
		t.Errorf("DefaultTemplate/ActiveProfile = %q/%q, want empty (no embedded defaults)", s.DefaultTemplate, s.ActiveProfile)
	}
	if s.Runtimes == nil || s.Harnesses == nil || s.Profiles == nil {
		t.Errorf("maps = %v/%v/%v, want non-nil", s.Runtimes, s.Harnesses, s.Profiles)
	}
}

// ResolveSettingValueIgnoringEnv leaves out only the named variable: the
// value and source come from the files, and other variables still apply
// (ptone/scion#5483).
func TestResolveSettingValueIgnoringEnv(t *testing.T) {
	dir := fromDirTestDir(t, "schema_version: \"1\"\ncli:\n  mode: agent\n")
	t.Setenv("SCION_CLI_MODE", "human")
	t.Setenv("SCION_HUB_ENDPOINT", "https://env.example.com")

	with, err := ResolveSettingValue(dir, "cli.mode")
	if err != nil {
		t.Fatal(err)
	}
	if with.Value != "human" || with.Source != "env SCION_CLI_MODE" {
		t.Fatalf("ResolveSettingValue = %+v, want human from env SCION_CLI_MODE", with)
	}
	without, err := ResolveSettingValueIgnoringEnv(dir, "cli.mode", "SCION_CLI_MODE")
	if err != nil {
		t.Fatal(err)
	}
	if want := "project " + GetSettingsPath(dir); without.Value != "agent" || without.Source != want {
		t.Errorf("ResolveSettingValueIgnoringEnv = %+v, want agent from %q", without, want)
	}
	other, err := ResolveSettingValueIgnoringEnv(dir, "hub.endpoint", "SCION_CLI_MODE")
	if err != nil {
		t.Fatal(err)
	}
	if other.Value != "https://env.example.com" || other.Source != "env SCION_HUB_ENDPOINT" {
		t.Errorf("hub.endpoint = %+v, want the SCION_HUB_ENDPOINT value", other)
	}
}

// An unversioned file with a snake-case hub.project_id (the shape the
// legacy hub-key migration leaves) and a top-level project_id is converted as
// a legacy file. The view, like LoadSettings, takes both ProjectID and
// Hub.ProjectID from hub.project_id: only a legacy camelCase hub.projectId
// is kept out of ProjectID (ptone/scion#5427 review).
func TestLoadSettingsFromDir_UnversionedSnakeHubProjectID(t *testing.T) {
	dir := fromDirTestDir(t, "hub:\n  endpoint: https://hub.example.com\n  project_id: hub-snake-id\nproject_id: own-id\n")
	s, err := LoadSettingsFromDir(dir)
	if err != nil {
		t.Fatalf("LoadSettingsFromDir: %v", err)
	}
	if got := s.GetHubProjectID(); got != "hub-snake-id" {
		t.Errorf("GetHubProjectID() = %q, want hub-snake-id", got)
	}
	if s.ProjectID != "hub-snake-id" {
		t.Errorf("ProjectID = %q, want hub-snake-id (hub.project_id outranks the top-level project_id in the view)", s.ProjectID)
	}
}

// The unused-key warning is only a warning: a file the legacy probe cannot
// decode, but the view reads, still loads (ptone/scion#5427 review).
func TestLoadSettingsFromDir_LegacyProbeDecodeFailureStillLoads(t *testing.T) {
	// A scalar "harnesses" key in a v1 file: the v1 view has no such key
	// and ignores it, but the legacy Settings probe's Harnesses map cannot
	// decode a string.
	dir := fromDirTestDir(t, "schema_version: \"1\"\nhub:\n  endpoint: https://hub.example.com\n  project_id: hub-proj-1\nharnesses: not-a-map\n")
	s, err := LoadSettingsFromDir(dir)
	if err != nil {
		t.Fatalf("LoadSettingsFromDir: %v", err)
	}
	if got := s.GetHubProjectID(); got != "hub-proj-1" {
		t.Errorf("GetHubProjectID() = %q, want hub-proj-1", got)
	}
}
