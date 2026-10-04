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
	"testing"
)

// legacyTZSettingsYAML has a dotted profile name, an empty value, a null
// value and a profile without the key.
const legacyTZSettingsYAML = `schema_version: "1"
default_timezone: Europe/Paris
profiles:
  team.west:
    runtime: docker
    timezone: America/Los_Angeles
  plain:
    runtime: docker
    timezone: ""
  nulled:
    runtime: docker
    timezone:
  untouched:
    runtime: docker
`

func TestScanSettingsFileProfileTimezones_DottedNameAndFileUnchanged(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.yaml")
	if err := os.WriteFile(path, []byte(legacyTZSettingsYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	before, _ := os.Stat(path)

	scan, err := ScanSettingsFileProfileTimezones(dir)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if scan.Path != path {
		t.Errorf("Path = %q, want %q", scan.Path, path)
	}
	if scan.DefaultTimezone != "Europe/Paris" {
		t.Errorf("DefaultTimezone = %q, want Europe/Paris", scan.DefaultTimezone)
	}
	want := []LegacyProfileTimezone{
		{Profile: "nulled", Timezone: ""},
		{Profile: "plain", Timezone: ""},
		{Profile: "team.west", Timezone: "America/Los_Angeles"},
	}
	if !reflect.DeepEqual(scan.ProfileTimezones, want) {
		t.Errorf("ProfileTimezones = %+v, want %+v", scan.ProfileTimezones, want)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != legacyTZSettingsYAML {
		t.Errorf("settings file was modified:\n%s", data)
	}
	after, _ := os.Stat(path)
	if !after.ModTime().Equal(before.ModTime()) {
		t.Errorf("settings file mtime changed")
	}
}

func TestScanSettingsFileProfileTimezones_JSONAndMissing(t *testing.T) {
	dir := t.TempDir()
	scan, err := ScanSettingsFileProfileTimezones(dir)
	if err != nil || scan.Path != "" || len(scan.ProfileTimezones) != 0 {
		t.Fatalf("empty dir: scan=%+v err=%v, want zero value", scan, err)
	}

	body := `{"profiles":{"a.b":{"timezone":"Asia/Kathmandu"},"c":{"runtime":"docker"}}}`
	if err := os.WriteFile(filepath.Join(dir, "settings.json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	scan, err = ScanSettingsFileProfileTimezones(dir)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	want := []LegacyProfileTimezone{{Profile: "a.b", Timezone: "Asia/Kathmandu"}}
	if !reflect.DeepEqual(scan.ProfileTimezones, want) {
		t.Errorf("ProfileTimezones = %+v, want %+v", scan.ProfileTimezones, want)
	}
}

func TestLegacyProfileTimezonesFromProfiles_NonString(t *testing.T) {
	got := LegacyProfileTimezonesFromProfiles(map[string]any{
		"n":   map[string]any{"timezone": 5},
		"bad": "not-an-object",
	})
	want := []LegacyProfileTimezone{{Profile: "n", Timezone: "5"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v, want %+v", got, want)
	}
}
