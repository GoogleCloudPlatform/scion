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

package opsettings

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/knadh/koanf/v2"
)

// ptone/scion#4108: free-form map keys containing a dot survive loading a
// section document into koanf, so the section still decodes into its type.
func TestLoadSectionsIntoKoanf_DottedMapKeysStayWhole(t *testing.T) {
	doc := json.RawMessage(`{
		"enabled": true,
		"cloud": {"endpoint": "otel.example.com:4317", "headers": {"x-api.key": "v"}},
		"filter": {"sampling": {"rates": {"agent.tool.call": 0.5}}},
		"resource": {"service.namespace": "team-a", "plain": "p"}
	}`)
	k, err := LoadSectionsIntoKoanf(map[string]json.RawMessage{"telemetry": doc})
	if err != nil {
		t.Fatalf("LoadSectionsIntoKoanf: %v", err)
	}

	// Leaf lookups by path still work.
	if got := k.String("telemetry.cloud.endpoint"); got != "otel.example.com:4317" {
		t.Errorf("telemetry.cloud.endpoint = %q", got)
	}
	if !k.Bool("telemetry.enabled") {
		t.Errorf("telemetry.enabled not true")
	}

	// The subtree decodes into the telemetry type with the keys intact,
	// also after a merge into another koanf (the Snapshot path).
	target := koanf.New(".")
	if err := target.Merge(k); err != nil {
		t.Fatalf("Merge: %v", err)
	}
	for name, kk := range map[string]*koanf.Koanf{"loaded": k, "merged": target} {
		data, err := json.Marshal(kk.Cut("telemetry").Raw())
		if err != nil {
			t.Fatalf("%s: marshal: %v", name, err)
		}
		var tc config.V1TelemetryConfig
		if err := json.Unmarshal(data, &tc); err != nil {
			t.Fatalf("%s: telemetry does not decode: %v (%s)", name, err, data)
		}
		if want := map[string]string{"service.namespace": "team-a", "plain": "p"}; !reflect.DeepEqual(tc.Resource, want) {
			t.Errorf("%s: resource = %v, want %v", name, tc.Resource, want)
		}
		if tc.Filter == nil || tc.Filter.Sampling == nil || tc.Filter.Sampling.Rates["agent.tool.call"] != 0.5 {
			t.Errorf("%s: sampling rates lost the dotted key: %+v", name, tc.Filter)
		}
		if tc.Cloud == nil || tc.Cloud.Headers["x-api.key"] != "v" {
			t.Errorf("%s: cloud headers lost the dotted key: %+v", name, tc.Cloud)
		}
	}
}

// Dotted keys in a map-of-objects section (a runtime name) stay whole too.
func TestLoadSectionsIntoKoanf_DottedRuntimeNameStaysWhole(t *testing.T) {
	k, err := LoadSectionsIntoKoanf(map[string]json.RawMessage{"runtimes": json.RawMessage(`{"k8s.prod":{"type":"kubernetes"}}`)})
	if err != nil {
		t.Fatalf("LoadSectionsIntoKoanf: %v", err)
	}
	keys := k.MapKeys("runtimes")
	if !reflect.DeepEqual(keys, []string{"k8s.prod"}) {
		t.Errorf("runtimes keys = %v, want [k8s.prod]", keys)
	}
}

func TestIsRestartRequired(t *testing.T) {
	cases := map[string]bool{
		"server.hub.public_url":            true,
		"server.github_app.app_id":         true,
		"server.github_app.api_base_url":   false,
		"server.hub.hub_name":              false,
		"image_registry":                   false,
		"server.hub.soft_delete_retention": false,
		"server.database.url":              false, // Layer-0: not a saved setting
	}
	for key, want := range cases {
		if got := IsRestartRequired(key); got != want {
			t.Errorf("IsRestartRequired(%q) = %v, want %v", key, got, want)
		}
	}
}

func TestSchemaInfo_ListsRestartRequiredKeys(t *testing.T) {
	info := SchemaInfo()
	if info == nil {
		t.Fatal("SchemaInfo returned nil")
	}
	if got := info["endpoints"].RestartRequired; !reflect.DeepEqual(got, []string{"server.hub.public_url"}) {
		t.Errorf("endpoints restart_required = %v", got)
	}
	if got := info["lifecycle"].RestartRequired; len(got) != 0 {
		t.Errorf("lifecycle restart_required = %v, want none", got)
	}
}

// ptone/scion#4108: dotted map keys from settings.yaml survive the bootstrap
// load, the startup seeding extract (ExtractSectionFromKoanf) and the load
// of the seeded document back into koanf.
func TestBootstrapTelemetry_DottedMapKeysFromSettingsYAML(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	scionDir := filepath.Join(home, ".scion")
	if err := os.MkdirAll(scionDir, 0o755); err != nil {
		t.Fatal(err)
	}
	yaml := `schema_version: "1"
telemetry:
  enabled: true
  resource:
    service.namespace: team-a
  filter:
    sampling:
      rates:
        agent.tool.call: 0.5
`
	if err := os.WriteFile(filepath.Join(scionDir, "settings.yaml"), []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}

	k := config.LoadBootstrapKoanf()
	seeded, err := ExtractSectionFromKoanf(k, "telemetry")
	if err != nil {
		t.Fatalf("ExtractSectionFromKoanf: %v", err)
	}
	if errs := Validate("telemetry", seeded); len(errs) > 0 {
		t.Fatalf("seeded telemetry document does not validate: %v (%s)", errs, seeded)
	}

	check := func(name string, data []byte) {
		t.Helper()
		var tc config.V1TelemetryConfig
		if err := json.Unmarshal(data, &tc); err != nil {
			t.Fatalf("%s: telemetry does not decode: %v (%s)", name, err, data)
		}
		if tc.Resource["service.namespace"] != "team-a" {
			t.Errorf("%s: resource = %v, want the service.namespace key intact", name, tc.Resource)
		}
		if tc.Filter == nil || tc.Filter.Sampling == nil || tc.Filter.Sampling.Rates["agent.tool.call"] != 0.5 {
			t.Errorf("%s: sampling rates lost the dotted key: %s", name, data)
		}
	}
	check("seeded document", seeded)

	reloaded, err := LoadSectionsIntoKoanf(map[string]json.RawMessage{"telemetry": seeded})
	if err != nil {
		t.Fatalf("LoadSectionsIntoKoanf: %v", err)
	}
	data, err := json.Marshal(reloaded.Cut("telemetry").Raw())
	if err != nil {
		t.Fatal(err)
	}
	check("reloaded document", data)
}
