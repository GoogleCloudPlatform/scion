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

package opsettings_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/config/opsettings"
	"github.com/knadh/koanf/parsers/yaml"
	"github.com/knadh/koanf/providers/rawbytes"
	"github.com/knadh/koanf/v2"
)

// TestProfilesSeedExtraction_DropsLegacyTimezone checks the profiles seed
// document carries no timezone key once DeleteLegacyProfileTimezones has run
// over the bootstrap koanf, including for a profile name containing '.'.
func TestProfilesSeedExtraction_DropsLegacyTimezone(t *testing.T) {
	const body = `profiles:
  team.west:
    runtime: docker
    timezone: America/Los_Angeles
  tz.only:
    timezone: Asia/Tokyo
  plain:
    runtime: k8s
    timezone: ""
`
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "settings.yaml"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	scan, err := config.ScanSettingsFileProfileTimezones(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(scan.ProfileTimezones) != 3 {
		t.Fatalf("scan found %+v, want 3 values", scan.ProfileTimezones)
	}

	k := koanf.New(".")
	if err := k.Load(rawbytes.Provider([]byte(body)), yaml.Parser()); err != nil {
		t.Fatal(err)
	}
	before, err := opsettings.ExtractSectionFromKoanf(k, "profiles")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(before), "timezone") {
		t.Fatalf("precondition: seed doc before deletion has no timezone: %s", before)
	}

	config.DeleteLegacyProfileTimezones(k, scan.ProfileTimezones)
	doc, err := opsettings.ExtractSectionFromKoanf(k, "profiles")
	if err != nil {
		t.Fatal(err)
	}
	var tree map[string]any
	if err := json.Unmarshal(doc, &tree); err != nil {
		t.Fatalf("seed doc %s: %v", doc, err)
	}
	if hasKey(tree, config.LegacyProfileTimezoneKey) {
		t.Errorf("profiles seed doc still carries timezone: %s", doc)
	}
	// Other profile keys survive.
	if !strings.Contains(string(doc), "docker") || !strings.Contains(string(doc), "k8s") {
		t.Errorf("profiles seed doc lost other keys: %s", doc)
	}
	if errs := opsettings.Validate("profiles", doc); len(errs) > 0 {
		t.Errorf("stripped profiles doc fails validation: %v", errs)
	}
}

func hasKey(v any, key string) bool {
	m, ok := v.(map[string]any)
	if !ok {
		return false
	}
	for k, child := range m {
		if k == key || hasKey(child, key) {
			return true
		}
	}
	return false
}

// TestProfilesSchema_NoTimezoneProperty checks the removed field is gone from
// the profiles section schema.
func TestProfilesSchema_NoTimezoneProperty(t *testing.T) {
	info, ok := opsettings.SchemaInfo()["profiles"]
	if !ok {
		t.Fatal("profiles section missing from SchemaInfo")
	}
	raw, err := json.Marshal(info.Schema)
	if err != nil {
		t.Fatal(err)
	}
	var schema struct {
		AdditionalProperties struct {
			Properties map[string]any `json:"properties"`
		} `json:"additionalProperties"`
	}
	if err := json.Unmarshal(raw, &schema); err != nil {
		t.Fatal(err)
	}
	props := schema.AdditionalProperties.Properties
	if len(props) == 0 {
		t.Fatalf("profiles schema has no per-profile properties: %s", raw)
	}
	if _, ok := props["timezone"]; ok {
		t.Errorf("profiles schema still declares timezone: %s", raw)
	}
}
