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
	"slices"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/knadh/koanf/providers/confmap"
	"github.com/knadh/koanf/v2"
)

// The hub observability switches are Layer-1 keys of their own section,
// and both apply only after a restart (the exporters are created once at
// startup), so a save reports them as pending a restart.
func TestHubObservability_SectionAndRestartRequired(t *testing.T) {
	for _, key := range []string{config.HubMetricsEnabledKey, config.HubTracingEnabledKey} {
		if got := OwningSection(key); got != "hub_observability" {
			t.Errorf("OwningSection(%q) = %q, want hub_observability", key, got)
		}
		if !IsRestartRequired(key) {
			t.Errorf("IsRestartRequired(%q) = false, want true", key)
		}
	}
	info, ok := SchemaInfo()["hub_observability"]
	if !ok {
		t.Fatal("SchemaInfo has no hub_observability section")
	}
	for _, key := range []string{config.HubMetricsEnabledKey, config.HubTracingEnabledKey} {
		if !slices.Contains(info.RestartRequired, key) {
			t.Errorf("schema restart_required = %v, want it to list %s", info.RestartRequired, key)
		}
		if !slices.Contains(info.KoanfPaths, key) {
			t.Errorf("schema koanf_paths = %v, want it to list %s", info.KoanfPaths, key)
		}
	}
}

func TestHubObservability_Validate(t *testing.T) {
	valid := []string{
		`{}`,
		`{"metrics_enabled":true}`,
		`{"metrics_enabled":false,"tracing_enabled":true}`,
		`{"tracing_enabled":false}`,
	}
	for _, doc := range valid {
		if errs := Validate("hub_observability", json.RawMessage(doc)); len(errs) > 0 {
			t.Errorf("Validate(%s) = %v, want no errors", doc, errs)
		}
	}
	invalid := map[string]string{
		`{"metrics_enabled":"yes"}`:    "wrong type for boolean",
		`{"tracing_enabled":1}`:        "wrong type for boolean",
		`{"project_id":"my-project"}`:  "the export project is not a Layer-1 key",
		`{"export_endpoint":"x:4317"}`: "no endpoint key",
	}
	for doc, desc := range invalid {
		if errs := Validate("hub_observability", json.RawMessage(doc)); len(errs) == 0 {
			t.Errorf("Validate(%s): want an error (%s), got none", doc, desc)
		}
	}
}

// A settings file or env value of the switches is seed material for the
// section, and a section document loads back to the same koanf keys.
func TestHubObservability_SeedRoundTrip(t *testing.T) {
	k := koanf.New(".")
	if err := k.Load(confmap.Provider(map[string]interface{}{
		config.HubMetricsEnabledKey: false,
		config.HubTracingEnabledKey: true,
		"server.hub.gcp_project_id": "kept-out",
	}, "."), nil); err != nil {
		t.Fatal(err)
	}
	doc, err := ExtractSectionFromKoanf(k, "hub_observability")
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]interface{}
	if err := json.Unmarshal(doc, &got); err != nil {
		t.Fatal(err)
	}
	want := map[string]interface{}{"metrics_enabled": false, "tracing_enabled": true}
	if len(got) != len(want) || got["metrics_enabled"] != false || got["tracing_enabled"] != true {
		t.Fatalf("section doc = %v, want %v", got, want)
	}

	back, err := LoadSectionsIntoKoanf(map[string]json.RawMessage{"hub_observability": doc})
	if err != nil {
		t.Fatal(err)
	}
	if !back.Exists(config.HubMetricsEnabledKey) || back.Bool(config.HubMetricsEnabledKey) {
		t.Errorf("%s = %v, want false", config.HubMetricsEnabledKey, back.Get(config.HubMetricsEnabledKey))
	}
	if !back.Bool(config.HubTracingEnabledKey) {
		t.Errorf("%s = %v, want true", config.HubTracingEnabledKey, back.Get(config.HubTracingEnabledKey))
	}
	if back.Exists("server.hub.gcp_project_id") {
		t.Error("the export project must not be part of the section")
	}
}
