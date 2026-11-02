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
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

// TestProfilesSectionSchema_RejectsEnv pins ptone/scion#3048: a profile has
// no env field (V1ProfileConfig), so profiles.<name>.env must fail
// validation instead of being accepted and dropped on decode. Env under a
// profile's harness_overrides entry is a real field and stays valid.
func TestProfilesSectionSchema_RejectsEnv(t *testing.T) {
	errs := Validate("profiles", json.RawMessage(`{"dev": {"runtime": "docker", "env": {"FOO": "bar"}}}`))
	if len(errs) == 0 {
		t.Fatal("profiles.dev.env passed validation; want a schema error")
	}
	var msgs []string
	for _, e := range errs {
		msgs = append(msgs, e.Error())
	}
	if joined := strings.Join(msgs, "; "); !strings.Contains(joined, "env") {
		t.Errorf("validation error does not name env: %s", joined)
	}

	if errs := Validate("profiles", json.RawMessage(`{"dev": {"runtime": "docker", "harness_overrides": {"claude": {"env": {"FOO": "bar"}}}}}`)); len(errs) > 0 {
		t.Errorf("profiles.dev.harness_overrides.claude.env failed validation: %v", errs)
	}
}

// TestProfilesSectionSchema_RejectsUnknownKey checks that the profiles
// section refuses any key V1ProfileConfig has no field for.
func TestProfilesSectionSchema_RejectsUnknownKey(t *testing.T) {
	if errs := Validate("profiles", json.RawMessage(`{"dev": {"runtime": "docker", "no_such_key": "x"}}`)); len(errs) == 0 {
		t.Fatal("profiles.dev.no_such_key passed validation; want a schema error")
	}
}

// TestProfilesSectionSchema_KubernetesBlockServiceAccount checks the
// kubernetes_block_service_account field, which V1ProfileConfig has and the
// section schema now lists, with the same rule as settings-v1.schema.json.
func TestProfilesSectionSchema_KubernetesBlockServiceAccount(t *testing.T) {
	for _, v := range []string{"", "block-ksa", "a.b-c"} {
		doc := json.RawMessage(`{"k8s": {"runtime": "kubernetes", "kubernetes_block_service_account": "` + v + `"}}`)
		if errs := Validate("profiles", doc); len(errs) > 0 {
			t.Errorf("kubernetes_block_service_account %q failed validation: %v", v, errs)
		}
	}
	// The section schema carries the type only; the name rule is a save
	// check (config.RuntimeSaveChecks, ptone/scion#6580), so type-correct
	// invalid names pass here and a non-string fails.
	for _, v := range []string{"Bad_Name", "-lead", strings.Repeat("a", 254)} {
		doc := json.RawMessage(`{"k8s": {"runtime": "kubernetes", "kubernetes_block_service_account": "` + v + `"}}`)
		if errs := Validate("profiles", doc); len(errs) > 0 {
			t.Errorf("kubernetes_block_service_account %q failed the section schema: %v", v, errs)
		}
	}
	if errs := Validate("profiles", json.RawMessage(`{"k8s": {"runtime": "kubernetes", "kubernetes_block_service_account": 5}}`)); len(errs) == 0 {
		t.Error("a non-string kubernetes_block_service_account passed validation")
	}
}

// TestProfilesSectionSchema_MatchesProfileConfig pins the profiles section
// schema's property set to V1ProfileConfig's JSON field names, and its
// additionalProperties to false. A schema-only key would be accepted and
// then dropped on decode; a struct-only key could not be saved.
func TestProfilesSectionSchema_MatchesProfileConfig(t *testing.T) {
	sec, ok := rawSchemas["profiles"]
	if !ok {
		t.Fatal("no profiles section schema")
	}
	entry, ok := sec["additionalProperties"].(map[string]interface{})
	if !ok {
		t.Fatalf("profiles section has no per-profile schema: %v", sec["additionalProperties"])
	}
	if ap, ok := entry["additionalProperties"].(bool); !ok || ap {
		t.Errorf("per-profile schema additionalProperties = %v, want false", entry["additionalProperties"])
	}
	props, _ := entry["properties"].(map[string]interface{})
	var schemaKeys []string
	for k := range props {
		schemaKeys = append(schemaKeys, k)
	}
	sort.Strings(schemaKeys)

	var structKeys []string
	typ := reflect.TypeOf(config.V1ProfileConfig{})
	for i := 0; i < typ.NumField(); i++ {
		f := typ.Field(i)
		if f.Anonymous {
			// encoding/json would promote an embedded struct's fields;
			// list them in the schema and extend this test before adding one.
			t.Fatalf("V1ProfileConfig embeds %s; this test does not handle embedded fields", f.Name)
		}
		if !f.IsExported() {
			continue
		}
		name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		if name == "-" {
			continue
		}
		if name == "" {
			// encoding/json emits an untagged field under its Go name.
			name = f.Name
		}
		structKeys = append(structKeys, name)
	}
	sort.Strings(structKeys)

	if !reflect.DeepEqual(schemaKeys, structKeys) {
		t.Errorf("profiles section schema properties differ from V1ProfileConfig fields:\n  schema: %v\n  struct: %v", schemaKeys, structKeys)
	}
}
