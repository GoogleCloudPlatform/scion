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
	"testing"
)

// TestRuntimesSectionSchema_KubernetesFieldTypes pins the JSON types of the
// runtimes section's Kubernetes placement and identity properties to
// settings-v1.schema.json's runtimeConfig (ptone/scion#6580). The section
// keeps the types only; the value rules are save-time checks
// (config.RuntimeSaveChecks).
func TestRuntimesSectionSchema_KubernetesFieldTypes(t *testing.T) {
	data, err := os.ReadFile("../schemas/settings-v1.schema.json")
	if err != nil {
		t.Fatal(err)
	}
	var root struct {
		Defs map[string]struct {
			Properties map[string]map[string]interface{} `json:"properties"`
		} `json:"$defs"`
	}
	if err := json.Unmarshal(data, &root); err != nil {
		t.Fatal(err)
	}
	want := root.Defs["runtimeConfig"].Properties
	sec, ok := rawSchemas["runtimes"]
	if !ok {
		t.Fatal("no runtimes section schema")
	}
	entry, _ := sec["additionalProperties"].(map[string]interface{})
	got, _ := entry["properties"].(map[string]interface{})
	for _, k := range []string{"node_selector", "tolerations", "runtime_class_name", "kubernetes_block_service_account", "kubernetes_service_account_mappings", "priority_class_name"} {
		w, ok := want[k]
		if !ok {
			t.Fatalf("settings-v1.schema.json runtimeConfig has no %s", k)
		}
		g, ok := got[k].(map[string]interface{})
		if !ok {
			t.Errorf("runtimes section schema has no %s", k)
			continue
		}
		if g["type"] != w["type"] {
			t.Errorf("%s: section type %v, settings schema type %v", k, g["type"], w["type"])
		}
	}
}

// Stored values that break a value rule still pass the section schema, so a
// save that carries them is decided by config.RuntimeSaveChecks (a warning),
// not refused by the schema.
func TestRuntimesSectionSchema_KubernetesValuesAreTypeOnly(t *testing.T) {
	doc := json.RawMessage(`{"gke": {"type": "kubernetes", "node_selector": {"bad key!": "v"}, "tolerations": [{"operator": "exists"}], "runtime_class_name": "Not_Valid", "kubernetes_block_service_account": "Bad_Name", "priority_class_name": "Bad_Name", "kubernetes_service_account_mappings": {"NotAnEmail": "Bad_Name"}}}`)
	if errs := Validate("runtimes", doc); len(errs) > 0 {
		t.Errorf("type-correct values failed the section schema: %v", errs)
	}
	bad := json.RawMessage(`{"gke": {"type": "kubernetes", "node_selector": ["a"]}}`)
	if errs := Validate("runtimes", bad); len(errs) == 0 {
		t.Error("a node_selector that is not an object passed the section schema")
	}
}
